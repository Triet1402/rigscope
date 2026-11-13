/**
 * Wire contract for the durable collector protocol.
 *
 * Browser-safe: no Node built-ins, no runtime dependencies. Parsers are strict
 * (unexpected fields are rejected) so both hub and collector fail fast on
 * contract drift instead of silently accepting data they do not understand.
 *
 * Delivery semantics:
 * - Every `samples` record carries (collectorId, runId, sequence); the triple is
 *   unique per collector. Sequence increases across record kinds within a run
 *   and may skip values (cadence filters, per-destination gap records).
 * - The hub acknowledges records only after a durable commit. Duplicates of a
 *   committed record are acknowledged as accepted. Records that must never be
 *   retried are listed in `rejected` with a reason.
 * - `mode: "live"` batches carry the newest sample(s) and may update latest
 *   state/alerts; `mode: "replay"` batches only fill history and must never
 *   update live state, appear fresh, or open incidents.
 * - `gap` records report intervals a destination dropped (outbox age/byte/disk
 *   budgets, write failures). They are acknowledged like samples.
 * - Process detail is request-scoped (`detail.*`) and never spooled.
 */

/** Max bytes for any message except detail results. */
export const MAX_MESSAGE_BYTES = 262_144;
/** Max bytes for a detail.result message (process tables). */
export const MAX_DETAIL_BYTES = 1_048_576;
export const MAX_BATCH_RECORDS = 32;
/**
 * Largest unacknowledged sample-batch window a hub may advertise. Existing
 * collectors reject a welcome above this bound, so it is the protocol ceiling;
 * the hub's persistent flush interval is bounded to fit it at the fastest
 * supported 100 ms cadence.
 */
export const MAX_IN_FLIGHT_BATCHES = 64;
/** Shared delivery bound used by hub flush configuration and collector live-slot reservation. */
export const MAX_PERSISTENT_FLUSH_MS = 5_000;
export const MAX_CORES = 1024;
export const MAX_GPUS = 16;
export const MAX_FILESYSTEMS = 64;
export const MAX_DISKS = 64;
export const MAX_INTERFACES = 64;
export const MAX_ADDRESSES_PER_INTERFACE = 32;
export const MAX_PROCESS_ROWS = 6000;
export const MAX_COLLECTION_FAILURES = 32;
export const MIN_DETAIL_INTERVAL_MS = 1_000;
export const MAX_DETAIL_INTERVAL_MS = 60_000;
export const MIN_DETAIL_LEASE_MS = 5_000;
export const MAX_DETAIL_LEASE_MS = 120_000;
/**
 * Declared delivery cadence bounds: collectors advertise the nominal interval
 * at which this destination receives samples in `hello.expectedIntervalMs`,
 * and the hub echoes that value per machine. The upper bound exceeds the
 * collector's 3_600_000 ms destination cap because delivery ticks are
 * quantized up to a multiple of the sampling interval.
 */
export const MIN_EXPECTED_INTERVAL_MS = 100;
export const MAX_EXPECTED_INTERVAL_MS = 7_200_000;

const MAX_ID = 128;
const MAX_RESOURCE_ID = 256;
const MAX_TEXT = 256;
const MAX_HOSTNAME = 255;
const MAX_TS = 64;
const MIN_EPOCH = Date.parse("2000-01-01T00:00:00Z");
const MAX_EPOCH = Date.parse("2200-01-01T00:00:00Z");

/**
 * The one timestamp grammar shared with the Go protocol package:
 * `YYYY-MM-DDTHH:MM:SS(.fraction of 1-9 digits)?(Z|±HH:MM)`, with valid
 * calendar and clock ranges and an offset within ±23:59. Fraction digits
 * beyond milliseconds are truncated, so ordering uses millisecond precision.
 */
const TIMESTAMP_PATTERN =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/;

/** Parses a wire timestamp to epoch milliseconds, or null when invalid. */
function timestampMillis(value: string): number | null {
  const match = TIMESTAMP_PATTERN.exec(value);
  if (match === null) return null;
  const year = Number(match[1]);
  const month = Number(match[2]);
  const day = Number(match[3]);
  const hour = Number(match[4]);
  const minute = Number(match[5]);
  const second = Number(match[6]);
  const fraction = match[7] ?? "";
  const zone = match[8];
  if (month < 1 || month > 12 || day < 1 || day > daysInMonth(year, month)) return null;
  if (hour > 23 || minute > 59 || second > 59) return null;
  const millis = fraction.length === 0 ? 0 : Number(`${fraction}000`.slice(0, 3));
  let offsetMinutes = 0;
  if (zone !== "Z") {
    const offsetHour = Number(zone.slice(1, 3));
    const offsetMinute = Number(zone.slice(4, 6));
    if (offsetHour > 23 || offsetMinute > 59) return null;
    offsetMinutes = (offsetHour * 60 + offsetMinute) * (zone[0] === "-" ? -1 : 1);
  }
  return Date.UTC(year, month - 1, day, hour, minute, second, millis) - offsetMinutes * 60_000;
}

/** Length of a Gregorian month. */
function daysInMonth(year: number, month: number): number {
  if (month === 2) return year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0) ? 29 : 28;
  return month === 4 || month === 6 || month === 9 || month === 11 ? 30 : 31;
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export type Unavailable = { status: "unavailable"; reason: string };

/** Capability names a collector may advertise in hello. */
export const CAPABILITY_NAMES = ["metrics", "inventory", "processes"] as const;
export type CapabilityName = (typeof CAPABILITY_NAMES)[number];

/** Metric families; also the keys of MetricsPayload and sharing policies. */
export const METRIC_FAMILIES = ["cpu", "memory", "swap", "gpus", "disks", "network"] as const;
export type MetricFamily = (typeof METRIC_FAMILIES)[number];

export type CpuMetric =
  | {
      status: "ok";
      /** 0..100, busy-counter delta over monotonic elapsed × logical cores. */
      usagePercent: number;
      /** Per logical core 0..100, index = core number; null when that core's counters failed. */
      perCore: (number | null)[];
      /** CPU package/Tctl/Tdie/core temperature; absent or null when unreadable. */
      temperatureC?: number | null;
      /** Top-level RAPL package power; absent or null when unreadable. */
      powerWatts?: number | null;
      /** 0..100 share of measured CPU capacity spent in iowait; absent/null when unavailable. */
      iowaitPercent?: number | null;
      /** Linux PSI cpu "some" avg10; absent or null when PSI is unavailable. */
      pressureSomePercent?: number | null;
    }
  | Unavailable;

export type BytesUsageMetric = { status: "ok"; usedBytes: number; totalBytes: number } | Unavailable;

/**
 * Memory keeps the bytes usage shape used by swap, plus optional PSI fields.
 * Missing pressure (kernel without PSI, unreadable file) is absent or null,
 * never a successful zero.
 */
export type MemoryMetric =
  | {
      status: "ok";
      usedBytes: number;
      totalBytes: number;
      pressureSomePercent?: number | null;
      pressureFullPercent?: number | null;
    }
  | Unavailable;

export type GpuReading =
  | {
      /** Stable resource id, e.g. "gpu:GPU-4ace4b62-..." (NVIDIA UUID). */
      id: string;
      status: "ok";
      utilizationPercent: number | null;
      memoryUsedBytes: number | null;
      memoryTotalBytes: number | null;
      temperatureC: number | null;
      powerWatts: number | null;
      /** Hottest die hotspot (junction) in °C; read from GPU registers. */
      junctionTemperatureC?: number | null;
      /** Hottest VRAM source in °C; read from GPU registers. */
      vramTemperatureC?: number | null;
    }
  | { id: string; status: "unavailable"; reason: string };

export type GpusMetric = { status: "ok"; devices: GpuReading[] } | Unavailable;

export type FilesystemReading =
  | {
      id: string;
      status: "ok";
      usedBytes: number;
      totalBytes: number;
      /** Allocated inode count and used inodes; absent/null when statfs lacks them. */
      inodesUsed?: number | null;
      inodesTotal?: number | null;
    }
  | { id: string; status: "unavailable"; reason: string };

export type DiskIoReading =
  | {
      id: string;
      status: "ok";
      readBytesPerSec: number;
      writeBytesPerSec: number;
      /** Mean completed-read latency over the interval; null when there were no reads. */
      readLatencyMs?: number | null;
      /** Mean completed-write latency over the interval; null when there were no writes. */
      writeLatencyMs?: number | null;
      /** 0..100 share of the interval the device had I/O in flight. */
      ioUtilizationPercent?: number | null;
    }
  | { id: string; status: "unavailable"; reason: string };

export type DisksMetric =
  | {
      status: "ok";
      filesystems: FilesystemReading[];
      devices: DiskIoReading[];
      /** Linux PSI io "some"/"full" avg10; absent or null when PSI is unavailable. */
      pressureSomePercent?: number | null;
      pressureFullPercent?: number | null;
    }
  | Unavailable;

export type InterfaceReading =
  | { id: string; status: "ok"; rxBytesPerSec: number; txBytesPerSec: number }
  | { id: string; status: "unavailable"; reason: string };

export type NetworkMetric = { status: "ok"; interfaces: InterfaceReading[] } | Unavailable;

/**
 * Canonical metrics payload. Every connection carries the full current field
 * set: optional readings (temperature, power, PSI, inode counts, I/O latency)
 * are present only when the collector can read them, with no per-connection
 * negotiation or base-vs-extended downgrade.
 */
export type MetricsPayload = {
  cpu: CpuMetric;
  memory: MemoryMetric;
  swap: BytesUsageMetric;
  gpus: GpusMetric;
  disks: DisksMetric;
  network: NetworkMetric;
};

export type GapReason = "outbox-age" | "outbox-bytes" | "disk-budget" | "write-failure" | "rejected";
export const GAP_REASONS: readonly GapReason[] = ["outbox-age", "outbox-bytes", "disk-budget", "write-failure", "rejected"];

export type GapPayload = {
  /** Capture time of the first dropped record (inclusive). */
  fromCapturedAt: string;
  /** Capture time of the last dropped record (inclusive). */
  toCapturedAt: string;
  droppedRecords: number;
  reason: GapReason;
};

type RecordEnvelope = {
  collectorId: string;
  runId: string;
  bootId: string;
  sequence: number;
  capturedAt: string;
};

export type MetricsRecord = RecordEnvelope & { kind: "metrics"; payload: MetricsPayload };
export type GapRecord = RecordEnvelope & { kind: "gap"; payload: GapPayload };
export type TelemetryRecord = MetricsRecord | GapRecord;

export type RecordRef = { runId: string; sequence: number };

export type CapabilityState = { status: "supported" | "unsupported" | "withheld"; reason?: string };

export type GpuInventory = {
  id: string;
  name: string;
  vendor: "nvidia" | "amd" | "intel" | "aspeed" | "other";
  pciBusId: string | null;
  /** "unsupported" for management adapters (e.g. ASPEED) without telemetry. */
  telemetry: "supported" | "unsupported";
  memoryTotalBytes: number | null;
  reason?: string;
};

export type FilesystemInventory = {
  id: string;
  mountPoint: string;
  device: string;
  fsType: string;
  totalBytes: number | null;
};

export type DiskInventory = {
  id: string;
  name: string;
  model: string | null;
  rotational: boolean | null;
  sizeBytes: number | null;
};

export type InterfaceAddress = { family: "ipv4" | "ipv6"; address: string; prefix: number };

export type InterfaceInventory = {
  id: string;
  name: string;
  mac: string | null;
  /** Empty when withheld by sharing policy. */
  addresses: InterfaceAddress[];
  virtual: boolean;
  state: "up" | "down" | "unknown";
  speedMbps: number | null;
};

export type Inventory = {
  os: { platform: string; release: string; distro: string | null; arch: string };
  cpu: { model: string | null; logicalCores: number };
  memory: { totalBytes: number | null; swapTotalBytes: number | null };
  gpus: GpuInventory[];
  filesystems: FilesystemInventory[];
  disks: DiskInventory[];
  network: InterfaceInventory[];
  /** Per family plus "processes"; withheld = local sharing policy for this hub. */
  capabilities: Record<MetricFamily | "processes", CapabilityState>;
};

export type CollectionFailure = { metric: string; resourceId: string | null; reason: string };

export type DeliveryStatus = {
  /**
   * How the collector retains samples it has not had acknowledged. `memory`
   * (the collector default) loses them on a collector restart; `durable` keeps
   * them in a per-destination SQLite outbox. Optional so a collector predating
   * the mode still parses.
   */
  queueMode?: "memory" | "durable";
  pendingRecords: number;
  pendingBytes: number;
  oldestPendingCapturedAt: string | null;
  inFlightRecords: number;
  droppedRecords: number;
  replaying: boolean;
  lastAckAt: string | null;
};

export type ProcessRow = {
  pid: number;
  ppid: number;
  /** Kernel start time (clock ticks since boot) — with pid, identifies a process. */
  startTicks: number;
  name: string;
  user: string | null;
  state: string;
  cpuPercent: number | null;
  memoryRssBytes: number | null;
  gpuMemoryBytes: number | null;
  ioReadBytesPerSec: number | null;
  ioWriteBytesPerSec: number | null;
  threads: number | null;
  /** systemd unit/scope from the process cgroup (e.g. "firefox.scope"); null when unknown. Used for app aggregation. */
  unit: string | null;
  /** Present only when the destination's policy allows process arguments. */
  command?: string;
};

export type ProcessSnapshot = {
  totalProcesses: number;
  truncated: boolean;
  /** Processes that could not be read (permissions, disappeared mid-scan). */
  inaccessible: number;
  processes: ProcessRow[];
};

// Collector -> hub
export type HelloMessage = {
  type: "hello";
  collectorId: string;
  runId: string;
  bootId: string;
  hostname: string;
  capabilities: CapabilityName[];
  expectedIntervalMs: number;
  /**
   * Optional wire protocol version. Absent for collectors that predate
   * versioning; when present it must be a positive integer.
   */
  protocol?: number;
};
export type SamplesMessage = { type: "samples"; mode: "live" | "replay"; records: TelemetryRecord[] };
export type InventoryMessage = { type: "inventory"; capturedAt: string; inventory: Inventory };
export type StatusMessage = {
  type: "status";
  capturedAt: string;
  delivery: DeliveryStatus;
  failures: CollectionFailure[];
};
export type DetailResult = {
  type: "detail.result";
  subscriptionId: string;
  capturedAt: string;
  snapshot: ProcessSnapshot;
};
export type DetailEnded = {
  type: "detail.ended";
  subscriptionId: string;
  reason: "expired" | "cancelled" | "withheld" | "unsupported" | "busy" | "failed";
};
export type CollectorMessage = HelloMessage | SamplesMessage | InventoryMessage | StatusMessage | DetailResult | DetailEnded;

// Hub -> collector
export type WelcomeMessage = {
  type: "welcome";
  hubRunId: string;
  machineId: string;
  /**
   * `durable` means an ack follows a crash-safe commit. `memory` means the hub
   * accepted the sample into RAM and its history intentionally disappears on
   * hub restart. Collectors keep the same outbox/retry behavior for both.
   */
  delivery: "durable" | "memory";
  /** Maximum unacknowledged samples messages the collector may have outstanding. */
  maxInFlightBatches: number;
  /**
   * Optional wire protocol version the hub accepted. Absent for hubs that
   * predate versioning; when present it must be a positive integer.
   */
  protocol?: number;
};
export type AckMessage = {
  type: "ack";
  accepted: RecordRef[];
  rejected: (RecordRef & { reason: string })[];
  /** Set when the hub could not accept the batch now; retry after this delay. */
  retryAfterMs?: number;
};
export type DetailSubscribe = {
  type: "detail.subscribe";
  /** Create-or-renew: re-sending an active id renews its lease. */
  subscriptionId: string;
  kind: "processes";
  intervalMs: number;
  leaseMs: number;
};
export type DetailCancel = { type: "detail.cancel"; subscriptionId: string };
export type HubMessage = WelcomeMessage | AckMessage | DetailSubscribe | DetailCancel;

// ---------------------------------------------------------------------------
// Parsers (throw Error on invalid input)
// ---------------------------------------------------------------------------

export function parseCollectorMessage(value: unknown): CollectorMessage {
  const o = obj(value, "collector message");
  switch (o.type) {
    case "hello": {
      keys(o, ["type", "collectorId", "runId", "bootId", "hostname", "capabilities", "expectedIntervalMs"], "hello", ["protocol"]);
      const caps = arr(o.capabilities, "hello.capabilities", 1, CAPABILITY_NAMES.length).map((c) => oneOf(c, CAPABILITY_NAMES, "hello.capabilities"));
      if (new Set(caps).size !== caps.length || !caps.includes("metrics")) throw new Error("invalid hello: capabilities must be unique and include metrics");
      const hello: HelloMessage = {
        type: "hello",
        collectorId: id(o.collectorId, "collectorId"), runId: id(o.runId, "runId"),
        bootId: text(o.bootId, "bootId", 0, MAX_ID), hostname: text(o.hostname, "hostname", 0, MAX_HOSTNAME),
        capabilities: caps, expectedIntervalMs: int(o.expectedIntervalMs, "expectedIntervalMs", MIN_EXPECTED_INTERVAL_MS, MAX_EXPECTED_INTERVAL_MS),
      };
      if (hasOwn(o, "protocol")) hello.protocol = int(o.protocol, "protocol", 1, Number.MAX_SAFE_INTEGER);
      return hello;
    }
    case "samples": {
      keys(o, ["type", "mode", "records"], "samples");
      const mode = oneOf(o.mode, ["live", "replay"] as const, "samples.mode");
      const records = arr(o.records, "samples.records", 1, MAX_BATCH_RECORDS).map((r, i) => parseTelemetryRecord(r, `records[${i}]`));
      const seen = new Set<string>();
      for (const r of records) {
        const k = `${r.runId}\u0000${r.sequence}`;
        if (seen.has(k)) throw new Error("invalid samples: duplicate record in batch");
        seen.add(k);
      }
      return { type: "samples", mode, records };
    }
    case "inventory":
      keys(o, ["type", "capturedAt", "inventory"], "inventory");
      return { type: "inventory", capturedAt: ts(o.capturedAt, "capturedAt"), inventory: parseInventory(o.inventory) };
    case "status":
      keys(o, ["type", "capturedAt", "delivery", "failures"], "status");
      return {
        type: "status", capturedAt: ts(o.capturedAt, "capturedAt"),
        delivery: parseDeliveryStatus(o.delivery),
        failures: arr(o.failures, "failures", 0, MAX_COLLECTION_FAILURES).map((f) => {
          const x = obj(f, "failure");
          keys(x, ["metric", "resourceId", "reason"], "failure");
          return { metric: text(x.metric, "metric", 1, 64), resourceId: x.resourceId === null ? null : rid(x.resourceId), reason: text(x.reason, "reason", 0, MAX_TEXT) };
        }),
      };
    case "detail.result":
      keys(o, ["type", "subscriptionId", "capturedAt", "snapshot"], "detail.result");
      return { type: "detail.result", subscriptionId: id(o.subscriptionId, "subscriptionId"), capturedAt: ts(o.capturedAt, "capturedAt"), snapshot: parseProcessSnapshot(o.snapshot) };
    case "detail.ended":
      keys(o, ["type", "subscriptionId", "reason"], "detail.ended");
      return {
        type: "detail.ended", subscriptionId: id(o.subscriptionId, "subscriptionId"),
        reason: oneOf(o.reason, ["expired", "cancelled", "withheld", "unsupported", "busy", "failed"] as const, "detail.ended.reason"),
      };
    default:
      throw new Error(`invalid collector message: unsupported type ${desc(o.type)}`);
  }
}

export function parseHubMessage(value: unknown): HubMessage {
  const o = obj(value, "hub message");
  switch (o.type) {
    case "welcome": {
      keys(o, ["type", "hubRunId", "machineId", "delivery", "maxInFlightBatches"], "welcome", ["protocol"]);
      if (o.delivery !== "durable" && o.delivery !== "memory") {
        throw new Error(`invalid welcome: unexpected delivery ${desc(o.delivery)}`);
      }
      const welcome: WelcomeMessage = {
        type: "welcome",
        hubRunId: id(o.hubRunId, "hubRunId"),
        machineId: id(o.machineId, "machineId"),
        delivery: o.delivery,
        maxInFlightBatches: int(o.maxInFlightBatches, "maxInFlightBatches", 1, MAX_IN_FLIGHT_BATCHES),
      };
      if (hasOwn(o, "protocol")) welcome.protocol = int(o.protocol, "protocol", 1, Number.MAX_SAFE_INTEGER);
      return welcome;
    }
    case "ack": {
      keys(o, ["type", "accepted", "rejected"], "ack", ["retryAfterMs"]);
      const accepted = arr(o.accepted, "ack.accepted", 0, MAX_BATCH_RECORDS * 64).map((r) => parseRef(r, []));
      const rejected = arr(o.rejected, "ack.rejected", 0, MAX_BATCH_RECORDS * 64).map((r) => {
        const ref = parseRef(r, ["reason"]);
        return { ...ref, reason: text((r as Record<string, unknown>).reason, "reason", 0, MAX_TEXT) };
      });
      const out: AckMessage = { type: "ack", accepted, rejected };
      if (hasOwn(o, "retryAfterMs")) out.retryAfterMs = int(o.retryAfterMs, "retryAfterMs", 0, 600_000);
      return out;
    }
    case "detail.subscribe":
      keys(o, ["type", "subscriptionId", "kind", "intervalMs", "leaseMs"], "detail.subscribe");
      if (o.kind !== "processes") throw new Error("invalid detail.subscribe: unsupported kind");
      return {
        type: "detail.subscribe", subscriptionId: id(o.subscriptionId, "subscriptionId"), kind: "processes",
        intervalMs: int(o.intervalMs, "intervalMs", MIN_DETAIL_INTERVAL_MS, MAX_DETAIL_INTERVAL_MS),
        leaseMs: int(o.leaseMs, "leaseMs", MIN_DETAIL_LEASE_MS, MAX_DETAIL_LEASE_MS),
      };
    case "detail.cancel":
      keys(o, ["type", "subscriptionId"], "detail.cancel");
      return { type: "detail.cancel", subscriptionId: id(o.subscriptionId, "subscriptionId") };
    default:
      throw new Error(`invalid hub message: unsupported type ${desc(o.type)}`);
  }
}

export function parseTelemetryRecord(value: unknown, what = "record"): TelemetryRecord {
  const o = obj(value, what);
  keys(o, ["collectorId", "runId", "bootId", "sequence", "capturedAt", "kind", "payload"], what);
  const env = {
    collectorId: id(o.collectorId, "collectorId"),
    runId: id(o.runId, "runId"),
    bootId: text(o.bootId, "bootId", 0, MAX_ID),
    sequence: int(o.sequence, "sequence", 0, Number.MAX_SAFE_INTEGER),
    capturedAt: ts(o.capturedAt, "capturedAt"),
  };
  if (o.kind === "metrics") return { ...env, kind: "metrics", payload: parseMetricsPayload(o.payload) };
  if (o.kind === "gap") {
    const p = obj(o.payload, `${what}.payload`);
    keys(p, ["fromCapturedAt", "toCapturedAt", "droppedRecords", "reason"], "gap payload");
    const from = ts(p.fromCapturedAt, "fromCapturedAt");
    const to = ts(p.toCapturedAt, "toCapturedAt");
    const fromMillis = timestampMillis(from);
    const toMillis = timestampMillis(to);
    if (fromMillis === null || toMillis === null || fromMillis > toMillis) {
      throw new Error("invalid gap: fromCapturedAt after toCapturedAt");
    }
    return {
      ...env, kind: "gap",
      payload: { fromCapturedAt: from, toCapturedAt: to, droppedRecords: int(p.droppedRecords, "droppedRecords", 1, Number.MAX_SAFE_INTEGER), reason: oneOf(p.reason, GAP_REASONS, "gap.reason") },
    };
  }
  throw new Error(`invalid ${what}: unsupported kind ${desc(o.kind)}`);
}

export function parseMetricsPayload(value: unknown): MetricsPayload {
  const o = obj(value, "metrics payload");
  keys(o, [...METRIC_FAMILIES], "metrics payload");
  return {
    cpu: family(o.cpu, "cpu", (x) => {
      keys(
        x,
        ["status", "usagePercent", "perCore"],
        "cpu",
        ["temperatureC", "powerWatts", "iowaitPercent", "pressureSomePercent"],
      );
      const out: Extract<CpuMetric, { status: "ok" }> = {
        status: "ok", usagePercent: num(x.usagePercent, "usagePercent", 0, 100),
        perCore: arr(x.perCore, "perCore", 0, MAX_CORES).map((v) => (v === null ? null : num(v, "perCore", 0, 100))),
      };
      const temperatureC = optionalNullable(x, "temperatureC", (v) => num(v, "temperatureC", -50, 200));
      if (temperatureC !== undefined) out.temperatureC = temperatureC;
      const powerWatts = optionalNullable(x, "powerWatts", (v) => num(v, "powerWatts", 0, 10_000));
      if (powerWatts !== undefined) out.powerWatts = powerWatts;
      const iowaitPercent = optionalNullable(x, "iowaitPercent", (v) => num(v, "iowaitPercent", 0, 100));
      if (iowaitPercent !== undefined) out.iowaitPercent = iowaitPercent;
      const pressureSomePercent = optionalNullable(x, "pressureSomePercent", (v) => num(v, "pressureSomePercent", 0, 100));
      if (pressureSomePercent !== undefined) out.pressureSomePercent = pressureSomePercent;
      return out;
    }),
    memory: family(o.memory, "memory", memoryUsage),
    swap: family(o.swap, "swap", usage),
    gpus: family(o.gpus, "gpus", (x) => {
      keys(x, ["status", "devices"], "gpus");
      return {
        status: "ok",
        devices: uniqueIds(arr(x.devices, "gpus.devices", 0, MAX_GPUS).map((d) => resource(d, "gpu", (y, rid_) => {
          keys(
            y,
            ["id", "status", "utilizationPercent", "memoryUsedBytes", "memoryTotalBytes", "temperatureC", "powerWatts"],
            "gpu",
            ["junctionTemperatureC", "vramTemperatureC"],
          );
          const used = nullable(y.memoryUsedBytes, (v) => int(v, "memoryUsedBytes", 0, Number.MAX_SAFE_INTEGER));
          const total = nullable(y.memoryTotalBytes, (v) => int(v, "memoryTotalBytes", 0, Number.MAX_SAFE_INTEGER));
          if (used !== null && total !== null && used > total) throw new Error("invalid gpu: memoryUsedBytes exceeds total");
          const out: Extract<GpuReading, { status: "ok" }> = {
            id: rid_, status: "ok",
            utilizationPercent: nullable(y.utilizationPercent, (v) => num(v, "utilizationPercent", 0, 100)),
            memoryUsedBytes: used, memoryTotalBytes: total,
            temperatureC: nullable(y.temperatureC, (v) => num(v, "temperatureC", -50, 200)),
            powerWatts: nullable(y.powerWatts, (v) => num(v, "powerWatts", 0, 10_000)),
          };
          const junctionTemperatureC = optionalNullable(y, "junctionTemperatureC", (v) => num(v, "junctionTemperatureC", -50, 200));
          if (junctionTemperatureC !== undefined) out.junctionTemperatureC = junctionTemperatureC;
          const vramTemperatureC = optionalNullable(y, "vramTemperatureC", (v) => num(v, "vramTemperatureC", -50, 200));
          if (vramTemperatureC !== undefined) out.vramTemperatureC = vramTemperatureC;
          return out;
        }))),
      };
    }),
    disks: family(o.disks, "disks", (x) => {
      keys(x, ["status", "filesystems", "devices"], "disks", ["pressureSomePercent", "pressureFullPercent"]);
      const out: Extract<DisksMetric, { status: "ok" }> = {
        status: "ok",
        filesystems: uniqueIds(arr(x.filesystems, "filesystems", 0, MAX_FILESYSTEMS).map((d) => resource(d, "filesystem", (y, rid_) => {
          keys(y, ["id", "status", "usedBytes", "totalBytes"], "filesystem", ["inodesUsed", "inodesTotal"]);
          const u = usage({ status: "ok", usedBytes: y.usedBytes, totalBytes: y.totalBytes });
          const reading: Extract<FilesystemReading, { status: "ok" }> = {
            id: rid_, status: "ok", usedBytes: u.usedBytes, totalBytes: u.totalBytes,
          };
          const inodesUsed = optionalNullable(y, "inodesUsed", (v) => int(v, "inodesUsed", 0, Number.MAX_SAFE_INTEGER));
          if (inodesUsed !== undefined) reading.inodesUsed = inodesUsed;
          const inodesTotal = optionalNullable(y, "inodesTotal", (v) => int(v, "inodesTotal", 0, Number.MAX_SAFE_INTEGER));
          if (inodesTotal !== undefined) reading.inodesTotal = inodesTotal;
          if (inodesUsed != null && inodesTotal != null && inodesUsed > inodesTotal) {
            throw new Error("invalid filesystem: inodesUsed exceeds inodesTotal");
          }
          return reading;
        }))),
        devices: uniqueIds(arr(x.devices, "disk devices", 0, MAX_DISKS).map((d) => resource(d, "disk", (y, rid_) => {
          keys(
            y,
            ["id", "status", "readBytesPerSec", "writeBytesPerSec"],
            "disk",
            ["readLatencyMs", "writeLatencyMs", "ioUtilizationPercent"],
          );
          const reading: Extract<DiskIoReading, { status: "ok" }> = {
            id: rid_, status: "ok",
            readBytesPerSec: num(y.readBytesPerSec, "readBytesPerSec", 0, 1e13),
            writeBytesPerSec: num(y.writeBytesPerSec, "writeBytesPerSec", 0, 1e13),
          };
          const readLatencyMs = optionalNullable(y, "readLatencyMs", (v) => num(v, "readLatencyMs", 0, 86_400_000));
          if (readLatencyMs !== undefined) reading.readLatencyMs = readLatencyMs;
          const writeLatencyMs = optionalNullable(y, "writeLatencyMs", (v) => num(v, "writeLatencyMs", 0, 86_400_000));
          if (writeLatencyMs !== undefined) reading.writeLatencyMs = writeLatencyMs;
          const ioUtilizationPercent = optionalNullable(y, "ioUtilizationPercent", (v) => num(v, "ioUtilizationPercent", 0, 100));
          if (ioUtilizationPercent !== undefined) reading.ioUtilizationPercent = ioUtilizationPercent;
          return reading;
        }))),
      };
      const pressureSomePercent = optionalNullable(x, "pressureSomePercent", (v) => num(v, "pressureSomePercent", 0, 100));
      if (pressureSomePercent !== undefined) out.pressureSomePercent = pressureSomePercent;
      const pressureFullPercent = optionalNullable(x, "pressureFullPercent", (v) => num(v, "pressureFullPercent", 0, 100));
      if (pressureFullPercent !== undefined) out.pressureFullPercent = pressureFullPercent;
      return out;
    }),
    network: family(o.network, "network", (x) => {
      keys(x, ["status", "interfaces"], "network");
      return {
        status: "ok",
        interfaces: uniqueIds(arr(x.interfaces, "interfaces", 0, MAX_INTERFACES).map((d) => resource(d, "interface", (y, rid_) => {
          keys(y, ["id", "status", "rxBytesPerSec", "txBytesPerSec"], "interface");
          return { id: rid_, status: "ok", rxBytesPerSec: num(y.rxBytesPerSec, "rxBytesPerSec", 0, 1e13), txBytesPerSec: num(y.txBytesPerSec, "txBytesPerSec", 0, 1e13) };
        }))),
      };
    }),
  };
}

export function parseInventory(value: unknown): Inventory {
  const o = obj(value, "inventory");
  keys(o, ["os", "cpu", "memory", "gpus", "filesystems", "disks", "network", "capabilities"], "inventory");
  const os = obj(o.os, "inventory.os");
  keys(os, ["platform", "release", "distro", "arch"], "inventory.os");
  const cpu = obj(o.cpu, "inventory.cpu");
  keys(cpu, ["model", "logicalCores"], "inventory.cpu");
  const mem = obj(o.memory, "inventory.memory");
  keys(mem, ["totalBytes", "swapTotalBytes"], "inventory.memory");
  const caps = obj(o.capabilities, "inventory.capabilities");
  const capNames = [...METRIC_FAMILIES, "processes"] as const;
  keys(caps, [...capNames], "inventory.capabilities");
  const capabilities = {} as Record<MetricFamily | "processes", CapabilityState>;
  for (const name of capNames) {
    const c = obj(caps[name], `capability ${name}`);
    keys(c, ["status"], `capability ${name}`, ["reason"]);
    const state: CapabilityState = { status: oneOf(c.status, ["supported", "unsupported", "withheld"] as const, `capability ${name}`) };
    if (hasOwn(c, "reason")) state.reason = text(c.reason, "reason", 0, MAX_TEXT);
    capabilities[name] = state;
  }
  const optBytes = (v: unknown, f: string) => nullable(v, (x) => int(x, f, 0, Number.MAX_SAFE_INTEGER));
  return {
    os: { platform: text(os.platform, "platform", 0, 64), release: text(os.release, "release", 0, MAX_TEXT), distro: nullable(os.distro, (v) => text(v, "distro", 0, MAX_TEXT)), arch: text(os.arch, "arch", 0, 64) },
    cpu: { model: nullable(cpu.model, (v) => text(v, "model", 0, MAX_TEXT)), logicalCores: int(cpu.logicalCores, "logicalCores", 0, MAX_CORES) },
    memory: { totalBytes: optBytes(mem.totalBytes, "totalBytes"), swapTotalBytes: optBytes(mem.swapTotalBytes, "swapTotalBytes") },
    gpus: uniqueIds(arr(o.gpus, "inventory.gpus", 0, MAX_GPUS).map((g) => {
      const x = obj(g, "gpu inventory");
      keys(x, ["id", "name", "vendor", "pciBusId", "telemetry", "memoryTotalBytes"], "gpu inventory", ["reason"]);
      const out: GpuInventory = {
        id: rid(x.id), name: text(x.name, "name", 0, MAX_TEXT),
        vendor: oneOf(x.vendor, ["nvidia", "amd", "intel", "aspeed", "other"] as const, "vendor"),
        pciBusId: nullable(x.pciBusId, (v) => text(v, "pciBusId", 1, 64)),
        telemetry: oneOf(x.telemetry, ["supported", "unsupported"] as const, "telemetry"),
        memoryTotalBytes: optBytes(x.memoryTotalBytes, "memoryTotalBytes"),
      };
      if (hasOwn(x, "reason")) out.reason = text(x.reason, "reason", 0, MAX_TEXT);
      return out;
    })),
    filesystems: uniqueIds(arr(o.filesystems, "inventory.filesystems", 0, MAX_FILESYSTEMS).map((f) => {
      const x = obj(f, "filesystem inventory");
      keys(x, ["id", "mountPoint", "device", "fsType", "totalBytes"], "filesystem inventory");
      return { id: rid(x.id), mountPoint: text(x.mountPoint, "mountPoint", 1, MAX_TEXT), device: text(x.device, "device", 0, MAX_TEXT), fsType: text(x.fsType, "fsType", 0, 64), totalBytes: optBytes(x.totalBytes, "totalBytes") };
    })),
    disks: uniqueIds(arr(o.disks, "inventory.disks", 0, MAX_DISKS).map((d) => {
      const x = obj(d, "disk inventory");
      keys(x, ["id", "name", "model", "rotational", "sizeBytes"], "disk inventory");
      if (x.rotational !== null && typeof x.rotational !== "boolean") throw new Error("invalid disk inventory: rotational");
      return { id: rid(x.id), name: text(x.name, "name", 1, 64), model: nullable(x.model, (v) => text(v, "model", 0, MAX_TEXT)), rotational: x.rotational as boolean | null, sizeBytes: optBytes(x.sizeBytes, "sizeBytes") };
    })),
    network: uniqueIds(arr(o.network, "inventory.network", 0, MAX_INTERFACES).map((n) => {
      const x = obj(n, "interface inventory");
      keys(x, ["id", "name", "mac", "addresses", "virtual", "state", "speedMbps"], "interface inventory");
      if (typeof x.virtual !== "boolean") throw new Error("invalid interface inventory: virtual");
      return {
        id: rid(x.id), name: text(x.name, "name", 1, 64), mac: nullable(x.mac, (v) => text(v, "mac", 1, 64)),
        addresses: arr(x.addresses, "addresses", 0, MAX_ADDRESSES_PER_INTERFACE).map((a) => {
          const y = obj(a, "address");
          keys(y, ["family", "address", "prefix"], "address");
          const family = oneOf(y.family, ["ipv4", "ipv6"] as const, "family");
          return { family, address: text(y.address, "address", 2, 64), prefix: int(y.prefix, "prefix", 0, family === "ipv4" ? 32 : 128) };
        }),
        virtual: x.virtual, state: oneOf(x.state, ["up", "down", "unknown"] as const, "state"),
        speedMbps: nullable(x.speedMbps, (v) => int(v, "speedMbps", 0, 10_000_000)),
      };
    })),
    capabilities,
  };
}

export function parseDeliveryStatus(value: unknown): DeliveryStatus {
  const d = obj(value, "delivery");
  keys(d, ["pendingRecords", "pendingBytes", "oldestPendingCapturedAt", "inFlightRecords", "droppedRecords", "replaying", "lastAckAt"], "delivery", ["queueMode"]);
  if (typeof d.replaying !== "boolean") throw new Error("invalid delivery: replaying");
  if (d.queueMode !== undefined && d.queueMode !== "memory" && d.queueMode !== "durable") {
    throw new Error(`invalid delivery: unexpected queueMode ${desc(d.queueMode)}`);
  }
  const big = (v: unknown, f: string) => int(v, f, 0, Number.MAX_SAFE_INTEGER);
  return {
    pendingRecords: big(d.pendingRecords, "pendingRecords"), pendingBytes: big(d.pendingBytes, "pendingBytes"),
    oldestPendingCapturedAt: nullable(d.oldestPendingCapturedAt, (v) => ts(v, "oldestPendingCapturedAt")),
    inFlightRecords: big(d.inFlightRecords, "inFlightRecords"), droppedRecords: big(d.droppedRecords, "droppedRecords"),
    replaying: d.replaying, lastAckAt: nullable(d.lastAckAt, (v) => ts(v, "lastAckAt")),
    ...(d.queueMode === undefined ? {} : { queueMode: d.queueMode as "memory" | "durable" }),
  };
}

export function parseProcessSnapshot(value: unknown): ProcessSnapshot {
  const s = obj(value, "process snapshot");
  keys(s, ["totalProcesses", "truncated", "inaccessible", "processes"], "process snapshot");
  if (typeof s.truncated !== "boolean") throw new Error("invalid process snapshot: truncated");
  const optNum = (v: unknown, f: string, max: number) => nullable(v, (x) => num(x, f, 0, max));
  return {
    totalProcesses: int(s.totalProcesses, "totalProcesses", 0, 10_000_000),
    truncated: s.truncated,
    inaccessible: int(s.inaccessible, "inaccessible", 0, 10_000_000),
    processes: arr(s.processes, "processes", 0, MAX_PROCESS_ROWS).map((p) => {
      const x = obj(p, "process");
      keys(x, ["pid", "ppid", "startTicks", "name", "user", "state", "cpuPercent", "memoryRssBytes", "gpuMemoryBytes", "ioReadBytesPerSec", "ioWriteBytesPerSec", "threads", "unit"], "process", ["command"]);
      const row: ProcessRow = {
        pid: int(x.pid, "pid", 0, 2 ** 31), ppid: int(x.ppid, "ppid", 0, 2 ** 31),
        startTicks: int(x.startTicks, "startTicks", 0, Number.MAX_SAFE_INTEGER),
        name: text(x.name, "name", 0, MAX_TEXT), user: nullable(x.user, (v) => text(v, "user", 0, 64)),
        state: text(x.state, "state", 0, 16),
        // Per-process CPU may exceed 100 on multi-core (100 = one full core).
        cpuPercent: optNum(x.cpuPercent, "cpuPercent", 100 * MAX_CORES),
        memoryRssBytes: nullable(x.memoryRssBytes, (v) => int(v, "memoryRssBytes", 0, Number.MAX_SAFE_INTEGER)),
        gpuMemoryBytes: nullable(x.gpuMemoryBytes, (v) => int(v, "gpuMemoryBytes", 0, Number.MAX_SAFE_INTEGER)),
        ioReadBytesPerSec: optNum(x.ioReadBytesPerSec, "ioReadBytesPerSec", 1e13),
        ioWriteBytesPerSec: optNum(x.ioWriteBytesPerSec, "ioWriteBytesPerSec", 1e13),
        threads: nullable(x.threads, (v) => int(v, "threads", 0, 10_000_000)),
        unit: nullable(x.unit, (v) => text(v, "unit", 1, MAX_TEXT)),
      };
      if (hasOwn(x, "command")) row.command = text(x.command, "command", 0, 4096);
      return row;
    }),
  };
}

// ---------------------------------------------------------------------------
// Primitives
// ---------------------------------------------------------------------------

function parseRef(value: unknown, extra: string[]): RecordRef {
  const o = obj(value, "record ref");
  keys(o, ["runId", "sequence", ...extra], "record ref");
  return { runId: id(o.runId, "runId"), sequence: int(o.sequence, "sequence", 0, Number.MAX_SAFE_INTEGER) };
}

function usage(x: Record<string, unknown>): { status: "ok"; usedBytes: number; totalBytes: number } {
  keys(x, ["status", "usedBytes", "totalBytes"], "usage");
  const totalBytes = int(x.totalBytes, "totalBytes", 0, Number.MAX_SAFE_INTEGER);
  const usedBytes = int(x.usedBytes, "usedBytes", 0, Number.MAX_SAFE_INTEGER);
  if (usedBytes > totalBytes) throw new Error("invalid usage: usedBytes exceeds totalBytes");
  return { status: "ok", usedBytes, totalBytes };
}

function memoryUsage(x: Record<string, unknown>): Extract<MemoryMetric, { status: "ok" }> {
  keys(x, ["status", "usedBytes", "totalBytes"], "memory", ["pressureSomePercent", "pressureFullPercent"]);
  const totalBytes = int(x.totalBytes, "totalBytes", 0, Number.MAX_SAFE_INTEGER);
  const usedBytes = int(x.usedBytes, "usedBytes", 0, Number.MAX_SAFE_INTEGER);
  if (usedBytes > totalBytes) throw new Error("invalid memory: usedBytes exceeds totalBytes");
  const out: Extract<MemoryMetric, { status: "ok" }> = { status: "ok", usedBytes, totalBytes };
  const some = optionalNullable(x, "pressureSomePercent", (v) => num(v, "pressureSomePercent", 0, 100));
  if (some !== undefined) out.pressureSomePercent = some;
  const full = optionalNullable(x, "pressureFullPercent", (v) => num(v, "pressureFullPercent", 0, 100));
  if (full !== undefined) out.pressureFullPercent = full;
  return out;
}

function family<T>(value: unknown, what: string, ok: (x: Record<string, unknown>) => T): T | Unavailable {
  const x = obj(value, what);
  if (x.status === "ok") return ok(x);
  if (x.status === "unavailable") {
    keys(x, ["status", "reason"], what);
    return { status: "unavailable", reason: text(x.reason, "reason", 0, MAX_TEXT) };
  }
  throw new Error(`invalid ${what}: unsupported status ${desc(x.status)}`);
}

function resource<T>(value: unknown, what: string, ok: (x: Record<string, unknown>, id: string) => T): T | { id: string; status: "unavailable"; reason: string } {
  const x = obj(value, what);
  const rid_ = rid(x.id);
  if (x.status === "ok") return ok(x, rid_);
  if (x.status === "unavailable") {
    keys(x, ["id", "status", "reason"], what);
    return { id: rid_, status: "unavailable", reason: text(x.reason, "reason", 0, MAX_TEXT) };
  }
  throw new Error(`invalid ${what}: unsupported status ${desc(x.status)}`);
}

function uniqueIds<T extends { id: string }>(items: T[]): T[] {
  const seen = new Set<string>();
  for (const item of items) {
    if (seen.has(item.id)) throw new Error(`invalid resource list: duplicate id ${desc(item.id)}`);
    seen.add(item.id);
  }
  return items;
}

function nullable<T>(v: unknown, parse: (v: unknown) => T): T | null {
  return v === null ? null : parse(v);
}

/**
 * Optional wire field: absent stays absent (hardware without the reading),
 * a present null means explicitly unavailable, and any other value is parsed.
 */
function optionalNullable<T>(
  o: Record<string, unknown>,
  field: string,
  parse: (v: unknown) => T,
): T | null | undefined {
  if (!hasOwn(o, field)) return undefined;
  return nullable(o[field], parse);
}

function obj(value: unknown, what: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`invalid ${what}: expected object, got ${desc(value)}`);
  return value as Record<string, unknown>;
}

function keys(o: Record<string, unknown>, required: readonly string[], what: string, optional: readonly string[] = []): void {
  const allowed = new Set([...required, ...optional]);
  for (const k of Object.keys(o)) if (!allowed.has(k)) throw new Error(`invalid ${what}: unexpected field "${k}"`);
  for (const k of required) if (o[k] === undefined) throw new Error(`invalid ${what}: missing field "${k}"`);
  for (const k of optional) if (hasOwn(o, k) && o[k] === undefined) throw new Error(`invalid ${what}: ${k} must not be undefined`);
}

function hasOwn(o: Record<string, unknown>, k: string): boolean {
  return Object.prototype.hasOwnProperty.call(o, k);
}

function arr(v: unknown, what: string, min: number, max: number): unknown[] {
  if (!Array.isArray(v)) throw new Error(`invalid ${what}: expected array`);
  if (v.length < min || v.length > max) throw new Error(`invalid ${what}: length ${v.length} outside ${min}..${max}`);
  return v;
}

function oneOf<T extends string>(v: unknown, options: readonly T[], what: string): T {
  if (typeof v !== "string" || !(options as readonly string[]).includes(v)) throw new Error(`invalid ${what}: unexpected value ${desc(v)}`);
  return v as T;
}

function text(v: unknown, field: string, min: number, max: number): string {
  if (typeof v !== "string") throw new Error(`invalid ${field}: expected string, got ${desc(v)}`);
  if (!isWellFormed(v)) throw new Error(`invalid ${field}: string contains an unpaired UTF-16 surrogate`);
  if (v.length < min || v.length > max) throw new Error(`invalid ${field}: length ${v.length} outside ${min}..${max}`);
  return v;
}

/**
 * A protocol string must be a well-formed Unicode scalar sequence. Node has
 * String.prototype.isWellFormed; the regex fallback rejects an unpaired high or
 * low surrogate for older engines.
 */
function isWellFormed(value: string): boolean {
  const candidate = value as string & { isWellFormed?: () => boolean };
  if (typeof candidate.isWellFormed === "function") return candidate.isWellFormed();
  return /^(?:[\u0000-\uD7FF\uE000-\uFFFF]|[\uD800-\uDBFF][\uDC00-\uDFFF])*$/.test(value);
}

function id(v: unknown, field: string): string {
  return text(v, field, 1, MAX_ID);
}

function rid(v: unknown): string {
  return text(v, "resource id", 1, MAX_RESOURCE_ID);
}

function int(v: unknown, field: string, min: number, max: number): number {
  if (typeof v !== "number" || !Number.isSafeInteger(v)) throw new Error(`invalid ${field}: expected safe integer, got ${desc(v)}`);
  if (v < min || v > max) throw new Error(`invalid ${field}: ${v} outside ${min}..${max}`);
  return v;
}

function num(v: unknown, field: string, min: number, max: number): number {
  if (typeof v !== "number" || !Number.isFinite(v)) throw new Error(`invalid ${field}: expected finite number, got ${desc(v)}`);
  if (v < min || v > max) throw new Error(`invalid ${field}: ${v} outside ${min}..${max}`);
  return v;
}

function ts(v: unknown, field: string): string {
  const t = text(v, field, 1, MAX_TS);
  const epoch = timestampMillis(t);
  if (epoch === null || epoch < MIN_EPOCH || epoch > MAX_EPOCH) {
    throw new Error(`invalid ${field}: not a parseable timestamp`);
  }
  return t;
}

function desc(v: unknown): string {
  try {
    const s = JSON.stringify(v);
    if (typeof s === "string") return s.length <= 80 ? s : `${s.slice(0, 77)}...`;
  } catch {
    // fall through
  }
  return String(v);
}
