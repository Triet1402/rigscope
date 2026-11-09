/** The launcher must forward the collector queue mode it strips from its env. */
import test from 'node:test';
import assert from 'node:assert/strict';

import { collectorQueueModeEnvironment, collectorSupervisorEnvironment } from '../src/cli/runtime';

test('launcher forwards supported queue modes, including trimmed input', () => {
  assert.deepEqual(collectorQueueModeEnvironment({ COLLECTOR_QUEUE_MODE: 'durable' }), {
    COLLECTOR_QUEUE_MODE: 'durable',
  });
  assert.deepEqual(collectorQueueModeEnvironment({ COLLECTOR_QUEUE_MODE: ' durable ' }), {
    COLLECTOR_QUEUE_MODE: 'durable',
  });
  assert.deepEqual(collectorQueueModeEnvironment({ COLLECTOR_QUEUE_MODE: ' memory ' }), {
    COLLECTOR_QUEUE_MODE: 'memory',
  });
});

test('launcher omits an absent or blank queue mode so the collector default applies', () => {
  assert.deepEqual(collectorQueueModeEnvironment({}), {});
  assert.deepEqual(collectorQueueModeEnvironment({ COLLECTOR_QUEUE_MODE: '   ' }), {});
});

test('launcher surfaces an invalid queue mode instead of silently using the default', () => {
  assert.throws(
    () => collectorQueueModeEnvironment({ COLLECTOR_QUEUE_MODE: 'ephemeral' }),
    /COLLECTOR_QUEUE_MODE must be one of memory or durable/,
  );
  assert.throws(
    () => collectorQueueModeEnvironment({ COLLECTOR_QUEUE_MODE: 'DURABLE' }),
    /COLLECTOR_QUEUE_MODE/,
  );
});

test('launcher forwards the GPU-temps kill switch only for an exact off', () => {
  assert.deepEqual(collectorSupervisorEnvironment({ COLLECTOR_GPU_TEMPS: ' off ' }), {
    COLLECTOR_GPU_TEMPS: 'off',
  });
  assert.deepEqual(collectorSupervisorEnvironment({ COLLECTOR_GPU_TEMPS: 'OFF' }), {
    COLLECTOR_GPU_TEMPS: 'off',
  });
});

test('launcher omits the GPU-temps switch for absent, blank or other values', () => {
  assert.deepEqual(collectorSupervisorEnvironment({}), {});
  assert.deepEqual(collectorSupervisorEnvironment({ COLLECTOR_GPU_TEMPS: '   ' }), {});
  assert.deepEqual(collectorSupervisorEnvironment({ COLLECTOR_GPU_TEMPS: 'on' }), {});
});

test('launcher forwards a durable queue mode and the GPU-temps switch together', () => {
  assert.deepEqual(
    collectorSupervisorEnvironment({ COLLECTOR_QUEUE_MODE: 'durable', COLLECTOR_GPU_TEMPS: 'off' }),
    { COLLECTOR_QUEUE_MODE: 'durable', COLLECTOR_GPU_TEMPS: 'off' },
  );
});
