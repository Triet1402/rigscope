/**
 * Durable hub database migrations.
 *
 * Every later wave of work (alerts, users, credentials, ...) APPENDS a
 * migration here. These numbers order irreversible SQL changes; they are not
 * configuration or installation state versions, which are unnumbered and
 * canonical elsewhere. Versions are contiguous starting at 1 and are recorded
 * in `schema_migrations` by `openHubDatabase` after the migration itself has
 * committed. A migration must only use synchronous SQLite APIs: each one runs
 * inside a single `BEGIN IMMEDIATE ... COMMIT` so a crash leaves either none or
 * all of its changes.
 *
 * Migration 1 is the telemetry schema. Design notes:
 * - Each metric reading is a (machine, metric, resource) *series*. The unique
 *   key lets ingestion cache series ids and keeps the history query path on
 *   narrow integer keys.
 * - Raw points keep a NULL value for an explicit "unavailable" reading. NULL
 *   is never coerced to zero, so a chart can render an honest unknown state.
 * - Rollups are updated incrementally at insert time; they preserve min/max,
 *   which an average-only rollup would destroy. `count` counts non-NULL
 *   readings, `null_count` counts explicit unavailable readings.
 * - `sample_receipts` is the authoritative dedup key `(collector, run, seq)`.
 *   Points/rollups/gaps are only written when a receipt row was inserted, so
 *   at-least-once delivery (including a lost ack after commit) is idempotent.
 * - `machine_latest` survives restart so the UI can show last-known values as
 *   stale, never as fresh.
 */

import type { DatabaseSync } from "node:sqlite";
import { adminSchema } from '../admin/store';
import { installAlertSchema } from '../alerts/schema';
import { installRegistrySchema } from '../enrollment/registry';

export type Migration = {
  version: number;
  name: string;
  up: (db: DatabaseSync) => void;
};

export const MIGRATIONS: Migration[] = [
  {
    version: 1,
    name: "telemetry",
    up(db) {
      db.exec(`
        -- Hub-local machine presentation metadata; never required for ingest.
        CREATE TABLE machine_meta (
          machine_id TEXT PRIMARY KEY,
          display_name TEXT,
          tags_json TEXT NOT NULL DEFAULT '[]',
          updated_at_ms INTEGER NOT NULL
        );

        -- Integer series catalog. resource_id is '' for single-resource
        -- families and e.g. "core:3", a GPU/filesystem/disk/interface id for
        -- per-resource families.
        CREATE TABLE series (
          series_id INTEGER PRIMARY KEY,
          machine_id TEXT NOT NULL,
          metric TEXT NOT NULL,
          resource_id TEXT NOT NULL DEFAULT '',
          UNIQUE (machine_id, metric, resource_id)
        );

        -- Raw samples. WITHOUT ROWID makes (series_id, ts_ms) the clustered
        -- primary key, so a history range read is one index range scan.
        -- A NULL value is an explicit unavailable reading, never zero.
        CREATE TABLE points_raw (
          series_id INTEGER NOT NULL,
          ts_ms INTEGER NOT NULL,
          value REAL,
          PRIMARY KEY (series_id, ts_ms)
        ) WITHOUT ROWID;

        -- One-minute rollups, maintained on insert (including late/replayed
        -- data): min/max preserved, count of non-NULL values, null_count of
        -- explicit unavailable readings. sum is 0 for a null-only bucket and
        -- is only interpreted together with count.
        CREATE TABLE points_1m (
          series_id INTEGER NOT NULL,
          bucket_ms INTEGER NOT NULL,
          min REAL,
          max REAL,
          sum REAL NOT NULL DEFAULT 0,
          count INTEGER NOT NULL DEFAULT 0,
          null_count INTEGER NOT NULL DEFAULT 0,
          PRIMARY KEY (series_id, bucket_ms)
        ) WITHOUT ROWID;

        CREATE TABLE points_1h (
          series_id INTEGER NOT NULL,
          bucket_ms INTEGER NOT NULL,
          min REAL,
          max REAL,
          sum REAL NOT NULL DEFAULT 0,
          count INTEGER NOT NULL DEFAULT 0,
          null_count INTEGER NOT NULL DEFAULT 0,
          PRIMARY KEY (series_id, bucket_ms)
        ) WITHOUT ROWID;

        -- Durable receipt / dedup key. Every accepted record kind has a row
        -- here before its side effects are committed.
        CREATE TABLE sample_receipts (
          collector_id TEXT NOT NULL,
          run_id TEXT NOT NULL,
          seq INTEGER NOT NULL,
          machine_id TEXT NOT NULL,
          kind TEXT NOT NULL,
          mode TEXT NOT NULL,
          captured_at_ms INTEGER NOT NULL,
          received_at_ms INTEGER NOT NULL,
          expected_interval_ms INTEGER,
          PRIMARY KEY (collector_id, run_id, seq)
        ) WITHOUT ROWID;

        -- Freshness and gap detection scan a machine's receipt timeline.
        CREATE INDEX sample_receipts_machine_captured
          ON sample_receipts (machine_id, captured_at_ms);
        -- Retention deletes oldest receipts without an unbounded scan.
        CREATE INDEX sample_receipts_captured
          ON sample_receipts (captured_at_ms);

        -- Collector-reported dropped intervals.
        CREATE TABLE gaps (
          collector_id TEXT NOT NULL,
          run_id TEXT NOT NULL,
          seq INTEGER NOT NULL,
          machine_id TEXT NOT NULL,
          from_ms INTEGER NOT NULL,
          to_ms INTEGER NOT NULL,
          dropped INTEGER NOT NULL,
          reason TEXT NOT NULL,
          received_at_ms INTEGER NOT NULL,
          PRIMARY KEY (collector_id, run_id, seq)
        ) WITHOUT ROWID;

        CREATE INDEX gaps_machine_to ON gaps (machine_id, to_ms);
        -- Retention deletes oldest gaps without a full scan.
        CREATE INDEX gaps_to ON gaps (to_ms);

        -- Latest LIVE metrics per machine, persisted across restart. A replay
        -- batch never touches this table.
        CREATE TABLE machine_latest (
          machine_id TEXT PRIMARY KEY,
          collector_id TEXT NOT NULL,
          run_id TEXT NOT NULL,
          seq INTEGER NOT NULL,
          captured_at_ms INTEGER NOT NULL,
          received_at_ms INTEGER NOT NULL,
          payload_json TEXT NOT NULL
        );

        -- Latest inventory per machine. Inventory is not time-series data.
        CREATE TABLE inventory (
          machine_id TEXT PRIMARY KEY,
          captured_at_ms INTEGER NOT NULL,
          received_at_ms INTEGER NOT NULL,
          inventory_json TEXT NOT NULL
        );
      `);
    },
  },
  {
    version: 2,
    name: "latest_session_identity",
    up(db) {
      // Append-only: existing version-1 databases gain the session identity
      // needed to restore latest LIVE state without a new receipt time. The
      // pragma guard also makes this safe if a pre-release development build
      // already added the columns to its version-1 schema.
      const columns = new Set(
        (db.prepare("PRAGMA table_info(machine_latest)").all() as Array<{ name: string }>).map(
          (column) => column.name,
        ),
      );
      if (!columns.has("boot_id")) {
        db.exec("ALTER TABLE machine_latest ADD COLUMN boot_id TEXT NOT NULL DEFAULT ''");
      }
      if (!columns.has("expected_interval_ms")) {
        db.exec("ALTER TABLE machine_latest ADD COLUMN expected_interval_ms INTEGER");
      }
      if (!columns.has("hostname")) {
        db.exec("ALTER TABLE machine_latest ADD COLUMN hostname TEXT");
      }
    },
  },
  { version: 3, name: 'admin', up: adminSchema },
  { version: 4, name: 'alerts', up: installAlertSchema },
  { version: 5, name: 'collector_registry', up: installRegistrySchema },
  { version: 6, name: 'telemetry_modes_and_recordings', up: telemetryModesAndRecordings },
];

/**
 * Summary-mode telemetry and explicit recordings.
 *
 * `points_10s` is a compact immutable summary tier used instead of one raw row
 * per collector sample when persistent history is enabled. It keeps the same
 * min/max/sum/count/null_count contract as the existing rollups, so spikes and
 * unavailable readings survive.
 *
 * Recording metadata is control state (survives restart); the captured data
 * itself is a private append-only JSON-lines file owned by RecordingManager.
 */
function telemetryModesAndRecordings(db: DatabaseSync): void {
  db.exec(`
    CREATE TABLE IF NOT EXISTS points_10s (
      series_id INTEGER NOT NULL,
      bucket_ms INTEGER NOT NULL,
      min REAL,
      max REAL,
      sum REAL NOT NULL DEFAULT 0,
      count INTEGER NOT NULL DEFAULT 0,
      null_count INTEGER NOT NULL DEFAULT 0,
      PRIMARY KEY (series_id, bucket_ms)
    ) WITHOUT ROWID;

    CREATE TABLE IF NOT EXISTS recordings (
      id TEXT PRIMARY KEY,
      created_by TEXT NOT NULL,
      status TEXT NOT NULL,
      machines_json TEXT NOT NULL,
      metrics_json TEXT NOT NULL,
      resolution_ms INTEGER NOT NULL,
      created_at_ms INTEGER NOT NULL,
      started_at_ms INTEGER NOT NULL,
      stopped_at_ms INTEGER,
      deadline_ms INTEGER NOT NULL,
      max_bytes INTEGER NOT NULL,
      bytes_written INTEGER NOT NULL DEFAULT 0,
      file_name TEXT NOT NULL,
      include_backlog INTEGER NOT NULL DEFAULT 0,
      reason TEXT,
      error TEXT
    );

    CREATE INDEX IF NOT EXISTS recordings_created
      ON recordings (created_at_ms DESC);
  `);
}
