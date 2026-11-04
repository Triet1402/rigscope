/**
 * Real Linux process scanning for the collector (`/proc`).
 *
 * The scanner is deliberately Node-only and read-only: it never signals a
 * process, writes to `/proc`, or spawns a helper on the scan path. All reads
 * are asynchronous and bounded by a small worker pool so a host with ~700
 * processes does not stall the collector's event loop for the whole scan.
 *
 * Rates are deltas between two scans of the *same process identity*
 * (`pid` + kernel `startTicks`), normalized by elapsed monotonic time. A first
 * sighting, a vanished process, PID reuse, a counter reset, or a non-positive
 * elapsed interval yields `null` rather than a fabricated zero. CPU follows
 * the contract convention: 100 = one fully busy core, so a multithreaded
 * process may exceed 100.
 *
 * Values the kernel does not expose (GPU memory, process I/O for an
 * unreadable `io` file) stay `null`; an unreadable `io` file is *not* counted
 * as an inaccessible process because the rest of the row is still usable.
 */
import { promises as fsp } from 'node:fs';

import { MAX_CORES, MAX_PROCESS_ROWS, type ProcessRow, type ProcessSnapshot } from '../../src/contracts';

export type ProcessScannerOptions = {
  /** Root of the proc filesystem; default `/proc`. Tests pass a fixture root. */
  procRoot?: string;
  /** Password database used for real-UID to user-name lookup; default `/etc/passwd`. */
  passwdPath?: string;
  /**
   * Kernel clock ticks per second (USER_HZ). The default 100 matches Linux on
   * every supported architecture in practice. It is an explicit option so a
   * caller may override it; the scanner never spawns `getconf` on the hot
   * path (or anywhere else).
   */
  clockTicksPerSecond?: number;
  /** Page size used to convert stat RSS pages to bytes; default 4096. */
  pageSize?: number;
  /** Optional per-PID GPU memory lookup (bytes already summed across GPUs). */
  gpuMemoryByPid?: () => Map<number, number> | null;
  /** Monotonic clock in milliseconds; default `performance.now`. */
  monotonicMs?: () => number;
  /** Maximum rows returned; default and hard cap `MAX_PROCESS_ROWS`. */
  maxRows?: number;
  /** Parallel process readers; default 32. */
  concurrency?: number;
};

export type ProcessScanStats = {
  /** Wall-independent duration of the last completed scan, according to `monotonicMs`. */
  durationMs: number;
  /** Processes read successfully (before truncation); equals `snapshot.totalProcesses`. */
  processes: number;
  /** Processes whose `stat`/`status` could not be read for a reason other than disappearance. */
  inaccessible: number;
};

type PreviousCounters = {
  utime: number;
  stime: number;
  readBytes: number | null;
  writeBytes: number | null;
  /** Monotonic timestamp when these counters were read. */
  atMs: number;
};

type ReadText = { ok: true; text: string } | { ok: false; code: string | null };

type RowResult =
  | { kind: 'row'; index: number; row: ProcessRow; key: string; previous: PreviousCounters }
  | { kind: 'skip' }
  | { kind: 'inaccessible' };

const DEFAULT_CLOCK_TICKS = 100;
const DEFAULT_PAGE_SIZE = 4096;
const DEFAULT_CONCURRENCY = 32;
const MAX_NAME_LENGTH = 256;
const MAX_USER_LENGTH = 64;
const MAX_UNIT_LENGTH = 256;
const MAX_STATE_LENGTH = 16;
const MAX_COMMAND_LENGTH = 4096;
const MAX_IO_BYTES_PER_SEC = 1e13;
const MAX_CPU_PERCENT = 100 * MAX_CORES;

/** Linux errno values that mean "the process went away while we read it". */
const VANISHED_CODES = new Set(['ENOENT', 'ESRCH', 'ENOTDIR']);

export class ProcessScanner {
  private readonly procRoot: string;
  private readonly passwdPath: string;
  private readonly clockTicksPerSecond: number;
  private readonly pageSize: number;
  private readonly gpuMemoryByPid: (() => Map<number, number> | null) | null;
  private readonly monotonicMs: () => number;
  private readonly maxRows: number;
  private readonly concurrency: number;

  private passwd: Map<number, string> | null = null;
  private previous = new Map<string, PreviousCounters>();
  private lastStats: ProcessScanStats = { durationMs: 0, processes: 0, inaccessible: 0 };

  constructor(options: ProcessScannerOptions = {}) {
    this.procRoot = options.procRoot ?? '/proc';
    this.passwdPath = options.passwdPath ?? '/etc/passwd';
    this.clockTicksPerSecond = positiveFinite(options.clockTicksPerSecond, DEFAULT_CLOCK_TICKS);
    this.pageSize = positiveFinite(options.pageSize, DEFAULT_PAGE_SIZE);
    this.gpuMemoryByPid = options.gpuMemoryByPid ?? null;
    this.monotonicMs = options.monotonicMs ?? (() => performance.now());
    this.maxRows = Math.max(1, Math.min(MAX_PROCESS_ROWS, Math.floor(positiveFinite(options.maxRows, MAX_PROCESS_ROWS))));
    this.concurrency = Math.max(1, Math.floor(positiveFinite(options.concurrency, DEFAULT_CONCURRENCY)));
  }

  lastScanStats(): ProcessScanStats {
    return { ...this.lastStats };
  }

  /**
   * Read one process snapshot.
   *
   * `includeCommand` controls only whether `/proc/<pid>/cmdline` is read and
   * the optional `command` field is populated. The scanner always reads the
   * same metrics so a shared scan can serve destinations with different
   * argument policies.
   */
  async scan(options: { includeCommand: boolean }): Promise<ProcessSnapshot> {
    const startedAt = this.monotonicMs();
    const includeCommand = options?.includeCommand === true;

    const entries = await fsp.readdir(this.procRoot, { withFileTypes: true });
    const pids: number[] = [];
    for (const entry of entries) {
      if (!/^\d+$/.test(entry.name)) continue;
      const pid = Number(entry.name);
      if (Number.isSafeInteger(pid) && pid >= 0 && pid <= 2 ** 31) pids.push(pid);
    }
    // Deterministic worker input order also makes truncation ties stable.
    pids.sort((a, b) => a - b);

    await this.ensurePasswd();
    const gpuMemory = this.readGpuMemory();

    const results: Array<RowResult | null> = new Array(pids.length).fill(null);
    let cursor = 0;
    const worker = async (): Promise<void> => {
      for (;;) {
        const index = cursor++;
        if (index >= pids.length) return;
        results[index] = await this.readProcess(pids[index], includeCommand, gpuMemory);
      }
    };
    await Promise.all(Array.from({ length: Math.min(this.concurrency, pids.length) }, worker));

    const rows: ProcessRow[] = [];
    const nextPrevious = new Map<string, PreviousCounters>();
    let inaccessible = 0;
    for (const result of results) {
      if (result === null) continue;
      if (result.kind === 'inaccessible') {
        inaccessible++;
      } else if (result.kind === 'row') {
        rows.push(result.row);
        nextPrevious.set(result.key, result.previous);
      }
    }
    // Pruning by replacement (instead of delete-many) keeps memory bounded by
    // the processes that actually exist now, which also makes PID reuse safe.
    this.previous = nextPrevious;

    const totalProcesses = rows.length;
    const truncated = rows.length > this.maxRows;
    let processes = rows;
    if (truncated) {
      processes = rows
        .slice()
        .sort(
          (a, b) =>
            (b.cpuPercent ?? 0) - (a.cpuPercent ?? 0) ||
            (b.memoryRssBytes ?? -1) - (a.memoryRssBytes ?? -1) ||
            a.pid - b.pid,
        )
        .slice(0, this.maxRows);
    }

    const durationMs = Math.max(0, this.monotonicMs() - startedAt);
    this.lastStats = { durationMs, processes: totalProcesses, inaccessible };

    return { totalProcesses, truncated, inaccessible, processes };
  }

  private async readProcess(
    pid: number,
    includeCommand: boolean,
    gpuMemory: Map<number, number> | null,
  ): Promise<RowResult> {
    const dir = `${this.procRoot}/${pid}`;
    const nowMs = this.monotonicMs();

    const [statRead, statusRead, ioRead, cgroupRead, cmdlineRead] = await Promise.all([
      this.readText(`${dir}/stat`),
      this.readText(`${dir}/status`),
      this.readText(`${dir}/io`),
      this.readText(`${dir}/cgroup`),
      includeCommand ? this.readText(`${dir}/cmdline`) : Promise.resolve(null),
    ]);

    if (!statRead.ok) return VANISHED_CODES.has(statRead.code ?? '') ? { kind: 'skip' } : { kind: 'inaccessible' };
    if (!statusRead.ok) return VANISHED_CODES.has(statusRead.code ?? '') ? { kind: 'skip' } : { kind: 'inaccessible' };

    const stat = parseStat(pid, statRead.text);
    if (!stat) return { kind: 'inaccessible' };

    const status = parseStatus(statusRead.text);
    const io = ioRead.ok ? parseIo(ioRead.text) : null;
    const unit = cgroupRead.ok ? parseCgroupUnit(cgroupRead.text) : null;
    const command = includeCommand && cmdlineRead?.ok ? parseCommandLine(cmdlineRead.text) : undefined;

    const key = `${pid}:${stat.startTicks}`;
    const previous = this.previous.get(key);
    const elapsedSeconds = previous ? (nowMs - previous.atMs) / 1000 : 0;

    let cpuPercent: number | null = null;
    if (previous && elapsedSeconds > 0) {
      const deltaTicks = stat.utime + stat.stime - (previous.utime + previous.stime);
      if (deltaTicks >= 0) {
        cpuPercent = clamp((deltaTicks / this.clockTicksPerSecond / elapsedSeconds) * 100, 0, MAX_CPU_PERCENT);
      }
    }

    const ioReadBytesPerSec = rate(io?.readBytes ?? null, previous?.readBytes ?? null, elapsedSeconds);
    const ioWriteBytesPerSec = rate(io?.writeBytes ?? null, previous?.writeBytes ?? null, elapsedSeconds);

    const user = status.uid === null ? null : this.userFor(status.uid);
    const row: ProcessRow = {
      pid,
      ppid: stat.ppid,
      startTicks: stat.startTicks,
      name: truncate(stat.name, MAX_NAME_LENGTH),
      user,
      state: truncate(stat.state, MAX_STATE_LENGTH),
      cpuPercent,
      memoryRssBytes: stat.rssPages === null ? null : bytesFromPages(stat.rssPages, this.pageSize),
      gpuMemoryBytes: gpuBytes(gpuMemory?.get(pid)),
      ioReadBytesPerSec,
      ioWriteBytesPerSec,
      threads: status.threads ?? stat.threads,
      unit: unit === null ? null : truncate(unit, MAX_UNIT_LENGTH),
    };
    if (command !== undefined) row.command = truncate(command, MAX_COMMAND_LENGTH);

    return {
      kind: 'row',
      index: pid,
      row,
      key,
      previous: {
        utime: stat.utime,
        stime: stat.stime,
        readBytes: io?.readBytes ?? null,
        writeBytes: io?.writeBytes ?? null,
        atMs: nowMs,
      },
    };
  }

  private async readText(path: string): Promise<ReadText> {
    try {
      return { ok: true, text: await fsp.readFile(path, 'utf8') };
    } catch (error) {
      return { ok: false, code: errorCode(error) };
    }
  }

  private async ensurePasswd(): Promise<void> {
    if (this.passwd !== null) return;
    try {
      const text = await fsp.readFile(this.passwdPath, 'utf8');
      const users = new Map<number, string>();
      for (const line of text.split('\n')) {
        if (line.length === 0 || line.startsWith('#')) continue;
        const fields = line.split(':');
        if (fields.length < 3) continue;
        const uid = Number(fields[2]);
        if (!Number.isSafeInteger(uid) || uid < 0) continue;
        if (!users.has(uid)) users.set(uid, fields[0]);
      }
      this.passwd = users;
    } catch {
      // Leave the cache empty so a transient failure can recover on a later
      // scan. UID 0 still maps to root below, which covers kernel threads.
    }
  }

  private userFor(uid: number): string | null {
    const name = this.passwd?.get(uid);
    if (name !== undefined && name.length > 0) return truncate(name, MAX_USER_LENGTH);
    if (uid === 0) return 'root';
    return null;
  }

  private readGpuMemory(): Map<number, number> | null {
    if (!this.gpuMemoryByPid) return null;
    try {
      return this.gpuMemoryByPid();
    } catch {
      return null;
    }
  }
}

type ParsedStat = {
  name: string;
  state: string;
  ppid: number;
  utime: number;
  stime: number;
  threads: number | null;
  startTicks: number;
  rssPages: number | null;
};

/**
 * Parse `/proc/<pid>/stat` without trusting the command name.
 *
 * `comm` is wrapped in parentheses and may itself contain spaces and `)`.
 * Splitting at the *last* `)` is the only safe boundary because the rest of
 * the line contains no parentheses. Fields after it start at field 3 (state).
 */
function parseStat(pid: number, text: string): ParsedStat | null {
  const open = text.indexOf('(');
  const close = text.lastIndexOf(')');
  if (open < 0 || close <= open) return null;
  const pidText = text.slice(0, open).trim();
  if (pidText !== String(pid)) return null;

  const rest = text.slice(close + 1).trim().split(/\s+/);
  // rest[0] = field 3 (state); field N is rest[N - 3].
  const state = rest[0];
  const ppid = safeInt(rest[1], 0, 2 ** 31);
  const utime = safeInt(rest[11], 0, Number.MAX_SAFE_INTEGER);
  const stime = safeInt(rest[12], 0, Number.MAX_SAFE_INTEGER);
  const threads = safeInt(rest[17], 0, 10_000_000);
  const startTicks = safeInt(rest[19], 0, Number.MAX_SAFE_INTEGER);
  const rssRaw = safeInt(rest[21], -Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER);
  if (state === undefined || state.length === 0 || ppid === null || utime === null || stime === null || startTicks === null) {
    return null;
  }
  return {
    name: text.slice(open + 1, close),
    state,
    ppid,
    utime,
    stime,
    threads,
    startTicks,
    rssPages: rssRaw !== null && rssRaw >= 0 ? rssRaw : null,
  };
}

function parseStatus(text: string): { uid: number | null; threads: number | null } {
  const uidMatch = /^Uid:\s+(\d+)/m.exec(text);
  const threadsMatch = /^Threads:\s+(\d+)/m.exec(text);
  return {
    uid: uidMatch ? safeInt(uidMatch[1], 0, 2 ** 31) : null,
    threads: threadsMatch ? safeInt(threadsMatch[1], 0, 10_000_000) : null,
  };
}

function parseIo(text: string): { readBytes: number | null; writeBytes: number | null } | null {
  const read = /^read_bytes:\s+(\d+)\s*$/m.exec(text);
  const write = /^write_bytes:\s+(\d+)\s*$/m.exec(text);
  if (!read && !write) return null;
  return {
    readBytes: read ? safeInt(read[1], 0, Number.MAX_SAFE_INTEGER) : null,
    writeBytes: write ? safeInt(write[1], 0, Number.MAX_SAFE_INTEGER) : null,
  };
}

/**
 * Extract the systemd unit from the unified cgroup path (`0::/...`). Only the
 * final component is considered, and only `.service`/`.scope` units count;
 * hierarchy slices and arbitrary cgroup names yield `null` rather than a
 * guessed unit.
 */
function parseCgroupUnit(text: string): string | null {
  for (const line of text.split('\n')) {
    if (!line.startsWith('0::')) continue;
    const path = line.slice(3);
    const components = path.split('/').filter((part) => part.length > 0);
    const last = components[components.length - 1];
    if (!last) return null;
    if (!last.endsWith('.service') && !last.endsWith('.scope')) return null;
    return last;
  }
  return null;
}

function parseCommandLine(text: string): string {
  return text.split('\u0000').join(' ').trim();
}

function rate(current: number | null, previous: number | null, elapsedSeconds: number): number | null {
  if (current === null || previous === null || elapsedSeconds <= 0) return null;
  const delta = current - previous;
  if (delta < 0) return null;
  return clamp(delta / elapsedSeconds, 0, MAX_IO_BYTES_PER_SEC);
}

function bytesFromPages(pages: number, pageSize: number): number | null {
  const bytes = pages * pageSize;
  return Number.isSafeInteger(bytes) && bytes >= 0 ? bytes : null;
}

function gpuBytes(value: number | undefined): number | null {
  if (value === undefined || !Number.isFinite(value) || value < 0) return null;
  const bytes = Math.trunc(value);
  return Number.isSafeInteger(bytes) ? bytes : null;
}

function positiveFinite(value: number | undefined, fallback: number): number {
  return value !== undefined && Number.isFinite(value) && value > 0 ? value : fallback;
}

function safeInt(value: string | undefined, min: number, max: number): number | null {
  if (value === undefined || !/^-?\d+$/.test(value)) return null;
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < min || parsed > max) return null;
  return parsed;
}

function clamp(value: number, min: number, max: number): number {
  if (!Number.isFinite(value)) return min;
  return Math.min(max, Math.max(min, value));
}

function truncate(value: string, max: number): string {
  return value.length > max ? value.slice(0, max) : value;
}

function errorCode(error: unknown): string | null {
  if (typeof error === 'object' && error !== null && 'code' in error) {
    const code = (error as { code?: unknown }).code;
    if (typeof code === 'string') return code;
  }
  return null;
}
