/**
 * Coordinated reset of an incompatible user installation.
 *
 * A reset is destructive, so it only runs after the replacement package has
 * been staged and validated and the fresh-setup options have been validated.
 * It then stops every service unit and identified foreground process, holds
 * the runtime leases, removes the unusable state, activates the replacement
 * runtime and creates the canonical configuration from scratch.
 *
 * Reset is never part of readSetup() or initialize(): those paths must not
 * delete live state as a side effect.
 */

import { lstatSync, readFileSync, rmSync, unlinkSync } from 'node:fs';
import { basename, join, resolve } from 'node:path';
import { OperationsError, errorMessage, writePrivateFile } from '../operations/fs';
import { acquireProcessLease } from '../runtime/process-lease';
import { parseHubConfig } from '../hub/config';
import {
  defaultComponents,
  hubEnvironment,
  initialize,
  RESET_PENDING_FILE,
  installationNeedsReset,
  readPrivate,
  validateSetupOptions,
  type Setup,
} from './config';
import { databasePath, assertRuntimeArtifacts, runtimeVersion } from './release-state';
import { activateReplacementRuntime, stopAndRemoveOwnedUnits } from './services';

const MAX_LOCK_BYTES = 1024;
const TERM_GRACE_MS = 12_000;
const KILL_GRACE_MS = 5_000;

export type ResetDependencies = {
  /** Stop and remove this installation's owned units (including historical ones). */
  stopServices?: (dir: string) => void;
  /** Terminate identified foreground processes and wait for them to exit. */
  stopProcesses?: (dir: string) => void;
  acquireLease?: typeof acquireProcessLease;
  /** Activate an already validated replacement runtime tree. */
  activate?: (dir: string, stage: string) => void;
  initialize?: typeof initialize;
};

function processLocks(dir: string): string[] {
  return [
    join(dir, 'run', 'hub.lock'),
    join(dir, 'run', 'collector.lock'),
    join(dir, 'run', 'rigscope.lock'),
    join(dir, 'collector', 'collector-runtime.lock'),
    join(dir, 'collector', 'state.json.lock'),
    `${databasePath(dir)}.hub.lock`,
  ];
}

function readLeasePid(path: string): number | null {
  let stat;
  try { stat = lstatSync(path); }
  catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw new OperationsError('lease-unreadable', `could not inspect runtime lease ${path}: ${errorMessage(error)}`, {cause: error});
  }
  if (!stat.isFile() || stat.isSymbolicLink() || (stat.mode & 0o077) !== 0) {
    throw new OperationsError('lease-unsafe', `runtime lease is not a private regular file: ${path}; inspect it before resetting`);
  }
  if (stat.size > MAX_LOCK_BYTES) {
    throw new OperationsError('lease-incomplete', `runtime lease is oversized: ${path}; inspect it before resetting`);
  }
  let raw: string;
  try { raw = readPrivate(path); }
  catch (error) {
    throw new OperationsError('lease-unreadable', `could not read runtime lease ${path}: ${errorMessage(error)}`, {cause: error});
  }
  let owner: unknown;
  try { owner = JSON.parse(raw); }
  catch {
    throw new OperationsError('lease-incomplete', `runtime lease is incomplete: ${path}; inspect it before resetting`);
  }
  const pid = (owner as {pid?: unknown} | null)?.pid;
  if (!Number.isSafeInteger(pid) || (pid as number) < 1) {
    throw new OperationsError('lease-invalid', `runtime lease has no valid owner PID: ${path}; inspect it before resetting`);
  }
  return pid as number;
}

function processState(pid: number): string | null {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, 'utf8');
    const end = stat.lastIndexOf(')');
    return end < 0 ? null : (stat.slice(end + 2).split(' ')[0] ?? null);
  } catch {
    return null;
  }
}

function isAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === 'ESRCH') return false;
    if (code === 'EPERM' || code === 'EACCES') return true;
    throw new OperationsError('process-unverifiable', `could not verify process ${pid}: ${errorMessage(error)}`, {cause: error});
  }
  // A zombie has already exited; its parent (possibly a synchronous test
  // harness) has not reaped it yet, so kill(pid, 0) still succeeds.
  const state = processState(pid);
  return state !== 'Z' && state !== 'X';
}

function processUid(pid: number): number | null {
  try {
    const status = readFileSync(`/proc/${pid}/status`, 'utf8');
    const match = /^Uid:\s+(\d+)/m.exec(status);
    return match ? Number(match[1]) : null;
  } catch {
    return null;
  }
}

function processEnvironment(pid: number): string[] {
  try {
    return readFileSync(`/proc/${pid}/environ`, 'utf8').split('\0').filter(Boolean);
  } catch {
    return [];
  }
}

function processCommandLine(pid: number): string[] {
  try {
    return readFileSync(`/proc/${pid}/cmdline`, 'utf8').split('\0').filter(Boolean);
  } catch {
    return [];
  }
}

/**
 * Ownership proof for a PID named by a lease. A live PID alone is not enough:
 * a stale lock can name a PID that the kernel has reused for an unrelated
 * process, so the command line or installation-specific environment must also
 * point at this state directory.
 */
function assertProcessOwned(dir: string, pid: number, lease: string): void {
  const uid = processUid(pid);
  if (uid === null) {
    throw new OperationsError(
      'process-unverifiable',
      `runtime lease ${lease} names PID ${pid}, but that process could not be inspected; stop it manually before resetting`,
    );
  }
  if (uid !== process.getuid?.()) {
    throw new OperationsError(
      'process-foreign',
      `runtime lease ${lease} names PID ${pid}, which is not owned by this user; stop it manually before resetting`,
    );
  }
  const target = resolve(dir);
  const belongsToInstallation = (value: string): boolean => value === target || value.startsWith(`${target}/`);
  if (processCommandLine(pid).some(value => belongsToInstallation(value) || value === `--state-dir=${target}`)) return;
  const stateVariables = new Set([
    'RIGSCOPE_STATE_DIR', 'RIGSCOPE_HUB_LOCK_FILE', 'HUB_STATE_FILE',
    'HUB_DATABASE_PATH', 'COLLECTOR_STATE_FILE', 'COLLECTOR_HEALTH_FILE',
  ]);
  for (const entry of processEnvironment(pid)) {
    const index = entry.indexOf('=');
    if (index < 0) continue;
    if (!stateVariables.has(entry.slice(0, index))) continue;
    const value = resolve(entry.slice(index + 1));
    if (belongsToInstallation(value)) return;
  }
  throw new OperationsError(
    'process-unidentified',
    `runtime lease ${lease} names live PID ${pid}, but its identity does not reference ${target}; refusing to signal an unrelated process`,
  );
}

/** PIDs that a live lease claims for this installation and whose identity matches. */
function identifiedProcesses(dir: string, components?: Array<'hub' | 'collector'>): number[] {
  const pids = new Set<number>();
  for (const lease of processLocks(dir)) {
    const component = ['collector.lock', 'collector-runtime.lock', 'state.json.lock'].includes(basename(lease)) ? 'collector' : 'hub';
    if (components && !lease.endsWith('rigscope.lock') && !components.includes(component)) continue;
    const pid = readLeasePid(lease);
    if (pid === null || !isAlive(pid)) continue;
    if (pid === process.pid) {
      throw new OperationsError(
        'process-self',
        `runtime lease ${lease} is held by this command; retry after the current operation finishes`,
      );
    }
    assertProcessOwned(dir, pid, lease);
    pids.add(pid);
  }
  return [...pids];
}

function waitForExit(pid: number, deadlineMs: number): boolean {
  const deadline = Date.now() + deadlineMs;
  const pause = new Int32Array(new SharedArrayBuffer(4));
  while (Date.now() < deadline) {
    if (!isAlive(pid)) return true;
    Atomics.wait(pause, 0, 0, 50);
  }
  return !isAlive(pid);
}

function terminate(pid: number): void {
  try { process.kill(pid, 'SIGTERM'); }
  catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ESRCH') return;
    throw new OperationsError('process-stop-failed', `could not stop process ${pid}: ${errorMessage(error)}`, {cause: error});
  }
  if (waitForExit(pid, TERM_GRACE_MS)) return;
  try { process.kill(pid, 'SIGKILL'); }
  catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ESRCH') return;
    throw new OperationsError('process-stop-failed', `could not force-stop process ${pid}: ${errorMessage(error)}`, {cause: error});
  }
  if (!waitForExit(pid, KILL_GRACE_MS)) {
    throw new OperationsError('process-stuck', `process ${pid} did not stop; refusing to delete its state`);
  }
}

/** Inspect live leases and prove ownership before any prompt or signal. */
export function runningOwnedComponents(dir: string): Array<'hub' | 'collector'> {
  identifiedProcesses(dir);
  const components = new Set<'hub' | 'collector'>();
  for (const path of processLocks(dir)) {
    const pid = readLeasePid(path);
    if (pid === null || !isAlive(pid)) continue;
    if (path.endsWith('rigscope.lock')) { components.add('hub'); components.add('collector'); }
    else components.add(['collector.lock', 'collector-runtime.lock', 'state.json.lock'].includes(basename(path)) ? 'collector' : 'hub');
  }
  return [...components];
}

export function stopOwnedRuntimes(dir: string, components?: Array<'hub' | 'collector'>): void {
  for (const pid of identifiedProcesses(dir, components)) terminate(pid);
  clearStoppedLeases(dir);
}

/** Stop every foreground process whose lease and identity belong to this installation. */
export function stopOwnedInstallationProcesses(dir: string): void {
  for (const pid of identifiedProcesses(dir)) terminate(pid);
}

/**
 * Remove leases left by processes this reset has already stopped. A killed
 * process cannot run its own lease cleanup, and its zombie may still answer
 * kill(pid, 0) until reaped. The inode is checked again before unlinking so a
 * lease that a competing owner replaced is preserved.
 */
function clearStoppedLeases(dir: string): void {
  for (const path of processLocks(dir)) {
    let pid: number | null;
    try { pid = readLeasePid(path); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') continue;
      throw error;
    }
    if (pid === null || isAlive(pid)) continue;
    let before;
    try { before = lstatSync(path); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') continue;
      throw error;
    }
    if (readLeasePid(path) !== pid) continue;
    let after;
    try { after = lstatSync(path); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') continue;
      throw error;
    }
    if (after.ino !== before.ino || after.dev !== before.dev) continue;
    try { unlinkSync(path); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
    }
  }
}

function acquireLeases(
  dir: string,
  acquireLease: typeof acquireProcessLease,
): {release: () => void; releaseBeforeInitialize: () => void} {
  const paths = [
    join(dir, 'run', 'operation.lock'),
    ...processLocks(dir).filter(path => !path.endsWith('state.json.lock') && !path.endsWith('.hub.lock')),
    join(dir, 'collector', 'state.json.lock'),
    `${databasePath(dir)}.hub.lock`,
  ];
  const releases: Array<() => void> = [];
  try {
    for (const path of paths) releases.push(acquireLease(path));
  } catch (error) {
    for (const release of releases.reverse()) {
      try { release(); } catch { /* preserve the acquisition failure */ }
    }
    throw new OperationsError(
      'runtime-active',
      `another runtime still owns this installation: ${errorMessage(error)}; stop it before resetting`,
      {cause: error},
    );
  }
  let released = false;
  const release = (): void => {
    if (released) return;
    released = true;
    for (const releaseLease of releases.reverse()) {
      try { releaseLease(); } catch { /* leases are advisory cleanup */ }
    }
  };
  // The collector state and database locks would block the fresh initialize
  // that runs after the old state is gone. Everything else stays held.
  const releaseBeforeInitialize = (): void => {
    for (const path of [join(dir, 'collector', 'state.json.lock'), `${databasePath(dir)}.hub.lock`]) {
      const index = paths.indexOf(path);
      if (index < 0) continue;
      try { releases[index](); } catch { /* the run leases still protect the reset */ }
    }
  };
  return {release, releaseBeforeInitialize};
}

function removeIncompatibleState(dir: string): void {
  for (const relative of [
    'config.json',
    'release-state.json',
    'hub',
    'backups',
    join('collector', 'state.json'),
    join('collector', 'outbox'),
    join('run', 'collector-health.json'),
    join('run', 'hub.sock'),
  ]) {
    rmSync(join(dir, relative), {recursive: true, force: true});
  }
}

export type ResetReport = {
  config: Setup;
  password?: string;
  created: boolean;
};

/**
 * Replace an incompatible installation with a fresh canonical one. The caller
 * must pass an already staged replacement; this function validates it again
 * before any destructive step and leaves the original state untouched when
 * validation fails.
 */
export function resetInstallation(
  options: {
    dir: string;
    stage: string;
    setup?: {role?: string; port?: string; origin?: string; socket?: string};
  },
  injected: ResetDependencies = {},
): ResetReport {
  const stopServices = injected.stopServices ?? stopAndRemoveOwnedUnits;
  const stopProcesses = injected.stopProcesses ?? stopOwnedInstallationProcesses;
  const acquireLease = injected.acquireLease ?? acquireProcessLease;
  const activate = injected.activate ?? activateReplacementRuntime;
  const create = injected.initialize ?? initialize;

  // Validate the replacement and the fresh setup before stopping or deleting
  // anything. A rejected package leaves the original installation intact.
  assertRuntimeArtifacts(options.stage, 'replacement runtime');
  runtimeVersion(options.stage);
  const setup = validateSetupOptions(options.setup);
  parseHubConfig(hubEnvironment(options.dir, {
    ...setup, users: [], collectors: [], components: defaultComponents(setup.role),
  }));
  if (!installationNeedsReset(options.dir)) {
    throw new OperationsError(
      'installation-canonical',
      'This installation already uses the canonical configuration; use install or upgrade instead of reset.',
    );
  }

  // Refuse an unidentifiable live lease before removing units, so a safety
  // check cannot leave the installation half-cleaned.
  identifiedProcesses(options.dir);
  stopServices(options.dir);
  stopProcesses(options.dir);
  clearStoppedLeases(options.dir);

  const leases = acquireLeases(options.dir, acquireLease);
  const marker = join(options.dir, RESET_PENDING_FILE);
  const pendingReset = `${JSON.stringify({role: setup.role})}\n`;
  try {
    // Keep the reset detectable even if activation fails after deleting state.
    writePrivateFile(marker, pendingReset);
    removeIncompatibleState(options.dir);
    leases.releaseBeforeInitialize();
    activate(options.dir, options.stage);
    rmSync(marker, {force: true});
    try {
      const result = create(options.dir, options.setup);
      if (!result.created) throw new OperationsError('reset-failed', 'fresh initialization did not create a canonical installation');
      return result;
    } catch (error) {
      writePrivateFile(marker, pendingReset);
      throw error;
    }
  } finally {
    leases.release();
  }
}
