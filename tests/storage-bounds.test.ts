/**
 * Physical hub-storage bound tests. They use small isolated databases, measure
 * real main+WAL bytes, and verify that a full cap produces busy (never a
 * durable ack), leaves recent history intact, and is shared with non-telemetry
 * writes on the same SQLite connection.
 */
import { strict as assert } from "node:assert";
import {
  chmodSync,
  linkSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  symlinkSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { describe, it } from "node:test";

import {
  isStorageFullError,
  openHubDatabase,
  type HubDatabase,
} from "../src/hub/storage/db";
import { RetentionManager } from "../src/hub/storage/retention";
import {
  DEFAULT_MAX_DATABASE_DISK_BYTES,
  TelemetryStore,
  type IngestBatch,
  type IngestResult,
} from "../src/hub/storage/telemetry-store";
import { makeMetricsRecord, makePayload } from "./storage-fixtures";

const NOW = Date.parse("2026-11-02T12:00:00Z");
const HOUR = 3_600_000;

function tempDir(): string {
  return mkdtempSync(join(tmpdir(), "rigscope-storage-bounds-"));
}

function largePayload() {
  return makePayload({
    cores: 32,
    gpus: Array.from({ length: 8 }, (_, index) => ({
      id: `GPU-${index}`,
      utilization: index * 10,
      memoryUsed: 1_000,
      memoryTotal: 2_000,
    })),
    filesystems: Array.from({ length: 16 }, (_, index) => ({
      id: `fs-${index}`,
      used: 1,
      total: 2,
    })),
    disks: Array.from({ length: 4 }, (_, index) => ({ id: `disk-${index}` })),
    interfaces: Array.from({ length: 8 }, (_, index) => ({ id: `eth-${index}` })),
  });
}

function batchOf(records: IngestBatch["records"]): IngestBatch {
  return {
    machineId: "machine-1",
    collectorId: "collector-1",
    mode: "live",
    expectedIntervalMs: 2_000,
    records,
  };
}

async function ingest(store: TelemetryStore, batch: IngestBatch): Promise<IngestResult> {
  const pending = store.enqueue(batch);
  await store.flushNow();
  return pending;
}

function scalar(db: HubDatabase, sql: string, ...params: Array<string | number | null>): number {
  const row = db.db.prepare(sql).get(...params) as Record<string, unknown> | undefined;
  assert.ok(row);
  return Number(Object.values(row)[0]);
}

type StoreContext = { dir: string; db: HubDatabase; store: TelemetryStore; cap: number };

function setupStore(cap: (baseline: number) => number, options: { now?: () => number } = {}): StoreContext {
  const dir = tempDir();
  const db = openHubDatabase(join(dir, "hub.db"));
  const baseline = db.sizeBytes();
  const capBytes = cap(baseline);
  const store = new TelemetryStore({
    database: db,
    flushIntervalMs: 60_000,
    maxRecordsPerTransaction: 4,
    maxPhysicalBytes: capBytes,
    now: options.now ?? (() => NOW),
    retention: {
      rawMs: 100 * HOUR,
      oneMinuteMs: 100 * HOUR,
      oneHourMs: 100 * HOUR,
      receiptsMs: 100 * HOUR,
      gapsMs: 100 * HOUR,
    },
  });
  return { dir, db, store, cap: capBytes };
}

async function teardown(context: StoreContext): Promise<void> {
  await context.store.close();
  rmSync(context.dir, { recursive: true, force: true });
}

describe("hub storage bounds", () => {
  it("uses a finite default physical cap and derives it from the production soft budget", async () => {
    const firstDir = tempDir();
    const first = openHubDatabase(join(firstDir, "hub.db"));
    const derived = new TelemetryStore({
      database: first,
      flushIntervalMs: 60_000,
      maxDatabaseBytes: 16 * 1024 * 1024,
    });
    try {
      assert.equal(derived.stats().dbBytesLimit, 16 * 1024 * 1024);
      assert.equal(first.physicalLimit()?.limitBytes, 16 * 1024 * 1024);
    } finally {
      await derived.close();
      rmSync(firstDir, { recursive: true, force: true });
    }

    const secondDir = tempDir();
    const second = openHubDatabase(join(secondDir, "hub.db"));
    const fallback = new TelemetryStore({ database: second, flushIntervalMs: 60_000 });
    try {
      assert.equal(fallback.stats().dbBytesLimit, DEFAULT_MAX_DATABASE_DISK_BYTES);
      assert.equal(DEFAULT_MAX_DATABASE_DISK_BYTES, 2 * 1024 * 1024 * 1024);
    } finally {
      await fallback.close();
      rmSync(secondDir, { recursive: true, force: true });
    }
  });

  it("refuses backup aliases and keeps the previous backup until a new one succeeds", async (t) => {
    const dir = tempDir();
    const dbPath = join(dir, "hub.db");
    const db = openHubDatabase(dbPath);
    try {
      db.db
        .prepare("INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, ?, ?, ?)")
        .run("machine-1", "Workstation", "[]", 1);

      mkdirSync(join(dir, "nested"));
      const syntacticAlias = join(dir, "nested", "..", "hub.db");
      await assert.rejects(db.backupTo(syntacticAlias), /aliases the live database/);

      const hardLink = join(dir, "hard-link.db");
      linkSync(dbPath, hardLink);
      const sourceInode = statSync(dbPath).ino;
      assert.equal(statSync(hardLink).ino, sourceInode);
      await assert.rejects(db.backupTo(hardLink), /aliases the live database/);
      assert.equal(statSync(hardLink).ino, sourceInode, "the hard link is still a live alias");

      const symlink = join(dir, "symlink.db");
      symlinkSync(dbPath, symlink);
      await assert.rejects(db.backupTo(symlink), /regular file/);

      const marker = db.db
        .prepare("SELECT display_name FROM machine_meta WHERE machine_id = ?")
        .get("machine-1") as { display_name: string };
      assert.equal(marker.display_name, "Workstation");

      const goodBackup = join(dir, "good-backup.db");
      await db.backupTo(goodBackup);
      const before = readFileSync(goodBackup);
      assert.ok(before.length > 0);

      if (process.getuid?.() === 0) {
        t.skip("the read-only-directory failure simulation does not apply to root");
        return;
      }
      chmodSync(dir, 0o500);
      try {
        await assert.rejects(db.backupTo(goodBackup));
      } finally {
        chmodSync(dir, 0o700);
      }
      assert.deepEqual(readFileSync(goodBackup), before, "the old good backup survives a failed replacement");
    } finally {
      db.close();
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("bounds measured main+WAL bytes, rejects with busy, and shares the quota with admin writes", async () => {
    const context = setupStore((baseline) => baseline + 512 * 1024);
    try {
      const payload = largePayload();
      let committed = 0;
      let busy = 0;
      for (let index = 0; index < 400; index += 1) {
        const result = await ingest(
          context.store,
          batchOf([
            makeMetricsRecord({
              sequence: index + 1,
              capturedAt: NOW + index * 1_000,
              payload,
            }),
          ]),
        );
        if (result.status === "committed") committed += 1;
        else busy += 1;
        assert.ok(context.db.sizeBytes() <= context.cap, "each completed batch stays within the measured budget");
      }

      assert.ok(committed > 20, `expected meaningful commits, got ${committed}`);
      assert.ok(busy > 0, "the hard cap must eventually reject");
      const stats = context.store.stats();
      assert.ok(stats.dbBytes <= context.cap, `${stats.dbBytes} > ${context.cap}`);
      assert.equal(stats.dbBytesLimit, context.cap);
      assert.ok(stats.diskFullFailures > 0);
      assert.match(stats.lastWriteError ?? "", /disk budget|full/i);
      assert.ok(stats.dbWalBytes <= (context.db.physicalLimit()?.walBudgetBytes ?? 0));
      assert.equal(scalar(context.db, "SELECT COUNT(*) FROM sample_receipts"), committed);

      const adminBefore = scalar(context.db, "SELECT COUNT(*) FROM admin_meta");
      assert.throws(
        () =>
          context.db.db
            .prepare("INSERT INTO admin_meta (key, value) VALUES (?, ?)")
            .run("full-probe", "x".repeat(512 * 1024)),
        (error: unknown) => isStorageFullError(error),
        "registry/admin writes share the same finite page cap",
      );
      assert.equal(scalar(context.db, "SELECT COUNT(*) FROM admin_meta"), adminBefore);
    } finally {
      await teardown(context);
    }
  });

  it("keeps the last hour under pressure and rejects writes at the hard cap", async () => {
    const dir = tempDir();
    const db = openHubDatabase(join(dir, "hub.db"));
    try {
      const series = Number(
        db.db
          .prepare("INSERT INTO series (machine_id, metric, resource_id) VALUES (?, ?, '')")
          .run("machine-1", "cpu.usage").lastInsertRowid,
      );
      const insertPoint = db.db.prepare("INSERT INTO points_raw (series_id, ts_ms, value) VALUES (?, ?, 1)");
      insertPoint.run(series, NOW - 2 * HOUR);
      insertPoint.run(series, NOW - 0.5 * HOUR);
      db.checkpoint();

      const size = db.sizeBytes();
      const cap = Math.max(64 * 1024, size - 128 * 1024);
      const manager = new RetentionManager(db, {
        maxPhysicalBytes: cap,
        now: () => NOW,
        chunkSize: 10,
        policy: {
          rawMs: 100 * HOUR,
          oneMinuteMs: 100 * HOUR,
          oneHourMs: 100 * HOUR,
          receiptsMs: 100 * HOUR,
          gapsMs: 100 * HOUR,
        },
      });
      assert.equal(manager.status().physicalPressure, true);
      assert.equal(scalar(db, "SELECT COUNT(*) FROM points_raw"), 2);

      manager.relievePressure(4);
      assert.equal(scalar(db, "SELECT COUNT(*) FROM points_raw WHERE ts_ms = ?", NOW - 2 * HOUR), 0);
      assert.equal(scalar(db, "SELECT COUNT(*) FROM points_raw WHERE ts_ms = ?", NOW - 0.5 * HOUR), 1);
      assert.equal(manager.status().pressure, true);

      const store = new TelemetryStore({
        database: db,
        flushIntervalMs: 60_000,
        maxPhysicalBytes: cap,
        now: () => NOW,
        retention: {
          rawMs: 100 * HOUR,
          oneMinuteMs: 100 * HOUR,
          oneHourMs: 100 * HOUR,
          receiptsMs: 100 * HOUR,
          gapsMs: 100 * HOUR,
        },
      });
      try {
        const result = await ingest(
          store,
          batchOf([makeMetricsRecord({ sequence: 1, capturedAt: NOW, payload: largePayload() })]),
        );
        assert.equal(result.status, "busy");
        if (result.status === "busy") assert.match(result.reason, /disk budget|storage/i);
        assert.equal(scalar(db, "SELECT COUNT(*) FROM points_raw WHERE ts_ms = ?", NOW - 0.5 * HOUR), 1);
      } finally {
        await store.close();
      }
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("keeps WAL bounded across sustained commits", async () => {
    const context = setupStore((baseline) => baseline + 2 * 1024 * 1024);
    try {
      const payload = largePayload();
      for (let index = 0; index < 200; index += 1) {
        const result = await ingest(
          context.store,
          batchOf([
            makeMetricsRecord({
              sequence: index + 1,
              capturedAt: NOW + index * 1_000,
              payload,
            }),
          ]),
        );
        assert.equal(result.status, "committed");
      }
      const physical = context.db.physicalLimit();
      assert.ok(physical);
      const stats = context.store.stats();
      assert.ok(stats.dbBytes <= context.cap);
      assert.ok(stats.dbWalBytes <= physical!.walBudgetBytes, `${stats.dbWalBytes} > ${physical!.walBudgetBytes}`);
    } finally {
      await teardown(context);
    }
  });
});
