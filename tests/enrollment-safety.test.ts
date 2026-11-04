import test from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { EnrollmentStore } from '../src/hub/enrollment/store';

test('enrollment rejects unsafe storage, long-lived grants and mismatched persisted claims', t => {
  const dir=mkdtempSync(join(tmpdir(),'rigscope-enroll-safe-'));t.after(()=>rmSync(dir,{recursive:true,force:true}));
  const path=join(dir,'state.json');
  const now=()=>new Date('2026-11-02T00:00:00.000Z');
  const grants=[{token:'test-grant',expiresAt:'2026-11-02T00:10:00.000Z'}];
  const open=()=>new EnrollmentStore({path,identities:[],grants,now});
  assert.throws(()=>new EnrollmentStore({path,identities:[],grants:[{token:'x',expiresAt:'2030-01-01T00:00:00.000Z'}],now}),/fifteen minutes/);
  open().enroll({collectorId:'c',token:'test-grant'});
  const raw=readFileSync(path,'utf8');
  chmodSync(path,0o644);assert.throws(open,/private/);chmodSync(path,0o600);
  const target=join(dir,'target.json');writeFileSync(target,raw,{mode:0o600});rmSync(path);symlinkSync(target,path);assert.throws(open,/regular file/);rmSync(path);
  const state=JSON.parse(raw);state.consumed=[];writeFileSync(path,JSON.stringify(state),{mode:0o600});assert.throws(open,/claims/);
});

test('two enrollment store instances cannot reuse a grant or lose a mapping', t => {
  const dir=mkdtempSync(join(tmpdir(),'rigscope-enroll-writers-'));t.after(()=>rmSync(dir,{recursive:true,force:true}));
  const path=join(dir,'state.json');const now=()=>new Date('2026-11-02T00:00:00.000Z');
  const grants=['a','b'].map(token=>({token,expiresAt:'2026-11-02T00:10:00.000Z'}));
  const open=()=>new EnrollmentStore({path,identities:[],grants,now});
  const a=open();const b=open();
  a.enroll({collectorId:'collector-a',token:'a'});
  assert.throws(()=>b.enroll({collectorId:'intruder',token:'a'}),/already been claimed/);
  b.enroll({collectorId:'collector-b',token:'b'});
  assert.equal(open().identities().length,2);
  // A restored deployment may combine many static identities with persisted
  // enrollments; the registry is byte-bounded, not machine-count bounded.
  const expanded=new EnrollmentStore({path,identities:Array.from({length:60},(_,i)=>({collectorId:`s${i}`,machineId:`m${i}`,token:`t${i}`})),grants,now});
  assert.equal(expanded.identities().length,62);
});


test('failed enrollment state reload preserves the last validated registry', t => {
  const dir=mkdtempSync(join(tmpdir(),'rigscope-enroll-reload-'));t.after(()=>rmSync(dir,{recursive:true,force:true}));
  const path=join(dir,'state.json');const now=()=>new Date('2026-11-02T00:00:00.000Z');
  const grants=['a','b','c'].map(token=>({token,expiresAt:'2026-11-02T00:10:00.000Z'}));
  const store=new EnrollmentStore({path,identities:[],grants,now});
  store.enroll({collectorId:'one',token:'a'});store.enroll({collectorId:'two',token:'b'});
  const previous=store.identities();const good=readFileSync(path,'utf8');const bad=JSON.parse(good);
  bad.enrollments[1].collectorId=bad.enrollments[0].collectorId;writeFileSync(path,JSON.stringify(bad));
  assert.throws(()=>store.enroll({collectorId:'three',token:'c'}),/duplicate collector/);
  assert.deepEqual(store.identities(),previous);
  writeFileSync(path,good);store.enroll({collectorId:'three',token:'c'});
  assert.equal(store.identities().length,3);
});


test('enrollment refuses stale locks and rejects grants that cannot travel in bearer headers', t => {
  const dir=mkdtempSync(join(tmpdir(),'rigscope-enroll-lock-'));t.after(()=>rmSync(dir,{recursive:true,force:true}));
  const path=join(dir,'state.json');const now=()=>new Date('2026-11-02T00:00:00.000Z');
  const expiresAt='2026-11-02T00:10:00.000Z';
  for (const token of ['has spaces','has\ttab','has\nnewline']) {
    assert.throws(()=>new EnrollmentStore({path,identities:[],grants:[{token,expiresAt}],now}),/token must/);
  }
  const store=new EnrollmentStore({path,identities:[],grants:[{token:'valid-grant',expiresAt}],now});
  writeFileSync(`${path}.lock`,'interrupted write',{mode:0o600});
  assert.throws(()=>store.enroll({collectorId:'c',token:'valid-grant'}),/locked/);
  assert.equal(store.enrolledCount(),0);rmSync(`${path}.lock`);
  assert.equal(store.enroll({collectorId:'c',token:'valid-grant'}).collectorId,'c');
});


test('enrollment recovers a proven dead lock owner but refuses live and ambiguous locks', t => {
  const dir=mkdtempSync(join(tmpdir(),'rigscope-enroll-lease-'));t.after(()=>rmSync(dir,{recursive:true,force:true}));
  const path=join(dir,'state.json');const lock=`${path}.lock`;
  const now=()=>new Date('2026-11-02T00:00:00.000Z');
  const grants=['a','b','c'].map(token=>({token,expiresAt:'2026-11-02T00:10:00.000Z'}));
  const store=new EnrollmentStore({path,identities:[],grants,now});

  writeFileSync(lock,JSON.stringify({pid:2147483647,instance:'dead-owner'}),{mode:0o600});
  const recovered=store.enroll({collectorId:'recovered',token:'a'});
  assert.equal(recovered.collectorId,'recovered');
  assert.equal(existsSync(lock),false,'dead-owner lock is replaced and released');

  writeFileSync(lock,JSON.stringify({pid:process.pid,instance:'live-owner'}),{mode:0o600});
  assert.throws(()=>store.enroll({collectorId:'live',token:'b'}),/locked by another process/);
  assert.equal(store.enrolledCount(),1,'live lock never mutates the registry');
  rmSync(lock);

  writeFileSync(lock,'interrupted write',{mode:0o600});
  assert.throws(()=>store.enroll({collectorId:'ambiguous',token:'b'}),/locked by an unsafe or incomplete/);
  assert.equal(store.enrolledCount(),1,'ambiguous lock never mutates the registry');
  rmSync(lock);

  assert.equal(store.enroll({collectorId:'after-recovery',token:'b'}).collectorId,'after-recovery');
  assert.equal(store.enrolledCount(),2);
});
