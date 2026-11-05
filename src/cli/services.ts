import { execFileSync } from 'node:child_process';
import { existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, readlinkSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import {
  enabledComponents,
  privateDirectory,
  readSetup,
  type Component,
  type Setup,
} from './config';
import { assertRuntimeArtifacts, runtimeVersion } from './release-state';

export const COMPONENTS: readonly Component[] = ['hub', 'collector'];

function installationHash(dir: string): string {
  return createHash('sha256').update(dir).digest('hex').slice(0, 12);
}

/**
 * Component units are independent so one component can be replaced without
 * restarting the other.
 */
export function unitName(dir: string, component: Component): string {
  return `rigscope-${installationHash(dir)}-${component}.service`;
}
export function unitPath(dir: string, component: Component): string {
  return join(process.env.XDG_CONFIG_HOME || join(homedir(), '.config'), 'systemd', 'user', unitName(dir, component));
}
/**
 * Historical combined supervisor unit. Only cleanup reads or removes this name;
 * current installs always use independent component units.
 */
export function combinedUnitName(dir: string): string {
  return `rigscope-${installationHash(dir)}.service`;
}
export function combinedUnitPath(dir: string): string {
  return join(unitDirectory(), combinedUnitName(dir));
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
  components?: Partial<Record<Component, ServiceStatus>>;
};

/**
 * Read-only service state. systemd is optional: a foreground installation or
 * a test environment without a user manager reports inactive instead of
 * crashing the command. Omitted component aggregates enabled components.
 */
export function serviceStatus(dir: string, component?: Component): ServiceStatus {
  if (component) {
    return inspectedUnit(unitName(dir, component), existsSync(unitPath(dir, component)), component);
  }
  const components: Partial<Record<Component, ServiceStatus>> = {};
  for (const selected of COMPONENTS) components[selected] = serviceStatus(dir, selected);
  const enabledComponentsList = fallbackComponents(dir);
  const activeStatuses = enabledComponentsList.map(selected => components[selected]!).filter(Boolean);
  if (activeStatuses.length === 0) {
    return {
      unit: '',
      installed: false,
      active: false,
      enabled: false,
      subState: 'disabled',
      execMainPid: null,
      error: null,
      components,
    };
  }
  const errors = activeStatuses.map(status => status.error).filter((value): value is string => value !== null);
  return {
    unit: activeStatuses.map(status => status.unit).join(' '),
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
 * Write component units. With no component, both units are written because one
 * package always contains the complete hub+collector runtime; enabled state is
 * what selects behavior. An explicit component writes only that unit.
 */
export function writeService(dir: string, component?: Component): void {
  const config = readSetup(dir);
  if (!config) throw new Error('No installation found. Run rigscope install first.');
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
  components?: Partial<Record<Component, {enabled: boolean}>>;
};

function componentStartup(dir: string, component: Component): {enabled: boolean} {
  if (!existsSync(unitPath(dir, component))) return {enabled: false};
  return {enabled: unitEnabled(unitName(dir, component))};
}

export function startupStatus(dir: string, component?: Component): StartupStatus {
  let linger = false;
  try {
    linger = execFileSync('loginctl', ['show-user', String(process.getuid?.()), '--property=Linger', '--value'], {encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 5_000}).trim() === 'yes';
  } catch { /* no login manager */ }
  if (component) return {...componentStartup(dir, component), linger};
  const components: Partial<Record<Component, {enabled: boolean}>> = {};
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
  if (targets.length === 0) throw new Error('No enabled components. Run rigscope enable hub or rigscope enable collector.');
  const units = targets.map(target => unitName(dir, target));
  execFileSync('journalctl', ['--user', ...units.flatMap(unit => ['--unit', unit]), '--lines=100', '--no-pager', ...(follow ? ['--follow'] : [])], {stdio: 'inherit'});
}

type OwnedUnit = { unit: string; path: string; component?: Component };

function ownedUnits(dir: string): OwnedUnit[] {
  return [
    ...COMPONENTS.map(component => ({
      component,
      unit: unitName(dir, component),
      path: unitPath(dir, component),
    })),
    // The combined supervisor predates independent component units. It is
    // never written again, but cleanup must stop and remove it.
    {unit: combinedUnitName(dir), path: combinedUnitPath(dir)},
  ];
}

/**
 * Stop and remove every unit this installation owns, including the historical
 * combined unit. Every present file is ownership-checked before any is stopped
 * so an unrecognized unit cannot be destroyed by a reset or uninstall.
 */
export function stopAndRemoveOwnedUnits(dir: string): string[] {
  const targets = ownedUnits(dir);
  const present = targets.filter(target => existsSync(target.path));
  for (const target of present) {
    if (!unitOwned(target.path, dir)) throw new Error('Refusing to remove an unrecognized service file; inspect it first.');
  }
  const removed: string[] = [];
  for (const target of present) {
    try { runSystemctl(target.unit, 'stop', false); }
    catch { throw new Error(`Could not stop the user service. Check systemctl --user status ${target.unit}.`); }
    try { runSystemctl(target.unit, 'disable', false); }
    catch { throw new Error(`Could not disable the user service. Check systemctl --user status ${target.unit}.`); }
    rmSync(target.path);
    removed.push(target.unit);
  }
  if (removed.length > 0) runSystemctl(null, 'daemon-reload', false);
  return removed;
}

/**
 * Remove every unit this installation owns, the launcher and installed runtime
 * trees. Configuration, history, backups and enrollment state are retained.
 */
export function uninstallService(dir: string, keepRuntime = false): void {
  stopAndRemoveOwnedUnits(dir);
  const alias = join(process.env.XDG_BIN_HOME || join(homedir(), '.local', 'bin'), 'rigscope');
  const target = runtimeTarget(dir);
  try {
    const stat = lstatSync(alias);
    if (stat.uid === process.getuid?.() && (
      (stat.isSymbolicLink() && readlinkSync(alias) === target) ||
      (stat.isFile() && readFileSync(alias, 'utf8').split('\n')[1] === aliasMarker(dir))
    )) rmSync(alias);
  } catch (error) { if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error; }
  if (keepRuntime) return;
  for (const tree of ['runtime', 'runtime-previous', 'runtime-next', 'runtime-rollback-tmp', 'runtime-remove-tmp']) {
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

/**
 * Activate a validated replacement after an incompatible installation reset.
 * The incompatible runtime is removed rather than kept as a rollback target
 * the old configuration can never use. The staged tree is never deleted before
 * the rename succeeds.
 */
export function activateReplacementRuntime(dir: string, stage: string): void {
  const runtime = join(dir, 'runtime');
  const previous = join(dir, 'runtime-previous');
  const rollbackTemporary = join(dir, 'runtime-rollback-tmp');
  const removalTemporary = join(dir, 'runtime-remove-tmp');
  assertRuntimeArtifacts(stage, 'replacement runtime');
  runtimeVersion(stage);
  rmSync(previous, {recursive: true, force: true});
  rmSync(rollbackTemporary, {recursive: true, force: true});
  rmSync(removalTemporary, {recursive: true, force: true});
  if (existsSync(runtime)) renameSync(runtime, removalTemporary);
  try {
    renameSync(stage, runtime);
  } catch (error) {
    if (!existsSync(runtime) && existsSync(removalTemporary)) renameSync(removalTemporary, runtime);
    throw error;
  }
  rmSync(removalTemporary, {recursive: true, force: true});
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
