import assert from "node:assert/strict";
import test from "node:test";
import { credential, demoToken } from "./support/config";
import { startCollector, startHub } from "./support/fixtures";
import { expectHealth } from "./support/http";
import { SseClient } from "./support/sse";
import { assertSample, parseSnapshot, waitMachineState } from "./support/wire";

test("mock collector: samples reach SSE snapshot and update with full contract shape; SIGTERM exits zero", async (t) => {
  const token = demoToken("sse");
  const cred = credential("c-sse", "m-sse", token);
  const hub = await startHub(t, { collectors: [cred] });
  const health = await expectHealth(hub);

  // Subscribe first so the initial snapshot and any later update are both observed.
  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  const initial = await live.next();
  assert.equal(initial.event, "snapshot", `initial SSE event must be snapshot (got ${initial.event})`);
  live.assertResponseHeaders();
  const initialSnapshot = parseSnapshot(initial.data, "initial snapshot");
  assert.equal(initialSnapshot.hubRunId, health.hubRunId, "snapshot hubRunId equals health hubRunId");
  if (initial.id !== undefined) {
    assert.match(initial.id, new RegExp(`^${health.hubRunId}:\\d+$`), `event id must be <hubRunId>:<cursor> (got ${initial.id})`);
  }

  const collector = startCollector(t, {
    collectorId: cred.collectorId,
    hostname: "demo-node-primary",
    hubs: [{ url: hub.collectorWsUrl, token }],
    intervalMs: 200,
  });

  const found = await waitMachineState(live, 30_000, { machineId: cred.machineId, collectorId: cred.collectorId, withMetrics: true });
  const machine = found.snapshot.machines.find((m) => m.machineId === cred.machineId);
  assert.ok(machine, "mapped machine present in the delivered snapshot");
  if (machine !== undefined) {
    assert.equal(machine.hostname, "demo-node-primary", "hostname comes from the collector hello");
    assert.equal(machine.connected, true, "machine marked connected");
    const metrics = machine.metrics;
    assert.ok(metrics, "metrics reach the SSE snapshot/update");
    if (metrics !== undefined) {
      assertSample(metrics, "delivered metrics record");
      assert.ok(metrics.sequence >= 1, "sequence positive within the run");
    }
  }

  // Clean collector shutdown stays exit-zero even after successful delivery.
  const exit = await collector.stop("SIGTERM");
  assert.equal(exit.code, 0, `collector SIGTERM must exit zero (got code ${exit.code}, signal ${exit.signal}). ${collector.dump()}`);
  live.close();
});

test("SSE reconnect: Last-Event-ID raises a reset snapshot; event ids stay <hubRunId>:<cursor>", async (t) => {
  const hub = await startHub(t, { collectors: [credential("c-reset", "m-reset", demoToken("reset"))] });
  const health = await expectHealth(hub);

  const first = new SseClient(hub.liveUrl, hub.browserHeaders);
  const initial = await first.next();
  assert.equal(initial.event, "snapshot", `initial SSE event must be snapshot (got ${initial.event})`);
  const baseSnapshot = parseSnapshot(initial.data, "initial snapshot");
  assert.equal(baseSnapshot.hubRunId, health.hubRunId);
  assert.ok(initial.id !== undefined, "snapshot event carries an id");
  assert.match(initial.id, new RegExp(`^${health.hubRunId}:\\d+$`), `event id format (got ${initial.id})`);
  const lastEventId = initial.id;
  first.close();

  // Reconnect with Last-Event-ID: the contract resynchronizes with a reset snapshot.
  const reconnected = new SseClient(hub.liveUrl, { ...hub.browserHeaders, "last-event-id": lastEventId });
  const resetEvent = await reconnected.next();
  assert.equal(resetEvent.event, "reset", `reconnect with Last-Event-ID must produce reset (got ${resetEvent.event})`);
  const resetSnapshot = parseSnapshot(resetEvent.data, "reset snapshot");
  assert.equal(resetSnapshot.hubRunId, health.hubRunId, "reset is a fresh snapshot for this run");
  assert.ok(resetEvent.id !== undefined, "reset event carries an id");
  if (resetEvent.id !== undefined) {
    assert.match(resetEvent.id, new RegExp(`^${health.hubRunId}:\\d+$`), `reset id format (got ${resetEvent.id})`);
    assert.notEqual(resetEvent.id, lastEventId, "the reset event gets a fresh cursor");
  }

  // Unknown earlier cursors also resynchronize instead of failing.
  const bogus = new SseClient(hub.liveUrl, { ...hub.browserHeaders, "last-event-id": `${health.hubRunId}:999999` });
  const bogusEvent = await bogus.next();
  assert.equal(bogusEvent.event, "reset", "unknown Last-Event-ID also resynchronizes with a reset");
  parseSnapshot(bogusEvent.data, "reset for unknown cursor");
  bogus.close();
  reconnected.close();
});
