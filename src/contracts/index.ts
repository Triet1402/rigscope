/**
 * Shared browser-side state contract.
 *
 * Browser-safe: no Node built-ins, no environment assumptions, and no runtime
 * dependencies. Re-exports the wire contract from ./telemetry and defines the
 * machine snapshot types the hub serves to browsers over SSE.
 *
 * Parsers are strict by design: they reject unexpected fields (rather than
 * stripping them) so that both hub and consumer fail fast on contract drift
 * instead of silently accepting data they do not understand.
 */

import { MIN_EXPECTED_INTERVAL_MS, MAX_EXPECTED_INTERVAL_MS, parseCollectorMessage, parseInventory, parseTelemetryRecord, type Inventory, type MetricsRecord, type StatusMessage } from "./telemetry";

export * from "./telemetry";

export type MachineState = {
  machineId: string;
  collectorId: string;
  hostname: string;
  connected: boolean;
  lastReceivedAt: string | null;
  /**
   * Nominal delivery cadence advertised in the machine's hello (ms), or null
   * before a hello. Optional in the type so hand-built snapshot fixtures stay
   * valid; the hub emits it on every machine row.
   */
  expectedIntervalMs?: number | null;
  metrics?: MetricsRecord & { receivedAt: string };
  inventory?: Inventory;
  delivery?: StatusMessage;
  displayName?: string | null;
  tags?: string[];
};

export type HubSnapshot = { hubRunId: string; machines: MachineState[] };

/** Browser stream snapshots use the same validated measurement contract. */
export function parseHubSnapshot(value: unknown): HubSnapshot {
  const obj = expectObject(value, "hub snapshot");
  expectKeys(obj, ["hubRunId", "machines"], "hub snapshot");
  const hubRunId = parseIdField(obj.hubRunId, "hubRunId", "hub snapshot");
  if (!Array.isArray(obj.machines)) throw new Error("invalid snapshot machines");
  const ids = new Set<string>();
  const machines = obj.machines.map((value): MachineState => {
    const row = expectObject(value, "machine");
    expectKeys(
      row,
      ["machineId", "collectorId", "hostname", "connected", "lastReceivedAt"],
      "machine",
      ["expectedIntervalMs", "metrics", "inventory", "delivery", "displayName", "tags"],
    );
    const machineId = parseIdField(row.machineId, "machineId", "machine");
    if (ids.has(machineId)) throw new Error("duplicate snapshot machine");
    ids.add(machineId);
    const collectorId = parseIdField(row.collectorId, "collectorId", "machine");
    const hostname = parseTextField(row.hostname, "hostname", "machine", 0, MAX_HOSTNAME_LENGTH);
    if (typeof row.connected !== "boolean") throw new Error("invalid machine connection state");
    const lastReceivedAt = row.lastReceivedAt === null ? null : parseTimestamp(row.lastReceivedAt, "lastReceivedAt", "machine");
    const extra: Partial<MachineState> = {};
    // Absent stays absent in the result so hand-built fixtures round-trip
    // unchanged; a present key must be null or a bounded integer.
    const expectedIntervalMs = hasOwn(row, "expectedIntervalMs")
      ? parseOptionalCadence(row.expectedIntervalMs, "expectedIntervalMs", "machine")
      : undefined;
    if (hasOwn(row, "metrics")) {
      const { receivedAt, ...record } = expectObject(row.metrics, "received metrics");
      const metrics = parseTelemetryRecord(record);
      if (metrics.kind !== "metrics" || metrics.collectorId !== collectorId) throw new Error("conflicting machine metrics");
      extra.metrics = { ...metrics, receivedAt: parseTimestamp(receivedAt, "receivedAt", "metrics") };
      if (extra.metrics.receivedAt !== lastReceivedAt) throw new Error("conflicting metrics receipt time");
    }
    if (hasOwn(row, "inventory")) extra.inventory = parseInventory(row.inventory);
    if (hasOwn(row, "delivery")) {
      const status = parseCollectorMessage(row.delivery);
      if (status.type !== "status") throw new Error("invalid delivery status");
      extra.delivery = status;
    }
    if (hasOwn(row, "displayName")) extra.displayName = row.displayName === null ? null : parseTextField(row.displayName, "displayName", "machine", 0, 128);
    if (hasOwn(row, "tags")) {
      if (!Array.isArray(row.tags) || row.tags.length > 32) throw new Error("invalid machine tags");
      extra.tags = row.tags.map(v => parseTextField(v, "tag", "machine", 1, 64));
    }
    return {
      machineId,
      collectorId,
      hostname,
      connected: row.connected,
      lastReceivedAt,
      ...(expectedIntervalMs === undefined ? {} : { expectedIntervalMs }),
      ...extra,
    };
  });
  return { hubRunId, machines };
}

// ---------------------------------------------------------------------------
// Parsing helpers (snapshot layer only; wire parsing lives in ./telemetry)
// ---------------------------------------------------------------------------

const MAX_HOSTNAME_LENGTH = 255;
const MAX_ID_LENGTH = 128;
const MAX_TIMESTAMP_LENGTH = 64;
// Wall-clock sanity window for timestamps; values outside it are typos
// or clock corruption rather than telemetry.
const MIN_TIMESTAMP_EPOCH = Date.parse("2000-01-01T00:00:00Z");
const MAX_TIMESTAMP_EPOCH = Date.parse("2200-01-01T00:00:00Z");

function expectObject(value: unknown, what: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`invalid ${what}: expected object, got ${describe(value)}`);
  }
  return value as Record<string, unknown>;
}

/**
 * Enforces an exact key set: unexpected fields are rejected (kept out of the
 * parsed result) and required fields must be present with defined values.
 * `optionalKeys` may be omitted entirely; when present they must still carry a
 * defined value — absent-but-allowed is detected by the caller via hasOwn
 * before this check runs.
 */
function expectKeys(
  obj: Record<string, unknown>,
  keys: readonly string[],
  what: string,
  optionalKeys: readonly string[] = [],
): void {
  const expected = new Set([...keys, ...optionalKeys]);
  for (const key of Object.keys(obj)) {
    if (!expected.has(key)) {
      throw new Error(`invalid ${what}: unexpected field "${key}"`);
    }
  }
  for (const key of keys) {
    if (obj[key] === undefined) {
      throw new Error(`invalid ${what}: missing field "${key}"`);
    }
  }
  // Optional keys may be absent, but an explicitly present undefined value is
  // a caller-side contract breach that JSON never produces.
  for (const key of optionalKeys) {
    if (hasOwn(obj, key) && obj[key] === undefined) {
      throw new Error(`invalid ${what}: ${key} must not be undefined`);
    }
  }
}

/** True only when the key exists on the object (a present key may be undefined). */
function hasOwn(obj: Record<string, unknown>, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(obj, key);
}

/**
 * Cadence field primitives. A hello must carry a bounded integer; a snapshot
 * machine row may additionally carry null, meaning the cadence is unknown
 * (before a hello). Neither is ever defaulted to an invented cadence, and the
 * upper bound cannot be exceeded.
 */
function parseOptionalCadence(value: unknown, field: string, what: string): number | null {
  if (value === null) return null;
  if (value === undefined) {
    throw new Error(`invalid ${what}: ${field} must not be undefined`);
  }
  return parseInteger(value, field, what, MIN_EXPECTED_INTERVAL_MS, MAX_EXPECTED_INTERVAL_MS);
}

function parseIdField(value: unknown, field: string, what: string): string {
  return parseTextField(value, field, what, 1, MAX_ID_LENGTH);
}

function parseTextField(
  value: unknown,
  field: string,
  what: string,
  minLength: number,
  maxLength: number,
): string {
  if (typeof value !== "string") {
    throw new Error(`invalid ${what}: ${field} must be a string, got ${describe(value)}`);
  }
  if (value.length < minLength || value.length > maxLength) {
    throw new Error(
      `invalid ${what}: ${field} length ${value.length} outside ${minLength}..${maxLength}`,
    );
  }
  return value;
}

function parseInteger(
  value: unknown,
  field: string,
  what: string,
  min: number,
  max: number,
): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value)) {
    throw new Error(
      `invalid ${what}: ${field} must be a safe integer, got ${describe(value)}`,
    );
  }
  if (value < min || value > max) {
    throw new Error(`invalid ${what}: ${field} ${value} outside ${min}..${max}`);
  }
  return value;
}

function parseTimestamp(value: unknown, field: string, what: string): string {
  const text = parseTextField(value, field, what, 1, MAX_TIMESTAMP_LENGTH);
  const epoch = Date.parse(text);
  if (!Number.isFinite(epoch) || epoch < MIN_TIMESTAMP_EPOCH || epoch > MAX_TIMESTAMP_EPOCH) {
    throw new Error(`invalid ${what}: ${field} is not a parseable timestamp`);
  }
  return text;
}

/** Renders an invalid value for error messages without throwing. */
function describe(value: unknown): string {
  try {
    const text = JSON.stringify(value);
    if (typeof text === "string") {
      return text.length <= 80 ? text : `${text.slice(0, 77)}...`;
    }
  } catch {
    // fall through
  }
  return String(value);
}
