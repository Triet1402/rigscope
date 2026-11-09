/**
 * Durable alert evaluation and notification engine.
 *
 * The parent calls `observe` only after a LIVE metrics record committed, so a
 * replay batch can never reopen an old condition. `tick` is the parent's 1 s
 * clock for stale and unavailable rules. Rule state (pending since, open
 * incident id, last value) lives in SQLite, which is what makes a restart
 * continue a duration or avoid a duplicate incident.
 *
 * Notifications are at-least-once: one durable job per (incident, event) with
 * a deterministic idempotency key, bounded attempts and persisted retry state.
 * The engine never logs or returns the configured webhook URL/headers/secret.
 */
import { createHash, randomUUID } from "node:crypto";
import type { DatabaseSync, StatementSync } from "node:sqlite";

import type { MachineState, MetricsPayload, MetricsRecord } from "../../contracts";
import {
  ALERT_CONDITIONS,
  ALERT_DELIVERY_DISCLOSURE,
  ALERT_LIMITS,
  ALERT_METRICS,
  alertMetricFamily,
  alertStaleThresholdMs,
  alertScopeIncludes,
  isAlertMetric,
  type AlertCondition,
  type AlertIncident,
  type AlertJobStatus,
  type AlertListResponse,
  type AlertMachineScope,
  type AlertMetric,
  type AlertMetricUnit,
  type AlertNotification,
  type AlertNotificationSummary,
  type AlertRule,
  type AlertRuleInput,
  type AlertRuleState,
  type AlertWebhookConfigureInput,
  type AlertWebhookStatus,
  type AlertWebhookTestResult,
  type AlertWebhookTransport,
} from "../../alerts/types";
import type { HubDatabase } from "../storage/db";
import { flattenMetrics } from "../storage/series";
import { AlertNotFoundError, AlertValidationError } from "./errors";
import { installAlertSchema } from "./schema";
import {
  defaultWebhookSender,
  parseWebhookHeaders,
  parseWebhookSecret,
  parseWebhookUrl,
  sanitizeDeliveryError,
  type WebhookDeliveryResult,
  type WebhookSender,
} from "./webhook";

const DEFAULT_MAX_ATTEMPTS = 6;
const MAX_DELIVERY_TIMEOUT_MS = 5_000;
const RETRY_BASE_MS = 5_000;
const RETRY_CAP_MS = 300_000;
const MISSING_WEBHOOK_RETRY_MS = 60_000;
const MAX_PAYLOAD_BYTES = 64 * 1024;
const MAX_ERROR_TEXT = 256;

type RuleRow = {
  id: string;
  name: string;
  machine_id: string;
  metric: string;
  resource_id: string;
  condition: string;
  threshold: number | null;
  duration_ms: number | bigint;
  enabled: number | bigint;
  maintenance_until_ms: number | bigint | null;
  state: string;
  pending_since_ms: number | bigint | null;
  active_incident_id: string | null;
  last_value: number | null;
  last_observed_at_ms: number | bigint | null;
  created_at_ms: number | bigint;
  updated_at_ms: number | bigint;
};

type IncidentRow = {
  id: string;
  rule_id: string;
  rule_name: string;
  machine_id: string;
  metric: string;
  resource_id: string;
  condition: string;
  status: string;
  started_at_ms: number | bigint;
  resolved_at_ms: number | bigint | null;
  started_value: number | null;
  resolved_value: number | null;
  last_observed_at_ms: number | bigint;
  message: string;
};

type JobRow = {
  id: string;
  rule_id: string;
  incident_id: string;
  machine_id: string;
  event: string;
  status: string;
  attempts: number | bigint;
  max_attempts: number | bigint;
  next_attempt_ms: number | bigint | null;
  last_attempt_ms: number | bigint | null;
  last_error: string | null;
  created_at_ms: number | bigint;
  delivered_at_ms: number | bigint | null;
  payload_json: string;
};

type WebhookRow = {
  url: string;
  headers_json: string;
  secret: string | null;
  enabled: number | bigint;
  updated_at_ms: number | bigint;
  last_test_at_ms: number | bigint | null;
  last_test_ok: number | bigint | null;
  last_error: string | null;
};

type ProgressRow = {
  collector_id: string;
  run_id: string;
  seq: number | bigint;
  received_at_ms: number | bigint;
};

type Observation =
  | {state: "value"; value: number}
  | {state: "unavailable"}
  | {state: "absent"};

function iso(ms: number | bigint | null | undefined): string | null {
  if (ms === null || ms === undefined) return null;
  const value = Number(ms);
  return Number.isFinite(value) ? new Date(value).toISOString() : null;
}

function epoch(ms: number | bigint | null | undefined): number | null {
  if (ms === null || ms === undefined) return null;
  const value = Number(ms);
  return Number.isFinite(value) ? value : null;
}

function capError(value: string): string {
  const clean = value.replace(/[\u0000-\u001f\u007f]/g, " ").trim();
  return clean.length <= MAX_ERROR_TEXT ? clean : `${clean.slice(0, MAX_ERROR_TEXT - 3)}...`;
}

/**
 * Remote responses/errors are untrusted. Remove the configured URL, header
 * values, secret and URL-shaped/Bearer-shaped fragments before persistence.
 */
function redactDeliveryError(
  raw: string,
  config: {url: string; headers: Record<string, string>; secret: string | null},
): string {
  let text = String(raw);
  const sensitive = [config.url, config.secret ?? "", ...Object.values(config.headers)]
    .filter((value) => value !== "");
  try {
    const parsed = new URL(config.url);
    if (parsed.pathname !== "/") sensitive.push(parsed.pathname);
    for (const value of parsed.searchParams.values()) {
      if (value !== "") sensitive.push(value);
    }
  } catch {
    // The URL was validated when configured; keep the exact-string redaction.
  }
  sensitive.sort((a, b) => b.length - a.length);
  for (const value of sensitive) {
    text = text.split(value).join("[redacted]");
  }
  text = text.replace(/https?:\/\/[^\s"'<>]+/gi, "[redacted-url]");
  text = text.replace(/\bBearer\s+[A-Za-z0-9._~+/=-]+/gi, "Bearer [redacted]");
  text = text.replace(/\b(authorization|proxy-authorization)\b[^\n]*/gi, "$1 [redacted]");
  return capError(text);
}

function formatDuration(ms: number): string {
  if (ms < 60_000) return `${Math.round(ms / 1000)} s`;
  if (ms < 3_600_000) return `${Math.round(ms / 60_000)} min`;
  return `${Math.round(ms / 3_600_000)} h`;
}

function formatBytes(bytes: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = Math.max(0, bytes);
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return index === 0 ? `${Math.round(value)} ${units[index]}` : `${value.toFixed(1)} ${units[index]}`;
}

function formatValue(metric: AlertMetric, unit: AlertMetricUnit, value: number | null): string {
  if (value === null) return "unavailable";
  if (!Number.isFinite(value)) return "unavailable";
  switch (unit) {
    case "percent":
      return `${Math.round(value * 100) / 100}%`;
    case "bytes":
      return formatBytes(value);
    case "bytes_per_second":
      return `${formatBytes(value)}/s`;
    case "celsius":
      return `${Math.round(value)} °C`;
    case "watts":
      return `${Math.round(value)} W`;
    case "milliseconds":
      return `${Math.round(value * 10) / 10} ms`;
    default:
      void metric;
      return String(value);
  }
}

function conditionPhrase(condition: AlertCondition): string {
  switch (condition) {
    case "above":
      return "at or above";
    case "below":
      return "at or below";
    case "stale":
      return "stale";
    case "unavailable":
      return "unavailable";
  }
}

function mapRule(row: RuleRow): AlertRule {
  const metric = isAlertMetric(row.metric) ? row.metric : "cpu.usage";
  const condition = (ALERT_CONDITIONS.some((item) => item.value === row.condition)
    ? row.condition
    : "above") as AlertCondition;
  const state = (row.state === "pending" || row.state === "firing" ? row.state : "ok") as AlertRuleState;
  return {
    id: row.id,
    name: row.name,
    machineId: row.machine_id,
    metric,
    resourceId: row.resource_id,
    condition,
    threshold: row.threshold,
    durationMs: Number(row.duration_ms),
    enabled: Number(row.enabled) === 1,
    maintenanceUntil: iso(row.maintenance_until_ms),
    state,
    pendingSince: iso(row.pending_since_ms),
    activeIncidentId: row.active_incident_id,
    lastValue: row.last_value,
    lastObservedAt: iso(row.last_observed_at_ms),
    createdAt: iso(row.created_at_ms) ?? new Date(0).toISOString(),
    updatedAt: iso(row.updated_at_ms) ?? new Date(0).toISOString(),
  };
}

function mapIncident(row: IncidentRow): AlertIncident {
  return {
    id: row.id,
    ruleId: row.rule_id,
    ruleName: row.rule_name,
    machineId: row.machine_id,
    metric: isAlertMetric(row.metric) ? row.metric : "cpu.usage",
    resourceId: row.resource_id,
    condition: row.condition as AlertCondition,
    status: row.status === "recovered" ? "recovered" : "open",
    startedAt: iso(row.started_at_ms) ?? new Date(0).toISOString(),
    resolvedAt: iso(row.resolved_at_ms),
    startedValue: row.started_value,
    resolvedValue: row.resolved_value,
    lastObservedAt: iso(row.last_observed_at_ms) ?? new Date(0).toISOString(),
    message: row.message,
  };
}

function mapJob(row: JobRow): AlertNotification {
  const status: AlertJobStatus =
    row.status === "delivered" || row.status === "failed" ? row.status : "pending";
  return {
    id: row.id,
    ruleId: row.rule_id,
    incidentId: row.incident_id,
    event: row.event === "recovered" ? "recovered" : "fired",
    status,
    attempts: Number(row.attempts),
    maxAttempts: Number(row.max_attempts),
    nextAttemptAt: iso(row.next_attempt_ms),
    lastAttemptAt: iso(row.last_attempt_ms),
    lastError: row.last_error,
    createdAt: iso(row.created_at_ms) ?? new Date(0).toISOString(),
    deliveredAt: iso(row.delivered_at_ms),
  };
}

export type AlertEngineOptions = {
  database: HubDatabase;
  now?: () => number;
  sendWebhook?: WebhookSender;
  maxIncidents?: number;
  maxJobs?: number;
  maxAttempts?: number;
  deliveryTimeoutMs?: number;
};

export class AlertEngine {
  private readonly database: HubDatabase;
  private readonly db: DatabaseSync;
  private readonly now: () => number;
  private readonly sendWebhook: WebhookSender;
  private readonly maxIncidents: number;
  private readonly maxJobs: number;
  private readonly maxAttempts: number;
  private readonly deliveryTimeoutMs: number;
  private readonly startedAtMs: number;
  private stopped = false;
  private closed = false;
  private notificationActive = false;
  private notificationQueued = false;
  private notificationChain: Promise<void> = Promise.resolve();
  private testInFlight: Promise<AlertWebhookTestResult> | null = null;
  private readonly activeControllers = new Set<AbortController>();

  private readonly ruleById: StatementSync;
  private readonly rulesForMachine: StatementSync;
  private readonly allRules: StatementSync;
  private readonly insertRule: StatementSync;
  private readonly updateRuleConfig: StatementSync;
  private readonly updateRuleState: StatementSync;
  private readonly deleteRuleStmt: StatementSync;
  private readonly countRules: StatementSync;
  private readonly insertIncident: StatementSync;
  private readonly incidentById: StatementSync;
  private readonly openIncidentForRule: StatementSync;
  private readonly resolveIncident: StatementSync;
  private readonly incidentsRecent: StatementSync;
  private readonly trimIncidents: StatementSync;
  private readonly insertJob: StatementSync;
  private readonly dueJobs: StatementSync;
  private readonly updateJob: StatementSync;
  private readonly recentJobs: StatementSync;
  private readonly trimJobs: StatementSync;
  private readonly webhookSelect: StatementSync;
  private readonly webhookUpsert: StatementSync;
  private readonly webhookDelete: StatementSync;
  private readonly webhookTestUpdate: StatementSync;
  private readonly progressSelect: StatementSync;
  private readonly progressUpsert: StatementSync;
  private readonly observedSelect: StatementSync;
  private readonly observedUpsert: StatementSync;
  private readonly observedDeleteRule: StatementSync;
  private readonly seriesObserved: StatementSync;

  constructor(options: AlertEngineOptions) {
    this.database = options.database;
    this.db = options.database.db;
    this.now = options.now ?? Date.now;
    this.sendWebhook = options.sendWebhook ?? defaultWebhookSender;
    this.maxIncidents = boundedCount(options.maxIncidents, ALERT_LIMITS.maxIncidents);
    this.maxJobs = boundedCount(options.maxJobs, ALERT_LIMITS.maxJobs);
    this.maxAttempts = boundedCount(options.maxAttempts, DEFAULT_MAX_ATTEMPTS, 1, 100);
    this.deliveryTimeoutMs = boundedCount(options.deliveryTimeoutMs, MAX_DELIVERY_TIMEOUT_MS, 100, 30_000);
    this.startedAtMs = this.now();
    installAlertSchema(this.db);

    const db = this.db;
    this.ruleById = db.prepare("SELECT * FROM alert_rules WHERE id = ?");
    this.rulesForMachine = db.prepare("SELECT * FROM alert_rules WHERE machine_id = ? ORDER BY created_at_ms ASC");
    this.allRules = db.prepare("SELECT * FROM alert_rules ORDER BY created_at_ms ASC");
    this.insertRule = db.prepare(
      "INSERT INTO alert_rules " +
        "(id, name, machine_id, metric, resource_id, condition, threshold, duration_ms, enabled, " +
        "maintenance_until_ms, state, pending_since_ms, active_incident_id, last_value, " +
        "last_observed_at_ms, created_at_ms, updated_at_ms) " +
        "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ok', NULL, NULL, NULL, NULL, ?, ?)",
    );
    this.updateRuleConfig = db.prepare(
      "UPDATE alert_rules SET name = ?, machine_id = ?, metric = ?, resource_id = ?, condition = ?, " +
        "threshold = ?, duration_ms = ?, enabled = ?, maintenance_until_ms = ?, updated_at_ms = ? WHERE id = ?",
    );
    this.updateRuleState = db.prepare(
      "UPDATE alert_rules SET state = ?, pending_since_ms = ?, active_incident_id = ?, " +
        "last_value = ?, last_observed_at_ms = ? WHERE id = ?",
    );
    this.deleteRuleStmt = db.prepare("DELETE FROM alert_rules WHERE id = ?");
    this.countRules = db.prepare("SELECT COUNT(*) AS count FROM alert_rules");

    this.insertIncident = db.prepare(
      "INSERT INTO alert_incidents " +
        "(id, rule_id, rule_name, machine_id, metric, resource_id, condition, status, started_at_ms, " +
        "resolved_at_ms, started_value, resolved_value, last_observed_at_ms, message) " +
        "VALUES (?, ?, ?, ?, ?, ?, ?, 'open', ?, NULL, ?, NULL, ?, ?)",
    );
    this.incidentById = db.prepare("SELECT * FROM alert_incidents WHERE id = ?");
    this.openIncidentForRule = db.prepare(
      "SELECT * FROM alert_incidents WHERE rule_id = ? AND status = 'open' ORDER BY started_at_ms DESC LIMIT 1",
    );
    this.resolveIncident = db.prepare(
      "UPDATE alert_incidents SET status = 'recovered', resolved_at_ms = ?, resolved_value = ?, " +
        "last_observed_at_ms = ? WHERE id = ? AND status = 'open'",
    );
    this.incidentsRecent = db.prepare(
      "SELECT * FROM alert_incidents ORDER BY started_at_ms DESC LIMIT ?",
    );
    this.trimIncidents = db.prepare(
      "DELETE FROM alert_incidents WHERE id NOT IN (" +
        "SELECT id FROM alert_incidents ORDER BY (status = 'open') DESC, started_at_ms DESC LIMIT ?)",
    );

    this.insertJob = db.prepare(
      "INSERT OR IGNORE INTO alert_jobs " +
        "(id, rule_id, incident_id, machine_id, event, status, attempts, max_attempts, next_attempt_ms, " +
        "last_attempt_ms, last_error, created_at_ms, delivered_at_ms, payload_json) " +
        "VALUES (?, ?, ?, ?, ?, 'pending', 0, ?, ?, NULL, NULL, ?, NULL, ?)",
    );
    this.dueJobs = db.prepare(
      "SELECT * FROM alert_jobs WHERE status = 'pending' AND (next_attempt_ms IS NULL OR next_attempt_ms <= ?) " +
        "ORDER BY created_at_ms ASC LIMIT 8",
    );
    this.updateJob = db.prepare(
      "UPDATE alert_jobs SET status = ?, attempts = ?, next_attempt_ms = ?, last_attempt_ms = ?, " +
        "last_error = ?, delivered_at_ms = ? WHERE id = ?",
    );
    this.recentJobs = db.prepare("SELECT * FROM alert_jobs ORDER BY created_at_ms DESC LIMIT ?");
    this.trimJobs = db.prepare(
      "DELETE FROM alert_jobs WHERE id NOT IN (" +
        "SELECT id FROM alert_jobs ORDER BY (status = 'pending') DESC, created_at_ms DESC LIMIT ?)",
    );

    this.webhookSelect = db.prepare("SELECT * FROM alert_webhook WHERE id = 1");
    this.webhookUpsert = db.prepare(
      "INSERT INTO alert_webhook (id, url, headers_json, secret, enabled, updated_at_ms, " +
        "last_test_at_ms, last_test_ok, last_error) VALUES (1, ?, ?, ?, ?, ?, NULL, NULL, NULL) " +
        "ON CONFLICT(id) DO UPDATE SET url = excluded.url, headers_json = excluded.headers_json, " +
        "secret = excluded.secret, enabled = excluded.enabled, updated_at_ms = excluded.updated_at_ms, " +
        "last_test_at_ms = NULL, last_test_ok = NULL, last_error = NULL",
    );
    this.webhookDelete = db.prepare("DELETE FROM alert_webhook WHERE id = 1");
    this.webhookTestUpdate = db.prepare(
      "UPDATE alert_webhook SET last_test_at_ms = ?, last_test_ok = ?, last_error = ? WHERE id = 1",
    );

    this.progressSelect = db.prepare("SELECT * FROM alert_machine_progress WHERE machine_id = ?");
    this.progressUpsert = db.prepare(
      "INSERT INTO alert_machine_progress (machine_id, collector_id, run_id, seq, received_at_ms, updated_at_ms) " +
        "VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(machine_id) DO UPDATE SET collector_id = excluded.collector_id, " +
        "run_id = excluded.run_id, seq = excluded.seq, received_at_ms = excluded.received_at_ms, " +
        "updated_at_ms = excluded.updated_at_ms",
    );
    this.observedSelect = db.prepare(
      "SELECT 1 AS seen FROM alert_observed_resources WHERE rule_id = ? AND metric = ? AND resource_id = ?",
    );
    this.observedUpsert = db.prepare(
      "INSERT INTO alert_observed_resources (rule_id, metric, resource_id, first_seen_ms, last_seen_ms) " +
        "VALUES (?, ?, ?, ?, ?) ON CONFLICT(rule_id, metric, resource_id) DO UPDATE SET " +
        "last_seen_ms = excluded.last_seen_ms",
    );
    this.observedDeleteRule = db.prepare("DELETE FROM alert_observed_resources WHERE rule_id = ?");
    this.seriesObserved = db.prepare(
      "SELECT 1 AS seen FROM series WHERE machine_id = ? AND metric = ? AND resource_id = ? LIMIT 1",
    );
  }

  // -- lifecycle --------------------------------------------------------

  stop(): void {
    if (this.closed) return;
    this.stopped = true;
    // Block any write from a late remote completion even if the caller closes
    // SQLite without awaiting drain(). drain() is still the cooperative path
    // that waits for an in-flight request up to its deadline.
    this.closed = true;
    for (const controller of this.activeControllers) controller.abort();
  }

  /**
   * Bounded shutdown. Aborts in-flight webhook requests, waits for the
   * notification chain/test request (up to a deadline), then marks the engine
   * closed so a late remote response can never write SQLite after the parent
   * closes the database. The parent MUST await this before `database.close()`.
   */
  async drain(timeoutMs = Math.min(30_000, this.deliveryTimeoutMs + 1_000)): Promise<void> {
    this.stop();
    const waits: Array<Promise<unknown>> = [this.notificationChain];
    if (this.testInFlight !== null) waits.push(this.testInFlight);
    const bounded = Number.isFinite(timeoutMs) && timeoutMs > 0 ? timeoutMs : 6_000;
    let timer: NodeJS.Timeout | null = null;
    try {
      await Promise.race([
        Promise.allSettled(waits),
        new Promise<void>((resolve) => {
          timer = setTimeout(resolve, bounded);
        }),
      ]);
    } finally {
      if (timer !== null) clearTimeout(timer);
      this.closed = true;
    }
  }

  /** Alias for callers that prefer an explicit async close. */
  async close(): Promise<void> {
    await this.drain();
  }

  observe(machineId: string, record: MetricsRecord, receivedAtMs: number): void {
    if (this.stopped || this.closed || !Number.isFinite(receivedAtMs)) return;
    // Parent contract: only NEW durable LIVE commits call observe. This second
    // guard rejects a historical/replayed/old record after a restart even if
    // the parent invocation is wrong.
    if (receivedAtMs < this.startedAtMs) return;
    const rules = this.rulesForMachine.all(machineId) as RuleRow[];
    // No rules is the normal default. Do not write a progress row for every
    // one-second sample just to remember a sequence number that no rule uses;
    // that turned an otherwise RAM-only hub into a sustained disk writer.
    // Pending deliveries from a previously deleted rule still get a chance to
    // drain.
    if (rules.length === 0) {
      void this.processNotifications();
      return;
    }
    try {
      this.transaction(() => {
        const progress = this.progressSelect.get(machineId) as ProgressRow | undefined;
        if (progress !== undefined) {
          const sameRun = progress.run_id === record.runId;
          if (sameRun && Number(record.sequence) <= Number(progress.seq)) return;
          if (receivedAtMs < Number(progress.received_at_ms)) return;
        }
        this.progressUpsert.run(
          machineId,
          record.collectorId,
          record.runId,
          record.sequence,
          receivedAtMs,
          this.now(),
        );
        for (const row of rules) {
          const rule = mapRule(row);
          if (!rule.enabled) continue;
          if (rule.condition === "stale") {
            // A just-received live record proves the machine is not stale.
            this.evaluate(rule, false, null, receivedAtMs);
            continue;
          }
          const observation = this.observationFor(rule, record.payload, receivedAtMs);
          const condition = this.conditionIsTrue(rule, observation);
          this.evaluate(rule, condition, observation.state === "value" ? observation.value : null, receivedAtMs);
        }
      });
    } catch {
      // The live path must never be interrupted by an alerting failure.
      try {
        this.db.exec("ROLLBACK");
      } catch {
        // already rolled back
      }
    }
    void this.processNotifications();
  }

  tick(machines: MachineState[]): void {
    if (this.stopped || this.closed) return;
    const count = Number((this.countRules.get() as { count?: number | bigint } | undefined)?.count ?? 0);
    if (count === 0) {
      void this.processNotifications();
      return;
    }
    const at = this.now();
    try {
      this.transaction(() => {
        for (const machine of machines) {
          const rows = this.rulesForMachine.all(machine.machineId) as RuleRow[];
          for (const row of rows) {
            const rule = mapRule(row);
            if (!rule.enabled) continue;
            if (rule.condition === "stale") {
              const last = machine.lastReceivedAt === null ? null : Date.parse(machine.lastReceivedAt);
              const stale = last !== null && Number.isFinite(last)
                && at - last >= alertStaleThresholdMs(machine.expectedIntervalMs);
              this.evaluate(rule, stale, null, at);
              continue;
            }
            if (rule.condition === "unavailable") {
              const payload = machine.metrics?.payload;
              if (payload === undefined) continue;
              // A restored last-known payload is history until this process has
              // received a fresh live record. Stale rules still evaluate: their
              // condition is the current absence of a receipt, not a value.
              const metricsReceived = machine.metrics === undefined
                ? null
                : Date.parse(machine.metrics.receivedAt);
              if (metricsReceived === null || !Number.isFinite(metricsReceived)
                || metricsReceived < this.startedAtMs) {
                continue;
              }
              const observation = this.observationFor(rule, payload, at);
              this.evaluate(rule, observation.state === "unavailable", null, at);
            }
          }
        }
        // Machines absent from the snapshot are left untouched; the parent
        // controls which machines are in scope.
      });
    } catch {
      try {
        this.db.exec("ROLLBACK");
      } catch {
        // already rolled back
      }
    }
    void this.processNotifications();
  }

  // -- rules ------------------------------------------------------------

  createRule(input: AlertRuleInput): AlertRule {
    const validated = this.validateRuleInput(input);
    const count = Number((this.countRules.get() as {count?: number | bigint} | undefined)?.count ?? 0);
    if (count >= ALERT_LIMITS.maxRules) throw new AlertValidationError("alert rule limit reached");
    const id = randomUUID();
    const at = this.now();
    return this.transaction(() => {
      this.insertRule.run(
        id,
        validated.name,
        validated.machineId,
        validated.metric,
        validated.resourceId,
        validated.condition,
        validated.threshold,
        validated.durationMs,
        validated.enabled ? 1 : 0,
        validated.maintenanceUntilMs,
        at,
        at,
      );
      return this.getRule(id);
    });
  }

  updateRule(id: string, input: AlertRuleInput): AlertRule {
    const existingRow = this.ruleById.get(id) as RuleRow | undefined;
    if (existingRow === undefined) throw new AlertNotFoundError();
    const existing = mapRule(existingRow);
    const validated = this.validateRuleInput(input);
    const at = this.now();
    const targetChanged =
      existing.machineId !== validated.machineId ||
      existing.metric !== validated.metric ||
      existing.resourceId !== validated.resourceId ||
      existing.condition !== validated.condition ||
      existing.threshold !== validated.threshold;
    const maintenanceActive = validated.maintenanceUntilMs !== null && validated.maintenanceUntilMs > at;
    return this.transaction(() => {
      if (existing.state === "firing" && (targetChanged || !validated.enabled)) {
        this.closeIncidentSilently(existing, at);
      } else if (existing.state === "pending" && (targetChanged || maintenanceActive || !validated.enabled)) {
        this.setState(existing.id, "ok", null, null, existing.lastValue, at);
      }
      if (targetChanged) this.observedDeleteRule.run(id);
      this.updateRuleConfig.run(
        validated.name,
        validated.machineId,
        validated.metric,
        validated.resourceId,
        validated.condition,
        validated.threshold,
        validated.durationMs,
        validated.enabled ? 1 : 0,
        validated.maintenanceUntilMs,
        at,
        id,
      );
      return this.getRule(id);
    });
  }

  deleteRule(id: string): boolean {
    const row = this.ruleById.get(id) as RuleRow | undefined;
    if (row === undefined) return false;
    const rule = mapRule(row);
    return this.transaction(() => {
      this.closeIncidentSilently(rule, this.now());
      this.observedDeleteRule.run(id);
      this.deleteRuleStmt.run(id);
      return true;
    });
  }

  getRule(id: string): AlertRule {
    const row = this.ruleById.get(id) as RuleRow | undefined;
    if (row === undefined) throw new AlertNotFoundError();
    return mapRule(row);
  }

  listRules(scope: AlertMachineScope = "*"): AlertRule[] {
    const rows = this.allRules.all() as RuleRow[];
    return rows
      .filter((row) => alertScopeIncludes(scope, row.machine_id))
      .slice(0, ALERT_LIMITS.maxRules)
      .map(mapRule);
  }

  listIncidents(scope: AlertMachineScope = "*", limit = 200): AlertIncident[] {
    const bounded = boundedCount(limit, 200, 1, ALERT_LIMITS.maxListLimit);
    const rows = this.incidentsRecent.all(this.maxIncidents) as IncidentRow[];
    return rows
      .filter((row) => alertScopeIncludes(scope, row.machine_id))
      .slice(0, bounded)
      .map(mapIncident);
  }

  notificationSummary(scope: AlertMachineScope = "*"): AlertNotificationSummary {
    const rows = (this.recentJobs.all(this.maxJobs) as JobRow[])
      .filter((row) => alertScopeIncludes(scope, row.machine_id));
    const notifications = rows.map(mapJob);
    const pending = notifications.filter((job) => job.status === "pending").length;
    const delivered = notifications.filter((job) => job.status === "delivered").length;
    const failed = notifications.filter((job) => job.status === "failed").length;
    const lastDelivery = notifications.find((job) => job.status === "delivered") ?? null;
    const lastError = notifications.find((job) => job.lastError !== null)?.lastError ?? null;
    return {
      pending,
      delivered,
      failed,
      recent: notifications.slice(0, 50),
      lastDeliveryAt: lastDelivery?.deliveredAt ?? null,
      lastError,
      delivery: "at-least-once",
    };
  }

  listResponse(scope: AlertMachineScope, canAdmin: boolean): AlertListResponse {
    return {
      generatedAt: new Date(this.now()).toISOString(),
      canAdmin,
      rules: this.listRules(scope),
      incidents: this.listIncidents(scope),
      notifications: this.notificationSummary(scope),
      webhook: this.getWebhookStatus(),
      limits: ALERT_LIMITS,
      deliveryDisclosure: ALERT_DELIVERY_DISCLOSURE,
    };
  }

  // -- webhook configuration -------------------------------------------

  getWebhookStatus(): AlertWebhookStatus {
    const row = this.webhookSelect.get() as WebhookRow | undefined;
    if (row === undefined) {
      return {
        channel: "webhook",
        configured: false,
        enabled: false,
        transport: null,
        lastTestAt: null,
        lastTestOk: null,
        lastError: null,
        delivery: "at-least-once",
      };
    }
    let transport: AlertWebhookTransport | null = null;
    try {
      transport = parseWebhookUrl(row.url).transport;
    } catch {
      transport = null;
    }
    return {
      channel: "webhook",
      configured: true,
      enabled: Number(row.enabled) === 1,
      transport,
      lastTestAt: iso(row.last_test_at_ms),
      lastTestOk: row.last_test_ok === null ? null : Number(row.last_test_ok) === 1,
      lastError: row.last_error,
      delivery: "at-least-once",
    };
  }

  configureWebhook(input: AlertWebhookConfigureInput): AlertWebhookStatus {
    const parsed = parseWebhookUrl(input.url);
    const headers = parseWebhookHeaders(input.headers);
    const secret = parseWebhookSecret(input.secret);
    const enabled = input.enabled ?? true;
    if (typeof enabled !== "boolean") throw new AlertValidationError("webhook enabled must be boolean");
    this.webhookUpsert.run(
      parsed.url,
      JSON.stringify(headers),
      secret,
      enabled ? 1 : 0,
      this.now(),
    );
    return this.getWebhookStatus();
  }

  clearWebhook(): AlertWebhookStatus {
    this.webhookDelete.run();
    return this.getWebhookStatus();
  }

  testWebhook(): Promise<AlertWebhookTestResult> {
    if (this.stopped || this.closed) {
      return Promise.resolve({
        ok: false,
        status: null,
        error: "alerts are shutting down",
        testedAt: new Date(this.now()).toISOString(),
      });
    }
    const run = this.performTestWebhook();
    this.testInFlight = run;
    void run.finally(() => {
      if (this.testInFlight === run) this.testInFlight = null;
    }).catch(() => {});
    return run;
  }

  private async performTestWebhook(): Promise<AlertWebhookTestResult> {
    const testedAt = this.now();
    const row = this.webhookSelect.get() as WebhookRow | undefined;
    if (row === undefined) {
      return {ok: false, status: null, error: "webhook is not configured", testedAt: new Date(testedAt).toISOString()};
    }
    if (Number(row.enabled) !== 1) {
      return {ok: false, status: null, error: "webhook is disabled", testedAt: new Date(testedAt).toISOString()};
    }
    let parsed;
    try {
      parsed = parseWebhookUrl(row.url);
    } catch {
      return {ok: false, status: null, error: "webhook url is invalid", testedAt: new Date(testedAt).toISOString()};
    }
    if (parsed.transport !== "loopback-http") {
      const result = {
        ok: false,
        status: null,
        error: "test delivery is restricted to a loopback HTTP sink",
        testedAt: new Date(testedAt).toISOString(),
      };
      this.recordTest(testedAt, false, result.error);
      return result;
    }
    const body = JSON.stringify({
      event: "test",
      testedAt: new Date(testedAt).toISOString(),
      text: "Rigscope webhook test. This is an explicit admin action.",
      delivery: "at-least-once",
    });
    let result: WebhookDeliveryResult;
    let headers: Record<string, string> = {};
    const controller = this.beginController();
    try {
      headers = parseWebhookHeaders(JSON.parse(row.headers_json) as unknown);
      result = await this.sendWebhook({
        url: row.url,
        headers,
        secret: row.secret,
        body,
        timeoutMs: this.deliveryTimeoutMs,
        jobId: `alert-test-${createHash("sha256").update(`${row.url}\0${row.updated_at_ms}`).digest("hex").slice(0, 24)}`,
        signal: controller.signal,
      });
    } catch (error) {
      result = {ok: false, status: null, error: sanitizeDeliveryError(error)};
    } finally {
      this.endController(controller);
    }
    const error = result.ok
      ? null
      : redactDeliveryError(result.error ?? "delivery failed", {url: row.url, headers, secret: row.secret});
    this.recordTest(testedAt, result.ok, error);
    return {
      ok: result.ok,
      status: result.status,
      error,
      testedAt: new Date(testedAt).toISOString(),
    };
  }

  private recordTest(at: number, ok: boolean, error: string | null): void {
    if (this.closed) return;
    this.webhookTestUpdate.run(at, ok ? 1 : 0, error);
  }

  // -- notifications ----------------------------------------------------

  processNotifications(): Promise<void> {
    if (this.stopped || this.closed) return Promise.resolve();
    if (this.notificationActive) {
      this.notificationQueued = true;
      return this.notificationChain;
    }
    this.notificationActive = true;
    const run = (async () => {
      try {
        do {
          this.notificationQueued = false;
          try {
            await this.runNotifications();
          } catch {
            // Per-job failures are persisted; a storage error must not escape
            // the live/tick path or reject an unrelated caller.
          }
        } while (this.notificationQueued && !this.stopped);
      } finally {
        this.notificationActive = false;
      }
    })();
    this.notificationChain = run;
    return run;
  }

  private beginController(): AbortController {
    const controller = new AbortController();
    this.activeControllers.add(controller);
    return controller;
  }

  private endController(controller: AbortController): void {
    this.activeControllers.delete(controller);
  }

  private async runNotifications(): Promise<void> {
    if (this.stopped || this.closed) return;
    const due = this.dueJobs.all(this.now()) as JobRow[];
    if (due.length === 0) return;
    const webhook = this.webhookSelect.get() as WebhookRow | undefined;
    if (webhook === undefined || Number(webhook.enabled) !== 1) {
      const retryAt = this.now() + MISSING_WEBHOOK_RETRY_MS;
      for (const job of due) {
        this.updateJob.run(
          "pending",
          Number(job.attempts),
          retryAt,
          epoch(job.last_attempt_ms),
          "webhook not configured",
          null,
          job.id,
        );
      }
      return;
    }
    let headers: Record<string, string>;
    try {
      headers = parseWebhookHeaders(JSON.parse(webhook.headers_json) as unknown);
    } catch {
      headers = {};
    }
    for (const job of due) {
      if (this.stopped || this.closed) return;
      const attempts = Number(job.attempts) + 1;
      let result: WebhookDeliveryResult;
      const controller = this.beginController();
      try {
        result = await this.sendWebhook({
          url: webhook.url,
          headers,
          secret: webhook.secret,
          body: job.payload_json,
          timeoutMs: this.deliveryTimeoutMs,
          jobId: job.id,
          signal: controller.signal,
        });
      } catch (error) {
        result = {ok: false, status: null, error: sanitizeDeliveryError(error)};
      } finally {
        this.endController(controller);
      }
      if (this.closed) return;
      const attemptAt = this.now();
      if (result.ok) {
        this.updateJob.run("delivered", attempts, null, attemptAt, null, attemptAt, job.id);
        continue;
      }
      const error = redactDeliveryError(result.error ?? "delivery failed", {
        url: webhook.url,
        headers,
        secret: webhook.secret,
      });
      if (attempts >= Number(job.max_attempts)) {
        this.updateJob.run("failed", attempts, null, attemptAt, error, null, job.id);
      } else {
        const backoff = Math.min(RETRY_CAP_MS, RETRY_BASE_MS * 2 ** Math.max(0, attempts - 1));
        this.updateJob.run("pending", attempts, attemptAt + backoff, attemptAt, error, null, job.id);
      }
    }
  }

  // -- evaluation -------------------------------------------------------

  private evaluate(rule: AlertRule, condition: boolean, value: number | null, at: number): void {
    const maintenanceUntil = rule.maintenanceUntil === null ? null : Date.parse(rule.maintenanceUntil);
    const maintenance = maintenanceUntil !== null && Number.isFinite(maintenanceUntil) && maintenanceUntil > at;
    if (maintenance) {
      if (rule.state === "pending") {
        this.setState(rule.id, "ok", null, null, rule.lastValue, at);
      }
      return;
    }

    if (rule.state === "firing") {
      if (condition) {
        this.setState(rule.id, "firing", rule.pendingSince, rule.activeIncidentId, value, at);
        return;
      }
      this.recover(rule, value, at);
      return;
    }

    if (!condition) {
      this.setState(rule.id, "ok", null, null, value, at);
      return;
    }

    const pendingSince = rule.pendingSince === null ? null : Date.parse(rule.pendingSince);
    const since = rule.state === "pending" && pendingSince !== null && Number.isFinite(pendingSince)
      ? pendingSince
      : at;
    if (at - since >= rule.durationMs) {
      this.openIncident(rule, value, at);
    } else {
      this.setState(rule.id, "pending", new Date(since).toISOString(), null, value, at);
    }
  }

  private openIncident(rule: AlertRule, value: number | null, at: number): void {
    const existing = this.openIncidentForRule.get(rule.id) as IncidentRow | undefined;
    if (existing !== undefined) {
      this.setState(rule.id, "firing", null, existing.id, value, at);
      return;
    }
    const incidentId = randomUUID();
    const message = this.incidentMessage(rule, value);
    this.insertIncident.run(
      incidentId,
      rule.id,
      rule.name,
      rule.machineId,
      rule.metric,
      rule.resourceId,
      rule.condition,
      at,
      value,
      at,
      message,
    );
    this.setState(rule.id, "firing", null, incidentId, value, at);
    const incident = this.incidentById.get(incidentId) as IncidentRow | undefined;
    if (incident !== undefined) this.enqueueJob(rule, mapIncident(incident), "fired", value, at);
    this.trimIncidents.run(this.maxIncidents);
  }

  private recover(rule: AlertRule, value: number | null, at: number): void {
    if (rule.activeIncidentId === null) {
      this.setState(rule.id, "ok", null, null, value, at);
      return;
    }
    this.resolveIncident.run(at, value, at, rule.activeIncidentId);
    const incident = this.incidentById.get(rule.activeIncidentId) as IncidentRow | undefined;
    this.setState(rule.id, "ok", null, null, value, at);
    if (incident !== undefined) this.enqueueJob(rule, mapIncident(incident), "recovered", value, at);
  }

  private closeIncidentSilently(rule: AlertRule, at: number): void {
    if (rule.activeIncidentId !== null) {
      this.resolveIncident.run(at, rule.lastValue, at, rule.activeIncidentId);
    }
    if (rule.state !== "ok" || rule.activeIncidentId !== null) {
      this.setState(rule.id, "ok", null, null, rule.lastValue, at);
    }
  }

  private setState(
    ruleId: string,
    state: AlertRuleState,
    pendingSince: string | null,
    activeIncidentId: string | null,
    lastValue: number | null,
    observedAt: number,
  ): void {
    this.updateRuleState.run(
      state,
      pendingSince === null ? null : Date.parse(pendingSince),
      activeIncidentId,
      lastValue,
      observedAt,
      ruleId,
    );
  }

  private incidentMessage(rule: AlertRule, value: number | null): string {
    const label = `${ALERT_METRICS[rule.metric].label}${rule.resourceId ? ` (${rule.resourceId})` : ""}`;
    if (rule.condition === "stale") {
      return `${rule.name}: no receipt from ${rule.machineId} within the stale threshold`;
    }
    if (rule.condition === "unavailable") {
      return `${rule.name}: ${label} unavailable on ${rule.machineId} for ${formatDuration(rule.durationMs)}`;
    }
    const unit = ALERT_METRICS[rule.metric].unit;
    const observed = formatValue(rule.metric, unit, value);
    const threshold = formatValue(rule.metric, unit, rule.threshold);
    return `${rule.name}: ${label} ${observed} is ${conditionPhrase(rule.condition)} ${threshold} on ${rule.machineId}`;
  }

  private enqueueJob(
    rule: AlertRule,
    incident: AlertIncident,
    event: "fired" | "recovered",
    value: number | null,
    at: number,
  ): void {
    const jobId = `alert-${createHash("sha256")
      .update(`${incident.id}\0${event}`)
      .digest("hex")
      .slice(0, 40)}`;
    const payload = {
      event,
      jobId,
      alert: {
        id: rule.id,
        name: rule.name,
        machineId: rule.machineId,
        metric: rule.metric,
        resourceId: rule.resourceId,
        condition: rule.condition,
        threshold: rule.threshold,
        durationMs: rule.durationMs,
      },
      incident: {
        id: incident.id,
        status: incident.status,
        startedAt: incident.startedAt,
        resolvedAt: incident.resolvedAt,
      },
      observedValue: value,
      occurredAt: new Date(at).toISOString(),
      delivery: "at-least-once",
    };
    const body = JSON.stringify(payload);
    if (Buffer.byteLength(body, "utf8") > MAX_PAYLOAD_BYTES) return;
    this.insertJob.run(
      jobId,
      rule.id,
      incident.id,
      rule.machineId,
      event,
      this.maxAttempts,
      at,
      at,
      body,
    );
    this.trimJobs.run(this.maxJobs);
  }

  private observationFor(rule: AlertRule, payload: MetricsPayload, at: number): Observation {
    const points = flattenMetrics(payload);
    const match = points.find(
      (point) => point.metric === rule.metric && point.resourceId === rule.resourceId,
    );
    if (match !== undefined) {
      this.markResourceObserved(rule, at);
      return match.value === null ? {state: "unavailable"} : {state: "value", value: match.value};
    }
    if (this.familyUnavailable(rule.metric, payload)) {
      // The rule has now observed this resource in the failed family; when the
      // family returns without the resource it must remain unavailable.
      this.markResourceObserved(rule, at);
      return {state: "unavailable"};
    }
    if (this.wasResourceObserved(rule)) return {state: "unavailable"};
    return {state: "absent"};
  }

  private markResourceObserved(rule: AlertRule, at: number): void {
    if (rule.resourceId === "") return;
    this.observedUpsert.run(rule.id, rule.metric, rule.resourceId, at, at);
  }

  private wasResourceObserved(rule: AlertRule): boolean {
    if (rule.resourceId === "") return false;
    const row = this.observedSelect.get(rule.id, rule.metric, rule.resourceId) as
      | {seen?: number | bigint}
      | undefined;
    if (row !== undefined) return true;
    const historical = this.seriesObserved.get(rule.machineId, rule.metric, rule.resourceId) as
      | {seen?: number | bigint}
      | undefined;
    return historical !== undefined;
  }

  private familyUnavailable(metric: AlertMetric, payload: MetricsPayload): boolean {
    switch (alertMetricFamily(metric)) {
      case "cpu":
        return payload.cpu.status === "unavailable";
      case "memory":
        return payload.memory.status === "unavailable";
      case "swap":
        return payload.swap.status === "unavailable";
      case "gpus":
        return payload.gpus.status === "unavailable";
      case "disks":
        // Filesystem, disk-device and I/O-pressure readings share one family.
        return payload.disks.status === "unavailable";
      case "network":
        return payload.network.status === "unavailable";
    }
  }

  private conditionIsTrue(rule: AlertRule, observation: Observation): boolean {
    if (rule.condition === "unavailable") return observation.state === "unavailable";
    if (observation.state !== "value") return false;
    if (rule.threshold === null) return false;
    return rule.condition === "above"
      ? observation.value >= rule.threshold
      : observation.value <= rule.threshold;
  }

  // -- validation -------------------------------------------------------

  private validateRuleInput(input: AlertRuleInput): {
    name: string;
    machineId: string;
    metric: AlertMetric;
    resourceId: string;
    condition: AlertCondition;
    threshold: number | null;
    durationMs: number;
    enabled: boolean;
    maintenanceUntilMs: number | null;
  } {
    if (input === null || typeof input !== "object" || Array.isArray(input)) {
      throw new AlertValidationError("rule must be an object");
    }
    const name = boundedText(input.name, "name", ALERT_LIMITS.maxRuleName);
    const machineId = boundedText(input.machineId, "machineId", ALERT_LIMITS.maxMachineId, false);
    if (!isAlertMetric(input.metric)) throw new AlertValidationError("unknown metric");
    const metric = input.metric;
    const resourceId = boundedText(input.resourceId ?? "", "resourceId", ALERT_LIMITS.maxResourceId, false, true);
    if (ALERT_METRICS[metric].resource === "required" && resourceId === "") {
      throw new AlertValidationError("this metric requires a resource id");
    }
    if (ALERT_METRICS[metric].resource === "none" && resourceId !== "") {
      throw new AlertValidationError("this metric does not accept a resource id");
    }
    if (!ALERT_CONDITIONS.some((item) => item.value === input.condition)) {
      throw new AlertValidationError("unknown condition");
    }
    const condition = input.condition;
    if (!Number.isFinite(input.durationMs) || !Number.isInteger(input.durationMs)
      || input.durationMs < 0 || input.durationMs > ALERT_LIMITS.maxDurationMs) {
      throw new AlertValidationError("durationMs is outside bounds");
    }
    let threshold: number | null = null;
    if (condition === "above" || condition === "below") {
      if (typeof input.threshold !== "number" || !Number.isFinite(input.threshold)
        || Math.abs(input.threshold) > ALERT_LIMITS.maxThreshold) {
        throw new AlertValidationError("threshold is required and must be a finite bounded number");
      }
      threshold = input.threshold;
    } else if (input.threshold !== undefined && input.threshold !== null) {
      throw new AlertValidationError("this condition does not accept a threshold");
    }
    if (input.enabled !== undefined && typeof input.enabled !== "boolean") {
      throw new AlertValidationError("enabled must be boolean");
    }
    let maintenanceUntilMs: number | null = null;
    if (input.maintenanceUntil !== undefined && input.maintenanceUntil !== null) {
      if (typeof input.maintenanceUntil !== "string") {
        throw new AlertValidationError("maintenanceUntil must be an ISO timestamp or null");
      }
      const parsed = Date.parse(input.maintenanceUntil);
      if (!Number.isFinite(parsed)) throw new AlertValidationError("maintenanceUntil is not a valid timestamp");
      maintenanceUntilMs = parsed;
    }
    return {
      name,
      machineId,
      metric,
      resourceId,
      condition,
      threshold,
      durationMs: input.durationMs,
      enabled: input.enabled ?? true,
      maintenanceUntilMs,
    };
  }

  private transaction<T>(work: () => T): T {
    this.db.exec("BEGIN IMMEDIATE");
    try {
      const result = work();
      this.db.exec("COMMIT");
      return result;
    } catch (error) {
      try {
        this.db.exec("ROLLBACK");
      } catch {
        // Preserve the original failure.
      }
      throw error;
    }
  }
}

function boundedText(
  value: unknown,
  field: string,
  max: number,
  allowSpaces = true,
  allowEmpty = false,
): string {
  if (typeof value !== "string") throw new AlertValidationError(`${field} must be a string`);
  const result = value.trim();
  if (result.length < (allowEmpty ? 0 : 1) || result.length > max) {
    throw new AlertValidationError(`${field} length is invalid`);
  }
  if (/[\u0000-\u001f\u007f]/.test(result) || (!allowSpaces && /\s/.test(result))) {
    throw new AlertValidationError(`${field} contains invalid characters`);
  }
  return result;
}

function boundedCount(value: number | undefined, fallback: number, min = 1, max = Number.MAX_SAFE_INTEGER): number {
  if (value === undefined) return fallback;
  if (!Number.isInteger(value) || value < min || value > max) return fallback;
  return value;
}
