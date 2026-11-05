/**
 * Component lifecycle fixtures: independent units, incompatible-state reset,
 * omitted versus explicit operations, mixed activity across upgrade/rollback
 * and uninstall retention. Nothing here starts a real service.
 */
import assert from 'node:assert/strict';
import test from 'node:test';
import { spawn } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { configureComponent, initialize, installationNeedsReset, readSetup, type Component } from '../src/cli/config';
import { diagnoseInstallation } from '../src/cli/doctor';
import { startInstallation } from '../src/cli/lifecycle';
import { resetInstallation } from '../src/cli/reset';
import {
  combinedUnitName,
  combinedUnitPath,
  serviceStatus,
  startupStatus,
  systemctl,
  uninstallService,
  unitName,
  unitPath,
  writeService,
} from '../src/cli/services';
import { rollbackInstallation, upgradeInstallation, type ServiceController } from '../src/cli/upgrade';
import { inspectDatabase, SUPPORTED_SCHEMA_VERSION } from '../src/operations/database';
import { openHubDatabase } from '../src/hub/storage/db';
import { emptyRuntimeState, runtimeVersion, writeRuntimeState } from '../src/cli/release-state';
import { acquireProcessLease } from '../src/runtime/process-lease';

const ROOT = mkdtempSync(join(tmpdir(), 'rigscope-component-'));
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
  writeFileSync(join(app, 'package.json'), `${JSON.stringify({ name: 'rigscope', version })}\n`);
  for (const file of ['dist/cli.cjs', 'dist/hub.cjs', 'dist/collector.cjs', 'dist/ops.cjs', '.next/BUILD_ID']) {
    writeFileSync(join(app, file), 'fixture\n');
  }
}

type FakeSystem = {
  configHome: string;
  binHome: string;
  log: string;
  state: string;
  restore(): void;
};

function fakeSystem(): FakeSystem {
  const root = fresh('system');
  const configHome = join(root, 'config');
  const binHome = join(root, 'bin');
  const bin = join(root, 'path');
  const log = join(root, 'systemctl.log');
  const state = join(root, 'state.json');
  mkdirSync(configHome, { recursive: true });
  mkdirSync(binHome, { recursive: true });
  mkdirSync(bin, { recursive: true });
  writeFileSync(state, JSON.stringify({ units: {} }), { mode: 0o600 });
  const script = join(bin, 'systemctl');
  writeFileSync(script, `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(process.env.FAKE_SYSTEMCTL_LOG, JSON.stringify(args) + '\\n');
const statePath = process.env.FAKE_SYSTEMCTL_STATE;
const state = fs.existsSync(statePath) ? JSON.parse(fs.readFileSync(statePath, 'utf8')) : { units: {} };
const command = args[0] === '--user' ? args[1] : args[0];
const unit = args.slice(2).find((item) => item.endsWith('.service')) || args.find((item) => item.endsWith('.service'));
const entry = unit ? state.units[unit] || { active: false, enabled: false, pid: 0 } : null;
if (command === 'show' && unit) {
  process.stdout.write([
    'ActiveState=' + (entry.active ? 'active' : 'inactive'),
    'SubState=' + (entry.active ? 'running' : 'dead'),
    'ExecMainPID=' + (entry.pid || 0),
    'UnitFileState=' + (entry.enabled ? 'enabled' : 'disabled'),
  ].join('\\n') + '\\n');
} else if (command === 'is-enabled' && unit) {
  if (entry.enabled) process.stdout.write('enabled\\n');
  else { process.stderr.write('disabled\\n'); process.exitCode = 1; }
} else if (unit && ['start', 'stop', 'restart', 'enable', 'disable'].includes(command)) {
  const next = state.units[unit] || { active: false, enabled: false, pid: 0 };
  if (command === 'start' || command === 'restart') { next.active = true; next.pid = 4242; }
  if (command === 'stop') { next.active = false; next.pid = 0; }
  if (command === 'enable') next.enabled = true;
  if (command === 'disable') next.enabled = false;
  state.units[unit] = next;
}
fs.writeFileSync(statePath, JSON.stringify(state));
`, { mode: 0o755 });
  const previousConfig = process.env.XDG_CONFIG_HOME;
  const previousBin = process.env.XDG_BIN_HOME;
  const previousPath = process.env.PATH;
  process.env.XDG_CONFIG_HOME = configHome;
  process.env.XDG_BIN_HOME = binHome;
  process.env.PATH = `${bin}:${previousPath ?? ''}`;
  process.env.FAKE_SYSTEMCTL_LOG = log;
  process.env.FAKE_SYSTEMCTL_STATE = state;
  return {
    configHome, binHome, log, state,
    restore() {
      if (previousConfig === undefined) delete process.env.XDG_CONFIG_HOME; else process.env.XDG_CONFIG_HOME = previousConfig;
      if (previousBin === undefined) delete process.env.XDG_BIN_HOME; else process.env.XDG_BIN_HOME = previousBin;
      if (previousPath === undefined) delete process.env.PATH; else process.env.PATH = previousPath;
      delete process.env.FAKE_SYSTEMCTL_LOG;
      delete process.env.FAKE_SYSTEMCTL_STATE;
    },
  };
}

type UnitState = { active?: boolean; enabled?: boolean; pid?: number };
function setUnits(system: FakeSystem, units: Record<string, UnitState>): void {
  const next: Record<string, { active: boolean; enabled: boolean; pid: number }> = {};
  for (const [unit, value] of Object.entries(units)) {
    next[unit] = { active: value.active ?? false, enabled: value.enabled ?? false, pid: value.pid ?? 0 };
  }
  writeFileSync(system.state, JSON.stringify({ units: next }), { mode: 0o600 });
}
function unitStates(system: FakeSystem): Record<string, { active: boolean; enabled: boolean; pid: number }> {
  return (JSON.parse(readFileSync(system.state, 'utf8')) as { units: Record<string, { active: boolean; enabled: boolean; pid: number }> }).units;
}
function systemctlCalls(system: FakeSystem): Array<{ command: string; unit: string | null }> {
  if (!existsSync(system.log)) return [];
  return readFileSync(system.log, 'utf8').trim().split('\n').filter(Boolean).map((line) => {
    const args = JSON.parse(line) as string[];
    return { command: args[1] ?? '', unit: args[2] ?? null };
  });
}
function clearLog(system: FakeSystem): void {
  writeFileSync(system.log, '');
}

function writeIncompatibleState(dir: string): void {
  mkdirSync(join(dir, 'collector'), {recursive: true, mode: 0o700});
  mkdirSync(join(dir, 'hub'), {recursive: true, mode: 0o700});
  mkdirSync(join(dir, 'backups'), {recursive: true, mode: 0o700});
  writeFileSync(join(dir, 'config.json'), `${JSON.stringify({
    version: 2, role: 'local', port: 19215,
    users: [{username: 'owner', passwordHash: 'old-hash', machineIds: '*', admin: true}],
    collectors: [{collectorId: 'old-collector', machineId: 'old-machine', token: 'old-token'}],
    components: {hub: {enabled: true}, collector: {enabled: true}},
  })}\n`, {mode: 0o600});
  writeFileSync(join(dir, 'collector', 'state.json'),
    `${JSON.stringify({version: 2, collectorId: 'old-collector', destinations: []})}\n`, {mode: 0o600});
  writeFileSync(join(dir, 'release-state.json'),
    `${JSON.stringify({formatVersion: 1, stateDir: dir})}\n`, {mode: 0o600});
  writeFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'old history\n', {mode: 0o600});
  writeFileSync(join(dir, 'backups', 'old.sqlite'), 'old backup\n', {mode: 0o600});
}

function ownedUnit(dir: string, component?: Component): string {
  const target = join(dir, 'runtime', 'node_modules', 'rigscope', 'bin', 'rigscope.cjs');
  const suffix = component ? ` run ${component}` : '';
  return `[Service]\nExecStart=/usr/bin/node ${target}${suffix} --state-dir ${dir}\n`;
}

test('component units are independent and omitted operations select enabled components only', () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('independent'), 'state');
    initialize(dir, { port: '19201' });
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    writeService(dir);
    const hubUnit = readFileSync(unitPath(dir, 'hub'), 'utf8');
    const collectorUnit = readFileSync(unitPath(dir, 'collector'), 'utf8');
    assert.match(hubUnit, / run hub --state-dir /);
    assert.match(collectorUnit, / run collector --state-dir /);
    assert.doesNotMatch(hubUnit, /PartOf=|BindsTo=|Requires=/);
    assert.doesNotMatch(collectorUnit, /PartOf=|BindsTo=|Requires=/);
    assert.equal(hubUnit.includes(unitName(dir, 'collector')), false);
    assert.equal(collectorUnit.includes(unitName(dir, 'hub')), false);
    assert.notEqual(unitName(dir, 'hub'), unitName(dir, 'collector'));

    setUnits(system, { [unitName(dir, 'hub')]: { enabled: true }, [unitName(dir, 'collector')]: { enabled: true } });
    clearLog(system);
    systemctl(dir, 'start');
    let calls = systemctlCalls(system);
    assert.deepEqual(calls.map((call) => call.unit).sort(), [unitName(dir, 'hub'), unitName(dir, 'collector')].sort());

    clearLog(system);
    systemctl(dir, 'restart', false, 'hub');
    calls = systemctlCalls(system);
    assert.deepEqual(calls, [{ command: 'restart', unit: unitName(dir, 'hub') }]);

    const hub = serviceStatus(dir, 'hub');
    const collector = serviceStatus(dir, 'collector');
    assert.equal(hub.enabled, true);
    assert.equal(hub.active, true);
    assert.equal(collector.enabled, true);
    assert.equal(startupStatus(dir, 'hub').enabled, true);
    assert.equal(startupStatus(dir, 'collector').enabled, true);

    // Disabled components are not part of an omitted operation.
    configureComponent(dir, 'collector', { enabled: false });
    clearLog(system);
    systemctl(dir, 'start');
    calls = systemctlCalls(system);
    assert.deepEqual(calls, [{ command: 'start', unit: unitName(dir, 'hub') }]);
  } finally {
    system.restore();
  }
});

test('startInstallation verifies only the requested component and never touches the other', async () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('start-explicit'), 'state');
    initialize(dir, { port: '19206' });
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    writeService(dir);
    setUnits(system, {
      [unitName(dir, 'hub')]: { active: false, enabled: true },
      [unitName(dir, 'collector')]: { active: true, enabled: true, pid: 777 },
    });
    clearLog(system);
    let verified: Component[] | null = null;
    const result = await startInstallation(dir, 'hub', {
      verify: async (options) => {
        verified = options.components ?? null;
        return { version: '1.0.0', health: null, database: null };
      },
    });
    assert.equal(result, 'started');
    assert.deepEqual(verified, ['hub']);
    const calls = systemctlCalls(system);
    assert.ok(calls.some((call) => call.command === 'start' && call.unit === unitName(dir, 'hub')));
    assert.equal(calls.some((call) => call.unit === unitName(dir, 'collector')), false);
    assert.equal(unitStates(system)[unitName(dir, 'collector')]?.active, true, 'unrelated component stays active');
  } finally {
    system.restore();
  }
});

test('starting an unenrolled collector reports awaiting connection without a restart loop', async () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('start-awaiting'), 'state');
    initialize(dir, { role: 'collector', port: '19207' });
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    writeService(dir);
    setUnits(system, { [unitName(dir, 'collector')]: { active: false, enabled: true } });
    clearLog(system);
    await assert.rejects(startInstallation(dir, 'collector'), /awaiting connection/i);
    assert.equal(systemctlCalls(system).some((call) => call.command === 'start'), false);
    assert.equal(unitStates(system)[unitName(dir, 'collector')]?.active, false);
  } finally {
    system.restore();
  }
});

test('uninstall removes component units and runtimes but retains configuration and data', () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('uninstall'), 'state');
    initialize(dir, { port: '19208' });
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    writeRuntime(join(dir, 'runtime-previous'), '0.9.0');
    writeService(dir);
    writeFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'history', { mode: 0o600 });
    setUnits(system, {
      [unitName(dir, 'hub')]: { active: true, enabled: true, pid: 1 },
      [unitName(dir, 'collector')]: { active: true, enabled: true, pid: 2 },
    });
    uninstallService(dir);
    assert.equal(existsSync(unitPath(dir, 'hub')), false);
    assert.equal(existsSync(unitPath(dir, 'collector')), false);
    assert.equal(existsSync(join(dir, 'runtime')), false);
    assert.equal(existsSync(join(dir, 'runtime-previous')), false);
    assert.ok(existsSync(join(dir, 'config.json')));
    assert.ok(existsSync(join(dir, 'hub', 'telemetry.sqlite')));
    assert.ok(existsSync(join(dir, 'collector', 'state.json')));
  } finally {
    system.restore();
  }
});

test('uninstall cleanup removes an owned historical combined unit', () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('uninstall-combined'), 'state');
    initialize(dir, { port: '19216' });
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    mkdirSync(join(system.configHome, 'systemd', 'user'), { recursive: true });
    writeFileSync(combinedUnitPath(dir), ownedUnit(dir), { mode: 0o600 });
    setUnits(system, { [combinedUnitName(dir)]: { active: true, enabled: true, pid: 55 } });
    clearLog(system);
    uninstallService(dir);
    assert.equal(existsSync(combinedUnitPath(dir)), false);
    const calls = systemctlCalls(system);
    assert.ok(calls.some((call) => call.command === 'stop' && call.unit === combinedUnitName(dir)));
    assert.ok(calls.some((call) => call.command === 'disable' && call.unit === combinedUnitName(dir)));
  } finally {
    system.restore();
  }
});

test('reset replaces an incompatible runtime, removes every owned unit, and deletes state', () => {
  const system = fakeSystem();
  try {
    const root = fresh('reset-replace');
    const dir = join(root, 'state');
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    chmodSync(dir, 0o700);
    writeIncompatibleState(dir);
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    writeFileSync(join(dir, 'runtime', 'old-marker'), 'old\n');
    mkdirSync(join(system.configHome, 'systemd', 'user'), { recursive: true });
    writeFileSync(unitPath(dir, 'hub'), ownedUnit(dir, 'hub'), { mode: 0o600 });
    writeFileSync(combinedUnitPath(dir), ownedUnit(dir), { mode: 0o600 });
    setUnits(system, {
      [unitName(dir, 'hub')]: { active: true, enabled: true, pid: 111 },
      [combinedUnitName(dir)]: { active: true, enabled: true, pid: 222 },
    });
    clearLog(system);

    const stage = join(root, 'stage');
    writeRuntime(stage, '9.9.9');
    const report = resetInstallation({ dir, stage, setup: { port: '19217' } });

    assert.equal(report.created, true);
    assert.ok(report.password);
    assert.equal(runtimeVersion(join(dir, 'runtime')), '9.9.9', 'the staged replacement is active');
    assert.equal(existsSync(join(dir, 'runtime', 'old-marker')), false, 'the incompatible runtime is removed');
    assert.equal(existsSync(join(dir, 'runtime-previous')), false, 'an unusable runtime is not retained for rollback');
    assert.equal(existsSync(stage), false, 'the staged tree became the active runtime');

    assert.equal(existsSync(unitPath(dir, 'hub')), false);
    assert.equal(existsSync(combinedUnitPath(dir)), false);
    const calls = systemctlCalls(system);
    for (const unit of [unitName(dir, 'hub'), combinedUnitName(dir)]) {
      assert.ok(calls.some((call) => call.command === 'stop' && call.unit === unit), `${unit} is stopped`);
      assert.ok(calls.some((call) => call.command === 'disable' && call.unit === unit), `${unit} is disabled`);
    }

    const config = JSON.parse(readFileSync(join(dir, 'config.json'), 'utf8')) as Record<string, unknown>;
    assert.equal('version' in config, false);
    assert.equal(config.port, 19217);
    assert.deepEqual(config.components, { hub: { enabled: true }, collector: { enabled: true } });
    const collector = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8')) as Record<string, unknown>;
    assert.equal('version' in collector, false);
    assert.equal((collector.destinations as { url: string }[])[0]!.url, 'ws://127.0.0.1:19217/api/collector/connect');
    assert.equal(existsSync(join(dir, 'hub', 'telemetry.sqlite')), false, 'old history is removed');
    assert.equal(existsSync(join(dir, 'backups', 'old.sqlite')), false, 'old backups are removed');
    assert.equal(installationNeedsReset(dir), false);
  } finally {
    system.restore();
  }
});

test('reset refuses an invalid replacement before stopping services or deleting state', () => {
  const system = fakeSystem();
  try {
    const root = fresh('reset-invalid');
    const dir = join(root, 'state');
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    chmodSync(dir, 0o700);
    writeIncompatibleState(dir);
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    mkdirSync(join(system.configHome, 'systemd', 'user'), { recursive: true });
    writeFileSync(unitPath(dir, 'hub'), ownedUnit(dir, 'hub'), { mode: 0o600 });
    setUnits(system, { [unitName(dir, 'hub')]: { active: true, enabled: true, pid: 333 } });
    const configBefore = readFileSync(join(dir, 'config.json'), 'utf8');
    const collectorBefore = readFileSync(join(dir, 'collector', 'state.json'), 'utf8');
    clearLog(system);

    const stage = join(root, 'invalid-stage');
    mkdirSync(stage, { recursive: true, mode: 0o700 });
    chmodSync(stage, 0o700);
    assert.throws(() => resetInstallation({ dir, stage, setup: { port: '19218' } }), /replacement runtime|missing/i);
    writeRuntime(stage, '9.9.9');
    const metadata = join(stage, 'node_modules', 'rigscope', 'package.json');
    writeFileSync(metadata, JSON.stringify({name: 'rigscope', version: '9.9.9', rigscopeConfigVersion: 2}));
    assert.throws(() => resetInstallation({dir, stage}), /canonical|unfinished/i);

    assert.equal(readFileSync(join(dir, 'config.json'), 'utf8'), configBefore);
    assert.equal(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'), collectorBefore);
    assert.equal(readFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'utf8'), 'old history\n');
    assert.equal(runtimeVersion(join(dir, 'runtime')), '1.0.0');
    assert.ok(existsSync(unitPath(dir, 'hub')));
    assert.equal(unitStates(system)[unitName(dir, 'hub')]?.active, true, 'services are untouched');
    assert.deepEqual(systemctlCalls(system), []);
  } finally {
    system.restore();
  }
});

test('an interrupted reset remains detectable until replacement initialization succeeds', () => {
  const system = fakeSystem();
  try {
    const root = fresh('reset-retry');
    const dir = join(root, 'state');
    mkdirSync(dir, {recursive: true, mode: 0o700});
    writeIncompatibleState(dir);
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    const stage = join(root, 'stage');
    writeRuntime(stage, '9.9.9');
    assert.throws(() => resetInstallation({dir, stage, setup: {role: 'hub'}}, {
      activate: () => { throw new Error('activation unavailable'); },
    }), /activation unavailable/);
    assert.equal(existsSync(join(dir, 'config.json')), false);
    assert.equal(installationNeedsReset(dir), true);
    assert.throws(() => initialize(dir), /older configuration or state shape/);
    resetInstallation({dir, stage, setup: {role: 'hub'}});
    assert.equal(runtimeVersion(join(dir, 'runtime')), '9.9.9');
    assert.equal(installationNeedsReset(dir), false);
    assert.equal(readSetup(dir)?.role, 'hub');
  } finally { system.restore(); }
});

test('reset validates access options before stopping services or deleting state', () => {
  const system = fakeSystem();
  try {
    const root = fresh('reset-access');
    const dir = join(root, 'state');
    mkdirSync(dir, {recursive: true, mode: 0o700});
    writeIncompatibleState(dir);
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    const stage = join(root, 'stage');
    writeRuntime(stage, '9.9.9');
    const configBefore = readFileSync(join(dir, 'config.json'), 'utf8');
    for (const setup of [
      {origin: 'http://remote.example.com'},
      {origin: 'https://remote.example.com', socket: 'relative.sock'},
    ]) {
      assert.throws(() => resetInstallation({dir, stage, setup}), /HTTPS|absolute path/i);
      assert.equal(readFileSync(join(dir, 'config.json'), 'utf8'), configBefore);
      assert.equal(readFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'utf8'), 'old history\n');
      assert.equal(runtimeVersion(join(dir, 'runtime')), '1.0.0');
      assert.deepEqual(systemctlCalls(system), []);
    }
  } finally { system.restore(); }
});

test('reset refuses a live lease PID whose identity is unrelated', { timeout: 30000 }, async () => {
  const system = fakeSystem();
  let unrelated: ReturnType<typeof spawn> | undefined;
  try {
    const root = fresh('reset-unrelated');
    const dir = join(root, 'state');
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    chmodSync(dir, 0o700);
    mkdirSync(join(dir, 'run'), { recursive: true, mode: 0o700 });
    chmodSync(join(dir, 'run'), 0o700);
    writeIncompatibleState(dir);
    writeRuntime(join(dir, 'runtime'), '1.0.0');

    const unrelatedEnv = { ...process.env };
    for (const key of ['RIGSCOPE_STATE_DIR', 'RIGSCOPE_HUB_LOCK_FILE', 'HUB_STATE_FILE', 'HUB_DATABASE_PATH', 'COLLECTOR_STATE_FILE', 'COLLECTOR_HEALTH_FILE']) {
      delete unrelatedEnv[key];
    }
    unrelatedEnv.HUB_DATABASE_PATH = join(`${dir}-sibling`, 'hub', 'telemetry.sqlite');
    unrelated = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)', `${dir}-sibling`], {
      stdio: 'ignore',
      env: unrelatedEnv,
    });
    assert.ok(unrelated.pid);
    const unrelatedPid = unrelated.pid!;
    writeFileSync(join(dir, 'run', 'rigscope.lock'), JSON.stringify({ pid: unrelatedPid, instance: 'stale' }), { mode: 0o600 });
    const configBefore = readFileSync(join(dir, 'config.json'), 'utf8');

    const stage = join(root, 'stage');
    writeRuntime(stage, '9.9.9');
    assert.throws(() => resetInstallation({ dir, stage, setup: { port: '19220' } }), /refusing to signal an unrelated process/);
    assert.equal(readFileSync(join(dir, 'config.json'), 'utf8'), configBefore);
    assert.equal(readFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'utf8'), 'old history\n');
    assert.equal(runtimeVersion(join(dir, 'runtime')), '1.0.0');
    assert.doesNotThrow(() => process.kill(unrelatedPid, 0), 'the unrelated process is left alone');
  } finally {
    if (unrelated && unrelated.pid) {
      try { process.kill(unrelated.pid, 'SIGKILL'); } catch { /* already stopped */ }
    }
    system.restore();
  }
});

test('reset stops an identified foreground process before deleting its state', { timeout: 30000 }, async () => {
  const system = fakeSystem();
  let child: ReturnType<typeof spawn> | undefined;
  try {
    const root = fresh('reset-process');
    const dir = join(root, 'state');
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    chmodSync(dir, 0o700);
    mkdirSync(join(dir, 'run'), { recursive: true, mode: 0o700 });
    chmodSync(join(dir, 'run'), 0o700);
    writeIncompatibleState(dir);
    writeRuntime(join(dir, 'runtime'), '1.0.0');

    const marker = join(root, 'stopped');
    const lock = join(dir, 'run', 'rigscope.lock');
    const script = join(root, 'hold.ts');
    writeFileSync(script, `
import { writeFileSync } from 'node:fs';
import { acquireProcessLease } from ${JSON.stringify(join(import.meta.dirname, '..', 'src', 'runtime', 'process-lease.ts'))};
acquireProcessLease(process.argv[2]);
process.on('SIGTERM', () => { writeFileSync(process.argv[3], 'stopped'); process.exit(0); });
setInterval(() => {}, 1000);
`);
    child = spawn(process.execPath, [
      '--import', 'tsx', script, lock, marker, '--state-dir', dir,
    ], { stdio: 'ignore', env: { ...process.env } });
    await waitFor(() => existsSync(lock) && existsSync(dir), 5000);

    const stage = join(root, 'stage');
    writeRuntime(stage, '9.9.9');
    const report = resetInstallation({ dir, stage, setup: { port: '19219' } });

    assert.equal(report.created, true);
    assert.equal(existsSync(marker), true, 'the process received the stop request before state deletion');
    assert.equal(existsSync(join(dir, 'hub', 'telemetry.sqlite')), false);
    assert.equal(runtimeVersion(join(dir, 'runtime')), '9.9.9');
    await new Promise<void>((resolve) => {
      if (child!.exitCode !== null || child!.signalCode !== null) resolve();
      else child!.once('exit', () => resolve());
    });
    assert.throws(() => process.kill(child!.pid!, 0), /ESRCH/, 'the foreground process is gone');
  } finally {
    if (child && child.pid) {
      try { process.kill(child.pid, 'SIGKILL'); } catch { /* already stopped */ }
    }
    system.restore();
  }
});

async function waitFor(condition: () => boolean, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error('condition was not reached before the deadline');
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
}

type ComponentState = { active: boolean; enabled: boolean };
type ControllerCall = { operation: 'stop' | 'start' | 'setStartup'; component: Component; enabled?: boolean };

function componentService(initial: Record<Component, ComponentState>): ServiceController & {
  states: Record<Component, ComponentState>;
  calls: ControllerCall[];
} {
  const states: Record<Component, ComponentState> = {
    hub: { ...initial.hub },
    collector: { ...initial.collector },
  };
  const leases: Partial<Record<Component, Array<() => void>>> = {};
  const calls: ControllerCall[] = [];
  return {
    states,
    calls,
    status(dir, component): ReturnType<ServiceController['status']> {
      const target = component ?? 'hub';
      const state = states[target];
      return {
        unit: `fixture-${target}.service`, installed: true, active: state.active,
        enabled: state.enabled, subState: state.active ? 'running' : 'dead',
        execMainPid: state.active ? 100 : null, error: null, component: target,
      };
    },
    stop(dir, component): void {
      const targets = component ? [component] : (['hub', 'collector'] as Component[]);
      for (const target of targets) {
        calls.push({ operation: 'stop', component: target });
        states[target].active = false;
        for (const release of leases[target]?.splice(0) ?? []) release();
      }
    },
    start(dir, component): void {
      const target = component ?? 'hub';
      calls.push({ operation: 'start', component: target });
      const acquired = [acquireProcessLease(join(dir, 'run', `${target}.lock`))];
      try {
        if (target === 'collector') {
          acquired.push(acquireProcessLease(join(dir, 'collector', 'collector-runtime.lock')));
          writeFileSync(join(dir, 'run', 'collector-health.json'), JSON.stringify({
            pid: process.pid, sampledAt: Date.now(), version: JSON.parse(readFileSync(join(dir, 'runtime', 'node_modules', 'rigscope', 'package.json'), 'utf8')).version,
          }), { mode: 0o600 });
        }
      } catch (error) {
        for (const release of acquired.reverse()) release();
        throw error;
      }
      leases[target] = acquired;
      states[target].active = true;
    },
    startup(dir, component): boolean {
      return states[component ?? 'hub'].enabled;
    },
    setStartup(dir, enabled, component): void {
      const target = component ?? 'hub';
      calls.push({ operation: 'setStartup', component: target, enabled });
      states[target].enabled = enabled;
    },
  };
}

function healthOk(): Awaited<ReturnType<typeof import('../src/cli/health').waitForHealth>> {
  return {
    ok: true, attempts: 1, elapsedMs: 1,
    last: { ok: true, statusCode: 200, body: { status: 'ok' }, error: null }, version: null,
  };
}

function inspector() {
  return (runtimeDir: string, database: string) => {
    const actual = inspectDatabase(database);
    return {
      supportedSchemaVersion: SUPPORTED_SCHEMA_VERSION,
      schemaVersion: actual.schemaVersion,
      integrityOk: actual.integrityOk,
      newerSchema: actual.schemaVersion > SUPPORTED_SCHEMA_VERSION,
      ledgerProblem: actual.ledgerProblem,
    };
  };
}

test('upgrade snapshots mixed activity and restores only the previously running component', async () => {
  const root = fresh('upgrade-mixed');
  const dir = join(root, 'state');
  initialize(dir, { port: '19209' });
  configureComponent(dir, 'collector', { enabled: false });
  writeRuntime(join(dir, 'runtime'), '1.0.0');
  writeRuntimeState(dir, { ...emptyRuntimeState(dir), currentVersion: '1.0.0' });
  const database = join(dir, 'hub', 'telemetry.sqlite');
  openHubDatabase(database).close();

  const service = componentService({
    hub: { active: true, enabled: true },
    collector: { active: false, enabled: false },
  });
  let stageCalls = 0;
  const leases: string[] = [];
  const report = await upgradeInstallation({ root, dir, from: 'fixture@2.0.0' }, {
    service,
    stage: () => {
      stageCalls += 1;
      const stage = join(root, 'stage');
      writeRuntime(stage, '2.0.0');
      return stage;
    },
    inspectRuntime: inspector(),
    waitHealth: async () => healthOk(),
    acquireLease: (path) => {
      leases.push(path);
      return acquireProcessLease(path);
    },
  });
  assert.equal(stageCalls, 1);
  assert.ok(leases.includes(join(dir, 'run', 'hub.lock')));
  assert.ok(leases.includes(join(dir, 'run', 'collector.lock')));
  assert.ok(leases.includes(join(dir, 'run', 'rigscope.lock')));
  assert.ok(leases.includes(join(dir, 'collector', 'collector-runtime.lock')));
  assert.equal(report.version, '2.0.0');
  assert.equal(report.serviceRestarted, true);
  assert.ok(report.backupPath && existsSync(report.backupPath));
  assert.equal(service.states.hub.active, true);
  assert.equal(service.states.collector.active, false);
  assert.equal(service.states.hub.enabled, true);
  assert.equal(service.states.collector.enabled, false);
  assert.equal(service.calls.some((call) => call.component === 'collector' && call.operation !== 'setStartup'), false);
  assert.ok(service.calls.some((call) => call.component === 'hub' && call.operation === 'stop'));
  assert.ok(service.calls.some((call) => call.component === 'hub' && call.operation === 'start'));
});

test('upgrade with an active collector and disabled hub never touches the hub service', async () => {
  const root = fresh('upgrade-collector-only');
  const dir = join(root, 'state');
  initialize(dir, { port: '19210' });
  configureComponent(dir, 'hub', { enabled: false });
  writeRuntime(join(dir, 'runtime'), '1.0.0');
  writeRuntimeState(dir, { ...emptyRuntimeState(dir), currentVersion: '1.0.0' });
  const service = componentService({
    hub: { active: false, enabled: false },
    collector: { active: true, enabled: true },
  });
  const report = await upgradeInstallation({ root, dir, from: 'fixture@2.0.0' }, {
    service,
    stage: () => {
      const stage = join(root, 'stage');
      writeRuntime(stage, '2.0.0');
      return stage;
    },
    inspectRuntime: inspector(),
    waitHealth: async () => healthOk(),
  });
  assert.equal(report.version, '2.0.0');
  assert.equal(service.states.collector.active, true);
  assert.equal(service.states.hub.active, false);
  assert.equal(service.calls.some((call) => call.component === 'hub' && call.operation !== 'setStartup'), false);
  assert.ok(service.calls.some((call) => call.component === 'collector' && call.operation === 'start'));
});

test('a failed mixed upgrade restores the prior runtime, activity and startup flags', async () => {
  const root = fresh('upgrade-mixed-failure');
  const dir = join(root, 'state');
  initialize(dir, { port: '19211' });
  configureComponent(dir, 'collector', { enabled: false });
  writeRuntime(join(dir, 'runtime'), '1.0.0');
  writeRuntimeState(dir, { ...emptyRuntimeState(dir), currentVersion: '1.0.0' });
  const database = join(dir, 'hub', 'telemetry.sqlite');
  openHubDatabase(database).close();
  const service = componentService({
    hub: { active: true, enabled: true },
    collector: { active: false, enabled: false },
  });
  await assert.rejects(
    upgradeInstallation({ root, dir, from: 'fixture@2.0.0', healthDeadlineMs: 100 }, {
      service,
      stage: () => {
        const stage = join(root, 'stage');
        writeRuntime(stage, '2.0.0');
        return stage;
      },
      inspectRuntime: inspector(),
      waitHealth: async (_target, options) => options?.expectedVersion === '1.0.0'
        ? healthOk()
        : { ok: false, attempts: 2, elapsedMs: 10, last: { ok: false, statusCode: null, body: null, error: 'startup failed' }, version: null },
    }),
    /upgrade to 2\.0\.0 failed.*restored runtime 1\.0\.0/s,
  );
  assert.equal(readSetup(dir)?.components?.collector.enabled, false);
  assert.equal(service.states.hub.active, true);
  assert.equal(service.states.collector.active, false);
  assert.equal(service.states.hub.enabled, true);
  assert.equal(service.states.collector.enabled, false);
  assert.equal(service.calls.some((call) => call.component === 'collector' && call.operation !== 'setStartup'), false);
  const state = JSON.parse(readFileSync(join(dir, 'config.json'), 'utf8')) as { components: { collector: { enabled: boolean } } };
  assert.equal(state.components.collector.enabled, false);
});

test('rollback snapshots mixed activity and leaves a disabled component stopped', async () => {
  const dir = join(fresh('rollback-mixed'), 'state');
  initialize(dir, { port: '19212' });
  configureComponent(dir, 'collector', { enabled: false });
  writeRuntime(join(dir, 'runtime'), '2.0.0');
  writeRuntime(join(dir, 'runtime-previous'), '1.0.0');
  const database = join(dir, 'hub', 'telemetry.sqlite');
  openHubDatabase(database).close();
  const service = componentService({
    hub: { active: true, enabled: true },
    collector: { active: false, enabled: false },
  });
  const report = await rollbackInstallation({ dir }, {
    service,
    inspectRuntime: inspector(),
    waitHealth: async () => healthOk(),
  });
  assert.equal(report.version, '1.0.0');
  assert.equal(report.serviceRestarted, true);
  assert.equal(service.states.hub.active, true);
  assert.equal(service.states.collector.active, false);
  assert.equal(service.calls.some((call) => call.component === 'collector' && call.operation !== 'setStartup'), false);
});

test('doctor reports a disabled component independently of an active hub', async () => {
  const dir = join(fresh('doctor-disabled'), 'state');
  initialize(dir, { port: '19213' });
  configureComponent(dir, 'collector', { enabled: false });
  writeRuntime(join(dir, 'runtime'), '1.0.0');
  openHubDatabase(join(dir, 'hub', 'telemetry.sqlite')).close();
  const report = await diagnoseInstallation(dir, {
    serviceStatus: (_dir, component) => component === 'hub'
      ? { unit: 'fixture-hub.service', installed: true, active: true, enabled: true, subState: 'running', execMainPid: 1, error: null }
      : { unit: 'fixture-collector.service', installed: true, active: false, enabled: false, subState: 'dead', execMainPid: null, error: null },
    inspectRuntime: inspector(),
    probeHealth: async () => ({ ok: true, statusCode: 200, body: { status: 'ok', version: '1.0.0' }, error: null }),
    probeTcp: async () => ({ ok: true, error: null, ms: 1 }),
    probeTls: async () => ({ ok: true, error: null, ms: 1 }),
    diskFree: () => ({ free: 8 * 1024 * 1024 * 1024, total: 64 * 1024 * 1024 * 1024 }),
    sensorReadable: (paths) => paths,
  });
  assert.equal(report.ok, true, JSON.stringify(report.checks));
  const collector = report.checks.find((check) => check.id === 'service:collector');
  assert.equal(collector?.status, 'ok');
  assert.equal(collector?.detail, 'disabled');
  assert.equal(report.checks.some((check) => check.id === 'collector'), false, 'disabled collector has no destination health check');
});

test('doctor reports awaiting connection for an enabled unenrolled collector', async () => {
  const dir = join(fresh('doctor-awaiting'), 'state');
  initialize(dir, { role: 'collector', port: '19214' });
  writeRuntime(join(dir, 'runtime'), '1.0.0');
  const report = await diagnoseInstallation(dir, {
    serviceStatus: (_dir, component) => component === 'collector'
      ? { unit: 'fixture-collector.service', installed: true, active: false, enabled: true, subState: 'dead', execMainPid: null, error: null }
      : { unit: 'fixture-hub.service', installed: true, active: false, enabled: false, subState: 'dead', execMainPid: null, error: null },
    inspectRuntime: inspector(),
    probeHealth: async () => ({ ok: false, statusCode: null, body: null, error: 'not probed' }),
    probeTcp: async () => ({ ok: false, error: 'not probed', ms: 0 }),
    probeTls: async () => ({ ok: false, error: 'not probed', ms: 0 }),
    diskFree: () => null,
    sensorReadable: (paths) => paths,
  });
  assert.equal(report.ok, true, JSON.stringify(report.checks));
  assert.equal(report.checks.find((check) => check.id === 'service:collector')?.detail, 'Awaiting connection');
  assert.equal(report.checks.find((check) => check.id === 'collector')?.detail, 'Awaiting connection');
  assert.equal(report.checks.some((check) => check.id === 'database'), false, 'disabled hub has no database check');
});
