/**
 * Unit tests for the shared wire contracts (src/contracts).
 * Run via the Node test runner through tsx: tests/*.test.ts.
 */

import { strict as assert } from "node:assert";
import { describe, it } from "node:test";
import {
  COLLECTOR_PROTOCOL_VERSION,
  MAX_BATCH_RECORDS,
  MAX_EXPECTED_INTERVAL_MS,
  MAX_MESSAGE_BYTES,
  METRIC_EXTENSION_NAMES,
  MIN_EXPECTED_INTERVAL_MS,
  collectorSupportsMetricExtensions,
  projectTelemetryRecordForWire,
  type HelloMessage,
  type MetricsRecord,
  type TelemetryRecord,
  parseCollectorMessage,
  parseHubMessage,
  parseHubSnapshot,
  parseTelemetryRecord,
} from "../src/contracts/index";

function validHello(): HelloMessage {
  return {
    type: "hello",
    collectorId: "collector-a",
    runId: "run-1",
    bootId: "boot-123",
    hostname: "mock-machine",
    capabilities: ["metrics"],
    expectedIntervalMs: 5_000,
    collectorVersion: "test-1",
  };
}

function validMetricsRecord(overrides: Partial<MetricsRecord> = {}): MetricsRecord {
  return {
    collectorId: "collector-a",
    runId: "run-1",
    bootId: "boot-123",
    sequence: 1,
    capturedAt: new Date().toISOString(),
    kind: "metrics",
    payload: {
      cpu: { status: "ok", usagePercent: 12.5, perCore: [12.5] },
      memory: { status: "ok", usedBytes: 4, totalBytes: 8 },
      swap: { status: "unavailable", reason: "no swap" },
      gpus: { status: "unavailable", reason: "no GPUs detected" },
      disks: { status: "ok", filesystems: [], devices: [] },
      network: { status: "ok", interfaces: [] },
    },
    ...overrides,
  };
}

const expects = (body: () => unknown, message: string): void => {
  assert.throws(() => void body(), (error: unknown) => error instanceof Error && error.message.includes(message));
};

describe("contract constants", () => {
  it("exposes the message bounds", () => {
    assert.equal(MAX_MESSAGE_BYTES, 262_144);
    assert.equal(MAX_BATCH_RECORDS, 32);
  });

  it("exposes the advertised-cadence bounds", () => {
    assert.equal(MIN_EXPECTED_INTERVAL_MS, 100);
    assert.equal(MAX_EXPECTED_INTERVAL_MS, 7_200_000);
  });
});

describe("parseCollectorMessage: hello", () => {
  it("accepts a valid hello", () => {
    const hello = parseCollectorMessage(JSON.parse(JSON.stringify(validHello())));
    assert.equal(hello.type, "hello");
    assert.equal(hello.collectorId, "collector-a");
    assert.deepEqual(hello.capabilities, ["metrics"]);
  });

  it("rejects unexpected fields instead of stripping them", () => {
    expects(
      () => parseCollectorMessage({ ...validHello(), extra: 1 }),
      'unexpected field "extra"',
    );
  });

  it("rejects messages that still carry a removed protocolVersion field", () => {
    expects(() => parseCollectorMessage({ ...validHello(), protocolVersion: 1 }), 'unexpected field "protocolVersion"');
    expects(() => parseCollectorMessage({ ...validHello(), protocolVersion: 999 }), 'unexpected field "protocolVersion"');
  });

  it("rejects missing fields", () => {
    const { runId: _dropped, ...rest } = validHello();
    expects(() => parseCollectorMessage(rest), 'missing field "runId"');
  });

  it("rejects invalid collector identity fields", () => {
    expects(() => parseCollectorMessage({ ...validHello(), collectorId: "" }), "collectorId");
    expects(() => parseCollectorMessage({ ...validHello(), runId: 7 }), "runId");
    expects(() => parseCollectorMessage({ ...validHello(), collectorVersion: "" }), "collectorVersion");
  });

  it("rejects empty or duplicate capabilities and requires metrics", () => {
    expects(() => parseCollectorMessage({ ...validHello(), capabilities: ["metrics", "metrics"] }), "capabilities");
    expects(() => parseCollectorMessage({ ...validHello(), capabilities: [] }), "capabilities");
    expects(() => parseCollectorMessage({ ...validHello(), capabilities: ["inventory"] }), "capabilities");
    expects(() => parseCollectorMessage({ ...validHello(), capabilities: ["summary"] }), "capabilities");
  });

  it("rejects non-object payloads", () => {
    expects(() => parseCollectorMessage("hello"), "expected object");
    expects(() => parseCollectorMessage(null), "expected object");
  });
});

describe("parseCollectorMessage: hello expectedIntervalMs", () => {
  it("accepts bounded integers at both inclusive limits", () => {
    for (const interval of [MIN_EXPECTED_INTERVAL_MS, 600, 45_000, MAX_EXPECTED_INTERVAL_MS]) {
      const parsed = parseCollectorMessage({ ...validHello(), expectedIntervalMs: interval });
      assert.equal(parsed.type, "hello");
      assert.equal(parsed.expectedIntervalMs, interval);
    }
  });

  it("rejects explicit null, undefined, and non-integer values", () => {
    const bad = (value: unknown) =>
      parseCollectorMessage({ ...validHello(), expectedIntervalMs: value as number });
    // undefined degrades to a missing required field.
    expects(() => bad(undefined), "missing field \"expectedIntervalMs\"");
    expects(() => bad(null), "expectedIntervalMs");
    expects(() => bad("5000"), "expected safe integer");
    expects(() => bad(600.5), "safe integer");
    expects(() => bad(Number.NaN), "safe integer");
    expects(() => bad(Number.POSITIVE_INFINITY), "safe integer");
    expects(() => bad(true), "safe integer");
  });

  it("bounds the interval below minutes-to-hours limits", () => {
    const bad = (value: number) =>
      parseCollectorMessage({ ...validHello(), expectedIntervalMs: value });
    expects(() => bad(MIN_EXPECTED_INTERVAL_MS - 1), "outside 100..7200000");
    expects(() => bad(0), "outside 100..7200000");
    expects(() => bad(-600), "outside 100..7200000");
    expects(() => bad(MAX_EXPECTED_INTERVAL_MS + 1), "outside 100..7200000");
  });

  it("parse throws when the field is serialized as null on the wire", () => {
    expects(
      () => parseCollectorMessage(JSON.parse(JSON.stringify({ ...validHello(), expectedIntervalMs: null }))),
      "expectedIntervalMs",
    );
  });

  it("still rejects unexpected fields when the optional field is present", () => {
    expects(
      () => parseCollectorMessage({ ...validHello(), collectorVersion: "test-1", extra: 1 }),
      'unexpected field "extra"',
    );
  });
});

describe("parseCollectorMessage: samples", () => {
  it("accepts a valid one-record live batch", () => {
    const message = parseCollectorMessage({ type: "samples", mode: "live", records: [validMetricsRecord()] });
    assert.equal(message.type, "samples");
    assert.equal(message.mode, "live");
    assert.equal(message.records.length, 1);
    const first = message.records[0]!;
    assert.equal(first.kind, "metrics");
    assert.deepEqual((first as MetricsRecord).payload.cpu, { status: "ok", usagePercent: 12.5, perCore: [12.5] });
  });

  it("round-trips records through parseTelemetryRecord unchanged", () => {
    const record = validMetricsRecord();
    const parsed = parseTelemetryRecord(JSON.parse(JSON.stringify(record)));
    assert.deepEqual(parsed, record);
  });

  it("accepts cpu available + memory unavailable (missing metric semantics)", () => {
    const record = validMetricsRecord({
      payload: {
        cpu: { status: "ok", usagePercent: 0, perCore: [null] },
        memory: { status: "unavailable", reason: "permission denied" },
        swap: { status: "unavailable", reason: "no swap" },
        gpus: { status: "unavailable", reason: "no GPUs detected" },
        disks: { status: "ok", filesystems: [], devices: [] },
        network: { status: "ok", interfaces: [] },
      },
    });
    const message = parseCollectorMessage({ type: "samples", mode: "live", records: [record] });
    assert.equal(message.type, "samples");
    assert.deepEqual((message.records[0] as MetricsRecord).payload.memory, {
      status: "unavailable",
      reason: "permission denied",
    });
  });

  it("requires ok cpu to carry perCore readings", () => {
    const { perCore: _dropped, ...cpu } = validMetricsRecord().payload.cpu as { perCore: number[] };
    expects(
      () => parseTelemetryRecord({ ...validMetricsRecord(), payload: { ...validMetricsRecord().payload, cpu } }),
      'missing field "perCore"',
    );
  });

  it("bounds the batch size", () => {
    const tooLarge = Array.from({ length: MAX_BATCH_RECORDS + 1 }, (_, i) => validMetricsRecord({ sequence: i }));
    expects(
      () => parseCollectorMessage({ type: "samples", mode: "live", records: tooLarge }),
      "length 33 outside 1..32",
    );
    expects(() => parseCollectorMessage({ type: "samples", mode: "live", records: [] }), "length 0 outside 1..32");
    expects(() => parseCollectorMessage({ type: "samples", mode: "live", records: "x" }), "expected array");
    expects(() => parseTelemetryRecord("x"), "expected object");
  });

  it("rejects duplicate records within one batch", () => {
    const record = validMetricsRecord();
    expects(
      () => parseCollectorMessage({ type: "samples", mode: "live", records: [record, { ...record }] }),
      "duplicate record in batch",
    );
  });

  it("rejects cpu percentages outside 0..100 or nonnumbers", () => {
    const cpu = (usagePercent: unknown) =>
      parseTelemetryRecord({
        ...validMetricsRecord(),
        payload: { ...validMetricsRecord().payload, cpu: { status: "ok", usagePercent, perCore: [usagePercent] } },
      });
    expects(() => cpu(100.01), "usagePercent");
    expects(() => cpu(-0.001), "usagePercent");
    expects(() => cpu(Number.NaN), "finite number");
    expects(() => cpu(Number.POSITIVE_INFINITY), "finite number");
    expects(() => cpu("42"), "finite number");
  });

  it("rejects memory bounds violations and nonintegers", () => {
    const memory = (m: unknown) =>
      parseTelemetryRecord({ ...validMetricsRecord(), payload: { ...validMetricsRecord().payload, memory: m } });
    expects(() => memory({ status: "ok", usedBytes: 3, totalBytes: 2 }), "usedBytes exceeds totalBytes");
    expects(() => memory({ status: "ok", usedBytes: 1.5, totalBytes: 2 }), "safe integer");
    expects(() => memory({ status: "ok", usedBytes: -1, totalBytes: 2 }), "usedBytes");
    expects(() => memory({ status: "ok", usedBytes: 1 }), 'missing field "totalBytes"');
  });

  it("rejects stale or malformed timestamps", () => {
    expects(() => parseTelemetryRecord(validMetricsRecord({ capturedAt: "not-a-date" })), "parseable timestamp");
    expects(() => parseTelemetryRecord({ ...validMetricsRecord(), capturedAt: 0 }), "expected string");
    expects(
      () => parseTelemetryRecord(validMetricsRecord({ capturedAt: "1999-12-31T23:59:59Z" })),
      "parseable timestamp",
    );
  });

  it("rejects bad sequence numbers", () => {
    expects(() => parseTelemetryRecord(validMetricsRecord({ sequence: -1 })), "sequence");
    expects(() => parseTelemetryRecord(validMetricsRecord({ sequence: 0.5 })), "safe integer");
    expects(
      () => parseTelemetryRecord(validMetricsRecord({ sequence: Number.MAX_SAFE_INTEGER + 1 })),
      "safe integer",
    );
  });

  it("rejects unexpected per-record and payload fields", () => {
    const withExtra = validMetricsRecord();
    (withExtra as { extra?: unknown }).extra = true;
    expects(() => parseTelemetryRecord(withExtra), 'unexpected field "extra"');
    const badPayload = validMetricsRecord();
    Object.assign(badPayload.payload, { io: 1 });
    expects(() => parseTelemetryRecord(badPayload), 'unexpected field "io"');
  });

  it("rejects wrong kind and unsupported types", () => {
    expects(
      () => parseTelemetryRecord({ ...validMetricsRecord(), kind: "processes" }),
      "kind",
    );
    expects(() => parseCollectorMessage({ type: "detail.subscribe", subscriptionId: "s", kind: "processes", intervalMs: 1000, leaseMs: 10000 }), "unsupported type");
  });
});

describe("parseTelemetryRecord: optional bottleneck fields", () => {
  function bottleneckRecord(): MetricsRecord {
    const base = validMetricsRecord();
    return {
      ...base,
      payload: {
        ...base.payload,
        cpu: {
          status: "ok",
          usagePercent: 12.5,
          perCore: [12.5],
          temperatureC: 61.4,
          powerWatts: 88.2,
          iowaitPercent: 1.25,
          pressureSomePercent: 3.5,
        },
        memory: {
          status: "ok",
          usedBytes: 4,
          totalBytes: 8,
          pressureSomePercent: 1.5,
          pressureFullPercent: 0.5,
        },
        disks: {
          status: "ok",
          filesystems: [
            { id: "fs:/", status: "ok", usedBytes: 5, totalBytes: 10, inodesUsed: 10, inodesTotal: 100 },
          ],
          devices: [
            {
              id: "disk:sda",
              status: "ok",
              readBytesPerSec: 1,
              writeBytesPerSec: 2,
              readLatencyMs: 1.5,
              writeLatencyMs: 2.5,
              ioUtilizationPercent: 12.5,
            },
          ],
          pressureSomePercent: 2.5,
          pressureFullPercent: 1.5,
        },
      },
    };
  }

  it("round-trips the new fields and keeps absent fields absent", () => {
    const record = bottleneckRecord();
    const parsed = parseTelemetryRecord(JSON.parse(JSON.stringify(record)));
    assert.deepEqual(parsed, record);

    const old = parseTelemetryRecord(JSON.parse(JSON.stringify(validMetricsRecord()))) as MetricsRecord;
    for (const field of ["temperatureC", "powerWatts", "iowaitPercent", "pressureSomePercent"]) {
      assert.equal(Object.hasOwn(old.payload.cpu, field), false, `old cpu record must not gain ${field}`);
    }
    assert.equal(Object.hasOwn(old.payload.memory, "pressureSomePercent"), false);
    assert.equal(Object.hasOwn(old.payload.disks, "pressureSomePercent"), false);
  });

  it("accepts explicit null for every optional bottleneck field", () => {
    const record = bottleneckRecord();
    if (record.payload.cpu.status !== "ok" || record.payload.memory.status !== "ok" || record.payload.disks.status !== "ok") {
      throw new Error("fixture must be available");
    }
    record.payload.cpu.temperatureC = null;
    record.payload.cpu.powerWatts = null;
    record.payload.cpu.iowaitPercent = null;
    record.payload.cpu.pressureSomePercent = null;
    record.payload.memory.pressureSomePercent = null;
    record.payload.memory.pressureFullPercent = null;
    record.payload.disks.pressureSomePercent = null;
    record.payload.disks.pressureFullPercent = null;
    const fs = record.payload.disks.filesystems[0]!;
    if (fs.status !== "ok") throw new Error("fixture must be available");
    fs.inodesUsed = null;
    fs.inodesTotal = null;
    const disk = record.payload.disks.devices[0]!;
    if (disk.status !== "ok") throw new Error("fixture must be available");
    disk.readLatencyMs = null;
    disk.writeLatencyMs = null;
    disk.ioUtilizationPercent = null;
    assert.deepEqual(parseTelemetryRecord(record), record);
  });

  it("rejects out-of-range optional values instead of clamping them", () => {
    const withCpu = (changes: Record<string, unknown>) => {
      const record = bottleneckRecord();
      Object.assign(record.payload.cpu, changes);
      return () => parseTelemetryRecord(record);
    };
    expects(withCpu({ temperatureC: 201 }), "temperatureC");
    expects(withCpu({ powerWatts: -0.01 }), "powerWatts");
    expects(withCpu({ iowaitPercent: 100.01 }), "iowaitPercent");
    expects(withCpu({ pressureSomePercent: Number.NaN }), "finite number");

    const withDisk = (changes: Record<string, unknown>) => {
      const record = bottleneckRecord();
      if (record.payload.disks.status !== "ok") throw new Error("fixture must be available");
      Object.assign(record.payload.disks.devices[0], changes);
      return () => parseTelemetryRecord(record);
    };
    expects(withDisk({ readLatencyMs: -1 }), "readLatencyMs");
    expects(withDisk({ ioUtilizationPercent: 101 }), "ioUtilizationPercent");

    const record = bottleneckRecord();
    if (record.payload.disks.status !== "ok") throw new Error("fixture must be available");
    Object.assign(record.payload.disks.filesystems[0], { inodesUsed: 101 });
    expects(() => parseTelemetryRecord(record), "inodesUsed exceeds inodesTotal");
  });

  it("does not allow memory pressure fields on swap", () => {
    const record = validMetricsRecord({
      payload: {
        ...validMetricsRecord().payload,
        swap: { status: "ok", usedBytes: 1, totalBytes: 2, pressureSomePercent: 1 } as never,
      },
    });
    expects(() => parseTelemetryRecord(record), 'unexpected field "pressureSomePercent"');
  });
});

describe("parseHubMessage", () => {
  it("accepts a valid durable welcome", () => {
    const welcome = parseHubMessage({
      type: "welcome",
      hubRunId: "hub-run-1",
      machineId: "machine-1",
      delivery: "durable",
      maxInFlightBatches: 8,
    });
    assert.ok(welcome.type === "welcome");
    assert.equal(welcome.delivery, "durable");
    assert.equal(welcome.machineId, "machine-1");
    assert.equal(welcome.maxInFlightBatches, 8);
    assert.equal(Object.hasOwn(welcome, "metricExtensions"), false, "old welcome shape stays unchanged");
  });

  it("accepts the optional negotiated metric extensions", () => {
    const welcome = parseHubMessage({
      type: "welcome",
      hubRunId: "hub-run-1",
      machineId: "machine-1",
      delivery: "durable",
      maxInFlightBatches: 8,
      metricExtensions: [...METRIC_EXTENSION_NAMES],
    });
    assert.ok(welcome.type === "welcome");
    assert.deepEqual(welcome.metricExtensions, [...METRIC_EXTENSION_NAMES]);
  });

  it("keeps metricExtensions strict and bounded", () => {
    const welcome = (metricExtensions: unknown) =>
      parseHubMessage({
        type: "welcome",
        hubRunId: "hub-run-1",
        machineId: "machine-1",
        delivery: "durable",
        maxInFlightBatches: 8,
        metricExtensions,
      });
    expects(() => welcome([]), "outside 1..1");
    expects(() => welcome(["bottleneck-v1", "bottleneck-v1"]), "outside 1..1");
    expects(() => welcome(["unknown-extension"]), "unexpected value");
    expects(() => welcome(["bottleneck-v1", "unknown-extension"]), "outside 1..1");
    expects(() => welcome("bottleneck-v1"), "expected array");
    expects(() => welcome(null), "expected array");
  });

    it("rejects unexpected fields and unsupported types", () => {
    expects(
      () =>
        parseHubMessage({
          type: "welcome",
          hubRunId: "hub-run-1",
          machineId: "machine-1",
          delivery: "durable",
          maxInFlightBatches: 8,
          acked: 7,
        }),
      'unexpected field "acked"',
    );
    expects(
      () =>
        parseHubMessage({
          type: "welcome",
          hubRunId: "hub-run-1",
          machineId: "machine-1",
          delivery: "ephemeral",
          maxInFlightBatches: 8,
        }),
      "delivery",
    );
    expects(
      () =>
        parseHubMessage({
          type: "detail.result",
          subscriptionId: "sub-1",
          capturedAt: new Date().toISOString(),
          snapshot: { totalProcesses: 0, truncated: false, inaccessible: 0, processes: [] },
        }),
      "unsupported type",
    );
  });

  it("rejects a welcome that still carries a removed protocolVersion field", () => {
    expects(
      () =>
        parseHubMessage({
          type: "welcome",
          protocolVersion: 2,
          hubRunId: "hub-run-1",
          machineId: "machine-1",
          delivery: "durable",
          maxInFlightBatches: 8,
        }),
      'unexpected field "protocolVersion"',
    );
  });
});

describe("mixed-version metric-extension negotiation", () => {
  function extendedRecord(): MetricsRecord {
    const base = validMetricsRecord();
    return {
      ...base,
      payload: {
        ...base.payload,
        cpu: {
          status: "ok",
          usagePercent: 12.5,
          perCore: [12.5],
          temperatureC: 61.4,
          powerWatts: 88.2,
          iowaitPercent: 1.25,
          pressureSomePercent: 3.5,
        },
        memory: {
          status: "ok",
          usedBytes: 4,
          totalBytes: 8,
          pressureSomePercent: 1.5,
          pressureFullPercent: 0.5,
        },
        disks: {
          status: "ok",
          filesystems: [
            { id: "fs:/", status: "ok", usedBytes: 5, totalBytes: 10, inodesUsed: 10, inodesTotal: 100 },
          ],
          devices: [
            {
              id: "disk:sda",
              status: "ok",
              readBytesPerSec: 1,
              writeBytesPerSec: 2,
              readLatencyMs: 1.5,
              writeLatencyMs: 2.5,
              ioUtilizationPercent: 12.5,
            },
          ],
          pressureSomePercent: 2.5,
          pressureFullPercent: 1.5,
        },
      },
    };
  }

  it("matches only the exact known protocol version marker", () => {
    assert.equal(COLLECTOR_PROTOCOL_VERSION, "0.3.0");
    assert.equal(collectorSupportsMetricExtensions(COLLECTOR_PROTOCOL_VERSION), true);
    for (const version of ["0.2.0", "0.3", "0.3.0.0", "0.3.0-beta", "test-1", "", " 0.3.0"]) {
      assert.equal(collectorSupportsMetricExtensions(version), false, `must not guess ${JSON.stringify(version)}`);
    }
  });

  it("projects an extended metrics record to the base wire shape without mutating it", () => {
    const record = extendedRecord();
    const original = structuredClone(record);
    const projected = projectTelemetryRecordForWire(record, []) as MetricsRecord;
    assert.deepEqual(projected, {
      ...record,
      payload: {
        cpu: { status: "ok", usagePercent: 12.5, perCore: [12.5] },
        memory: { status: "ok", usedBytes: 4, totalBytes: 8 },
        swap: record.payload.swap,
        gpus: record.payload.gpus,
        disks: {
          status: "ok",
          filesystems: [{ id: "fs:/", status: "ok", usedBytes: 5, totalBytes: 10 }],
          devices: [{ id: "disk:sda", status: "ok", readBytesPerSec: 1, writeBytesPerSec: 2 }],
        },
        network: record.payload.network,
      },
    });
    assert.deepEqual(record, original, "durable record stays extended");

    // A second projection is idempotent and still leaves the source alone.
    assert.deepEqual(projectTelemetryRecordForWire(projected, []), projected);
    assert.deepEqual(record, original);
  });

  it("removes explicitly null optional fields as well as present values", () => {
    const record = extendedRecord();
    if (record.payload.cpu.status !== "ok" || record.payload.memory.status !== "ok" || record.payload.disks.status !== "ok") {
      throw new Error("fixture must be available");
    }
    record.payload.cpu.temperatureC = null;
    record.payload.memory.pressureFullPercent = null;
    const filesystem = record.payload.disks.filesystems[0]!;
    if (filesystem.status !== "ok") throw new Error("fixture must be available");
    filesystem.inodesUsed = null;
    const projected = projectTelemetryRecordForWire(record, []) as MetricsRecord;
    if (projected.payload.cpu.status !== "ok" || projected.payload.memory.status !== "ok" || projected.payload.disks.status !== "ok") {
      throw new Error("projection must keep the family available");
    }
    assert.equal(Object.hasOwn(projected.payload.cpu, "temperatureC"), false);
    assert.equal(Object.hasOwn(projected.payload.memory, "pressureFullPercent"), false);
    assert.equal(Object.hasOwn(projected.payload.disks.filesystems[0]!, "inodesUsed"), false);
  });

  it("preserves the extended fields for a bottleneck-v1 peer", () => {
    const record = extendedRecord();
    assert.equal(projectTelemetryRecordForWire(record, ["bottleneck-v1"]), record);
  });

  it("passes gap records through unchanged for base peers", () => {
    const gap: TelemetryRecord = {
      collectorId: "collector-a",
      runId: "run-1",
      bootId: "boot-123",
      sequence: 2,
      capturedAt: new Date().toISOString(),
      kind: "gap",
      payload: {
        fromCapturedAt: new Date().toISOString(),
        toCapturedAt: new Date().toISOString(),
        droppedRecords: 3,
        reason: "outbox-bytes",
      },
    };
    assert.equal(projectTelemetryRecordForWire(gap, []), gap);
  });
});

describe("parseHubSnapshot", () => {
  const machine = {
    machineId: "machine-1",
    collectorId: "collector-1",
    hostname: "rig-1",
    connected: true,
    lastReceivedAt: null,
    summary: null,
  };

  it("accepts versionless rows without optional fields and keeps the keys absent", () => {
    const row = {
      machineId: "machine-1",
      collectorId: "collector-1",
      hostname: "rig-1",
      connected: true,
      lastReceivedAt: null,
    };
    const snapshot = parseHubSnapshot({ hubRunId: "hub-run-1", machines: [row] });
    assert.deepEqual(snapshot.machines, [row]);
    assert.equal(Object.hasOwn(snapshot.machines[0] ?? {}, "summary"), false);
  });

  it("rejects machine rows that still carry the removed summary field", () => {
    expects(
      () => parseHubSnapshot({ hubRunId: "hub-run-1", machines: [machine] }),
      'unexpected field "summary"',
    );
  });

  it("rejects machine rows that still carry a removed protocolVersion field", () => {
    expects(
      () =>
        parseHubSnapshot({
          hubRunId: "hub-run-1",
          machines: [
            {
              machineId: "machine-2",
              collectorId: "collector-2",
              hostname: "rig-2",
              connected: true,
              lastReceivedAt: null,
              protocolVersion: 2,
            },
          ],
        }),
      'unexpected field "protocolVersion"',
    );
  });

  describe("expectedIntervalMs", () => {
    const base = {
      machineId: "machine-1",
      collectorId: "collector-1",
      hostname: "rig-1",
      connected: true,
      lastReceivedAt: null,
    };

    it("accepts present null and bounded integers", () => {
      for (const interval of [null, MIN_EXPECTED_INTERVAL_MS, 600, MAX_EXPECTED_INTERVAL_MS]) {
        const snapshot = parseHubSnapshot({
          hubRunId: "hub-run-1",
          machines: [{ ...base, expectedIntervalMs: interval }],
        });
        assert.equal(snapshot.machines[0]?.expectedIntervalMs, interval);
      }
    });

    it("rejects malformed values", () => {
      const bad = (interval: unknown) =>
        parseHubSnapshot({
          hubRunId: "hub-run-1",
          machines: [{ ...base, expectedIntervalMs: interval }],
        });
      expects(() => bad(undefined), "must not be undefined");
      expects(() => bad("600"), "must be a safe integer");
      expects(() => bad(600.5), "safe integer");
      expects(() => bad(MIN_EXPECTED_INTERVAL_MS - 1), "outside 100..7200000");
      expects(() => bad(MAX_EXPECTED_INTERVAL_MS + 1), "outside 100..7200000");
      expects(() => bad(Number.NaN), "safe integer");
      expects(() => bad(true), "safe integer");
    });

    it("still rejects unexpected fields strictly", () => {
      expects(
        () =>
          parseHubSnapshot({
            hubRunId: "hub-run-1",
            machines: [{ ...base, expectedIntervalMs: 600, cadence2: 7 }],
          }),
        'unexpected field "cadence2"',
      );
      expects(
        () =>
          parseHubSnapshot({
            hubRunId: "hub-run-1",
            cadence: 600,
            machines: [base],
          }),
        'unexpected field "cadence"',
      );
    });
  });

  describe("metrics", () => {
    it("accepts a machine row with received metrics", () => {
      const record = validMetricsRecord({ collectorId: "collector-1" });
      const receivedAt = "2026-11-02T12:00:00.000Z";
      const snapshot = parseHubSnapshot({
        hubRunId: "hub-run-1",
        machines: [
          {
            machineId: "machine-1",
            collectorId: "collector-1",
            hostname: "rig-1",
            connected: true,
            lastReceivedAt: receivedAt,
            metrics: { ...record, receivedAt },
          },
        ],
      });
      assert.deepEqual(snapshot.machines[0]?.metrics, { ...record, receivedAt });
    });

    it("rejects metrics whose collectorId conflicts with the row", () => {
      const record = validMetricsRecord({ collectorId: "collector-other" });
      const receivedAt = "2026-11-02T12:00:00.000Z";
      expects(
        () =>
          parseHubSnapshot({
            hubRunId: "hub-run-1",
            machines: [
              {
                machineId: "machine-1",
                collectorId: "collector-1",
                hostname: "rig-1",
                connected: true,
                lastReceivedAt: receivedAt,
                metrics: { ...record, receivedAt },
              },
            ],
          }),
        "conflicting machine metrics",
      );
    });

    it("rejects a metrics field whose record is not a metrics record", () => {
      expects(
        () =>
          parseHubSnapshot({
            hubRunId: "hub-run-1",
            machines: [
              {
                machineId: "machine-1",
                collectorId: "collector-1",
                hostname: "rig-1",
                connected: true,
                lastReceivedAt: "2026-11-02T12:00:00.000Z",
                metrics: { sequence: 1 },
              },
            ],
          }),
        "invalid",
      );
    });
  });
});
