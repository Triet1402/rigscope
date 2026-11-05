import test from 'node:test';
import assert from 'node:assert/strict';
import { readSysfsGpu } from '../collector/sampling/sysfs-gpu';
import { buildInventory } from '../collector/sampling/inventory';
import { createMetricsSampler } from '../collector/sampling/metrics';
import { parseInventory, parseTelemetryRecord } from '../src/contracts';
import { createSequenceAllocator } from '../collector/sampling/sequence';
import { createReaders, fakeGpuPoller, meminfo, pciDeviceFiles, procStatLine } from './sampling-helpers';

const address = '0000:03:00.0';
const base = `/sys/bus/pci/devices/${address}`;
function fixture(vendor = '0x1002') {
  return createReaders({
    ...pciDeviceFiles(address, vendor),
    '/proc/stat': `cpu ${procStatLine({user: 100, idle: 100})}\ncpu0 ${procStatLine({user: 100, idle: 100})}`,
    '/proc/meminfo': meminfo(),
    [`${base}/gpu_busy_percent`]: '37', [`${base}/mem_info_vram_total`]: '8589934592',
    [`${base}/mem_info_vram_used`]: '1073741824', [`${base}/hwmon/hwmon4/temp1_input`]: '61500',
    [`${base}/hwmon/hwmon4/power1_average`]: '21500000',
  }, {dirs: {'/sys/bus/pci/devices': [address], [`${base}/hwmon`]: ['hwmon4']}});
}

test('AMD sysfs readings use correct units, stable PCI identity and validated inventory/telemetry', () => {
  const readers = fixture();
  const reading = readSysfsGpu(readers, address);
  assert.deepEqual(reading, {id: `pci:${address}`, status: 'ok', utilizationPercent: 37,
    memoryTotalBytes: 8589934592, memoryUsedBytes: 1073741824, temperatureC: 61.5, powerWatts: 21.5});
  const inventory = buildInventory(readers, null);
  assert.doesNotThrow(() => parseInventory(inventory));
  assert.equal(inventory.gpus[0]!.telemetry, 'supported');
  assert.equal(inventory.capabilities.gpus.status, 'supported');
  const sampler = createMetricsSampler({collectorId: 'amd-fixture', runId: 'run-fixture', bootId: 'boot-fixture'}, {
    readers, sequence: createSequenceAllocator(), monotonicMs: () => 1000, now: () => 1700000000000,
  });
  sampler.inventory();
  const record = sampler.next();
  assert.doesNotThrow(() => parseTelemetryRecord(record));
  assert.deepEqual(record.payload.gpus, {status: 'ok', devices: [reading]});
});

test('integrated GPU shared RAM stays unavailable as VRAM; zero busy remains a valid reading', () => {
  const readers = fixture();
  readers.files[`${base}/gpu_busy_percent`] = '0';
  readers.files[`${base}/mem_info_vram_total`] = '0';
  const reading = readSysfsGpu(readers, address);
  assert.equal(reading.status, 'ok');
  if (reading.status !== 'ok') return;
  assert.equal(reading.utilizationPercent, 0);
  assert.equal(reading.memoryTotalBytes, null);
  assert.equal(reading.memoryUsedBytes, null);
});

test('Intel sensors are partial readings; missing utilization is never fabricated', () => {
  const readers = fixture('0x8086');
  for (const name of ['gpu_busy_percent', 'mem_info_vram_total', 'mem_info_vram_used']) delete readers.files[`${base}/${name}`];
  const reading = readSysfsGpu(readers, address);
  assert.equal(reading.status, 'ok');
  if (reading.status !== 'ok') return;
  assert.equal(reading.utilizationPercent, null);
  assert.equal(reading.memoryTotalBytes, null);
  assert.equal(reading.temperatureC, 61.5);
  delete readers.files[`${base}/hwmon/hwmon4/temp1_input`];
  delete readers.files[`${base}/hwmon/hwmon4/power1_average`];
  assert.equal(readSysfsGpu(readers, address).status, 'unavailable');
});

test('malformed, negative, out-of-range and disappearing counters stay unavailable', () => {
  const readers = fixture();
  readers.files[`${base}/gpu_busy_percent`] = '101';
  readers.files[`${base}/mem_info_vram_total`] = '-1';
  readers.files[`${base}/hwmon/hwmon4/temp1_input`] = 'NaN';
  readers.files[`${base}/hwmon/hwmon4/power1_average`] = '';
  assert.equal(readSysfsGpu(readers, address).status, 'unavailable');
  delete readers.files[`${base}/vendor`];
  assert.equal(readSysfsGpu(readers, address).status, 'unavailable');
});


test('mixed-vendor GPU collection preserves AMD readings when NVIDIA tooling is absent', () => {
  const readers = fixture();
  const nvidiaAddress = '0000:04:00.0';
  Object.assign(readers.files, pciDeviceFiles(nvidiaAddress, '0x10de'));
  readers.dirs['/sys/bus/pci/devices']!.push(nvidiaAddress);
  const sampler = createMetricsSampler({collectorId: 'mixed-fixture', runId: 'run-fixture', bootId: 'boot-fixture'}, {
    readers, gpu: fakeGpuPoller({availability: 'no'}), sequence: createSequenceAllocator(), monotonicMs: () => 1000, now: () => 1700000000000,
  });
  const record = sampler.next();
  assert.doesNotThrow(() => parseTelemetryRecord(record));
  const metric = record.payload.gpus;
  assert.equal(metric.status, 'ok');
  if (metric.status !== 'ok') return;
  assert.equal(metric.devices.length, 2);
  assert.equal(metric.devices[0]!.status, 'ok');
  assert.equal(metric.devices[1]!.status, 'unavailable');
});


test('AMD integrated display-controller class 0380 is inventoried and sampled', () => {
  const readers = fixture();
  readers.files[`${base}/class`] = '0x038000';
  const inventory = buildInventory(readers, null);
  assert.equal(inventory.gpus.length, 1);
  assert.equal(inventory.gpus[0]!.vendor, 'amd');
  assert.equal(inventory.gpus[0]!.telemetry, 'supported');
  assert.doesNotThrow(() => parseInventory(inventory));
});

test('Intel adapter with no counters remains inventoried with an honest fallback name', () => {
  const readers = createReaders(pciDeviceFiles(address, '0x8086'), {dirs: {'/sys/bus/pci/devices': [address]}});
  const inventory = buildInventory(readers, null);
  assert.equal(inventory.gpus.length, 1);
  assert.equal(inventory.gpus[0]!.name, 'Intel display adapter');
  assert.equal(inventory.gpus[0]!.telemetry, 'unsupported');
  assert.match(inventory.gpus[0]!.reason!, /driver support or permissions may/);
});

test('distro PCI names improve AMD naming without guessing a hardware model', () => {
  const readers = fixture();
  readers.files[`${base}/device`] = '0x150e';
  readers.files['/usr/share/hwdata/pci.ids'] = '1002  AMD\n\t150e  Radeon fixture graphics\n8086  Intel\n\t150e  Other device';
  assert.equal(buildInventory(readers, null).gpus[0]!.name, 'Radeon fixture graphics');
});
