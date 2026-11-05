import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from 'node:fs';
import { DatabaseSync } from 'node:sqlite';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { startChild } from './support/process';
import { baseEnv, startHub } from './support/fixtures';
import { loginBrowser } from './support/browser';
import { credential } from './support/config';
import { SseClient } from './support/sse';
import { parseHubSnapshot, type MachineState } from '../../src/contracts';
import { DestinationOutbox, outboxDirFor } from '../../collector/destinations/outbox';
import { createSyntheticMetricsSampler } from '../../collector/sampling/synthetic';
import { createSequenceAllocator } from '../../collector/sampling/sequence';

async function waitMetrics(live: SseClient, minimumSequence = -1): Promise<MachineState> {
  const deadline = Date.now() + 20000;
  while (Date.now() < deadline) {
    const event = await live.next(Math.max(1, deadline - Date.now()));
    const machine = parseHubSnapshot(JSON.parse(event.data)).machines[0];
    if (machine?.metrics && machine.metrics.sequence > minimumSequence) return machine;
  }
  throw new Error('metrics did not arrive');
}

test('packaged collector replays a retained canonical queue and reapplies local privacy', async t => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-retained-runtime-'));
  t.after(() => rmSync(dir, {recursive: true, force: true}));
  const collectorId = 'retained-runtime-collector';
  const creds = credential(collectorId, 'retained-machine', 'public-retained-token');
  const databasePath = join(dir, 'hub', 'telemetry.sqlite');
  const hub = await startHub(t, {collectors: [creds], databasePath});
  const statePath = join(dir, 'collector.json');
  writeFileSync(statePath, JSON.stringify({collectorId,
    destinations: [{url: hub.collectorWsUrl, token: creds.token, sharing: {cpu: true, memory: false}}]}), {mode: 0o600});
  const identity = {collectorId, runId: 'retained-run', bootId: 'retained-boot'};
  const record = createSyntheticMetricsSampler(identity, {sequence: createSequenceAllocator(), now: () => Date.now() - 2000}).next();
  const queueDir = outboxDirFor(dir, hub.collectorWsUrl);
  const outbox = DestinationOutbox.open({dir: queueDir, identity, allocateSequence: () => 100});
  outbox.append(record);
  outbox.close();
  const spool = new DatabaseSync(join(queueDir, 'outbox.sqlite'));
  const gap = {...identity, sequence: 2, kind: 'gap', capturedAt: record.capturedAt,
    payload: {fromCapturedAt: record.capturedAt, toCapturedAt: record.capturedAt, droppedRecords: 1, reason: 'outbox-age'}};
  const json = JSON.stringify(gap);
  spool.prepare("INSERT INTO outbox_records (run_id,sequence,captured_at,kind,state,gap_reason,offered,bytes,json) VALUES (?,?,?,'gap','inflight','outbox-age',1,?,?)")
    .run(identity.runId, 2, gap.capturedAt, Buffer.byteLength(json), json);
  spool.close();

  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  t.after(() => live.close());
  const collector = startChild('retained-real-collector', ['dist/collector.cjs'], baseEnv('collector', {
    COLLECTOR_MODE: 'real', COLLECTOR_STATE_FILE: statePath, COLLECTOR_INTERVAL_MS: '500',
  }));
  t.after(() => collector.stop());
  const current = await waitMetrics(live);
  assert.notEqual(current.metrics!.runId, identity.runId, 'replay cannot replace fresh live metrics');
  const db = new DatabaseSync(databasePath, {readOnly: true});
  try {
    const receipts = db.prepare('SELECT seq,kind FROM sample_receipts WHERE collector_id = ? AND run_id = ? ORDER BY seq');
    const deadline = Date.now() + 10000;
    while (receipts.all(collectorId, identity.runId).length < 2 && Date.now() < deadline) {
      await new Promise(resolve => setTimeout(resolve, 100));
    }
    assert.deepEqual(receipts.all(collectorId, identity.runId).map(r => ({...r})), [{seq: 1, kind: 'metrics'}, {seq: 2, kind: 'gap'}]);
    assert.equal(db.prepare('SELECT COUNT(*) AS count FROM gaps WHERE run_id = ? AND seq = 2').get(identity.runId)!.count, 1);

    // Retained canonical fields replay unchanged, while the state sharing
    // policy is applied again to memory before the record leaves the collector.
    assert.equal(record.payload.cpu.status, 'ok');
    assert.equal(record.payload.memory.status, 'ok');
    const capturedMs = Date.parse(record.capturedAt);
    const point = (metric: string) => {
      const series = db.prepare("SELECT series_id FROM series WHERE machine_id = ? AND metric = ? AND resource_id = ''")
        .get(creds.machineId, metric) as {series_id: number} | undefined;
      assert.ok(series !== undefined, `expected a ${metric} series`);
      return db.prepare('SELECT value FROM points_raw WHERE series_id = ? AND ts_ms = ?')
        .get(series.series_id, capturedMs) as {value: number | null} | undefined;
    };
    assert.equal(point('memory.used')?.value, null, 'withheld memory never reaches the hub');
    assert.equal(point('cpu.temperature')?.value, record.payload.cpu.temperatureC, 'canonical cpu field replays as stored');
  } finally { db.close(); }
  const state = JSON.parse(readFileSync(statePath, 'utf8'));
  assert.equal(state.collectorId, collectorId);
  assert.equal(state.destinations[0].url, hub.collectorWsUrl);
  assert.equal(state.destinations[0].token, creds.token);
  assert.deepEqual(state.destinations[0].sharing, {cpu: true, memory: false});
  assert.equal((await collector.stop()).code, 0);
});

test('packaged real collector: multi-hub live/history, process interests and durable hub restart', async t => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-durable-runtime-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const collectorId = 'real-runtime-collector';
  const credsA = credential(collectorId, 'machine-a', 'public-token-a');
  const credsB = credential(collectorId, 'machine-b', 'public-token-b');
  const databasePath = join(dir, 'hub-a', 'telemetry.sqlite');
  const hubA = await startHub(t, { collectors: [credsA], databasePath });
  const hubB = await startHub(t, { collectors: [credsB], databasePath: join(dir, 'hub-b', 'telemetry.sqlite') });
  const statePath = join(dir, 'collector.json');
  writeFileSync(statePath, JSON.stringify({ collectorId, destinations: [
    { url: hubA.collectorWsUrl, token: credsA.token }, { url: hubB.collectorWsUrl, token: credsB.token },
  ] }), { mode: 0o600 });
  const liveA = new SseClient(hubA.liveUrl, hubA.browserHeaders);
  const liveB = new SseClient(hubB.liveUrl, hubB.browserHeaders);
  t.after(() => { liveA.close(); liveB.close(); });
  const collector = startChild('durable-real-collector', ['dist/collector.cjs'], baseEnv('collector', {
    COLLECTOR_MODE: 'real', COLLECTOR_STATE_FILE: statePath, COLLECTOR_INTERVAL_MS: '500',
  }));
  t.after(() => collector.stop());
  const [firstA, firstB] = await Promise.all([waitMetrics(liveA), waitMetrics(liveB)]);
  assert.equal(firstA.collectorId, firstB.collectorId);
  const latestA = await waitMetrics(liveA, firstA.metrics!.sequence);
  assert.equal(latestA.metrics!.payload.cpu.status, 'ok');
  const browser = await loginBrowser(hubA.baseUrl);
  const url = `${hubA.baseUrl}/api/processes?machineId=${credsA.machineId}&viewerId=runtime-view`;
  const headers = { ...browser.headers, Origin: hubA.baseUrl, 'X-CSRF-Token': browser.csrfToken };
  assert.equal((await fetch(url, { method: 'POST', headers })).status, 200);
  let processes = 0;
  for (let tries = 0; tries < 20 && processes === 0; tries++) {
    const response = await fetch(url, { headers });
    assert.equal(response.status, 200);
    const data = await response.json() as { result: null | { snapshot: { totalProcesses: number } } };
    processes = data.result?.snapshot.totalProcesses ?? 0;
    if (!processes) await new Promise(resolve => setTimeout(resolve, 100));
  }
  assert.ok(processes > 0, 'actual Linux process scanning reached the browser query');
  assert.equal((await fetch(url, { method: 'DELETE', headers })).status, 200);
  const historyUrl = `/api/history?machineId=${credsA.machineId}&metrics=cpu.usage,memory.used&from=${Date.now() - 60000}&to=${Date.now() + 1000}`;
  const history = await fetch(hubA.baseUrl + historyUrl, { headers: browser.headers });
  assert.equal(history.status, 200);
  const before = await history.json() as { series: { points: unknown[] }[] };
  assert.ok(before.series.some(s => s.points.length > 0));
  liveA.close();
  assert.equal((await hubA.stop()).code, 0);
  const advancingB = await waitMetrics(liveB, firstB.metrics!.sequence + 2);
  assert.ok(advancingB.connected, 'a stopped destination does not interrupt the healthy hub');
  // Stop the collector so the hub restart must recover without fresh samples.
  assert.equal((await collector.stop()).code, 0);
  const restarted = await startHub(t, { port: hubA.port, collectors: [credsA], databasePath });
  const restoredLive = new SseClient(restarted.liveUrl, restarted.browserHeaders);
  t.after(() => restoredLive.close());
  const event = await restoredLive.next();
  const restored = parseHubSnapshot(JSON.parse(event.data)).machines[0];
  assert.equal(restored.connected, false);
  assert.ok(restored.metrics);
  assert.equal(restored.metrics!.bootId, latestA.metrics!.bootId);
  assert.ok(Date.parse(restored.metrics!.receivedAt) < Date.now());
  const restoredHistory = await fetch(restarted.baseUrl + historyUrl, { headers: restarted.browserHeaders });
  assert.equal(restoredHistory.status, 200);
  assert.ok((await restoredHistory.json() as { series: { points: unknown[] }[] }).series.some(s => s.points.length));
});
