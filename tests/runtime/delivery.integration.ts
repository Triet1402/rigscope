/**
 * Delivery control-plane tests through the relay: delayed acks, lost acks,
 * retry responses, reconnect across a hub restart, inventory re-announcement
 * and process-detail leases.
 *
 * These are the Phase 1 exit-check behaviours that only exist on the wire, so
 * they run against the Go collector and the Go hub.
 */
import assert from "node:assert/strict";
import test, { type TestContext } from "node:test";
import { mkdtempSync, rmSync } from "node:fs";
import { DatabaseSync } from "node:sqlite";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { credential, demoToken } from "./support/config";
import { startHub } from "./support/fixtures";
import { startCollectorProcess } from "./support/launcher";
import { failOnExit } from "./support/process";
import { loginBrowser } from "./support/browser";
import { SseClient } from "./support/sse";
import { parseSnapshot } from "./support/wire";
import {
  isAckFrame,
  relayMessageType,
  relaySampleRecords,
  startRelay,
  type RelayController,
  type RelayFrame,
} from "./support/relay";

type DeliveryStatus = {
  pendingRecords: number;
  inFlightRecords: number;
  lastAckAt: string | null;
  queueMode?: string;
  replaying?: boolean;
};

function deliveryOf(frame: RelayFrame): DeliveryStatus | null {
  const value = frame.data as { delivery?: unknown } | null;
  if (typeof value !== "object" || value === null) return null;
  const delivery = value.delivery as DeliveryStatus | undefined;
  return typeof delivery === "object" && delivery !== null ? delivery : null;
}

/** Wait for a collector status frame after `since` satisfying the predicate. */
async function waitForStatus(
  relay: RelayController,
  predicate: (delivery: DeliveryStatus) => boolean,
  deadlineMs: number,
  what: string,
  since = 0,
): Promise<RelayFrame> {
  return await relay.waitForFrame((frame) => {
    if (relay.frames().indexOf(frame) < since) return false;
    if (frame.direction !== "collector" || relayMessageType(frame) !== "status") return false;
    const delivery = deliveryOf(frame);
    return delivery !== null && predicate(delivery);
  }, deadlineMs, what);
}

async function waitAfter(
  relay: RelayController,
  since: number,
  predicate: (frame: RelayFrame) => boolean,
  deadlineMs: number,
  what: string,
): Promise<RelayFrame> {
  return await relay.waitForFrame(
    (frame) => relay.frames().indexOf(frame) >= since && predicate(frame),
    deadlineMs,
    what,
  );
}

function refKey(record: { runId: string; sequence: number }): string {
  return `${record.runId}\u0000${record.sequence}`;
}

type RecordIdentity = { runId: string; sequence: number };

/** The accepted identities carried by an `ack` frame, read from that frame. */
function acceptedRefs(frame: RelayFrame): RecordIdentity[] {
  const value = frame.data as { accepted?: unknown } | null;
  if (typeof value !== "object" || value === null || !Array.isArray(value.accepted)) return [];
  const refs: RecordIdentity[] = [];
  for (const entry of value.accepted) {
    const ref = entry as { runId?: unknown; sequence?: unknown };
    if (typeof ref.runId !== "string" || typeof ref.sequence !== "number") continue;
    refs.push({ runId: ref.runId, sequence: ref.sequence });
  }
  return refs;
}

/** Distinct record identities the collector sent through the relay from `from` (a frame index) on. */
function sentRefsSince(relay: RelayController, from: number): Set<string> {
  const refs = new Set<string>();
  for (const frame of relay.frames().slice(from)) {
    if (frame.direction !== "collector") continue;
    for (const record of relaySampleRecords(frame)) refs.add(refKey(record));
  }
  return refs;
}

/** Wait until every key in `expected` was resent after frame index `from`. */
async function waitForResends(
  relay: RelayController,
  from: number,
  expected: ReadonlySet<string>,
  deadlineMs: number,
  what: string,
): Promise<Set<string>> {
  const end = Date.now() + deadlineMs;
  while (true) {
    const resent = sentRefsSince(relay, from);
    if ([...expected].every((key) => resent.has(key))) return resent;
    if (Date.now() >= end) {
      throw new Error(`relay timeout after ${deadlineMs}ms waiting for ${what}; missing ` +
        `${[...expected].filter((key) => !resent.has(key)).join(", ") || "(none)"}`);
    }
    await delay(50);
  }
}

/** Distinct record identities the collector sent through the relay before `end`. */
function sentRefs(relay: RelayController, end: number): Set<string> {
  const refs = new Set<string>();
  for (const frame of relay.frames().slice(0, end)) {
    if (frame.direction !== "collector") continue;
    for (const record of relaySampleRecords(frame)) refs.add(refKey(record));
  }
  return refs;
}

function countAcks(relay: RelayController, delivery: "forwarded" | "held" | "dropped" | "injected"): number {
  return relay.frames().filter((frame) => frame.direction === "hub" && frame.delivery === delivery && isAckFrame(frame)).length;
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function fixture(t: TestContext): string {
  const root = mkdtempSync(join(tmpdir(), "rigscope-delivery-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  return root;
}

test("delayed acks retain delivered records until the hub is allowed to answer", async (t) => {
  const token = demoToken("delayed-ack");
  const cred = credential("c-delayed-ack", "m-delayed-ack", token);
  const hub = await startHub(t, { collectors: [cred] });
  const relay = await startRelay({ upstreamUrl: hub.collectorWsUrl });
  relay.holdAcks();
  const collector = startCollectorProcess(t, {
    label: "delayed-ack collector",
    mode: "mock",
    collectorId: cred.collectorId,
    hostname: "relay-delayed-ack",
    hubs: [{ url: relay.url, token }],
    intervalMs: 200,
  });
  await collector.waitForLine((line) => line.includes("welcomed"), 15_000, "delayed-ack collector welcome");

  const retained = await failOnExit(collector, waitForStatus(
    relay,
    (delivery) => (delivery.inFlightRecords ?? 0) + (delivery.pendingRecords ?? 0) > 0,
    25_000,
    "a collector status reporting records retained by the held ack",
  ), "delayed ack retention");
  assert.ok((deliveryOf(retained)?.inFlightRecords ?? 0) > 0, "records stay in flight while the ack is held");
  assert.ok(relay.heldAcks() > 0, "the hub answered, and the relay withheld the ack");
  assert.equal(countAcks(relay, "forwarded"), 0, "no ack reached the collector while held");

  const mark = relay.frames().length;
  relay.releaseAcks();
  const settled = await failOnExit(collector, waitForStatus(
    relay,
    (delivery) => delivery.inFlightRecords === 0 && delivery.pendingRecords === 0 && delivery.lastAckAt !== null,
    25_000,
    "a collector status reporting an empty queue after the acks were released",
    mark,
  ), "delayed ack release");
  assert.ok(deliveryOf(settled)?.lastAckAt, "the released ack was processed");
  assert.ok(countAcks(relay, "forwarded") > 0, "the held ack was released to the collector");
  assert.equal((await collector.stop()).code, 0, collector.dump());
});

test("lost acks are resent on reconnect and the hub commits each record once", async (t) => {
  const root = fixture(t);
  const databasePath = join(root, "hub", "telemetry.sqlite");
  const token = demoToken("lost-ack");
  const cred = credential("c-lost-ack", "m-lost-ack", token);
  const hub = await startHub(t, {
    collectors: [cred],
    databasePath,
    telemetryMode: "persistent",
    telemetryFlushMs: 1_000,
  });
  const relay = await startRelay({ upstreamUrl: hub.collectorWsUrl });
  const collector = startCollectorProcess(t, {
    label: "lost-ack collector",
    mode: "mock",
    collectorId: cred.collectorId,
    hostname: "relay-lost-ack",
    hubs: [{ url: relay.url, token }],
    intervalMs: 100,
  });
  await collector.waitForLine((line) => line.includes("welcomed"), 15_000, "lost-ack collector welcome");
  await relay.waitForFrame(
    () => sentRefs(relay, relay.frames().length).size >= 3,
    15_000,
    "three distinct sample records",
  );

  relay.dropAcks();
  const dropped = await relay.waitForFrame(
    (frame) => frame.direction === "hub" && frame.delivery === "dropped" && isAckFrame(frame),
    15_000,
    "an ack dropped on the way back to the collector",
  );
  assert.ok(dropped.at > 0);
  // The hub tells the collector exactly which identities it committed, but
  // this ack never reaches it. Those identities are the ones a reconnect must
  // offer again.
  const droppedAccepted = acceptedRefs(dropped);
  assert.ok(droppedAccepted.length > 0, "the dropped ack named at least one accepted record");
  const droppedKeys = new Set(droppedAccepted.map(refKey));
  // Stop losing new acks, but the dropped one stays lost: only a resend
  // after reconnecting can recover it. Both calls are synchronous, so no
  // further ack can be dropped between capturing it and cutting the link.
  relay.forwardAcks();
  const beforeReconnect = sentRefs(relay, relay.frames().length);
  for (const key of droppedKeys) {
    assert.ok(beforeReconnect.has(key), "every identity accepted by the dropped ack was offered before the disconnect");
  }

  const disconnectMark = relay.frames().length;
  const disconnectAt = Date.now();
  relay.disconnect();
  await relay.waitForConnection(2, 20_000, "the collector reconnecting after a lost ack");
  await waitForResends(
    relay,
    disconnectMark,
    droppedKeys,
    20_000,
    "the reconnect resending every identity whose ack was lost",
  );
  const resendFrame = await relay.waitForFrame(
    (frame) => relay.frames().indexOf(frame) >= disconnectMark && frame.direction === "collector" &&
      relaySampleRecords(frame).some((record) => droppedKeys.has(refKey(record))),
    20_000,
    "a resend of a record whose ack was lost",
  );
  assert.ok(resendFrame.at >= disconnectAt, "a previously offered record was resent after reconnect");

  const mark = relay.frames().length;
  await waitForAcknowledgment(
    relay,
    droppedKeys,
    disconnectMark,
    20_000,
    "the restarted session acknowledging the resent identities",
  );
  assert.ok(deliveryOf(await waitForStatus(
    relay,
    (delivery) => delivery.lastAckAt !== null,
    15_000,
    "a collector status after the resend was acknowledged",
    mark,
  )) !== null);
  assert.equal((await collector.stop()).code, 0, collector.dump());

  // Let the last frames the collector wrote reach the relay before computing
  // the full sent set; then the hub's receipts must account for exactly that
  // set, once per identity.
  const fullSent = await settledSentRefs(relay);
  // The last batch can still be in the hub's commit queue when the collector
  // exits; wait for the hub to account for every identity that was sent.
  const statusDeadline = Date.now() + 15_000;
  let acceptedRecords = -1;
  while (Date.now() < statusDeadline) {
    const response = await fetch(`${hub.baseUrl}/api/history/status`, { headers: hub.browserHeaders });
    assert.equal(response.status, 200);
    acceptedRecords = (await response.json() as { acceptedRecords: number }).acceptedRecords;
    if (acceptedRecords === fullSent.size) break;
    await delay(100);
  }
  assert.equal(
    acceptedRecords,
    fullSent.size,
    `the hub committed each distinct (run, sequence) exactly once (${fullSent.size} distinct records offered)`,
  );
  assert.equal((await hub.stop()).code, 0, hub.child.dump());

  const receipts = readReceipts(databasePath, cred.collectorId);
  assert.deepEqual(
    [...receipts.keys()].sort(),
    [...fullSent].sort(),
    "the hub holds one receipt for every identity the collector sent and none it never saw",
  );
  assert.ok(
    [...receipts.values()].every((count) => count === 1),
    `no identity was committed twice (${[...receipts].filter(([, count]) => count !== 1).map(([key]) => key).join(", ")})`,
  );
  for (const key of droppedKeys) {
    assert.equal(receipts.get(key), 1, "the lost-ack identity was committed exactly once after its resend");
  }
});

/** Wait until a forwarded ack observes every key in `expected` after frame index `from`. */
async function waitForAcknowledgment(
  relay: RelayController,
  expected: ReadonlySet<string>,
  from: number,
  deadlineMs: number,
  what: string,
): Promise<Set<string>> {
  const end = Date.now() + deadlineMs;
  while (true) {
    const acknowledged = new Set<string>();
    for (const frame of relay.frames().slice(from)) {
      if (frame.direction !== "hub" || frame.delivery !== "forwarded" || !isAckFrame(frame)) continue;
      for (const ref of acceptedRefs(frame)) acknowledged.add(refKey(ref));
    }
    if ([...expected].every((key) => acknowledged.has(key))) return acknowledged;
    if (Date.now() >= end) {
      throw new Error(`relay timeout after ${deadlineMs}ms waiting for ${what}; missing ` +
        `${[...expected].filter((key) => !acknowledged.has(key)).join(", ") || "(none)"}`);
    }
    await delay(50);
  }
}

async function settledSentRefs(relay: RelayController): Promise<Set<string>> {
  let previous = -1;
  let stableSince = Date.now();
  while (Date.now() - stableSince < 500) {
    const count = relay.frames().length;
    if (count !== previous) {
      previous = count;
      stableSince = Date.now();
    }
    await delay(25);
  }
  return sentRefs(relay, relay.frames().length);
}

function readReceipts(path: string, collectorId: string): Map<string, number> {
  const db = new DatabaseSync(path, { readOnly: true });
  try {
    const rows = db.prepare(
      "SELECT run_id, seq, COUNT(*) AS count FROM sample_receipts WHERE collector_id = ? GROUP BY run_id, seq",
    ).all(collectorId) as Array<{ run_id: string | null; seq: number | bigint; count: number | bigint }>;
    const receipts = new Map<string, number>();
    for (const row of rows) {
      receipts.set(`${String(row.run_id)}\u0000${String(row.seq)}`, Number(row.count));
    }
    return receipts;
  } finally {
    db.close();
  }
}

test("a retry response pauses replay and delivery then resumes", async (t) => {
  const token = demoToken("retry");
  const cred = credential("c-retry", "m-retry", token);
  const hub = await startHub(t, { collectors: [cred] });
  const relay = await startRelay({ upstreamUrl: hub.collectorWsUrl });
  relay.holdAcks();
  const collector = startCollectorProcess(t, {
    label: "retry collector",
    mode: "mock",
    collectorId: cred.collectorId,
    hostname: "relay-retry",
    hubs: [{ url: relay.url, token }],
    intervalMs: 100,
  });
  await collector.waitForLine((line) => line.includes("welcomed"), 15_000, "retry collector welcome");

  const backlog = await failOnExit(collector, waitForStatus(
    relay,
    (delivery) => delivery.pendingRecords > 0,
    35_000,
    "a backlog while every ack is held (the in-flight window must fill first)",
  ), "retry backlog");
  assert.ok((deliveryOf(backlog)?.pendingRecords ?? 0) > 0, "records are waiting beyond the in-flight window");
  assert.ok(relay.heldAcks() > 0, "acks were withheld while the backlog formed");

  const injectedAt = Date.now();
  assert.equal(relay.injectRetry(1_500), true, "the relay can inject a retry response into the live session");
  relay.releaseAcks();
  const replay = await relay.waitForFrame(
    (frame) => frame.direction === "collector" && frame.at > injectedAt &&
      relaySampleRecords(frame).some((record) => record.mode === "replay"),
    25_000,
    "a replay batch after the retry pause",
  );
  const waited = replay.at - injectedAt;
  assert.ok(waited >= 1_000, `replay respected the hub's retry delay (first replay after ${waited}ms)`);

  const mark = relay.frames().length;
  const resumed = await failOnExit(collector, waitForStatus(
    relay,
    (delivery) => delivery.pendingRecords === 0 && delivery.inFlightRecords === 0,
    30_000,
    "delivery to drain after the retry pause",
    mark,
  ), "retry recovery");
  assert.ok(deliveryOf(resumed) !== null);
  assert.equal((await collector.stop()).code, 0, collector.dump());
});

test("inventory is announced on connect and re-announced after a reconnect", async (t) => {
  const token = demoToken("inventory");
  const cred = credential("c-inventory", "m-inventory", token);
  const hub = await startHub(t, { collectors: [cred] });
  const relay = await startRelay({ upstreamUrl: hub.collectorWsUrl });
  const collector = startCollectorProcess(t, {
    label: "inventory collector",
    mode: "mock",
    collectorId: cred.collectorId,
    hostname: "relay-inventory",
    hubs: [{ url: relay.url, token }],
    intervalMs: 200,
  });
  await collector.waitForLine((line) => line.includes("welcomed"), 15_000, "inventory collector welcome");

  const first = await relay.waitForFrame(
    (frame) => frame.direction === "collector" && relayMessageType(frame) === "inventory",
    15_000,
    "inventory on connect",
  );
  const firstInventory = (first.data as { inventory?: { capabilities?: Record<string, { status?: string }> } }).inventory;
  assert.ok(firstInventory, "inventory frame carries an inventory object");
  assert.equal(firstInventory!.capabilities?.cpu?.status, "supported", "cpu capability is announced");

  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  t.after(() => live.close());
  const waitForInventory = async (deadline: number) => {
    while (Date.now() < deadline) {
      const event = await live.next(Math.max(1, deadline - Date.now()));
      const snapshot = parseSnapshot(event.data, "inventory snapshot");
      const machine = snapshot.machines.find((candidate) => candidate.machineId === cred.machineId);
      const inventory = (machine as { inventory?: { capabilities?: Record<string, { status?: string }> } } | undefined)?.inventory;
      if (machine?.connected && inventory !== undefined) return inventory;
    }
    throw new Error("hub live state never exposed the collector inventory");
  };
  const hubInventory = await failOnExit(collector, waitForInventory(Date.now() + 15_000), "inventory in hub state");
  assert.equal(hubInventory.capabilities?.cpu?.status, "supported");

  const mark = relay.frames().length;
  const connected = Date.now();
  relay.disconnect();
  await relay.waitForConnection(2, 20_000, "the collector reconnecting");
  const second = await waitAfter(
    relay,
    mark,
    (frame) => frame.direction === "collector" && relayMessageType(frame) === "inventory",
    20_000,
    "inventory re-announced after reconnect",
  );
  assert.ok(second.at >= connected, "inventory is connection-scoped and re-announced on the new connection");
  const reannounced = await failOnExit(collector, waitForInventory(Date.now() + 15_000), "re-announced inventory in hub state");
  assert.equal(reannounced.capabilities?.cpu?.status, "supported");
  assert.equal((await collector.stop()).code, 0, collector.dump());
});

test("the collector keeps running across a hub restart and resumes delivery", async (t) => {
  const root = fixture(t);
  const token = demoToken("hub-restart");
  const cred = credential("c-hub-restart", "m-hub-restart", token);
  const hubDir = join(root, "hub");
  const databasePath = join(hubDir, "telemetry.sqlite");
  const first = await startHub(t, { stateDir: hubDir, databasePath, collectors: [cred] });
  const relay = await startRelay({ upstreamUrl: first.collectorWsUrl });
  const collector = startCollectorProcess(t, {
    label: "hub-restart collector",
    mode: "mock",
    collectorId: cred.collectorId,
    hostname: "relay-hub-restart",
    hubs: [{ url: relay.url, token }],
    intervalMs: 200,
  });
  await collector.waitForLine((line) => line.includes("welcomed"), 15_000, "hub-restart collector welcome");
  const live = new SseClient(first.liveUrl, first.browserHeaders);
  t.after(() => live.close());
  const before = await failOnExit(collector, (async () => {
    const deadline = Date.now() + 20_000;
    while (Date.now() < deadline) {
      const event = await live.next(Math.max(1, deadline - Date.now()));
      const snapshot = parseSnapshot(event.data, "pre-restart delivery");
      const machine = snapshot.machines.find((candidate) => candidate.machineId === cred.machineId);
      if (machine?.connected && machine.metrics) return machine.metrics;
    }
    throw new Error("delivery before the hub restart never arrived");
  })(), "delivery before the hub restart");
  const pid = collector.proc.pid;
  assert.equal((await first.stop()).code, 0);
  assert.equal(collector.proc.exitCode, null, `the collector survives the hub going away. ${collector.dump()}`);

  const mark = relay.frames().length;
  const second = await startHub(t, { port: first.port, stateDir: hubDir, databasePath, collectors: [cred] });
  await waitAfter(
    relay,
    mark,
    (frame) => frame.direction === "hub" && relayMessageType(frame) === "welcome",
    30_000,
    "the collector being welcomed by the restarted hub",
  );
  const afterLive = new SseClient(second.liveUrl, second.browserHeaders);
  t.after(() => afterLive.close());
  const after = await failOnExit(collector, (async () => {
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      const event = await afterLive.next(Math.max(1, deadline - Date.now()));
      const snapshot = parseSnapshot(event.data, "post-restart delivery");
      const machine = snapshot.machines.find((candidate) => candidate.machineId === cred.machineId);
      // Persistent history restores the pre-restart reading immediately; wait
      // for a sample this process produced after the restart, which is also
      // what the sequence assertion below requires.
      if (machine?.connected && machine.metrics && machine.metrics.sequence > before.sequence) {
        return machine.metrics;
      }
    }
    throw new Error("delivery after the hub restart never arrived");
  })(), "delivery after the hub restart");
  assert.equal(collector.proc.pid, pid, "delivery resumed from the same collector process");
  assert.equal(after.collectorId, cred.collectorId);
  assert.equal(after.runId, before.runId, "a hub restart does not restart the collector run");
  assert.ok(after.sequence > before.sequence, `sequences advance across the restart (${before.sequence} -> ${after.sequence})`);
  assert.equal((await collector.stop()).code, 0, collector.dump());
});

type DetailGet = { result: null | { snapshot: { totalProcesses: number } }; state: string };

async function detailRequest(
  url: string,
  headers: Record<string, string>,
  method: "GET" | "POST" | "DELETE",
): Promise<{ status: number; body: DetailGet | { ok?: boolean; error?: string } }> {
  const response = await fetch(url, { method, headers, signal: AbortSignal.timeout(5_000) });
  const body = await response.json() as DetailGet | { ok?: boolean; error?: string };
  return { status: response.status, body };
}

async function waitForDetailSnapshot(
  url: string,
  headers: Record<string, string>,
  deadline: number,
): Promise<DetailGet> {
  while (Date.now() < deadline) {
    const { status, body } = await detailRequest(url, headers, "GET");
    const detail = body as DetailGet;
    if (status === 200 && detail.result !== null && detail.result.snapshot.totalProcesses > 0) return detail;
    await delay(200);
  }
  throw new Error("process detail results never reached the browser query");
}

function detailResults(relay: RelayController, subscriptionId?: string): RelayFrame[] {
  return relay.frames().filter(
    (frame) => frame.direction === "collector" && relayMessageType(frame) === "detail.result" &&
      (subscriptionId === undefined ||
        (frame.data as { subscriptionId?: unknown } | null)?.subscriptionId === subscriptionId),
  );
}

type LeaseTerms = { subscriptionId: string; intervalMs: number; leaseMs: number };

/** The lease the hub asked the collector to honour, as it appeared on the wire. */
function leaseTerms(frame: RelayFrame): LeaseTerms | null {
  const value = frame.data as {
    subscriptionId?: unknown;
    intervalMs?: unknown;
    leaseMs?: unknown;
  } | null;
  if (typeof value !== "object" || value === null) return null;
  if (typeof value.subscriptionId !== "string" ||
    typeof value.intervalMs !== "number" || typeof value.leaseMs !== "number") return null;
  return {
    subscriptionId: value.subscriptionId,
    intervalMs: value.intervalMs,
    leaseMs: value.leaseMs,
  };
}

test("process detail leases renew past the viewer lease and stop on DELETE", async (t) => {
  const token = demoToken("detail-renew");
  const collectorId = "detail-renew-collector";
  const cred = credential(collectorId, "m-detail-renew", token);
  const hub = await startHub(t, { collectors: [cred] });
  const relay = await startRelay({ upstreamUrl: hub.collectorWsUrl });
  const collector = startCollectorProcess(t, {
    label: "detail collector",
    mode: "real",
    collectorId,
    hostname: "relay-detail",
    hubs: [{ url: relay.url, token }],
    intervalMs: 250,
  });
  await collector.waitForLine((line) => line.includes("welcomed"), 15_000, "detail collector welcome");
  const browser = await loginBrowser(hub.baseUrl);
  const url = `${hub.baseUrl}/api/processes?machineId=${cred.machineId}&viewerId=runtime-renew`;
  const headers = { ...browser.headers, Origin: hub.baseUrl, "X-CSRF-Token": browser.csrfToken };

  const subscribe = await detailRequest(url, headers, "POST");
  assert.equal(subscribe.status, 200, JSON.stringify(subscribe.body));
  const subscription = await relay.waitForFrame(
    (frame) => frame.direction === "hub" && relayMessageType(frame) === "detail.subscribe",
    15_000,
    "the hub forwarding detail.subscribe to the collector",
  );
  assert.ok(subscription.at > 0);
  const terms = leaseTerms(subscription);
  assert.ok(terms, "detail.subscribe carries the lease the collector must honour");
  await failOnExit(collector, waitForDetailSnapshot(url, headers, Date.now() + 20_000), "process detail results");

  // The hub's initial collector lease is 20 s, longer than the 15 s viewer
  // lease this test used to rely on. Renewing the viewer for less than the
  // collector lease proves nothing: ignored renewals would still pass. Keep
  // renewing past `subscribe.at + leaseMs`, then require a matching result
  // strictly after that instant, which only a renewal can explain.
  const leaseDeadline = subscription.at + terms.leaseMs;
  while (Date.now() < leaseDeadline + 2_000) {
    await delay(2_000);
    const renew = await detailRequest(url, headers, "POST");
    assert.equal(renew.status, 200, JSON.stringify(renew.body));
  }
  await failOnExit(collector, relay.waitForFrame(
    (frame) => frame.direction === "collector" && relayMessageType(frame) === "detail.result" &&
      (frame.data as { subscriptionId?: unknown } | null)?.subscriptionId === terms.subscriptionId &&
      frame.at > leaseDeadline,
    15_000,
    `process detail results continuing past the initial ${terms.leaseMs}ms collector lease while renewed`,
  ), "renewed process detail results");
  const renewed = await detailRequest(url, headers, "GET");
  assert.equal(renewed.status, 200);
  assert.ok((renewed.body as DetailGet).result !== null, "the renewed interest still has a cached result");

  const beforeDelete = relay.frames().length;
  const cancel = await detailRequest(url, headers, "DELETE");
  assert.equal(cancel.status, 200, JSON.stringify(cancel.body));
  await waitAfter(
    relay,
    beforeDelete,
    (frame) => frame.direction === "hub" && relayMessageType(frame) === "detail.cancel",
    15_000,
    "the hub forwarding detail.cancel after DELETE",
  );
  const atCancel = detailResults(relay, terms.subscriptionId).length;
  await delay(2_500);
  assert.equal(
    detailResults(relay, terms.subscriptionId).length,
    atCancel,
    "no detail results arrive after DELETE",
  );
  const afterDelete = await detailRequest(url, headers, "GET");
  assert.equal(afterDelete.status, 410, "the cancelled interest is gone");
  assert.equal((await collector.stop()).code, 0, collector.dump());
});

test("process detail leases stop delivering results after the viewer expires", async (t) => {
  const token = demoToken("detail-expiry");
  const collectorId = "detail-expiry-collector";
  const cred = credential(collectorId, "m-detail-expiry", token);
  const hub = await startHub(t, { collectors: [cred] });
  const relay = await startRelay({ upstreamUrl: hub.collectorWsUrl });
  const collector = startCollectorProcess(t, {
    label: "detail expiry collector",
    mode: "real",
    collectorId,
    hostname: "relay-detail-expiry",
    hubs: [{ url: relay.url, token }],
    intervalMs: 250,
  });
  await collector.waitForLine((line) => line.includes("welcomed"), 15_000, "detail expiry collector welcome");
  const browser = await loginBrowser(hub.baseUrl);
  const url = `${hub.baseUrl}/api/processes?machineId=${cred.machineId}&viewerId=runtime-expiry`;
  const headers = { ...browser.headers, Origin: hub.baseUrl, "X-CSRF-Token": browser.csrfToken };

  const mark = relay.frames().length;
  const subscribe = await detailRequest(url, headers, "POST");
  assert.equal(subscribe.status, 200, JSON.stringify(subscribe.body));
  const subscribedAt = Date.now();
  const subscription = await waitAfter(
    relay,
    mark,
    (frame) => frame.direction === "hub" && relayMessageType(frame) === "detail.subscribe",
    15_000,
    "the hub forwarding detail.subscribe",
  );
  const terms = leaseTerms(subscription);
  assert.ok(terms, "detail.subscribe carries the lease the collector must honour");
  const firstResult = await failOnExit(collector, relay.waitForFrame(
    (frame) => frame.direction === "collector" && relayMessageType(frame) === "detail.result" &&
      (frame.data as { subscriptionId?: unknown } | null)?.subscriptionId === terms.subscriptionId,
    20_000,
    "the first detail result for the subscription",
  ), "process detail results");
  assert.ok(firstResult.at >= subscription.at, "results flowed before the lease expired");

  // From here the collector may not learn anything else from the hub: hide
  // both lease renewals and the eventual detail.cancel. Only the collector's
  // own copy of the last forwarded lease can stop the result stream, so a
  // collector that relies on the hub cancelling it is caught.
  relay.dropHubTypes(["detail.subscribe", "detail.cancel"]);
  const lastLease = [...relay.frames()].reverse().find(
    (frame) => frame.direction === "hub" && relayMessageType(frame) === "detail.subscribe" &&
      frame.delivery === "forwarded" &&
      (frame.data as { subscriptionId?: unknown } | null)?.subscriptionId === terms.subscriptionId,
  );
  assert.ok(lastLease, "the collector received at least one lease");
  const expiryAt = lastLease.at + terms.leaseMs;
  // One delivery interval of tolerance covers a scan already in flight at the
  // boundary; nothing may arrive after that.
  const stopBy = expiryAt + terms.intervalMs;

  // No renewal: the viewer lease must expire and the hub must try to cancel
  // the collector subscription. The relay hides that cancel, so the result
  // stream must stop by itself.
  let status = 200;
  const deadline = Date.now() + 25_000;
  while (Date.now() < deadline) {
    status = (await detailRequest(url, headers, "GET")).status;
    if (status === 410) break;
    await delay(500);
  }
  assert.equal(status, 410, "the viewer interest expired without renewal");
  const expiredAfter = Date.now() - subscribedAt;
  assert.ok(expiredAfter >= 15_000, `expiry took the documented viewer lease (${expiredAfter}ms)`);
  const cancel = await relay.waitForFrame(
    (frame) => frame.at >= subscription.at && frame.direction === "hub" &&
      relayMessageType(frame) === "detail.cancel" && frame.delivery === "dropped" &&
      (frame.data as { subscriptionId?: unknown } | null)?.subscriptionId === terms.subscriptionId,
    15_000,
    "the hub's detail.cancel being suppressed by the relay",
  );
  assert.ok(cancel.at >= subscribedAt);
  assert.ok(relay.suppressedHubFrames() > 0, "the relay really hid the hub's renewals/cancellation");

  await delay(Math.max(0, stopBy - Date.now()) + 100);
  const beforeObservation = detailResults(relay, terms.subscriptionId).length;
  const observationMs = Math.max(5_000, terms.intervalMs * 5);
  await delay(observationMs);
  const results = detailResults(relay, terms.subscriptionId);
  const late = results.filter((frame) => frame.at > stopBy);
  assert.deepEqual(
    late.map((frame) => frame.at),
    [],
    `no detail results arrive after the forwarded lease expires (expiry ${expiryAt}, tolerance ${terms.intervalMs}ms)`,
  );
  assert.equal(results.length, beforeObservation, "the result count is stable across several intervals past expiry");
  assert.equal((await collector.stop()).code, 0, collector.dump());
});
