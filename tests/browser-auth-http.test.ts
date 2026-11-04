/**
 * Unit tests for the browser auth HTTP layer and the scoped live feed:
 * exact same-origin login/logout, CSRF, cookies, generic failures, request
 * bounds and rate limiting (BrowserHttpAuth), plus per-client snapshot
 * filtering and session-ending in LiveFeed. Run via the Node test runner.
 */

import { strict as assert } from "node:assert";
import { createServer, type Server } from "node:http";
import { describe, it } from "node:test";
import { Readable } from "node:stream";
import { hashPassword } from "../src/hub/auth/password";
import { parseBrowserUsers } from "../src/hub/auth/config";
import { BrowserHttpAuth, loopbackOriginHostnames, readJsonBody } from "../src/hub/auth/http";
import { BrowserSessionStore } from "../src/hub/auth/sessions";
import { LiveFeed, type LiveAccess } from "../src/hub/sse";
import { HubState } from "../src/hub/state";
import type { HubSnapshot } from "../src/contracts";
import { within } from "./runtime/support/deadline";

const USERS = parseBrowserUsers({
  HUB_BROWSER_USERS: JSON.stringify([
    { username: "demo", passwordHash: hashPassword("demo-password-local"), machineIds: "*" },
  ]),
});

it("decodes passwords correctly when UTF-8 characters cross request chunks", async () => {
  const body = { username: "demo", password: "a密码é" };
  const bytes = Buffer.from(JSON.stringify(body));
  const chunks = Array.from(bytes, byte => Buffer.from([byte]));
  const request = Readable.from(chunks) as import("node:http").IncomingMessage;
  assert.deepEqual(await readJsonBody(request), { ok: true, value: body });
});

it("the expiry sweep closes an idle session's stream without requiring another sample", async t => {
  const state = new HubState({ collectors: [], hubRunId: "idle-hub" });
  const feed = new LiveFeed(state, { keepaliveMs: 60_000 });
  let now = 0;
  const sessions = new BrowserSessionStore({ now: () => now, ttlMs: 1_000, sweepMs: 10,
    onEnd: session => { feed.endSessionStreams(session.id); } });
  const session = sessions.create("demo", "*");
  const server = createServer((req, res) => feed.attach(req, res, { sessionId: session.id, scope: "*" }));
  t.after(() => { sessions.dispose(); feed.stop(); server.closeAllConnections(); server.close(); });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const address = server.address() as { port: number };
  const response = await fetch(`http://127.0.0.1:${address.port}`);
  const reader = response.body!.getReader();
  assert.equal((await reader.read()).done, false);
  now = 1_000;
  assert.equal((await within(reader.read(), 2_000, "idle stream expiry")).done, true);
  assert.equal(sessions.find(session.id), null);
  assert.equal(feed.clientCount(), 0);
});

/** Loopback server driving ONLY the /api/auth/* endpoints. */
async function startAuthServer(
  input: { users?: ReturnType<typeof parseBrowserUsers>; ttlMs?: number; now?: () => number } = {},
): Promise<{ server: Server; origin: string; sessions: BrowserSessionStore; dispose: () => void }> {
  const sessions = new BrowserSessionStore({
    ttlMs: input.ttlMs ?? 60_000,
    sweepMs: 0, // unit tests drive expiration directly; runtime sweeps at 1s
    now: input.now,
  });
  let auth: BrowserHttpAuth;
  const server = createServer((req, res) => {
    void auth.handleApiAuth(req, res, new URL(req.url ?? "/", "http://hub.invalid").pathname);
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  assert.ok(address !== null && typeof address === "object" && !Array.isArray(address));
  const origin = `http://127.0.0.1:${address.port}`;
  // Same-origin rules always match the listening (ephemeral) port.
  auth = new BrowserHttpAuth({
    users: input.users ?? USERS,
    sessions,
    origin: { originPort: address.port, originHostnames: loopbackOriginHostnames("127.0.0.1") },
  });
  return { server, origin, sessions, dispose: () => sessions.dispose() };
}

async function jsonPost(
  url: string,
  headers: Record<string, string>,
  body: unknown,
): Promise<{ status: number; json: unknown; setCookie: string | null }> {
  const response = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...headers },
    body: typeof body === "string" ? body : JSON.stringify(body),
  });
  const setCookie = response.headers.get("set-cookie");
  let json: unknown = null;
  try {
    json = await response.json();
  } catch {
    // Empty or non-JSON body on some error paths.
  }
  return { status: response.status, json, setCookie };
}

function cookiePair(setCookie: string): string {
  return setCookie.split(";", 1)[0];
}

describe("browser auth http", () => {
  it("issues an opaque scoped cookie and a payload without credentials in it", async t => {
    const hub = await startAuthServer();
    t.after(() => { hub.server.closeAllConnections(); hub.server.close(); hub.dispose(); });
    const result = await jsonPost(`${hub.origin}/api/auth/login`, { Origin: hub.origin }, {
      username: "demo",
      password: "demo-password-local",
    });
    assert.equal(result.status, 200);
    const payload = result.json as Record<string, unknown>;
    assert.equal(payload.username, "demo");
    assert.equal(typeof payload.csrfToken, "string");
    assert.ok((payload.csrfToken as string).length >= 32);
    assert.equal(JSON.stringify(payload).includes("password"), false, "no credentials in payload");
    const setCookie = result.setCookie;
    assert.ok(setCookie);
    const pair = cookiePair(setCookie);
    assert.match(pair, /^rigscope_session=[A-Za-z0-9_-]{43}$/);
    assert.match(setCookie, /HttpOnly/i);
    assert.match(setCookie, /SameSite=Strict/i);
    assert.match(setCookie, /Path=\//i);
    assert.equal(/;\s*Secure/i.test(setCookie), false, "loopback plaintext is explicit; no Secure attr yet");
    assert.match(setCookie, /Max-Age=60/, "cookie window mirrors the store TTL");
  });

  it("rejects bad and unknown credentials generically with no cookie", async t => {
    const hub = await startAuthServer();
    t.after(() => { hub.server.closeAllConnections(); hub.server.close(); hub.dispose(); });
    for (const body of [
      { username: "demo", password: "wrong-password" },
      { username: "no-such-user", password: "demo-password-local" },
    ]) {
      const result = await jsonPost(`${hub.origin}/api/auth/login`, { Origin: hub.origin }, body);
      assert.equal(result.status, 401);
      assert.deepEqual(result.json, { error: "invalid username or password" });
      assert.equal(result.setCookie, null);
    }
    // Wrong shape/size/type are bad requests, not credential probes.
    for (const [body, status] of [
      [{ username: "demo" }, 400],
      [{ password: "demo-password-local" }, 400],
      [{ username: "demo", password: "demo-password", extra: true }, 400],
      ["[]", 400],
      ["not json", 400],
    ] as const) {
      const result = await jsonPost(`${hub.origin}/api/auth/login`, { Origin: hub.origin }, body);
      assert.equal(result.status, status);
      assert.equal(result.setCookie, null);
    }
  });

  it("requires exact same-origin for login", async t => {
    const hub = await startAuthServer();
    t.after(() => { hub.server.closeAllConnections(); hub.server.close(); hub.dispose(); });
    for (const [headers, label] of [
      [{}, "missing origin"],
      [{ Origin: "http://127.0.0.1:9999" }, "wrong port"],
      [{ Origin: "https://127.0.0.1:9999" }, "wrong scheme"],
      [{ Origin: "http://hostile.invalid" }, "foreign host"],
      [{ Origin: "null" }, "sandboxed origin"],
      [{ Origin: "http://rebind.invalid" }, "rebind host"],
      [{ Origin: "http://evil.example/path" }, "origin with path"],
    ] as const) {
      const result = await jsonPost(`${hub.origin}/api/auth/login`, { ...headers }, {
        username: "demo",
        password: "demo-password-local",
      });
      assert.equal(result.status, 403, `${label} must be rejected`);
      assert.equal(result.setCookie, null, `${label} must not produce a cookie`);
    }
    // A loopback alias is fine when it is used consistently in BOTH the
    // request URL (Host header) and the Origin header.
    const port = hub.origin.split(":")[2];
    const alias = await jsonPost(`http://localhost:${port}/api/auth/login`, { Origin: `http://localhost:${port}` }, {
      username: "demo",
      password: "demo-password-local",
    });
    assert.equal(alias.status, 200);
    // But mixing hosts (Origin localhost over a 127.0.0.1 request) is not same-origin.
    const mixed = await jsonPost(`${hub.origin}/api/auth/login`, { Origin: `http://localhost:${port}` }, {
      username: "demo",
      password: "demo-password-local",
    });
    assert.equal(mixed.status, 403);
  });

  it("bounds request bodies and rejects wrong methods and content types", async t => {
    const hub = await startAuthServer();
    t.after(() => { hub.server.closeAllConnections(); hub.server.close(); hub.dispose(); });
    const getLogin = await fetch(`${hub.origin}/api/auth/login`);
    assert.equal(getLogin.status, 405);
    const wrongType = await jsonPost(`${hub.origin}/api/auth/login`, { Origin: hub.origin, "Content-Type": "text/plain" }, "{}");
    assert.equal(wrongType.status, 415);
    const tooBig = await fetch(`${hub.origin}/api/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: hub.origin },
      body: JSON.stringify({ username: "demo", password: "x".repeat(5000) }),
    });
    assert.equal(tooBig.status, 413);
    const oversizedUsername = await jsonPost(`${hub.origin}/api/auth/login`, { Origin: hub.origin }, {
      username: "x".repeat(129),
      password: "y",
    });
    assert.equal(oversizedUsername.status, 400);
  });

  it("answers session/logout consistently, including when not configured", async t => {
    const hub = await startAuthServer();
    const none = await startAuthServer({ users: [] });
    t.after(() => {
      hub.server.closeAllConnections(); hub.server.close(); hub.dispose();
      none.server.closeAllConnections(); none.server.close(); none.dispose();
    });
    // From the real origin with valid credentials:
    const login = await jsonPost(`${hub.origin}/api/auth/login`, { Origin: hub.origin }, {
      username: "demo", password: "demo-password-local",
    });
    assert.equal(login.status, 200);
    const cookie = cookiePair(login.setCookie!);
    const session = await fetch(`${hub.origin}/api/auth/session`, { headers: { cookie } });
    assert.equal(session.status, 200);
    const payload = await session.json() as Record<string, string>;
    assert.equal(payload.username, "demo");
    assert.equal(typeof payload.csrfToken, "string");
    // Foreign/handmade cookies are unknown sessions.
    assert.equal((await fetch(`${hub.origin}/api/auth/session`, { headers: { cookie: `rigscope_session=${"A".repeat(43)}` } })).status, 401);
    // Signed out: /api/auth/session is 401; wrong methods are 405.
    assert.equal((await fetch(`${hub.origin}/api/auth/session`)).status, 401);
    assert.equal((await fetch(`${hub.origin}/api/auth/session`, { method: "POST" })).status, 405);
    // Logout requires X-CSRF-Token; failures leave the session alive.
    const badExit = await fetch(`${hub.origin}/api/auth/logout`, {
      method: "POST", headers: { cookie, Origin: hub.origin },
    });
    assert.equal(badExit.status, 403);
    const wrongExit = await fetch(`${hub.origin}/api/auth/logout`, {
      method: "POST", headers: { cookie, Origin: hub.origin, "X-CSRF-Token": `${payload.csrfToken!}00` },
    });
    assert.equal(wrongExit.status, 403);
    assert.equal((await fetch(`${hub.origin}/api/auth/session`, { headers: { cookie } })).status, 200);
    // Logout without Origin is rejected before any cookie handling.
    const crossExit = await fetch(`${hub.origin}/api/auth/logout`, {
      method: "POST", headers: { cookie, "X-CSRF-Token": payload.csrfToken! },
    });
    assert.equal(crossExit.status, 403);
    assert.equal((await fetch(`${hub.origin}/api/auth/session`, { headers: { cookie } })).status, 200);
    // Correct logout: clears session and cookie.
    const logout = await fetch(`${hub.origin}/api/auth/logout`, {
      method: "POST", headers: { cookie, Origin: hub.origin, "X-CSRF-Token": payload.csrfToken! },
    });
    assert.equal(logout.status, 200);
    assert.ok(logout.headers.get("set-cookie")!.startsWith("rigscope_session=;"));
    assert.match(logout.headers.get("set-cookie")!, /Max-Age=0/);
    assert.equal((await fetch(`${hub.origin}/api/auth/session`, { headers: { cookie } })).status, 401);
    // Repeated logout with the old cookie: 401 (and a cleared cookie).
    assert.equal(
      (await fetch(`${hub.origin}/api/auth/logout`, { method: "POST", headers: { cookie, Origin: hub.origin, "X-CSRF-Token": payload.csrfToken! } })).status,
      401,
    );

    // Not configured: login/session fail closed with 503.
    assert.equal((await fetch(`${none.origin}/api/auth/session`)).status, 503);
    assert.equal(
      (await fetch(`${none.origin}/api/auth/login`, { method: "POST", headers: { "Content-Type": "application/json", Origin: none.origin }, body: "{}" })).status,
      503,
    );
    assert.equal(
      (await fetch(`${none.origin}/api/auth/logout`, { method: "POST", headers: { "Content-Type": "application/json", Origin: none.origin }, body: "{}" })).status,
      503,
    );
  });

  it("rate limits a login brute force; failed attempts never set cookies", async t => {
    const hub = await startAuthServer();
    t.after(() => { hub.server.closeAllConnections(); hub.server.close(); hub.dispose(); });
    const attempts = await Promise.all(
      Array.from({ length: 30 }, (_, i) =>
        jsonPost(`${hub.origin}/api/auth/login`, { Origin: hub.origin }, {
          username: "demo",
          password: `attempt-${i}`,
        })),
    );
    assert.equal(attempts.filter((attempt) => attempt.status === 401).length > 0, true);
    assert.equal(attempts.filter((attempt) => attempt.status === 429).length > 0, true, "a burst beyond the token bucket must hit 429");
    assert.equal(attempts.every((attempt) => attempt.setCookie === null), true, "failed attempts never set cookies");
    // The bucket recovers and a correct login succeeds again afterwards.
    await new Promise((resolve) => setTimeout(resolve, 1_100));
    const after = await jsonPost(`${hub.origin}/api/auth/login`, { Origin: hub.origin }, {
      username: "demo",
      password: "demo-password-local",
    });
    assert.equal(after.status, 200);
  });

  it("expires sessions by the injected clock (no sliding renewal)", async t => {
    let clock = 1_000_000;
    const hub = await startAuthServer({ ttlMs: 50, now: () => clock });
    t.after(() => { hub.server.closeAllConnections(); hub.server.close(); hub.dispose(); });
    const login = await jsonPost(`${hub.origin}/api/auth/login`, { Origin: hub.origin }, {
      username: "demo", password: "demo-password-local",
    });
    assert.equal(login.status, 200);
    const cookie = cookiePair(login.setCookie!);
    const alive = await fetch(`${hub.origin}/api/auth/session`, { headers: { cookie } });
    assert.equal(alive.status, 200);
    // Fixed expiration: session activity does not extend the deadline.
    await fetch(`${hub.origin}/api/auth/session`, { headers: { cookie } });
    clock += 100;
    await hub.sessions.sweepExpired();
    assert.equal((await fetch(`${hub.origin}/api/auth/session`, { headers: { cookie } })).status, 401);
    assert.equal(hub.sessions.size(), 0, "no sessions survive expiry");
  });
});

describe("live feed session filtering (unit)", () => {
  type Frame = { event: string; id: string; data: string };

  /** Minimal SSE consumer for unit assertions. */
  function readStream(url: string, headers: Record<string, string> = {}): {
    current: () => Frame[];
    response: Promise<Response>;
    abort: () => void;
  } {
    const controller = new AbortController();
    const collected: Frame[] = [];
    const response = fetch(url, { headers, signal: controller.signal });
    void (async () => {
      try {
        const res = await response;
        const body = res.body!.getReader() as ReadableStreamDefaultReader<Uint8Array>;
        const decoder = new TextDecoder();
        let buffer = "";
        while (true) {
          const { done, value } = await body.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true });
          let boundary = buffer.indexOf("\n\n");
          while (boundary >= 0) {
            const frame = buffer.slice(0, boundary);
            buffer = buffer.slice(boundary + 2);
            const event = /^event: (.*)$/m.exec(frame)?.[1] ?? "";
            const id = /^id: (.*)$/m.exec(frame)?.[1] ?? "";
            const data = /^data: (.*)$/m.exec(frame)?.[1] ?? "";
            if (event !== "" || data !== "") collected.push({ event, id, data });
            boundary = buffer.indexOf("\n\n");
          }
        }
      } catch {
        // aborted — remaining frames stay as collected
      }
    })();
    return { current: () => [...collected], response, abort: () => controller.abort() };
  }

  async function waitFor(
    name: string,
    ready: () => boolean,
    timeoutMs = 5_000,
  ): Promise<void> {
    const started = Date.now();
    while (!ready()) {
      if (Date.now() - started > timeoutMs) throw new Error(`timed out waiting for ${name}`);
      await new Promise((resolve) => setTimeout(resolve, 25));
    }
  }

  it("filters initial/update/reset frames server-side and ends the owning session's streams", async () => {
    const state = new HubState({
      collectors: [
        { collectorId: "c-a", machineId: "m-a", token: "t-a" },
        { collectorId: "c-b", machineId: "m-b", token: "t-b" },
      ],
      hubRunId: "hub-run",
      now: () => new Date(0),
    });
    const feed = new LiveFeed(state, { keepaliveMs: 60_000 });
    const server = createServer((req, res) => {
      const scope = new URL(req.url ?? "/", "http://hub.invalid").searchParams.get("scope");
      const access: LiveAccess =
        scope === "a"
          ? { sessionId: "session-a", scope: new Set(["m-a"]) }
          : { sessionId: "session-full", scope: "*" };
      feed.attach(req, res, access);
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    assert.ok(address !== null && typeof address === "object");
    const base = `http://127.0.0.1:${address.port}`;

    const scoped = readStream(`${base}/?scope=a`);
    const full = readStream(`${base}/?scope=all`);
    const scopedResponse = await scoped.response;
    assert.equal(scopedResponse.status, 200);
    const [initialScoped] = scoped.current().length > 0
      ? scoped.current().slice(0, 1)
      : await (async () => { await waitFor("initial scoped frame", () => scoped.current().length >= 1); return scoped.current().slice(0, 1); })();
    assert.equal(initialScoped.event, "snapshot");
    const scopedSnapshot = JSON.parse(initialScoped.data) as HubSnapshot;
    assert.deepEqual(scopedSnapshot.machines.map((machine) => machine.machineId), ["m-a"]);

    // Machine b connects and reports; the scoped client must never see it.
    state.startSession({
      sessionId: "s-a", collectorId: "c-a", runId: "r-a", bootId: "b-a", hostname: "host-a",
      expectedIntervalMs: null,
    });
    state.startSession({
      sessionId: "s-b", collectorId: "c-b", runId: "r-b", bootId: "b-b", hostname: "host-b-secret",
      expectedIntervalMs: null,
    });
    await waitFor("updates", () => scoped.current().length >= 2 && full.current().length >= 2);
    const fullFrames = full.current();
    assert.equal(fullFrames[1].event, "update");
    const fullUpdate = JSON.parse(fullFrames[1].data) as HubSnapshot;
    assert.deepEqual(fullUpdate.machines.map((machine) => machine.machineId), ["m-a", "m-b"]);
    const scopedFrames = scoped.current();
    assert.equal(scopedFrames.length, 2, "hidden hello creates no scoped frame");
    const scopedUpdate = JSON.parse(scopedFrames[1].data) as HubSnapshot;
    assert.deepEqual(scopedUpdate.machines.map((machine) => machine.machineId), ["m-a"]);
    const scopedSerialized = JSON.stringify(scopedFrames);
    assert.equal(scopedSerialized.includes("m-b"), false);
    assert.equal(scopedSerialized.includes("c-b"), false);
    assert.equal(scopedSerialized.includes("host-b-secret"), false);

    // Reconnect (Last-Event-ID) resets are filtered as well.
    const reset = readStream(`${base}/?scope=a`, { "last-event-id": "hub-run:0" });
    const resetResponse = await reset.response;
    assert.equal(resetResponse.status, 200);
    await waitFor("reset frame", () => reset.current().length >= 1);
    const resetFrame = reset.current()[0];
    assert.equal(resetFrame.event, "reset");
    const resetSnapshot = JSON.parse(resetFrame.data) as HubSnapshot;
    assert.deepEqual(resetSnapshot.machines.map((machine) => machine.machineId), ["m-a"]);

    // End the reset client first so its socket closes before session counts.
    reset.abort();
    await waitFor("reset client to disconnect", () => feed.clientCount() === 2);

    // Ending the owning session closes exactly its own stream.
    assert.equal(feed.endSessionStreams("session-a"), 1);
    assert.equal(feed.endSessionStreams("missing-session"), 0);
    assert.equal(feed.clientCount(), 1, "only the owning session's stream ended; full stays");
    scoped.abort();
    full.abort();
    feed.stop();
    server.closeAllConnections();
    server.close();
  });

  it("rejects clients without explicit authorization", async () => {
    const state = new HubState({ collectors: [], hubRunId: "hub-run", now: () => new Date(0) });
    const feed = new LiveFeed(state, { keepaliveMs: 60_000 });
    const server = createServer((req, res) => {
      feed.attach(req, res, req.headers["x-authorize"] === "1" ? { sessionId: "s", scope: "*" } : null);
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    assert.ok(address !== null && typeof address === "object");
    const base = `http://127.0.0.1:${address.port}`;
    const denied = await fetch(base);
    assert.equal(denied.status, 401);
    assert.equal(/event-stream/.test(denied.headers.get("content-type") ?? ""), false);
    const allowed = await fetch(base, { headers: { "x-authorize": "1" } });
    assert.equal(allowed.status, 200);
    await allowed.body!.cancel();
    feed.stop();
    server.closeAllConnections();
    server.close();
  });
});
