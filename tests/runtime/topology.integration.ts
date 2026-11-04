import assert from "node:assert/strict";
import test from "node:test";
import { CONNECT_PATH, credential, demoToken } from "./support/config";
import { startCollector, startHub } from "./support/fixtures";
import { closedLoopbackPort } from "./support/http";
import { SseClient } from "./support/sse";
import { HubSnapshotShape, parseSnapshot, waitMachineState } from "./support/wire";

test("two collectors map to separate machines on one hub", async (t) => {
  const alphaToken = demoToken("alpha");
  const betaToken = demoToken("beta");
  const hub = await startHub(t, {
    collectors: [
      credential("c-alpha", "m-alpha", alphaToken),
      credential("c-beta", "m-beta", betaToken),
    ],
  });
  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  await live.next(); // initial snapshot

  startCollector(t, {
    collectorId: "c-alpha",
    hostname: "demo-alpha",
    hubs: [{ url: hub.collectorWsUrl, token: alphaToken }],
    intervalMs: 200,
  });
  startCollector(t, {
    collectorId: "c-beta",
    hostname: "demo-beta",
    hubs: [{ url: hub.collectorWsUrl, token: betaToken }],
    intervalMs: 200,
  });

  const end = Date.now() + 30_000;
  let snapshot: HubSnapshotShape | null = null;
  while (Date.now() < end && snapshot === null) {
    const remaining = end - Date.now();
    if (remaining <= 0) break;
    const event = await live.next(remaining);
    if (event.event !== "snapshot" && event.event !== "update") continue;
    const candidate = parseSnapshot(event.data, `${event.event} (two collectors)`);
    if (candidate.machines.length === 2 && candidate.machines.every((m) => m.connected && m.metrics !== undefined)) {
      snapshot = candidate;
    }
  }
  assert.ok(snapshot, "both machines must reach connected + metrics state");
  if (snapshot !== null) {
    const machineIds = new Set(snapshot.machines.map((m) => m.machineId));
    const collectorIds = new Set(snapshot.machines.map((m) => m.collectorId));
    assert.equal(machineIds.size, 2, "two distinct machineIds after two collectors");
    assert.equal(collectorIds.size, 2, "two distinct collector identities");
    const alpha = snapshot.machines.find((m) => m.machineId === "m-alpha");
    const beta = snapshot.machines.find((m) => m.machineId === "m-beta");
    assert.ok(alpha !== undefined && alpha.metrics !== undefined, "m-alpha reached summary state");
    assert.ok(beta !== undefined && beta.metrics !== undefined, "m-beta reached summary state");
    if (alpha !== undefined) {
      assert.equal(alpha.collectorId, "c-alpha");
      assert.equal(alpha.hostname, "demo-alpha");
      assert.ok(alpha.metrics!.sequence >= 1);
    }
    if (beta !== undefined) {
      assert.equal(beta.collectorId, "c-beta");
      assert.equal(beta.hostname, "demo-beta");
      assert.ok(beta.metrics!.sequence >= 1);
    }
  }
  live.close();
});

test("one collector delivers to two hubs with independent tokens and machine mappings", async (t) => {
  const tokenA = demoToken("hub-a");
  const tokenB = demoToken("hub-b");
  const hubA = await startHub(t, { collectors: [credential("c-multi", "m-a", tokenA)] });
  const hubB = await startHub(t, { collectors: [credential("c-multi", "m-b", tokenB)] });

  const liveA = new SseClient(hubA.liveUrl, hubA.browserHeaders);
  const liveB = new SseClient(hubB.liveUrl, hubB.browserHeaders);
  await Promise.all([liveA.next(), liveB.next()]); // initial snapshots

  const collector = startCollector(t, {
    collectorId: "c-multi",
    hostname: "demo-multihub",
    hubs: [
      { url: hubA.collectorWsUrl, token: tokenA },
      { url: hubB.collectorWsUrl, token: tokenB },
    ],
    intervalMs: 200,
  });

  const [foundA, foundB] = await Promise.all([
    waitMachineState(liveA, 30_000, { machineId: "m-a", collectorId: "c-multi", withMetrics: true }),
    waitMachineState(liveB, 30_000, { machineId: "m-b", collectorId: "c-multi", withMetrics: true }),
  ]);
  const machineA = foundA.snapshot.machines.find((m) => m.machineId === "m-a");
  const machineB = foundB.snapshot.machines.find((m) => m.machineId === "m-b");
  assert.ok(machineA !== undefined && machineA.metrics !== undefined, "hub A saw the collector");
  assert.ok(machineB !== undefined && machineB.metrics !== undefined, "hub B saw the collector");
  if (machineA !== undefined && machineA.metrics !== undefined) {
    assert.ok(machineA.metrics!.sequence >= 1, "hub A received samples");
  }
  if (machineB !== undefined && machineB.metrics !== undefined) {
    assert.ok(machineB.metrics!.sequence >= 1, "hub B received samples");
  }

  // One sampling process fans out to two destinations with independent
  // credentials; tokens are never shared across hubs, and each hub maps the
  // collector to its own machine.
  const exit = await collector.stop("SIGTERM");
  assert.equal(exit.code, 0, `collector SIGTERM stays clean after multi-hub delivery (code ${exit.code}). ${collector.dump()}`);
  liveA.close();
  liveB.close();
});

test("offline destination never blocks healthy delivery", async (t) => {
  const token = demoToken("healthy");
  const hub = await startHub(t, { collectors: [credential("c-healthy", "m-healthy", token)] });
  const closed = await closedLoopbackPort();

  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  await live.next(); // initial snapshot

  const collector = startCollector(t, {
    collectorId: "c-healthy",
    hostname: "demo-healthy",
    hubs: [
      { url: hub.collectorWsUrl, token },
      { url: `ws://127.0.0.1:${closed}${CONNECT_PATH}`, token: demoToken("dead") }, // unreachable
    ],
    intervalMs: 200,
  });

  const initial = await waitMachineState(live, 5000, { machineId: "m-healthy", withMetrics: true });
  const firstSequence = initial.snapshot.machines[0].metrics!.sequence;
  // Observe continued delivery instead of sleeping and merely checking that
  // the process is alive. A stalled healthy destination must fail this test.
  for (;;) {
    const event = await live.next(5000);
    const snapshot = parseSnapshot(event.data, "continued healthy delivery");
    if ((snapshot.machines[0].metrics?.sequence ?? 0) >= firstSequence + 3) break;
  }
  assert.equal(collector.proc.exitCode, null, "collector must keep running while a destination is unreachable");
  const exit = await collector.stop("SIGTERM");
  assert.equal(exit.code, 0, `collector SIGTERM with a failed destination exits zero (code ${exit.code}). ${collector.dump()}`);
  live.close();
});
