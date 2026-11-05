import test from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  collectorHasDestinations, componentEnabled, configureComponent, connectUrl, enabledComponents, hubEnvironment,
  initialize, installationNeedsReset, pairColocatedCollector, readSetup, writeSetup,
} from '../src/cli/config';
import { configureAccess, planAccessChange } from '../src/cli/access';
import { parseCollectorConfig } from '../collector/config';
import { parseHubConfig } from '../src/hub/config';
import { hashPassword, verifyPassword } from '../src/hub/auth/password';
import { runInstallation } from '../src/cli/runtime';
import { acquireProcessLease } from '../src/runtime/process-lease';
import { databasePath } from '../src/cli/release-state';
import { openHubDatabase } from '../src/hub/storage/db';
import { CollectorRegistry } from '../src/hub/enrollment/registry';

test('local setup pairs a persistent collector and authenticated hub without storing the password', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-config-'));
  try {
    const first = initialize(dir, {port: '9123'});
    assert.ok(first.password);
    assert.equal(verifyPassword(first.password, first.config.users[0]!.passwordHash), true);
    const state = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal(state.collectorId, first.config.collectors[0]!.collectorId);
    assert.equal(state.destinations[0].token, first.config.collectors[0]!.token);
    assert.equal(state.destinations[0].sharing.processArguments, false);
    assert.equal(state.destinations[0].sharing.networkAddresses, false);
    assert.doesNotThrow(() => parseHubConfig(hubEnvironment(dir, first.config)));
    assert.equal(parseHubConfig(hubEnvironment(dir, {...first.config, maxCollectorConnections: 500})).maxCollectorConnections, 500);
    assert.equal(lstatSync(join(dir, 'config.json')).mode & 0o777, 0o600);
    assert.ok(!readFileSync(join(dir, 'config.json'), 'utf8').includes(first.password));
    const second = initialize(dir);
    assert.equal(second.password, undefined);
    assert.equal(second.created, false);
    assert.deepEqual(second.config, first.config);
    assert.throws(() => initialize(dir, {role: 'hub'}), /different settings/);
    assert.throws(() => initialize(dir, {port: '9124'}), /different settings/);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('remote hub setup uses only its private socket, and collector setup creates no browser credentials', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-remote-'));
  try {
    const hub = initialize(join(dir, 'hub'), {role: 'hub', origin: 'https://rig.example.com'});
    const env = hubEnvironment(join(dir, 'hub'), hub.config);
    assert.equal(env.HUB_HOST, undefined);
    assert.equal(env.HUB_PORT, undefined);
    assert.equal(parseHubConfig(env).remote?.publicOrigin, 'https://rig.example.com');
    assert.equal(hub.config.collectors.length, 0);
    const collector = initialize(join(dir, 'agent'), {role: 'collector'});
    assert.equal(collector.password, undefined);
    assert.equal(collector.config.users.length, 0);
    assert.throws(() => initialize(join(dir, 'invalid'), {role: 'hub', origin: 'http://remote.example.com'}), /https/i);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('private setup rejects loose permissions and URL normalization retains the TLS boundary', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-private-'));
  try {
    initialize(dir);
    chmodSync(join(dir, 'config.json'), 0o644);
    assert.throws(() => readSetup(dir), /private regular file/);
    assert.equal(connectUrl('https://rig.example.com'), 'wss://rig.example.com/api/collector/connect');
    assert.equal(connectUrl('http://localhost:8081'), 'ws://127.0.0.1:8081/api/collector/connect');
    for (const raw of ['http://remote.example.com', 'https://user:secret@rig.example.com', 'https://rig.example.com/?token=secret']) {
      assert.throws(() => parseCollectorConfig({COLLECTOR_HUBS: JSON.stringify([{url: connectUrl(raw), token: 'grant'}])}));
    }
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('a new colocated hub and collector can use HTTPS without a second installation or enrollment', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-remote-local-'));
  try {
    const setup = initialize(dir, {origin: 'https://rig.example.com:8081'});
    assert.equal(setup.config.role, 'local');
    assert.equal(readSetup(dir)!.origin, 'https://rig.example.com:8081');
    const env = hubEnvironment(dir, setup.config);
    assert.equal(env.HUB_HOST, undefined);
    assert.equal(parseHubConfig(env).remote?.hostHeader, 'rig.example.com:8081');
    const state = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal(state.destinations[0].url, 'wss://rig.example.com:8081/api/collector/connect');
    assert.equal(state.destinations[0].token, setup.config.collectors[0].token);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('a hub-first installation gains a colocated collector without creating a second owner', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-hub-first-'));
  try {
    const hub = initialize(dir, {role: 'hub', port: '9131'});
    assert.ok(hub.password);
    assert.deepEqual(enabledComponents(hub.config), ['hub']);
    assert.equal(componentEnabled(hub.config, 'collector'), false);
    assert.equal(existsSync(join(dir, 'collector', 'state.json')), false);

    const collector = configureComponent(dir, 'collector', {enabled: true});
    assert.equal(collector.password, undefined, 'the owner is created exactly once');
    assert.deepEqual(enabledComponents(collector.config), ['hub', 'collector']);
    assert.equal(collector.config.role, 'hub', 'role remains the initial preset metadata');
    assert.equal(collector.config.users.length, 1);
    assert.deepEqual(collector.config.users, hub.config.users);
    const state = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal(state.destinations.length, 1);
    assert.equal(state.destinations[0].url, 'ws://127.0.0.1:9131/api/collector/connect');
    assert.equal(state.destinations[0].token, collector.config.collectors[0].token);
    assert.equal(state.collectorId, collector.config.collectors[0].collectorId);

    const again = configureComponent(dir, 'collector', {enabled: true});
    assert.equal(again.password, undefined);
    assert.equal(again.config.users.length, 1);
    assert.equal(again.config.collectors.length, 1);
    assert.equal(JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8')).destinations.length, 1);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('a collector-first installation enables its hub later and pairs only on explicit collector setup', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-collector-first-'));
  try {
    const collector = initialize(dir, {role: 'collector', port: '9132'});
    assert.equal(collector.password, undefined);
    assert.deepEqual(enabledComponents(collector.config), ['collector']);
    assert.equal(existsSync(join(dir, 'collector', 'state.json')), false);

    const hub = configureComponent(dir, 'hub', {enabled: true});
    assert.ok(hub.password);
    assert.deepEqual(enabledComponents(hub.config), ['hub', 'collector']);
    assert.equal(hub.config.role, 'collector');
    assert.equal(existsSync(join(dir, 'collector', 'state.json')), false, 'enabling a hub does not enroll an existing collector');

    const paired = configureComponent(dir, 'collector', {enabled: true});
    assert.equal(paired.password, undefined);
    const state = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal(state.destinations.length, 1);
    assert.equal(state.destinations[0].url, 'ws://127.0.0.1:9132/api/collector/connect');
    assert.equal(paired.config.collectors[0].token, state.destinations[0].token);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('enabling a collector preserves existing remote destinations instead of pairing locally', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-remote-collector-'));
  try {
    initialize(dir, {role: 'hub', origin: 'https://rig.example.com'});
    const remote = {url: 'wss://other.example.com/api/collector/connect', token: 'remote-token'};
    const state = {collectorId: 'remote-collector', destinations: [remote]};
    writeFileSync(join(dir, 'collector', 'state.json'), `${JSON.stringify(state)}\n`, {mode: 0o600});
    const collector = configureComponent(dir, 'collector', {enabled: true});
    assert.deepEqual(collector.config.collectors, [], 'no colocated credential is invented for a remote collector');
    assert.deepEqual(JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8')).destinations, [remote]);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('pairing after the hub database exists uses the authoritative registry, not stale config', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-registry-pair-'));
  try {
    initialize(dir, {role: 'hub', port: '9142'});
    // Simulate a hub that has already started and created its private registry.
    const database = openHubDatabase(databasePath(dir));
    database.close();
    const enabled = configureComponent(dir, 'collector', {enabled: true});
    assert.deepEqual(enabled.config.collectors, [], 'no stale HUB_COLLECTORS credential is invented');
    assert.equal(collectorHasDestinations(dir), false);

    const paired = await pairColocatedCollector(dir);
    assert.deepEqual(paired.collectors, []);
    const state = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal(state.destinations.length, 1);
    const check = openHubDatabase(databasePath(dir));
    try {
      const registry = new CollectorRegistry({database: check, legacyIdentities: []});
      const identity = registry.identities().find(item => item.collectorId === state.collectorId);
      assert.ok(identity, 'the hub registry owns the new identity');
      assert.equal(identity.token, state.destinations[0].token);
    } finally { check.close(); }
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('live pairing creates a one-use grant and enrolls over the running hub HTTP endpoint', {timeout: 15000}, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-live-pair-'));
  let lease: (() => void) | undefined;
  const requests: Array<{authorization: string; collectorId: string}> = [];
  const server = createServer((req, res) => {
    let body = '';
    req.on('data', chunk => { body += chunk; });
    req.on('end', () => {
      const parsed = JSON.parse(body) as {collectorId: string};
      requests.push({authorization: req.headers.authorization ?? '', collectorId: parsed.collectorId});
      res.writeHead(201, {'Content-Type': 'application/json'});
      res.end(JSON.stringify({collectorId: parsed.collectorId, machineId: 'live-machine', token: 'live-token'}));
    });
  });
  try {
    await new Promise<void>(resolveListen => server.listen(0, '127.0.0.1', resolveListen));
    const address = server.address();
    assert.ok(address && typeof address === 'object');
    initialize(dir, {role: 'hub', port: String(address.port)});
    const database = openHubDatabase(databasePath(dir));
    database.close();
    lease = acquireProcessLease(`${databasePath(dir)}.hub.lock`);
    const paired = await pairColocatedCollector(dir);
    assert.deepEqual(paired.collectors, []);
    const state = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal(state.destinations.length, 1);
    assert.equal(state.destinations[0].token, 'live-token');
    assert.equal(requests.length, 1);
    assert.equal(requests[0]!.collectorId, state.collectorId);
    assert.match(requests[0]!.authorization, /^Bearer /);
    assert.ok(!requests[0]!.authorization.includes('live-token'));
  } finally {
    lease?.();
    await new Promise<void>(resolveClose => server.close(() => resolveClose()));
    rmSync(dir, {recursive: true, force: true});
  }
});

test('live pairing with an HTTPS proxy hub enrolls through its private Unix socket', {timeout: 15000}, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-socket-pair-'));
  let lease: (() => void) | undefined;
  const seen: Array<{host: string; path: string}> = [];
  const server = createServer((req, res) => {
    let body = '';
    req.on('data', chunk => { body += chunk; });
    req.on('end', () => {
      const parsed = JSON.parse(body) as {collectorId: string};
      seen.push({host: req.headers.host ?? '', path: req.url ?? ''});
      res.writeHead(201, {'Content-Type': 'application/json'});
      res.end(JSON.stringify({collectorId: parsed.collectorId, machineId: 'socket-machine', token: 'socket-token'}));
    });
  });
  try {
    const socket = join(dir, 'run', 'hub.sock');
    initialize(dir, {role: 'hub', origin: 'https://rig.example.com:8443'});
    await new Promise<void>(resolveListen => server.listen(socket, resolveListen));
    const database = openHubDatabase(databasePath(dir));
    database.close();
    lease = acquireProcessLease(`${databasePath(dir)}.hub.lock`);
    await pairColocatedCollector(dir);
    const state = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal(state.destinations.length, 1);
    assert.equal(state.destinations[0].url, 'wss://rig.example.com:8443/api/collector/connect');
    assert.equal(state.destinations[0].token, 'socket-token');
    assert.deepEqual(seen, [{host: 'rig.example.com:8443', path: '/api/collector/enroll'}]);
  } finally {
    lease?.();
    await new Promise<void>(resolveClose => server.close(() => resolveClose()));
    rmSync(dir, {recursive: true, force: true});
  }
});

test('disabled components retain credentials, destinations and files', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-disabled-'));
  try {
    const setup = initialize(dir, {port: '9133'});
    const stateBefore = readFileSync(join(dir, 'collector', 'state.json'), 'utf8');
    const disabledHub = configureComponent(dir, 'hub', {enabled: false});
    assert.equal(disabledHub.password, undefined);
    assert.equal(componentEnabled(disabledHub.config, 'hub'), false);
    assert.equal(componentEnabled(disabledHub.config, 'collector'), true);
    assert.deepEqual(disabledHub.config.users, setup.config.users);
    assert.deepEqual(disabledHub.config.collectors, setup.config.collectors);

    const disabledCollector = configureComponent(dir, 'collector', {enabled: false});
    assert.deepEqual(disabledCollector.config.users, setup.config.users);
    assert.deepEqual(disabledCollector.config.collectors, setup.config.collectors);
    assert.equal(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'), stateBefore);

    const reenabled = configureComponent(dir, 'hub', {enabled: true});
    assert.equal(reenabled.password, undefined, 're-enabling keeps the existing owner password hash');
    assert.deepEqual(reenabled.config.users, setup.config.users);
    assert.equal(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'), stateBefore);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('canonical configuration round-trips without a version key and keeps components', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-canonical-'));
  try {
    const setup = initialize(dir, {port: '9134'});
    const raw = JSON.parse(readFileSync(join(dir, 'config.json'), 'utf8'));
    assert.equal('version' in raw, false);
    assert.deepEqual(raw.components, {hub: {enabled: true}, collector: {enabled: true}});

    const config = readSetup(dir)!;
    assert.deepEqual(enabledComponents(config), ['hub', 'collector']);
    writeSetup(dir, config);
    const written = JSON.parse(readFileSync(join(dir, 'config.json'), 'utf8'));
    assert.equal('version' in written, false);
    assert.deepEqual(written.components, raw.components);
    assert.deepEqual(written.users, setup.config.users);
    assert.deepEqual(written.collectors, setup.config.collectors);
    assert.deepEqual(readSetup(dir), config);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('initialize refuses an older configuration shape and leaves its state untouched', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-refuse-legacy-'));
  try {
    const passwordHash = hashPassword('retained-password');
    const legacy = {version: 2, role: 'local', port: 9134,
      users: [{username: 'owner', passwordHash, machineIds: '*', admin: true}],
      collectors: [{collectorId: 'old-collector', machineId: 'old-machine', token: 'old-token'}],
      components: {hub: {enabled: true}, collector: {enabled: false}}};
    const raw = `${JSON.stringify(legacy, null, 2)}\n`;
    writeFileSync(join(dir, 'config.json'), raw, {mode: 0o600});
    mkdirSync(join(dir, 'collector'), {recursive: true, mode: 0o700});
    const collector = `${JSON.stringify({version: 2, collectorId: 'old-collector', destinations: []})}\n`;
    writeFileSync(join(dir, 'collector', 'state.json'), collector, {mode: 0o600});

    assert.equal(installationNeedsReset(dir), true);
    assert.throws(() => initialize(dir, {role: 'hub', port: '9135'}), /older configuration or state shape/);
    assert.equal(readFileSync(join(dir, 'config.json'), 'utf8'), raw, 'initialize never deletes the config');
    assert.equal(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'), collector, 'initialize never deletes collector state');
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('incompatible state is detected structurally without deleting the managed runtime', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-incompatible-detect-'));
  try {
    mkdirSync(join(dir, 'collector'), {recursive: true, mode: 0o700});
    mkdirSync(join(dir, 'hub'), {recursive: true, mode: 0o700});
    mkdirSync(join(dir, 'backups'), {recursive: true, mode: 0o700});
    mkdirSync(join(dir, 'runtime'), {recursive: true, mode: 0o700});
    writeFileSync(join(dir, 'config.json'), `${JSON.stringify({version: 2, role: 'local', port: 9134})}\n`, {mode: 0o600});
    writeFileSync(join(dir, 'collector', 'state.json'), `${JSON.stringify({version: 2, collectorId: 'old', destinations: []})}\n`, {mode: 0o600});
    writeFileSync(join(dir, 'release-state.json'), `${JSON.stringify({formatVersion: 1, stateDir: dir})}\n`, {mode: 0o600});
    writeFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'old database\n', {mode: 0o600});

    assert.equal(installationNeedsReset(dir), true);
    assert.throws(() => initialize(dir), /older configuration or state shape/);
    for (const path of ['config.json', 'release-state.json', join('collector', 'state.json'), 'hub', 'backups', 'runtime']) {
      assert.equal(existsSync(join(dir, path)), true, `${path} is retained until a coordinated reset`);
    }
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('obsolete collector endpoints and enrollment state require coordinated reset', () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-wire-reset-'));
  try {
    for (const fixture of ['destination', 'pending', 'enrollment']) {
      const dir = join(root, fixture);
      initialize(dir);
      if (fixture === 'enrollment') {
        writeFileSync(join(dir, 'hub', 'enrollment.json'), JSON.stringify({version: 1, enrollments: [], consumed: []}), {mode: 0o600});
      } else {
        const path = join(dir, 'collector', 'state.json');
        const state = JSON.parse(readFileSync(path, 'utf8'));
        const url = 'ws://127.0.0.1:8081/api/collector/v1/connect';
        if (fixture === 'destination') state.destinations[0].url = url;
        else state.pendingEnrollments = [{url, requestId: 'pending', recoverySecret: 'a'.repeat(32)}];
        writeFileSync(path, JSON.stringify(state), {mode: 0o600});
      }
      const before = readFileSync(join(dir, 'config.json'), 'utf8');
      if (fixture === 'enrollment') {
        assert.equal(installationNeedsReset(dir), true, fixture);
        assert.throws(() => initialize(dir), /older configuration or state shape/);
      } else {
        assert.throws(() => installationNeedsReset(dir), /Invalid saved state/);
        assert.throws(() => initialize(dir), /Invalid saved state/);
      }
      assert.equal(readFileSync(join(dir, 'config.json'), 'utf8'), before);
    }
  } finally { rmSync(root, {recursive: true, force: true}); }
});

test('malformed auxiliary state cannot trigger deletion of canonical history', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-corrupt-state-'));
  try {
    initialize(dir);
    writeFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'retained history', {mode: 0o600});
    for (const raw of ['{truncated', 'null', '{}']) {
      writeFileSync(join(dir, 'release-state.json'), raw, {mode: 0o600});
      assert.throws(() => installationNeedsReset(dir), /Invalid saved state/);
      assert.equal(readFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'utf8'), 'retained history');
    }
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('a canonical configuration and canonical release state need no reset', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-canonical-detect-'));
  try {
    const setup = initialize(dir, {port: '9134'});
    writeFileSync(join(dir, 'release-state.json'), `${JSON.stringify({
      stateDir: dir, currentVersion: '1.0.0', previousVersion: null, backups: {}, lastActivation: null, lastRollback: null,
    })}\n`, {mode: 0o600});
    const before = readFileSync(join(dir, 'config.json'), 'utf8');
    assert.equal(installationNeedsReset(dir), false);
    const again = initialize(dir);
    assert.equal(again.created, false);
    assert.deepEqual(again.config, setup.config);
    assert.equal(readFileSync(join(dir, 'config.json'), 'utf8'), before, 'canonical reinstall retains settings and data');
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('a port-only access change updates local config and the colocated collector destination', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-port-change-'));
  try {
    const setup = initialize(dir, {port: '9137'});
    const statePath = join(dir, 'collector', 'state.json');
    const before = JSON.parse(readFileSync(statePath, 'utf8'));
    assert.equal(before.destinations[0].url, 'ws://127.0.0.1:9137/api/collector/connect');

    const plan = planAccessChange(dir, {port: '9138'});
    assert.equal(plan.next.port, 9138);
    assert.equal(plan.newEndpoint, 'ws://127.0.0.1:9138/api/collector/connect');
    assert.equal(plan.collectorChanged, true);
    assert.equal(plan.changed, true);

    const result = configureAccess(dir, {port: '9138'});
    assert.equal(result.changed, true);
    assert.equal(readSetup(dir)!.port, 9138);
    const state = JSON.parse(readFileSync(statePath, 'utf8'));
    assert.equal(state.destinations[0].url, 'ws://127.0.0.1:9138/api/collector/connect');
    assert.equal(state.destinations[0].token, before.destinations[0].token);
    assert.equal(state.collectorId, before.collectorId);
    assert.deepEqual(readSetup(dir)!.collectors, setup.config.collectors);

    assert.throws(() => planAccessChange(dir, {origin: 'https://rig.example.com', port: '9139'}),
      /--port is unused with --origin/);
  } finally { rmSync(dir, {recursive: true, force: true}); }
});

test('an unconfigured collector exits cleanly while a selected hub still runs', async () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-awaiting-'));
  try {
    const dist = join(root, 'dist');
    const marker = (name: string) => JSON.stringify(join(root, `${name}.started`));
    mkdirSync(dist, {recursive: true});
    writeFileSync(join(dist, 'hub.cjs'), `require('node:fs').writeFileSync(${marker('hub')}, 'hub');`);
    writeFileSync(join(dist, 'collector.cjs'), `require('node:fs').writeFileSync(${marker('collector')}, 'collector'); process.exit(1);`);

    const explicit = join(root, 'explicit');
    initialize(explicit, {role: 'collector'});
    assert.equal(await runInstallation(root, explicit, 'collector'), 0);
    assert.equal(existsSync(join(root, 'collector.started')), false);
    assert.equal(existsSync(join(explicit, 'run', 'collector.lock')), false);

    const combined = join(root, 'combined');
    initialize(combined, {role: 'hub'});
    configureComponent(combined, 'collector', {enabled: true, pairColocated: false});
    assert.equal(collectorHasDestinations(combined), false);
    // The fixture hub exits immediately, so the combined supervisor reports
    // that child exit; the point is that the collector was never spawned.
    await runInstallation(root, combined);
    assert.equal(existsSync(join(root, 'hub.started')), true, 'the enabled hub still runs');
    assert.equal(existsSync(join(root, 'collector.started')), false, 'the unconfigured collector is skipped');
  } finally { rmSync(root, {recursive: true, force: true}); }
});

test('component leases isolate hub and collector and a failed component runtime does not launch the other', async () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-runtime-'));
  const dir = join(root, 'state');
  try {
    initialize(dir, {port: '9135'});
    const dist = join(root, 'dist');
    const marker = (name: string) => JSON.stringify(join(root, `${name}.started`));
    mkdirSync(dist, {recursive: true});
    writeFileSync(join(dist, 'hub.cjs'), `require('node:fs').writeFileSync(${marker('hub')}, 'hub');`);
    writeFileSync(join(dist, 'collector.cjs'), `require('node:fs').writeFileSync(${marker('collector')}, 'collector'); process.exit(3);`);

    // Holding the hub supervisor lease must not block the collector.
    const hubLease = acquireProcessLease(join(dir, 'run', 'hub.lock'));
    try {
      assert.equal(await runInstallation(root, dir, 'collector'), 3);
    } finally { hubLease(); }
    assert.equal(existsSync(join(root, 'collector.started')), true);
    assert.equal(existsSync(join(root, 'hub.started')), false, 'an explicit collector run never launches the hub');
    assert.equal(existsSync(join(dir, 'run', 'collector.lock')), false, 'the failed run releases its component lease');
  } finally { rmSync(root, {recursive: true, force: true}); }
});
