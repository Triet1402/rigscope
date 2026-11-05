import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { initialize, readSetup, writeSetup } from '../src/cli/config';
import { configureAccess, planAccessChange } from '../src/cli/access';
import { acquireProcessLease } from '../src/runtime/process-lease';
import { outboxDirFor } from '../collector/destinations/outbox';

const cli = join(process.cwd(), 'bin', 'rigscope.cjs');
test('retained local installation converts to HTTPS without reinstalling, changing identity, or losing history', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-configure-'));
  try {
    const setup = initialize(dir);
    const collectorBefore = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    const beforeConfig = readFileSync(join(dir, 'config.json'), 'utf8');
    // The exact failed reinstall from the user's screenshots must explain the recovery path.
    const reinstall = spawnSync(process.execPath, [cli, 'install', '--role', 'hub', '--origin', 'https://rig.example.com:8081', '--state-dir', dir], {encoding: 'utf8', timeout: 5000});
    assert.equal(reinstall.status, 1);
    assert.match(reinstall.stderr, /To change access while preserving data/);
    assert.match(reinstall.stderr, /rigscope configure --origin/);
    assert.equal(readFileSync(join(dir, 'config.json'), 'utf8'), beforeConfig);
    const history = join(dir, 'hub', 'keep-history');
    writeFileSync(history, 'retained history');
    const output = execFileSync(process.execPath, [cli, 'configure', '--origin', 'https://rig.example.com:8081', '--state-dir', dir], {encoding: 'utf8', timeout: 5000});
    const configured = readSetup(dir)!;
    assert.equal(configured.origin, 'https://rig.example.com:8081');
    assert.equal(configured.role, 'local');
    assert.deepEqual(configured.users, setup.config.users);
    assert.deepEqual(configured.collectors, setup.config.collectors);
    assert.equal(readFileSync(history, 'utf8'), 'retained history');
    const collectorAfter = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal(collectorAfter.collectorId, collectorBefore.collectorId);
    assert.equal(collectorAfter.destinations[0].url, 'wss://rig.example.com:8081/api/collector/connect');
    assert.equal(collectorAfter.destinations[0].token, collectorBefore.destinations[0].token);
    assert.deepEqual(collectorAfter.destinations[0].sharing, collectorBefore.destinations[0].sharing);
    assert.ok(!output.includes(setup.password!));
    assert.ok(!output.includes(collectorBefore.destinations[0].token));
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('access conversion carries pending outbox files, leaves other hubs alone, and can return to localhost', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-access-'));
  try {
    const setup = initialize(dir);
    const statePath = join(dir, 'collector', 'state.json');
    const state = JSON.parse(readFileSync(statePath, 'utf8'));
    const other = {...state.destinations[0], url: 'wss://other.example.com/api/collector/connect'};
    state.destinations.push(other);
    writeFileSync(statePath, JSON.stringify(state));
    const oldQueue = outboxDirFor(join(dir, 'collector'), state.destinations[0].url);
    mkdirSync(oldQueue, {recursive: true, mode: 0o700});
    writeFileSync(join(oldQueue, 'pending.sqlite-wal'), 'pending samples');
    const result = configureAccess(dir, {origin: 'https://rig.example.com'});
    assert.ok(result.backup);
    assert.equal(JSON.parse(readFileSync(join(result.backup!, 'config.json'), 'utf8')).users[0].passwordHash, setup.config.users[0].passwordHash);
    const newQueue = outboxDirFor(join(dir, 'collector'), 'wss://rig.example.com/api/collector/connect');
    assert.equal(existsSync(oldQueue), false);
    assert.equal(readFileSync(join(newQueue, 'pending.sqlite-wal'), 'utf8'), 'pending samples');
    assert.deepEqual(JSON.parse(readFileSync(statePath, 'utf8')).destinations[1], other);
    assert.equal(configureAccess(dir, {origin: 'https://rig.example.com'}).changed, false);
    configureAccess(dir, {local: true});
    assert.equal(readSetup(dir)!.origin, undefined);
    assert.equal(readFileSync(join(oldQueue, 'pending.sqlite-wal'), 'utf8'), 'pending samples');
    assert.equal(JSON.parse(readFileSync(statePath, 'utf8')).destinations[0].url, state.destinations[0].url);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('access changes flag the collector only when its colocated destination actually moves', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-access-plan-'));
  try {
    const hubOnly = join(dir, 'hub');
    initialize(hubOnly, {role: 'hub'});
    const hubPlan = planAccessChange(hubOnly, {origin: 'https://rig.example.com'});
    assert.equal(hubPlan.changed, true);
    assert.equal(hubPlan.collectorChanged, false, 'a hub-only installation has no collector to stop');

    const shared = join(dir, 'shared');
    initialize(shared);
    assert.equal(planAccessChange(shared, {origin: 'https://rig.example.com'}).collectorChanged, true);
    // A collector enrolled only to a remote hub must not be disturbed by a local access change.
    const statePath = join(shared, 'collector', 'state.json');
    const state = JSON.parse(readFileSync(statePath, 'utf8'));
    const remote = {url: 'wss://remote.example.com/api/collector/connect', token: 'remote-token'};
    writeFileSync(statePath, JSON.stringify({...state, destinations: [remote]}));
    const remotePlan = planAccessChange(shared, {origin: 'https://rig.example.com'});
    assert.equal(remotePlan.collectorChanged, false);
    configureAccess(shared, {origin: 'https://rig.example.com'});
    assert.equal(readSetup(shared)!.origin, 'https://rig.example.com');
    assert.deepEqual(JSON.parse(readFileSync(statePath, 'utf8')).destinations, [remote]);

    // A registry-enrolled collector has no HUB_COLLECTORS entry; the local
    // destination is still identified by its canonical hub endpoint.
    const registry = join(dir, 'registry');
    initialize(registry);
    writeSetup(registry, {...readSetup(registry)!, collectors: []});
    assert.equal(planAccessChange(registry, {origin: 'https://rig.example.com'}).collectorChanged, true);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('invalid URLs, conflicting queues and a live runtime refuse conversion without changing saved settings', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-access-refusal-'));
  try {
    initialize(dir);
    const before = readFileSync(join(dir, 'config.json'), 'utf8');
    const stateBefore = readFileSync(join(dir, 'collector', 'state.json'), 'utf8');
    assert.throws(() => configureAccess(dir, {origin: 'http://remote.example.com'}), /https/i);
    const target = outboxDirFor(join(dir, 'collector'), 'wss://rig.example.com/api/collector/connect');
    mkdirSync(target, {recursive: true, mode: 0o700});
    assert.throws(() => configureAccess(dir, {origin: 'https://rig.example.com'}), /already has an outbox/);
    const release = acquireProcessLease(join(dir, 'run', 'rigscope.lock'));
    try {
      const result = spawnSync(process.execPath, [cli, 'configure', '--origin', 'https://rig.example.com', '--state-dir', dir], {encoding: 'utf8', timeout: 5000});
      assert.equal(result.status, 1);
      assert.match(result.stderr, /refusing to signal an unrelated process/);
    } finally { release(); }
    assert.equal(readFileSync(join(dir, 'config.json'), 'utf8'), before);
    assert.equal(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'), stateBefore);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});
