/**
 * Persistent delivery regressions: real collector session + real hub sampler
 * ingestion over the persistent (summary) store.
 *
 * The bridge below is the smallest integration seam that still exercises the
 * production pieces together:
 * - `DestinationSession` and `DestinationOutbox` are the real collector path,
 * - every `samples` frame is parsed with the shared collector contract and fed
 *   to the real `SampleIngestion` over a real `TelemetryStore`,
 * - acknowledgements are only emitted after the store's commit promise resolves,
 *   and each accepted ref is checked against a second SQLite connection before
 *   the ack is sent.
 *
 * Timers and `Date` use `node:test` mock timers, so a multi-second flush
 * window and a 100 ms cadence are deterministic and finish in milliseconds.
 */

import { strict as assert } from "node:assert";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { describe, it, type TestContext } from "node:test";

import { openHubDatabase } from "../src/hub/storage/db";
import { TelemetryStore } from "../src/hub/storage/telemetry-store";
import { HubState } from "../src/hub/state";
import {
  MAX_IN_FLIGHT_SAMPLE_BATCHES,
  SampleIngestion,
  type IngestionStorage,
  type SampleOutcome,
  type SampleSubmission,
} from "../src/hub/sample-ingestion";
import { parseHubConfig, HubConfigError } from "../src/hub/config";
import {
  parseCollectorMessage,
  type AckMessage,
  type RecordRef,
  type SamplesMessage,
  type WelcomeMessage,
} from "../src/contracts";
import {
  DEFAULT_ACK_TIMEOUT_MS,
  DEFAULT_MAX_LOCAL_IN_FLIGHT_BATCHES,
  DestinationSession,
  type CollectorSocket,
} from "../collector/destinations/session";
import { DestinationOutbox } from "../collector/destinations/outbox";
import { makeMetricsRecord, makePayload } from "./storage-fixtures";

const BASE = Date.parse("2026-11-01T12:00:00Z");
const CADENCE_MS = 100;
const COLLECTOR_ID = "collector-1";
const MACHINE_ID = "machine-1";
const RUN_ID = "run-1";
const BOOT_ID = "boot-1";
const SESSION_ID = "session-1";
const IDENTITY = { collectorId: COLLECTOR_ID, runId: RUN_ID, bootId: BOOT_ID, hostname: "host-1" };
const COLLECTORS = [{ collectorId: COLLECTOR_ID, machineId: MACHINE_ID, token: "token-1" }];

/** Accepted persistent flush interval, or null when the hub rejects it. */
function acceptedFlushMs(value: number): number | null {
  try {
    return parseHubConfig({
      HUB_TELEMETRY_MODE: "persistent",
      HUB_TELEMETRY_FLUSH_MS: String(value),
    }).history.flushIntervalMs;
  } catch (error) {
    assert.ok(error instanceof HubConfigError, `expected HubConfigError, got ${String(error)}`);
    return null;
  }
}

/** Let microtasks, the store driver and the session pump run after a tick. */
async function settle(): Promise<void> {
  for (let index = 0; index < 4; index += 1) await Promise.resolve();
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
}

type AckRecord = {
  message: AckMessage;
  /** True when every accepted ref was already visible in SQLite. */
  afterCommit: boolean;
};

type HarnessOptions = {
  flushIntervalMs: number;
  /** Return true once to drop that committed ack (simulates a lost ack). */
  dropAck?: (refs: readonly RecordRef[]) => boolean;
};

type Harness = {
  dir: string;
  observer: DatabaseSync;
  store: TelemetryStore;
  state: HubState;
  ingestion: SampleIngestion;
  outbox: DestinationOutbox;
  session: DestinationSession;
  socket: BridgeSocket;
  welcome: WelcomeMessage | null;
  published: number[];
  /** Every accepted/rejected ref the hub actually committed. */
  outcomes: SampleOutcome[];
  /** Acknowledgements the collector actually received. */
  acks: AckRecord[];
  /** Sends the bridge repeated, e.g. after an ack timeout. */
  samples: SamplesMessage[];
  /** Sequences the hub answered with retry/busy. */
  retried: number[];
  errors: Error[];
  advance(ms: number): Promise<void>;
  deliver(sequence: number, capturedAt?: number): boolean;
  settle(): Promise<void>;
};

/**
 * In-memory socket that speaks the canonical collector wire contract to the
 * real hub ingestion module. It is deliberately not a hub server: the session
 * lifecycle, outbox, framing, batching and ack handling remain production code.
 */
class BridgeSocket implements CollectorSocket {
  readyState = 1;
  bufferedAmount = 0;
  terminated = false;
  private readonly listeners = new Map<string, Array<(...args: unknown[]) => void>>();

  constructor(private readonly bridge: Bridge) {}

  on(event: string, listener: (...args: unknown[]) => void): unknown {
    const bucket = this.listeners.get(event) ?? [];
    bucket.push(listener);
    this.listeners.set(event, bucket);
    return this;
  }

  emit(event: string, ...args: unknown[]): void {
    for (const listener of [...(this.listeners.get(event) ?? [])]) listener(...args);
  }

  send(data: string): void {
    this.bridge.receive(this, data);
  }

  close(): void {
    this.readyState = 3;
  }

  terminate(): void {
    this.terminated = true;
    this.readyState = 3;
  }
}

type Bridge = {
  receive(socket: BridgeSocket, data: string): void;
};

function createHarness(t: TestContext, options: HarnessOptions): Harness {
  // Mock Date as well so capturedAt, receivedAt and every injected clock agree.
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: BASE });
  const dir = mkdtempSync(join(tmpdir(), "rigscope-persistent-delivery-"));
  const path = join(dir, "hub.db");
  const db = openHubDatabase(path);
  const store = new TelemetryStore({
    database: db,
    storageTier: "summary",
    flushIntervalMs: options.flushIntervalMs,
    // Keep the test independent of disk budgets and retention housekeeping.
    maxPhysicalBytes: null,
    maxDatabaseBytes: null,
    now: () => Date.now(),
  });
  const state = new HubState({
    collectors: COLLECTORS,
    hubRunId: "hub-run-1",
    now: () => new Date(Date.now()),
  });
  state.startSession({
    sessionId: SESSION_ID,
    collectorId: COLLECTOR_ID,
    runId: RUN_ID,
    bootId: BOOT_ID,
    hostname: "host-1",
    expectedIntervalMs: CADENCE_MS,
  });
  const published: number[] = [];
  const ingestion = new SampleIngestion({
    state,
    storage: store,
    onLiveSample: (_machineId, record) => published.push(record.sequence),
  });
  const observer = new DatabaseSync(path);
  const outbox = DestinationOutbox.open({
    dir: join(dir, "outbox"),
    identity: IDENTITY,
    allocateSequence: () => 1_000_000,
    now: () => Date.now(),
  });

  const outcomes: SampleOutcome[] = [];
  const acks: AckRecord[] = [];
  const samples: SamplesMessage[] = [];
  const retried: number[] = [];
  const errors: Error[] = [];
  const dropAck = options.dropAck;

  const receiptVisible = (ref: RecordRef): boolean => {
    const row = observer
      .prepare(
        "SELECT COUNT(*) AS n FROM sample_receipts WHERE collector_id = ? AND run_id = ? AND seq = ?",
      )
      .get(COLLECTOR_ID, ref.runId, ref.sequence) as { n: number };
    return Number(row.n) === 1;
  };

  let socket: BridgeSocket;
  let welcomeMessage: WelcomeMessage | null = null;
  const bridge: Bridge = {
    receive(target, data) {
      let message;
      try {
        message = parseCollectorMessage(JSON.parse(data) as unknown);
      } catch (error) {
        errors.push(error instanceof Error ? error : new Error(String(error)));
        return;
      }
      if (message.type === "hello") {
        // Answer off the current stack, as a real socket would.
        queueMicrotask(() => {
          const welcome: WelcomeMessage = {
            type: "welcome",
            hubRunId: state.hubRunId,
            machineId: MACHINE_ID,
            // The composition root advertises the storage contract this way.
            delivery: store.historyMode === "persistent" ? "durable" : "memory",
            maxInFlightBatches: MAX_IN_FLIGHT_SAMPLE_BATCHES,
          };
          welcomeMessage = welcome;
          target.emit("message", JSON.stringify(welcome));
        });
        return;
      }
      if (message.type !== "samples") return;
      samples.push(message);
      const submission = ingestion.submitSamples(SESSION_ID, message.mode, message.records);
      if (submission.status === "retry") {
        retried.push(...message.records.map((record) => record.sequence));
        queueMicrotask(() => {
          target.emit(
            "message",
            JSON.stringify({
              type: "ack",
              accepted: [],
              rejected: [],
              retryAfterMs: submission.retryAfterMs,
            } satisfies AckMessage),
          );
        });
        return;
      }
      void submission.committed.then((outcome) => {
        outcomes.push(outcome);
        if (outcome.status !== "committed") {
          retried.push(...message.records.map((record) => record.sequence));
          target.emit(
            "message",
            JSON.stringify({
              type: "ack",
              accepted: [],
              rejected: [],
              retryAfterMs: outcome.retryAfterMs,
            } satisfies AckMessage),
          );
          return;
        }
        const afterCommit = outcome.accepted.every(receiptVisible);
        const acksBatch = {
          type: "ack",
          accepted: outcome.accepted,
          rejected: outcome.rejected,
        } satisfies AckMessage;
        if (dropAck?.(outcome.accepted) === true) return;
        acks.push({ message: acksBatch, afterCommit });
        target.emit("message", JSON.stringify(acksBatch));
      }).catch((error: unknown) => {
        errors.push(error instanceof Error ? error : new Error(String(error)));
      });
    },
  };

  const sessionOptions: ConstructorParameters<typeof DestinationSession>[0] = {
    name: "hub0",
    url: "ws://127.0.0.1:1/collect",
    token: "token-1",
    identity: { ...IDENTITY },
    outbox,
    expectedIntervalMs: CADENCE_MS,
    capabilities: ["metrics"],
    socketFactory: () => {
      socket = new BridgeSocket(bridge);
      queueMicrotask(() => socket.emit("open"));
      return socket;
    },
    welcomeTimeoutMs: 1_000,
    idleTimeoutMs: 120_000,
    // No accidental reconnect: each test receives at most one welcome.
    backoff: { baseMs: 900_000, maxMs: 900_000 },
  };
  const session = new DestinationSession(sessionOptions);

  t.after(async () => {
    session.stop();
    outbox.close();
    try {
      observer.close();
    } catch {
      // Already closed by a failed setup path.
    }
    await store.close();
    try {
      db.close();
    } catch {
      // The store owns the shared handle.
    }
    rmSync(dir, { recursive: true, force: true });
  });

  const harness: Harness = {
    dir,
    observer,
    store,
    state,
    ingestion,
    outbox,
    session,
    get socket() {
      return socket;
    },
    get welcome() {
      return welcomeMessage;
    },
    published,
    outcomes,
    acks,
    samples,
    retried,
    errors,
    async advance(ms) {
      t.mock.timers.tick(ms);
      await settle();
    },
    deliver(sequence, capturedAt = Date.now()) {
      const record = makeMetricsRecord({
        collectorId: COLLECTOR_ID,
        runId: RUN_ID,
        bootId: BOOT_ID,
        sequence,
        capturedAt,
        payload: makePayload({ cores: 2, coreValue: 5, cpuUsage: 10 + (sequence % 80) }),
      });
      outbox.append(record);
      return session.deliverLive();
    },
    settle,
  };
  return harness;
}

/** Drives the harness through its handshake. */
async function welcome(harness: Harness): Promise<void> {
  harness.session.start();
  await settle();
  assert.equal(
    harness.session.describeState().status,
    "welcomed",
    JSON.stringify({ state: harness.session.describeState(), errors: harness.errors.map(String) }),
  );
  assert.equal(harness.session.describeState().inFlightWindow, Math.min(
    MAX_IN_FLIGHT_SAMPLE_BATCHES,
    DEFAULT_MAX_LOCAL_IN_FLIGHT_BATCHES,
  ));
}

function acceptedSequences(harness: Harness): number[] {
  return harness.acks.flatMap((ack) => ack.message.accepted.map((ref) => ref.sequence));
}

function uniqueSorted(values: readonly number[]): number[] {
  return [...new Set(values)].sort((a, b) => a - b);
}

describe("persistent default flush with the fastest supported cadence", () => {
  it("keeps fresh live delivery flowing and acknowledges only after the commit", async (t) => {
    // The historical default was ten seconds. Use it while it is accepted so
    // this regression keeps covering that exact configuration; after a safe
    // rejection, cover the widest accepted interval instead.
    const configured = parseHubConfig({ HUB_TELEMETRY_MODE: "persistent" });
    assert.equal(configured.history.mode, "persistent");
    const flushIntervalMs = acceptedFlushMs(10_000) ?? configured.history.flushIntervalMs;
    assert.ok(flushIntervalMs > 0);

    // At 100 ms cadence, one flush window holds exactly flush/100 batches. Add
    // a margin so the collector must cross a flush boundary and keep going.
    const samples = Math.ceil(flushIntervalMs / CADENCE_MS) + 50;
    const harness = createHarness(t, { flushIntervalMs });
    await welcome(harness);

    const stalls: number[] = [];
    const missingLive: number[] = [];
    let maxInFlight = 0;
    for (let sequence = 1; sequence <= samples; sequence += 1) {
      await harness.advance(CADENCE_MS);
      const sent = harness.deliver(sequence);
      await settle();
      if (!sent) stalls.push(sequence);
      if (harness.published.at(-1) !== sequence) missingLive.push(sequence);
      const state = harness.session.describeState();
      maxInFlight = Math.max(maxInFlight, state.inFlightBatches);
      assert.ok(
        state.inFlightBatches <= state.inFlightWindow,
        `collector in-flight ${state.inFlightBatches} exceeded its window ${state.inFlightWindow}`,
      );
      assert.ok(
        state.inFlightBatches <= MAX_IN_FLIGHT_SAMPLE_BATCHES,
        "collector in-flight batches must stay inside the hub-advertised bound",
      );
    }

    // Drain whatever the last flush window still holds. Keep simulating the
    // documented cadence; deterministic mock time makes this cheap.
    const deadline = flushIntervalMs * 2 + 10_000;
    for (let elapsed = 0;
      elapsed < deadline &&
        (harness.outbox.status().pendingRecords > 0 ||
          harness.session.describeState().inFlightBatches > 0);
      elapsed += CADENCE_MS) {
      await harness.advance(CADENCE_MS);
    }

    assert.deepEqual(stalls, [], "fresh live delivery must never wait for an older flush");
    assert.deepEqual(missingLive, [], "every captured sample must reach live state immediately");
    assert.deepEqual(harness.retried, [], "supported cadence must not overflow the bounded hub queue");
    assert.deepEqual(harness.errors, []);
    assert.ok(
      maxInFlight <= Math.min(MAX_IN_FLIGHT_SAMPLE_BATCHES, DEFAULT_MAX_LOCAL_IN_FLIGHT_BATCHES),
      `in-flight batches stayed bounded (max ${maxInFlight})`,
    );
    assert.equal(
      harness.outbox.status().pendingRecords,
      0,
      "the bounded outbox drains once commits arrive",
    );
    assert.equal(
      harness.session.describeState().inFlightBatches,
      0,
      "no acknowledged batch stays in flight",
    );

    const accepted = uniqueSorted(acceptedSequences(harness));
    assert.equal(accepted.length, samples, "every sample is acknowledged exactly once");
    assert.equal(accepted[0], 1);
    assert.equal(accepted.at(-1), samples);
    assert.ok(
      harness.acks.every((ack) => ack.afterCommit),
      "every acknowledgement was sent only after its receipt was visible in SQLite",
    );
    assert.equal(harness.published.length, samples, "live publication is not repeated");
    assert.equal(harness.store.stats().committedRecords, samples);
    assert.equal(harness.store.stats().duplicateRecords, 0);

    // A second connection proves the durable summary holds each sample once.
    const series = harness.observer
      .prepare(
        "SELECT series_id FROM series WHERE machine_id = ? AND metric = 'cpu.usage' AND resource_id = ''",
      )
      .get(MACHINE_ID) as { series_id: number } | undefined;
    assert.ok(series, "the persistent store created the cpu.usage series");
    const total = harness.observer
      .prepare("SELECT COALESCE(SUM(count), 0) AS n FROM points_10s WHERE series_id = ?")
      .get(series.series_id) as { n: number };
    assert.equal(Number(total.n), samples, "each sample is folded into the summary exactly once");
    assert.equal(
      harness.state.snapshot().machines[0]!.metrics?.sequence,
      samples,
      "live state ends at the newest sample",
    );
  });

  it("publishes live immediately but withholds the ack until the default flush commits", async (t) => {
    const configured = parseHubConfig({ HUB_TELEMETRY_MODE: "persistent" });
    const harness = createHarness(t, { flushIntervalMs: configured.history.flushIntervalMs });
    await welcome(harness);

    assert.equal(harness.welcome?.delivery, "durable");
    assert.equal(harness.welcome?.maxInFlightBatches, MAX_IN_FLIGHT_SAMPLE_BATCHES);
    assert.equal(harness.deliver(1), true);
    await settle();
    assert.deepEqual(harness.published, [1], "live state does not wait for the flush");
    assert.equal(harness.state.snapshot().machines[0]!.metrics?.sequence, 1);
    assert.equal(harness.acks.length, 0, "no acknowledgement before the commit");
    assert.equal(harness.outbox.status().inFlightRecords, 1);
    assert.equal(
      (harness.observer.prepare("SELECT COUNT(*) AS n FROM sample_receipts").get() as { n: number }).n,
      0,
    );

    // Cross the configured flush interval with deterministic mock time.
    await harness.advance(configured.history.flushIntervalMs);
    assert.equal(harness.acks.length, 1);
    assert.equal(harness.acks[0]!.afterCommit, true);
    assert.deepEqual(
      harness.acks[0]!.message.accepted.map((ref) => ref.sequence),
      [1],
    );
    assert.equal(harness.outbox.status().inFlightRecords, 0);
    assert.equal(
      (harness.observer.prepare("SELECT COUNT(*) AS n FROM sample_receipts").get() as { n: number }).n,
      1,
    );
    assert.deepEqual(harness.published, [1], "a delayed ack never republishes the sample");
  });
});

describe("bounded queues and replay deduplication", () => {
  it("answers retry beyond the hub in-flight bound and releases the bound on commit", async (t) => {
    t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: BASE });
    const dir = mkdtempSync(join(tmpdir(), "rigscope-persistent-bound-"));
    const state = new HubState({
      collectors: COLLECTORS,
      hubRunId: "hub-run-1",
      now: () => new Date(Date.now()),
    });
    state.startSession({
      sessionId: SESSION_ID,
      collectorId: COLLECTOR_ID,
      runId: RUN_ID,
      bootId: BOOT_ID,
      hostname: "host-1",
      expectedIntervalMs: CADENCE_MS,
    });
    const published: number[] = [];
    const pending: Array<{ resolve(result: Awaited<ReturnType<IngestionStorage["enqueue"]>>): void }> = [];
    const storage: IngestionStorage = {
      enqueue(batch) {
        return new Promise((resolve) => {
          pending.push({ resolve });
          assert.ok(batch.records.length > 0);
        });
      },
      latestLive: () => [],
      saveInventory: () => {},
      loadInventory: () => null,
      getMachineMeta: () => ({ displayName: null, tags: [] }),
    };
    const ingestion = new SampleIngestion({
      state,
      storage,
      onLiveSample: (_machineId, record) => published.push(record.sequence),
    });
    t.after(() => {
      for (const entry of pending.splice(0)) {
        entry.resolve({
          status: "committed",
          accepted: [],
          rejected: [],
          newLive: null,
          receivedAtMs: Date.now(),
        });
      }
      rmSync(dir, { recursive: true, force: true });
    });

    const submissions: SampleSubmission[] = [];
    for (let sequence = 1; sequence <= MAX_IN_FLIGHT_SAMPLE_BATCHES; sequence += 1) {
      const record = makeMetricsRecord({
        collectorId: COLLECTOR_ID,
        runId: RUN_ID,
        bootId: BOOT_ID,
        sequence,
        capturedAt: Date.now(),
        payload: makePayload({ cores: 2 }),
      });
      const submission = ingestion.submitSamples(SESSION_ID, "live", [record]);
      assert.equal(submission.status, "queued");
      submissions.push(submission);
    }
    assert.equal(published.length, MAX_IN_FLIGHT_SAMPLE_BATCHES);
    assert.equal(pending.length, MAX_IN_FLIGHT_SAMPLE_BATCHES);

    const overflow = ingestion.submitSamples(
      SESSION_ID,
      "live",
      [makeMetricsRecord({
        collectorId: COLLECTOR_ID,
        runId: RUN_ID,
        bootId: BOOT_ID,
        sequence: MAX_IN_FLIGHT_SAMPLE_BATCHES + 1,
        capturedAt: Date.now(),
        payload: makePayload({ cores: 2 }),
      })],
    );
    assert.equal(overflow.status, "retry", "overload is answered, not queued without bound");
    if (overflow.status === "retry") assert.equal(overflow.retryAfterMs, 1_000);
    assert.equal(
      published.length,
      MAX_IN_FLIGHT_SAMPLE_BATCHES,
      "an overloaded submission does not publish live state",
    );

    // Commit every queued batch; each in-flight slot becomes available again.
    const latest = makeMetricsRecord({
      collectorId: COLLECTOR_ID,
      runId: RUN_ID,
      bootId: BOOT_ID,
      sequence: MAX_IN_FLIGHT_SAMPLE_BATCHES,
      capturedAt: Date.now(),
      payload: makePayload({ cores: 2 }),
    });
    for (const entry of pending.splice(0)) {
      entry.resolve({
        status: "committed",
        accepted: [{ runId: RUN_ID, sequence: latest.sequence }],
        rejected: [],
        newLive: latest,
        receivedAtMs: Date.now(),
      });
    }
    await settle();
    assert.equal(state.currentSession(SESSION_ID) !== null, true);
    const next = ingestion.submitSamples(
      SESSION_ID,
      "live",
      [makeMetricsRecord({
        collectorId: COLLECTOR_ID,
        runId: RUN_ID,
        bootId: BOOT_ID,
        sequence: MAX_IN_FLIGHT_SAMPLE_BATCHES + 2,
        capturedAt: Date.now(),
        payload: makePayload({ cores: 2 }),
      })],
    );
    assert.equal(next.status, "queued", "the bound is available again after acknowledgements");
    if (next.status === "queued") {
      pending.splice(0).forEach((entry) => entry.resolve({
        status: "committed",
        accepted: [],
        rejected: [],
        newLive: null,
        receivedAtMs: Date.now(),
      }));
      await next.committed;
    }
    for (const submission of submissions) {
      if (submission.status === "queued") await submission.committed;
    }
  });

  it("resends after a lost ack and lets the hub deduplicate the replay", async (t) => {
    const configured = parseHubConfig({ HUB_TELEMETRY_MODE: "persistent" });
    let dropped = false;
    const harness = createHarness(t, {
      flushIntervalMs: configured.history.flushIntervalMs,
      // Use the collector's default acknowledgement deadline so the regression
      // exercises the production timeout, not a shortened test value.
      dropAck: (refs) => {
        if (!dropped && refs.some((ref) => ref.sequence === 1)) {
          dropped = true;
          return true;
        }
        return false;
      },
    });
    await welcome(harness);

    assert.equal(harness.deliver(1), true);
    await settle();
    // Commit the first attempt, then lose its acknowledgement.
    await harness.advance(configured.history.flushIntervalMs);
    assert.equal(harness.outcomes.length, 1);
    assert.equal(harness.acks.length, 0, "the first acknowledgement is lost");
    assert.equal(harness.outbox.status().inFlightRecords, 1);

    const receiptCount = (): number =>
      Number(
        (harness.observer.prepare(
          "SELECT COUNT(*) AS n FROM sample_receipts WHERE run_id = ? AND seq = 1",
        ).get(RUN_ID) as { n: number }).n,
      );
    assert.equal(receiptCount(), 1, "the first attempt committed before the ack was lost");

    // The ack timeout releases the batch and the session replays it.
    await harness.advance(DEFAULT_ACK_TIMEOUT_MS - configured.history.flushIntervalMs);
    assert.ok(
      harness.session
        .describeState()
        .status === "welcomed",
      "the session stays connected while it replays",
    );
    assert.ok(
      harness.samples.filter((message) =>
        message.mode === "replay" && message.records.some((record) => record.sequence === 1),
      ).length >= 1,
      "the committed record is resent as a replay",
    );

    // The retry commits (as a duplicate), is acknowledged and clears the outbox.
    await harness.advance(configured.history.flushIntervalMs);
    assert.equal(harness.acks.length, 1);
    assert.equal(harness.acks[0]!.afterCommit, true);
    assert.deepEqual(
      harness.acks[0]!.message.accepted.map((ref) => ref.sequence),
      [1],
    );
    assert.equal(receiptCount(), 1, "deduplication keeps exactly one receipt");
    assert.equal(harness.store.stats().committedRecords, 1);
    assert.equal(harness.store.stats().duplicateRecords, 1, "the replay was recognized as a duplicate");
    assert.deepEqual(harness.published, [1], "a replay never republishes live state");
    assert.equal(harness.outbox.status().pendingRecords, 0);
    assert.equal(harness.outbox.status().inFlightRecords, 0);
    assert.equal(harness.state.snapshot().machines[0]!.metrics?.sequence, 1);
  });
});

describe("flush configuration compatibility", () => {
  it("keeps accepted flush intervals inside the collector acknowledgement deadline", () => {
    const configured = parseHubConfig({ HUB_TELEMETRY_MODE: "persistent" });
    assert.ok(
      configured.history.flushIntervalMs + 1_000 <= DEFAULT_ACK_TIMEOUT_MS,
      `the default ${configured.history.flushIntervalMs}ms flush must leave headroom ` +
        `inside the ${DEFAULT_ACK_TIMEOUT_MS}ms default collector deadline`,
    );

    // Every interval the hub accepts has to remain compatible with a collector
    // that only knows the default deadline. A value the hub cannot answer
    // within that deadline must be rejected, never accepted and then retried
    // forever by every default collector.
    for (const candidate of [5_000, 10_000, 30_000, 120_000]) {
      let accepted: number | null = null;
      try {
        accepted = parseHubConfig({
          HUB_TELEMETRY_MODE: "persistent",
          HUB_TELEMETRY_FLUSH_MS: String(candidate),
        }).history.flushIntervalMs;
      } catch (error) {
        assert.ok(error instanceof HubConfigError);
        assert.match(
          error.message,
          /HUB_TELEMETRY_FLUSH_MS/,
          `a rejected interval names the exact variable (${error.message})`,
        );
        continue;
      }
      assert.ok(
        accepted < DEFAULT_ACK_TIMEOUT_MS,
        `HUB_TELEMETRY_FLUSH_MS=${candidate} is accepted (${accepted}ms) but the ` +
          `collector's default ack deadline is ${DEFAULT_ACK_TIMEOUT_MS}ms; reject it ` +
          "or negotiate a longer deadline in the welcome contract",
      );
    }

    // The historical incompatibility: 120s was accepted while the collector
    // times out at 30s. Either answer is legitimate now: a startup rejection
    // or a negotiated deadline. Prove the current contract picks at least one.
    const extreme = acceptedFlushMs(120_000);
    if (extreme === null) {
      assert.throws(
        () =>
          parseHubConfig({
            HUB_TELEMETRY_MODE: "persistent",
            HUB_TELEMETRY_FLUSH_MS: "120000",
          }),
        (error: unknown) =>
          error instanceof HubConfigError &&
          /HUB_TELEMETRY_FLUSH_MS/.test(error.message),
        "an incompatible flush interval is rejected with a precise configuration error",
      );
    } else {
      // Accepted means a default collector must learn about the longer
      // deadline through the welcome instead of timing out at 30s. The
      // session applies the deadline it is given, so exercise that path with
      // the deterministic bridge below only after the public contract exposes
      // it. Until then, accepting 120s is a compatibility bug.
      assert.fail(
        `HUB_TELEMETRY_FLUSH_MS=120000 is still accepted (${extreme}ms) while the ` +
          `collector default ack deadline is ${DEFAULT_ACK_TIMEOUT_MS}ms; reject the value ` +
          "or advertise and honour a negotiated deadline",
      );
    }
  });
});


it("keeps fast live delivery flowing while a reconnect replay backlog drains", async (t) => {
  const config = parseHubConfig({ HUB_TELEMETRY_MODE: "persistent" });
  const harness = createHarness(t, { flushIntervalMs: config.history.flushIntervalMs });
  for (let sequence = 1; sequence <= 2_500; sequence += 1) {
    harness.outbox.append(makeMetricsRecord({
      collectorId: COLLECTOR_ID,
      runId: "old-run",
      bootId: BOOT_ID,
      sequence,
      capturedAt: BASE - 300_000 + sequence * CADENCE_MS,
      payload: makePayload({ cores: 2 }),
    }));
  }
  // A reopened outbox has no fresh live candidate from the previous run.
  harness.outbox.takeLive();
  await welcome(harness);
  const stalled: number[] = [];
  for (let sequence = 1; sequence <= 150; sequence += 1) {
    if (!harness.deliver(sequence)) stalled.push(sequence);
    await harness.advance(CADENCE_MS);
  }
  assert.deepEqual(stalled, [], "replay must leave room for the full live flush window");
  assert.equal(harness.published.length, 150, "all fresh samples publish immediately");
  assert.ok(harness.samples.some((message) => message.mode === "replay"));
  assert.ok(harness.acks.every((ack) => ack.afterCommit));
  assert.deepEqual(harness.retried, []);
  assert.deepEqual(harness.errors, []);
});
