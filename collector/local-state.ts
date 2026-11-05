/** Low-frequency installation/enrollment state; never used on the sampling path. */
import { randomUUID } from 'node:crypto';
import { closeSync, fsyncSync, lstatSync, mkdirSync, openSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { homedir } from 'node:os';
import { ConfigError, parseCollectorConfig, validateHubUrl, type DestinationEndpoint } from './config';
import { acquireProcessLease, ProcessLeaseError } from '../src/runtime/process-lease';

export type PendingEnrollment = { url: string; requestId: string; recoverySecret: string };
export type InstallationState = { collectorId: string; destinations: DestinationEndpoint[]; pendingEnrollments?: PendingEnrollment[] };
export function defaultStatePath(): string {
  return join(process.env.XDG_STATE_HOME || join(homedir(), '.local', 'state'), 'rigscope', 'collector.json');
}
function readState(path: string): InstallationState {
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.size > 64 * 1024 || (stat.mode & 0o077) !== 0) throw new Error('collector state must be a private regular file');
  const parsed = JSON.parse(readFileSync(path, 'utf8')) as unknown;
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('invalid collector state');
  const raw = parsed as Record<string, unknown>;
  // An older versioned file is never rewritten into the canonical shape in
  // place: the explicit install/upgrade reset owns that destructive step.
  if (Object.hasOwn(raw, 'version') || Object.hasOwn(raw, 'formatVersion')) {
    throw new Error('collector state uses an older versioned shape; run rigscope install or upgrade to reset it');
  }
  if (typeof raw.collectorId !== 'string' || !/^[\x21-\x7e]{1,128}$/.test(raw.collectorId) || !Array.isArray(raw.destinations)) throw new Error('invalid collector state');
  const destinations = raw.destinations.length === 0 ? [] : [...parseCollectorConfig({ COLLECTOR_HUBS: JSON.stringify(raw.destinations) }).destinations];
  const pending = raw.pendingEnrollments;
  if (pending !== undefined && (!Array.isArray(pending) || pending.length > 8 ||
    pending.some(p => !p || typeof p !== 'object' || typeof (p as Record<string, unknown>).url !== 'string' ||
      typeof (p as Record<string, unknown>).requestId !== 'string' ||
      !/^[\x21-\x7e]{1,128}$/.test((p as Record<string, unknown>).requestId as string) ||
      typeof (p as Record<string, unknown>).recoverySecret !== 'string' ||
      !/^[A-Za-z0-9_-]{32,128}$/.test((p as Record<string, unknown>).recoverySecret as string)))) {
    throw new Error('invalid pending enrollment recovery state');
  }
  const pendingEnrollments = pending?.map(value => {
    const pendingEnrollment = value as PendingEnrollment;
    const issues: string[] = [];
    const url = validateHubUrl(pendingEnrollment.url, 'pending enrollment', issues);
    if (!url || issues.length) throw new ConfigError(issues);
    return {...pendingEnrollment, url: url.href};
  });
  return {collectorId: raw.collectorId, destinations,
    ...(pendingEnrollments === undefined ? {} : { pendingEnrollments })};
}
/** Exclusive lock prevents simultaneous enrollments losing a destination. */
export function updateInstallation(path: string, change: (state: InstallationState) => InstallationState = s => s): InstallationState {
  mkdirSync(dirname(path), {recursive: true, mode: 0o700});
  const directoryStat = lstatSync(dirname(path));
  if (!directoryStat.isDirectory() || (directoryStat.mode & 0o077) !== 0) {
    throw new ConfigError(['collector state requires a private directory (0700)']);
  }
  const lock = `${path}.lock`;
  let releaseLock: () => void;
  try {
    // Shared runtime lease: a live owner refuses, a proven dead owner recovers,
    // and an incomplete/unsafe lock fails closed for manual inspection.
    releaseLock = acquireProcessLease(lock);
  } catch (error) {
    if (error instanceof ProcessLeaseError) {
      if (error.code === 'busy') {
        throw new ConfigError(['collector state is locked by another process; stop it before retrying']);
      }
      throw new ConfigError(['collector state is locked by an unsafe or incomplete lock file; inspect it before recovery']);
    }
    throw new ConfigError(['collector state lock could not be acquired; check the state directory permissions']);
  }
  let temporary: string | undefined;
  try {
    let state: InstallationState;
    try { lstatSync(path); state = readState(path); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
      state = {collectorId: randomUUID(), destinations: []};
    }
    const changed = change(state);
    // Every write emits exactly the one canonical shape, even if a caller
    // accidentally carries extra fields forward.
    const next: InstallationState = {
      collectorId: changed.collectorId,
      destinations: changed.destinations,
      ...(changed.pendingEnrollments === undefined ? {} : {pendingEnrollments: changed.pendingEnrollments}),
    };
    temporary = `${path}.${randomUUID()}.tmp`;
    const out = openSync(temporary, 'wx', 0o600);
    try { writeFileSync(out, JSON.stringify(next) + '\n'); fsyncSync(out); } finally { closeSync(out); }
    renameSync(temporary, path);
    temporary = undefined;
    const directory = openSync(dirname(path), 'r');
    try { fsyncSync(directory); } finally { closeSync(directory); }
    return next;
  } finally {
    try {
      if (temporary) unlinkSync(temporary);
    } finally {
      releaseLock();
    }
  }
}
