import assert from "node:assert/strict";
import test from "node:test";
import { CONNECT_PATH, credential, demoToken } from "./support/config";
import { startCollector, startHub } from "./support/fixtures";
import { failOnExit } from "./support/process";
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

  const alpha = startCollector(t, {
    collectorId: "c-alpha",
    hostname: "demo-alpha",
    hubs: [{ url: hub.collectorWsUrl, token: alphaToken }],
    intervalMs: 200,
  });
  const beta = startCollector(t, {
    collectorId: "c-beta",
    hostname: "demo-beta",
    hubs: [{ url: hub.collectorWsUrl, token: betaToken }],
    intervalMs: 200,
  });

  const end = Date.now() + 30_000;
  let snapshot: HubSnapshotShape | null = null;
  const waitForBoth = (async (): Promise<HubSnapshotShape> => {
    while (Date.now() < end) {
      const remaining = end - Date.now();
      if (remaining <= 0) break;
      const event = await live.next(remaining);
      if (event.event !== "snapshot" && event.event !== "update") continue;
      const candidate = parseSnapshot(event.data, `${event.event} (two collectors)`);
      if (candidate.machines.length === 2 && candidate.machines.every((m) => m.connected && m.metrics !== undefined)) {
        return candidate;
      }
    }
    throw new Error("both machines must reach connected + metrics state");
  })();
  snapshot = await failOnExit([alpha, beta], waitForBoth, "two collectors reaching one hub");
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

/**
 * The 1.0 single-destination variant of offline isolation: the hub is absent
 * when the collector starts, appears, goes away, and returns. One process owns
 * the whole lifecycle, so a sequence that resumes above the pre-outage value
 * proves the collector kept sampling rather than restarting.
 */
test("a single destination recovers after the hub is unavailable and later returns", async (t) => {
  const port = await closedLoopbackPort();
  const token = demoToken("recover");
  const cred = credential("c-recover", "m-recover", token);
  const collector = startCollector(t, {
    collectorId: cred.collectorId,
    hostname: "demo-recover",
    hubs: [{ url: `ws://127.0.0.1:${port}${CONNECT_PATH}`, token }],
    intervalMs: 200,
  });
  // No hub exists yet: the collector must keep retrying instead of exiting.
  await new Promise((resolve) => setTimeout(resolve, 500));
  assert.equal(collector.proc.exitCode, null, `collector keeps retrying an unavailable hub. ${collector.dump()}`);

  const first = await startHub(t, { port, collectors: [cred] });
  const live = new SseClient(first.liveUrl, first.browserHeaders);
  t.after(() => live.close());
  const initial = await failOnExit(collector, waitMachineState(live, 15_000, {
    machineId: cred.machineId, collectorId: cred.collectorId, withMetrics: true,
  }), "delivery after the hub appears");
  const firstSequence = initial.snapshot.machines[0].metrics!.sequence;
  let advanced = firstSequence;
  const advanceDeadline = Date.now() + 10_000;
  while (advanced < firstSequence + 3) {
    const event = await failOnExit(collector, live.next(Math.max(1, advanceDeadline - Date.now())), "advancing delivery");
    const snapshot = parseSnapshot(event.data, "pre-outage delivery");
    advanced = Math.max(advanced, snapshot.machines.find(m => m.machineId === cred.machineId)?.metrics?.sequence ?? advanced);
  }

  assert.equal((await first.stop()).code, 0);
  assert.equal(collector.proc.exitCode, null, `collector survives the hub going away. ${collector.dump()}`);

  const second = await startHub(t, { port, collectors: [cred] });
  const resumedLive = new SseClient(second.liveUrl, second.browserHeaders);
  t.after(() => resumedLive.close());
  const resumed = await failOnExit(collector, waitMachineState(resumedLive, 20_000, {
    machineId: cred.machineId, collectorId: cred.collectorId, withMetrics: true,
  }), "delivery after the hub returns");
  const resumedMetrics = resumed.snapshot.machines.find(m => m.machineId === cred.machineId)!.metrics!;
  assert.ok(resumedMetrics.sequence > advanced,
    `delivery resumes with advancing sequences (resumed ${resumedMetrics.sequence}, pre-outage ${advanced})`);
  const exit = await collector.stop("SIGTERM");
  assert.equal(exit.code, 0, `clean shutdown after recovery (code ${exit.code}). ${collector.dump()}`);
});
