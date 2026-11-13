import test from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, mkdtempSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { startCollectorProcess, startEnrollmentProcess } from './support/launcher';
import { failOnExit } from './support/process';
import { startHub } from './support/fixtures';
import { loginBrowser } from './support/browser';
import { credential } from './support/config';
import { within } from './support/deadline';
import { SseClient } from './support/sse';
import { parseHubSnapshot, type MachineState } from '../../src/contracts';

async function waitMetrics(live: SseClient, minimumSequence = -1): Promise<MachineState> {
  const deadline = Date.now() + 20000;
  while (Date.now() < deadline) {
    const event = await live.next(Math.max(1, deadline - Date.now()));
    const machine = parseHubSnapshot(JSON.parse(event.data)).machines[0];
    if (machine?.metrics && machine.metrics.sequence > minimumSequence) return machine;
  }
  throw new Error('metrics did not arrive');
}

/** Wait for a settled CPU reading so the history assertions are meaningful. */
async function waitSettledMetrics(
  live: SseClient,
  minimumSequence = -1,
  excludeRunId?: string,
): Promise<MachineState> {
  const deadline = Date.now() + 20000;
  while (Date.now() < deadline) {
    const event = await live.next(Math.max(1, deadline - Date.now()));
    const machine = parseHubSnapshot(JSON.parse(event.data)).machines[0];
    const metrics = machine?.metrics;
    if (!metrics) continue;
    // After a restart the live snapshot can still hold the previous process's
    // last sample. A new runId is the only proof this is the new collector.
    if (excludeRunId !== undefined && metrics.runId === excludeRunId) continue;
    if (excludeRunId === undefined && metrics.sequence <= minimumSequence) continue;
    if (metrics.payload.cpu.status === 'ok') return machine;
  }
  throw new Error('settled metrics did not arrive');
}

type HistoryBody = {
  series: Array<{ metric: string; points: Array<{ min: number | null; max: number | null }> }>;
};

async function waitHistory(
  hubBase: string,
  headers: Record<string, string>,
  machineId: string,
  metrics: string,
  predicate: (body: HistoryBody) => boolean,
): Promise<HistoryBody> {
  const url = `${hubBase}/api/history?machineId=${machineId}&metrics=${metrics}` +
    `&from=${Date.now() - 60_000}&to=${Date.now() + 5_000}`;
  const deadline = Date.now() + 15_000;
  let body: HistoryBody = { series: [] };
  while (Date.now() < deadline) {
    const response = await fetch(url, { headers });
    assert.equal(response.status, 200);
    body = await response.json() as HistoryBody;
    if (predicate(body)) return body;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  return body;
}

async function waitInventoryMachine(live: SseClient, machineId: string): Promise<MachineState> {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    const event = await live.next(Math.max(1, deadline - Date.now()));
    const machine = parseHubSnapshot(JSON.parse(event.data)).machines[0];
    if (machine?.machineId === machineId && machine.inventory) return machine;
  }
  throw new Error('inventory did not arrive');
}

/**
 * The enrollment-time sharing policy is part of the installation: it survives
 * a restart, withholds memory from live state, inventory and persistent
 * history, and never fabricates a memory reading.
 */
test('packaged collector: enrollment sharing withholds memory across restart while CPU reaches history', async t => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-privacy-runtime-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const token = 'demo-token-enroll-privacy';
  const hubDir = join(dir, 'hub');
  const stateDir = join(dir, 'collector');
  const hub = await startHub(t, {
    stateDir: hubDir,
    telemetryMode: 'persistent',
    databasePath: join(hubDir, 'telemetry.sqlite'),
    grants: [{ token, expiresAt: new Date(Date.now() + 600000).toISOString() }],
  });
  const enrollment = startEnrollmentProcess(t, {
    label: 'privacy enrollment',
    stateDir,
    url: hub.collectorWsUrl,
    token,
    sharing: { cpu: true, memory: false },
  });
  assert.equal((await within(enrollment.exit, 10000, 'privacy enrollment')).code, 0, enrollment.dump());

  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  t.after(() => live.close());
  const collector = startCollectorProcess(t, {
    label: 'privacy collector',
    mode: 'real',
    stateDir,
    intervalMs: 250,
  });
  const first = await failOnExit(collector, waitSettledMetrics(live), 'privacy metrics');
  assert.equal(first.metrics!.payload.cpu.status, 'ok', 'real CPU reaches live state');
  assert.deepEqual(
    first.metrics!.payload.memory,
    { status: 'unavailable', reason: 'withheld by local sharing policy' },
    'enrollment policy withholds memory live',
  );
  const inventoried = await failOnExit(collector, waitInventoryMachine(live, first.machineId), 'privacy inventory');
  assert.equal(inventoried.inventory!.capabilities.memory.status, 'withheld', 'inventory carries the same policy');

  // CPU reaches history, while withheld memory can only appear as null points.
  const history = await waitHistory(
    hub.baseUrl,
    hub.browserHeaders,
    first.machineId,
    'cpu.usage,memory.used',
    body => body.series.some(series => series.metric === 'cpu.usage' && series.points.length > 0),
  );
  const cpu = history.series.find(series => series.metric === 'cpu.usage');
  assert.ok(cpu && cpu.points.length > 0, 'CPU reaches persistent history');
  const memory = history.series.find(series => series.metric === 'memory.used');
  assert.ok(
    memory === undefined || memory.points.every(point => point.min === null && point.max === null),
    'withheld memory never becomes a value in history',
  );

  // A restart rebuilds the runtime config from the installation; the
  // enrollment-time policy must survive that rebuild unchanged.
  assert.equal((await collector.stop()).code, 0);
  const restarted = startCollectorProcess(t, {
    label: 'privacy collector restart',
    mode: 'real',
    stateDir,
    intervalMs: 250,
  });
  const after = await failOnExit(
    restarted,
    waitSettledMetrics(live, -1, first.metrics!.runId),
    'privacy restart metrics',
  );
  assert.equal(after.collectorId, first.collectorId, 'restart preserves the installation identity');
  assert.notEqual(after.metrics!.runId, first.metrics!.runId, 'the restart samples under a new runId');
  assert.deepEqual(
    after.metrics!.payload.memory,
    { status: 'unavailable', reason: 'withheld by local sharing policy' },
    'enrollment sharing survives the restart/config rebuild',
  );
  assert.equal((await restarted.stop()).code, 0);
});

/**
 * Persistent history, process-detail interests and hub-restart recovery on a
 * single hub. 1.0 has one hub per collector and a bounded RAM queue, so the
 * multi-hub and durable-outbox variants are gone.
 */
test('packaged real collector: persistent live/history, process interests and hub restart', async t => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-durable-runtime-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const collectorId = 'real-runtime-collector';
  const creds = credential(collectorId, 'machine-a', 'public-token-a');
  const hubDir = join(dir, 'hub');
  const databasePath = join(hubDir, 'telemetry.sqlite');
  const hub = await startHub(t, { stateDir: hubDir, collectors: [creds], databasePath, telemetryFlushMs: null, telemetryMode: 'persistent' });
  const statusResponse = await fetch(`${hub.baseUrl}/api/history/status`, { headers: hub.browserHeaders });
  assert.equal(statusResponse.status, 200);
  const status = await statusResponse.json() as { persistence: { flushIntervalMs: number } };
  assert.equal(status.persistence.flushIntervalMs, 5000, 'persistent mode reports its effective default batch delay');
  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  t.after(() => live.close());
  const collector = startCollectorProcess(t, {
    label: 'durable-real-collector',
    mode: 'real',
    collectorId,
    hubs: [{ url: hub.collectorWsUrl, token: creds.token }],
    intervalMs: 500,
  });
  const first = await failOnExit(collector, waitMetrics(live), 'durable collector metrics');
  const latest = await failOnExit(collector, waitMetrics(live, first.metrics!.sequence), 'advancing durable metrics');
  assert.equal(latest.metrics!.payload.cpu.status, 'ok');
  const browser = await loginBrowser(hub.baseUrl);
  const url = `${hub.baseUrl}/api/processes?machineId=${creds.machineId}&viewerId=runtime-view`;
  const headers = { ...browser.headers, Origin: hub.baseUrl, 'X-CSRF-Token': browser.csrfToken };
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
  const historyUrl = `/api/history?machineId=${creds.machineId}&metrics=cpu.usage,memory.used&from=${Date.now() - 60000}&to=${Date.now() + 1000}`;
  let before: { series: { points: unknown[] }[] } = { series: [] };
  const historyDeadline = Date.now() + 15000;
  while (Date.now() < historyDeadline) {
    const history = await fetch(hub.baseUrl + historyUrl, { headers: browser.headers });
    assert.equal(history.status, 200);
    before = await history.json() as typeof before;
    if (before.series.some(s => s.points.length > 0)) break;
    // Match the history query budget while waiting for the real 5-second
    // persistent batch; faster polling exhausts the session's token bucket.
    await new Promise(resolve => setTimeout(resolve, 300));
  }
  assert.ok(before.series.some(s => s.points.length > 0));
  live.close();
  // Stop the collector so the hub restart must recover history without fresh samples.
  assert.equal((await collector.stop()).code, 0);
  assert.equal((await hub.stop()).code, 0);
  const restarted = await startHub(t, {
    stateDir: hubDir, port: hub.port, collectors: [creds], databasePath, telemetryFlushMs: null, telemetryMode: 'persistent',
  });
  const restoredLive = new SseClient(restarted.liveUrl, restarted.browserHeaders);
  t.after(() => restoredLive.close());
  const event = await restoredLive.next();
  const restored = parseHubSnapshot(JSON.parse(event.data)).machines[0];
  assert.equal(restored.connected, false);
  assert.ok(restored.metrics);
  assert.equal(restored.metrics!.bootId, latest.metrics!.bootId);
  assert.ok(Date.parse(restored.metrics!.receivedAt) < Date.now());
  const restoredHistory = await fetch(restarted.baseUrl + historyUrl, { headers: restarted.browserHeaders });
  assert.equal(restoredHistory.status, 200);
  assert.ok((await restoredHistory.json() as { series: { points: unknown[] }[] }).series.some(s => s.points.length));
});

test('default RAM hub loses telemetry on restart while preserving collector enrollment', async t => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-ram-hub-runtime-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const creds = credential('ram-hub-collector', 'ram-hub-machine', 'public-ram-hub-token');
  const hub = await startHub(t, { stateDir: dir, collectors: [creds] });
  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  t.after(() => live.close());
  const collector = startCollectorProcess(t, {
    mode: 'mock', collectorId: creds.collectorId,
    hubs: [{ url: hub.collectorWsUrl, token: creds.token }], intervalMs: 250,
  });
  await failOnExit(collector, waitMetrics(live), 'RAM hub metrics');
  await waitHistory(hub.baseUrl, hub.browserHeaders, creds.machineId, 'cpu.usage', body => body.series.some(s => s.points.length > 0));
  const before = await fetch(`${hub.baseUrl}/api/history/status`, { headers: hub.browserHeaders });
  const status = await before.json() as { mode: string; persistentHistory: boolean };
  assert.equal(status.mode, 'memory');
  assert.equal(status.persistentHistory, false);
  assert.equal((await collector.stop()).code, 0);
  live.close();
  assert.equal((await hub.stop()).code, 0);
  const restarted = await startHub(t, { stateDir: dir, port: hub.port, collectors: [creds] });
  const fresh = new SseClient(restarted.liveUrl, restarted.browserHeaders);
  t.after(() => fresh.close());
  const event = await fresh.next();
  const machine = parseHubSnapshot(JSON.parse(event.data)).machines[0];
  assert.equal(machine.machineId, creds.machineId, 'enrollment survives');
  assert.equal(machine.metrics == null, true, 'telemetry is lost on hub restart');
  const after = await fetch(`${restarted.baseUrl}/api/history/status`, { headers: restarted.browserHeaders });
  assert.equal((await after.json() as { acceptedRecords: number }).acceptedRecords, 0);
  const history = await fetch(`${restarted.baseUrl}/api/history?machineId=${creds.machineId}&metrics=cpu.usage&from=${Date.now() - 60_000}&to=${Date.now() + 1_000}`, { headers: restarted.browserHeaders });
  assert.equal(history.status, 200);
  assert.equal((await history.json() as HistoryBody).series.some(s => s.points.length > 0), false);
});

test('the default collector writes no queue file and reports its memory queue', async t => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-ram-queue-runtime-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const creds = credential('ram-queue-collector', 'ram-queue-machine', 'public-ram-queue-token');
  const hub = await startHub(t, { collectors: [creds], telemetryMode: 'persistent' });

  const stateDir = join(dir, 'collector');
  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  t.after(() => live.close());
  const collector = startCollectorProcess(t, {
    label: 'ram-queue-collector',
    mode: 'mock',
    stateDir,
    collectorId: creds.collectorId,
    hostname: 'ram-queue-machine',
    hubs: [{ url: hub.collectorWsUrl, token: creds.token }],
    intervalMs: 250,
  });

  // Samples are being delivered, so the queue is in use, not merely unopened.
  await failOnExit(collector, waitMetrics(live), 'default-queue metrics');

  // Persistent history serves these samples.
  const historyUrl = `${hub.baseUrl}/api/history?machineId=${creds.machineId}` +
    `&metrics=cpu.usage&from=${Date.now() - 60_000}&to=${Date.now() + 1_000}`;
  let history: { series: { points: unknown[] }[] } = { series: [] };
  const historyDeadline = Date.now() + 15_000;
  while (Date.now() < historyDeadline) {
    const response = await fetch(historyUrl, { headers: hub.browserHeaders });
    assert.equal(response.status, 200);
    history = await response.json() as typeof history;
    if (history.series.some((series) => series.points.length > 0)) break;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  assert.ok(history.series.some((series) => series.points.length > 0),
    'history serves this collector\'s samples');

  // The default queue is RAM only: routine delivery creates no queue on disk.
  assert.equal(existsSync(join(stateDir, 'outbox')), false, 'the default queue must not create a directory');
  assert.deepEqual(
    existsSync(stateDir) ? readdirSync(stateDir).filter((name) => name.startsWith('outbox')) : [],
    [],
    'no queue file or WAL may appear in the collector state directory',
  );

  // The hub must be able to tell an operator that this collector's pending
  // samples do not survive a collector restart.
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    const event = await live.next(Math.max(1, deadline - Date.now()));
    const machine = parseHubSnapshot(JSON.parse(event.data)).machines[0];
    if (machine?.delivery) {
      assert.equal(machine.delivery.delivery.queueMode, 'memory');
      return;
    }
  }
  throw new Error('the collector never reported its delivery status');
});
