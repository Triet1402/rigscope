/** Sharing defaults, non-mutating destination filters and strict config keys. */
import test from 'node:test';
import assert from 'node:assert/strict';

import { parseCollectorConfig } from '../collector/config';
import {
  applySharing,
  createMetricsFilter,
  effectiveIntervalMs,
  filterInventory,
  normalizeSharing,
} from '../collector/destinations/policy';
import { createSequenceAllocator } from '../collector/sampling/sequence';
import { createSyntheticMetricsSampler } from '../collector/sampling/synthetic';

const URL = 'ws://127.0.0.1:8081/api/collector/connect';

function syntheticRecord() {
  return createSyntheticMetricsSampler(
    {collectorId: 'c', runId: 'r', bootId: 'b'},
    {sequence: createSequenceAllocator(), now: () => 1_700_000_000_000, rand: () => 0.5},
  ).next();
}

test('normalizeSharing: legacy defaults, swap follows memory, arguments opt-in', () => {
  assert.deepEqual(normalizeSharing(undefined), {
    cpu: true,
    memory: true,
    swap: true,
    gpus: true,
    disks: true,
    network: true,
    networkAddresses: true,
    processes: true,
    processArguments: false,
  });
  const legacy = normalizeSharing({cpu: true, memory: false});
  assert.equal(legacy.swap, false);
  assert.equal(legacy.processArguments, false);
  assert.equal(legacy.gpus, true);
  const explicit = normalizeSharing({cpu: false, memory: true, swap: false, processArguments: true});
  assert.equal(explicit.swap, false);
  assert.equal(explicit.processArguments, true);
});

test('config: extended sharing keys accepted and preserved; unknown keys rejected', () => {
  const sharing = {
    cpu: true,
    memory: true,
    swap: false,
    gpus: false,
    disks: true,
    network: true,
    networkAddresses: false,
    processes: true,
    processArguments: true,
  };
  const parsed = parseCollectorConfig({COLLECTOR_HUBS: JSON.stringify([{url: URL, token: 'secret', sharing}])});
  assert.deepEqual(parsed.destinations[0]!.sharing, sharing);

  const legacy = parseCollectorConfig({
    COLLECTOR_HUBS: JSON.stringify([{url: URL, token: 'secret', sharing: {cpu: true, memory: false}}]),
  });
  assert.deepEqual(legacy.destinations[0]!.sharing, {cpu: true, memory: false});

  for (const invalid of [
    {cpu: true, memory: true, camera: false},
    {cpu: true, memory: true, swap: 'no'},
    {cpu: true},
    {memory: true},
    null,
    [],
  ]) {
    assert.throws(
      () => parseCollectorConfig({COLLECTOR_HUBS: JSON.stringify([{url: URL, token: 'secret', sharing: invalid}])}),
      /sharing/,
    );
  }
  assert.throws(
    () => parseCollectorConfig({
      COLLECTOR_HUBS: JSON.stringify([{url: URL, token: 'secret', extra: true}]),
    }),
  );
});

test('createMetricsFilter: raw cadence cutoff, fresh objects, no mutation', () => {
  const record = syntheticRecord();
  const original = structuredClone(record);
  const filter = createMetricsFilter(
    {
      url: 'unused',
      token: 'a',
      intervalMs: 1_000,
      sharing: {
        cpu: false,
        memory: true,
        swap: false,
        gpus: false,
        disks: false,
        network: false,
      },
    },
    100,
  );
  const first = filter(record, 0);
  assert.ok(first);
  assert.notEqual(first, record);
  assert.notEqual(first.payload, record.payload);
  for (const family of ['cpu', 'swap', 'gpus', 'disks', 'network'] as const) {
    assert.deepEqual(first.payload[family], {
      status: 'unavailable',
      reason: 'withheld by local sharing policy',
    });
  }
  assert.deepEqual(first.payload.memory, record.payload.memory);
  assert.equal(filter(record, 99), null);
  assert.equal(filter(record, 100), null);
  const second = filter(record, 1_000);
  assert.ok(second);
  assert.deepEqual(record, original, 'source record must never be mutated');
});

test('bottleneck fields stay inside their family and never leak to another hub', () => {
  const record = syntheticRecord();
  assert.equal(record.payload.cpu.status, 'ok');
  if (record.payload.cpu.status !== 'ok' || record.payload.memory.status !== 'ok' || record.payload.disks.status !== 'ok') {
    assert.fail('synthetic record must carry bottleneck fields');
  }
  assert.equal(typeof record.payload.cpu.temperatureC, 'number');
  assert.equal(typeof record.payload.cpu.powerWatts, 'number');
  assert.equal(typeof record.payload.cpu.iowaitPercent, 'number');
  assert.equal(typeof record.payload.cpu.pressureSomePercent, 'number');
  assert.equal(typeof record.payload.memory.pressureSomePercent, 'number');
  assert.equal(typeof record.payload.disks.pressureSomePercent, 'number');

  const original = structuredClone(record);
  // One hub shares every family; the other withholds CPU, memory and disks.
  const shared = applySharing(record, {cpu: true, memory: true, swap: true, gpus: true, disks: true, network: true});
  const withheld = applySharing(record, {cpu: false, memory: false, swap: false, gpus: true, disks: false, network: true});

  assert.equal(shared.payload.cpu.status, 'ok');
  if (shared.payload.cpu.status === 'ok') assert.equal(shared.payload.cpu.temperatureC, record.payload.cpu.temperatureC);
  if (shared.payload.memory.status === 'ok') {
    assert.equal(shared.payload.memory.pressureSomePercent, record.payload.memory.pressureSomePercent);
  }
  if (shared.payload.disks.status === 'ok') {
    assert.equal(shared.payload.disks.pressureSomePercent, record.payload.disks.pressureSomePercent);
    const fs = shared.payload.disks.filesystems[0];
    assert.equal(fs?.status, 'ok');
    if (fs?.status === 'ok') assert.equal(typeof fs.inodesTotal, 'number');
    const disk = shared.payload.disks.devices[0];
    assert.equal(disk?.status, 'ok');
    if (disk?.status === 'ok') assert.equal(typeof disk.readLatencyMs, 'number');
  }

  for (const family of ['cpu', 'memory', 'disks'] as const) {
    assert.deepEqual(withheld.payload[family], {
      status: 'unavailable',
      reason: 'withheld by local sharing policy',
    });
  }
  assert.deepEqual(record, original, 'applying one hub policy must not mutate the shared sample');
});

test('filterInventory: withheld families have no resources; addresses can be withheld', () => {
  const sampler = createSyntheticMetricsSampler(
    {collectorId: 'c', runId: 'r', bootId: 'b'},
    {sequence: createSequenceAllocator(), now: () => 1_700_000_000_000, rand: () => 0.5},
  );
  const inventory = sampler.inventory();
  const original = structuredClone(inventory);
  const filtered = filterInventory(inventory, {
    cpu: false,
    memory: true,
    gpus: false,
    disks: false,
    network: true,
    networkAddresses: false,
    processes: false,
  });
  assert.equal(filtered.capabilities.cpu.status, 'withheld');
  assert.deepEqual(filtered.cpu, { model: null, logicalCores: 0 });
  assert.equal(filtered.capabilities.gpus.status, 'withheld');
  assert.equal(filtered.capabilities.disks.status, 'withheld');
  assert.equal(filtered.capabilities.processes.status, 'withheld');
  assert.equal(filtered.capabilities.memory.status, 'supported');
  assert.deepEqual(filtered.gpus, []);
  assert.deepEqual(filtered.filesystems, []);
  assert.deepEqual(filtered.disks, []);
  assert.ok(filtered.network.every((iface) => iface.addresses.length === 0));
  assert.deepEqual(inventory, original, 'source inventory must never be mutated');
  assert.notEqual(filtered.capabilities, inventory.capabilities);
});
