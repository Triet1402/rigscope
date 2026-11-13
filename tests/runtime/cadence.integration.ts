import assert from 'node:assert/strict';
import test from 'node:test';
import { credential, demoToken } from './support/config';
import { startHub } from './support/fixtures';
import { startCollectorProcess } from './support/launcher';
import { failOnExit } from './support/process';
import { SseClient } from './support/sse';
import { parseSnapshot, waitMachineState, type ReceivedSampleShape } from './support/wire';

test('real collector advertises its one hub cadence and applies its local sharing policy', async t => {
  const collectorId = 'cadence-collector';
  const cred = credential(collectorId, 'cadence-machine', demoToken('cadence-one'));
  const hub = await startHub(t, { collectors: [cred] });
  const collector = startCollectorProcess(t, {
    label: 'single-hub cadence collector',
    mode: 'real',
    collectorId,
    intervalMs: 600,
    hubs: [{ url: hub.collectorWsUrl, token: cred.token, sharing: { cpu: true, memory: false } }],
  });
  // Collection and the handshake begin with no browser connected.
  await collector.waitForLine(line => line.includes('welcomed'), 10_000, 'cadence collector welcome');
  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  t.after(() => live.close());
  const observed = new Map<number, ReceivedSampleShape>();
  const deadline = Date.now() + 15_000;
  while (observed.size < 3) {
    const remaining = deadline - Date.now();
    assert.ok(remaining > 0, 'the one advertised cadence must keep delivering samples');
    const event = await failOnExit(collector, live.next(remaining), 'single cadence delivery');
    const snapshot = parseSnapshot(event.data, 'single cadence snapshot');
    assert.equal(snapshot.machines.length, 1);
    const machine = snapshot.machines[0];
    if (!machine.connected) continue;
    assert.equal(machine.expectedIntervalMs, 600, 'exactly one hub cadence is advertised');
    if (machine.metrics) observed.set(machine.metrics.sequence, machine.metrics);
  }
  const sequences = [...observed.keys()].sort((left, right) => left - right);
  assert.ok(
    sequences[sequences.length - 1] >= sequences[0] + 2,
    `samples advance at the advertised cadence (${sequences.join(', ')})`,
  );
  const settled = sequences.map(sequence => observed.get(sequence)!).find(metrics => metrics.payload.cpu.status === 'ok');
  assert.ok(settled, 'CPU rates settle after the first reading');
  assert.deepEqual(
    settled!.payload.memory,
    { status: 'unavailable', reason: 'withheld by local sharing policy' },
    'the collector-local sharing policy withholds memory from its one hub',
  );
  assert.equal(settled!.collectorId, collectorId);
  assert.equal((await collector.stop()).code, 0, 'clean shutdown');
  const stopped = await waitMachineState(live, 5_000, { connected: false, withMetrics: true });
  assert.equal(stopped.snapshot.machines[0].expectedIntervalMs, 600, 'disconnect retains the last-known cadence');
});
