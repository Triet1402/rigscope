import assert from 'node:assert/strict';
import test, {type TestContext} from 'node:test';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { startHub, baseEnv, type HubController } from './support/fixtures';
import { startChild } from './support/process';
import { SseClient } from './support/sse';
import { parseSnapshot, type ReceivedSampleShape } from './support/wire';
import { closedLoopbackPort } from './support/http';
import { updateInstallation } from '../../collector/local-state';
import { within } from './support/deadline';
import { startTlsProxy } from './support/tls-proxy';
import { wsUpgrade } from './support/ws';

function fixture(t: TestContext): string {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-real-'));
  t.after(() => rmSync(root, {recursive:true,force:true}));
  return root;
}
async function enroll(t: TestContext, path: string, hub: HubController, token: string, sharing = {cpu:true,memory:true}): Promise<void> {
  const child = startChild('collector enrollment',['dist/collector.cjs','--enroll'],baseEnv('collector', {
    COLLECTOR_STATE_FILE:path, COLLECTOR_ENROLL_URL:hub.collectorWsUrl, COLLECTOR_ENROLL_TOKEN:token,
    COLLECTOR_SHARING:JSON.stringify(sharing),
  }));
  t.after(() => child.stop());
  const result = await within(child.exit,10000,'enrollment exit');
  assert.equal(result.code,0,child.dump());
  assert.ok(!child.stdoutLines().join().includes(token));
}
function runCollector(t: TestContext, path: string) {
  const child = startChild('real collector',['dist/collector.cjs'],baseEnv('collector',{
    COLLECTOR_MODE:'real', COLLECTOR_STATE_FILE:path, COLLECTOR_INTERVAL_MS:'200',
  }));
  t.after(() => child.stop());
  return child;
}
async function summaries(client: SseClient, count: number): Promise<Map<string,ReceivedSampleShape>> {
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

test('real collectors enroll with persistent identities: fan-in, filtered fan-out and offline isolation', async t => {
  const root = fixture(t);
  const grantA = 'demo-token-enroll-a';
  const grantB = 'demo-token-enroll-b';
  const grantC = 'demo-token-enroll-c';
  const expiresAt = new Date(Date.now()+600000).toISOString();
  const hubA = await startHub(t,{stateFile:join(root,'hub-a','state.json'),grants:[{token:grantA,expiresAt},{token:grantB,expiresAt}]});
  const hubB = await startHub(t,{stateFile:join(root,'hub-b','state.json'),grants:[{token:grantC,expiresAt}]});
  const pathA = join(root,'collector-a','state.json');
  const pathB = join(root,'collector-b','state.json');
  await enroll(t,pathA,hubA,grantA);
  await enroll(t,pathB,hubA,grantB);
  await enroll(t,pathA,hubB,grantC,{cpu:true,memory:false});
  const offlinePort = await closedLoopbackPort();
  const stateA = updateInstallation(pathA,s=>({...s,destinations:[...s.destinations,{url:`ws://127.0.0.1:${offlinePort}/api/collector/connect`,token:'demo-token-offline'}]}));
  const stateB = updateInstallation(pathB);
  assert.notEqual(stateA.collectorId,stateB.collectorId);
  assert.notEqual(stateA.destinations[0].token,stateA.destinations[1].token);
  const wrongHub = await wsUpgrade(hubB.collectorWsUrl,{Authorization:`Bearer ${stateA.destinations[0].token}`});
  assert.equal(wrongHub.kind,'rejected','a credential from hub A cannot authenticate to hub B');
  if (wrongHub.kind === 'rejected') assert.equal(wrongHub.status,401);
  const ca = runCollector(t,pathA); const cb = runCollector(t,pathB);
  // No browser exists while the collectors connect and begin sampling.
  await Promise.all([ca.waitForLine(s=>s.includes('welcomed'),10000,'collector A welcome'), cb.waitForLine(s=>s.includes('welcomed'),10000,'collector B welcome')]);
  const liveA = new SseClient(hubA.liveUrl, hubA.browserHeaders); const liveB = new SseClient(hubB.liveUrl, hubB.browserHeaders);
  t.after(()=>{liveA.close();liveB.close();});
  const [samplesA,samplesB] = await Promise.all([summaries(liveA,2),summaries(liveB,1)]);
  assert.equal(samplesA.get(stateB.collectorId)!.payload.memory.status,'ok');
  assert.equal(samplesA.get(stateA.collectorId)!.payload.memory.status,'ok');
  assert.equal(samplesB.get(stateA.collectorId)!.payload.memory.status,'unavailable');
  // Find one exact sample sequence observed by both hubs, proving one scan
  // supplies both, while credentials/mappings and payload policy stay local.
  const latestA = new Map<number,ReceivedSampleShape>([[samplesA.get(stateA.collectorId)!.sequence,samplesA.get(stateA.collectorId)!]]);
  const latestB = new Map<number,ReceivedSampleShape>([[samplesB.get(stateA.collectorId)!.sequence,samplesB.get(stateA.collectorId)!]]);
  for (let n=0; ![...latestA.keys()].some(k=>latestB.has(k)); n++) {
    assert.ok(n<10,'both hubs must observe a shared sample sequence');
    const [nextA,nextB] = await Promise.all([summaries(liveA,2),summaries(liveB,1)]);
    const a=nextA.get(stateA.collectorId)!; const b=nextB.get(stateA.collectorId)!;
    latestA.set(a.sequence,a);latestB.set(b.sequence,b);
  }
  const seq = [...latestA.keys()].find(k=>latestB.has(k))!;
  const a=latestA.get(seq)!;const b=latestB.get(seq)!;
  assert.deepEqual(a.payload.cpu,b.payload.cpu);
  assert.equal(a.capturedAt,b.capturedAt);assert.equal(a.runId,b.runId);
  const firstSequence = a.sequence;
  let next: ReceivedSampleShape;
  const progressDeadline=Date.now()+10000;
  do { assert.ok(Date.now()<progressDeadline,'healthy delivery must keep advancing'); next=(await summaries(liveA,2)).get(stateA.collectorId)!; } while(next.sequence<firstSequence+3);
  assert.equal(ca.proc.exitCode,null,'offline destination does not stop sampling');
  assert.equal((await ca.stop()).code,0);
  const restarted = runCollector(t,pathA);
  let fresh: ReceivedSampleShape;
  const restartDeadline=Date.now()+10000;
  do { assert.ok(Date.now()<restartDeadline,'restart must produce a fresh run'); fresh=(await summaries(liveA,2)).get(stateA.collectorId)!; } while(fresh.runId===a.runId);
  assert.equal(fresh.collectorId,a.collectorId); assert.notEqual(fresh.runId,a.runId);
  assert.equal((await restarted.stop()).code,0);
  assert.equal((await cb.stop()).code,0);
});

test('enrolled hub credentials and consumed grants survive hub restart', async t => {
  const root = fixture(t); const path = join(root,'collector','state.json');
  const stateFile = join(root,'hub','state.json'); const token='demo-token-restart-grant';
  const grants=[{token,expiresAt:new Date(Date.now()+600000).toISOString()}];
  const first=await startHub(t,{stateFile,grants});
  await enroll(t,path,first,token);
  const state=updateInstallation(path);
  await first.stop();
  const second=await startHub(t,{stateFile,grants});
  updateInstallation(path,s=>({...s,destinations:[{...s.destinations[0],url:second.collectorWsUrl}]}));
  const replay = await fetch(`${second.baseUrl}/api/collector/enroll`,{method:'POST',headers:{'Content-Type':'application/json',Authorization:`Bearer ${token}`},body:JSON.stringify({collectorId:'another-installation'})});
  assert.equal(replay.status,403); await replay.arrayBuffer();
  const child=runCollector(t,path); const live=new SseClient(second.liveUrl, second.browserHeaders);t.after(()=>live.close());
  assert.equal((await summaries(live,1)).get(state.collectorId)!.payload.memory.status,'ok');
  assert.equal((await child.stop()).code,0);
  const registryDb = new DatabaseSync(join(root,'hub','telemetry.sqlite'), {readOnly:true});
  try {
    const rows = registryDb.prepare('SELECT token_hash FROM collector_grants').all();
    assert.ok(rows.length > 0);
    assert.ok(!JSON.stringify(rows).includes(token), 'bootstrap token is hashed');
  } finally { registryDb.close(); }
});


test('real enrollment and sampling over TLS require a trusted certificate', async t => {
  const root=fixture(t);const token='demo-token-enroll-tls';const path=join(root,'collector','state.json');
  const hub=await startHub(t,{stateFile:join(root,'hub','state.json'),grants:[{token,expiresAt:new Date(Date.now()+600000).toISOString()}]});
  const proxy=await startTlsProxy(hub);
  if (!proxy) {t.skip('openssl unavailable');return;}
  t.after(()=>proxy.close());
  const url=`wss://127.0.0.1:${proxy.port}/api/collector/connect`;
  const ca=join(root,'ca.pem');writeFileSync(ca,proxy.caPem,{mode:0o600});
  const variables={COLLECTOR_STATE_FILE:path,COLLECTOR_ENROLL_URL:url,COLLECTOR_ENROLL_TOKEN:token};
  const untrusted=startChild('untrusted enrollment',['dist/collector.cjs','--enroll'],baseEnv('collector',{...variables,NODE_EXTRA_CA_CERTS:undefined}));
  t.after(()=>untrusted.stop());assert.equal((await within(untrusted.exit,10000,'untrusted enrollment')).code,1);
  assert.equal(updateInstallation(path).destinations.length,0,'failed TLS never stores a credential');
  const trusted=startChild('trusted enrollment',['dist/collector.cjs','--enroll'],baseEnv('collector',{...variables,NODE_EXTRA_CA_CERTS:ca}));
  t.after(()=>trusted.stop());assert.equal((await within(trusted.exit,10000,'trusted enrollment')).code,0,trusted.dump());
  const child=startChild('TLS real collector',['dist/collector.cjs'],baseEnv('collector',{COLLECTOR_STATE_FILE:path,COLLECTOR_INTERVAL_MS:'200',NODE_EXTRA_CA_CERTS:ca}));
  t.after(()=>child.stop());const live=new SseClient(hub.liveUrl, hub.browserHeaders);t.after(()=>live.close());
  assert.equal((await summaries(live,1)).size,1);
  assert.equal((await child.stop()).code,0);
});
