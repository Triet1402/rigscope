import { execFileSync } from 'node:child_process';
import { existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, readlinkSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import {
  componentEnabled,
  enabledComponents,
  privateDirectory,
  readSetup,
  type Component,
  type Setup,
} from './config';
import { assertComponentRuntime, assertRuntimeArtifacts, runtimeVersion } from './release-state';

export const COMPONENTS: readonly Component[] = ['hub', 'collector'];

function installationHash(dir: string): string {
  return createHash('sha256').update(dir).digest('hex').slice(0, 12);
}

/**
 * The unqualified name is the legacy combined supervisor. Component units are
 * independent so one component can be replaced without restarting the other.
 */
export function unitName(dir: string, component?: Component): string {
  const hash = installationHash(dir);
  return component ? `rigscope-${hash}-${component}.service` : `rigscope-${hash}.service`;
}
export function unitPath(dir: string, component?: Component): string {
  return join(process.env.XDG_CONFIG_HOME || join(homedir(), '.config'), 'systemd', 'user', unitName(dir, component));
}
function unitDirectory(): string {
  return join(process.env.XDG_CONFIG_HOME || join(homedir(), '.config'), 'systemd', 'user');
}
function runtimeTarget(dir: string): string {
  return join(dir, 'runtime', 'node_modules', 'rigscope', 'bin', 'rigscope.cjs');
}

function runSystemctl(unit: string | null, command: string, quiet = false): void {
  const args = ['--user', command, ...(unit ? [unit] : [])];
  execFileSync('systemctl', args, {stdio: quiet ? 'pipe' : 'inherit'});
}

function readConfig(dir: string): Setup | null {
  try { return readSetup(dir); }
  catch { return null; }
}

function fallbackComponents(dir: string): Component[] {
  const config = readConfig(dir);
  if (!config) return [...COMPONENTS];
  try { return enabledComponents(config); }
  catch { return [...COMPONENTS]; }
}

/** Units selected by an omitted operation: only the enabled components. */
function selectedUnits(dir: string, component?: Component): Array<{component: Component; unit: string}> {
  if (component) return [{component, unit: unitName(dir, component)}];
  return fallbackComponents(dir).map(selected => ({component: selected, unit: unitName(dir, selected)}));
}

/**
 * Run one systemctl command for one component, or aggregate every enabled
 * component when no component is given. Explicit commands never touch the
 * other component's unit.
 */
export function systemctl(dir: string, command: string, quiet = false, component?: Component): void {
  if (command === 'daemon-reload') {
    try { runSystemctl(null, command, quiet); }
    catch { throw new Error('Could not reload the user service manager. Check systemctl --user daemon-reload.'); }
    return;
  }
  if (['start', 'stop', 'restart', 'enable', 'disable'].includes(command) && existsSync(legacyUnitPath(dir))) {
    // A lifecycle command is an explicit mutation, so this is a safe point to
    // convert the recognized combined unit. Read-only status never reaches it.
    migrateLegacyService(dir);
  }
  const targets = selectedUnits(dir, component);
  if (targets.length === 0) return;
  const failures: string[] = [];
  for (const target of targets) {
    try { runSystemctl(target.unit, command, quiet); }
    catch { failures.push(target.unit); }
  }
  if (failures.length > 0) {
    throw new Error(`Could not ${command} the user service. Check systemctl --user status ${failures.join(' ')}.`);
  }
}

function unitEnabled(unit: string): boolean {
  try {
    return execFileSync('systemctl', ['--user', 'is-enabled', unit], {encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 5_000}).trim() === 'enabled';
  } catch { return false; }
}

function inspectedUnit(unit: string, installed: boolean, component?: Component): ServiceStatus {
  try {
    const output = execFileSync('systemctl', ['--user', 'show', unit, '--no-page', '--property=ActiveState,SubState,ExecMainPID,UnitFileState'], {
      encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 5_000,
    });
    const values = new Map<string, string>();
    for (const line of output.split('\n')) {
      const index = line.indexOf('=');
      if (index > 0) values.set(line.slice(0, index), line.slice(index + 1).trim());
    }
    const activeState = values.get('ActiveState') ?? '';
    const pid = Number(values.get('ExecMainPID') ?? '0');
    const unitFileState = values.get('UnitFileState') ?? '';
    return {
      unit,
      installed,
      active: activeState === 'active' || activeState === 'reloading',
      enabled: unitFileState === 'enabled' || unitFileState === 'enabled-runtime',
      subState: values.get('SubState') ?? activeState,
      execMainPid: Number.isSafeInteger(pid) && pid > 0 ? pid : null,
      error: null,
      ...(component ? {component} : {}),
    };
  } catch (error) {
    return {
      unit,
      installed,
      active: false,
      enabled: installed ? unitEnabled(unit) : false,
      subState: 'unknown',
      execMainPid: null,
      error: error instanceof Error ? error.message : String(error),
      ...(component ? {component} : {}),
    };
  }
}

function legacyUnitPath(dir: string): string {
  return unitPath(dir);
}

function legacyUnitStatus(dir: string, component: Component): ServiceStatus | null {
  const legacy = legacyUnitPath(dir);
  if (!existsSync(legacy)) return null;
  const config = readConfig(dir);
  const covers = config ? componentEnabled(config, component) : true;
  const status = inspectedUnit(unitName(dir), true);
  return {
    ...status,
    installed: true,
    active: covers && status.active,
    enabled: covers && status.enabled,
    subState: covers ? status.subState : 'disabled',
    execMainPid: covers ? status.execMainPid : null,
    component,
    legacy: true,
  };
}

export function serviceActive(dir: string, component?: Component): boolean {
  return serviceStatus(dir, component).active;
}

export type ServiceStatus = {
  unit: string;
  installed: boolean;
  active: boolean;
  /** systemd is-enabled state, when the platform reports it. */
  enabled?: boolean;
  subState: string;
  execMainPid: number | null;
  error: string | null;
  component?: Component;
  /** True when this state is represented by the old combined unit. */
  legacy?: boolean;
  components?: Partial<Record<Component, ServiceStatus>>;
};

/**
 * Read-only service state. systemd is optional: a foreground installation or
 * a test environment without a user manager reports inactive instead of
 * crashing the command. Omitted component aggregates enabled components; it
 * never migrates or rewrites units.
 */
export function serviceStatus(dir: string, component?: Component): ServiceStatus {
  if (component) {
    const path = unitPath(dir, component);
    const status = inspectedUnit(unitName(dir, component), existsSync(path), component);
    return status.installed ? status : legacyUnitStatus(dir, component) ?? status;
  }
  const components: Partial<Record<Component, ServiceStatus>> = {};
  for (const selected of COMPONENTS) components[selected] = serviceStatus(dir, selected);
  const enabledComponentsList = fallbackComponents(dir);
  const activeStatuses = enabledComponentsList.map(selected => components[selected]!).filter(Boolean);
  if (activeStatuses.length === 0) {
    return {
      unit: unitName(dir),
      installed: false,
      active: false,
      enabled: false,
      subState: 'disabled',
      execMainPid: null,
      error: null,
      components,
    };
  }
  const legacyOnly = activeStatuses.every(status => status.legacy === true);
  const unit = legacyOnly ? unitName(dir) : activeStatuses.map(status => status.unit).join(' ');
  const errors = activeStatuses.map(status => status.error).filter((value): value is string => value !== null);
  return {
    unit,
    installed: activeStatuses.every(status => status.installed),
    active: activeStatuses.some(status => status.active),
    enabled: activeStatuses.every(status => status.enabled === true),
    subState: activeStatuses.every(status => status.active)
      ? 'active'
      : activeStatuses.some(status => status.active) ? 'partial' : activeStatuses[0]?.subState ?? 'inactive',
    execMainPid: activeStatuses.find(status => status.execMainPid !== null)?.execMainPid ?? null,
    error: errors.length > 0 ? errors.join('; ') : null,
    components,
  };
}

function quoted(value: string): string {
  if (/[\r\n\0]/.test(value)) throw new Error('Service paths must not contain control characters.');
  return `"${value.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/%/g, '%%').replace(/\$/g, '$$')}"`;
}

function ownershipMarker(dir: string): string {
  return `# Rigscope installation: ${createHash('sha256').update(dir).digest('hex')}`;
}

function unitOwned(path: string, dir: string): boolean {
  try {
    const stat = lstatSync(path);
    return stat.isFile() && stat.uid === process.getuid?.() && readFileSync(path, 'utf8').includes(runtimeTarget(dir));
  } catch { return false; }
}

function assertUnitReplaceable(path: string, dir: string): void {
  if (existsSync(path) && !unitOwned(path, dir)) {
    throw new Error('Refusing to replace an unrecognized service file; inspect it first.');
  }
}

function componentUnit(dir: string, component: Component): string {
  const label = component === 'hub' ? 'hub' : 'collector';
  return `${ownershipMarker(dir)}\n[Unit]\nDescription=Rigscope ${label}\nAfter=network.target\n\n[Service]\nType=simple\nExecStart=${quoted(process.execPath)} ${quoted(runtimeTarget(dir))} run ${component} --state-dir ${quoted(dir)}\nRestart=on-failure\nRestartSec=5\nUMask=0077\nKillMode=control-group\nTimeoutStopSec=10\n\n[Install]\nWantedBy=default.target\n`;
}

function writeComponentUnit(dir: string, component: Component): void {
  const path = unitPath(dir, component);
  assertUnitReplaceable(path, dir);
  mkdirSync(unitDirectory(), {recursive: true});
  writeFileSync(path, componentUnit(dir, component), {mode: 0o600});
}

/**
 * Convert a recognized legacy combined unit into independent component units.
 * This is a lifecycle mutation: read-only status never calls it. The old
 * supervisor is stopped before any component starts, so migration cannot
 * leave duplicate hub/collector processes. Prior activity and startup flags
 * are applied to the enabled components.
 */
export function migrateLegacyService(dir: string): boolean {
  const legacy = legacyUnitPath(dir);
  if (!existsSync(legacy)) return false;
  if (!unitOwned(legacy, dir)) {
    throw new Error('Refusing to replace an unrecognized service file; inspect it first.');
  }
  // Older runtimes cannot parse `run hub|collector`. Keep the working
  // supervisor intact until its existing CLI has upgraded the package.
  assertComponentRuntime(join(dir, 'runtime'));
  // Validate every destination before removing the working legacy unit.
  for (const component of COMPONENTS) assertUnitReplaceable(unitPath(dir, component), dir);
  const previous = inspectedUnit(unitName(dir), true);
  if (previous.active) {
    try { runSystemctl(unitName(dir), 'stop', true); }
    catch { throw new Error(`Could not stop the legacy user service. Check systemctl --user status ${unitName(dir)}.`); }
  }
  if (previous.enabled) {
    try { runSystemctl(unitName(dir), 'disable', true); }
    catch { throw new Error(`Could not disable the legacy user service. Check systemctl --user status ${unitName(dir)}.`); }
  }
  rmSync(legacy);
  runSystemctl(null, 'daemon-reload', true);
  for (const component of COMPONENTS) writeComponentUnit(dir, component);
  runSystemctl(null, 'daemon-reload', true);
  const enabled = fallbackComponents(dir);
  for (const component of enabled) {
    runSystemctl(unitName(dir, component), previous.enabled ? 'enable' : 'disable', true);
  }
  if (previous.active) {
    for (const component of enabled) runSystemctl(unitName(dir, component), 'start', true);
  }
  return true;
}

/**
 * Write component units. With no component, both units are written because one
 * package always contains the complete hub+collector runtime; enabled state is
 * what selects behavior. An explicit component writes only that unit.
 */
export function writeService(dir: string, component?: Component): void {
  const config = readSetup(dir);
  if (!config) throw new Error('No installation found. Run rigscope install first.');
  migrateLegacyService(dir);
  for (const target of component ? [component] : COMPONENTS) writeComponentUnit(dir, target);
  runSystemctl(null, 'daemon-reload');
}

function shellQuote(value: string): string {
  return "'" + value.replace(/'/g, "'\\''") + "'";
}

function aliasMarker(dir: string): string {
  return `# Rigscope installation: ${createHash('sha256').update(dir).digest('hex')}`;
}

export function commandLauncher(dir: string, nodePath = process.execPath): string {
  const target = runtimeTarget(dir);
  return `#!/bin/sh\n${aliasMarker(dir)}\nexport RIGSCOPE_STATE_DIR=\${RIGSCOPE_STATE_DIR:-${shellQuote(dir)}}\nexec ${shellQuote(nodePath)} ${shellQuote(target)} "$@"\n`;
}

/** Pin the same Node and durable runtime used by the service, independent of PATH. */
export function installAlias(dir: string, sourceRoot?: string): string {
  const bin = process.env.XDG_BIN_HOME || join(homedir(), '.local', 'bin');
  mkdirSync(bin, {recursive: true});
  const alias = join(bin, 'rigscope');
  const target = runtimeTarget(dir);
  try {
    const stat = lstatSync(alias);
    const ours = stat.uid === process.getuid?.() && (
      (stat.isSymbolicLink() && [target, sourceRoot && join(sourceRoot, 'bin', 'rigscope.cjs')].includes(readlinkSync(alias))) ||
      (stat.isFile() && readFileSync(alias, 'utf8').split('\n')[1] === aliasMarker(dir))
    );
    if (!ours) {
      console.log(`Keeping existing command at ${alias}.`);
      return target;
    }
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
  }
  const temporary = `${alias}.${process.pid}.tmp`;
  try {
    writeFileSync(temporary, commandLauncher(dir), {mode: 0o755, flag: 'wx'});
    renameSync(temporary, alias);
  } finally { rmSync(temporary, {force: true}); }
  return alias;
}

export type StartupStatus = {
  enabled: boolean;
  linger: boolean;
  legacy?: boolean;
  components?: Partial<Record<Component, {enabled: boolean; legacy?: boolean}>>;
};

function componentStartup(dir: string, component: Component): {enabled: boolean; legacy?: boolean} {
  if (existsSync(unitPath(dir, component))) return {enabled: unitEnabled(unitName(dir, component))};
  if (existsSync(legacyUnitPath(dir))) {
    const config = readConfig(dir);
    const covers = config ? componentEnabled(config, component) : true;
    return {enabled: covers && unitEnabled(unitName(dir)), legacy: true};
  }
  return {enabled: false};
}

export function startupStatus(dir: string, component?: Component): StartupStatus {
  let linger = false;
  try {
    linger = execFileSync('loginctl', ['show-user', String(process.getuid?.()), '--property=Linger', '--value'], {encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 5_000}).trim() === 'yes';
  } catch { /* no login manager */ }
  if (component) return {...componentStartup(dir, component), linger};
  const components: Partial<Record<Component, {enabled: boolean; legacy?: boolean}>> = {};
  for (const selected of COMPONENTS) components[selected] = componentStartup(dir, selected);
  const enabledComponentsList = fallbackComponents(dir);
  const selectedStatuses = enabledComponentsList.map(selected => components[selected]!);
  return {
    enabled: selectedStatuses.length > 0 && selectedStatuses.every(status => status.enabled),
    linger,
    components,
  };
}

export function enableBootStartup(): void {
  try {
    execFileSync('loginctl', ['enable-linger', String(process.getuid?.())], {stdio: 'inherit'});
  } catch {
    throw new Error('Could not enable boot startup. Run loginctl enable-linger "$USER", then rigscope start.');
  }
}

export function showLogs(dir: string, follow = false, component?: Component): void {
  const targets = component ? [component] : fallbackComponents(dir);
  const componentUnits = targets.map(target => unitName(dir, target));
  // Before an explicit migration the legacy combined unit is still the only
  // source of logs; reading them must not rewrite anything.
  const selected = !component && componentUnits.every(unit => !existsSync(join(unitDirectory(), unit))) && existsSync(legacyUnitPath(dir))
    ? [unitName(dir)]
    : componentUnits.length > 0 ? componentUnits : [unitName(dir)];
  execFileSync('journalctl', ['--user', ...selected.flatMap(unit => ['--unit', unit]), '--lines=100', '--no-pager', ...(follow ? ['--follow'] : [])], {stdio: 'inherit'});
}

/**
 * Remove every unit this installation owns, the launcher and installed runtime
 * trees. Configuration, history, backups and enrollment state are retained.
 */
export function uninstallService(dir: string): void {
  const units = [...COMPONENTS.map(component => unitPath(dir, component)), legacyUnitPath(dir)];
  const present = [...new Set(units)].filter(path => existsSync(path));
  for (const path of present) {
    if (!unitOwned(path, dir)) throw new Error('Refusing to remove an unrecognized service file; inspect it first.');
  }
  const removed: string[] = [];
  for (const path of present) {
    const unit = path.slice(path.lastIndexOf('/') + 1);
    try { runSystemctl(unit, 'stop', false); } catch { throw new Error(`Could not stop the user service. Check systemctl --user status ${unit}.`); }
    try { runSystemctl(unit, 'disable', false); } catch { throw new Error(`Could not disable the user service. Check systemctl --user status ${unit}.`); }
    rmSync(path);
    removed.push(unit);
  }
  if (removed.length > 0) runSystemctl(null, 'daemon-reload', false);
  const alias = join(process.env.XDG_BIN_HOME || join(homedir(), '.local', 'bin'), 'rigscope');
  const target = runtimeTarget(dir);
  try {
    const stat = lstatSync(alias);
    if (stat.uid === process.getuid?.() && (
      (stat.isSymbolicLink() && readlinkSync(alias) === target) ||
      (stat.isFile() && readFileSync(alias, 'utf8').split('\n')[1] === aliasMarker(dir))
    )) rmSync(alias);
  } catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
  for (const tree of ['runtime', 'runtime-previous', 'runtime-next', 'runtime-rollback-tmp']) {
    rmSync(join(dir, tree), {recursive: true, force: true});
  }
}
export function stagePackage(root: string, dir: string, from?: string): string {
  const stage = join(dir, 'runtime-next');
  if (existsSync(stage)) throw new Error(`A staged runtime exists at ${stage}; inspect it before retrying.`);
  privateDirectory(stage);
  const temporary = mkdtempSync(join(tmpdir(), 'rigscope-package-'));
  try {
    let source = from;
    if (!source) {
      const output = execFileSync('npm', ['pack', root, '--pack-destination', temporary, '--ignore-scripts', '--json'], {encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit']});
      const packed = JSON.parse(output) as {filename: string}[] | Record<string, {filename: string}>;
      const first = Array.isArray(packed) ? packed[0] : Object.values(packed)[0];
      if (!first?.filename) throw new Error('npm pack did not return a package filename.');
      source = join(temporary, first.filename);
    }
    execFileSync('npm', ['install', '--prefix', stage, '--omit=dev', '--ignore-scripts', '--no-audit', '--no-fund', '--', source], {stdio: 'inherit'});
    assertRuntimeArtifacts(stage, 'staged package');
    runtimeVersion(stage);
    return stage;
  } catch (error) { rmSync(stage, {recursive: true, force: true}); throw error; }
  finally { rmSync(temporary, {recursive: true, force: true}); }
}
export function activatePackage(dir: string, stage: string): void {
  const runtime = join(dir, 'runtime');
  const previous = join(dir, 'runtime-previous');
  assertRuntimeArtifacts(stage, 'staged runtime');
  runtimeVersion(stage);
  if (existsSync(previous)) rmSync(previous, {recursive: true, force: true});
  if (existsSync(runtime)) renameSync(runtime, previous);
  try { renameSync(stage, runtime); }
  catch (error) { if (existsSync(previous) && !existsSync(runtime)) renameSync(previous, runtime); throw error; }
}
/** Swap active and previous runtimes without deleting either tree. */
export function swapRuntimes(dir: string): void {
  const runtime = join(dir, 'runtime');
  const previous = join(dir, 'runtime-previous');
  const temporary = join(dir, 'runtime-rollback-tmp');
  if (!existsSync(runtime)) throw new Error('No active runtime is installed.');
  if (!existsSync(previous)) throw new Error('No previous runtime is available for rollback.');
  rmSync(temporary, { recursive: true, force: true });
  renameSync(runtime, temporary);
  try {
    renameSync(previous, runtime);
    renameSync(temporary, previous);
  } catch (error) {
    if (!existsSync(runtime) && existsSync(temporary)) renameSync(temporary, runtime);
    throw error;
  }
}
