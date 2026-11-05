import test from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { updateInstallation } from '../collector/local-state';
import { createMetricsFilter } from '../collector/destinations/policy';
import { createSyntheticMetricsSampler } from '../collector/sampling/synthetic';
import { createSequenceAllocator } from '../collector/sampling/sequence';
import { parseCollectorConfig } from '../collector/config';

test('installation identity survives restarts; independent private destination credentials persist', t => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-state-')); t.after(() => rmSync(dir, {recursive: true, force: true}));
  const path = join(dir, 'collector.json');
  const first = updateInstallation(path);
  const destination = {url: 'ws://127.0.0.1:8081/api/collector/connect', token: 'test-private', sharing: {cpu: true, memory: false}};
  updateInstallation(path, s => ({...s, destinations: [destination]}));
  const second = updateInstallation(path);
  assert.equal(second.collectorId, first.collectorId);
  assert.deepEqual(second.destinations, [destination]);
  assert.equal(statSync(path).mode & 0o777, 0o600);
  assert.equal(updateInstallation(join(dir, 'other.json')).collectorId === first.collectorId, false);
  writeFileSync(path, '{broken');
  assert.throws(() => updateInstallation(path));
  assert.equal(readFileSync(path, 'utf8'), '{broken');
});

test('versioned collector state is rejected without rewriting the file', t => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-state-versioned-'));
  t.after(() => rmSync(dir, {recursive: true, force: true}));
  const path = join(dir, 'collector.json');
  for (const versionField of ['version', 'formatVersion']) {
    const original = JSON.stringify({
      [versionField]: 1,
      collectorId: 'existing-installation',
      destinations: [{url: 'wss://hub.example:8081/api/collector/connect', token: 'private-credential'}],
      pendingEnrollments: [],
    });
    writeFileSync(path, original, {mode: 0o600});
    assert.throws(() => updateInstallation(path), /older versioned shape/);
    assert.equal(readFileSync(path, 'utf8'), original, `${versionField} state must be left byte-for-byte unchanged`);
  }
});

test('saved obsolete endpoints are rejected instead of normalized', t => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-state-obsolete-'));
  t.after(() => rmSync(dir, {recursive: true, force: true}));
  const path = join(dir, 'collector.json');
  const oldUrl = 'wss://hub.example:8081/api/collector/v1/connect';
  for (const original of [
    {
      collectorId: 'existing-installation',
      destinations: [{url: oldUrl, token: 'private-credential', intervalMs: 15000, sharing: {cpu: true, memory: false}}],
    },
    {
      collectorId: 'existing-installation',
      destinations: [{url: 'wss://hub.example:8081/api/collector/connect', token: 'private-credential'}],
      pendingEnrollments: [{url: oldUrl, requestId: 'pending-request', recoverySecret: 'a'.repeat(32)}],
    },
  ]) {
    const raw = JSON.stringify(original);
    writeFileSync(path, raw, {mode: 0o600});
    assert.throws(() => updateInstallation(path), /path must be exactly \/api\/collector\/connect/);
    assert.equal(readFileSync(path, 'utf8'), raw, 'rejected state is never silently rewritten');
  }
  assert.throws(
    () => parseCollectorConfig({COLLECTOR_HUBS: JSON.stringify([{url: oldUrl, token: 'private-credential'}])}),
    /path must be exactly \/api\/collector\/connect/,
  );
});

test('sharing and cadence filters are independent and cannot mutate the shared record', () => {
  const sample = createSyntheticMetricsSampler({collectorId:'c',runId:'r',bootId:'b'}, {sequence: createSequenceAllocator(), now: () => 1_700_000_000_000, rand: () => 0.5}).next();
  const original = structuredClone(sample);
  const a = createMetricsFilter({url: 'unused', token: 'a', sharing: {cpu: false, memory: true}, intervalMs: 1000}, 100);
  const b = createMetricsFilter({url: 'unused', token: 'b', sharing: {cpu: true, memory: false}}, 100);
  assert.equal(a(sample, 0)!.payload.cpu.status, 'unavailable');
  assert.equal(b(sample, 0)!.payload.memory.status, 'unavailable');
  assert.equal(a(sample, 100), null);
  assert.equal(b(sample, 100)!.payload.cpu.status, 'ok');
  assert.equal(a(sample, 1000)!.payload.memory.status, 'ok');
  assert.deepEqual(sample, original);
});

test('destination policy rejects malformed booleans, excess fields, and invalid cadences', () => {
  for (const extra of [{sharing:{cpu:1,memory:true}}, {sharing:{cpu:true}}, {sharing:{cpu:true,memory:true,unknownField:true}}, {intervalMs:99}, {intervalMs:Infinity}]) {
    assert.throws(() => parseCollectorConfig({COLLECTOR_HUBS:JSON.stringify([{url:'ws://127.0.0.1:8081/api/collector/connect',token:'x',...extra}])}));
  }
});


test('collector state rejects stale locks and unsafe paths without replacing its identity', t => {
  const dir=mkdtempSync(join(tmpdir(),'rigscope-state-safety-'));t.after(()=>rmSync(dir,{recursive:true,force:true}));
  const path=join(dir,'state.json');const original=updateInstallation(path);
  writeFileSync(`${path}.lock`,'interrupted write',{mode:0o600});assert.throws(()=>updateInstallation(path),/locked/);rmSync(`${path}.lock`);
  chmodSync(path,0o644);assert.throws(()=>updateInstallation(path),/private regular file/);chmodSync(path,0o600);
  const link=join(dir,'alias.json');symlinkSync(path,link);assert.throws(()=>updateInstallation(link),/private regular file/);
  chmodSync(dir,0o755);assert.throws(()=>updateInstallation(path),/private directory/);chmodSync(dir,0o700);
  assert.equal(updateInstallation(path).collectorId,original.collectorId);
});


test('collector state recovers a proven dead lock owner but refuses live and ambiguous locks', t => {
  const dir=mkdtempSync(join(tmpdir(),'rigscope-state-lease-'));t.after(()=>rmSync(dir,{recursive:true,force:true}));
  const path=join(dir,'state.json');const lock=`${path}.lock`;
  const original=updateInstallation(path);

  writeFileSync(lock,JSON.stringify({pid:2147483647,instance:'dead-owner'}),{mode:0o600});
  const recovered=updateInstallation(path);
  assert.equal(recovered.collectorId,original.collectorId);
  assert.equal(existsSync(lock),false,'dead-owner lock is replaced and released');

  writeFileSync(lock,JSON.stringify({pid:process.pid,instance:'live-owner'}),{mode:0o600});
  assert.throws(()=>updateInstallation(path),/locked by another process/);
  assert.equal(readFileSync(path,'utf8').includes(original.collectorId),true,'live lock never mutates state');
  rmSync(lock);

  writeFileSync(lock,'interrupted write',{mode:0o600});
  assert.throws(()=>updateInstallation(path),/locked by an unsafe or incomplete/);
  assert.equal(readFileSync(path,'utf8').includes(original.collectorId),true,'ambiguous lock never mutates state');
  rmSync(lock);

  assert.equal(updateInstallation(path).collectorId,original.collectorId);
});
