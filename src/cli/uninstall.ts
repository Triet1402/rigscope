/**
 * Safe removal of a user installation.
 *
 * Uninstall purges by default. `purge = false` is the explicit "keep data"
 * path: services, the command launcher, runtime trees and ephemeral run state
 * are removed while configuration, history, credentials and backups remain.
 *
 * Destructive work is gated by a recognized, owned, private installation
 * directory. Missing directories are already uninstalled and are a safe no-op
 * apart from cleaning up this installation's service units and launcher.
 * Foreground runtimes are identified through their leases and stopped through
 * the same coordinator used by reset, rather than reporting a manual-stop
 * error. Ambiguous, foreign or unrecognized leases still fail closed.
 */

import { lstatSync, readFileSync, realpathSync, rmSync, unlinkSync, type Stats } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join, resolve, sep } from 'node:path';
import { acquireProcessLease } from '../runtime/process-lease';
import { readSetup, type Setup } from './config';
import { databasePath } from './release-state';
import { runningOwnedComponents, stopOwnedInstallationProcesses } from './reset';
import { uninstallService } from './services';

const MAX_LOCK_BYTES = 1024;

export type UninstallDependencies = {
  /** Stops foreground processes whose lease and identity belong to `dir`. */
  stopProcesses?: (dir: string) => void;
  /** Stops owned units, removes the launcher, and removes runtime trees. */
  uninstallServices?: (dir: string, keepRuntime?: boolean) => void;
  acquireLease?: typeof acquireProcessLease;
  remove?: typeof rmSync;
  readConfig?: typeof readSetup;
  homeDirectory?: () => string;
};

function entry(path: string): Stats | null {
  try {
    return lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw error;
  }
}

function canonicalDirectory(path: string): string {
  try {
    return realpathSync(path);
  } catch {
    return resolve(path);
  }
}

function homePath(read: () => string): string {
  return canonicalDirectory(read());
}

/**
 * Never remove a filesystem root, a home directory, any ancestor of a home
 * directory, or a bare top-level directory such as /tmp or /var. A real
 * installation always lives below one of those.
 */
function assertNotProtected(candidate: string, home: string): void {
  const root = resolve(sep);
  const protectedPaths = new Set([root, home, dirname(home)]);
  if (protectedPaths.has(candidate) ||
      (home !== root && home.startsWith(candidate.endsWith(sep) ? candidate : `${candidate}${sep}`)) ||
      dirname(candidate) === root) {
    throw new Error(`Refusing to uninstall a protected system or home path: ${candidate}`);
  }
}

function processLocks(dir: string): string[] {
  return [
    join(dir, 'run', 'operation.lock'),
    join(dir, 'run', 'hub.lock'),
    join(dir, 'run', 'collector.lock'),
    join(dir, 'run', 'rigscope.lock'),
    join(dir, 'collector', 'collector-runtime.lock'),
    join(dir, 'collector', 'state.json.lock'),
    `${databasePath(dir)}.hub.lock`,
  ];
}

/** Existing lock locations only; removal must not create new private folders. */
function existingLocks(dir: string): string[] {
  return processLocks(dir).filter(lock => {
    const parent = entry(dirname(lock));
    return parent !== null && parent.isDirectory();
  });
}

function leasePid(path: string): number | null {
  let stat: Stats;
  try {
    stat = lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw error;
  }
  if (!stat.isFile() || stat.isSymbolicLink() || (stat.mode & 0o077) !== 0) {
    throw new Error(`Refusing to uninstall: runtime lease is not a private regular file: ${path}`);
  }
  if (stat.size > MAX_LOCK_BYTES) {
    throw new Error(`Refusing to uninstall: runtime lease is oversized: ${path}`);
  }
  let owner: unknown;
  try {
    owner = JSON.parse(readFileSync(path, 'utf8'));
  } catch {
    throw new Error(`Refusing to uninstall: runtime lease is incomplete: ${path}`);
  }
  const pid = (owner as {pid?: unknown} | null)?.pid;
  if (!Number.isSafeInteger(pid) || (pid as number) < 1) {
    throw new Error(`Refusing to uninstall: runtime lease has no valid owner PID: ${path}`);
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
    throw error;
  }
  // A zombie has exited even while kill(pid, 0) still succeeds.
  const state = processState(pid);
  return state !== 'Z' && state !== 'X';
}

/**
 * A stopped foreground process cannot release its own lease if it was killed,
 * and its zombie may answer kill(pid, 0) until reaped. Remove only leases that
 * are proven stale, checking the inode again so a competing owner's fresh lock
 * is preserved.
 */
function clearStoppedLeases(dir: string): void {
  for (const path of existingLocks(dir)) {
    let pid: number | null;
    try {
      pid = leasePid(path);
    } catch {
      // Leave malformed or unsafe leases for the acquisition below to reject.
      continue;
    }
    if (pid === null || isAlive(pid)) continue;
    let before: Stats;
    try {
      before = lstatSync(path);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') continue;
      throw error;
    }
    let after: Stats;
    try {
      after = lstatSync(path);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') continue;
      throw error;
    }
    if (before.ino !== after.ino || before.dev !== after.dev || leasePid(path) !== pid) continue;
    try {
      unlinkSync(path);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
    }
  }
}

/** Hold every existing runtime lease while destructive work runs. */
function acquireUninstallLeases(
  dir: string,
  acquireLease: typeof acquireProcessLease,
): () => void {
  const releases: Array<() => void> = [];
  try {
    for (const path of existingLocks(dir)) releases.push(acquireLease(path));
  } catch (error) {
    for (const release of releases.reverse()) {
      try { release(); } catch { /* preserve the acquisition failure */ }
    }
    throw new Error(
      `Another runtime still owns this installation: ${error instanceof Error ? error.message : 'lease busy'}. Stop it before uninstalling.`,
    );
  }
  return () => {
    for (const release of releases.reverse()) {
      try { release(); } catch { /* leases are advisory cleanup */ }
    }
  };
}

/**
 * Remove a validated state directory. Re-checking the inode immediately before
 * deletion prevents a path swap between validation and removal from deleting
 * an unrelated directory.
 */
function removeValidatedDirectory(
  path: string,
  expected: Stats,
  remove: typeof rmSync,
): void {
  let current: Stats;
  try {
    current = lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return;
    throw error;
  }
  if (!current.isDirectory() || current.isSymbolicLink() ||
      current.dev !== expected.dev || current.ino !== expected.ino) {
    throw new Error(`Refusing to purge: the state directory changed while uninstalling: ${path}`);
  }
  remove(path, {recursive: true, force: true});
}

/**
 * Remove this installation's services, launcher and runtimes, or purge all of
 * its state. Directory validation happens before any service, process or file
 * is changed, so a typo or an unrelated directory cannot become destructive.
 */
export function uninstallInstallation(
  dir: string,
  purge = true,
  injected: UninstallDependencies = {},
): void {
  const stopProcesses = injected.stopProcesses ?? stopOwnedInstallationProcesses;
  const uninstallServices = injected.uninstallServices ?? uninstallService;
  const acquireLease = injected.acquireLease ?? acquireProcessLease;
  const remove = injected.remove ?? rmSync;
  const readConfig = injected.readConfig ?? readSetup;
  const home = homePath(injected.homeDirectory ?? homedir);

  if (typeof dir !== 'string' || dir.trim() === '') {
    throw new Error('Refusing to uninstall: choose a state directory.');
  }

  const target = resolve(dir);
  const stat = entry(target);

  // A missing directory is already uninstalled. Cleaning owned units and the
  // launcher is still idempotent and safe, but there is no state to delete.
  if (stat === null) {
    uninstallServices(dir);
    return;
  }

  if (stat.isSymbolicLink()) {
    throw new Error(`Refusing to uninstall a symbolic link: ${target}`);
  }
  if (!stat.isDirectory()) {
    throw new Error(`Refusing to uninstall: state path is not a directory: ${target}`);
  }

  // Deletion follows the real path so a symlinked parent cannot redirect the
  // operation after validation.
  const canonical = canonicalDirectory(target);
  assertNotProtected(canonical, home);

  if (stat.uid !== process.getuid?.() || (stat.mode & 0o077) !== 0) {
    throw new Error(`Refusing to purge: state directory must be an owned private (0700) installation: ${canonical}`);
  }

  let config: Setup | null;
  try {
    config = readConfig(canonical);
  } catch (error) {
    throw new Error(`Refusing to uninstall: the saved configuration could not be verified: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (!config) {
    throw new Error(`Refusing to uninstall a directory without a saved Rigscope configuration: ${canonical}`);
  }

  // Identify and stop foreground runtimes before units/runtime trees change.
  // The coordinator refuses foreign or unidentifiable leases, so a failure
  // here leaves the installation untouched.
  runningOwnedComponents(dir);
  uninstallServices(dir, true);
  stopProcesses(dir);
  clearStoppedLeases(dir);

  const release = acquireUninstallLeases(dir, acquireLease);
  try {
    if (purge) {
      removeValidatedDirectory(canonical, stat, remove);
    } else {
      uninstallServices(dir);
      // Runtime, service and ephemeral run artifacts are gone; configuration,
      // history, credentials, enrollment state and backups remain.
      remove(join(canonical, 'run'), {recursive: true, force: true});
    }
  } finally {
    release();
  }
}
