import assert from 'node:assert/strict';
import test, {type TestContext} from 'node:test';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { startHub, type HubController } from './support/fixtures';
import { readCollectorState, startCollectorProcess, startEnrollmentProcess } from './support/launcher';
import { failOnExit, type ChildHandle } from './support/process';
import { SseClient } from './support/sse';
import { parseSnapshot, type ReceivedSampleShape } from './support/wire';
import { within } from './support/deadline';

function fixture(t: TestContext): string {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-real-'));
  t.after(() => rmSync(root, {recursive:true,force:true}));
  return root;
}
async function enroll(t: TestContext, stateDir: string, hub: HubController, token: string, sharing = {cpu:true,memory:true}): Promise<void> {
  const child = startEnrollmentProcess(t, {
    label: 'collector enrollment',
    stateDir,
    url: hub.collectorWsUrl,
    token,
    sharing,
  });
  const result = await within(child.exit,10000,'enrollment exit');
  assert.equal(result.code,0,child.dump());
  assert.ok(!child.stdoutLines().join().includes(token));
}
function runCollector(t: TestContext, stateDir: string) {
  return startCollectorProcess(t, {
    label: 'real collector',
    mode: 'real',
    stateDir,
    intervalMs: 200,
  });
}
async function summaries(
  client: SseClient,
  count: number,
  guards: ChildHandle | ChildHandle[],
): Promise<Map<string,ReceivedSampleShape>> {
  const found = new Map<string,ReceivedSampleShape>();
  const deadline = Date.now()+10000;
  while (Date.now()<deadline) {
    const event = await client.next(deadline-Date.now());
    const snapshot = parseSnapshot(event.data,'real collector stream');
    for (const machine of snapshot.machines) {
      if (machine.connected && machine.metrics?.payload.cpu.status === 'ok') found.set(machine.collectorId,machine.metrics);
    }
    if (found.size===count) return found;
  }
  throw new Error('real metrics did not arrive');
}

/**
 * Two independent collectors enroll into ONE hub, sample before any browser
 * exists, deliver real CPU/memory from one host, advance their sequences, shut
 * down cleanly, and a restart keeps the installation identity but starts a
 * fresh run.
 */
test('real collectors enroll into one hub: distinct identities, pre-browser sampling and restart', async t => {
  const root = fixture(t);
  const grantA = 'demo-token-enroll-a';
  const grantB = 'demo-token-enroll-b';
  const expiresAt = new Date(Date.now()+600000).toISOString();
  const hub = await startHub(t,{stateDir:join(root,'hub'),grants:[{token:grantA,expiresAt},{token:grantB,expiresAt}]});
  const dirA = join(root,'collector-a');
  const dirB = join(root,'collector-b');
  await enroll(t,dirA,hub,grantA);
  await enroll(t,dirB,hub,grantB);
  const stateA = readCollectorState(dirA)!;
  const stateB = readCollectorState(dirB)!;
  assert.notEqual(stateA.collectorId,stateB.collectorId,'independent installations keep distinct identities');
  assert.notEqual(stateA.destinations[0].token,stateB.destinations[0].token,'credentials are issued per enrollment');
  assert.equal(stateA.destinations.length,1,'a 1.0 installation has exactly one hub');
  const ca = runCollector(t,dirA); const cb = runCollector(t,dirB);
  // No browser exists while the collectors connect and begin sampling.
  await Promise.all([
    ca.waitForLine(s=>s.includes('welcomed'),10000,'collector A welcome'),
    cb.waitForLine(s=>s.includes('welcomed'),10000,'collector B welcome'),
  ]);
  const live = new SseClient(hub.liveUrl, hub.browserHeaders);
  t.after(()=>live.close());
  const samples = await failOnExit([ca,cb], summaries(live,2,[ca,cb]), 'both enrolled collectors reaching the hub');
  assert.equal(samples.get(stateA.collectorId)!.payload.memory.status,'ok','real memory metrics from collector A');
  assert.equal(samples.get(stateB.collectorId)!.payload.memory.status,'ok','real memory metrics from collector B');
  const first = samples.get(stateA.collectorId)!;
  assert.equal(first.payload.cpu.status,'ok');
  let next: ReceivedSampleShape;
  const progressDeadline=Date.now()+10000;
  do {
    assert.ok(Date.now()<progressDeadline,'healthy delivery must keep advancing');
    next=(await failOnExit([ca,cb],summaries(live,2,[ca,cb]),'advancing delivery')).get(stateA.collectorId)!;
  } while(next.sequence<first.sequence+3);
  assert.equal((await ca.stop()).code,0,'collector A shuts down cleanly');
  const restarted = runCollector(t,dirA);
  let fresh: ReceivedSampleShape;
  const restartDeadline=Date.now()+10000;
  do {
    assert.ok(Date.now()<restartDeadline,'restart must produce a fresh run');
    fresh=(await failOnExit([restarted,cb],summaries(live,2,[restarted,cb]),'restart delivery')).get(stateA.collectorId)!;
  } while(fresh.runId===first.runId);
  assert.equal(fresh.collectorId,first.collectorId,'a restart preserves the persistent collector identity');
  assert.notEqual(fresh.runId,first.runId,'a restart starts a fresh run');
  assert.equal((await restarted.stop()).code,0);
  assert.equal((await cb.stop()).code,0);
});

test('enrolled hub credentials and consumed grants survive hub restart', async t => {
  const root = fixture(t);
  const collectorDir = join(root,'collector');
  const hubDir = join(root,'hub');
  const databasePath = join(hubDir,'telemetry.sqlite');
  const token='demo-token-restart-grant';
  const grants=[{token,expiresAt:new Date(Date.now()+600000).toISOString()}];
  const first=await startHub(t,{stateDir:hubDir,databasePath,grants});
  await enroll(t,collectorDir,first,token);
  const state=readCollectorState(collectorDir)!;
  await first.stop();
  // Same state directory, database and port: the issued credential stays valid.
  const second=await startHub(t,{stateDir:hubDir,databasePath,port:first.port,grants});
  const replay = await fetch(`${second.baseUrl}/api/collector/enroll`,{method:'POST',headers:{'Content-Type':'application/json',Authorization:`Bearer ${token}`},body:JSON.stringify({collectorId:'another-installation'})});
  assert.equal(replay.status,403); await replay.arrayBuffer();
  const child=runCollector(t,collectorDir); const live=new SseClient(second.liveUrl, second.browserHeaders);t.after(()=>live.close());
  assert.equal((await failOnExit(child,summaries(live,1,child),'delivery after hub restart')).get(state.collectorId)!.payload.memory.status,'ok');
  assert.equal((await child.stop()).code,0);
  const registryDb = new DatabaseSync(databasePath, {readOnly:true});
  try {
    const rows = registryDb.prepare('SELECT token_hash FROM collector_grants').all();
    assert.ok(rows.length > 0);
    assert.ok(!JSON.stringify(rows).includes(token), 'bootstrap token is hashed');
  } finally { registryDb.close(); }
});
