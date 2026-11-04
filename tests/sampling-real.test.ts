/**
 * Real-hardware smoke test (target host: 48 logical cores, 2× RTX 3090 and an
 * ASPEED management VGA). Skipped unless `RIGSCOPE_REAL_HW=1`, because it reads the
 * live host and waits 2s. It never starts/stops workloads; `nvidia-smi` is a
 * read-only query.
 *
 * Run:
 *   RIGSCOPE_REAL_HW=1 npx tsx --test tests/sampling-real.test.ts
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import { setTimeout as delay } from 'node:timers/promises';

import { parseInventory, parseTelemetryRecord } from '../src/contracts';
import { GpuPoller } from '../collector/sampling/gpu';
import { createMetricsSampler } from '../collector/sampling/metrics';
import { createSequenceAllocator } from '../collector/sampling/sequence';

test(
  'real hardware: 48 cores, 2 NVIDIA GPUs, ASPEED unsupported, second-sample rates',
  {skip: process.env.RIGSCOPE_REAL_HW !== '1'},
  async () => {
    const gpu = new GpuPoller({intervalMs: 2_000});
    await gpu.pollOnce();
    const sampler = createMetricsSampler(
      {collectorId: 'real-hw-test', runId: 'real-hw-run', bootId: 'real-hw-boot'},
      {sequence: createSequenceAllocator(), gpu},
    );

    const startedFirst = performance.now();
    const first = sampler.next();
    const firstDurationMs = performance.now() - startedFirst;
    assert.doesNotThrow(() => parseTelemetryRecord(first));

    await delay(2_000);

    const startedSecond = performance.now();
    const second = sampler.next();
    const secondDurationMs = performance.now() - startedSecond;
    assert.doesNotThrow(() => parseTelemetryRecord(second));

    const inventory = sampler.inventory();
    assert.doesNotThrow(() => parseInventory(inventory));
    assert.equal(inventory.cpu.logicalCores, 48, 'expected 48 logical cores');

    const nvidia = inventory.gpus.filter((entry) => entry.vendor === 'nvidia');
    assert.equal(nvidia.length, 2, 'expected two NVIDIA GPUs');
    assert.ok(nvidia.every((entry) => /^gpu:GPU-/.test(entry.id)), JSON.stringify(nvidia));
    assert.ok(
      nvidia.every((entry) => entry.telemetry === 'supported' && entry.memoryTotalBytes !== null),
      JSON.stringify(nvidia),
    );
    const aspeed = inventory.gpus.find((entry) => entry.vendor === 'aspeed');
    assert.ok(aspeed, 'expected the ASPEED management adapter in inventory');
    assert.equal(aspeed.telemetry, 'unsupported');
    assert.match(aspeed.reason ?? '', /management adapter; no telemetry/);

    if (second.payload.cpu.status === 'ok') {
      assert.equal(second.payload.cpu.perCore.length, 48);
    } else {
      assert.fail(`second CPU sample unavailable: ${second.payload.cpu.reason}`);
    }

    if (second.payload.gpus.status === 'ok') {
      const okDevices = second.payload.gpus.devices.filter((device) => device.status === 'ok');
      assert.equal(okDevices.length, 2);
    } else {
      assert.fail(`second GPU sample unavailable: ${second.payload.gpus.reason}`);
    }

    if (second.payload.disks.status === 'ok') {
      assert.ok(
        second.payload.disks.devices.some((device) => device.status === 'ok'),
        'expected at least one disk rate on the second sample',
      );
    } else {
      assert.fail(`second disk sample unavailable: ${second.payload.disks.reason}`);
    }

    if (second.payload.network.status === 'ok') {
      assert.ok(
        second.payload.network.interfaces.some((iface) => iface.status === 'ok'),
        'expected at least one interface rate on the second sample',
      );
    } else {
      assert.fail(`second network sample unavailable: ${second.payload.network.reason}`);
    }

    console.log('real hardware nvidia-smi costStats:', JSON.stringify(gpu.costStats()));
    console.log(
      'real hardware sync next() duration ms:',
      JSON.stringify({first: firstDurationMs, second: secondDurationMs}),
    );
    console.log('real hardware inventory gpus:', JSON.stringify(inventory.gpus));
    console.log('real hardware failures:', JSON.stringify(sampler.failures()));
  },
);
