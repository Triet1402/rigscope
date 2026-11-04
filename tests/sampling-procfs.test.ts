/** Pure procfs/sysfs parsing fixtures for sampling. */
import test from 'node:test';
import assert from 'node:assert/strict';

import {
  parseCpuinfoModel,
  parseDiskstats,
  parseMeminfo,
  parseMountinfo,
  parseNetDev,
  parseOsRelease,
  parseProcStat,
} from '../collector/sampling/procfs';
import { meminfo, procStatLine } from './sampling-helpers';

test('parseProcStat: aggregate and per-core busy/idle counters', () => {
  const text = [
    `cpu  ${procStatLine({user: 100, nice: 10, system: 50, idle: 1_000, iowait: 20, irq: 5, softirq: 7, steal: 3, guest: 40, guestNice: 2})}`,
    `cpu0 ${procStatLine({user: 60, nice: 0, system: 25, idle: 500, iowait: 10, irq: 2, softirq: 3, steal: 1})}`,
    `cpu1 ${procStatLine({user: 40, nice: 10, system: 25, idle: 500, iowait: 10, irq: 3, softirq: 4, steal: 2})}`,
  ].join('\n');
  const snapshot = parseProcStat(text);
  assert.ok(snapshot);
  assert.equal(snapshot.coreCount, 2);
  // guest/guestNice are already included in user, so they are not added again.
  assert.deepEqual(snapshot.total, {busy: 175, idle: 1_020, iowait: 20});
  assert.deepEqual(snapshot.perCore[0], {index: 0, busy: 91, idle: 510, iowait: 10});
  assert.deepEqual(snapshot.perCore[1], {index: 1, busy: 84, idle: 510, iowait: 10});
});

test('parseProcStat: malformed, empty and negative fixtures stay null', () => {
  assert.equal(parseProcStat(''), null);
  assert.equal(parseProcStat('cpu 1 2 3\n'), null);
  assert.equal(parseProcStat('cpu  1 2 3 4 5 6 7 8\ncpu0 -1 0 0 0 0 0 0 0\n'), null);
  assert.equal(parseProcStat('not a stat file'), null);
});

test('parseMeminfo: memory and swap kB fields become exact bytes', () => {
  const snapshot = parseMeminfo(meminfo());
  assert.ok(snapshot);
  assert.equal(snapshot.totalBytes, 1_000 * 1024);
  assert.equal(snapshot.availableBytes, 400 * 1024);
  assert.equal(snapshot.swapTotalBytes, 100 * 1024);
  assert.equal(snapshot.swapFreeBytes, 60 * 1024);

  const noSwap = parseMeminfo(meminfo({SwapTotal: 0, SwapFree: 0}));
  assert.equal(noSwap?.swapTotalBytes, 0);
  assert.equal(noSwap?.swapFreeBytes, 0);

  assert.equal(parseMeminfo('MemTotal: 100 MB\n'), null);
  assert.equal(parseMeminfo(''), null);
});

test('parseMountinfo: optional fields, separator and escaped paths', () => {
  const entries = parseMountinfo([
    '35 49 0:31 / /run rw,nosuid shared:13 - tmpfs tmpfs rw',
    '49 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw',
    '70 49 8:18 / /srv/my\\040storage rw,noatime - btrfs /dev/sdb2 rw,subvolid=5',
  ].join('\n'));
  assert.equal(entries.length, 3);
  assert.deepEqual(entries[0], {
    mountPoint: '/run',
    device: 'tmpfs',
    fsType: 'tmpfs',
    majorMinor: '0:31',
    root: '/',
  });
  assert.equal(entries[1]!.mountPoint, '/');
  assert.equal(entries[1]!.device, '/dev/nvme0n1p2');
  assert.equal(entries[1]!.fsType, 'ext4');
  assert.equal(entries[2]!.mountPoint, '/srv/my storage');
  assert.equal(entries[2]!.fsType, 'btrfs');
});

test('parseNetDev and parseDiskstats keep cumulative byte/sector counters', () => {
  const net = parseNetDev([
    'Inter-|   Receive                                                |  Transmit',
    ' face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed',
    '    lo: 100 1 0 0 0 0 0 0 200 1 0 0 0 0 0 0',
    'eno1np0: 300 2 0 0 0 0 0 0 400 2 0 0 0 0 0 0',
  ].join('\n'));
  assert.deepEqual(net.get('lo'), {rxBytes: 100, txBytes: 200});
  assert.deepEqual(net.get('eno1np0'), {rxBytes: 300, txBytes: 400});
  assert.equal(parseNetDev('garbage').size, 0);

  const disks = parseDiskstats([
    '   8       0 sda 10 0 1234 5 10 0 5678 6 0 0 0',
    ' 259       0 nvme0n1 10 0 9999 5 10 0 8888 6 0 0 0',
    '  bad line',
  ].join('\n'));
  assert.deepEqual(disks.get('sda'), {
    readSectors: 1234,
    writeSectors: 5678,
    readOps: 10,
    writeOps: 10,
    readTimeMs: 5,
    writeTimeMs: 6,
    ioTimeMs: 0,
  });
  assert.deepEqual(disks.get('nvme0n1'), {
    readSectors: 9999,
    writeSectors: 8888,
    readOps: 10,
    writeOps: 10,
    readTimeMs: 5,
    writeTimeMs: 6,
    ioTimeMs: 0,
  });
  assert.equal(disks.size, 2);

  const full = parseDiskstats('   8       0 sda 20 0 2000 30 40 0 4000 50 0 900 10 0 0 0').get('sda');
  assert.deepEqual(full, {
    readSectors: 2000,
    writeSectors: 4000,
    readOps: 20,
    writeOps: 40,
    readTimeMs: 30,
    writeTimeMs: 50,
    ioTimeMs: 900,
  });
});

test('CPU model and distro parsers tolerate real-world formatting', () => {
  assert.equal(
    parseCpuinfoModel('processor\t: 0\nmodel name\t: AMD EPYC 7402P 24-Core Processor\n'),
    'AMD EPYC 7402P 24-Core Processor',
  );
  assert.equal(parseCpuinfoModel('processor : 0'), null);
  assert.equal(parseOsRelease('NAME="Ubuntu"\nPRETTY_NAME="Ubuntu 26.10"\n'), 'Ubuntu 26.10');
  assert.equal(parseOsRelease("PRETTY_NAME='Fixture Linux'\n"), 'Fixture Linux');
  assert.equal(parseOsRelease('NAME=Nothing'), null);
});

test('CPU model parser supports ARM and RISC-V without treating core indexes as names', () => {
  assert.equal(parseCpuinfoModel('processor : 0\nHardware : BCM2835\n'), 'BCM2835');
  assert.equal(parseCpuinfoModel('processor : 0\nuarch : sifive,u74-mc\n'), 'sifive,u74-mc');
  assert.equal(parseCpuinfoModel('Processor : AArch64 Processor rev 1\n'), 'AArch64 Processor rev 1');
  assert.equal(parseCpuinfoModel('Hardware : board\nmodel name : Cortex-A76\n'), 'Cortex-A76');
});
