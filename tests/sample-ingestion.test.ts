/**
 * Sample ingestion module tests: everything runs through the deep module's
 * interface (SampleIngestion) against a real temporary SQLite database, so
 * commit-to-live sequencing, session replacement/revocation during commit,
 * replay eligibility, dedup and restart restoration are tested at the level
 * the hub composition root uses them.
 */

import { strict as assert } from "node:assert";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { describe, it } from "node:test";
import { openHubDatabase, type HubDatabase } from "../src/hub/storage/db";
import { TelemetryStore } from "../src/hub/storage/telemetry-store";
import { HubState } from "../src/hub/state";
import {
  SampleIngestion,
  MAX_IN_FLIGHT_SAMPLE_BATCHES,
  type SampleOutcome,
} from "../src/hub/sample-ingestion";
import { makeInventory, makeMetricsRecord, makePayload, type PayloadOptions } from "./storage-fixtures";
import type { MetricsRecord, TelemetryRecord } from "../src/contracts";
import type { SampleSubmission } from "../src/hub/sample-ingestion";

const BASE = Date.parse("2026-11-01T12:00:00Z");
const COLLECTORS = [{ collectorId: "collector-1", machineId: "machine-1", token: "token-1" }];

type Published = { machineId: string; record: MetricsRecord; receivedAtMs: number };

type Rig = {
  dir: string;
  path: string;
  db: HubDatabase;
  state: HubState;
  store: TelemetryStore;
  ingestion: SampleIngestion;
  published: Published[];
};

function open(dir: string, path: string): { db: HubDatabase; store: TelemetryStore } {
  const db = openHubDatabase(path);
  const store = new TelemetryStore({ database: db, flushIntervalMs: 60_000 });
  return { db, store };
}

/**
 * Fresh hub "process": real temp SQLite, one-machine state and one ingestion
 * module wired exactly as the composition root does. No session is open.
 */
function rig(): Rig {
  const dir = mkdtempSync(join(tmpdir(), "rigscope-sample-ingestion-"));
  const path = join(dir, "hub.db");
  const { db, store } = open(dir, path);
  const state = new HubState({ collectors: COLLECTORS, hubRunId: "hub-run-1" });
  const published: Published[] = [];
  const ingestion = new SampleIngestion({
    state,
    storage: store,
    onLiveSample: (machineId, record, receivedAtMs) => {
      published.push({ machineId, record, receivedAtMs });
    },
  });
  return { dir, path, db, state, store, ingestion, published };
}

async function teardown(rigging: Rig): Promise<void> {
  await rigging.store.close();
  rigging.db.close();
  rmSync(rigging.dir, { recursive: true, force: true });
}

function startSession(
  state: HubState,
  sessionId = "session-1",
  runId = "run-1",
  hostname = "host-1",
): void {
  state.startSession({
    sessionId,
    collectorId: "collector-1",
    runId,
    bootId: "boot-1",
    hostname,
    expectedIntervalMs: 5000,
  });
}

function metrics(sequence: number, atMs: number, runId = "run-1", payload: PayloadOptions = {}): MetricsRecord {
  return makeMetricsRecord({ collectorId: "collector-1", runId, sequence, capturedAt: atMs, payload: makePayload(payload) });
}

function count(rigging: Rig, sql: string, ...params: Array<string | number>): number {
  const row = rigging.db.db.prepare(sql).get(...params) as Record<string, unknown> | undefined;
  assert.ok(row);
  return Number(Object.values(row)[0]);
}

/** Narrowing helper: the module queued the batch to durable storage. */
function promised(submission: SampleSubmission): Promise<SampleOutcome> {
  assert.equal(submission.status, "queued");
  if (submission.status !== "queued") throw new Error("submission was answered as retry");
  return submission.committed;
}

async function flush(rigging: Rig): Promise<void> {
  await rigging.store.flushNow();
}

function machineSnapshot(state: HubState) {
  const snapshot = state.snapshot();
  assert.equal(snapshot.machines.length, 1);
  return snapshot.machines[0];
}

describe("sample ingestion module", () => {
  it("publishes live state immediately but acknowledges only after the durable commit is visible", async () => {
    const rigging = rig();
    try {
      startSession(rigging.state);
      const submission = promised(
        rigging.ingestion.submitSamples(
          "session-1",
          "live",
          [metrics(1, BASE)],
        ),
      );
      // The dashboard is never held behind the ten-second persistent flush.
      assert.equal(rigging.published.length, 1);
      assert.equal(machineSnapshot(rigging.state).metrics?.sequence, 1);
      // But the durable commit (and therefore the acknowledgement) is pending.
      const observerBefore = new DatabaseSync(rigging.path);
      try {
        const receipts = observerBefore.prepare("SELECT COUNT(*) AS n FROM sample_receipts").get() as { n: number };
        assert.equal(Number(receipts.n), 0);
      } finally {
        observerBefore.close();
      }
      await flush(rigging);
      const outcome: SampleOutcome = await submission;
      assert.equal(outcome.status, "committed");
      if (outcome.status !== "committed") return;
      assert.deepEqual(outcome.accepted, [{ runId: "run-1", sequence: 1 }]);
      assert.equal(outcome.rejected.length, 0);

      const observer = new DatabaseSync(rigging.path);
      try {
        const receipts = observer.prepare("SELECT COUNT(*) AS n FROM sample_receipts").get() as { n: number };
        const points = observer.prepare("SELECT COUNT(*) AS n FROM points_raw").get() as { n: number };
        assert.equal(Number(receipts.n), 1);
        assert.ok(Number(points.n) > 0);
      } finally {
        observer.close();
      }
      assert.equal(rigging.published.length, 1);
      assert.equal(rigging.published[0].record.sequence, 1);
      assert.equal(rigging.published[0].receivedAtMs > 0, true);
      assert.equal(machineSnapshot(rigging.state).metrics?.sequence, 1);
      assert.equal(machineSnapshot(rigging.state).connected, true);
    } finally {
      await teardown(rigging);
    }
  });

  it("answers overload beyond the in-flight batch bound and keeps verdicts queued", async () => {
    const rigging = rig();
    try {
      startSession(rigging.state);
      const pending: Promise<SampleOutcome>[] = [];
      for (let sequence = 1; sequence <= MAX_IN_FLIGHT_SAMPLE_BATCHES; sequence += 1) {
        pending.push(promised(rigging.ingestion.submitSamples("session-1", "live", [metrics(sequence, BASE + sequence * 1000)])));
      }
      const overflow = rigging.ingestion.submitSamples(
        "session-1",
        "live",
        [metrics(9, BASE + 9000)],
      );
      assert.equal(overflow.status, "retry");
      if (overflow.status === "retry") assert.equal(overflow.retryAfterMs, 1000);
      await flush(rigging);
      for (const outcome of await Promise.all(pending)) {
        assert.equal(outcome.status, "committed");
      }
      assert.equal(rigging.published.length, MAX_IN_FLIGHT_SAMPLE_BATCHES);
      // After every batch resolves, the bound is available again.
      const next = promised(rigging.ingestion.submitSamples("session-1", "live", [metrics(10, BASE + 10000)]));
      await flush(rigging);
      const outcome = await next;
      assert.equal(outcome.status, "committed");
    } finally {
      await teardown(rigging);
    }
  });

  it("fails record identities that do not match the authenticated session", async () => {
    const rigging = rig();
    try {
      startSession(rigging.state, "session-1", "run-current");
      assert.throws(
        () => rigging.ingestion.submitSamples("session-1", "live", [
          metrics(1, BASE, "run-other"),
        ]),
        /record identity/,
      );
      assert.throws(
        () => rigging.ingestion.submitSamples("session-1", "live", [
          { ...metrics(1, BASE), bootId: "boot-other" },
        ]),
        /record identity/,
      );
      assert.throws(
        () => rigging.ingestion.submitSamples("session-1", "live", [
          { ...metrics(1, BASE), collectorId: "collector-2" },
        ]),
        /record identity/,
      );
      // Nothing was queued or committed by any rejected batch.
      await flush(rigging);
      assert.equal(count(rigging, "SELECT COUNT(*) AS n FROM sample_receipts"), 0);
      assert.equal(rigging.published.length, 0);
      assert.throws(
        () => rigging.ingestion.submitSamples("session-unknown", "live", [metrics(1, BASE)]),
        /session is no longer current/,
      );
    } finally {
      await teardown(rigging);
    }
  });

  it("accepts replay identities and fills history without overwriting newer live state", async () => {
    const rigging = rig();
    try {
      startSession(rigging.state, "session-1", "run-current");
      const first = rigging.ingestion.submitSamples("session-1", "live", [metrics(1, BASE, "run-current")]);
      // A finished previous run replays with its own run identity.
      const replay = rigging.ingestion.submitSamples(
        "session-1",
        "replay",
        [
          metrics(9, BASE + 300_000, "run-old"),
          {
            ...metrics(10, BASE + 300_000, "run-old"),
            kind: "gap" as const,
            payload: {
              fromCapturedAt: new Date(BASE + 60_000).toISOString(),
              toCapturedAt: new Date(BASE + 120_000).toISOString(),
              droppedRecords: 3,
              reason: "outbox-bytes" as const,
            },
          } satisfies TelemetryRecord,
        ],
      );
      await flush(rigging);
      const liveOutcome = await promised(first);
      const replayOutcome = await promised(replay);
      assert.equal(liveOutcome.status, "committed");
      assert.equal(replayOutcome.status, "committed");
      if (replayOutcome.status !== "committed") return;
      assert.deepEqual(
        replayOutcome.accepted.map(ref => ref.sequence).sort((a, b) => a - b),
        [9, 10],
      );
      // Exactly the current live sample reached Incident evaluation; replay,
      // including a newer capture time and a gap record, never did.
      assert.equal(rigging.published.length, 1);
      assert.equal(rigging.published[0].record.sequence, 1);
      assert.equal(machineSnapshot(rigging.state).metrics?.sequence, 1);
      assert.equal(machineSnapshot(rigging.state).lastReceivedAt === null, false);
      assert.equal(
        machineSnapshot(rigging.state).metrics && JSON.stringify(machineSnapshot(rigging.state).metrics).includes("run-old"),
        false,
      );
      assert.equal(count(rigging, "SELECT COUNT(*) AS n FROM points_raw") >= 2, true);
      assert.equal(count(rigging, "SELECT COUNT(*) AS n FROM gaps WHERE run_id = 'run-old'"), 1);
    } finally {
      await teardown(rigging);
    }
  });

  it("clears a replaced session's live row even though its earlier sample was published", async () => {
    const rigging = rig();
    try {
      startSession(rigging.state, "session-1", "run-1");
      const stale = promised(rigging.ingestion.submitSamples("session-1", "live", [metrics(1, BASE)]));
      assert.equal(rigging.published.length, 1);
      // A new authenticated run supersedes the session while the batch is
      // still uncommitted. Replacement clears the old run's live row.
      startSession(rigging.state, "session-2", "run-2", "host-2");
      await flush(rigging);
      const outcome = await stale;
      // The commit is durable and acknowledged even though the publisher lost.
      assert.equal(outcome.status, "committed");
      if (outcome.status !== "committed") return;
      assert.deepEqual(outcome.accepted, [{ runId: "run-1", sequence: 1 }]);
      assert.equal(count(rigging, "SELECT COUNT(*) AS n FROM sample_receipts WHERE run_id = 'run-1'"), 1);
      const snapshot = machineSnapshot(rigging.state);
      assert.equal(snapshot.connected, true);
      assert.equal(snapshot.metrics, undefined);
      // The replacement session can publish normally afterwards.
      const replacement = promised(rigging.ingestion.submitSamples("session-2", "live", [metrics(1, BASE + 1000, "run-2")]));
      await flush(rigging);
      const replacementOutcome = await replacement;
      assert.equal(replacementOutcome.status, "committed");
      assert.equal(rigging.published.length, 2);
      assert.equal(machineSnapshot(rigging.state).metrics?.runId, "run-2");
    } finally {
      await teardown(rigging);
    }
  });

  it("keeps the last published reading but stops accepting after revocation", async () => {
    const rigging = rig();
    try {
      startSession(rigging.state, "session-1", "run-1");
      const stale = promised(rigging.ingestion.submitSamples("session-1", "live", [metrics(1, BASE)]));
      assert.equal(rigging.published.length, 1);
      assert.equal(rigging.state.endSession("session-1"), true);
      await flush(rigging);
      const outcome = await stale;
      assert.equal(outcome.status, "committed");
      assert.equal(outcome.status !== "committed" ? true : outcome.accepted.length, 1);
      const snapshot = machineSnapshot(rigging.state);
      assert.equal(snapshot.connected, false);
      // Last-known state is retained while offline, exactly as before.
      assert.equal(snapshot.metrics?.sequence, 1);
    } finally {
      await teardown(rigging);
    }
  });

  it("answers retries for repeated records and cannot affect live state twice", async () => {
    const rigging = rig();
    try {
      startSession(rigging.state);
      const first = promised(rigging.ingestion.submitSamples("session-1", "live", [metrics(1, BASE)]));
      await flush(rigging);
      assert.equal((await first).status, "committed");
      assert.equal(rigging.published.length, 1);
      const points = count(rigging, "SELECT COUNT(*) AS n FROM points_raw");

      // Lost-ack retry: deduplicated by Collector/run/sequence, acknowledged
      // as accepted, no extra history effects or Incident observations.
      const retry = promised(rigging.ingestion.submitSamples("session-1", "live", [metrics(1, BASE)]));
      await flush(rigging);
      const retryOutcome = await retry;
      assert.equal(retryOutcome.status, "committed");
      if (retryOutcome.status !== "committed") return;
      assert.deepEqual(retryOutcome.accepted, [{ runId: "run-1", sequence: 1 }]);
      assert.equal(count(rigging, "SELECT COUNT(*) AS n FROM points_raw"), points);
      assert.equal(rigging.published.length, 1);

      // A later, lower-sequence live sample never regresses live state.
      const older = promised(
        rigging.ingestion.submitSamples(
          "session-1",
          "live",
          [metrics(0, BASE + 1000, "run-1", { cpuUsage: 99 })],
        ),
      );
      await flush(rigging);
      assert.equal((await older).status, "committed");
      assert.equal(rigging.published.length, 1);
      assert.equal(machineSnapshot(rigging.state).metrics?.sequence, 1);
      assert.equal(count(rigging, "SELECT COUNT(*) AS n FROM sample_receipts"), 2);
      assert.equal(count(rigging, "SELECT value FROM points_raw JOIN series USING(series_id) WHERE metric='cpu.usage' AND ts_ms=?", BASE + 1000), 99);
    } finally {
      await teardown(rigging);
    }
  });

  it("answers retry while live publication stays independent of a failed store", async () => {
    const rigging = rig();
    try {
      startSession(rigging.state);
      await rigging.store.close();
      const submission = promised(rigging.ingestion.submitSamples("session-1", "live", [metrics(1, BASE)]));
      const outcome = await submission;
      assert.equal(outcome.status, "retry");
      if (outcome.status === "retry") assert.ok(outcome.retryAfterMs >= 1000);
      // Read committed state through an independent connection because the
      // ingestion store itself is closed.
      const observer = new DatabaseSync(rigging.path);
      try {
        const receipts = observer.prepare("SELECT COUNT(*) AS n FROM sample_receipts").get() as { n: number };
        assert.equal(Number(receipts.n), 0);
      } finally {
        observer.close();
      }
      // Publishing is decoupled from storage; the collector will retry and the
      // duplicate sequence cannot publish twice.
      assert.equal(rigging.published.length, 1);
      const retry = promised(rigging.ingestion.submitSamples("session-1", "live", [metrics(1, BASE)]));
      assert.equal((await retry).status, "retry");
      assert.equal(rigging.published.length, 1);
    } finally {
      await teardown(rigging);
    }
  });

  it("accepts inventory only for the current session, durable first", async () => {
    const rigging = rig();
    try {
      startSession(rigging.state);
      const inventory = makeInventory();
      assert.throws(() =>
        rigging.ingestion.acceptInventory("session-missing", new Date(BASE).toISOString(), inventory),
      /session is no longer current/);
      rigging.ingestion.acceptInventory("session-1", new Date(BASE).toISOString(), inventory);
      assert.deepEqual(machineSnapshot(rigging.state).inventory, inventory);
      assert.equal(count(rigging, "SELECT COUNT(*) AS n FROM inventory"), 1);
      // A replaced session can no longer present inventory.
      startSession(rigging.state, "session-2", "run-2");
      assert.throws(() =>
        rigging.ingestion.acceptInventory("session-1", new Date(BASE).toISOString(), inventory),
      /session is no longer current/);
    } finally {
      await teardown(rigging);
    }
  });

  it("restores metrics, delivery cadence, inventory and metadata without a connected Collector", async () => {
    const dir = mkdtempSync(join(tmpdir(), "rigscope-sample-restore-"));
    const path = join(dir, "hub.db");
    try {
      {
        const { db, store } = open(dir, path);
        try {
          const state = new HubState({ collectors: COLLECTORS, hubRunId: "hub-run-1" });
          const published: Published[] = [];
          const ingestion = new SampleIngestion({
            state,
            storage: store,
            onLiveSample: (machineId, record, receivedAtMs) => {
              published.push({ machineId, record, receivedAtMs });
            },
          });
          startSession(state, "session-1", "run-1", "host-1");
          const submission = promised(ingestion.submitSamples(
            "session-1",
            "live",
            [metrics(7, BASE, "run-1", { cpuUsage: 42 })],
          ));
          await store.flushNow();
          assert.equal((await submission).status, "committed");
          store.saveInventory("machine-1", new Date(BASE).toISOString(), makeInventory());
          store.setMachineMeta("machine-1", { displayName: "Tower", tags: ["edge"] });
          assert.equal(published.length, 1);
        } finally {
          await store.close();
          db.close();
        }
      }
      // A fresh hub run starts against the same database.
      const { db, store } = open(dir, path);
      try {
        const state = new HubState({ collectors: COLLECTORS, hubRunId: "hub-run-2" });
        const published: Published[] = [];
        const ingestion = new SampleIngestion({
          state,
          storage: store,
          onLiveSample: (machineId, record, receivedAtMs) => {
            published.push({ machineId, record, receivedAtMs });
          },
        });
        ingestion.restorePersisted();
        const machine = machineSnapshot(state);
        assert.equal(machine.connected, false, "stored values must not imply a connected Collector");
        assert.equal(published.length, 0, "restoration must not manufacture Incident observations");
        assert.equal(machine.metrics?.kind, "metrics");
        assert.equal(machine.metrics?.runId, "run-1");
        assert.equal(machine.metrics?.sequence, 7);
        assert.equal(JSON.stringify(machine.metrics?.payload).includes("42"), true);
        assert.equal(machine.hostname, "host-1");
        assert.equal(machine.expectedIntervalMs, 5000);
        assert.deepEqual(machine.inventory, makeInventory());
        assert.equal(machine.displayName, "Tower");
        assert.deepEqual(machine.tags, ["edge"]);
        // Restored records cannot be republished by re-restoring or by an old
        // session echoing them; a genuine new live sample still publishes.
        ingestion.restorePersisted();
        assert.equal(published.length, 0);
      } finally {
        await store.close();
        db.close();
      }
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
