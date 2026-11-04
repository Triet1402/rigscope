/**
 * CLI operations fixtures: doctor, staged upgrade/rollback pairing and
 * offline admin recovery. No service is started, no live installation is
 * touched, and every temporary database is isolated.
 */
import assert from 'node:assert/strict';
import test from 'node:test';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer } from 'node:http';
import { DatabaseSync } from 'node:sqlite';

import { initialize } from '../src/cli/config';
import { diagnoseInstallation } from '../src/cli/doctor';
import { waitForHealth, type HealthTarget } from '../src/cli/health';
import {
  emptyRuntimeState,
  inspectDatabaseWithRuntime,
  readRuntimeState,
  runtimeVersion,
  writeRuntimeState,
} from '../src/cli/release-state';
import type { ServiceStatus } from '../src/cli/services';
import {
  rollbackInstallation,
  upgradeInstallation,
  type UpgradeDependencies,
} from '../src/cli/upgrade';
import { hashPassword, verifyPassword } from '../src/hub/auth/password';
import { MIGRATIONS } from '../src/hub/storage/migrations';
import { SUPPORTED_SCHEMA_VERSION, inspectDatabase, schemaVersionOf } from '../src/operations/database';
import { recoverAdminPassword } from '../src/operations/recovery';
import { openHubDatabase } from '../src/hub/storage/db';
import { acquireProcessLease } from '../src/runtime/process-lease';

const ROOT = mkdtempSync(join(tmpdir(), 'rigscope-cli-ops-'));
process.on('exit', () => rmSync(ROOT, { recursive: true, force: true }));

function fresh(name: string): string {
  const path = join(ROOT, `${name}-${Math.random().toString(36).slice(2)}`);
  mkdirSync(path, { recursive: true, mode: 0o700 });
  chmodSync(path, 0o700);
  return path;
}

function writeRuntime(runtimeDir: string, version: string): void {
  const app = join(runtimeDir, 'node_modules', 'rigscope');
  for (const directory of ['dist', '.next']) mkdirSync(join(app, directory), { recursive: true, mode: 0o755 });
  writeFileSync(join(app, 'package.json'), `${JSON.stringify({ name: 'rigscope', version, rigscopeConfigVersion: 2 })}\n`);
  for (const file of ['dist/cli.cjs', 'dist/hub.cjs', 'dist/collector.cjs', 'dist/ops.cjs', '.next/BUILD_ID']) {
    writeFileSync(join(app, file), 'fixture\n');
  }
}

function seedDatabase(path: string, version: number): void {
  mkdirSync(join(path, '..'), { recursive: true, mode: 0o700 });
  const db = new DatabaseSync(path);
  try {
    db.exec(`CREATE TABLE schema_migrations (
      version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at_ms INTEGER NOT NULL
    )`);
    const insert = db.prepare('INSERT INTO schema_migrations VALUES (?, ?, ?)');
    for (const migration of MIGRATIONS.filter((item) => item.version <= version)) {
      migration.up(db);
      insert.run(migration.version, migration.name, 1_000 + migration.version);
    }
  } finally {
    db.close();
  }
  chmodSync(path, 0o600);
}

type FakeService = {
  status(dir: string, component?: 'hub' | 'collector'): ServiceStatus;
  stop(dir: string, component?: 'hub' | 'collector'): void;
  start(dir: string, component?: 'hub' | 'collector'): void;
};

function fakeService(initial: boolean | Partial<Record<'hub' | 'collector', boolean>>): FakeService & { active: boolean } {
  const states: Record<'hub' | 'collector', {active: boolean; enabled: boolean}> = {
    hub: {active: typeof initial === 'boolean' ? initial : initial.hub ?? false, enabled: true},
    collector: {active: typeof initial === 'boolean' ? initial : initial.collector ?? false, enabled: true},
  };
  const leases: Partial<Record<'hub' | 'collector', Array<() => void>>> = {};
  const controller = {
    get active(): boolean { return states.hub.active || states.collector.active; },
    status: (dir: string, component?: 'hub' | 'collector'): ServiceStatus => {
      const target = component ?? 'hub';
      const state = states[target];
      return {
        unit: `fixture-${target}-${dir.length}.service`,
        installed: true,
        active: state.active,
        enabled: state.enabled,
        subState: state.active ? 'running' : 'dead',
        execMainPid: state.active ? 100 : null,
        error: null,
        component: target,
      };
    },
    stop: (dir: string, component?: 'hub' | 'collector'): void => {
      for (const target of component ? [component] : ['hub', 'collector'] as const) {
        states[target].active = false;
        for (const release of leases[target]?.splice(0) ?? []) release();
      }
    },
    start: (dir: string, component?: 'hub' | 'collector'): void => {
      const target = component ?? 'hub';
      // Behave like real processes: starting while the installer retains these
      // leases must fail, and recovery must stop us before restoring files.
      const acquired = [acquireProcessLease(join(dir, 'run', `${target}.lock`))];
      try {
        if (target === 'collector') {
          acquired.push(acquireProcessLease(join(dir, 'collector', 'collector-runtime.lock')));
          writeFileSync(join(dir, 'run', 'collector-health.json'), JSON.stringify({
            pid: process.pid, sampledAt: Date.now(), version: runtimeVersion(join(dir, 'runtime')),
          }), {mode: 0o600});
        }
      } catch (error) {
        for (const release of acquired.reverse()) release();
        throw error;
      }
      leases[target] = acquired;
      states[target].active = true;
    },
    startup: (_dir: string, component?: 'hub' | 'collector'): boolean => states[component ?? 'hub'].enabled,
    setStartup: (_dir: string, enabled: boolean, component?: 'hub' | 'collector'): void => {
      states[component ?? 'hub'].enabled = enabled;
    },
  };
  return controller;
}

function runtimeInspector(supports: (runtimeDir: string) => number): typeof inspectDatabaseWithRuntime {
  return (runtimeDir: string, database: string) => {
    const actual = inspectDatabase(database);
    const supportedSchemaVersion = supports(runtimeDir);
    return {
      supportedSchemaVersion,
      schemaVersion: actual.schemaVersion,
      integrityOk: actual.integrityOk,
      newerSchema: actual.schemaVersion > supportedSchemaVersion,
      ledgerProblem: actual.ledgerProblem,
    };
  };
}

function healthOk(): Awaited<ReturnType<typeof waitForHealth>> {
  return {
    ok: true,
    attempts: 1,
    elapsedMs: 1,
    last: { ok: true, statusCode: 200, body: { status: 'ok' }, error: null },
    version: null,
  };
}

test('doctor reports role-appropriate health without leaking credentials', async () => {
  const dir = join(fresh('doctor'), 'local');
  const setup = initialize(dir, { port: '19191' });
  assert.ok(setup.password);
  writeRuntime(join(dir, 'runtime'), '2.3.4');
  seedDatabase(join(dir, 'hub', 'telemetry.sqlite'), MIGRATIONS.length === 0 ? 0 : MIGRATIONS[MIGRATIONS.length - 1]!.version);

  const report = await diagnoseInstallation(dir, {
    serviceStatus: () => ({
      unit: 'fixture.service', installed: true, active: true, subState: 'running', execMainPid: 42, error: null,
    }),
    probeHealth: async () => ({ ok: true, statusCode: 200, body: { status: 'ok', version: '2.3.4' }, error: null }),
    probeTcp: async () => ({ ok: true, error: null, ms: 1 }),
    probeTls: async () => ({ ok: true, error: null, ms: 1 }),
    inspectRuntime: runtimeInspector(() => MIGRATIONS[MIGRATIONS.length - 1]!.version),
    diskFree: () => ({ free: 8 * 1024 * 1024 * 1024, total: 64 * 1024 * 1024 * 1024 }),
    sensorReadable: (paths) => paths,
  });
  assert.equal(report.ok, true, JSON.stringify(report.checks));
  assert.equal(report.version, '2.3.4');
  assert.ok(report.checks.some((check) => check.id === 'health' && check.status === 'ok'));
  assert.ok(report.checks.some((check) => check.id === 'database' && check.status === 'ok'));
  const serialized = JSON.stringify(report);
  assert.equal(serialized.includes(setup.password!), false);
  assert.equal(serialized.includes(setup.config.users[0]!.passwordHash), false);
  assert.equal(serialized.includes(setup.config.collectors[0]!.token), false);

  const hubDir = join(fresh('doctor-hub'), 'hub');
  initialize(hubDir, { role: 'hub', origin: 'https://hub.example.com' });
  writeRuntime(join(hubDir, 'runtime'), '2.3.4');
  seedDatabase(join(hubDir, 'hub', 'telemetry.sqlite'), MIGRATIONS[MIGRATIONS.length - 1]!.version);
  const hub = await diagnoseInstallation(hubDir, {
    serviceStatus: () => ({ unit: 'hub.service', installed: true, active: false, subState: 'dead', execMainPid: null, error: null }),
    inspectRuntime: runtimeInspector(() => MIGRATIONS[MIGRATIONS.length - 1]!.version),
    diskFree: () => ({ free: 8 * 1024 * 1024 * 1024, total: 64 * 1024 * 1024 * 1024 }),
    probeTcp: async () => ({ ok: false, error: 'not listening', ms: 1 }),
    probeHealth: async () => ({ ok: false, statusCode: null, body: null, error: 'not probed' }),
    probeTls: async () => ({ ok: false, error: 'not probed', ms: 0 }),
    sensorReadable: () => [],
  });
  assert.equal(hub.checks.some((check) => check.id === 'collector'), false, 'hub role does not require a collector');
  assert.equal(hub.checks.some((check) => check.id === 'sensors'), false, 'hub role does not read sensors');
});

test('bounded health wait rejects a version mismatch and accepts the expected version', async () => {
  let advertisedVersion: string | undefined = '2.0.0';
  const server = createServer((req, res) => {
    if (req.url !== '/api/health') {
      res.writeHead(404).end();
      return;
    }
    const version = advertisedVersion;
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ status: 'ok', version }));
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  assert.ok(address && typeof address === 'object');
  const target: HealthTarget = { kind: 'loopback', host: '127.0.0.1', port: address.port, path: '/api/health' };
  const mismatch = await waitForHealth(target, { deadlineMs: 150, intervalMs: 25, expectedVersion: 'wrong' });
  assert.equal(mismatch.ok, false);
  assert.match(mismatch.last.error ?? '', /does not match expected/);
  const match = await waitForHealth(target, { deadlineMs: 1_000, intervalMs: 25, expectedVersion: '2.0.0' });
  assert.equal(match.ok, true);
  assert.equal(match.version, '2.0.0');
  advertisedVersion = undefined;
  const missing = await waitForHealth(target, {deadlineMs: 150, intervalMs: 25, expectedVersion: '2.0.0'});
  assert.equal(missing.ok, false, 'an unversioned generic success cannot accept an upgrade');
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

test('health deadlines remain bounded when a server dribbles response bytes', async () => {
  const server = createServer((_req, res) => {
    res.writeHead(200, {'Content-Type':'application/json'});
    const timer = setInterval(() => res.write(' '), 20);
    res.once('close', () => clearInterval(timer));
  });
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
  try {
    const address = server.address();
    assert.ok(address && typeof address === 'object');
    const start = Date.now();
    const result = await waitForHealth({kind:'loopback',host:'127.0.0.1',port:address.port,path:'/api/health'},
      {deadlineMs:150, intervalMs:25, probeTimeoutMs:100});
    assert.equal(result.ok, false);
    assert.ok(Date.now() - start < 750, 'traffic must not renew the total deadline');
  } finally { await new Promise<void>(resolve => server.close(() => resolve())); }
});

test('an inactive installation activates a staged upgrade without fabricating live health', async () => {
  const root = fresh('inactive-upgrade');
  const dir = join(root, 'state');
  initialize(dir, { port: '19196' });
  writeRuntime(join(dir, 'runtime'), '1.0.0');
  writeRuntimeState(dir, { ...emptyRuntimeState(dir), currentVersion: '1.0.0' });
  const service = fakeService(false);
  let healthCalls = 0;
  const deps: UpgradeDependencies = {
    service,
    stage: () => {
      const stage = join(root, 'stage-inactive');
      writeRuntime(stage, '2.0.0');
      return stage;
    },
    inspectRuntime: runtimeInspector(() => SUPPORTED_SCHEMA_VERSION),
    waitHealth: async () => {
      healthCalls += 1;
      return healthOk();
    },
  };
  const report = await upgradeInstallation({ root, dir, from: 'fixture@2.0.0' }, deps);
  assert.equal(report.version, '2.0.0');
  assert.equal(report.verification.health, null);
  assert.equal(healthCalls, 0);
  assert.equal(service.active, false, 'upgrade does not start an inactive installation');
  assert.equal(existsSync(join(dir, 'hub', 'telemetry.sqlite')), false);
});

test('schema-changing upgrade pairs a backup and rollback restores it after preserving the newer database', async () => {
  const root = fresh('upgrade');
  const dir = join(root, 'state');
  initialize(dir, { port: '19192' });
  writeRuntime(join(dir, 'runtime'), '1.0.0');
  writeRuntimeState(dir, { ...emptyRuntimeState(dir), currentVersion: '1.0.0' });
  const database = join(dir, 'hub', 'telemetry.sqlite');
  seedDatabase(database, 3);

  const service = fakeService(true);
  let stagePath = '';
  const deps: UpgradeDependencies = {
    service,
    stage: () => {
      stagePath = join(root, 'stage-2');
      writeRuntime(stagePath, '2.0.0');
      return stagePath;
    },
    inspectRuntime: runtimeInspector((runtimeDir) => (runtimeVersion(runtimeDir) === '1.0.0' ? SUPPORTED_SCHEMA_VERSION - 1 : SUPPORTED_SCHEMA_VERSION)),
    waitHealth: async () => {
      // Simulate the hub's startup migration from the old to the new schema.
      openHubDatabase(database).close();
      return healthOk();
    },
  };
  const upgraded = await upgradeInstallation({ root, dir, from: 'fixture@2.0.0' }, deps);
  assert.equal(upgraded.version, '2.0.0');
  assert.equal(runtimeVersion(join(dir, 'runtime')), '2.0.0');
  assert.ok(upgraded.backupPath && existsSync(upgraded.backupPath));
  assert.equal(schemaVersionOf(database), SUPPORTED_SCHEMA_VERSION);
  const state = readRuntimeState(dir);
  assert.equal(state?.currentVersion, '2.0.0');
  assert.equal(state?.backups['2.0.0']?.path, upgraded.backupPath);

  // Make the newer database distinguishable; rollback must preserve it before
  // replacing it with the paired schema-3 copy.
  const db = new DatabaseSync(database);
  db.exec('CREATE TABLE post_upgrade_marker (value TEXT)');
  db.prepare('INSERT INTO post_upgrade_marker VALUES (?)').run('keep me');
  db.close();

  const rolled = await rollbackInstallation({ dir }, {
    service,
    inspectRuntime: runtimeInspector((runtimeDir) => (runtimeVersion(runtimeDir) === '1.0.0' ? SUPPORTED_SCHEMA_VERSION - 1 : SUPPORTED_SCHEMA_VERSION)),
    waitHealth: async () => healthOk(),
  });
  assert.equal(rolled.version, '1.0.0');
  assert.equal(runtimeVersion(join(dir, 'runtime')), '1.0.0');
  assert.equal(schemaVersionOf(database), 3);
  assert.equal(rolled.restoredBackup, upgraded.backupPath);
  assert.ok(rolled.preRestoreBackup && existsSync(rolled.preRestoreBackup));
  const preserved = new DatabaseSync(rolled.preRestoreBackup);
  try {
    const row = preserved.prepare('SELECT value FROM post_upgrade_marker').get() as { value: string };
    assert.equal(row.value, 'keep me');
  } finally {
    preserved.close();
  }
});

test('a failed upgrade restores the previous runtime and the pre-upgrade database', async () => {
  const root = fresh('failed-upgrade');
  const dir = join(root, 'state');
  initialize(dir, { port: '19193' });
  writeRuntime(join(dir, 'runtime'), '1.0.0');
  writeRuntimeState(dir, { ...emptyRuntimeState(dir), currentVersion: '1.0.0' });
  const database = join(dir, 'hub', 'telemetry.sqlite');
  seedDatabase(database, 3);
  const service = fakeService(true);
  const deps: UpgradeDependencies = {
    service,
    stage: () => {
      const stage = join(root, 'stage-failing');
      writeRuntime(stage, '2.0.0');
      return stage;
    },
    inspectRuntime: runtimeInspector((runtimeDir) => (runtimeVersion(runtimeDir) === '1.0.0' ? SUPPORTED_SCHEMA_VERSION - 1 : SUPPORTED_SCHEMA_VERSION)),
    waitHealth: async (_target, options) => {
      if (options?.expectedVersion === '1.0.0') return healthOk();
      openHubDatabase(database).close();
      return { ok: false, attempts: 2, elapsedMs: 25, last: { ok: false, statusCode: null, body: null, error: 'startup failed' }, version: null };
    },
  };
  await assert.rejects(
    upgradeInstallation({ root, dir, from: 'fixture@2.0.0', healthDeadlineMs: 100 }, deps),
    /upgrade to 2\.0\.0 failed.*restored runtime 1\.0\.0.*restored pre-upgrade database/s,
  );
  assert.equal(runtimeVersion(join(dir, 'runtime')), '1.0.0');
  assert.equal(runtimeVersion(join(dir, 'runtime-previous')), '2.0.0');
  assert.equal(service.active, true);
  assert.equal(schemaVersionOf(database), 3);
});

test('rollback keeps a database the previous runtime can still read', async () => {
  const root = fresh('compatible-rollback');
  const dir = join(root, 'state');
  initialize(dir, { port: '19195' });
  writeRuntime(join(dir, 'runtime'), '2.0.0');
  writeRuntime(join(dir, 'runtime-previous'), '1.0.0');
  const database = join(dir, 'hub', 'telemetry.sqlite');
  seedDatabase(database, SUPPORTED_SCHEMA_VERSION);
  const db = new DatabaseSync(database);
  db.exec('CREATE TABLE compatible_marker (value TEXT)');
  db.prepare('INSERT INTO compatible_marker VALUES (?)').run('survives rollback');
  db.close();
  const service = fakeService(true);
  const report = await rollbackInstallation({ dir }, {
    service,
    inspectRuntime: runtimeInspector(() => SUPPORTED_SCHEMA_VERSION),
    waitHealth: async () => healthOk(),
  });
  assert.equal(report.restoredBackup, null);
  assert.equal(runtimeVersion(join(dir, 'runtime')), '1.0.0');
  const after = new DatabaseSync(database);
  try {
    const row = after.prepare('SELECT value FROM compatible_marker').get() as { value: string };
    assert.equal(row.value, 'survives rollback');
  } finally {
    after.close();
  }
});

test('legacy rollback refuses an incompatible schema when no paired backup exists', async () => {
  const root = fresh('legacy');
  const dir = join(root, 'state');
  initialize(dir, { port: '19194' });
  writeRuntime(join(dir, 'runtime'), '2.0.0');
  writeRuntime(join(dir, 'runtime-previous'), '1.0.0');
  const database = join(dir, 'hub', 'telemetry.sqlite');
  seedDatabase(database, SUPPORTED_SCHEMA_VERSION);
  await assert.rejects(
    rollbackInstallation({ dir }, {
      service: fakeService(false),
      inspectRuntime: runtimeInspector((runtimeDir) => (runtimeVersion(runtimeDir) === '1.0.0' ? SUPPORTED_SCHEMA_VERSION - 1 : SUPPORTED_SCHEMA_VERSION)),
      waitHealth: async () => healthOk(),
    }),
    /no paired pre-upgrade backup/,
  );
  assert.equal(runtimeVersion(join(dir, 'runtime')), '2.0.0', 'refusal happens before the runtime switch');
});

test('offline password recovery updates only the managed store, audits and invalidates sessions', async () => {
  const dir = fresh('recovery');
  const database = join(dir, 'telemetry.sqlite');
  const hub = openHubDatabase(database);
  hub.close();
  const ownerHash = hashPassword('old-owner-password');
  const otherHash = hashPassword('other-password');
  const db = new DatabaseSync(database);
  try {
    db.exec('PRAGMA busy_timeout = 5000');
    db.prepare('INSERT INTO users (username, password_hash, scope_json, is_admin, disabled) VALUES (?, ?, ?, ?, ?)')
      .run('owner', ownerHash, '*', 1, 0);
    db.prepare('INSERT INTO users (username, password_hash, scope_json, is_admin, disabled) VALUES (?, ?, ?, ?, ?)')
      .run('viewer', otherHash, '["m1"]', 0, 1);
    db.prepare('INSERT INTO admin_audit (actor, action, target, at_ms) VALUES (?, ?, ?, ?)')
      .run('owner', 'user.update', 'viewer', 1);
  } finally {
    db.close();
  }
  const report = await recoverAdminPassword({ database, username: 'owner', password: 'new-owner-password', enable: true });
  assert.equal(report.username, 'owner');
  assert.equal(report.auditAction, 'user.password-recovery');
  assert.equal(report.sessionEpoch, 1);
  const after = new DatabaseSync(database);
  try {
    const owner = after.prepare('SELECT password_hash, disabled, is_admin FROM users WHERE username = ?').get('owner') as {
      password_hash: string; disabled: number; is_admin: number;
    };
    assert.equal(verifyPassword('new-owner-password', owner.password_hash), true);
    assert.equal(verifyPassword('old-owner-password', owner.password_hash), false);
    assert.equal(Number(owner.disabled), 0);
    assert.equal(Number(owner.is_admin), 1);
    const viewer = after.prepare('SELECT password_hash, disabled, scope_json FROM users WHERE username = ?').get('viewer') as {
      password_hash: string; disabled: number; scope_json: string;
    };
    assert.equal(viewer.password_hash, otherHash);
    assert.equal(Number(viewer.disabled), 1);
    assert.equal(viewer.scope_json, '["m1"]');
    const audit = after.prepare('SELECT actor, action, target FROM admin_audit ORDER BY id DESC LIMIT 1').get() as {
      actor: string; action: string; target: string;
    };
    assert.equal(audit.actor, 'local-recovery');
    assert.equal(audit.action, 'user.password-recovery');
    assert.equal(audit.target, 'owner');
    const meta = new Map(
      (after.prepare('SELECT key, value FROM admin_meta').all() as Array<{ key: string; value: string }>)
        .map((row) => [row.key, row.value]),
    );
    assert.equal(meta.get('session_epoch'), '1');
    assert.ok(meta.get('sessions_invalidated_at'));
  } finally {
    after.close();
  }
  await assert.rejects(
    recoverAdminPassword({ database, username: 'missing', password: 'unused' }),
    /does not exist/,
  );
});

test('offline password recovery refuses while another runtime owns the database lease', async () => {
  const dir = fresh('recovery-lock');
  const database = join(dir, 'telemetry.sqlite');
  const hub = openHubDatabase(database);
  hub.close();
  const db = new DatabaseSync(database);
  try {
    db.prepare('INSERT INTO users (username, password_hash, scope_json, is_admin, disabled) VALUES (?, ?, ?, ?, ?)')
      .run('owner', hashPassword('old'), '*', 1, 0);
  } finally {
    db.close();
  }
  const { acquireProcessLease } = await import('../src/runtime/process-lease');
  const release = acquireProcessLease(`${database}.hub.lock`);
  try {
    await assert.rejects(
      recoverAdminPassword({ database, username: 'owner', password: 'new-password' }),
      /another process owns this runtime state/,
    );
  } finally {
    release();
  }
});
