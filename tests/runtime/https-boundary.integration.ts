/**
 * Packaged HTTPS-boundary integration tests: the hub listens only on a
 * private Unix socket, trusts the configured canonical HTTPS origin, rejects
 * spoofed Host/Origin/forwarded headers, issues Secure host-only cookies,
 * scopes SSE, and removes its socket on clean shutdown.
 *
 * These are isolated local fixtures; no tailnet, Tailscale Serve, or Funnel is
 * involved. The direct Unix HTTP client supplies the Host a TLS terminator
 * would forward.
 */

import assert from "node:assert/strict";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, rmSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { CONNECT_PATH, credential, demoToken } from "./support/config";
import { within } from "./support/deadline";
import {
  firstSetCookie,
  loginRemote,
  remoteListenMatch,
  spawnRemoteCandidate,
  startRemoteHub,
} from "./support/remote";
import {
  openUnixSse,
  rawUnixRequest,
  unixRequest,
  unixPathStats,
  wsUpgradeUnix,
} from "./support/unix";
import { parseSnapshot, rawCollector, recordOf, samplesMessage } from "./support/wire";
import { bindCollectorSession, expectClose, wsTerminate } from "./support/ws";

test("remote crash recovery refuses a stale socket until the stopped owner's socket is explicitly removed", async (t) => {
  const hub = await startRemoteHub(t, { browserUsers: false });
  assert.deepEqual(await hub.stop("SIGKILL"), { code: null, signal: "SIGKILL" });
  assert.equal(existsSync(hub.socketPath), true, "SIGKILL cannot run socket cleanup handlers");
  const env = { HUB_PUBLIC_ORIGIN: hub.publicOrigin, HUB_SOCKET_PATH: hub.socketPath };
  const blocked = spawnRemoteCandidate(t, env);
  const blockedExit = await within(blocked.exit, 10_000, "stale socket startup failure");
  assert.equal(blockedExit.code, 1);
  assert.ok(blocked.stderrLines().some(line => line.includes("already exists as a socket")));
  assert.equal(existsSync(hub.socketPath), true, "startup must not unlink an occupied socket");

  // The fixture owner is confirmed exited above. This is explicit operator
  // recovery, not automatic deletion of a potentially live listener's path.
  unlinkSync(hub.socketPath);
  const recovered = spawnRemoteCandidate(t, env);
  await recovered.waitForLine(remoteListenMatch, 30_000, "recovered remote hub");
  const health = await unixRequest(hub.socketPath, { path: "/api/health", headers: { host: hub.hostHeader } });
  assert.equal(health.status, 200);
  assert.deepEqual(await recovered.stop("SIGTERM", 8_000), { code: 0, signal: null });
  assert.equal(existsSync(hub.socketPath), false);
});

/** Drains queued frames and reports whether the stream really ended. */
async function streamEnds(
  stream: { next(deadlineMs: number): Promise<unknown | null> },
  deadlineMs = 3_000,
): Promise<boolean> {
  const end = Date.now() + deadlineMs;
  for (;;) {
    const remaining = end - Date.now();
    if (remaining <= 0) return false;
    const event = await stream.next(remaining);
    if (event === null) return true;
  }
}

test("remote boundary: exact Host/Origin first, spoofed proxy headers never trusted, Secure __Host cookie lifecycle", async (t) => {
  const hub = await startRemoteHub(t, {
    publicOrigin: "https://hub.example.test:8443",
    collectors: [credential("c-remote", "m-remote", demoToken("remote"))],
  });

  // Exact canonical Host reaches health. Wrong Host fails before health with
  // 421, even when a spoofed X-Forwarded-Host names the right value.
  const health = await hub.request({ path: "/api/health" });
  assert.equal(health.status, 200, JSON.stringify(health.json));
  assert.equal((health.json as { mode?: string }).mode, "runtime-spike");
  for (const target of ["//evil.example/path", "ftp://evil.example/path", "/%5Cevil.example/path"]) {
    const invalidTarget = await rawUnixRequest(hub.socketPath,
      `GET ${target} HTTP/1.1\r\nHost: ${hub.hostHeader}\r\nConnection: close\r\n\r\n`);
    assert.match(invalidTarget, /^HTTP\/1\.1 400/, target);
    assert.equal(/\r\nlocation:/i.test(invalidTarget), false, "no redirect from ambiguous target");
  }

  const fleet = await hub.request({ path: "/", headers: hub.browserHeaders });
  assert.equal(fleet.status, 200);
  assert.match(fleet.headers["content-type"] ?? "", /text\/html/);
  assert.match(fleet.body, /aria-label="Primary"/, "primary navigation is server-rendered before hydration");
  const script = /<script[^>]+src="(\/_next\/static\/[^\"]+)"/.exec(fleet.body);
  assert.ok(script, "production remote HTML references framework assets");
  const asset = await hub.request({ path: script![1] });
  assert.equal(asset.status, 200);
  assert.match(asset.headers["content-type"] ?? "", /javascript/);
  const badHosts: Record<string, string>[] = [
    { host: "evil.example.test" },
    { host: "hub.example.test" },
    { host: "evil.example.test", "x-forwarded-host": hub.hostHeader },
    { host: "evil.example.test", forwarded: `host=${hub.hostHeader};proto=https` },
  ];
  for (const headers of badHosts) {
    const response = await hub.request({ path: "/api/health", headers });
    assert.equal(response.status, 421, `Host ${JSON.stringify(headers)} must be rejected`);
    assert.equal(response.body.includes("runtime-spike"), false, "no health body leaks on a boundary failure");
  }

  // Any foreign Origin is rejected before auth/Next; the exact public origin
  // is accepted on reads.
  assert.equal((await hub.request({ path: "/api/health", headers: { origin: "https://evil.example.test" } })).status, 403);
  assert.equal((await hub.request({ path: "/api/health", headers: { origin: hub.publicOrigin } })).status, 200);

  // Duplicate raw boundary headers are invalid, not last/first-wins.
  const duplicateHost = await rawUnixRequest(
    hub.socketPath,
    `GET /api/health HTTP/1.1\r\nHost: ${hub.hostHeader}\r\nHost: evil.example.test\r\nConnection: close\r\n\r\n`,
  );
  assert.match(duplicateHost, /^HTTP\/1\.1 400/, duplicateHost.slice(0, 200));
  const duplicateOrigin = await rawUnixRequest(
    hub.socketPath,
    `GET /api/health HTTP/1.1\r\nHost: ${hub.hostHeader}\r\nOrigin: ${hub.publicOrigin}\r\nOrigin: https://evil.example.test\r\nConnection: close\r\n\r\n`,
  );
  assert.match(duplicateOrigin, /^HTTP\/1\.1 400/, duplicateOrigin.slice(0, 200));

  // Unauthenticated protected endpoints fail before data/headers, but only
  // after the boundary accepted the request.
  const live = await hub.request({ path: "/api/live" });
  assert.equal(live.status, 401);
  assert.equal(/event-stream/.test(String(live.headers["content-type"] ?? "")), false);
  assert.equal((await hub.request({ path: "/api/auth/session" })).status, 401);

  // Remote browser mutations require the exact configured Origin even on
  // routes delegated to Next.js (foreign/missing values fail before auth).
  const mutationPath = "/__remote_mutation_probe__";
  assert.equal((await hub.request({ method: "POST", path: mutationPath, body: "{}" })).status, 403);
  assert.equal(
    (await hub.request({ method: "POST", path: mutationPath, headers: { origin: "https://evil.example.test" }, body: "{}" })).status,
    403,
  );
  assert.equal(
    (await hub.request({ method: "POST", path: mutationPath, headers: { origin: hub.publicOrigin }, body: "{}" })).status,
    401,
    "the exact origin passes the boundary, then authentication fails closed",
  );

  // Login requires the exact HTTPS origin. Missing/foreign Origin and spoofed
  // forwarded transport headers are rejected without a cookie.
  const badLoginHeaders: Record<string, string>[] = [
    {},
    { origin: "https://evil.example.test" },
    { origin: "http://hub.example.test" },
    { "x-forwarded-proto": "https", "x-forwarded-host": hub.hostHeader },
  ];
  for (const headers of badLoginHeaders) {
    const response = await hub.request({
      method: "POST",
      path: "/api/auth/login",
      headers: { "content-type": "application/json", ...headers },
      body: JSON.stringify({ username: "demo-viewer", password: "demo-password-browser-local-only" }),
    });
    assert.equal(response.status, 403, `login with ${JSON.stringify(headers)} must be rejected`);
    assert.equal(firstSetCookie(response.headers["set-cookie"]), null);
  }

  const login = await loginRemote(hub);
  assert.ok(login.rawSetCookie);
  const cookiePair = login.headers.cookie;

  const session = await hub.request({ path: "/api/auth/session", headers: { cookie: cookiePair } });
  assert.equal(session.status, 200);
  assert.equal((session.json as { username?: string }).username, "demo-viewer");

  // The local plaintext cookie name is a different credential and cannot
  // authenticate a remote session.
  const token = cookiePair.slice("__Host-rigscope_session=".length);
  assert.equal(
    (await hub.request({ path: "/api/auth/session", headers: { cookie: `rigscope_session=${token}` } })).status,
    401,
  );

  // Logout requires CSRF and emits a deletion cookie with the same Secure,
  // host-only attributes.
  const wrongCsrf = await hub.request({
    method: "POST",
    path: "/api/auth/logout",
    headers: { cookie: cookiePair, origin: hub.publicOrigin, "x-csrf-token": "wrong" },
  });
  assert.equal(wrongCsrf.status, 403);
  const logout = await hub.request({
    method: "POST",
    path: "/api/auth/logout",
    headers: { cookie: cookiePair, origin: hub.publicOrigin, "x-csrf-token": login.csrfToken },
  });
  assert.equal(logout.status, 200);
  const cleared = firstSetCookie(logout.headers["set-cookie"]);
  assert.ok(cleared);
  assert.match(cleared, /^__Host-rigscope_session=;/);
  assert.match(cleared, /;\s*Secure/i);
  assert.match(cleared, /HttpOnly/i);
  assert.match(cleared, /SameSite=Strict/i);
  assert.match(cleared, /Path=\//i);
  assert.match(cleared, /Max-Age=0/);
  assert.equal(/Domain=/i.test(cleared), false);
  assert.equal((await hub.request({ path: "/api/auth/session", headers: { cookie: cookiePair } })).status, 401);
});

test("remote collector upgrades and SSE: Origin rejection, bearer auth, scoped frames, reconnect/reset, logout closure", async (t) => {
  const visible = credential("c-visible", "m-visible", demoToken("remote-visible"));
  const hidden = credential("c-hidden", "m-hidden", demoToken("remote-hidden"));
  const hub = await startRemoteHub(t, {
    collectors: [visible, hidden],
    browserUsers: [{ username: "restricted", machineIds: [visible.machineId] }],
    loginUsername: "restricted",
  });
  const login = { headers: hub.browserHeaders, csrfToken: hub.csrfToken };

  // Collector upgrades never accept a browser Origin, including the hub's own.
  const anonymous = await wsUpgradeUnix(hub.socketPath, CONNECT_PATH, hub.hostHeader);
  assert.equal(anonymous.kind, "rejected");
  if (anonymous.kind === "rejected") assert.equal(anonymous.status, 401);
  const wrongHost = await wsUpgradeUnix(hub.socketPath, CONNECT_PATH, "evil.example.test", {
    authorization: `Bearer ${visible.token}`,
    "x-forwarded-host": hub.hostHeader,
  });
  assert.equal(wrongHost.kind, "rejected");
  if (wrongHost.kind === "rejected") assert.equal(wrongHost.status, 421, "spoofed forwarded host cannot authorize an upgrade");
  const withOrigin = await wsUpgradeUnix(hub.socketPath, CONNECT_PATH, hub.hostHeader, {
    authorization: `Bearer ${visible.token}`,
    origin: hub.publicOrigin,
  });
  assert.equal(withOrigin.kind, "rejected");
  if (withOrigin.kind === "rejected") assert.equal(withOrigin.status, 403);
  assert.equal((await hub.request({ path: CONNECT_PATH })).status, 426, "plain HTTP collector access is upgrade-only");

  // Production remote mode serves no framework upgrades, even authenticated
  // and exactly same-origin.
  const framework = await wsUpgradeUnix(hub.socketPath, "/_next/hmr", hub.hostHeader, {
    cookie: login.headers.cookie,
    origin: hub.publicOrigin,
  });
  assert.equal(framework.kind, "rejected");
  if (framework.kind === "rejected") assert.equal(framework.status, 403);

  const sse = await openUnixSse(hub.socketPath, "/api/live", {
    host: hub.hostHeader,
    cookie: login.headers.cookie,
  });
  assert.match(String(sse.response.headers["content-type"] ?? ""), /text\/event-stream/);
  assert.equal(sse.response.headers["x-accel-buffering"], "no");
  assert.match(String(sse.response.headers["cache-control"] ?? ""), /no-cache|no-store/);
  const initial = await sse.stream.next(8_000);
  assert.ok(initial);
  assert.equal(initial!.event, "snapshot");
  const initialSnapshot = parseSnapshot(initial!.data, "remote restricted snapshot");
  assert.deepEqual(initialSnapshot.machines.map((machine) => machine.machineId), [visible.machineId]);
  const initialId = initial!.id;

  // A visible collected sample is delivered; a hidden one is filtered before
  // serialization. The hidden insert generates no scoped frame.
  const visibleCollector = rawCollector(visible.collectorId, "visible-host");
  const visibleConn = await wsUpgradeUnix(hub.socketPath, CONNECT_PATH, hub.hostHeader, {
    authorization: `Bearer ${visible.token}`,
  });
  assert.equal(visibleConn.kind, "open");
  if (visibleConn.kind !== "open") return;
  t.after(() => wsTerminate(visibleConn.ws));
  await bindCollectorSession(visibleConn.ws, visibleCollector, "remote visible collector");

  const hiddenCollector = rawCollector(hidden.collectorId, "hidden-secret-host");
  const hiddenConn = await wsUpgradeUnix(hub.socketPath, CONNECT_PATH, hub.hostHeader, {
    authorization: `Bearer ${hidden.token}`,
  });
  assert.equal(hiddenConn.kind, "open");
  if (hiddenConn.kind !== "open") return;
  t.after(() => wsTerminate(hiddenConn.ws));
  await bindCollectorSession(hiddenConn.ws, hiddenCollector, "remote hidden collector");
  hiddenConn.ws.send(JSON.stringify(samplesMessage("live", [recordOf(hiddenCollector, hiddenCollector.seq)])));

  visibleConn.ws.send(JSON.stringify(samplesMessage("live", [recordOf(visibleCollector, visibleCollector.seq)])));

  let update = await sse.stream.next(8_000);
  while (update !== null && !update.data.includes(visibleCollector.collectorId)) {
    update = await sse.stream.next(8_000);
  }
  assert.ok(update, "the visible machine's update arrives over the Unix SSE stream");
  assert.ok(update!.data.includes("visible-host"));
  assert.equal(update!.data.includes("hidden-secret-host"), false, "hidden hostname never serialized");
  assert.equal(update!.data.includes(hidden.collectorId), false, "hidden collector identity never serialized");
  const visibleSnapshot = parseSnapshot(update!.data, "remote restricted update");
  assert.deepEqual(visibleSnapshot.machines.map((machine) => machine.machineId), [visible.machineId]);

  // Reconnect with Last-Event-ID always resets to a fresh, filtered snapshot.
  const reset = await openUnixSse(hub.socketPath, "/api/live", {
    host: hub.hostHeader,
    cookie: login.headers.cookie,
    "last-event-id": initialId ?? "unknown",
  });
  const resetEvent = await reset.stream.next(8_000);
  assert.ok(resetEvent);
  assert.equal(resetEvent!.event, "reset");
  const resetSnapshot = parseSnapshot(resetEvent!.data, "remote restricted reset");
  assert.deepEqual(resetSnapshot.machines.map((machine) => machine.machineId), [visible.machineId]);

  // Logout closes both existing streams rather than leaving an authorized
  // response open after the session is gone.
  const logout = await hub.request({
    method: "POST",
    path: "/api/auth/logout",
    headers: { cookie: login.headers.cookie, origin: hub.publicOrigin, "x-csrf-token": login.csrfToken },
  });
  assert.equal(logout.status, 200);
  assert.equal(await streamEnds(reset.stream, 2_000), true, "logout closes the reconnect stream");
  assert.equal(await streamEnds(sse.stream, 2_000), true, "logout closes the original stream");
  assert.equal((await hub.request({ path: "/api/auth/session", headers: { cookie: login.headers.cookie } })).status, 401);

  sse.stream.destroy();
  reset.stream.destroy();
  wsTerminate(visibleConn.ws);
  wsTerminate(hiddenConn.ws);
});

test("remote boundary gates enrollment before grants: Host first, browsers rejected, collector grant still works", async (t) => {
  const stateRoot = mkdtempSync(join(tmpdir(), "rigscope-remote-enroll-"));
  t.after(() => rmSync(stateRoot, { recursive: true, force: true }));
  const grant = demoToken("remote-enroll");
  const hub = await startRemoteHub(t, {
    browserUsers: false,
    env: {
      HUB_STATE_FILE: join(stateRoot, "hub", "enrollment.json"),
      HUB_ENROLLMENT_GRANTS: JSON.stringify([
        { token: grant, expiresAt: new Date(Date.now() + 5 * 60_000).toISOString() },
      ]),
    },
  });
  const enrollPath = "/api/collector/enroll";
  const enrollBody = JSON.stringify({ collectorId: "c-remote-enroll" });
  const baseHeaders = {
    authorization: `Bearer ${grant}`,
    "content-type": "application/json",
  };

  assert.equal(
    (await hub.request({
      method: "POST",
      path: enrollPath,
      headers: { ...baseHeaders, host: "evil.example.test" },
      body: enrollBody,
    })).status,
    421,
    "wrong Host is rejected before grant handling",
  );
  assert.equal(
    (await hub.request({
      method: "POST",
      path: enrollPath,
      headers: { ...baseHeaders, origin: "https://evil.example.test" },
      body: enrollBody,
    })).status,
    403,
    "foreign Origin is rejected before grant handling",
  );
  assert.equal(
    (await hub.request({
      method: "POST",
      path: enrollPath,
      headers: { ...baseHeaders, origin: hub.publicOrigin },
      body: enrollBody,
    })).status,
    403,
    "browser-origin enrollment is rejected even at the canonical origin",
  );

  const enrolled = await hub.request({
    method: "POST",
    path: enrollPath,
    headers: baseHeaders,
    body: enrollBody,
  });
  assert.equal(enrolled.status, 201, JSON.stringify(enrolled.json));
  const identity = enrolled.json as { collectorId: string; machineId: string; token: string };
  assert.equal(identity.collectorId, "c-remote-enroll");

  const conn = await wsUpgradeUnix(hub.socketPath, CONNECT_PATH, hub.hostHeader, {
    authorization: `Bearer ${identity.token}`,
  });
  assert.equal(conn.kind, "open", "the issued remote credential upgrades over the private socket");
  if (conn.kind !== "open") return;
  t.after(() => wsTerminate(conn.ws));
  const { welcome } = await bindCollectorSession(
    conn.ws,
    rawCollector(identity.collectorId, "remote-enrolled-host"),
    "remote enrolled collector",
  );
  assert.equal(welcome.machineId, identity.machineId);
});

test("remote shutdown closes SSE/collector sockets, exits zero, and removes its private socket", async (t) => {
  const token = demoToken("remote-shutdown");
  const hub = await startRemoteHub(t, {
    collectors: [credential("c-remote-shutdown", "m-remote-shutdown", token)],
  });
  const sse = await openUnixSse(hub.socketPath, "/api/live", {
    host: hub.hostHeader,
    cookie: hub.browserHeaders.cookie,
  });
  assert.ok(await sse.stream.next(8_000));
  const conn = await wsUpgradeUnix(hub.socketPath, CONNECT_PATH, hub.hostHeader, {
    authorization: `Bearer ${token}`,
  });
  assert.equal(conn.kind, "open");
  if (conn.kind !== "open") return;
  await bindCollectorSession(conn.ws, rawCollector("c-remote-shutdown", "shutdown-host"), "remote shutdown collector");
  const closed = expectClose(conn.ws, "remote shutdown collector close", 6_000);

  const exit = await within(hub.stop("SIGTERM", 6_000), 8_000, "remote hub shutdown");
  assert.deepEqual(exit, { code: 0, signal: null }, hub.child.dump());
  await closed;
  assert.equal(await streamEnds(sse.stream, 2_000), true, "shutdown ends the SSE stream");
  assert.equal(unixPathStats(hub.socketPath), null, "graceful shutdown removes the Unix socket");
});

test("remote startup fails closed for incomplete config, bad origins, occupied sockets, and unsafe parents", async (t) => {
  const root = mkdtempSync(join(tmpdir(), "rigscope-remote-invalid-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const privateDir = join(root, "private");
  mkdirSync(privateDir, { mode: 0o700 });
  const occupied = join(privateDir, "occupied.sock");
  writeFileSync(occupied, "not a socket", { mode: 0o600 });

  const looseDir = join(root, "loose");
  mkdirSync(looseDir, { mode: 0o700 });
  chmodSync(looseDir, 0o755);

  const cases: { name: string; env: Record<string, string> }[] = [
    { name: "origin without socket", env: { HUB_PUBLIC_ORIGIN: "https://hub.example.test" } },
    { name: "socket without origin", env: { HUB_SOCKET_PATH: join(privateDir, "none.sock") } },
    {
      name: "invalid public origin",
      env: { HUB_PUBLIC_ORIGIN: "https://hub.example.test/", HUB_SOCKET_PATH: join(privateDir, "bad-origin.sock") },
    },
    {
      name: "remote mode with a TCP port",
      env: {
        HUB_PUBLIC_ORIGIN: "https://hub.example.test",
        HUB_SOCKET_PATH: join(privateDir, "tcp.sock"),
        HUB_PORT: "8081",
      },
    },
    {
      name: "occupied socket path",
      env: { HUB_PUBLIC_ORIGIN: "https://hub.example.test", HUB_SOCKET_PATH: occupied },
    },
    {
      name: "non-private parent",
      env: { HUB_PUBLIC_ORIGIN: "https://hub.example.test", HUB_SOCKET_PATH: join(looseDir, "hub.sock") },
    },
  ];

  for (const { name, env } of cases) {
    const child = spawnRemoteCandidate(t, env);
    const info = await within(child.exit, 8_000, `${name}: startup termination`);
    assert.notEqual(info.code, 0, `${name}: must exit nonzero (got ${JSON.stringify(info)})`);
    const printed = [...child.stdoutLines(), ...child.stderrLines()].some(remoteListenMatch);
    assert.equal(printed, false, `${name}: must not print a listening line`);
    await child.stop("SIGKILL", 500);
  }

  // A live socket is occupied: a second hub must fail closed without
  // unlinking it or disturbing the first hub.
  const first = await startRemoteHub(t, {
    publicOrigin: "https://occupied.example.test",
  });
  const second = spawnRemoteCandidate(t, {
    HUB_PUBLIC_ORIGIN: "https://occupied.example.test",
    HUB_SOCKET_PATH: first.socketPath,
  });
  const secondExit = await within(second.exit, 8_000, "occupied socket startup termination");
  assert.notEqual(secondExit.code, 0, "a second hub on an occupied socket must exit nonzero");
  assert.equal(
    [...second.stdoutLines(), ...second.stderrLines()].some(remoteListenMatch),
    false,
    "the second hub must not print a listening line",
  );
  await second.stop("SIGKILL", 500);
  assert.equal((await first.request({ path: "/api/health" })).status, 200, "the first hub keeps serving");

  assert.equal(existsSync(occupied), true, "an occupied path is never deleted by failed startup");
});
