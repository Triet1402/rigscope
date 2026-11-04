import assert from "node:assert/strict";
import test from "node:test";
import { registerCleanup } from "./support/cleanup";
import { CONNECT_PATH, credential, demoToken } from "./support/config";
import { startHub } from "./support/fixtures";
import { httpsJson, openHttpsSse } from "./support/https";
import { startTlsProxy } from "./support/tls-proxy";
import { findMachine, parseSnapshot, rawCollector, recordOf, samplesMessage } from "./support/wire";
import { bindCollectorSession, wsTerminate, wsUpgrade } from "./support/ws";

test("loopback TLS proxy fixture: HTTPS health, SSE and WSS hello/welcome/sample forwarding with normal certificate validation", async (t) => {
  const token = demoToken("tls");
  const hub = await startHub(t, { collectors: [credential("c-tls", "m-tls", token)] });

  const proxy = await startTlsProxy(hub);
  if (proxy === null) {
    t.skip("openssl CLI unavailable: local TLS forwarding probe not run (documented in README known gaps)");
    return;
  }

  const collector = rawCollector("c-tls", "demo-node-tls");

  // HTTPS health through the TLS proxy; CA pinned to the fixture cert so
  // verification stays fully enabled.
  const healthJson = await httpsJson(proxy.port, "/api/health", proxy.caPem);
  assert.equal(healthJson.status, 200, "health is reachable through the TLS proxy");
  assert.equal((healthJson.body as { mode?: string }).mode, "runtime-spike", "health body intact through TLS");

  // SSE through the TLS proxy: initial snapshot event with exact shape.
  const proxied = await openHttpsSse(proxy.port, "/api/live", proxy.caPem, 8_000, hub.browserHeaders);
  registerCleanup(() => proxied.stream.destroy());
  assert.ok(
    String(proxied.response.headers["content-type"] ?? "").includes("text/event-stream"),
    "SSE content-type survives proxying",
  );
  const initial = await proxied.stream.next(8_000);
  assert.ok(initial !== null, "initial snapshot arrives through the TLS proxy");
  if (initial !== null) parseSnapshot(initial.data, `proxied ${initial.event}`);

  // WSS through the TLS proxy with the same pinned CA: welcome arrives.
  const upgraded = await wsUpgrade(
    `wss://127.0.0.1:${proxy.port}${CONNECT_PATH}`,
    { authorization: `Bearer ${token}` },
    proxy.caPem,
  );
  assert.equal(upgraded.kind, "open", "collector upgrade over TLS works with normal validation and a pinned CA");
  if (upgraded.kind !== "open") {
    proxied.stream.destroy();
    await proxy.close();
    return;
  }
  // hello/welcome over proxied TLS; the helper tolerates both handshake orderings.
  const { welcome } = await bindCollectorSession(upgraded.ws, collector, "proxied TLS handshake");
  assert.ok(welcome !== undefined, "welcome received over proxied TLS");
  if (welcome !== undefined) {
    assert.equal(welcome.machineId, "m-tls", "identity mapping intact over TLS");
  }

  // Push one record through the TLS proxy and watch the proxied SSE update.
  upgraded.ws.send(JSON.stringify(samplesMessage("live", [recordOf(collector, collector.seq)])));

  const end = Date.now() + 20_000;
  let delivered = false;
  while (Date.now() < end) {
    const remaining = end - Date.now();
    if (remaining <= 0) break;
    const event = await proxied.stream.next(remaining);
    if (event === null) break;
    if (event.event !== "snapshot" && event.event !== "update") continue;
    const snapshot = parseSnapshot(event.data, `proxied ${event.event}`);
    if (findMachine(snapshot, { machineId: "m-tls", withMetrics: true }) !== undefined) {
      delivered = true;
      break;
    }
  }
  assert.ok(delivered, "a record pushed over TLS surfaces in the SSE stream through the TLS proxy");

  wsTerminate(upgraded.ws);
  proxied.stream.destroy();
  await proxy.close();
});
