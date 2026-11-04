/** Deterministic PSI, hwmon, RAPL and disk-latency telemetry tests. */
import test from 'node:test';
import assert from 'node:assert/strict';

import { parseTelemetryRecord } from '../src/contracts';
import type { MetricsRecord } from '../src/contracts';
import { createRaplPowerMeter, readCpuTemperature } from '../collector/sampling/hwmon';
import { createMetricsSampler } from '../collector/sampling/metrics';
import { parsePressure } from '../collector/sampling/pressure';
import { createSequenceAllocator } from '../collector/sampling/sequence';
import { readFilesystemUsage } from '../collector/sampling/storage';
import { createReaders, diskstats, meminfo, netDev, procStatLine } from './sampling-helpers';

const IDENTITY = {collectorId: 'collector-bottleneck', runId: 'run-bottleneck', bootId: 'boot-bottleneck'};

test('filesystems without an inode table omit inode readings', () => {
  for (const fsType of ['btrfs', 'vfat']) {
    const {reading} = readFilesystemUsage({id: 'fs:/fixture', mountPoint: '/fixture', device: '/dev/sda1', fsType},
      () => ({bsize: 4096, blocks: 100, bfree: 50, files: 0, ffree: 0}));
    assert.equal(reading.status, 'ok');
    assert.equal(Object.hasOwn(reading, 'inodesTotal'), false);
    assert.equal(Object.hasOwn(reading, 'inodesUsed'), false);
  }
});

function metrics(record: MetricsRecord): MetricsRecord {
  const parsed = parseTelemetryRecord(record);
  if (parsed.kind !== 'metrics') assert.fail('not a metrics record');
  return parsed;
}

test('PSI: parses some/full avg10, keeps missing lines null, rejects malformed files', () => {
  const both = parsePressure([
    'some avg10=4.25 avg60=2.00 avg300=1.00 total=123',
    'full avg10=1.50 avg60=0.50 avg300=0.25 total=45',
  ].join('\n'));
  assert.deepEqual(both, {someAvg10: 4.25, fullAvg10: 1.5});

  const cpu = parsePressure('some avg10=0.00 avg60=0.00 avg300=0.00 total=0');
  assert.deepEqual(cpu, {someAvg10: 0, fullAvg10: null});

  assert.deepEqual(parsePressure('some avg60=1.00 total=1'), {someAvg10: null, fullAvg10: null});
  assert.deepEqual(parsePressure('some avg10=nan total=1'), {someAvg10: null, fullAvg10: null});
  assert.deepEqual(parsePressure('some avg10=101 total=1'), {someAvg10: null, fullAvg10: null});
  assert.equal(parsePressure(''), null);
  assert.equal(parsePressure('garbage'), null);
});

test('hwmon: only CPU drivers and package/Tctl/core labels contribute', () => {
  const files: Record<string, string> = {
    '/sys/class/hwmon/hwmon0/name': 'nvme\n',
    '/sys/class/hwmon/hwmon0/temp1_label': 'Composite\n',
    '/sys/class/hwmon/hwmon0/temp1_input': '90000\n',
    '/sys/class/hwmon/hwmon1/name': 'k10temp\n',
    '/sys/class/hwmon/hwmon1/temp1_label': 'Tctl\n',
    '/sys/class/hwmon/hwmon1/temp1_input': '61500\n',
    '/sys/class/hwmon/hwmon1/temp3_label': 'Tccd1\n',
    '/sys/class/hwmon/hwmon1/temp3_input': '70000\n',
    '/sys/class/hwmon/hwmon2/name': 'coretemp\n',
    '/sys/class/hwmon/hwmon2/temp1_label': 'Package id 0\n',
    '/sys/class/hwmon/hwmon2/temp1_input': '60000\n',
    '/sys/class/hwmon/hwmon2/temp2_label': 'Core 0\n',
    '/sys/class/hwmon/hwmon2/temp2_input': '80000\n',
  };
  const readers = createReaders(files, {
    dirs: {
      '/sys/class/hwmon': ['hwmon2', 'hwmon0', 'hwmon1'],
      '/sys/class/hwmon/hwmon0': ['name', 'temp1_label', 'temp1_input'],
      '/sys/class/hwmon/hwmon1': ['name', 'temp1_label', 'temp1_input', 'temp3_label', 'temp3_input'],
      '/sys/class/hwmon/hwmon2': ['name', 'temp1_label', 'temp1_input', 'temp2_label', 'temp2_input'],
    },
  });
  // Package/Tctl beats core sensors; the hottest package/Tctl wins.
  assert.equal(readCpuTemperature(readers.readFile, readers.readdir), 61.5);

  const unrelated = createReaders(
    {
      '/sys/class/hwmon/hwmon0/name': 'bnxt_en\n',
      '/sys/class/hwmon/hwmon0/temp1_input': '95000\n',
    },
    {dirs: {'/sys/class/hwmon': ['hwmon0'], '/sys/class/hwmon/hwmon0': ['name', 'temp1_input']}},
  );
  assert.equal(readCpuTemperature(unrelated.readFile, unrelated.readdir), null);

  const unreadable = createReaders(
    {'/sys/class/hwmon/hwmon0/name': 'k10temp\n'},
    {dirs: {'/sys/class/hwmon': ['hwmon0'], '/sys/class/hwmon/hwmon0': ['name']}},
  );
  assert.equal(readCpuTemperature(unreadable.readFile, unreadable.readdir), null);
  assert.equal(readCpuTemperature(unreadable.readFile, () => {
    throw new Error('no /sys/class/hwmon');
  }), null);
  const empty = createReaders({'/sys/class/hwmon/hwmon0/name': 'k10temp\n', '/sys/class/hwmon/hwmon0/temp1_input': '\n'},
    {dirs: {'/sys/class/hwmon': ['hwmon0'], '/sys/class/hwmon/hwmon0': ['temp1_input']}});
  assert.equal(readCpuTemperature(empty.readFile, empty.readdir), null, 'empty sensor must not become zero degrees');
});

function raplFiles(energy: Record<string, number>): Record<string, string> {
  const files: Record<string, string> = {};
  for (const [domain, value] of Object.entries(energy)) {
    files[`/sys/class/powercap/${domain}/energy_uj`] = `${value}\n`;
    files[`/sys/class/powercap/${domain}/max_energy_range_uj`] = '10000000\n';
  }
  return files;
}

test('RAPL: first interval pending, per-package deltas, wrap and reset handling', () => {
  const files = raplFiles({'intel-rapl:0': 1_000_000});
  const readers = createReaders(files, {
    dirs: {'/sys/class/powercap': ['intel-rapl:0', 'intel-rapl:0:0']},
  });
  const meter = createRaplPowerMeter(readers.readFile, readers.readdir);
  assert.equal(meter.sample(0), null, 'first interval has no baseline');
  files['/sys/class/powercap/intel-rapl:0/energy_uj'] = '3000000\n';
  assert.equal(meter.sample(1_000), 2, '2 J over 1 s is 2 W');
  files['/sys/class/powercap/intel-rapl:0/energy_uj'] = '3500000\n';
  assert.equal(meter.sample(2_000), 0.5);
  files['/sys/class/powercap/intel-rapl:0/energy_uj'] = '3500000\n';
  assert.equal(meter.sample(2_000), null, 'monotonic elapsed must advance');

  // Genuine wrap: previous sample was at the range ceiling.
  const wrapFiles = raplFiles({'intel-rapl:0': 9_500_000});
  const wrapReaders = createReaders(wrapFiles, {dirs: {'/sys/class/powercap': ['intel-rapl:0']}});
  const wrapMeter = createRaplPowerMeter(wrapReaders.readFile, wrapReaders.readdir);
  assert.equal(wrapMeter.sample(0), null);
  wrapFiles['/sys/class/powercap/intel-rapl:0/energy_uj'] = '500000\n';
  assert.equal(wrapMeter.sample(1_000), 1, '(10M - 9.5M) + 0.5M = 1 J over 1 s');

  // Reset: a decrease from nowhere near the ceiling is not a wrap.
  const resetFiles = raplFiles({'intel-rapl:0': 200_000});
  const resetReaders = createReaders(resetFiles, {dirs: {'/sys/class/powercap': ['intel-rapl:0']}});
  const resetMeter = createRaplPowerMeter(resetReaders.readFile, resetReaders.readdir);
  assert.equal(resetMeter.sample(0), null);
  resetFiles['/sys/class/powercap/intel-rapl:0/energy_uj'] = '0\n';
  assert.equal(resetMeter.sample(1_000), null, 'reset is unknown, not a huge wrap');
  resetFiles['/sys/class/powercap/intel-rapl:0/energy_uj'] = '500000\n';
  assert.equal(resetMeter.sample(2_000), 0.5, 'baseline restarts after the reset');
});

test('RAPL: top-level packages sum, subdomains never do, unreadable energy is unknown', () => {
  const files = raplFiles({'intel-rapl:0': 0, 'intel-rapl:1': 0, 'intel-rapl:0:0': 0});
  const readers = createReaders(files, {
    dirs: {'/sys/class/powercap': ['intel-rapl:0', 'intel-rapl:1', 'intel-rapl:0:0']},
  });
  const meter = createRaplPowerMeter(readers.readFile, readers.readdir);
  assert.equal(meter.sample(0), null);
  files['/sys/class/powercap/intel-rapl:0/energy_uj'] = '2000000\n';
  files['/sys/class/powercap/intel-rapl:1/energy_uj'] = '1000000\n';
  files['/sys/class/powercap/intel-rapl:0:0/energy_uj'] = '5000000\n';
  assert.equal(meter.sample(1_000), 3);

  const unreadable = createReaders({}, {dirs: {'/sys/class/powercap': ['intel-rapl:0']}});
  const noEnergy = createRaplPowerMeter(unreadable.readFile, unreadable.readdir);
  assert.equal(noEnergy.sample(0), null);
  assert.equal(noEnergy.sample(1_000), null);

  const noDomains = createReaders({}, {dirs: {'/sys/class/powercap': []}});
  assert.equal(createRaplPowerMeter(noDomains.readFile, noDomains.readdir).sample(0), null);
});

test('metrics: temperature, package power, PSI, iowait, latency and utilization are bounded and honest', () => {
  const files: Record<string, string> = {
    '/proc/stat': [
      `cpu  ${procStatLine({user: 100, idle: 100, iowait: 20})}`,
      `cpu0 ${procStatLine({user: 50, idle: 50, iowait: 10})}`,
      `cpu1 ${procStatLine({user: 50, idle: 50, iowait: 10})}`,
    ].join('\n'),
    '/proc/meminfo': meminfo(),
    '/proc/cpuinfo': 'model name\t: Fixture CPU\n',
    '/etc/os-release': 'PRETTY_NAME="Fixture Linux"\n',
    '/proc/self/mountinfo': '49 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw',
    '/proc/diskstats': diskstats([
      ['sda', 100, 50, {readsCompleted: 10, readTimeMs: 100, writesCompleted: 10, writeTimeMs: 200, ioTimeMs: 100}],
    ]),
    '/proc/net/dev': netDev([['lo', 1_000, 2_000]]),
    '/proc/pressure/cpu': 'some avg10=2.50 avg60=1.00 avg300=0.50 total=1\n',
    '/proc/pressure/memory': 'some avg10=4.50 avg60=2.00 avg300=1.00 total=1\n',
    '/proc/pressure/io': [
      'some avg10=1.50 avg60=1.00 avg300=0.50 total=1',
      'full avg10=0.75 avg60=0.50 avg300=0.25 total=1',
    ].join('\n'),
    '/sys/class/hwmon/hwmon0/name': 'k10temp\n',
    '/sys/class/hwmon/hwmon0/temp1_label': 'Tctl\n',
    '/sys/class/hwmon/hwmon0/temp1_input': '61500\n',
    '/sys/class/powercap/intel-rapl:0/energy_uj': '1000000\n',
    '/sys/class/powercap/intel-rapl:0/max_energy_range_uj': '10000000\n',
    '/sys/block/sda/device/model': 'Fixture Disk\n',
    '/sys/block/sda/queue/rotational': '0\n',
    '/sys/block/sda/size': '2000\n',
  };
  const dirs: Record<string, string[]> = {
    '/sys/bus/pci/devices': [],
    '/sys/class/hwmon': ['hwmon0'],
    '/sys/class/hwmon/hwmon0': ['name', 'temp1_label', 'temp1_input'],
    '/sys/class/powercap': ['intel-rapl:0', 'intel-rapl:0:0'],
    '/sys/block': ['sda'],
    '/sys/class/net': ['lo'],
    '/sys/devices/virtual/net': ['lo'],
  };
  const readers = createReaders(files, {
    dirs,
    statfs: {'/': {bsize: 4096, blocks: 1000, bfree: 250, files: 1000, ffree: 750}},
    cpuCount: 2,
  });
  let clock = 0;
  const sampler = createMetricsSampler(IDENTITY, {
    sequence: createSequenceAllocator(),
    readers,
    now: () => 1_700_000_000_000 + clock,
    monotonicMs: () => clock,
  });

  const first = metrics(sampler.next());
  assert.equal(first.payload.cpu.status, 'unavailable', 'CPU usage still needs two readings');
  if (first.payload.memory.status === 'ok') {
    assert.equal(first.payload.memory.pressureSomePercent, 4.5);
    assert.equal(first.payload.memory.pressureFullPercent, null);
  } else {
    assert.fail('memory unavailable');
  }
  if (first.payload.disks.status === 'ok') {
    assert.equal(first.payload.disks.pressureSomePercent, 1.5);
    assert.equal(first.payload.disks.pressureFullPercent, 0.75);
    const fs = first.payload.disks.filesystems[0]!;
    if (fs.status !== 'ok') assert.fail('filesystem unavailable');
    assert.equal(fs.inodesUsed, 250);
    assert.equal(fs.inodesTotal, 1000);
  } else {
    assert.fail('disks unavailable');
  }

  clock = 1_000;
  files['/proc/stat'] = [
    `cpu  ${procStatLine({user: 200, idle: 190, iowait: 30})}`,
    `cpu0 ${procStatLine({user: 100, idle: 95, iowait: 15})}`,
    `cpu1 ${procStatLine({user: 100, idle: 95, iowait: 15})}`,
  ].join('\n');
  files['/proc/diskstats'] = diskstats([
    ['sda', 200, 100, {readsCompleted: 18, readTimeMs: 180, writesCompleted: 14, writeTimeMs: 220, ioTimeMs: 600}],
  ]);
  const second = metrics(sampler.next());
  const cpu = second.payload.cpu;
  if (cpu.status !== 'ok') assert.fail('cpu unavailable');
  assert.equal(cpu.usagePercent, 50);
  assert.equal(cpu.iowaitPercent, 5);
  assert.equal(cpu.pressureSomePercent, 2.5);
  assert.equal(cpu.temperatureC, 61.5);
  assert.equal(cpu.powerWatts, null, 'power waits for a previous RAPL sample');
  if (second.payload.disks.status !== 'ok') assert.fail('disks unavailable');
  const disk = second.payload.disks.devices[0]!;
  if (disk.status !== 'ok') assert.fail('disk unavailable');
  assert.equal(disk.readLatencyMs, 10);
  assert.equal(disk.writeLatencyMs, 5);
  assert.equal(disk.ioUtilizationPercent, 50);

  clock = 2_000;
  files['/proc/stat'] = [
    `cpu  ${procStatLine({user: 300, idle: 280, iowait: 40})}`,
    `cpu0 ${procStatLine({user: 150, idle: 140, iowait: 20})}`,
    `cpu1 ${procStatLine({user: 150, idle: 140, iowait: 20})}`,
  ].join('\n');
  files['/sys/class/powercap/intel-rapl:0/energy_uj'] = '3000000\n';
  const third = metrics(sampler.next());
  assert.equal(third.payload.cpu.status, 'ok');
  if (third.payload.cpu.status === 'ok') assert.equal(third.payload.cpu.powerWatts, 2);
  if (third.payload.disks.status !== 'ok') assert.fail('disks unavailable');
  const idleDisk = third.payload.disks.devices[0]!;
  if (idleDisk.status !== 'ok') assert.fail('disk unavailable');
  assert.equal(idleDisk.readLatencyMs, null, 'zero completed reads means unknown latency, not 0 ms');
  assert.equal(idleDisk.writeLatencyMs, null, 'zero completed writes means unknown latency, not 0 ms');
  assert.equal(idleDisk.ioUtilizationPercent, 0);
  assert.doesNotThrow(() => parseTelemetryRecord(third));

  // A reset of operation counters invalidates only the derived latency and
  // utilization for that interval; throughput stays useful and the baseline
  // recovers on the next sample.
  clock = 3_000;
  files['/proc/stat'] = [
    `cpu  ${procStatLine({user: 400, idle: 370, iowait: 50})}`,
    `cpu0 ${procStatLine({user: 200, idle: 185, iowait: 25})}`,
    `cpu1 ${procStatLine({user: 200, idle: 185, iowait: 25})}`,
  ].join('\n');
  files['/proc/diskstats'] = diskstats([
    ['sda', 250, 120, {readsCompleted: 5, readTimeMs: 50, writesCompleted: 4, writeTimeMs: 20, ioTimeMs: 100}],
  ]);
  const reset = metrics(sampler.next());
  if (reset.payload.disks.status !== 'ok') assert.fail('disks unavailable');
  const resetDisk = reset.payload.disks.devices[0]!;
  if (resetDisk.status !== 'ok') assert.fail('disk throughput should survive an optional-counter reset');
  assert.equal(resetDisk.readLatencyMs, null);
  assert.equal(resetDisk.writeLatencyMs, null);
  assert.equal(resetDisk.ioUtilizationPercent, null);
  assert.equal(resetDisk.readBytesPerSec, 25_600);
  assert.equal(resetDisk.writeBytesPerSec, 10_240);

  clock = 4_000;
  files['/proc/stat'] = [
    `cpu  ${procStatLine({user: 500, idle: 460, iowait: 60})}`,
    `cpu0 ${procStatLine({user: 250, idle: 230, iowait: 30})}`,
    `cpu1 ${procStatLine({user: 250, idle: 230, iowait: 30})}`,
  ].join('\n');
  files['/proc/diskstats'] = diskstats([
    ['sda', 300, 140, {readsCompleted: 7, readTimeMs: 70, writesCompleted: 6, writeTimeMs: 30, ioTimeMs: 300}],
  ]);
  const recovered = metrics(sampler.next());
  if (recovered.payload.disks.status !== 'ok') assert.fail('disks unavailable');
  const recoveredDisk = recovered.payload.disks.devices[0]!;
  if (recoveredDisk.status !== 'ok') assert.fail('disk did not recover');
  assert.equal(recoveredDisk.readLatencyMs, 10);
  assert.equal(recoveredDisk.writeLatencyMs, 5);
  assert.equal(recoveredDisk.ioUtilizationPercent, 20);
});
