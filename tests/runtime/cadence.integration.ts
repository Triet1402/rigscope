import assert from 'node:assert/strict';
import test from 'node:test';
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

type HistoryStatus = { acceptedRecords: number };

/**
 * Poll the hub's history status for the cumulative accepted-record count. This
 * works in the default RAM mode (there is no machine_latest row to scrape) and
 * still proves that the fast destination's source stream is denser than the
 * slow destination's cadence.
 */
async function waitAccepted(
  baseUrl: string,
  headers: Record<string, string>,
  minimum: number,
  deadline: number,
): Promise<number> {
  let accepted = 0;
  while (Date.now() < deadline) {
    const response = await fetch(`${baseUrl}/api/history/status`, { headers });
    assert.equal(response.status, 200);
    const status = await response.json() as HistoryStatus;
    accepted = status.acceptedRecords;
    if (accepted >= minimum) return accepted;
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  return accepted;
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
  const fastLive = new SseClient(fastHub.liveUrl, fastHub.browserHeaders);
  t.after(() => fastLive.close());
  const fastSamples = new Map<number, ReceivedSampleShape>();
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
  // Observe both destinations through SSE; the fast hub's accepted-record
  // counter proves it received a denser stream than the slow cadence, even
  // though the default hub mode intentionally keeps no per-sample disk rows.
  await Promise.all([
    observeSse(slowLive, 600, slowSamples, 3, Date.now() + 15_000),
    observeSse(fastLive, 200, fastSamples, 3, Date.now() + 15_000),
  ]);
  assert.ok(slowSamples.size >= 3, 'slow destination delivered three cadence records');
  const accepted = await waitAccepted(fastHub.baseUrl, fastHub.browserHeaders, slowSamples.size, Date.now() + 5_000);
  assert.ok(accepted >= slowSamples.size, `fast destination accepted a denser stream (${accepted} vs ${slowSamples.size})`);
  // Every source sample both destinations delivered must have identical run,
  // capture time and CPU content, transformed only by sharing policy.
  const matched = [...slowSamples.keys()].filter(sequence => fastSamples.has(sequence));
  assert.ok(matched.length >= 1, `at least one source sample must match (slow ${[...slowSamples.keys()].join(', ')}; fast ${[...fastSamples.keys()].join(', ')})`);
  // The first source tick carries an honest cpu 'unavailable' (rates start at
  // the second reading), so compare a settled sample for the content
  // equality the destinations must share.
  const settledCommon = matched.find(sequence => slowSamples.get(sequence)!.payload.cpu.status === 'ok');
  assert.notEqual(settledCommon, undefined, 'a settled (ok cpu) source sample must exist in both destinations');
  const fast = fastSamples.get(settledCommon!)!;
  const slow = slowSamples.get(settledCommon!)!;
  assert.equal(slow.collectorId, installation.collectorId, 'same collector identity');
  assert.equal(fast.runId, slow.runId, 'same collector run across destinations');
  assert.equal(Date.parse(slow.capturedAt), Date.parse(fast.capturedAt), 'same captured source tick');
  assert.deepEqual(fast.payload.cpu, slow.payload.cpu, 'same source sample content');
  assert.equal(slow.payload.cpu.status, 'ok');
  assert.deepEqual(slow.payload.memory, { status: 'unavailable', reason: 'withheld by local sharing policy' });
  assert.equal((await collector.stop()).code, 0);
  const stopped = await waitMachineState(slowLive, 5_000, { connected: false, withMetrics: true });
  assert.equal(stopped.snapshot.machines[0].expectedIntervalMs, 600, 'disconnect retains last-known cadence');
});
