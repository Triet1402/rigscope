/**
 * Browser-safe alert domain types shared by the hub engine, HTTP API and UI.
 *
 * No Node built-ins live here. Timestamps crossing the HTTP boundary are ISO
 * strings; the engine keeps milliseconds internally.
 *
 * Notification semantics are at-least-once. A receiver may observe the same
 * deterministic job id twice after a network ambiguity; the id is an
 * idempotency key, not an exactly-once claim.
 */

export const ALERT_LIMITS = {
  maxRules: 200,
  maxIncidents: 1_000,
  maxJobs: 1_000,
  maxRuleName: 128,
  maxMachineId: 128,
  maxResourceId: 256,
  maxDurationMs: 86_400_000,
  maxThreshold: 1e13,
  maxWebhookUrl: 2_048,
  maxWebhookHeaders: 16,
  maxWebhookHeaderKey: 128,
  maxWebhookHeaderValue: 1_024,
  maxWebhookSecret: 256,
  maxListLimit: 500,
} as const;

export const ALERT_DELIVERY_DISCLOSURE =
  "Webhook delivery is at-least-once. After a network ambiguity the receiver may see the same deterministic job id more than once; retain it as an idempotency key.";

export type AlertCondition = "above" | "below" | "stale" | "unavailable";

export const ALERT_CONDITIONS: ReadonlyArray<{
  value: AlertCondition;
  label: string;
  needsThreshold: boolean;
}> = [
  {value: "above", label: "At or above threshold", needsThreshold: true},
  {value: "below", label: "At or below threshold", needsThreshold: true},
  {value: "stale", label: "No receipt within cadence threshold", needsThreshold: false},
  {value: "unavailable", label: "Reading unavailable", needsThreshold: false},
];

export type AlertMetricUnit =
  | "percent"
  | "bytes"
  | "bytes_per_second"
  | "celsius"
  | "watts"
  | "milliseconds";

export type AlertMetricResource = "none" | "required";

/**
 * Storage metric names and units follow the shared telemetry contract.
 * Percent values are stored on a 0–100 scale; derived percentages such as
 * `memory.usage` and `fs.usage` are computed by the collector/storage layer
 * from a positive total, so a rule threshold of 90 means 90%, not 0.9.
 */
export const ALERT_METRICS = {
  "cpu.usage": {label: "CPU usage", unit: "percent", resource: "none"},
  "cpu.core": {label: "CPU core usage", unit: "percent", resource: "required"},
  "cpu.temperature": {label: "CPU temperature", unit: "celsius", resource: "none"},
  "cpu.power": {label: "CPU package power", unit: "watts", resource: "none"},
  "cpu.iowait": {label: "CPU I/O wait", unit: "percent", resource: "none"},
  "cpu.pressure": {label: "CPU pressure", unit: "percent", resource: "none"},
  "memory.used": {label: "Memory used", unit: "bytes", resource: "none"},
  "memory.total": {label: "Memory total", unit: "bytes", resource: "none"},
  "memory.usage": {label: "Memory usage", unit: "percent", resource: "none"},
  "memory.pressure.some": {label: "Memory pressure (some)", unit: "percent", resource: "none"},
  "memory.pressure.full": {label: "Memory pressure (full)", unit: "percent", resource: "none"},
  "swap.used": {label: "Swap used", unit: "bytes", resource: "none"},
  "swap.total": {label: "Swap total", unit: "bytes", resource: "none"},
  "gpu.utilization": {label: "GPU utilization", unit: "percent", resource: "required"},
  "gpu.memory.used": {label: "GPU memory used", unit: "bytes", resource: "required"},
  "gpu.memory.total": {label: "GPU memory total", unit: "bytes", resource: "required"},
  "gpu.temperature": {label: "GPU temperature", unit: "celsius", resource: "required"},
  "gpu.power": {label: "GPU power", unit: "watts", resource: "required"},
  "io.pressure.some": {label: "I/O pressure (some)", unit: "percent", resource: "none"},
  "io.pressure.full": {label: "I/O pressure (full)", unit: "percent", resource: "none"},
  "fs.used": {label: "Filesystem used", unit: "bytes", resource: "required"},
  "fs.total": {label: "Filesystem total", unit: "bytes", resource: "required"},
  "fs.usage": {label: "Filesystem usage", unit: "percent", resource: "required"},
  "fs.inodes.usage": {label: "Filesystem inode usage", unit: "percent", resource: "required"},
  "disk.read": {label: "Disk read", unit: "bytes_per_second", resource: "required"},
  "disk.write": {label: "Disk write", unit: "bytes_per_second", resource: "required"},
  "disk.read.latency": {label: "Disk read latency", unit: "milliseconds", resource: "required"},
  "disk.write.latency": {label: "Disk write latency", unit: "milliseconds", resource: "required"},
  "disk.utilization": {label: "Disk utilization", unit: "percent", resource: "required"},
  "net.rx": {label: "Network receive", unit: "bytes_per_second", resource: "required"},
  "net.tx": {label: "Network transmit", unit: "bytes_per_second", resource: "required"},
} as const satisfies Record<
  string,
  {label: string; unit: AlertMetricUnit; resource: AlertMetricResource}
>;

export type AlertMetric = keyof typeof ALERT_METRICS;

export function isAlertMetric(value: unknown): value is AlertMetric {
  return typeof value === "string" && Object.prototype.hasOwnProperty.call(ALERT_METRICS, value);
}

export function alertMetricUnit(metric: AlertMetric): AlertMetricUnit {
  return ALERT_METRICS[metric].unit;
}

export function alertMetricNeedsResource(metric: AlertMetric): boolean {
  return ALERT_METRICS[metric].resource === "required";
}

/** Payload family that supplies a metric; I/O pressure belongs to the disks family. */
export type AlertMetricFamily = "cpu" | "memory" | "swap" | "gpus" | "disks" | "network";

export function alertMetricFamily(metric: AlertMetric): AlertMetricFamily {
  if (metric.startsWith("cpu.")) return "cpu";
  if (metric.startsWith("memory.")) return "memory";
  if (metric.startsWith("swap.")) return "swap";
  if (metric.startsWith("gpu.")) return "gpus";
  if (metric.startsWith("io.") || metric.startsWith("fs.") || metric.startsWith("disk.")) {
    return "disks";
  }
  return "network";
}

/** Stale threshold: max(15 s, 3 × advertised delivery interval). */
export function alertStaleThresholdMs(expectedIntervalMs: number | null | undefined): number {
  if (
    expectedIntervalMs === null ||
    expectedIntervalMs === undefined ||
    !Number.isFinite(expectedIntervalMs) ||
    expectedIntervalMs <= 0
  ) {
    return 15_000;
  }
  return Math.max(15_000, 3 * expectedIntervalMs);
}

export type AlertMachineScope = "*" | readonly string[];

export function alertScopeIncludes(scope: AlertMachineScope, machineId: string): boolean {
  return scope === "*" || scope.includes(machineId);
}

export type AlertRuleState = "ok" | "pending" | "firing";

export type AlertRule = {
  id: string;
  name: string;
  machineId: string;
  metric: AlertMetric;
  /** Empty for single-resource metrics; e.g. `gpu:GPU-…`, `core:3`, `fs:/`, `disk:sda`, `net:eno1`. */
  resourceId: string;
  condition: AlertCondition;
  threshold: number | null;
  durationMs: number;
  enabled: boolean;
  maintenanceUntil: string | null;
  state: AlertRuleState;
  pendingSince: string | null;
  activeIncidentId: string | null;
  lastValue: number | null;
  lastObservedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export type AlertRuleInput = {
  /** Present to update; omitted to create. */
  id?: string;
  name: string;
  machineId: string;
  metric: AlertMetric;
  resourceId?: string;
  condition: AlertCondition;
  threshold?: number | null;
  durationMs: number;
  enabled?: boolean;
  maintenanceUntil?: string | null;
};

export type AlertIncident = {
  id: string;
  ruleId: string;
  ruleName: string;
  machineId: string;
  metric: AlertMetric;
  resourceId: string;
  condition: AlertCondition;
  status: "open" | "recovered";
  startedAt: string;
  resolvedAt: string | null;
  startedValue: number | null;
  resolvedValue: number | null;
  lastObservedAt: string;
  message: string;
};

export type AlertEvent = "fired" | "recovered";

export type AlertJobStatus = "pending" | "delivered" | "failed";

export type AlertNotification = {
  /** Deterministic idempotency key sent with the webhook request. */
  id: string;
  ruleId: string;
  incidentId: string;
  event: AlertEvent;
  status: AlertJobStatus;
  attempts: number;
  maxAttempts: number;
  nextAttemptAt: string | null;
  lastAttemptAt: string | null;
  lastError: string | null;
  createdAt: string;
  deliveredAt: string | null;
};

export type AlertNotificationSummary = {
  pending: number;
  delivered: number;
  failed: number;
  recent: AlertNotification[];
  lastDeliveryAt: string | null;
  lastError: string | null;
  delivery: "at-least-once";
};

export type AlertWebhookTransport = "https" | "loopback-http";

export type AlertWebhookStatus = {
  channel: "webhook";
  configured: boolean;
  enabled: boolean;
  transport: AlertWebhookTransport | null;
  lastTestAt: string | null;
  lastTestOk: boolean | null;
  lastError: string | null;
  delivery: "at-least-once";
};

export type AlertWebhookConfigureInput = {
  url: string;
  headers?: Record<string, string>;
  secret?: string | null;
  enabled?: boolean;
};

export type AlertWebhookTestResult = {
  ok: boolean;
  status: number | null;
  error: string | null;
  testedAt: string;
};

export type AlertListResponse = {
  generatedAt: string;
  canAdmin: boolean;
  rules: AlertRule[];
  incidents: AlertIncident[];
  notifications: AlertNotificationSummary;
  webhook: AlertWebhookStatus;
  limits: typeof ALERT_LIMITS;
  deliveryDisclosure: string;
};

export type AlertRuleResponse = {rule: AlertRule};

export type AlertWebhookResponse = {status: AlertWebhookStatus};

export type AlertWebhookTestResponse = {
  result: AlertWebhookTestResult;
  status: AlertWebhookStatus;
};

export type AlertApiError = {error: string};
