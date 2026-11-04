import assert from 'node:assert/strict';
import test from 'node:test';
import { DatabaseSync } from 'node:sqlite';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { updateInstallation } from '../../collector/local-state';
import { credential, demoToken } from './support/config';
import { baseEnv, startHub } from './support/fixtures';
import { startChild } from './support/process';
import { closedLoopbackPort } from './support/http';
import { SseClient } from './support/sse';
import { parseSnapshot, waitMachineState, type ReceivedSampleShape } from './support/wire';

type FastCommitted = { seq: number; runId: string; capturedAtMs: number; cpu: unknown };

/**
 * Dense capture source for the fast destination: the hub commits every live
 * metrics record into `machine_latest`, so polling the database observes
 * nearly every source tick (as opposed to coalesced live SSE snapshots whose
 * throttling decouples them from the per-tick record stream).
 */
async function observeCommitted(
  databasePath: string,
  sink: Map<number, FastCommitted>,
  deadline: number,
  ready: () => boolean,
): Promise<void> {
  const db = new DatabaseSync(databasePath, { readOnly: true });
  try {
    const select = db.prepare('SELECT run_id, seq, captured_at_ms, payload_json FROM machine_latest');
    while (Date.now() < deadline && !ready() && sink.size < 40) {
      for (const row of select.all() as unknown as { seq: number; run_id: string; captured_at_ms: number; payload_json: string }[]) {
        if (sink.has(row.seq)) continue;
        const payload = JSON.parse(row.payload_json) as { cpu: unknown };
        sink.set(row.seq, { seq: row.seq, runId: row.run_id, capturedAtMs: row.captured_at_ms, cpu: payload.cpu });
      }
      if (ready()) return;
      await new Promise(resolve => setTimeout(resolve, 50));
    }
  } finally {
    db.close();
  }
}

test('real collector advertises each hub cadence without multiplying sampling or sharing fields', async t => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-cadence-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const statePath = join(root, 'collector', 'state.json');
  const installation = updateInstallation(statePath);
  const fastCredential = credential(installation.collectorId, 'fast-machine', demoToken('cadence-fast'));
  const slowCredential = credential(installation.collectorId, 'slow-machine', demoToken('cadence-slow'));
  const fastDatabasePath = join(root, 'hub-a', 'telemetry.sqlite');
  const fastHub = await startHub(t, { collectors: [fastCredential], databasePath: fastDatabasePath });
  const slowHub = await startHub(t, { collectors: [slowCredential] });
  const offlinePort = await closedLoopbackPort();
  const collector = startChild('cadence real collector', ['dist/collector.cjs'], baseEnv('collector', {
    COLLECTOR_MODE: 'real', COLLECTOR_STATE_FILE: statePath, COLLECTOR_INTERVAL_MS: '200',
    COLLECTOR_HUBS: JSON.stringify([
      { url: fastHub.collectorWsUrl, token: fastCredential.token },
      { url: slowHub.collectorWsUrl, token: slowCredential.token, intervalMs: 450, sharing: { cpu: true, memory: false } },
      { url: `ws://127.0.0.1:${offlinePort}/api/collector/connect`, token: demoToken('cadence-offline'), intervalMs: 1_000 },
    ]),
  }));
  t.after(() => collector.stop());
  // Collection and destination handshakes begin with no browser connected.
  await collector.waitForLine(line => line.includes('welcomed'), 10_000, 'cadence collector welcome');
  const slowLive = new SseClient(slowHub.liveUrl, slowHub.browserHeaders);
  t.after(() => slowLive.close());
  const fastCommitted = new Map<number, FastCommitted>();
  const slowSamples = new Map<number, ReceivedSampleShape>();
  async function observeSse(
    client: SseClient, interval: number, found: Map<number, ReceivedSampleShape>, count: number, deadline: number,
  ): Promise<void> {
    while (found.size < count) {
      const event = await client.next(Math.max(1, deadline - Date.now()));
      const snapshot = parseSnapshot(event.data, 'cadence snapshot');
      assert.equal(snapshot.machines.length, 1);
      const machine = snapshot.machines[0];
      if (!machine.connected) continue;
      assert.equal(machine.expectedIntervalMs, interval, 'only this hub delivery interval is advertised');
      if (machine.metrics) found.set(machine.metrics.sequence, machine.metrics);
      assert.ok(Date.now() < deadline, 'cadence deliveries must keep progressing');
    }
  }
  // Observe the slow destination independently; the fast hub's commits are
  // captured densely from its durable store. The advertised interval is
  // nominal: real scheduler delays may change the number of source ticks
  // between sends.
  await Promise.all([
    observeSse(slowLive, 600, slowSamples, 3, Date.now() + 15_000),
    observeCommitted(fastDatabasePath, fastCommitted, Date.now() + 15_000, () => slowSamples.size >= 3),
  ]);
  assert.ok(slowSamples.size >= 3, 'slow destination delivered three cadence records');
  assert.ok(fastCommitted.size >= slowSamples.size, 'fast destination committed a denser record stream');
  // Every source sample the slow destination delivered must have been
  // committed identically by the fast destination, transformed only by this
  // destination's sharing policy (identical run and (collectorId, sequence)).
  for (const [sequence, slow] of [...slowSamples]) {
    const fast = fastCommitted.get(sequence);
    if (fast === undefined) continue;
    assert.equal(slow.collectorId, installation.collectorId, 'same collector identity');
    assert.equal(fast.runId, slow.runId, 'same collector run across destinations');
    assert.equal(Date.parse(slow.capturedAt), fast.capturedAtMs, 'same captured source tick');
    assert.deepEqual(fast.cpu, slow.payload.cpu, 'same source sample content');
  }
  const matched = [...slowSamples.keys()].filter(sequence => fastCommitted.has(sequence));
  assert.ok(matched.length >= 1, `at least one source sample must match (slow ${[...slowSamples.keys()].join(', ')}; ${fastCommitted.size} fast commits observed)`);
  // The first source tick carries an honest cpu 'unavailable' (rates start at
  // the second reading), so compare a settled sample for the content
  // equality the destinations must share.
  const settledCommon = matched.find(sequence => slowSamples.get(sequence)!.payload.cpu.status === 'ok');
  assert.notEqual(settledCommon, undefined, 'a settled (ok cpu) source sample must exist in both destinations');
  const fast = fastCommitted.get(settledCommon!)!;
  const slow = slowSamples.get(settledCommon!)!;
  assert.equal(slow.collectorId, installation.collectorId, 'same collector identity');
  assert.equal(fast.runId, slow.runId, 'same collector run across destinations');
  assert.equal(Date.parse(slow.capturedAt), fast.capturedAtMs, 'same captured source tick');
  assert.deepEqual(fast.cpu, slow.payload.cpu, 'same source sample content');
  assert.equal(slow.payload.cpu.status, 'ok');
  assert.deepEqual(slow.payload.memory, { status: 'unavailable', reason: 'withheld by local sharing policy' });
  assert.equal((await collector.stop()).code, 0);
  const stopped = await waitMachineState(slowLive, 5_000, { connected: false, withMetrics: true });
  assert.equal(stopped.snapshot.machines[0].expectedIntervalMs, 600, 'disconnect retains last-known cadence');
});
