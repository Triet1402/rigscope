/**
 * Private Unix-domain socket lifecycle for remote hub mode.
 *
 * The outer TLS terminator reaches the hub through a filesystem socket, not a
 * TCP port. That transport is part of the trust boundary, so startup fails
 * closed unless:
 *
 * - HUB_SOCKET_PATH is an absolute, normalized, length-bounded path;
 * - its parent directory exists (or is created) as a private 0700 directory
 *   owned by this process and is not a symlink;
 * - the path itself is absent — a symlink, regular file, directory, or an
 *   existing socket (possibly active) is never removed or reused;
 * - after `listen`, the created socket is a socket file chmod 0600.
 *
 * Graceful shutdown removes only the exact socket identity this process
 * created; a replaced path is left untouched.
 */

import {
  chmodSync,
  lstatSync,
  mkdirSync,
  unlinkSync,
  type Stats,
} from "node:fs";
import { dirname } from "node:path";

export type UnixSocketIdentity = {
  dev: number;
  ino: number;
};

export class RemoteSocketError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "RemoteSocketError";
  }
}

/**
 * Prepares the private parent directory and verifies the socket path is free.
 * Runs before Next.js is prepared and before `listen`.
 */
export function prepareUnixSocketPath(socketPath: string): void {
  const parent = dirname(socketPath);
  try {
    mkdirSync(parent, { recursive: true, mode: 0o700 });
  } catch (error) {
    throw new RemoteSocketError(
      `cannot create HUB_SOCKET_PATH parent directory: ${errorMessage(error)}`,
    );
  }

  let parentStat: Stats;
  try {
    parentStat = lstatSync(parent);
  } catch (error) {
    throw new RemoteSocketError(
      `cannot inspect HUB_SOCKET_PATH parent directory: ${errorMessage(error)}`,
    );
  }
  if (parentStat.isSymbolicLink() || !parentStat.isDirectory()) {
    throw new RemoteSocketError(
      "HUB_SOCKET_PATH parent must be a real directory, not a symlink or file",
    );
  }
  if ((parentStat.mode & 0o077) !== 0) {
    throw new RemoteSocketError(
      "HUB_SOCKET_PATH parent directory must be private (0700); found group or other permissions",
    );
  }
  const uid = typeof process.getuid === "function" ? process.getuid() : undefined;
  if (uid !== undefined && parentStat.uid !== uid) {
    throw new RemoteSocketError(
      "HUB_SOCKET_PATH parent directory must be owned by the hub process user",
    );
  }

  const existing = lstatIfPresent(socketPath);
  if (existing === undefined) return;
  if (existing.isSymbolicLink()) {
    throw new RemoteSocketError(
      "HUB_SOCKET_PATH must not be a symlink; refusing to replace it",
    );
  }
  if (existing.isSocket()) {
    throw new RemoteSocketError(
      "HUB_SOCKET_PATH already exists as a socket; stop the other hub or remove the stale socket explicitly",
    );
  }
  throw new RemoteSocketError(
    "HUB_SOCKET_PATH is occupied by an existing non-socket file; refusing to replace it",
  );
}

/**
 * Called after `server.listen(socketPath)` succeeds. Enforces 0600 and
 * records the exact filesystem identity for ownership-safe cleanup.
 */
export function takeUnixSocketOwnership(socketPath: string): UnixSocketIdentity {
  let created: Stats;
  try {
    created = lstatSync(socketPath);
  } catch (error) {
    throw new RemoteSocketError(
      `hub did not create the expected Unix socket: ${errorMessage(error)}`,
    );
  }
  if (created.isSymbolicLink() || !created.isSocket()) {
    throw new RemoteSocketError(
      "hub-created Unix socket path is not a socket; refusing to continue",
    );
  }
  try {
    chmodSync(socketPath, 0o600);
  } catch (error) {
    throw new RemoteSocketError(
      `cannot set Unix socket permissions to 0600: ${errorMessage(error)}`,
    );
  }
  let secured: Stats;
  try {
    secured = lstatSync(socketPath);
  } catch (error) {
    throw new RemoteSocketError(
      `cannot re-inspect Unix socket permissions: ${errorMessage(error)}`,
    );
  }
  if (!secured.isSocket() || (secured.mode & 0o777) !== 0o600) {
    throw new RemoteSocketError(
      "Unix socket permissions are not exactly 0600 after chmod",
    );
  }
  return { dev: secured.dev, ino: secured.ino };
}

/**
 * Removes the socket only when it is still the socket file this process
 * created. A replaced file, symlink, or another process's socket is left
 * untouched. Returns whether this process removed it.
 */
export function removeOwnedUnixSocket(
  socketPath: string,
  identity: UnixSocketIdentity | null,
): boolean {
  if (identity === null) return false;
  const current = lstatIfPresent(socketPath);
  if (current === undefined) return false;
  if (
    current.isSymbolicLink() ||
    !current.isSocket() ||
    current.dev !== identity.dev ||
    current.ino !== identity.ino
  ) {
    return false;
  }
  try {
    unlinkSync(socketPath);
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw new RemoteSocketError(
      `cannot remove the hub Unix socket: ${errorMessage(error)}`,
    );
  }
}

/**
 * Startup-failure cleanup for a socket this process just created. Only call
 * after `prepareUnixSocketPath` required the path to be absent and this
 * process successfully listened on it. Non-sockets are never removed.
 */
export function abandonUnixSocket(socketPath: string): void {
  const current = lstatIfPresent(socketPath);
  if (current === undefined) return;
  if (current.isSymbolicLink() || !current.isSocket()) return;
  try {
    unlinkSync(socketPath);
  } catch {
    // Best-effort: the startup failure is already being reported.
  }
}

function lstatIfPresent(path: string): Stats | undefined {
  try {
    return lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw new RemoteSocketError(
      `cannot inspect HUB_SOCKET_PATH: ${errorMessage(error)}`,
    );
  }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
