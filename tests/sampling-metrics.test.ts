/** Deterministic metric sampler tests (no subprocess, fake clocks). */
import test from 'node:test';
import assert from 'node:assert/strict';

import { parseInventory, parseTelemetryRecord } from '../src/contracts';
import type { CpuMetric, MetricsRecord } from '../src/contracts';
import { createMetricsSampler } from '../collector/sampling/metrics';
import { createSequenceAllocator } from '../collector/sampling/sequence';
import {
  createReaders,
  diskstats,
  fakeGpuPoller,
  meminfo,
  netDev,
  pciDeviceFiles,
  procStatLine,
} from './sampling-helpers';

const IDENTITY = {collectorId: 'collector-metrics', runId: 'run-metrics', bootId: 'boot-metrics'};

function cpuLine(totalBusy: number, totalIdle: number, core0Busy: number, core0Idle: number, core1Busy: number, core1Idle: number): string {
  return [
    `cpu  ${procStatLine({user: totalBusy, idle: totalIdle})}`,
    `cpu0 ${procStatLine({user: core0Busy, idle: core0Idle})}`,
    `cpu1 ${procStatLine({user: core1Busy, idle: core1Idle})}`,
  ].join('\n');
}

function setup() {
  const files: Record<string, string> = {
    '/proc/stat': cpuLine(100, 100, 50, 50, 50, 50),
    '/proc/meminfo': meminfo(),
    '/proc/cpuinfo': 'model name\t: Fixture CPU\n',
    '/etc/os-release': 'PRETTY_NAME="Fixture Linux"\n',
    '/proc/self/mountinfo': '49 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw',
    '/proc/diskstats': diskstats([['sda', 100, 50]]),
    '/proc/net/dev': netDev([['lo', 1_000, 2_000]]),
    '/sys/block/sda/device/model': 'Fixture Disk\n',
    '/sys/block/sda/queue/rotational': '0\n',
    '/sys/block/sda/size': '2000\n',
  };
  const dirs: Record<string, string[]> = {
    '/sys/bus/pci/devices': [],
    '/sys/block': ['sda'],
    '/sys/class/net': ['lo'],
    '/sys/devices/virtual/net': ['lo'],
  };
  let network: NodeJS.Dict<import('node:os').NetworkInterfaceInfo[]> = {};
  const readers = createReaders(files, {
    dirs,
    statfs: {'/': {bsize: 4096, blocks: 1000, bfree: 250}},
    networkInterfaces: () => network,
    cpuCount: 2,
  });
  let clock = 0;
  const sampler = createMetricsSampler(IDENTITY, {
    sequence: createSequenceAllocator(),
    readers,
    now: () => 1_700_000_000_000 + clock,
    monotonicMs: () => clock,
  });
  return {
    files,
    dirs: readers.dirs,
    readers,
    sampler,
    advance(ms: number) {
      clock += ms;
    },
    setNetwork(value: NodeJS.Dict<import('node:os').NetworkInterfaceInfo[]>) {
      network = value;
    },
  };
}

function metrics(record: MetricsRecord): MetricsRecord {
  const parsed = parseTelemetryRecord(record);
  if (parsed.kind !== 'metrics') assert.fail('not a metrics record');
  return parsed;
}

function okCpu(record: MetricsRecord): Extract<CpuMetric, {status: 'ok'}> {
  const cpu = record.payload.cpu;
  assert.equal(cpu.status, 'ok', JSON.stringify(cpu));
  if (cpu.status !== 'ok') throw new Error('unreachable');
  return cpu;
}

test('metrics: first interval pending, then CPU/memory/swap/disk/network deltas', () => {
  const fixture = setup();
  const first = metrics(fixture.sampler.next());
  assert.equal(first.payload.cpu.status, 'unavailable');
  assert.match(first.payload.cpu.reason, /first interval/);
  assert.equal(first.payload.memory.status, 'ok');
  assert.deepEqual(first.payload.memory, {
    status: 'ok',
    usedBytes: (1_000 - 400) * 1024,
    totalBytes: 1_000 * 1024,
    pressureSomePercent: null,
    pressureFullPercent: null,
  });
  assert.deepEqual(first.payload.swap, {status: 'ok', usedBytes: (100 - 60) * 1024, totalBytes: 100 * 1024});
  assert.equal(first.payload.disks.status, 'ok');
  if (first.payload.disks.status === 'ok') {
    assert.equal(first.payload.disks.filesystems[0]!.status, 'ok');
    assert.equal(first.payload.disks.devices[0]!.status, 'unavailable');
    assert.match((first.payload.disks.devices[0] as {reason: string}).reason, /first interval/);
  }
  assert.equal(first.payload.network.status, 'ok');
  if (first.payload.network.status === 'ok') {
    assert.match((first.payload.network.interfaces[0] as {reason: string}).reason, /first interval/);
  }
  assert.equal(first.payload.gpus.status, 'unavailable');
  assert.equal((first.payload.gpus as {reason: string}).reason, 'no GPUs detected');
  assert.ok(first.sequence > 0);
  assert.ok(fixture.sampler.failures().length > 0);
  assert.ok(fixture.sampler.failures().length <= 32);
  assert.ok(fixture.sampler.failures().every((failure) => failure.reason.length <= 256));

  fixture.advance(1_000);
  fixture.files['/proc/stat'] = cpuLine(200, 200, 110, 90, 90, 110);
  fixture.files['/proc/diskstats'] = diskstats([['sda', 200, 100]]);
  fixture.files['/proc/net/dev'] = netDev([['lo', 2_000, 2_500]]);
  const second = metrics(fixture.sampler.next());
  const cpu = okCpu(second);
  assert.equal(cpu.usagePercent, 50);
  assert.deepEqual(cpu.perCore, [60, 40]);
  // No PSI/hwmon/RAPL fixtures: unavailable optional sensors stay null.
  assert.equal(cpu.temperatureC, null);
  assert.equal(cpu.powerWatts, null);
  assert.equal(cpu.pressureSomePercent, null);
  assert.equal(typeof cpu.iowaitPercent, 'number');
  if (second.payload.disks.status === 'ok') {
    assert.deepEqual(second.payload.disks.devices[0], {
      id: 'disk:sda',
      status: 'ok',
      readBytesPerSec: 51_200,
      writeBytesPerSec: 25_600,
      readLatencyMs: null,
      writeLatencyMs: null,
      ioUtilizationPercent: 0,
    });
  } else {
    assert.fail('disks unavailable');
  }
  if (second.payload.network.status === 'ok') {
    assert.deepEqual(second.payload.network.interfaces[0], {
      id: 'net:lo',
      status: 'ok',
      rxBytesPerSec: 1_000,
      txBytesPerSec: 500,
    });
  } else {
    assert.fail('network unavailable');
  }
  assert.equal(second.sequence, first.sequence + 1);
  assert.doesNotThrow(() => parseTelemetryRecord(second));
});

test('metrics: regressed core becomes null while aggregate usage stays useful', () => {
  const fixture = setup();
  fixture.sampler.next();
  fixture.advance(1_000);
  fixture.files['/proc/stat'] = cpuLine(200, 200, 110, 90, 90, 110);
  const second = metrics(fixture.sampler.next());
  assert.deepEqual(okCpu(second).perCore, [60, 40]);

  fixture.advance(1_000);
  // core1's cumulative busy counter decreased; total still advanced by 50.
  fixture.files['/proc/stat'] = cpuLine(250, 250, 160, 140, 50, 110);
  const third = metrics(fixture.sampler.next());
  const cpu = okCpu(third);
  assert.equal(cpu.usagePercent, 25);
  assert.deepEqual(cpu.perCore, [50, null]);
});

test('metrics: counter decrease and core-count change reset the CPU baseline honestly', () => {
  const fixture = setup();
  fixture.sampler.next();
  fixture.advance(1_000);
  fixture.files['/proc/stat'] = cpuLine(200, 200, 110, 90, 90, 110);
  assert.equal(okCpu(metrics(fixture.sampler.next())).usagePercent, 50);

  fixture.advance(1_000);
  fixture.files['/proc/stat'] = cpuLine(100, 100, 50, 50, 50, 50);
  const regressed = metrics(fixture.sampler.next());
  assert.equal(regressed.payload.cpu.status, 'unavailable');
  assert.match((regressed.payload.cpu as {reason: string}).reason, /decreased/);

  fixture.advance(1_000);
  fixture.files['/proc/stat'] = cpuLine(200, 200, 110, 90, 90, 110);
  assert.equal(okCpu(metrics(fixture.sampler.next())).usagePercent, 50);

  fixture.advance(1_000);
  fixture.files['/proc/stat'] = [`cpu  ${procStatLine({user: 220, idle: 220})}`, `cpu0 ${procStatLine({user: 120, idle: 100})}`].join('\n');
  const changed = metrics(fixture.sampler.next());
  assert.equal(changed.payload.cpu.status, 'unavailable');
  assert.match((changed.payload.cpu as {reason: string}).reason, /core count changed/);

  fixture.advance(1_000);
  fixture.files['/proc/stat'] = cpuLine(300, 300, 160, 140, 140, 160);
  const restored = metrics(fixture.sampler.next());
  assert.equal(restored.payload.cpu.status, 'unavailable');
  assert.match((restored.payload.cpu as {reason: string}).reason, /core count changed/);
});

test('metrics: disk counter reset and removal restart only that device baseline', () => {
  const fixture = setup();
  fixture.sampler.next();
  fixture.advance(1_000);
  fixture.files['/proc/diskstats'] = diskstats([['sda', 300, 150]]);
  const second = metrics(fixture.sampler.next());
  assert.equal(second.payload.disks.status, 'ok');
  if (second.payload.disks.status === 'ok') assert.equal(second.payload.disks.devices[0]!.status, 'ok');

  fixture.advance(1_000);
  fixture.files['/proc/diskstats'] = diskstats([['sda', 10, 5]]);
  const reset = metrics(fixture.sampler.next());
  assert.equal(reset.payload.disks.status, 'ok');
  if (reset.payload.disks.status === 'ok') {
    assert.equal(reset.payload.disks.devices[0]!.status, 'unavailable');
    assert.match((reset.payload.disks.devices[0] as {reason: string}).reason, /decreased/);
  }

  fixture.advance(1_000);
  fixture.files['/proc/diskstats'] = diskstats([['sda', 20, 10]]);
  const resumed = metrics(fixture.sampler.next());
  if (resumed.payload.disks.status === 'ok') {
    assert.deepEqual(resumed.payload.disks.devices[0], {
      id: 'disk:sda',
      status: 'ok',
      readBytesPerSec: 5_120,
      writeBytesPerSec: 2_560,
      readLatencyMs: null,
      writeLatencyMs: null,
      ioUtilizationPercent: 0,
    });
  } else {
    assert.fail('disks unavailable');
  }
});

test('metrics: memory and swap fail independently', () => {
  const fixture = setup();
  fixture.files['/proc/meminfo'] = meminfo({MemAvailable: null});
  const first = metrics(fixture.sampler.next());
  assert.equal(first.payload.memory.status, 'unavailable');
  assert.equal(first.payload.swap.status, 'ok');

  fixture.files['/proc/meminfo'] = meminfo({SwapFree: 200});
  const second = metrics(fixture.sampler.next());
  assert.equal(second.payload.memory.status, 'ok');
  assert.equal(second.payload.swap.status, 'unavailable');
  assert.match((second.payload.swap as {reason: string}).reason, /inconsistent/);

  fixture.files['/proc/meminfo'] = meminfo({SwapTotal: 0, SwapFree: 0});
  const third = metrics(fixture.sampler.next());
  assert.deepEqual(third.payload.swap, {status: 'ok', usedBytes: 0, totalBytes: 0});
});

test('metrics: hotplug, address changes and GPU arrival bump inventoryVersion', () => {
  const fixture = setup();
  const gpuInfo: Array<{uuid: string; name: string; pciBusId: string | null; memoryTotalBytes: number | null}> = [];
  const gpu = fakeGpuPoller({availability: 'yes', info: () => gpuInfo});
  const sampler = createMetricsSampler(IDENTITY, {
    sequence: createSequenceAllocator(),
    gpu,
    readers: fixture.readers,
    now: () => 1_700_000_000_000,
    monotonicMs: () => 0,
  });
  const before = sampler.inventory();
  assert.doesNotThrow(() => parseInventory(before));
  const v1 = sampler.inventoryVersion();
  assert.ok(v1 >= 1);

  // Disk hotplug.
  fixture.dirs['/sys/block'] = ['sda', 'sdb'];
  fixture.files['/sys/block/sdb/device/model'] = 'Fixture Disk 2\n';
  fixture.files['/sys/block/sdb/queue/rotational'] = '1\n';
  fixture.files['/sys/block/sdb/size'] = '4000\n';
  sampler.inventory();
  const v2 = sampler.inventoryVersion();
  assert.ok(v2 > v1, `expected version bump after disk hotplug (${v1} -> ${v2})`);

  // Interface address change on an existing interface.
  fixture.setNetwork({
    lo: [
      {
        address: '127.0.0.1',
        netmask: '255.0.0.0',
        family: 'IPv4',
        mac: '00:00:00:00:00:00',
        internal: true,
        cidr: '127.0.0.1/8',
      },
    ],
  });
  sampler.inventory();
  const v3 = sampler.inventoryVersion();
  assert.ok(v3 > v2, `expected version bump after address change (${v2} -> ${v3})`);

  // NVIDIA device appears and joins the poller cache.
  fixture.dirs['/sys/bus/pci/devices'] = ['0000:c1:00.0'];
  Object.assign(fixture.files, pciDeviceFiles('0000:c1:00.0', '0x10de'));
  gpuInfo.push({
    uuid: 'GPU-fixture-0',
    name: 'NVIDIA GeForce RTX 3090',
    pciBusId: '00000000:C1:00.0',
    memoryTotalBytes: 24 * 1024 * 1024 * 1024,
  });
  const withGpu = sampler.inventory();
  const v4 = sampler.inventoryVersion();
  assert.ok(v4 > v3, `expected version bump after GPU arrival (${v3} -> ${v4})`);
  assert.ok(withGpu.gpus.some((entry) => entry.id === 'gpu:GPU-fixture-0' && entry.telemetry === 'supported'));
  assert.doesNotThrow(() => parseInventory(withGpu));

  // No change: version stays put.
  sampler.inventory();
  assert.equal(sampler.inventoryVersion(), v4);
});
