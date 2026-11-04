/** Explicit synthetic v2 fixtures and contract validation. */
import test from 'node:test';
import assert from 'node:assert/strict';

import { parseInventory, parseTelemetryRecord } from '../src/contracts';
import { createSequenceAllocator } from '../collector/sampling/sequence';
import { createSyntheticMetricsSampler } from '../collector/sampling/synthetic';

test('synthetic v2 sampler: contract-valid records, inventory and sequence', () => {
  const sampler = createSyntheticMetricsSampler(
    {collectorId: 'mock-collector', runId: 'mock-run', bootId: 'mock-boot'},
    {sequence: createSequenceAllocator(), now: () => 1_700_000_000_000, rand: () => 0.5},
  );
  const first = sampler.next();
  const second = sampler.next();
  assert.equal(first.kind, 'metrics');
  assert.equal(first.sequence, 1);
  assert.equal(second.sequence, 2);
  assert.doesNotThrow(() => parseTelemetryRecord(first));
  assert.doesNotThrow(() => parseTelemetryRecord(second));

  const inventory = sampler.inventory();
  assert.doesNotThrow(() => parseInventory(inventory));
  assert.equal(inventory.cpu.logicalCores, 8);
  assert.equal(inventory.gpus.length, 1);
  assert.equal(inventory.gpus[0]!.id, 'gpu:SYNTHETIC-0');
  assert.match(inventory.gpus[0]!.name, /^synthetic/);
  assert.match(inventory.cpu.model ?? '', /^synthetic/);
  assert.match(inventory.disks[0]!.name, /^synthetic/);
  assert.match(inventory.network[0]!.name, /^synthetic/);
  assert.equal(sampler.inventoryVersion(), 1);
  assert.deepEqual(sampler.failures(), []);
});

test('synthetic v2 sampler: values are stable for a pinned clock and random source', () => {
  const make = () =>
    createSyntheticMetricsSampler(
      {collectorId: 'c', runId: 'r', bootId: 'b'},
      {sequence: createSequenceAllocator(), now: () => 1_700_000_000_000, rand: () => 0.25},
    );
  const a = make().next();
  const b = make().next();
  assert.deepEqual(a.payload.cpu, b.payload.cpu);
  assert.deepEqual(a.payload.memory, b.payload.memory);
  assert.deepEqual(a.payload.gpus, b.payload.gpus);
});
