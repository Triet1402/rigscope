/** GpuPoller CSV parsing, async isolation, ENOENT/backoff and cost fixtures. */
import test from 'node:test';
import assert from 'node:assert/strict';

import {
  GpuPoller,
  parseComputeAppsCsv,
  parseGpuCsv,
  type ExecFileFn,
} from '../collector/sampling/gpu';

const GPU_CSV = [
  'GPU-aaaaaaaa-1111, NVIDIA GeForce RTX 3090, 00000000:C1:00.0, 25, 15232, 24576, 30, 22.35',
  'GPU-bbbbbbbb-2222, NVIDIA GeForce RTX 3090, 00000000:C2:00.0, [N/A], [N/A], [Not Supported], 32, N/A',
].join('\n');

function execError(message: string, code?: string): Error & {code?: string; killed?: boolean} {
  const error = new Error(message) as Error & {code?: string; killed?: boolean};
  if (code !== undefined) error.code = code;
  return error;
}

test('parseGpuCsv: [N/A] and [Not Supported] become null, MiB becomes bytes', () => {
  const rows = parseGpuCsv(GPU_CSV);
  assert.equal(rows.length, 2);
  assert.deepEqual(rows[0], {
    uuid: 'GPU-aaaaaaaa-1111',
    name: 'NVIDIA GeForce RTX 3090',
    pciBusId: '00000000:C1:00.0',
    utilizationPercent: 25,
    memoryUsedBytes: 15_232 * 1024 * 1024,
    memoryTotalBytes: 24_576 * 1024 * 1024,
    temperatureC: 30,
    powerWatts: 22.35,
  });
  assert.equal(rows[1]!.utilizationPercent, null);
  assert.equal(rows[1]!.memoryUsedBytes, null);
  assert.equal(rows[1]!.memoryTotalBytes, null);
  assert.equal(rows[1]!.temperatureC, 32);
  assert.equal(rows[1]!.powerWatts, null);
  assert.deepEqual(parseGpuCsv(''), []);
  assert.deepEqual(parseGpuCsv('not,csv'), []);
});

test('parseComputeAppsCsv: pid memory rows and malformed lines', () => {
  assert.deepEqual(
    parseComputeAppsCsv([
      '2662911, GPU-aaaaaaaa-1111, 15222',
      '2662908, GPU-bbbbbbbb-2222, 15126',
      'bad, row',
      '',
    ].join('\n')),
    [
      {pid: 2_662_911, gpuUuid: 'GPU-aaaaaaaa-1111', usedBytes: 15_222 * 1024 * 1024},
      {pid: 2_662_908, gpuUuid: 'GPU-bbbbbbbb-2222', usedBytes: 15_126 * 1024 * 1024},
    ],
  );
});

test('GpuPoller: ENOENT marks tooling unavailable without wiping readings', async () => {
  let calls = 0;
  let timeout = 0;
  const execFile: ExecFileFn = (_file, _args, options, callback) => {
    calls += 1;
    timeout = options.timeout;
    callback(execError('spawn nvidia-smi ENOENT', 'ENOENT'), '', '');
  };
  const poller = new GpuPoller({intervalMs: 100, execFile});
  // Poll cadence has a 1s floor.
  assert.equal(poller.pollIntervalMs, 1_000);
  await poller.pollOnce();
  assert.equal(calls, 1);
  assert.equal(timeout, 3_000);
  assert.equal(poller.available(), 'no');
  assert.equal(poller.latest(), null);
  assert.deepEqual(poller.costStats(), {
    runs: 1,
    failures: 1,
    lastDurationMs: poller.costStats().lastDurationMs,
    avgDurationMs: poller.costStats().avgDurationMs,
    maxDurationMs: poller.costStats().maxDurationMs,
  });
  assert.equal(poller.costStats().avgDurationMs, poller.costStats().maxDurationMs);
});

test('GpuPoller: succeeds, parses devices, samples compute apps every third query', async () => {
  let gpuQueries = 0;
  let appQueries = 0;
  let clock = 0;
  const execFile: ExecFileFn = (_file, args, _options, callback) => {
    clock += 7;
    if (args.some((arg) => arg.startsWith('--query-gpu='))) {
      gpuQueries += 1;
      callback(null, GPU_CSV, '');
      return;
    }
    if (args.some((arg) => arg.startsWith('--query-compute-apps='))) {
      appQueries += 1;
      callback(null, '2662911, GPU-aaaaaaaa-1111, 15222\n2662911, GPU-bbbbbbbb-2222, 256', '');
      return;
    }
    callback(execError('unexpected arguments'), '', '');
  };
  const poller = new GpuPoller({intervalMs: 5_000, execFile, monotonicMs: () => clock});
  await poller.pollOnce();
  assert.equal(poller.available(), 'yes');
  assert.equal(gpuQueries, 1);
  assert.equal(appQueries, 0);
  const snapshot = poller.latest();
  assert.ok(snapshot);
  assert.equal(snapshot.devices.length, 2);
  assert.equal(snapshot.devices[0]!.id, 'gpu:GPU-aaaaaaaa-1111');
  if (snapshot.devices[0]!.status === 'ok') {
    assert.equal(snapshot.devices[0]!.memoryTotalBytes, 24_576 * 1024 * 1024);
  }
  assert.equal(poller.deviceInfo().length, 2);
  assert.equal(poller.deviceInfo()[0]!.uuid, 'GPU-aaaaaaaa-1111');

  await poller.pollOnce();
  await poller.pollOnce();
  assert.equal(gpuQueries, 3);
  assert.equal(appQueries, 1);
  assert.equal(poller.computeApps().get(2_662_911), (15_222 + 256) * 1024 * 1024);
  const costs = poller.costStats();
  assert.equal(costs.runs, 4);
  assert.equal(costs.failures, 0);
  assert.equal(costs.lastDurationMs, 7);
  assert.equal(costs.maxDurationMs, 7);
});

test('GpuPoller: concurrent pollOnce calls share one in-flight query', async () => {
  let calls = 0;
  const pending: Array<() => void> = [];
  const execFile: ExecFileFn = (_file, _args, _options, callback) => {
    calls += 1;
    pending.push(() => callback(null, GPU_CSV, ''));
  };
  const poller = new GpuPoller({intervalMs: 2_000, execFile});
  const first = poller.pollOnce();
  const second = poller.pollOnce();
  assert.equal(calls, 1);
  pending.shift()!();
  await Promise.all([first, second]);
  assert.equal(calls, 1);
  assert.equal(poller.latest()?.devices.length, 2);
});

test('GpuPoller: timeout callback is a failure and a later success recovers', async () => {
  let mode: 'timeout' | 'ok' = 'timeout';
  let observedTimeout = 0;
  const execFile: ExecFileFn = (_file, _args, options, callback) => {
    observedTimeout = options.timeout;
    if (mode === 'timeout') {
      callback(Object.assign(execError('Command failed: nvidia-smi'), {killed: true, signal: 'SIGTERM'}), '', '');
      return;
    }
    callback(null, GPU_CSV, '');
  };
  const poller = new GpuPoller({intervalMs: 2_000, timeoutMs: 50, execFile});
  await poller.pollOnce();
  assert.equal(observedTimeout, 50);
  assert.equal(poller.available(), 'unknown');
  assert.equal(poller.costStats().failures, 1);
  assert.equal(poller.latest(), null);

  mode = 'ok';
  await poller.pollOnce();
  assert.equal(poller.available(), 'yes');
  assert.equal(poller.costStats().failures, 1);
  assert.equal(poller.costStats().runs, 2);
  assert.equal(poller.latest()?.devices.length, 2);
});
