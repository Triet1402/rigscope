/**
 * Explicit recordings: inclusive backlog, bounded duration, stop/download,
 * deletion, and restart interruption with the output still available.
 */

import { strict as assert } from "node:assert";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";

import { openHubDatabase } from "../src/hub/storage/db";
import { MemoryTelemetryStore } from "../src/hub/storage/memory-store";
import { TelemetryStore } from "../src/hub/storage/telemetry-store";
import { RecordingManager } from "../src/hub/recordings/manager";
import { makeMetricsRecord, makePayload } from "./storage-fixtures";

const BASE = Date.parse("2026-11-01T12:00:00Z");

function tempDir(): string {
  return mkdtempSync(join(tmpdir(), "rigscope-recordings-"));
}

function record(sequence: number, atMs: number) {
  return makeMetricsRecord({
    sequence,
    capturedAt: atMs,
    payload: makePayload({ cpuUsage: 20 + sequence, cores: 2 }),
  });
}

function sample(sequence: number, atMs: number, cpuUsage: number) {
  return makeMetricsRecord({
    sequence,
    capturedAt: atMs,
    payload: makePayload({ cpuUsage, cores: 2 }),
  });
}

function batch(records: ReturnType<typeof makeMetricsRecord>[]) {
  return {
    machineId: "machine-1",
    collectorId: "collector-1",
    mode: "live" as const,
    expectedIntervalMs: 1_000,
    records,
  };
}

type BucketSeries = {
  metric: string;
  resourceId: string;
  min: number | null;
  max: number | null;
  avg: number | null;
};

type BucketLine = {
  type: "bucket";
  machineId: string;
  t: number;
  stepMs: number;
  partial: boolean;
  series: BucketSeries[];
};

function bucketsIn(recordings: RecordingManager, id: string): BucketLine[] {
  const { file } = recordings.read(id, "owner", false);
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line.length > 0)
    .map((line) => JSON.parse(line) as { type: string })
    .filter((line): line is BucketLine => line.type === "bucket");
}

function usageIn(bucket: BucketLine): BucketSeries {
  const series = bucket.series.filter((item) => item.metric === "cpu.usage");
  assert.equal(series.length, 1, `bucket ${bucket.t} has exactly one cpu.usage series`);
  return series[0]!;
}

/** Asserts the recording emitted exactly one bucket per expected start. */
function assertBucketGrid(
  buckets: BucketLine[],
  resolutionMs: number,
  expectedStarts: number[],
): void {
  assert.deepEqual(
    buckets.map((bucket) => bucket.t),
    expectedStarts,
    "bucket starts are unique, ordered, and complete",
  );
  for (const bucket of buckets) {
    assert.equal(bucket.stepMs, resolutionMs, `bucket ${bucket.t} uses the selected resolution`);
    assert.equal(bucket.t % resolutionMs, 0, `bucket ${bucket.t} is on the resolution grid`);
  }
}

function assertValues(
  bucket: BucketLine,
  expected: { min: number | null; max: number | null; avg: number | null },
): void {
  const actual = usageIn(bucket);
  assert.deepEqual(
    { min: actual.min, max: actual.max, avg: actual.avg },
    expected,
    `bucket ${bucket.t} aggregates every sample in its window`,
  );
}

async function ingest(store: TelemetryStore, value: ReturnType<typeof batch>) {
  const pending = store.enqueue(value);
  await store.flushNow();
  return pending;
}

function summaryStore(database: ReturnType<typeof openHubDatabase>, now: () => number) {
  return new TelemetryStore({
    database,
    now,
    storageTier: "summary",
    flushIntervalMs: 60_000,
    maxRecordsPerTransaction: 8_192,
    retention: {
      rawMs: 0,
      tenSecondMs: 3_600_000,
      oneMinuteMs: 86_400_000,
      oneHourMs: 604_800_000,
      receiptsMs: 7 * 86_400_000,
      gapsMs: 604_800_000,
    },
  });
}

describe("recordings", () => {
  it("captures recent RAM history, a live tail, and a downloadable file", async () => {
    const dir = tempDir();
    let now = BASE + 20_000;
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = new MemoryTelemetryStore({ database: db, now: () => now });
    const recordings = new RecordingManager({
      database: db,
      history: store,
      directory: join(dir, "recordings"),
      maxDurationMs: 60_000,
      maxBytes: 1024 * 1024,
      backlogMs: 120_000,
      maxActive: 2,
      now: () => now,
    });
    try {
      await store.enqueue(batch(Array.from({ length: 15 }, (_, index) => record(index + 1, BASE + index * 1_000))));
      const started = recordings.start("owner", {
        machines: ["machine-1"],
        metrics: ["cpu.usage"],
        resolution: "10s",
        durationMs: 60_000,
        includeBacklog: true,
      });
      assert.equal(started.status, "recording");
      assert.equal(started.bytesWritten > 0, true, "backlog is written immediately");

      now += 20_000;
      await store.enqueue(batch([
        record(16, now - 2_000),
        record(17, now - 1_000),
      ]));
      recordings.pump();
      const stopped = recordings.stop(started.id, "owner", false);
      assert.equal(stopped.status, "stopped");
      assert.equal(stopped.reason, "user-stopped");

      const file = recordings.read(started.id, "owner", false);
      const lines = readFileSync(file.file, "utf8").trim().split("\n").map((line) => JSON.parse(line) as { type: string });
      assert.equal(lines[0]!.type, "manifest");
      assert.ok(lines.some((line) => line.type === "bucket"));
      assert.equal(lines.at(-1)!.type, "end");
      assert.equal(Math.round(file.size), stopped.bytesWritten);

      // Another account cannot download or delete it.
      assert.throws(() => recordings.read(started.id, "other", false), /another account/);
      recordings.remove(started.id, "owner", false);
      assert.equal(existsSync(file.file), false);
      assert.equal(recordings.list("owner", false).length, 0);
    } finally {
      recordings.close();
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("stops at its duration bound and remains downloadable after a restart", async () => {
    const dir = tempDir();
    let now = BASE + 10_000;
    let db = openHubDatabase(join(dir, "hub.db"));
    let store = new MemoryTelemetryStore({ database: db, now: () => now });
    let recordings = new RecordingManager({
      database: db,
      history: store,
      directory: join(dir, "recordings"),
      maxDurationMs: 60_000,
      maxBytes: 1024 * 1024,
      backlogMs: 0,
      maxActive: 2,
      now: () => now,
    });
    let id = "";
    try {
      await store.enqueue(batch([record(1, BASE)]));
      id = recordings.start("owner", {
        machines: ["machine-1"],
        metrics: ["cpu.usage"],
        resolution: "10s",
        durationMs: 1_000,
      }).id;
      now += 2_000;
      recordings.pump();
      const completed = recordings.get(id, "owner", false);
      assert.equal(completed.status, "completed");
      assert.equal(completed.reason, "duration-limit");

      // Simulate a process restart: close marks an active capture interrupted,
      // and the file/metadata from the previous run survive.
      const second = recordings.start("owner", {
        machines: ["machine-1"],
        metrics: ["cpu.usage"],
        resolution: "10s",
        durationMs: 60_000,
      });
      recordings.close();
      await store.close();
      db = openHubDatabase(join(dir, "hub.db"));
      store = new MemoryTelemetryStore({ database: db, now: () => now });
      recordings = new RecordingManager({
        database: db,
        history: store,
        directory: join(dir, "recordings"),
        maxDurationMs: 60_000,
        maxBytes: 1024 * 1024,
        backlogMs: 0,
        maxActive: 2,
        now: () => now,
      });
      const after = recordings.get(second.id, "owner", false);
      assert.equal(after.status, "interrupted");
      assert.ok(after.reason === "hub-restart" || after.reason === "hub-shutdown");
      const file = recordings.read(second.id, "owner", false);
      assert.ok(file.size > 0);
    } finally {
      recordings.close();
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("records the selected ten-second resolution across pumps without dropping the start bucket", async () => {
    const dir = tempDir();
    let now = BASE + 5_000;
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = new MemoryTelemetryStore({ database: db, now: () => now });
    const recordings = new RecordingManager({
      database: db,
      history: store,
      directory: join(dir, "recordings"),
      maxDurationMs: 120_000,
      maxBytes: 1024 * 1024,
      backlogMs: 0,
      maxActive: 2,
      now: () => now,
    });
    try {
      // The recording starts halfway through the BASE..BASE+10s bucket. That
      // first, partially overlapped bucket must still be emitted at full width.
      await store.enqueue(batch([
        sample(1, BASE + 5_000, 10),
        sample(2, BASE + 6_000, 20),
        sample(3, BASE + 8_000, 30),
      ]));
      const started = recordings.start("owner", {
        machines: ["machine-1"],
        metrics: ["cpu.usage"],
        resolution: "10s",
        durationMs: 60_000,
      });

      now = BASE + 10_000;
      recordings.pump();
      now = BASE + 20_000;
      await store.enqueue(batch([
        sample(4, BASE + 11_000, 40),
        sample(5, BASE + 19_000, 100),
      ]));
      recordings.pump();
      now = BASE + 25_000;
      await store.enqueue(batch([sample(6, BASE + 21_000, 50)]));
      assert.equal(recordings.stop(started.id, "owner", false).status, "stopped");

      const buckets = bucketsIn(recordings, started.id);
      assertBucketGrid(buckets, 10_000, [BASE, BASE + 10_000, BASE + 20_000]);
      assertValues(buckets[0]!, { min: 10, max: 30, avg: 20 });
      assertValues(buckets[1]!, { min: 40, max: 100, avg: 70 });
      assertValues(buckets[2]!, { min: 50, max: 50, avg: 50 });
      assert.equal(buckets[0]!.partial, false);
      assert.equal(buckets[1]!.partial, false);
      assert.equal(buckets[2]!.partial, true, "the stop captures its final partial bucket once");
    } finally {
      recordings.close();
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("keeps one-hour buckets and merges weighted averages across pumps", async () => {
    const dir = tempDir();
    let now = BASE;
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = new MemoryTelemetryStore({ database: db, now: () => now });
    const recordings = new RecordingManager({
      database: db,
      history: store,
      directory: join(dir, "recordings"),
      maxDurationMs: 4 * 3_600_000,
      maxBytes: 4 * 1024 * 1024,
      backlogMs: 0,
      maxActive: 2,
      now: () => now,
    });
    try {
      const started = recordings.start("owner", {
        machines: ["machine-1"],
        metrics: ["cpu.usage"],
        resolution: "1h",
        durationMs: 3 * 3_600_000,
      });

      // Sixty 10% samples in one minute, then one 100% sample in another
      // minute of the same hour: the hour average is count-weighted, not an
      // average of the two minute averages.
      now = BASE + 30 * 60_000;
      await store.enqueue(batch(Array.from({ length: 60 }, (_, index) =>
        sample(index + 1, BASE + 5 * 60_000 + index * 1_000, 10))));
      recordings.pump();
      now = BASE + 50 * 60_000;
      await store.enqueue(batch([sample(61, BASE + 50 * 60_000, 100)]));
      recordings.pump();

      now = BASE + 61 * 60_000;
      recordings.pump();
      now = BASE + 125 * 60_000;
      await store.enqueue(batch([
        sample(62, BASE + 65 * 60_000, 100),
        sample(63, BASE + 110 * 60_000, 200),
      ]));
      recordings.pump();
      now = BASE + 126 * 60_000;
      assert.equal(recordings.stop(started.id, "owner", false).status, "stopped");

      const buckets = bucketsIn(recordings, started.id);
      assertBucketGrid(buckets, 3_600_000, [BASE, BASE + 3_600_000]);
      assertValues(buckets[0]!, { min: 10, max: 100, avg: 700 / 61 });
      assertValues(buckets[1]!, { min: 100, max: 200, avg: 150 });
    } finally {
      recordings.close();
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("includes the persistent summary bucket that overlaps the recording start", async () => {
    const dir = tempDir();
    let now = BASE + 5_000;
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = summaryStore(db, () => now);
    const recordings = new RecordingManager({
      database: db,
      history: store,
      directory: join(dir, "recordings"),
      maxDurationMs: 120_000,
      maxBytes: 1024 * 1024,
      backlogMs: 120_000,
      maxActive: 2,
      now: () => now,
    });
    try {
      await ingest(store, batch([
        sample(1, BASE + 1_000, 10),
        sample(2, BASE + 3_000, 30),
      ]));
      const started = recordings.start("owner", {
        machines: ["machine-1"],
        metrics: ["cpu.usage"],
        resolution: "10s",
        durationMs: 60_000,
        includeBacklog: true,
      });

      now = BASE + 10_000;
      await ingest(store, batch([
        sample(3, BASE + 6_000, 20),
        sample(4, BASE + 8_000, 40),
      ]));
      recordings.pump();
      now = BASE + 20_000;
      await ingest(store, batch([
        sample(5, BASE + 11_000, 50),
        sample(6, BASE + 19_000, 60),
      ]));
      recordings.pump();
      now = BASE + 25_000;
      await ingest(store, batch([sample(7, BASE + 21_000, 70)]));
      assert.equal(recordings.stop(started.id, "owner", false).status, "stopped");

      const buckets = bucketsIn(recordings, started.id);
      assertBucketGrid(buckets, 10_000, [BASE, BASE + 10_000, BASE + 20_000]);
      // The stored summary bucket starts before the recording did, but the
      // lead-up is enabled, so its full aggregate is written exactly once.
      assertValues(buckets[0]!, { min: 10, max: 40, avg: 25 });
      assertValues(buckets[1]!, { min: 50, max: 60, avg: 55 });
      assertValues(buckets[2]!, { min: 70, max: 70, avg: 70 });
      assert.equal(buckets[2]!.partial, true);
    } finally {
      recordings.close();
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("does not double-count backlog, pumps, or the stop partial", async () => {
    const dir = tempDir();
    let now = BASE + 5_000;
    const db = openHubDatabase(join(dir, "hub.db"));
    const store = new MemoryTelemetryStore({ database: db, now: () => now });
    const recordings = new RecordingManager({
      database: db,
      history: store,
      directory: join(dir, "recordings"),
      maxDurationMs: 120_000,
      maxBytes: 1024 * 1024,
      backlogMs: 120_000,
      maxActive: 2,
      now: () => now,
    });
    try {
      await store.enqueue(batch([
        sample(1, BASE - 20_000, 10),
        sample(2, BASE - 10_000, 30),
        sample(3, BASE + 5_000, 20),
        sample(4, BASE + 6_000, 40),
      ]));
      const started = recordings.start("owner", {
        machines: ["machine-1"],
        metrics: ["cpu.usage"],
        resolution: "10s",
        durationMs: 60_000,
        includeBacklog: true,
      });

      now = BASE + 10_000;
      recordings.pump();
      now = BASE + 20_000;
      await store.enqueue(batch([
        sample(5, BASE + 11_000, 50),
        sample(6, BASE + 19_000, 60),
      ]));
      recordings.pump();
      now = BASE + 25_000;
      await store.enqueue(batch([sample(7, BASE + 21_000, 70)]));
      assert.equal(recordings.stop(started.id, "owner", false).status, "stopped");

      const buckets = bucketsIn(recordings, started.id);
      // Every ten-second window appears once, including the two backlog
      // windows, the live windows, and the stop partial. Duplicates would
      // change the arrays below.
      assertBucketGrid(buckets, 10_000, [
        BASE - 20_000,
        BASE - 10_000,
        BASE,
        BASE + 10_000,
        BASE + 20_000,
      ]);
      assertValues(buckets[0]!, { min: 10, max: 10, avg: 10 });
      assertValues(buckets[1]!, { min: 30, max: 30, avg: 30 });
      assertValues(buckets[2]!, { min: 20, max: 40, avg: 30 });
      assertValues(buckets[3]!, { min: 50, max: 60, avg: 55 });
      assertValues(buckets[4]!, { min: 70, max: 70, avg: 70 });
      assert.equal(buckets[4]!.partial, true);
    } finally {
      recordings.close();
      await store.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
