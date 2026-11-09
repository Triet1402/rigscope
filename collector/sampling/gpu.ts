/**
 * Asynchronous NVIDIA telemetry polling.
 *
 * `nvidia-smi` is relatively expensive (~70 ms on the target host) and may
 * briefly stall under heavy GPU load, so it never runs on the synchronous
 * metric-sampling path. One poller owns a small cache; the metrics sampler
 * reads that cache. At most one `nvidia-smi` query is in flight at a time,
 * failures back off exponentially (capped at five minutes), and `ENOENT`
 * disables normal cadence polling while still retrying rarely in case tooling
 * is installed later.
 *
 * Every value is field-level optional: `[N/A]`, `[Not Supported]`, `N/A` and
 * empty CSV cells become `null` rather than fabricated zeros.
 */
import { execFile as nodeExecFile } from 'node:child_process';

import type { GpuReading } from '../../src/contracts';
import type { GpuSensorMap, GpuSensorSource } from './gpu-temps';
import { normalizeBusFunctionKey } from './gpu-temps';

export type GpuAvailability = 'yes' | 'no' | 'unknown';

export type GpuDeviceInfo = {
  uuid: string;
  name: string;
  pciBusId: string | null;
  memoryTotalBytes: number | null;
};

export type GpuSnapshot = {
  atMonotonicMs: number;
  devices: GpuReading[];
};

export type GpuCostStats = {
  /** Number of `nvidia-smi` invocations (GPU and compute-app queries). */
  runs: number;
  failures: number;
  lastDurationMs: number;
  avgDurationMs: number;
  maxDurationMs: number;
};

type ExecError = Error & {code?: string; killed?: boolean; signal?: string};

export type ExecFileFn = (
  file: string,
  args: readonly string[],
  options: {timeout: number; maxBuffer: number; encoding: 'utf8'},
  callback: (error: ExecError | null, stdout: string, stderr: string) => void,
) => void;

export type GpuPollerOptions = {
  /** Collector cadence; the actual poll interval is `max(cadence, 1000)`. */
  intervalMs: number;
  /** Default `nvidia-smi`. */
  command?: string;
  /** Per-invocation timeout; default 3000 ms. */
  timeoutMs?: number;
  /** Injectable for deterministic tests. */
  execFile?: ExecFileFn;
  /** Injectable clocks (wall clock is accepted for parity/debugging). */
  now?: () => number;
  monotonicMs?: () => number;
  /** Short diagnostic line sink. */
  onEvent?: (line: string) => void;
  /**
   * Optional junction/VRAM register temperature source. When provided, it is
   * polled alongside `nvidia-smi` and merged into readings by PCI bus id.
   */
  sensorSource?: GpuSensorSource | null;
};

const DEFAULT_TIMEOUT_MS = 3_000;
const MIN_POLL_INTERVAL_MS = 1_000;
const MAX_BACKOFF_MS = 300_000;
const ENOENT_RETRY_MS = 300_000;
const MAX_BUFFER_BYTES = 4 * 1024 * 1024;
const GPU_QUERY =
  '--query-gpu=uuid,name,pci.bus_id,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw';
const APP_QUERY = '--query-compute-apps=pid,gpu_uuid,used_memory';
const CSV_FORMAT = '--format=csv,noheader,nounits';
const NULL_TOKENS = new Set([
  '',
  'n/a',
  '[n/a]',
  'na',
  'not supported',
  '[not supported]',
  'unknown',
  '[unknown]',
  '[unknown error]',
  'unknown error',
]);

const defaultExecFile: ExecFileFn = (file, args, options, callback) => {
  nodeExecFile(file, [...args], options, (error, stdout, stderr) => {
    callback(
      (error as ExecError | null) ?? null,
      typeof stdout === 'string' ? stdout : String(stdout ?? ''),
      typeof stderr === 'string' ? stderr : String(stderr ?? ''),
    );
  });
};

function isNullToken(raw: string): boolean {
  return NULL_TOKENS.has(raw.trim().toLowerCase());
}

function nullableNumber(raw: string): number | null {
  if (isNullToken(raw)) return null;
  const value = Number(raw.trim());
  return Number.isFinite(value) ? value : null;
}

function nullableText(raw: string): string | null {
  if (isNullToken(raw)) return null;
  const value = raw.trim();
  return value === '' ? null : value;
}

function mibToBytes(raw: string): number | null {
  const mib = nullableNumber(raw);
  if (mib === null || mib < 0) return null;
  const bytes = Math.round(mib * 1024 * 1024);
  return Number.isSafeInteger(bytes) ? bytes : null;
}

export type ParsedGpuRow = {
  uuid: string;
  name: string;
  pciBusId: string | null;
  utilizationPercent: number | null;
  memoryUsedBytes: number | null;
  memoryTotalBytes: number | null;
  temperatureC: number | null;
  powerWatts: number | null;
};

/**
 * Parses `nvidia-smi --query-gpu=uuid,name,pci.bus_id,utilization.gpu,
 * memory.used,memory.total,temperature.gpu,power.draw --format=csv,noheader,nounits`.
 */
export function parseGpuCsv(stdout: string): ParsedGpuRow[] {
  const rows: ParsedGpuRow[] = [];
  if (typeof stdout !== 'string') return rows;
  const seen = new Set<string>();
  for (const line of stdout.split('\n')) {
    if (line.trim() === '') continue;
    const fields = line.split(',').map((field) => field.trim());
    if (fields.length < 3) continue;
    const uuid = nullableText(fields[0] ?? '');
    if (uuid === null) continue;
    if (seen.has(uuid)) continue;
    seen.add(uuid);
    rows.push({
      uuid,
      name: nullableText(fields[1] ?? '') ?? 'NVIDIA GPU',
      pciBusId: nullableText(fields[2] ?? ''),
      utilizationPercent: nullableNumber(fields[3] ?? ''),
      memoryUsedBytes: mibToBytes(fields[4] ?? ''),
      memoryTotalBytes: mibToBytes(fields[5] ?? ''),
      temperatureC: nullableNumber(fields[6] ?? ''),
      powerWatts: nullableNumber(fields[7] ?? ''),
    });
  }
  return rows;
}

export type ParsedComputeApp = {pid: number; gpuUuid: string; usedBytes: number};

/** Parses `--query-compute-apps=pid,gpu_uuid,used_memory`. */
export function parseComputeAppsCsv(stdout: string): ParsedComputeApp[] {
  const apps: ParsedComputeApp[] = [];
  if (typeof stdout !== 'string') return apps;
  for (const line of stdout.split('\n')) {
    if (line.trim() === '') continue;
    const fields = line.split(',').map((field) => field.trim());
    if (fields.length < 3) continue;
    const pid = Number(fields[0]);
    const gpuUuid = nullableText(fields[1] ?? '');
    const used = mibToBytes(fields[2] ?? '');
    if (!Number.isSafeInteger(pid) || pid < 0 || gpuUuid === null || used === null) continue;
    apps.push({pid, gpuUuid, usedBytes: used});
  }
  return apps;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function isEnoent(error: unknown): boolean {
  const code = (error as {code?: unknown} | null)?.code;
  return code === 'ENOENT' || /ENOENT|not found/i.test(errorMessage(error));
}

/**
 * Async GPU poller. Call `start()` in the collector's real mode and `stop()`
 * during shutdown. Tests can call `pollOnce()` directly (no timers needed).
 */
export class GpuPoller {
  readonly pollIntervalMs: number;
  readonly timeoutMs: number;
  private readonly command: string;
  private readonly execFile: ExecFileFn;
  private readonly now: () => number;
  private readonly monotonicMs: () => number;
  private readonly onEvent?: (line: string) => void;
  private readonly sensorSource: GpuSensorSource | null;

  private started = false;
  private timer: NodeJS.Timeout | null = null;
  private inflight: Promise<void> | null = null;
  private consecutiveFailures = 0;
  private gpuQueries = 0;
  private lastDeviceCount = -1;
  private settleTimeoutEmitted = false;
  private snapshot: GpuSnapshot | null = null;
  private info = new Map<string, GpuDeviceInfo>();
  private apps = new Map<number, number>();
  private availability: GpuAvailability = 'unknown';
  private stats: GpuCostStats = {
    runs: 0,
    failures: 0,
    lastDurationMs: 0,
    avgDurationMs: 0,
    maxDurationMs: 0,
  };
  private totalDurationMs = 0;

  constructor(options: GpuPollerOptions) {
    const cadence = Number.isFinite(options.intervalMs) && options.intervalMs > 0
      ? options.intervalMs
      : MIN_POLL_INTERVAL_MS;
    this.pollIntervalMs = Math.max(cadence, MIN_POLL_INTERVAL_MS);
    this.command = options.command ?? 'nvidia-smi';
    this.timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;
    this.execFile = options.execFile ?? defaultExecFile;
    this.now = options.now ?? Date.now;
    this.monotonicMs = options.monotonicMs ?? (() => performance.now());
    this.onEvent = options.onEvent;
    this.sensorSource = options.sensorSource ?? null;
  }

  /** Starts the low-frequency polling loop (immediately, then per cadence). */
  start(): void {
    if (this.started) return;
    this.started = true;
    this.arm(0);
  }

  /** Stops scheduling polls. An in-flight query is allowed to settle. */
  stop(): void {
    this.started = false;
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  /**
   * Runs one polling cycle immediately. Concurrent calls share the same
   * in-flight promise, so at most one query is ever in flight.
   */
  pollOnce(): Promise<void> {
    if (this.inflight !== null) return this.inflight;
    const run = this.runCycle();
    this.inflight = run;
    void run.finally(() => {
      if (this.inflight === run) this.inflight = null;
    });
    return run;
  }

  latest(): GpuSnapshot | null {
    return this.snapshot;
  }

  /** Per-pid GPU memory in bytes, summed across devices. */
  computeApps(): Map<number, number> {
    return new Map(this.apps);
  }

  deviceInfo(): GpuDeviceInfo[] {
    return [...this.info.values()].sort((a, b) =>
      (a.pciBusId ?? a.uuid).localeCompare(b.pciBusId ?? b.uuid),
    );
  }

  available(): GpuAvailability {
    return this.availability;
  }

  costStats(): GpuCostStats {
    return {...this.stats};
  }

  private arm(delayMs: number): void {
    if (!this.started) return;
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      this.timer = null;
      void this.tick();
    }, delayMs);
    // Do not keep a collector shutdown waiting on the poll cadence.
    this.timer.unref?.();
  }

  private async tick(): Promise<void> {
    if (!this.started) return;
    await this.pollOnce();
    if (!this.started) return;
    this.arm(this.nextDelayMs());
  }

  private nextDelayMs(): number {
    if (this.availability === 'no') return ENOENT_RETRY_MS;
    if (this.consecutiveFailures > 0) {
      const exponent = Math.min(this.consecutiveFailures - 1, 10);
      return Math.min(this.pollIntervalMs * 2 ** exponent, MAX_BACKOFF_MS);
    }
    return this.pollIntervalMs;
  }

  private async runCycle(): Promise<void> {
    try {
      // Register temperatures are read in parallel with the expensive query.
      // The early catch keeps the sensor promise owned even if the
      // `nvidia-smi` exec below throws before it is awaited.
      const sensorCycle = this.sensorSource !== null
        ? this.sensorSource.pollOnce().catch(() => undefined)
        : null;
      const stdout = await this.exec(GPU_QUERY);
      const rows = parseGpuCsv(stdout);
      this.applyRows(rows);
      this.gpuQueries += 1;
      const wasAvailable = this.availability === 'yes';
      this.availability = 'yes';
      this.consecutiveFailures = 0;
      const sensors = await this.settleSensorCycle(sensorCycle);
      this.snapshot = {atMonotonicMs: this.monotonicMs(), devices: this.toReadings(rows, sensors)};
      // Diagnostics only on state changes: this line would otherwise repeat
      // every healthy cycle (two seconds by default).
      if (!wasAvailable || rows.length !== this.lastDeviceCount) {
        this.emit(`gpu telemetry: ${rows.length} device(s)`);
      }
      this.lastDeviceCount = rows.length;
      if (this.gpuQueries % 3 === 0) {
        await this.pollComputeApps();
      }
    } catch (error) {
      this.consecutiveFailures += 1;
      this.lastDeviceCount = -1;
      if (isEnoent(error)) {
        this.availability = 'no';
        this.emit('gpu telemetry unavailable: nvidia-smi not found');
      } else {
        this.emit(`gpu telemetry query failed: ${errorMessage(error)}`);
      }
    }
  }

  /**
   * Awaits the in-flight sensor cycle within the same budget as an
   * `nvidia-smi` invocation so a sensor promise that never settles or rejects
   * cannot hold up the snapshot. The register reads themselves are synchronous
   * sysfs reads on the event loop, so this budget cannot preempt a read that
   * blocks: it only bounds waiting on the cycle's promise. Failures never
   * invalidate the `nvidia-smi` readings; GpuTempsPoller reports its own
   * diagnostics.
   */
  private async settleSensorCycle(cycle: Promise<void> | null): Promise<GpuSensorMap | null> {
    if (cycle === null || this.sensorSource === null) return null;
    let timer: NodeJS.Timeout | undefined;
    const budget = new Promise<'timeout'>((resolve) => {
      timer = setTimeout(() => resolve('timeout'), this.timeoutMs);
      timer.unref?.();
    });
    const outcome = await Promise.race([
      cycle.then(() => 'settled' as const, () => 'failed' as const),
      budget,
    ]);
    clearTimeout(timer);
    if (outcome === 'timeout') {
      // Same once-per-state-change discipline as the diagnostics elsewhere.
      if (!this.settleTimeoutEmitted) {
        this.settleTimeoutEmitted = true;
        this.emit('gpu register temps: cycle did not settle in time');
      }
    } else {
      this.settleTimeoutEmitted = false;
    }
    return this.sensorSource.latest();
  }

  private async pollComputeApps(): Promise<void> {
    try {
      const stdout = await this.exec(APP_QUERY);
      const apps = new Map<number, number>();
      for (const app of parseComputeAppsCsv(stdout)) {
        apps.set(app.pid, (apps.get(app.pid) ?? 0) + app.usedBytes);
      }
      this.apps = apps;
    } catch (error) {
      // GPU readings remain valid; only the process join is missing.
      this.emit(`gpu compute-app query failed: ${errorMessage(error)}`);
    }
  }

  private exec(args: string): Promise<string> {
    return new Promise<string>((resolve, reject) => {
      const startedAt = this.monotonicMs();
      this.stats.runs += 1;
      const finish = (error: ExecError | null, stdout: string, stderr: string): void => {
        const duration = Math.max(0, this.monotonicMs() - startedAt);
        this.recordDuration(duration);
        if (error !== null) {
          this.stats.failures += 1;
          const detail = stderr.trim() === '' ? error : `${errorMessage(error)}: ${stderr.trim()}`;
          reject(detail instanceof Error ? detail : new Error(String(detail)));
          return;
        }
        resolve(stdout);
      };
      try {
        this.execFile(
          this.command,
          [...args.split(' '), CSV_FORMAT],
          {timeout: this.timeoutMs, maxBuffer: MAX_BUFFER_BYTES, encoding: 'utf8'},
          finish,
        );
      } catch (error) {
        const duration = Math.max(0, this.monotonicMs() - startedAt);
        this.recordDuration(duration);
        this.stats.failures += 1;
        reject(error);
      }
    });
  }

  private recordDuration(duration: number): void {
    this.stats.lastDurationMs = duration;
    this.totalDurationMs += duration;
    this.stats.avgDurationMs = this.stats.runs === 0 ? 0 : this.totalDurationMs / this.stats.runs;
    if (duration > this.stats.maxDurationMs) this.stats.maxDurationMs = duration;
  }

  private applyRows(rows: ParsedGpuRow[]): void {
    const info = new Map<string, GpuDeviceInfo>();
    for (const row of rows) {
      info.set(row.uuid, {
        uuid: row.uuid,
        name: row.name,
        pciBusId: row.pciBusId,
        memoryTotalBytes: row.memoryTotalBytes,
      });
    }
    this.info = info;
  }

  private toReadings(rows: ParsedGpuRow[], sensors: GpuSensorMap | null): GpuReading[] {
    return rows.map((row) => {
      const reading: Extract<GpuReading, {status: 'ok'}> = {
        id: `gpu:${row.uuid}`,
        status: 'ok' as const,
        utilizationPercent: row.utilizationPercent,
        memoryUsedBytes: row.memoryUsedBytes,
        memoryTotalBytes: row.memoryTotalBytes,
        temperatureC: row.temperatureC,
        powerWatts: row.powerWatts,
      };
      const key = normalizeBusFunctionKey(row.pciBusId);
      const sensor = sensors !== null && key !== null ? sensors.get(key) : undefined;
      if (sensor !== undefined) {
        if (sensor.junctionTemperatureC !== null) {
          reading.junctionTemperatureC = sensor.junctionTemperatureC;
        }
        if (sensor.vramTemperatureC !== null) {
          reading.vramTemperatureC = sensor.vramTemperatureC;
        }
      }
      return reading;
    });
  }

  private emit(line: string): void {
    try {
      this.onEvent?.(line);
    } catch {
      // Diagnostics must never break polling.
    }
  }
}

/** Exposed for parity with the constructor and for diagnostics. */
export function gpuQueryArgs(): string[] {
  return [...GPU_QUERY.split(' '), CSV_FORMAT];
}
