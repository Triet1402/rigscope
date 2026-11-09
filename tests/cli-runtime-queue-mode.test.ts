/** The launcher must forward the collector queue mode it strips from its env. */
import test from 'node:test';
import assert from 'node:assert/strict';

import { collectorQueueModeEnvironment } from '../src/cli/runtime';

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
