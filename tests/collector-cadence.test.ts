import assert from 'node:assert/strict';
import test from 'node:test';
import { effectiveIntervalMs, createMetricsFilter } from '../collector/destinations/policy';
import { createSyntheticMetricsSampler } from '../collector/sampling/synthetic';
import { createSequenceAllocator } from '../collector/sampling/sequence';
import type { MetricsRecord } from '../src/contracts';

function syntheticRecord(): MetricsRecord {
  return createSyntheticMetricsSampler(
    {collectorId: 'c', runId: 'r', bootId: 'boot-1'},
    {sequence: createSequenceAllocator(), now: () => 1_700_000_000_000, rand: () => 0.5},
  ).next();
}

test('effectiveIntervalMs: quantizes the destination interval onto sample ticks', () => {
  // No destination interval: the base sampling cadence is advertised.
  assert.equal(effectiveIntervalMs(200, undefined), 200);
  assert.equal(effectiveIntervalMs(5000, undefined), 5000);
  // A destination interval can only slow delivery down, never speed sampling.
  assert.equal(effectiveIntervalMs(5000, 3000), 5000);
  assert.equal(effectiveIntervalMs(200, 200), 200);
  // Not divisible by the base: quantized up to the next tick multiple.
  assert.equal(effectiveIntervalMs(200, 450), 600);
  assert.equal(effectiveIntervalMs(100, 333), 400);
  assert.equal(effectiveIntervalMs(5000, 7000), 10_000);
});

test('effectiveIntervalMs: worst-case quantization stays inside the hello bound', () => {
  // Destination and base at the 3_600_000 ms configuration cap.
  assert.equal(effectiveIntervalMs(3_600_000, 3_600_000), 3_600_000);
  assert.equal(effectiveIntervalMs(100, 3_600_000), 3_600_000);
  // First quantization step above the destination cap stays under 7_200_000.
  assert.equal(effectiveIntervalMs(3_599_999, 3_600_000), 7_199_998);
  for (const base of [100, 200, 1000, 5000, 1_800_001, 3_599_999, 3_600_000]) {
    for (const destination of [100, 450, 3_600_000, undefined]) {
      assert.ok(
        effectiveIntervalMs(base, destination) < 7_200_000,
        `quantized interval must stay below the contract bound (base ${base}, destination ${destination})`,
      );
    }
  }
});

test('nominal cadence metadata preserves the configured delivery cutoff under timer jitter', () => {
  const base = 200;
  const destination = {url: 'unused', token: 'a', intervalMs: 450};
  const interval = effectiveIntervalMs(base, destination.intervalMs);
  assert.equal(interval, 600);
  const filter = createMetricsFilter(destination, base);
  const record = syntheticRecord();
  // A nominal 600ms tick can arrive fractionally early. The filter keeps the
  // configured 450ms cutoff so rounding cannot defer an eligible tick.
  assert.ok(filter(structuredClone(record), 0), 'first tick sends');
  assert.equal(filter(structuredClone(record), 200), null);
  assert.equal(filter(structuredClone(record), 400), null);
  assert.ok(filter(structuredClone(record), 450), 'configured cutoff sends');
  assert.equal(filter(structuredClone(record), 449), null, 'below cutoff defers');
  assert.equal(filter(structuredClone(record), 599.9), null, 'mid-schedule tick does not send');
  assert.ok(filter(structuredClone(record), 599.9 + interval - 1), 'later tick sends');
});
