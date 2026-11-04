/**
 * Alert schema, appended by the hub migration ledger as a later migration.
 *
 * `installAlertSchema` is idempotent and only uses synchronous SQLite APIs so
 * the parent can call it from a migration transaction. It owns no migration
 * row itself.
 */
import type { DatabaseSync } from "node:sqlite";

export function installAlertSchema(db: DatabaseSync): void {
  db.exec(`
    -- Rules are durable. Per-rule pending/incident state lives here so a
    -- restart can continue a duration or avoid opening a duplicate incident.
    CREATE TABLE IF NOT EXISTS alert_rules (
      id TEXT PRIMARY KEY,
      name TEXT NOT NULL,
      machine_id TEXT NOT NULL,
      metric TEXT NOT NULL,
      resource_id TEXT NOT NULL DEFAULT '',
      condition TEXT NOT NULL,
      threshold REAL,
      duration_ms INTEGER NOT NULL DEFAULT 0,
      enabled INTEGER NOT NULL DEFAULT 1,
      maintenance_until_ms INTEGER,
      state TEXT NOT NULL DEFAULT 'ok',
      pending_since_ms INTEGER,
      active_incident_id TEXT,
      last_value REAL,
      last_observed_at_ms INTEGER,
      created_at_ms INTEGER NOT NULL,
      updated_at_ms INTEGER NOT NULL
    );

    CREATE INDEX IF NOT EXISTS alert_rules_machine
      ON alert_rules (machine_id);

    -- Incident history is bounded by the engine, but never loses an open row.
    CREATE TABLE IF NOT EXISTS alert_incidents (
      id TEXT PRIMARY KEY,
      rule_id TEXT NOT NULL,
      rule_name TEXT NOT NULL,
      machine_id TEXT NOT NULL,
      metric TEXT NOT NULL,
      resource_id TEXT NOT NULL DEFAULT '',
      condition TEXT NOT NULL,
      status TEXT NOT NULL,
      started_at_ms INTEGER NOT NULL,
      resolved_at_ms INTEGER,
      started_value REAL,
      resolved_value REAL,
      last_observed_at_ms INTEGER NOT NULL,
      message TEXT NOT NULL
    );

    CREATE INDEX IF NOT EXISTS alert_incidents_started
      ON alert_incidents (started_at_ms DESC);
    CREATE INDEX IF NOT EXISTS alert_incidents_machine_started
      ON alert_incidents (machine_id, started_at_ms DESC);
    CREATE INDEX IF NOT EXISTS alert_incidents_rule_status
      ON alert_incidents (rule_id, status);
    CREATE UNIQUE INDEX IF NOT EXISTS alert_incidents_one_open
      ON alert_incidents (rule_id) WHERE status = 'open';

    -- One durable job per logical incident event. The primary key is the
    -- deterministic idempotency key sent to the webhook receiver.
    CREATE TABLE IF NOT EXISTS alert_jobs (
      id TEXT PRIMARY KEY,
      rule_id TEXT NOT NULL,
      incident_id TEXT NOT NULL,
      machine_id TEXT NOT NULL,
      event TEXT NOT NULL,
      status TEXT NOT NULL,
      attempts INTEGER NOT NULL DEFAULT 0,
      max_attempts INTEGER NOT NULL DEFAULT 6,
      next_attempt_ms INTEGER,
      last_attempt_ms INTEGER,
      last_error TEXT,
      created_at_ms INTEGER NOT NULL,
      delivered_at_ms INTEGER,
      payload_json TEXT NOT NULL,
      UNIQUE (incident_id, event)
    );

    CREATE INDEX IF NOT EXISTS alert_jobs_due
      ON alert_jobs (status, next_attempt_ms);
    CREATE INDEX IF NOT EXISTS alert_jobs_created
      ON alert_jobs (created_at_ms DESC);

    -- Exactly one webhook channel. URL, headers and optional HMAC secret are
    -- private database state and are never returned by the browser API.
    CREATE TABLE IF NOT EXISTS alert_webhook (
      id INTEGER PRIMARY KEY CHECK (id = 1),
      url TEXT NOT NULL,
      headers_json TEXT NOT NULL DEFAULT '{}',
      secret TEXT,
      enabled INTEGER NOT NULL DEFAULT 1,
      updated_at_ms INTEGER NOT NULL,
      last_test_at_ms INTEGER,
      last_test_ok INTEGER,
      last_error TEXT
    );

    -- Last LIVE record accepted by the engine. Used to ignore duplicate or
    -- older records after restart; replay never calls observe.
    CREATE TABLE IF NOT EXISTS alert_machine_progress (
      machine_id TEXT PRIMARY KEY,
      collector_id TEXT NOT NULL,
      run_id TEXT NOT NULL,
      seq INTEGER NOT NULL,
      received_at_ms INTEGER NOT NULL,
      updated_at_ms INTEGER NOT NULL
    );

    -- Resources this rule has actually observed. A resource missing from an
    -- otherwise-ok family after being seen is unavailable, not a silent
    -- recovery to "normal".
    CREATE TABLE IF NOT EXISTS alert_observed_resources (
      rule_id TEXT NOT NULL,
      metric TEXT NOT NULL,
      resource_id TEXT NOT NULL,
      first_seen_ms INTEGER NOT NULL,
      last_seen_ms INTEGER NOT NULL,
      PRIMARY KEY (rule_id, metric, resource_id)
    );
  `);
}
