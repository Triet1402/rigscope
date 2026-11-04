/**
 * Component lifecycle fixtures: independent units, legacy migration, omitted
 * versus explicit operations, mixed activity across upgrade/rollback and
 * uninstall retention. Nothing here starts a real service.
 */
import assert from 'node:assert/strict';
import test from 'node:test';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { configureComponent, initialize, readSetup, type Component } from '../src/cli/config';
import { diagnoseInstallation } from '../src/cli/doctor';
import { startInstallation } from '../src/cli/lifecycle';
import {
  migrateLegacyService,
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
import { emptyRuntimeState, writeRuntimeState } from '../src/cli/release-state';
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
  writeFileSync(join(app, 'package.json'), `${JSON.stringify({ name: 'rigscope', version, rigscopeConfigVersion: 2 })}\n`);
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

function makeLegacyConfig(dir: string, role: 'local' | 'hub' | 'collector'): void {
  const config = readSetup(dir)!;
  const value = {
    version: 1,
    role,
    port: config.port,
    ...(config.origin ? { origin: config.origin, socket: config.socket } : {}),
    users: config.users,
    collectors: config.collectors,
  };
  writeFileSync(join(dir, 'config.json'), `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
}

function writeLegacyUnit(dir: string, options: { recognized?: boolean } = {}): string {
  const target = join(dir, 'runtime', 'node_modules', 'rigscope', 'bin', 'rigscope.cjs');
  const command = options.recognized === false ? '/bin/true' : `"${process.execPath}" "${target}" run --state-dir "${dir}"`;
  const path = unitPath(dir);
  mkdirSync(unitPath(dir).replace(/\/[^/]+$/, ''), { recursive: true });
  writeFileSync(path, `[Unit]\nDescription=Rigscope system monitor\n\n[Service]\nExecStart=${command}\n\n[Install]\nWantedBy=default.target\n`, { mode: 0o600 });
  return path;
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

test('read-only status reports the legacy unit without migrating it', () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('legacy-read'), 'state');
    initialize(dir, { role: 'hub', port: '19202' });
    makeLegacyConfig(dir, 'hub');
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    const legacyPath = writeLegacyUnit(dir);
    setUnits(system, { [unitName(dir)]: { active: true, enabled: true, pid: 1234 } });
    clearLog(system);

    const hub = serviceStatus(dir, 'hub');
    assert.equal(hub.active, true);
    assert.equal(hub.enabled, true);
    assert.equal(hub.legacy, true);
    assert.equal(startupStatus(dir, 'hub').enabled, true);
    assert.equal(existsSync(legacyPath), true, 'status must not remove the legacy unit');
    assert.equal(existsSync(unitPath(dir, 'hub')), false);
    const mutations = systemctlCalls(system).filter((call) => ['start', 'stop', 'enable', 'disable', 'daemon-reload'].includes(call.command));
    assert.deepEqual(mutations, []);
  } finally {
    system.restore();
  }
});

test('explicit migration stops the legacy unit before starting independent components', () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('legacy-local'), 'state');
    initialize(dir, { port: '19203' });
    makeLegacyConfig(dir, 'local');
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    const legacyPath = writeLegacyUnit(dir);
    setUnits(system, { [unitName(dir)]: { active: true, enabled: true, pid: 1111 } });
    clearLog(system);

    assert.equal(migrateLegacyService(dir), true);
    const calls = systemctlCalls(system);
    const stopLegacy = calls.findIndex((call) => call.command === 'stop' && call.unit === unitName(dir));
    const startHub = calls.findIndex((call) => call.command === 'start' && call.unit === unitName(dir, 'hub'));
    const startCollector = calls.findIndex((call) => call.command === 'start' && call.unit === unitName(dir, 'collector'));
    assert.ok(stopLegacy >= 0);
    assert.ok(startHub > stopLegacy);
    assert.ok(startCollector > stopLegacy);
    assert.equal(existsSync(legacyPath), false);
    assert.ok(existsSync(unitPath(dir, 'hub')));
    assert.ok(existsSync(unitPath(dir, 'collector')));

    const states = unitStates(system);
    assert.equal(states[unitName(dir)]?.active, false);
    assert.equal(states[unitName(dir, 'hub')]?.active, true);
    assert.equal(states[unitName(dir, 'collector')]?.active, true);
    assert.equal(states[unitName(dir, 'hub')]?.enabled, true);
    assert.equal(states[unitName(dir, 'collector')]?.enabled, true);
  } finally {
    system.restore();
  }
});

test('migrating a hub preset does not start or enable the disabled collector', () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('legacy-hub'), 'state');
    initialize(dir, { role: 'hub', port: '19204' });
    makeLegacyConfig(dir, 'hub');
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    writeLegacyUnit(dir);
    setUnits(system, { [unitName(dir)]: { active: true, enabled: true } });
    clearLog(system);

    writeService(dir);
    const calls = systemctlCalls(system);
    assert.equal(calls.some((call) => call.unit === unitName(dir, 'collector') && ['start', 'enable'].includes(call.command)), false);
    assert.ok(calls.some((call) => call.unit === unitName(dir, 'hub') && call.command === 'start'));
    const states = unitStates(system);
    assert.equal(states[unitName(dir, 'hub')]?.active, true);
    assert.equal(states[unitName(dir, 'collector')]?.active ?? false, false);
  } finally {
    system.restore();
  }
});

test('pre-refactor runtimes keep their working legacy unit until upgraded through the installed CLI', () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('legacy-old-runtime'), 'state');
    initialize(dir, {role: 'hub'});
    makeLegacyConfig(dir, 'hub');
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    writeFileSync(join(dir, 'runtime', 'node_modules', 'rigscope', 'package.json'), JSON.stringify({name: 'rigscope', version: '1.0.0'}));
    const path = writeLegacyUnit(dir);
    const config = readFileSync(join(dir, 'config.json'), 'utf8');
    setUnits(system, {[unitName(dir)]: {active: true, enabled: true}});
    clearLog(system);
    assert.throws(() => migrateLegacyService(dir), /existing installed rigscope upgrade/);
    assert.equal(existsSync(path), true);
    assert.equal(unitStates(system)[unitName(dir)]!.active, true);
    assert.equal(readFileSync(join(dir, 'config.json'), 'utf8'), config);
    assert.equal(systemctlCalls(system).length, 0);
  } finally { system.restore(); }
});

test('rollback refuses a runtime that cannot read component configuration before stopping services', async () => {
  const dir = join(fresh('rollback-old-config'), 'state');
  initialize(dir, {role: 'hub'});
  writeRuntime(join(dir, 'runtime'), '2.0.0');
  writeRuntime(join(dir, 'runtime-previous'), '1.0.0');
  writeFileSync(join(dir, 'runtime-previous', 'node_modules', 'rigscope', 'package.json'), JSON.stringify({name: 'rigscope', version: '1.0.0'}));
  await assert.rejects(rollbackInstallation({dir}, {
    service: {
      status: () => { throw new Error('must not inspect services'); },
      stop: () => { throw new Error('must not stop services'); },
      start: () => { throw new Error('must not start services'); },
    },
  }), /Rollback requires a release that supports component configuration/);
  assert.equal(JSON.parse(readFileSync(join(dir, 'runtime', 'node_modules', 'rigscope', 'package.json'), 'utf8')).version, '2.0.0');
});

test('an unrecognized legacy unit is never replaced or stopped', () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('legacy-foreign'), 'state');
    initialize(dir, { role: 'hub', port: '19205' });
    makeLegacyConfig(dir, 'hub');
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    const legacyPath = writeLegacyUnit(dir, { recognized: false });
    setUnits(system, { [unitName(dir)]: { active: true, enabled: true } });
    clearLog(system);
    assert.throws(() => writeService(dir), /unrecognized service file/);
    assert.equal(existsSync(legacyPath), true);
    assert.equal(systemctlCalls(system).some((call) => ['start', 'stop', 'disable'].includes(call.command)), false);
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

test('a legacy lifecycle command migrates before acting on only the selected component', () => {
  const system = fakeSystem();
  try {
    const dir = join(fresh('legacy-stop'), 'state');
    initialize(dir, { port: '19215' });
    makeLegacyConfig(dir, 'local');
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    writeLegacyUnit(dir);
    setUnits(system, { [unitName(dir)]: { active: true, enabled: true } });
    clearLog(system);

    systemctl(dir, 'stop', false, 'hub');
    const calls = systemctlCalls(system);
    const stopLegacy = calls.findIndex((call) => call.command === 'stop' && call.unit === unitName(dir));
    const stopHub = calls.findIndex((call) => call.command === 'stop' && call.unit === unitName(dir, 'hub'));
    assert.ok(stopLegacy >= 0);
    assert.ok(stopHub > stopLegacy);
    assert.equal(existsSync(unitPath(dir)), false);
    const states = unitStates(system);
    assert.equal(states[unitName(dir, 'hub')]?.active, false);
    assert.equal(states[unitName(dir, 'collector')]?.active, true, 'the unselected component keeps its prior activity');
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
