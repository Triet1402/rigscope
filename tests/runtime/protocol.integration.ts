import assert from "node:assert/strict";
import test from "node:test";
import { credential, demoToken } from "./support/config";
import { startHub } from "./support/fixtures";
import { helloOf, rawCollector, recordOf, samplesMessage } from "./support/wire";
import { bindCollectorSession, expectClose, isWsOpen, wsTerminate, wsUpgrade } from "./support/ws";

test("collector protocol bounds reject malformed, misordered, oversized and invalid identity messages", async t => {
  const token = demoToken("bounds");
  const hub = await startHub(t, { collectors: [credential("c-bounds", "m-bounds", token)] });
  const collector = rawCollector("c-bounds", "mock-bounds");
  const cases = [
    { name: "records before hello", hello: false,
      message: JSON.stringify(samplesMessage("live", [recordOf(collector, 1)])) },
    { name: "malformed JSON", hello: true, message: "{not json" },
    { name: "forged hello identity", hello: false,
      message: JSON.stringify(helloOf(rawCollector("c-imposter", "mock-imposter"))) },
    { name: "record identity mismatch", hello: true,
      message: JSON.stringify(samplesMessage("live", [recordOf(rawCollector("c-imposter", "mock-imposter"), 1)])) },
    { name: "batch above 32", hello: true,
      message: JSON.stringify(samplesMessage("live",
        Array.from({ length: 33 }, (_, i) => recordOf(collector, i + 1)))) },
    { name: "text frame above 1MiB bytes", hello: true, message: "!".repeat(1_048_577), code: 1009 },
  ];
  for (const scenario of cases) {
    await t.test(scenario.name, async () => {
      const connection = await wsUpgrade(hub.collectorWsUrl, { authorization: `Bearer ${token}` });
      assert.equal(connection.kind, "open");
      if (connection.kind !== "open") return;
      if (scenario.hello) await bindCollectorSession(connection.ws, collector, scenario.name);
      const closed = expectClose(connection.ws, scenario.name, 3000);
      connection.ws.send(scenario.message);
      const code = await closed;
      assert.notEqual(code, 1000, "invalid messages must not close successfully");
      if (scenario.code !== undefined) assert.equal(code, scenario.code);
    });
  }
});

test("hello identity conflict rejects the duplicate and preserves the active session", async t => {
  const token = demoToken("conflict");
  const hub = await startHub(t, { collectors: [credential("c-conflict", "m-conflict", token)] });
  const collector = rawCollector("c-conflict", "mock-conflict");
  const first = await wsUpgrade(hub.collectorWsUrl, { authorization: `Bearer ${token}` });
  assert.equal(first.kind, "open");
  if (first.kind !== "open") return;
  await bindCollectorSession(first.ws, collector, "first session");
  const second = await wsUpgrade(hub.collectorWsUrl, { authorization: `Bearer ${token}` });
  assert.equal(second.kind, "open");
  if (second.kind !== "open") return;
  const closed = expectClose(second.ws, "duplicate session", 3000);
  second.ws.send(JSON.stringify(helloOf(collector)));
  assert.equal(await closed, 4005);
  assert.equal(isWsOpen(first.ws), true);
  wsTerminate(first.ws);
});

test("collector hello deadline closes an authenticated socket that never sends hello", async t => {
  const token = demoToken("deadline");
  const hub = await startHub(t, { collectors: [credential("c-deadline", "m-deadline", token)] });
  const connection = await wsUpgrade(hub.collectorWsUrl, { authorization: `Bearer ${token}` });
  assert.equal(connection.kind, "open");
  if (connection.kind !== "open") return;
  assert.equal(await expectClose(connection.ws, "hello deadline", 6500), 4001);
});
