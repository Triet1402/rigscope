import assert from "node:assert/strict";
import { SseClient, SseEvent } from "./sse";

export function assertExactKeys(value: unknown, expected: readonly string[], what: string): asserts value is Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${what} must be a JSON object (got ${JSON.stringify(value)})`);
  }
  assert.deepEqual([...Object.keys(value)].sort(), [...expected].sort(), `${what} field set must match the contract exactly`);
}

export function asFiniteInt(value: unknown, what: string): number {
  assert.equal(typeof value, "number", `${what} must be a number`);
  const asNumber = value as number;
  assert.ok(Number.isInteger(asNumber), `${what} must be an integer`);
  return asNumber;
}

export function asNonEmptyString(value: unknown, what: string): string {
  if (typeof value !== "string" || value.length === 0) {
    throw new Error(`${what} must be a non-empty string (got ${JSON.stringify(value)})`);
  }
  return value;
}

export function asBoolean(value: unknown, what: string): boolean {
  assert.equal(typeof value, "boolean", `${what} must be a boolean`);
  return value as boolean;
}

export function asTimestamp(value: unknown, what: string): string {
  assert.equal(typeof value, "string", `${what} must be a timestamp string`);
  const text = value as string;
  assert.ok(Number.isFinite(Date.parse(text)), `${what} must be a parseable timestamp`);
  return text;
}

export type CpuOk = { status: "ok"; usagePercent: number; perCore: (number | null)[] };

export type MemoryOk = { status: "ok"; usedBytes: number; totalBytes: number };

export type MetricUnavailable = { status: "unavailable"; reason: string };

export function assertCpuMetric(cpu: unknown, what: string): void {
  if ((cpu as { status?: string })?.status === "ok") {
    assertOptionalMetricKeys(cpu, ["status", "perCore", "usagePercent"],
      {temperatureC: [-50, 200], powerWatts: [0, 10000], iowaitPercent: [0, 100], pressureSomePercent: [0, 100]}, what);
    const usagePercent = (cpu as CpuOk).usagePercent;
    assert.equal(typeof usagePercent, "number", `${what}.usagePercent must be a number`);
    assert.ok(Number.isFinite(usagePercent) && usagePercent >= 0 && usagePercent <= 100, `${what}.usagePercent within 0..100`);
    const perCore = (cpu as CpuOk).perCore;
    assert.ok(Array.isArray(perCore), `${what}.perCore must be an array of core readings`);
    for (const value of perCore) {
      assert.ok(
        value === null || (typeof value === "number" && Number.isFinite(value) && value >= 0 && value <= 100),
        `${what}.perCore entries must be null or a 0..100 number`,
      );
    }
  } else {
    assertExactKeys(cpu, ["reason", "status"], `${what} (unavailable)`);
    asNonEmptyString((cpu as MetricUnavailable).reason, `${what}.reason`);
  }
}

export function assertMemoryMetric(memory: unknown, what: string): void {
  if ((memory as { status?: string })?.status === "ok") {
    assertOptionalMetricKeys(memory, ["status", "totalBytes", "usedBytes"],
      {pressureSomePercent: [0, 100], pressureFullPercent: [0, 100]}, what);
    const usedBytes = asFiniteInt((memory as MemoryOk).usedBytes, `${what}.usedBytes`);
    const totalBytes = asFiniteInt((memory as MemoryOk).totalBytes, `${what}.totalBytes`);
    assert.ok(usedBytes >= 0 && usedBytes <= totalBytes, `${what} requires 0 <= usedBytes <= totalBytes`);
  } else {
    assertExactKeys(memory, ["reason", "status"], `${what} (unavailable)`);
    asNonEmptyString((memory as MetricUnavailable).reason, `${what}.reason`);
  }
}

/** Optional fields remain independently checked; unknown wire keys still fail. */
function assertOptionalMetricKeys(value: unknown, required: readonly string[], bounds: Record<string, [number, number]>, what: string): void {
  const object = value as Record<string, unknown>;
  const present = Object.keys(bounds).filter(key => Object.hasOwn(object, key));
  assertExactKeys(value, [...required, ...present], `${what} (ok)`);
  for (const key of present) {
    const reading = object[key];
    const [min, max] = bounds[key];
    assert.ok(reading === null || (typeof reading === "number" && Number.isFinite(reading) && reading >= min && reading <= max),
      `${what}.${key} must be null or a finite value in ${min}..${max}`);
  }
}

export type MetricsRecordShape = {
  collectorId: string;
  runId: string;
  bootId: string;
  sequence: number;
  capturedAt: string;
  kind: "metrics";
  payload: {
    cpu: CpuOk | MetricUnavailable;
    memory: MemoryOk | MetricUnavailable;
    swap: MemoryOk | MetricUnavailable;
    gpus: unknown;
    disks: unknown;
    network: unknown;
  };
};

export const RECORD_FIELDS = ["bootId", "capturedAt", "collectorId", "kind", "payload", "runId", "sequence"] as const;

export const RECEIVED_RECORD_FIELDS = [...RECORD_FIELDS, "receivedAt"] as const;

const METRIC_FAMILIES = ["cpu", "disks", "gpus", "memory", "network", "swap"] as const;

export function assertSample(record: unknown, what: string): void {
  assertExactKeys(record, RECEIVED_RECORD_FIELDS, what);
  const metrics = record as MetricsRecordShape & { receivedAt: string };
  assert.equal(metrics.kind, "metrics", `${what}.kind`);
  asNonEmptyString(metrics.collectorId, `${what}.collectorId`);
  asNonEmptyString(metrics.runId, `${what}.runId`);
  asNonEmptyString(metrics.bootId, `${what}.bootId`);
  asFiniteInt(metrics.sequence, `${what}.sequence`);
  asTimestamp(metrics.capturedAt, `${what}.capturedAt`);
  asTimestamp(metrics.receivedAt, `${what}.receivedAt`);
  assertExactKeys(metrics.payload, METRIC_FAMILIES, `${what}.payload`);
  assertCpuMetric((metrics.payload as { cpu: unknown }).cpu, `${what}.payload.cpu`);
  assertMemoryMetric((metrics.payload as { memory: unknown }).memory, `${what}.payload.memory`);
}

export type ReceivedSampleShape = MetricsRecordShape & { receivedAt: string };

export type MachineStateShape = {
  machineId: string;
  collectorId: string;
  hostname: string;
  connected: boolean;
  expectedIntervalMs: number | null;
  lastReceivedAt: string | null;
  metrics: ReceivedSampleShape;
};

export type HubSnapshotShape = { hubRunId: string; machines: MachineStateShape[] };

export function parseSnapshot(data: string, what: string): HubSnapshotShape {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    throw new Error(`${what} data is not JSON: ${data.slice(0, 200)}`);
  }
  assertExactKeys(parsed, ["hubRunId", "machines"], what);
  const snapshot = parsed as HubSnapshotShape;
  asNonEmptyString(snapshot.hubRunId, `${what}.hubRunId`);
  assert.ok(Array.isArray(snapshot.machines), `${what}.machines must be an array`);
  snapshot.machines.forEach((machine, index) => {
    const prefix = `${what}.machines[${index}]`;
    assertExactKeys(
      machine,
      ["collectorId", "connected", "hostname", "lastReceivedAt", "machineId", "expectedIntervalMs",
        ...(Object.prototype.hasOwnProperty.call(machine, "metrics") ? ["metrics"] : []),
        ...(Object.prototype.hasOwnProperty.call(machine, "inventory") ? ["inventory"] : []),
        ...(Object.prototype.hasOwnProperty.call(machine, "delivery") ? ["delivery"] : []),
        ...(Object.prototype.hasOwnProperty.call(machine, "displayName") ? ["displayName"] : []),
        ...(Object.prototype.hasOwnProperty.call(machine, "tags") ? ["tags"] : [])],
      prefix,
    );
    asNonEmptyString(machine.machineId, `${prefix}.machineId`);
    asNonEmptyString(machine.collectorId, `${prefix}.collectorId`);
    if (typeof machine.hostname !== "string") {
      throw new Error(`${prefix}.hostname must be a string (got ${JSON.stringify(machine.hostname)})`);
    }
    if (machine.connected && machine.hostname.length === 0) {
      // hostname arrives with the hello message: empty is only acceptable pre-hello
      throw new Error(`${prefix}.hostname must be populated once connected`);
    }
    asBoolean(machine.connected, `${prefix}.connected`);
    if (machine.expectedIntervalMs !== null) {
      const interval = asFiniteInt(machine.expectedIntervalMs, `${prefix}.expectedIntervalMs`);
      assert.ok(interval >= 100 && interval <= 7_200_000, `${prefix}.expectedIntervalMs within 100..7200000`);
    }
    if (machine.lastReceivedAt !== null) asTimestamp(machine.lastReceivedAt, `${prefix}.lastReceivedAt`);
    if (Object.prototype.hasOwnProperty.call(machine, "metrics")) {
      assertSample(machine.metrics, `${prefix}.metrics`); // received records carry receivedAt
    }
  });
  return snapshot;
}

export type WelcomeShape = {
  type: "welcome";
  hubRunId: string;
  machineId: string;
  delivery: "durable" | "memory";
  maxInFlightBatches: number;
};

export function assertWelcome(raw: unknown, what: string): WelcomeShape {
  assertExactKeys(raw, ["delivery", "hubRunId", "machineId", "maxInFlightBatches", "type"], what);
  const welcome = raw as WelcomeShape;
  assert.equal(welcome.type, "welcome", `${what}.type`);
  assert.ok(
    welcome.delivery === "durable" || welcome.delivery === "memory",
    `${what}.delivery (durable or memory telemetry)`,
  );
  assert.ok(
    Number.isInteger(welcome.maxInFlightBatches) && welcome.maxInFlightBatches >= 1 && welcome.maxInFlightBatches <= 64,
    `${what}.maxInFlightBatches within 1..64`,
  );
  asNonEmptyString(welcome.hubRunId, `${what}.hubRunId`);
  asNonEmptyString(welcome.machineId, `${what}.machineId`);
  return welcome;
}

export type AckShape = {
  accepted: { runId: string; sequence: number }[];
  rejected: { runId: string; sequence: number; reason: string }[];
};

export function assertAck(raw: unknown, what: string): AckShape {
  assertExactKeys(raw, ["accepted", "rejected"], what);
  const ack = raw as AckShape;
  assert.ok(Array.isArray(ack.accepted), `${what}.accepted must be an array`);
  assert.ok(Array.isArray(ack.rejected), `${what}.rejected must be an array`);
  for (const ref of [...ack.accepted, ...ack.rejected]) {
    asNonEmptyString(ref.runId, `${what}.runId`);
    asFiniteInt(ref.sequence, `${what}.sequence`);
  }
  return ack;
}

export interface RawCollector {
  collectorId: string;
  runId: string;
  bootId: string;
  hostname: string;
  seq: number;
}

export function rawCollector(collectorId: string, hostname: string): RawCollector {
  return {
    collectorId,
    runId: `run-${Math.random().toString(16).slice(2, 10)}`,
    bootId: `boot-${Math.random().toString(16).slice(2, 10)}`,
    hostname,
    seq: 1,
  };
}

export function helloOf(c: RawCollector, expectedIntervalMs = 1_000): Record<string, unknown> {
  return {
    type: "hello",
    collectorId: c.collectorId,
    runId: c.runId,
    bootId: c.bootId,
    hostname: c.hostname,
    capabilities: ["metrics"],
    expectedIntervalMs,
  };
}

export function recordOf(c: RawCollector, seq: number): Record<string, unknown> {
  return {
    collectorId: c.collectorId,
    runId: c.runId,
    bootId: c.bootId,
    sequence: seq,
    capturedAt: new Date().toISOString(),
    kind: "metrics",
    payload: {
      cpu: { status: "ok", usagePercent: 12.5, perCore: [12.5] },
      memory: { status: "ok", usedBytes: 4_000_000_000, totalBytes: 16_000_000_000 },
      swap: { status: "ok", usedBytes: 0, totalBytes: 0 },
      gpus: { status: "unavailable", reason: "no GPUs detected" },
      disks: { status: "ok", filesystems: [], devices: [] },
      network: { status: "ok", interfaces: [] },
    },
  };
}

export function samplesMessage(mode: "live" | "replay", records: unknown[]): Record<string, unknown> {
  return { type: "samples", mode, records };
}

export type MachineExpectation = {
  machineId?: string;
  collectorId?: string;
  hostname?: string;
  connected?: boolean;
  withMetrics?: boolean;
};

export function findMachine(snapshot: HubSnapshotShape, expect: MachineExpectation): MachineStateShape | undefined {
  return snapshot.machines.find(
    (machine) =>
      (expect.machineId === undefined || machine.machineId === expect.machineId) &&
      (expect.collectorId === undefined || machine.collectorId === expect.collectorId) &&
      (expect.hostname === undefined || machine.hostname === expect.hostname) &&
      (expect.connected === undefined || machine.connected === expect.connected) &&
      (expect.withMetrics === undefined
        || (expect.withMetrics ? Object.prototype.hasOwnProperty.call(machine, "metrics") : true)),
  );
}

export async function waitMachineState(
  client: SseClient,
  deadlineMs: number,
  expect: MachineExpectation = {},
): Promise<{ snapshot: HubSnapshotShape; event: SseEvent }> {
  const end = Date.now() + deadlineMs;
  while (Date.now() < end) {
    const remaining = end - Date.now();
    if (remaining <= 0) break;
    const event = await client.next(remaining);
    if (event.event !== "snapshot" && event.event !== "update") continue;
    const snapshot = parseSnapshot(event.data, `SSE ${event.event} id=${event.id ?? "?"}`);
    if (findMachine(snapshot, expect) !== undefined) return { snapshot, event };
  }
  throw new Error(`timeout waiting for machine state under ${JSON.stringify(expect)}; last id ${client.lastEventId ?? "none"}`);
}
