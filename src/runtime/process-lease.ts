/**
 * One runtime owns each persistent state path; stale dead-owner leases recover
 * safely while live or ambiguous owners fail closed.
 *
 * Lock file shape: `{ "pid": <owner pid>, "instance": <random id> }`, mode 0600,
 * inside a private 0700 directory. Recovery is allowed only when the recorded
 * owner is proven dead (ESRCH) and the inode/device are unchanged between the
 * inspection and removal. Missing/changed locks are re-inspected rather than
 * treated as errors, so competing recoverers cannot crash each other. A live
 * owner (including EPERM, which means the process exists) refuses acquisition.
 * Empty/garbage/oversized/loose locks are ambiguous and fail closed.
 */

import {
  closeSync,
  fstatSync,
  fsyncSync,
  lstatSync,
  mkdirSync,
  openSync,
  readFileSync,
  unlinkSync,
  writeFileSync,
  type Stats,
} from 'node:fs';
import { dirname, resolve } from 'node:path';
import { randomUUID } from 'node:crypto';

const MAX_LOCK_BYTES = 1024;
/** Bound contention retries while another process recovers/reacquires. */
const MAX_ACQUIRE_ATTEMPTS = 8;

export type ProcessLeaseErrorCode =
  | 'busy'
  | 'unsafe'
  | 'incomplete'
  | 'invalid'
  | 'unavailable'
  | 'recovering';

export class ProcessLeaseError extends Error {
  readonly code: ProcessLeaseErrorCode;
  constructor(code: ProcessLeaseErrorCode, message: string) {
    super(message);
    this.name = 'ProcessLeaseError';
    this.code = code;
  }
}

type LockSnapshot = {
  stats: Stats;
  /** True only when process.kill(pid, 0) proves the PID still exists. */
  alive: boolean;
};

export function acquireProcessLease(path: string): () => void {
  const absolute = resolve(path);
  mkdirSync(dirname(absolute), { recursive: true, mode: 0o700 });
  const parent = lstatSync(dirname(absolute));
  if (!parent.isDirectory() || (parent.mode & 0o077) !== 0) {
    throw new ProcessLeaseError('unsafe', 'runtime state directory must be private');
  }

  for (let attempt = 0; attempt < MAX_ACQUIRE_ATTEMPTS; attempt++) {
    let fd: number;
    try {
      fd = openSync(absolute, 'wx', 0o600);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error;
      const snapshot = inspectLock(absolute);
      if (snapshot === null) {
        // A competing owner removed or created the lock after our lstat; look
        // again instead of surfacing a transient ENOENT.
        continue;
      }
      if (snapshot.alive) {
        throw new ProcessLeaseError(
          'busy',
          'another process owns this runtime state; stop it before starting another',
        );
      }
      // Proven dead: remove only the exact inode we inspected. If another
      // process recovered/replaced it meanwhile, re-inspect the new owner.
      try {
        const current = lstatSync(absolute);
        if (current.ino !== snapshot.stats.ino || current.dev !== snapshot.stats.dev) {
          continue;
        }
        unlinkSync(absolute);
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === 'ENOENT') continue;
        throw new ProcessLeaseError(
          'unavailable',
          'runtime lock could not be recovered; inspect it before recovery',
        );
      }
      continue;
    }

    const inode = fstatSync(fd);
    try {
      writeFileSync(fd, JSON.stringify({ pid: process.pid, instance: randomUUID() }));
      fsyncSync(fd);
    } catch (error) {
      closeSync(fd);
      removeIfSameInode(absolute, inode);
      throw error;
    }
    closeSync(fd);
    return createRelease(absolute, inode);
  }

  throw new ProcessLeaseError(
    'recovering',
    'runtime lease could not be acquired after repeated stale-lock recovery',
  );
}

function inspectLock(path: string): LockSnapshot | null {
  let stats: Stats;
  try {
    stats = lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw new ProcessLeaseError(
      'unavailable',
      'runtime lock could not be inspected; inspect it before recovery',
    );
  }
  if (!stats.isFile() || stats.size > MAX_LOCK_BYTES || (stats.mode & 0o077) !== 0) {
    throw new ProcessLeaseError('unsafe', 'runtime lock is unsafe; inspect it before recovery');
  }

  let raw: string;
  try {
    raw = readFileSync(path, 'utf8');
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw new ProcessLeaseError(
      'unavailable',
      'runtime lock could not be read; inspect it before recovery',
    );
  }

  let owner: unknown;
  try {
    owner = JSON.parse(raw);
  } catch {
    throw new ProcessLeaseError(
      'incomplete',
      'runtime lock is incomplete; inspect it before recovery',
    );
  }
  if (owner === null || typeof owner !== 'object' || Array.isArray(owner)) {
    throw new ProcessLeaseError('invalid', 'runtime lock has an invalid owner');
  }
  const pid = (owner as { pid?: unknown }).pid;
  if (!Number.isSafeInteger(pid) || (pid as number) < 1) {
    throw new ProcessLeaseError('invalid', 'runtime lock has an invalid owner');
  }

  try {
    process.kill(pid as number, 0);
    return { stats, alive: true };
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code;
    if (code === 'ESRCH') return { stats, alive: false };
    // EPERM means the process exists but belongs to another user. Any other
    // probe failure cannot prove death, so treat it as ambiguous and fail closed.
    if (code === 'EPERM' || code === 'EACCES') return { stats, alive: true };
    throw new ProcessLeaseError(
      'unavailable',
      'runtime lock owner could not be verified; inspect it before recovery',
    );
  }
}

function createRelease(path: string, inode: Stats): () => void {
  let released = false;
  const cleanup = (): void => {
    removeIfSameInode(path, inode);
  };
  const onExit = (): void => {
    if (released) return;
    released = true;
    cleanup();
  };
  process.once('exit', onExit);
  return () => {
    if (released) return;
    released = true;
    // Avoid accumulating an exit listener for every short-lived state update.
    process.removeListener('exit', onExit);
    cleanup();
  };
}

function removeIfSameInode(path: string, inode: Stats): void {
  try {
    const stat = lstatSync(path);
    if (stat.ino === inode.ino && stat.dev === inode.dev) unlinkSync(path);
  } catch {
    /* Missing/replaced leases must never mask the original exit/error. */
  }
}
