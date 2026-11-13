import assert from "node:assert/strict";
import test from "node:test";
import { BROWSER_PASSWORD, loginBrowser } from "./support/browser";
import { credential, demoToken } from "./support/config";
import { startHub } from "./support/fixtures";
import { within } from "./support/deadline";
import { SseClient } from "./support/sse";
import { assertWelcome, helloOf, parseSnapshot, rawCollector, recordOf, samplesMessage, waitMachineState } from "./support/wire";
import { bindCollectorSession, nextWsJson, wsUpgrade, wsTerminate } from "./support/ws";

test("browser boundary fails closed without users; collector and browser credentials are separate", async t => {
  const hub = await startHub(t, { browserUsers: false });
  assert.equal((await fetch(hub.healthUrl)).status, 200);
  const live = await fetch(hub.liveUrl);
  assert.equal(live.status, 503);
  assert.doesNotMatch(live.headers.get("content-type") ?? "", /event-stream/);
  const session = await fetch(`${hub.baseUrl}/api/auth/session`);
  assert.equal(session.status, 503);
  // The static export has no server-side framework layer: the unauthenticated
  // root is the export itself and the client routes to /login. The API paths
  // below are the fail-closed contract.
  const login = await fetch(`${hub.baseUrl}/login`);
  assert.equal(login.status, 200);

  const configured = await startHub(t, { collectors: [credential("c-private", "m-private", demoToken("private"))] });
  assert.equal((await fetch(configured.liveUrl)).status, 401);
  assert.equal((await fetch(configured.liveUrl, {
    headers: { ...configured.browserHeaders, Origin: "http://127.0.0.1:12345" },
  })).status, 403, "cross-origin EventSources cannot consume session streams");
  assert.equal((await fetch(configured.liveUrl, { headers: { Authorization: "Bearer demo-token-private" } })).status, 401);
  const upgrade = await wsUpgrade(configured.collectorWsUrl, configured.browserHeaders);
  assert.equal(upgrade.kind, "rejected", "browser cookie must not authorize a collector");
  const otherHub = await startHub(t);
  assert.equal((await fetch(otherHub.liveUrl, { headers: configured.browserHeaders })).status, 401, "sessions are hub-local");
});

test("scoped browser streams filter snapshot/update/reset and logout terminates existing access", async t => {
  const permitted = credential("c-visible", "m-visible", demoToken("visible"));
  const hidden = credential("c-hidden", "m-hidden", demoToken("hidden"));
  const hub = await startHub(t, {
    collectors: [permitted, hidden],
    browserUsers: [
      { username: "restricted", machineIds: [permitted.machineId] },
      { username: "empty", machineIds: [] },
    ],
  });
  const session = await loginBrowser(hub.baseUrl, "restricted");
  const live = new SseClient(hub.liveUrl, session.headers);
  t.after(() => live.close());
  const initial = await live.next();
  assert.equal(initial.event, "snapshot");
  assert.deepEqual(parseSnapshot(initial.data, "restricted initial").machines.map(m => m.machineId), [permitted.machineId]);
  assert.ok(!initial.data.includes(hidden.collectorId), "hidden collector identity never serialized");

  const collector = rawCollector(hidden.collectorId, "private-hostname");
  const conn = await wsUpgrade(hub.collectorWsUrl, { Authorization: `Bearer ${hidden.token}` });
  assert.equal(conn.kind, "open");
  if (conn.kind !== "open") return;
  t.after(() => wsTerminate(conn.ws));
  const hiddenWelcome = nextWsJson(conn.ws, 5_000);
  conn.ws.send(JSON.stringify({ ...helloOf(collector), expectedIntervalMs: 34_567 }));
  assertWelcome((await hiddenWelcome)?.data, "hidden collector welcome");
  conn.ws.send(JSON.stringify(samplesMessage("live", [recordOf(collector, collector.seq)])));

  const visibleCollector = rawCollector(permitted.collectorId, "visible-hostname");
  const visible = await wsUpgrade(hub.collectorWsUrl, { Authorization: `Bearer ${permitted.token}` });
  assert.equal(visible.kind, "open");
  if (visible.kind !== "open") return;
  t.after(() => wsTerminate(visible.ws));
  await bindCollectorSession(visible.ws, visibleCollector, "visible collector");
  const record = recordOf(visibleCollector, 1);
  (record.payload as Record<string, unknown>) = {
    cpu: { status: "ok", usagePercent: 12.5, perCore: [12.5] },
    memory: { status: "unavailable", reason: "withheld by local sharing policy" },
    swap: { status: "unavailable", reason: "withheld by local sharing policy" },
    gpus: { status: "unavailable", reason: "withheld by local sharing policy" },
    disks: { status: "unavailable", reason: "withheld by local sharing policy" },
    network: { status: "unavailable", reason: "withheld by local sharing policy" },
  };
  visible.ws.send(JSON.stringify(samplesMessage("live", [record])));
  const delivered = await waitMachineState(live, 5_000, { machineId: permitted.machineId, withMetrics: true });
  assert.equal(delivered.snapshot.machines.length, 1);
  assert.equal(delivered.snapshot.machines[0].expectedIntervalMs, 1_000, "hello without an explicit cadence advertises the default");
  assert.deepEqual(delivered.snapshot.machines[0].metrics?.payload, record.payload, "authorized metrics preserve honest unavailable states");
  assert.ok(!delivered.event.data.includes("private-hostname"));
  assert.ok(!delivered.event.data.includes('"expectedIntervalMs":34567'), "hidden cadence is never serialized");
  const cursor = (id: string) => BigInt(id.slice(id.lastIndexOf(":") + 1));
  assert.ok(cursor(delivered.event.id!) > cursor(initial.id!) && cursor(delivered.event.id!) <= cursor(initial.id!) + 2n, "only visible changes advance the cursor, with adjacent changes coalesced");

  const reset = new SseClient(hub.liveUrl, { ...session.headers, "Last-Event-ID": initial.id! });
  t.after(() => reset.close());
  const resetEvent = await reset.next();
  assert.equal(resetEvent.event, "reset");
  assert.deepEqual(parseSnapshot(resetEvent.data, "restricted reset").machines.map(m => m.machineId), [permitted.machineId]);
  const emptySession = await loginBrowser(hub.baseUrl, "empty");
  const empty = await fetch(hub.liveUrl, { headers: emptySession.headers });
  assert.equal(empty.status, 403, "explicitly empty scope is permission failure, not an empty hub");
  assert.doesNotMatch(empty.headers.get("content-type") ?? "", /event-stream/);

  const forbidden = await fetch(`${hub.baseUrl}/api/auth/logout`, {
    method: "POST", headers: { ...session.headers, Origin: hub.baseUrl, "X-CSRF-Token": "invalid" },
  });
  assert.equal(forbidden.status, 403);
  assert.equal((await fetch(`${hub.baseUrl}/api/auth/session`, { headers: session.headers })).status, 200);
  const logout = await fetch(`${hub.baseUrl}/api/auth/logout`, {
    method: "POST", headers: { ...session.headers, Origin: hub.baseUrl, "X-CSRF-Token": session.csrfToken },
  });
  assert.ok([200, 204].includes(logout.status));
  assert.equal(await within(live.settled(), 2_000, "logout closes existing SSE"), "done");
  assert.equal(await within(reset.settled(), 2_000, "logout closes reconnect SSE"), "done");
  assert.equal((await fetch(hub.liveUrl, { headers: session.headers })).status, 401);
});

test("browser login requires exact same origin and rejects bad passwords without a cookie", async t => {
  const hub = await startHub(t);
  for (const origin of [undefined, "https://hostile.invalid", "null"]) {
    const response = await fetch(`${hub.baseUrl}/api/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json", ...(origin ? { Origin: origin } : {}) },
      body: JSON.stringify({ username: "demo-viewer", password: BROWSER_PASSWORD }),
    });
    assert.equal(response.status, 403);
    assert.equal(response.headers.get("set-cookie"), null);
  }
  const response = await fetch(`${hub.baseUrl}/api/auth/login`, {
    method: "POST", headers: { "Content-Type": "application/json", Origin: hub.baseUrl },
    body: JSON.stringify({ username: "demo-viewer", password: "wrong-password" }),
  });
  assert.equal(response.status, 401);
  assert.equal(response.headers.get("set-cookie"), null);
});
