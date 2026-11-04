import assert from "node:assert/strict";
import test from "node:test";
import { credential, demoToken } from "./support/config";
import { within } from "./support/deadline";
import { startHub } from "./support/fixtures";
import { expectPortClosed } from "./support/http";
import { SseClient } from "./support/sse";
import { rawCollector } from "./support/wire";
import { bindCollectorSession, expectClose, wsUpgrade } from "./support/ws";

for (const signal of ["SIGTERM", "SIGINT"] as const) {
  test(`hub ${signal} closes active SSE and WS clients, exits zero and releases its port`, async t => {
    const token = demoToken(signal);
    const hub = await startHub(t, { collectors: [credential("c-shutdown", "m-shutdown", token)] });
    const live = new SseClient(hub.liveUrl, hub.browserHeaders);
    t.after(() => live.close());
    await live.next();
    const connection = await wsUpgrade(hub.collectorWsUrl, { authorization: `Bearer ${token}` });
    assert.equal(connection.kind, "open");
    if (connection.kind !== "open") return;
    await bindCollectorSession(connection.ws, rawCollector("c-shutdown", "mock-shutdown"), signal);
    const socketClosed = expectClose(connection.ws, `${signal} socket close`, 5000);
    const [exit] = await Promise.all([
      hub.stop(signal, 6000),
      socketClosed,
      within(live.settled(), 6000, `${signal} SSE close`),
    ]);
    assert.deepEqual(exit, { code: 0, signal: null }, hub.child.dump());
    await expectPortClosed(hub.port);
  });
}
