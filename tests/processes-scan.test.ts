/**
 * ProcessScanner tests use a synthetic `/proc` tree (clearly not the host's)
 * for deterministic parsing, deltas, PID reuse, truncation, and permissions.
 * The final test is a read-only smoke scan of the real `/proc`.
 */
import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { parseProcessSnapshot } from '../src/contracts';
import { ProcessScanner } from '../collector/processes/scan';

const PASSWD = [
  'root:x:0:0:root:/root:/bin/bash',
  'alice:x:1000:1000:Alice:/home/alice:/bin/bash',
  'bob:x:1001:1001:Bob:/home/bob:/bin/bash',
].join('\n');

type ProcessFixture = {
  comm: string;
  state?: string;
  ppid?: number;
  utime?: number;
  stime?: number;
  threads?: number;
  starttime?: number;
  rssPages?: number;
  uid?: number;
  statusThreads?: number;
  io?: { readBytes: number; writeBytes: number } | null;
  cgroup?: string | null;
  cmdline?: string | null;
  omitStat?: boolean;
  statAsDirectory?: boolean;
  omitStatus?: boolean;
};

class ProcFixture {
  readonly root: string;
  readonly passwdPath: string;

  constructor() {
    this.root = mkdtempSync(join(tmpdir(), 'rigscope-proc-'));
    this.passwdPath = join(this.root, 'passwd');
    writeFileSync(this.passwdPath, PASSWD);
  }

  add(pid: number, fixture: ProcessFixture): void {
    const dir = join(this.root, String(pid));
    mkdirSync(dir, { recursive: true });
    if (!fixture.omitStat) {
      if (fixture.statAsDirectory) {
        mkdirSync(join(dir, 'stat'), { recursive: true });
      } else {
        writeFileSync(join(dir, 'stat'), statText(pid, fixture));
      }
    }
    if (!fixture.omitStatus) {
      const uid = fixture.uid ?? 1000;
      writeFileSync(
        join(dir, 'status'),
        `Name:\t${fixture.comm}\nUid:\t${uid}\t${uid}\t${uid}\t${uid}\nThreads:\t${fixture.statusThreads ?? fixture.threads ?? 1}\n`,
      );
    }
    if (fixture.io !== undefined && fixture.io !== null) {
      writeFileSync(join(dir, 'io'), `rchar: 0\nread_bytes: ${fixture.io.readBytes}\nwrite_bytes: ${fixture.io.writeBytes}\n`);
    }
    if (fixture.cgroup !== undefined && fixture.cgroup !== null) {
      writeFileSync(join(dir, 'cgroup'), fixture.cgroup);
    }
    if (fixture.cmdline !== undefined && fixture.cmdline !== null) {
      writeFileSync(join(dir, 'cmdline'), fixture.cmdline);
    }
  }

  remove(pid: number): void {
    rmSync(join(this.root, String(pid)), { recursive: true, force: true });
  }

  scanner(options: ConstructorParameters<typeof ProcessScanner>[0] = {}): ProcessScanner {
    return new ProcessScanner({ procRoot: this.root, passwdPath: this.passwdPath, ...options });
  }

  dispose(): void {
    rmSync(this.root, { recursive: true, force: true });
  }
}

function statText(pid: number, fixture: ProcessFixture): string {
  const rest = new Array<string>(50).fill('0');
  rest[0] = fixture.state ?? 'S';
  rest[1] = String(fixture.ppid ?? 1);
  rest[11] = String(fixture.utime ?? 0);
  rest[12] = String(fixture.stime ?? 0);
  rest[17] = String(fixture.threads ?? 1);
  rest[19] = String(fixture.starttime ?? 1000);
  rest[21] = String(fixture.rssPages ?? 10);
  return `${pid} (${fixture.comm}) ${rest.join(' ')}`;
}

function row(snapshot: Awaited<ReturnType<ProcessScanner['scan']>>, pid: number) {
  const found = snapshot.processes.find((process) => process.pid === pid);
  assert.ok(found, `expected pid ${pid} in snapshot`);
  return found;
}

test('parses comm containing spaces and parentheses, status, io, unit, and command', async () => {
  const fixture = new ProcFixture();
  try {
    fixture.add(100, {
      comm: 'my worker) (2',
      state: 'R',
      ppid: 1,
      utime: 10,
      stime: 5,
      threads: 1,
      statusThreads: 3,
      starttime: 1000,
      rssPages: 2,
      uid: 1000,
      io: { readBytes: 100, writeBytes: 200 },
      cgroup: '0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-gnome-firefox-1234.scope',
      cmdline: 'node\u0000server.js\u0000--flag',
    });

    const scanner = fixture.scanner({ monotonicMs: () => 0 });
    const snapshot = await scanner.scan({ includeCommand: true });
    const parsed = parseProcessSnapshot(snapshot);
    assert.equal(parsed.totalProcesses, 1);
    assert.equal(parsed.truncated, false);
    assert.equal(parsed.inaccessible, 0);

    const process = row(parsed, 100);
    assert.equal(process.name, 'my worker) (2');
    assert.equal(process.state, 'R');
    assert.equal(process.ppid, 1);
    assert.equal(process.startTicks, 1000);
    assert.equal(process.user, 'alice');
    assert.equal(process.threads, 3, 'status Threads wins over stat num_threads');
    assert.equal(process.memoryRssBytes, 2 * 4096);
    assert.equal(process.cpuPercent, null, 'first sighting never invents CPU');
    assert.equal(process.ioReadBytesPerSec, null);
    assert.equal(process.ioWriteBytesPerSec, null);
    assert.equal(process.unit, 'app-gnome-firefox-1234.scope');
    assert.equal(process.command, 'node server.js --flag');
    assert.deepEqual(scanner.lastScanStats(), { durationMs: 0, processes: 1, inaccessible: 0 });

    // The command field is absent unless explicitly requested.
    const withoutCommand = await scanner.scan({ includeCommand: false });
    assert.equal(Object.prototype.hasOwnProperty.call(row(withoutCommand, 100), 'command'), false);
  } finally {
    fixture.dispose();
  }
});

test('cpu and io rates use monotonic deltas; decreases and first sightings stay null', async () => {
  const fixture = new ProcFixture();
  try {
    let now = 0;
    fixture.add(200, {
      comm: 'worker',
      utime: 100,
      stime: 50,
      starttime: 5000,
      io: { readBytes: 1000, writeBytes: 2000 },
    });
    const scanner = fixture.scanner({ monotonicMs: () => now });

    const first = await scanner.scan({ includeCommand: false });
    assert.equal(row(first, 200).cpuPercent, null);
    assert.equal(row(first, 200).ioReadBytesPerSec, null);
    assert.equal(row(first, 200).ioWriteBytesPerSec, null);

    now = 1000;
    fixture.add(200, {
      comm: 'worker',
      utime: 150,
      stime: 50,
      starttime: 5000,
      io: { readBytes: 3000, writeBytes: 1000 },
    });
    const second = await scanner.scan({ includeCommand: false });
    assert.equal(row(second, 200).cpuPercent, 50, '50 ticks / 100 Hz over 1s = 50% of one core');
    assert.equal(row(second, 200).ioReadBytesPerSec, 2000);
    assert.equal(row(second, 200).ioWriteBytesPerSec, null, 'a counter decrease is not a rate');

    now = 2000;
    fixture.add(200, {
      comm: 'worker',
      utime: 150,
      stime: 50,
      starttime: 5000,
      io: { readBytes: 3000, writeBytes: 1000 },
    });
    const third = await scanner.scan({ includeCommand: false });
    assert.equal(row(third, 200).cpuPercent, 0, 'a stable counter is an honest zero');
    assert.equal(row(third, 200).ioReadBytesPerSec, 0);
    assert.equal(row(third, 200).ioWriteBytesPerSec, 0);
  } finally {
    fixture.dispose();
  }
});

test('PID reuse never produces a delta; vanished identities are pruned', async () => {
  const fixture = new ProcFixture();
  try {
    let now = 0;
    fixture.add(300, {
      comm: 'reused',
      utime: 100,
      starttime: 100,
      io: { readBytes: 1000, writeBytes: 1000 },
    });
    const scanner = fixture.scanner({ monotonicMs: () => now });
    await scanner.scan({ includeCommand: false });

    now = 1000;
    fixture.add(300, {
      comm: 'reused',
      utime: 200,
      starttime: 200,
      io: { readBytes: 5000, writeBytes: 5000 },
    });
    const reused = await scanner.scan({ includeCommand: false });
    assert.equal(row(reused, 300).cpuPercent, null, 'new startTicks is a new identity');
    assert.equal(row(reused, 300).ioReadBytesPerSec, null);

    now = 2000;
    fixture.add(300, {
      comm: 'reused',
      utime: 250,
      starttime: 200,
      io: { readBytes: 6000, writeBytes: 6000 },
    });
    const reusedDelta = await scanner.scan({ includeCommand: false });
    assert.equal(row(reusedDelta, 300).cpuPercent, 50);
    assert.equal(row(reusedDelta, 300).ioReadBytesPerSec, 1000);

    // Vanishing prunes the baseline, so the same identity reappearing later
    // is a first sighting again even if the kernel reuses the same startTicks.
    fixture.remove(300);
    const empty = await scanner.scan({ includeCommand: false });
    assert.equal(empty.totalProcesses, 0);

    now = 3000;
    fixture.add(300, {
      comm: 'reused',
      utime: 300,
      starttime: 200,
      io: { readBytes: 7000, writeBytes: 7000 },
    });
    const reappeared = await scanner.scan({ includeCommand: false });
    assert.equal(row(reappeared, 300).cpuPercent, null);
    assert.equal(row(reappeared, 300).ioReadBytesPerSec, null);
  } finally {
    fixture.dispose();
  }
});

test('disappearing processes are skipped, inaccessible rows are counted, missing io is not inaccessible', async () => {
  const fixture = new ProcFixture();
  try {
    fixture.add(400, { comm: 'vanished', omitStat: true });
    fixture.add(401, { comm: 'unreadable', statAsDirectory: true });
    fixture.add(402, { comm: 'healthy', omitStatus: false });
    fixture.add(403, { comm: 'status-vanished', omitStatus: true });
    fixture.add(404, { comm: 'no-io', io: null, cgroup: null, cmdline: null });

    const scanner = fixture.scanner({ monotonicMs: () => 0 });
    const snapshot = await scanner.scan({ includeCommand: false });
    parseProcessSnapshot(snapshot);
    assert.equal(snapshot.totalProcesses, 2);
    assert.equal(snapshot.inaccessible, 1);
    assert.deepEqual(
      snapshot.processes.map((process) => process.pid).sort((a, b) => a - b),
      [402, 404],
    );
    assert.equal(row(snapshot, 404).ioReadBytesPerSec, null);
    assert.equal(row(snapshot, 404).ioWriteBytesPerSec, null);
    assert.equal(scanner.lastScanStats().inaccessible, 1);
  } finally {
    fixture.dispose();
  }
});

test('cgroup units require a unified path and a .service/.scope leaf; GPU memory is opt-in', async () => {
  const fixture = new ProcFixture();
  try {
    fixture.add(600, {
      comm: 'firefox',
      cgroup: '0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-gnome-firefox-1234.scope\n',
    });
    fixture.add(601, {
      comm: 'worker',
      cgroup: '12:pids:/foo.slice\n0::/foo.slice/bar.slice\n',
    });
    fixture.add(602, { comm: 'service', cgroup: '0::/system.slice/example.service\n' });
    fixture.add(603, { comm: 'none', cgroup: null });

    const scanner = fixture.scanner({
      monotonicMs: () => 0,
      gpuMemoryByPid: () => new Map([[600, 1234], [601, -1]]),
    });
    const snapshot = await scanner.scan({ includeCommand: false });
    assert.equal(row(snapshot, 600).unit, 'app-gnome-firefox-1234.scope');
    assert.equal(row(snapshot, 600).gpuMemoryBytes, 1234);
    assert.equal(row(snapshot, 601).unit, null);
    assert.equal(row(snapshot, 601).gpuMemoryBytes, null, 'negative GPU memory is not a value');
    assert.equal(row(snapshot, 602).unit, 'example.service');
    assert.equal(row(snapshot, 603).unit, null);
  } finally {
    fixture.dispose();
  }
});

test('truncation keeps the highest CPU then RSS rows and reports the full count', async () => {
  const fixture = new ProcFixture();
  try {
    let now = 0;
    for (const pid of [700, 701, 702]) {
      fixture.add(pid, { comm: `p${pid}`, utime: 0, starttime: 7000 + pid, rssPages: pid - 700 + 1 });
    }
    const scanner = fixture.scanner({ monotonicMs: () => now, maxRows: 2 });
    await scanner.scan({ includeCommand: false });

    now = 1000;
    fixture.add(700, { comm: 'p700', utime: 10, starttime: 7700, rssPages: 1 });
    fixture.add(701, { comm: 'p701', utime: 30, starttime: 7701, rssPages: 2 });
    fixture.add(702, { comm: 'p702', utime: 20, starttime: 7702, rssPages: 3 });
    const snapshot = await scanner.scan({ includeCommand: false });
    const parsed = parseProcessSnapshot(snapshot);
    assert.equal(parsed.totalProcesses, 3);
    assert.equal(parsed.truncated, true);
    assert.deepEqual(parsed.processes.map((process) => process.pid), [701, 702]);
    assert.equal(parsed.processes[0].cpuPercent, 30);
    assert.equal(parsed.processes[1].cpuPercent, 20);
  } finally {
    fixture.dispose();
  }
});

test('kernel threads are ordinary rows with user root; command lines are capped and NUL-separated', async () => {
  const fixture = new ProcFixture();
  try {
    fixture.add(2, { comm: 'kthreadd', ppid: 0, uid: 0, starttime: 2, omitStat: false });
    fixture.add(3, { comm: 'kworker/0:1', ppid: 2, uid: 0, starttime: 3 });
    fixture.add(800, {
      comm: 'longcmd',
      uid: 0,
      starttime: 800,
      cmdline: `${'a'.repeat(5000)}\u0000b`,
    });
    fixture.add(801, { comm: 'simple', cmdline: 'echo\u0000hi\u0000', uid: 1001 });

    const scanner = fixture.scanner({ monotonicMs: () => 0 });
    const snapshot = await scanner.scan({ includeCommand: true });
    parseProcessSnapshot(snapshot);
    assert.equal(row(snapshot, 2).user, 'root');
    assert.equal(row(snapshot, 3).user, 'root');
    assert.equal(row(snapshot, 800).command?.length, 4096);
    assert.equal(/^a+$/.test(row(snapshot, 800).command ?? ''), true);
    assert.equal(row(snapshot, 801).command, 'echo hi');
    assert.equal(row(snapshot, 801).user, 'bob');
  } finally {
    fixture.dispose();
  }
});

test('real-host smoke: two read-only /proc scans produce contract-valid rows', { timeout: 60_000 }, async () => {
  const scanner = new ProcessScanner();
  const first = await scanner.scan({ includeCommand: false });
  const firstStats = scanner.lastScanStats();
  parseProcessSnapshot(first);
  assert.ok(first.totalProcesses > 50, `expected a real host with >50 processes, saw ${first.totalProcesses}`);

  const second = await scanner.scan({ includeCommand: true });
  const secondStats = scanner.lastScanStats();
  parseProcessSnapshot(second);
  assert.ok(second.totalProcesses > 50);
  assert.ok(secondStats.durationMs >= 0);

  // Deliberate output: this is the number reported in the handover.
  console.log(
    `real /proc scans: ${first.totalProcesses} processes in ${firstStats.durationMs.toFixed(1)} ms; ` +
      `${second.totalProcesses} processes in ${secondStats.durationMs.toFixed(1)} ms`,
  );
});
