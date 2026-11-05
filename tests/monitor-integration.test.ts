import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { WebSocket } from 'ws';
import { HubState } from '../src/hub/state';
import { CollectorGateway } from '../src/hub/collector-ws';
import { SampleIngestion } from '../src/hub/sample-ingestion';
import { openHubDatabase } from '../src/hub/storage/db';
import { TelemetryStore } from '../src/hub/storage/telemetry-store';
import { ProcessInterests } from '../src/hub/process-interests';
import { MonitorHttp } from '../src/hub/monitor-http';
import { BrowserHttpAuth, type AuthOriginRules } from '../src/hub/auth/http';
import { BrowserSessionStore } from '../src/hub/auth/sessions';
import { createSyntheticMetricsSampler } from '../collector/sampling/synthetic';
import { createSequenceAllocator } from '../collector/sampling/sequence';
import { parseHubMessage, type ProcessSnapshot } from '../src/contracts';
import { parseHubSnapshot } from '../src/contracts';

const identity = { collectorId: 'fixture-collector', runId: 'fixture-run', bootId: 'fixture-boot', hostname: 'synthetic-host' };
const emptyProcesses: ProcessSnapshot = { totalProcesses: 0, inaccessible: 0, truncated: false, processes: [] };

async function fixture(t: test.TestContext) {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-monitor-test-'));
  const db = openHubDatabase(join(dir, 'telemetry.sqlite'));
  const storage = new TelemetryStore({ database: db });
  const state = new HubState({ hubRunId: 'fixture-hub-run', collectors: [
    { collectorId: identity.collectorId, machineId: 'allowed', token: 'public-test-token' },
    { collectorId: 'other-collector', machineId: 'hidden', token: 'public-hidden-token' },
  ] });
  const sessions = new BrowserSessionStore();
  const user = sessions.create('viewer', ['allowed']);
  const denied = sessions.create('denied', []);
  const origin: AuthOriginRules = { originPort: 0, originHostnames: ['127.0.0.1'] };
  const auth = new BrowserHttpAuth({ users: [], sessions, origin });
  let gateway: CollectorGateway;
  const interests = new ProcessInterests((id, msg) => gateway.sendToMachine(id, msg));
  gateway = new CollectorGateway({ state, ingestion: new SampleIngestion({ state, storage }), onDetail: (id, msg) => interests.receive(id, msg), onDisconnect: id => interests.disconnect(id) });
  const monitor = new MonitorHttp({ auth, origin, state, storage, database: db, processes: interests });
  const server = createServer((req, res) => { void monitor.handle(req, res, new URL(req.url!, 'http://fixture')).then(handled => {
    if (!handled) { res.writeHead(404); res.end(); }
  }).catch(() => { res.writeHead(500); res.end(); }); });
  server.on('upgrade', (req, socket, head) => gateway.handleUpgrade(req, socket, head));
  server.listen(0, '127.0.0.1'); await once(server, 'listening');
  const port = (server.address() as { port: number }).port; origin.originPort = port;
  const base = `http://127.0.0.1:${port}`;
  const headers = { cookie: `rigscope_session=${user.id}`, origin: base, 'x-csrf-token': user.csrfToken };
  const ws = new WebSocket(`ws://127.0.0.1:${port}/api/collector/connect`, { headers: { Authorization: 'Bearer public-test-token' } });
  await once(ws, 'open');
  const hello = once(ws, 'message');
  ws.send(JSON.stringify({ type: 'hello', ...identity, capabilities: ['metrics', 'inventory', 'processes'], expectedIntervalMs: 5000 }));
  const welcome = parseHubMessage(JSON.parse(String((await hello)[0])));
  assert.equal(welcome.type, 'welcome');
  t.after(async () => {
    interests.stop(); ws.terminate(); gateway.stop(); sessions.dispose(); server.closeAllConnections();
    await new Promise<void>(resolve => server.close(() => resolve()));
    await storage.close(); db.close(); rmSync(dir, { recursive: true, force: true });
  });
  return { dir, db, storage, state, sessions, user, denied, ws, base, headers, interests };
}

test('canonical commit/ack, lost-ack deduplication, scoped History and replay-only ingestion', async t => {
  const f = await fixture(t);
  const sampler = createSyntheticMetricsSampler(identity, { sequence: createSequenceAllocator() });
  const record = sampler.next();
  async function sendRecord(mode: 'live' | 'replay', data = record) {
    const response = once(f.ws, 'message');
    f.ws.send(JSON.stringify({ type: 'samples', mode, records: [data] }));
    return parseHubMessage(JSON.parse(String((await response)[0])));
  }
  const ack = await sendRecord('live');
  assert.equal(ack.type, 'ack');
  if (ack.type === 'ack') assert.deepEqual(ack.accepted, [{ runId: record.runId, sequence: record.sequence }]);
  // An independent read connection sees the row before the socket ack can be used.
  const reader = openHubDatabase(join(f.dir, 'telemetry.sqlite'));
  assert.equal(reader.db.prepare('SELECT COUNT(*) AS n FROM sample_receipts').get()!.n, 1); reader.close();
  await sendRecord('live');
  assert.equal(f.db.db.prepare('SELECT COUNT(*) AS n FROM sample_receipts').get()!.n, 1);
  const before = f.state.snapshot().machines[0].metrics;
  const newer = { ...sampler.next(), capturedAt: new Date(Date.parse(record.capturedAt) + 1000).toISOString() };
  await sendRecord('replay', newer);
  assert.deepEqual(f.state.snapshot().machines[0].metrics, before);
  const snapshot = parseHubSnapshot(f.state.snapshot());
  assert.equal(snapshot.machines[0].metrics?.sequence, record.sequence);
  const from = Date.parse(record.capturedAt) - 10000;
  const url = `${f.base}/api/history?machineId=allowed&metrics=cpu.usage,gpu.memory.used&from=${from}&to=${from + 30000}`;
  assert.equal((await fetch(url)).status, 401);
  const good = await fetch(url, { headers: f.headers });
  assert.equal(good.status, 200);
  const result = await good.json() as { series: { metric: string; resourceId: string; points: unknown[] }[] };
  assert.ok(result.series.some(s => s.metric === 'gpu.memory.used' && s.resourceId === 'gpu:SYNTHETIC-0' && s.points.length));
  assert.equal((await fetch(url.replace('machineId=allowed', 'machineId=hidden'), { headers: f.headers })).status, 403);
  assert.equal((await fetch(url, { headers: { cookie: `rigscope_session=${f.denied.id}` } })).status, 403);
});

test('History admits selection bursts and two polling tabs while bounding abusive bursts', async t => {
  let monotonic = 1000;
  t.mock.method(performance, 'now', () => monotonic);
  const f = await fixture(t);
  const url = `${f.base}/api/history?machineId=allowed&metrics=cpu.usage&from=${Date.now() - 60_000}&to=${Date.now()}`;
  for (let i = 0; i < 6; i++) assert.equal((await fetch(url, {headers: f.headers})).status, 200, 'rapid metric selections must fit the burst budget');
  for (let second = 0; second < 12; second++) {
    monotonic += 1000;
    const tabs = await Promise.all([fetch(url, {headers: f.headers}), fetch(url, {headers: f.headers})]);
    assert.deepEqual(tabs.map(r => r.status), [200, 200], 'two History tabs must keep polling');
  }
  const statuses = [];
  for (let i = 0; i < 12; i++) statuses.push((await fetch(url, {headers: f.headers})).status);
  assert.ok(statuses.includes(429), 'unbounded request bursts remain rate limited');
});

test('process interests require scope + CSRF, return transient results, and cancel on session end', async t => {
  const f = await fixture(t);
  const url = `${f.base}/api/processes?machineId=allowed&viewerId=fixture-view`;
  assert.equal((await fetch(url, { method: 'POST', headers: { cookie: f.headers.cookie, origin: f.base } })).status, 403);
  assert.equal((await fetch(url.replace('allowed', 'hidden'), { method: 'POST', headers: f.headers })).status, 403);
  const request = once(f.ws, 'message');
  assert.equal((await fetch(url, { method: 'POST', headers: f.headers })).status, 200);
  const subscribe = parseHubMessage(JSON.parse(String((await request)[0])));
  assert.equal(subscribe.type, 'detail.subscribe');
  if (subscribe.type !== 'detail.subscribe') return;
  f.ws.send(JSON.stringify({ type: 'detail.result', subscriptionId: subscribe.subscriptionId,
    capturedAt: new Date().toISOString(), snapshot: emptyProcesses }));
  await new Promise(resolve => setImmediate(resolve));
  const response = await fetch(url, { headers: f.headers });
  assert.equal(response.status, 200);
  assert.equal((await response.json() as { state: string }).state, 'ready');
  assert.equal(f.db.db.prepare("SELECT COUNT(*) AS n FROM sqlite_master WHERE name LIKE '%process%'").get()!.n, 0);
  const cancel = once(f.ws, 'message');
  f.interests.endSession(f.user.id);
  const cancellation = parseHubMessage(JSON.parse(String((await cancel)[0])));
  assert.equal(cancellation.type, 'detail.cancel');
  assert.equal((await fetch(url, { headers: f.headers })).status, 410);
});

test('viewer interests coalesce, expire by monotonic lease and drop cached process details', () => {
  let now = 0;
  const sent: { machineId: string; type: string }[] = [];
  const broker = new ProcessInterests((machineId, msg) => { sent.push({ machineId, type: msg.type }); return true; }, () => now);
  try {
    assert.equal(broker.renew('session-a', 'machine', 'view-a').intervalMs, 1000);
    broker.renew('session-b', 'machine', 'view-b');
    assert.equal(sent.filter(m => m.type === 'detail.subscribe').length, 1);
    broker.cancel('session-a', 'view-a');
    assert.equal(sent.filter(m => m.type === 'detail.cancel').length, 0);
    now = 16000;
    assert.equal(broker.read('session-b', 'machine', 'view-b'), null);
    assert.equal(sent.filter(m => m.type === 'detail.cancel').length, 1);
  } finally { broker.stop(); }
});

test('process viewer exhaustion returns an actionable capacity response', async t => {
  const f = await fixture(t);
  for (let i = 0; i < 8; i++) {
    const response = await fetch(`${f.base}/api/processes?machineId=allowed&viewerId=view-${i}`, { method: 'POST', headers: f.headers });
    assert.equal(response.status, 200);
  }
  const response = await fetch(`${f.base}/api/processes?machineId=allowed&viewerId=overflow`, { method: 'POST', headers: f.headers });
  assert.equal(response.status, 429);
  assert.match((await response.json() as {error:string}).error, /budget.*Close another Processes view/i);
});
