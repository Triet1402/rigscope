/**
 * Small filesystem and process-facing primitives for the operations CLI.
 *
 * Private-by-default: state, backups and environment files are owner-only.
 * Release artifacts are readable but never contain credentials. Everything
 * that writes a durable file stages it and renames atomically so a failed
 * command never leaves a half-written database, manifest or unit file in the
 * place a caller will read.
 */

import {
  chmodSync,
  closeSync,
  constants as fsConstants,
  copyFileSync,
  createReadStream,
  existsSync,
  fsyncSync,
  lstatSync,
  mkdirSync,
  openSync,
  readFileSync,
  readdirSync,
  readlinkSync,
  renameSync,
  statSync,
  symlinkSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { createHash, randomUUID } from "node:crypto";
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import type { Stats } from "node:fs";

export const PRIVATE_DIR_MODE = 0o700;
export const PRIVATE_FILE_MODE = 0o600;
export const PUBLIC_DIR_MODE = 0o755;
export const PUBLIC_FILE_MODE = 0o644;

export class OperationsError extends Error {
  readonly code: string;
  constructor(code: string, message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "OperationsError";
    this.code = code;
  }
}

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export function timestampSlug(ms = Date.now()): string {
  return new Date(ms).toISOString().replace(/[:.]/g, "-");
}

export function isPrivateMode(mode: number): boolean {
  return (mode & 0o077) === 0;
}

export function ensurePrivateDir(path: string): void {
  const absolute = resolve(path);
  mkdirSync(absolute, { recursive: true, mode: PRIVATE_DIR_MODE });
  const stat = lstatSync(absolute);
  if (!stat.isDirectory() || stat.isSymbolicLink()) {
    throw new OperationsError("unsafe-path", `${absolute} must be a real directory`);
  }
  if (!isPrivateMode(stat.mode)) {
    throw new OperationsError(
      "insecure-permissions",
      `${absolute} must be private (0700 or stricter); found ${formatMode(stat.mode)}`,
    );
  }
}

export function ensurePublicDir(path: string): void {
  const absolute = resolve(path);
  mkdirSync(absolute, { recursive: true, mode: PUBLIC_DIR_MODE });
  const stat = lstatSync(absolute);
  if (!stat.isDirectory() || stat.isSymbolicLink()) {
    throw new OperationsError("unsafe-path", `${absolute} must be a real directory`);
  }
}

export function assertPrivateRegularFile(path: string, label: string): Stats {
  const absolute = resolve(path);
  let stat: Stats;
  try {
    stat = lstatSync(absolute);
  } catch (error) {
    throw new OperationsError("missing-file", `${label} does not exist: ${absolute}`, { cause: error });
  }
  if (!stat.isFile() || stat.isSymbolicLink()) {
    throw new OperationsError("unsafe-path", `${label} must be a regular file, not a symlink: ${absolute}`);
  }
  if (!isPrivateMode(stat.mode)) {
    throw new OperationsError(
      "insecure-permissions",
      `${label} must be private (0600/0400); found ${formatMode(stat.mode)}: ${absolute}`,
    );
  }
  return stat;
}

export function assertPublicRegularFile(path: string, label: string): Stats {
  const absolute = resolve(path);
  let stat: Stats;
  try {
    stat = lstatSync(absolute);
  } catch (error) {
    throw new OperationsError("missing-file", `${label} does not exist: ${absolute}`, { cause: error });
  }
  if (!stat.isFile() || stat.isSymbolicLink()) {
    throw new OperationsError("unsafe-path", `${label} must be a regular file, not a symlink: ${absolute}`);
  }
  return stat;
}

export function formatMode(mode: number): string {
  return `0${(mode & 0o777).toString(8).padStart(3, "0")}`;
}

export function writeFileAtomic(path: string, content: string | Buffer, mode: number): void {
  const absolute = resolve(path);
  const parent = dirname(absolute);
  const temporary = join(parent, `.${basename(absolute)}.tmp-${randomUUID()}`);
  let fd: number | null = null;
  try {
    fd = openSync(temporary, "wx", mode);
    writeFileSync(fd, content);
    fsyncSync(fd);
    closeSync(fd);
    fd = null;
    chmodSync(temporary, mode);
    renameSync(temporary, absolute);
    fsyncDir(parent);
  } catch (error) {
    if (fd !== null) {
      try {
        closeSync(fd);
      } catch {
        // Preserve the original error.
      }
    }
    removeIfExists(temporary);
    throw error;
  }
}

export function writePrivateFile(path: string, content: string | Buffer): void {
  ensurePrivateDir(dirname(resolve(path)));
  writeFileAtomic(path, content, PRIVATE_FILE_MODE);
}

export function writePublicFile(path: string, content: string | Buffer): void {
  ensurePublicDir(dirname(resolve(path)));
  writeFileAtomic(path, content, PUBLIC_FILE_MODE);
}

export function ensurePrivateFile(path: string, content: string): { created: boolean; path: string } {
  const absolute = resolve(path);
  if (existsSync(absolute)) {
    assertPrivateRegularFile(absolute, "private file");
    return { created: false, path: absolute };
  }
  writePrivateFile(absolute, content);
  return { created: true, path: absolute };
}

export function fsyncDir(path: string): void {
  let fd: number | null = null;
  try {
    fd = openSync(path, "r");
    fsyncSync(fd);
  } catch {
    // Some filesystems do not support fsync on directories. The rename is
    // still atomic; this is a durability best effort.
  } finally {
    if (fd !== null) {
      try {
        closeSync(fd);
      } catch {
        // ignore
      }
    }
  }
}

export function removeIfExists(path: string): void {
  try {
    unlinkSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
}

export function fileSize(path: string): number {
  try {
    const stat = statSync(path);
    return stat.isFile() ? stat.size : 0;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return 0;
    throw error;
  }
}

export function resolvePath(value: string, label: string): string {
  if (typeof value !== "string" || value.trim() === "") {
    throw new OperationsError("invalid-argument", `${label} must be a nonempty path`);
  }
  return resolve(value);
}

export function isInside(parent: string, candidate: string): boolean {
  const root = resolve(parent);
  const target = resolve(candidate);
  const rel = relative(root, target);
  return rel === "" || (rel !== ".." && !rel.startsWith(`..${sep}`) && !isAbsolute(rel));
}

/** Path-safe release/version identifier; never `.`/`..` or a path separator. */
export function validateVersion(version: string): string {
  if (typeof version !== "string" || !/^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$/.test(version)) {
    throw new OperationsError(
      "invalid-version",
      `version must be 1..64 characters of [A-Za-z0-9._+-] and start alphanumeric, got ${JSON.stringify(version)}`,
    );
  }
  return version;
}

/**
 * systemd argument quoting. systemd splits ExecStart/Environment on whitespace
 * and understands double-quoted arguments with backslash escapes; a path with
 * spaces must survive a shell-less service manager.
 */
export function systemdQuote(value: string): string {
  if (/^[A-Za-z0-9_@%+=:,./-]+$/.test(value)) return value;
  return `"${value.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

export async function sha256File(path: string): Promise<string> {
  const hash = createHash("sha256");
  const input = createReadStream(resolve(path));
  for await (const chunk of input) hash.update(chunk as Buffer);
  return hash.digest("hex");
}

export type WalkedFile = { path: string; relative: string; bytes: number };

/** Deterministic recursive file walk. Symlinks are never followed. */
export function walkFiles(root: string, skipDir?: (relativePath: string) => boolean): WalkedFile[] {
  const base = resolve(root);
  const files: WalkedFile[] = [];
  const visit = (directory: string): void => {
    for (const entry of readdirSync(directory, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
      const absolute = join(directory, entry.name);
      const rel = relative(base, absolute).split(sep).join("/");
      if (entry.isSymbolicLink()) continue;
      if (entry.isDirectory()) {
        if (skipDir?.(rel)) continue;
        visit(absolute);
        continue;
      }
      if (!entry.isFile()) continue;
      files.push({ path: absolute, relative: rel, bytes: statSync(absolute).size });
    }
  };
  visit(base);
  return files.sort((a, b) => a.relative.localeCompare(b.relative));
}

export function currentSymlink(linkPath: string): string | null {
  try {
    return readlinkSync(linkPath);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return null;
    throw error;
  }
}

/** Atomic symlink replacement; `target` is written exactly as given. */
export function replaceSymlink(linkPath: string, target: string): void {
  const absolute = resolve(linkPath);
  const temporary = join(dirname(absolute), `.${basename(absolute)}.tmp-${randomUUID()}`);
  try {
    symlinkSync(target, temporary);
    renameSync(temporary, absolute);
    fsyncDir(dirname(absolute));
  } catch (error) {
    removeIfExists(temporary);
    throw error;
  }
}

/** Best-effort lock candidates used by the hub or supervisor. Never deletes them. */
export function findHubLock(databasePath: string): string | null {
  const absolute = resolve(databasePath);
  const parent = dirname(absolute);
  const envLock = process.env.RIGSCOPE_HUB_LOCK_FILE;
  const candidates = [
    ...(envLock ? [resolve(envLock)] : []),
    `${absolute}.lock`,
    `${absolute}-lock`,
    `${absolute}.hub.lock`,
    join(parent, "hub.lock"),
    join(parent, "hub.pid"),
    // `run/hub.lock` is the component supervisor lease, not a database
    // lease. Treating it as a hub lock would make an upgrade's own held lease
    // look like a live hub during database backup/restore.
  ];
  for (const candidate of candidates) {
    try {
      lstatSync(candidate);
      return candidate;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
  }
  return null;
}

export function assertNoActiveHubLock(databasePath: string): void {
  const lock = findHubLock(databasePath);
  if (lock !== null) {
    throw new OperationsError(
      "hub-active",
      `an active hub lock exists at ${lock}; stop the hub, verify no process owns it, and remove the lock only after explicit inspection`,
    );
  }
}

export function copyFileAtomic(source: string, destination: string, mode = PRIVATE_FILE_MODE): void {
  const absolute = resolve(destination);
  const temporary = join(dirname(absolute), `.${basename(absolute)}.tmp-${randomUUID()}`);
  try {
    copyFileSync(resolve(source), temporary, fsConstants.COPYFILE_EXCL);
    chmodSync(temporary, mode);
    const fd = openSync(temporary, "r");
    try {
      fsyncSync(fd);
    } finally {
      closeSync(fd);
    }
    renameSync(temporary, absolute);
    fsyncDir(dirname(absolute));
  } catch (error) {
    removeIfExists(temporary);
    throw error;
  }
}

export type ReadJsonResult<T> = { value: T; text: string };

export function readJsonFile<T>(path: string): ReadJsonResult<T> {
  assertPrivateRegularFile(path, "JSON file");
  const text = readFileSync(path, "utf8");
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (error) {
    throw new OperationsError("invalid-json", `${path} is not valid JSON`, { cause: error });
  }
  return { value: value as T, text };
}

export function readTextFile(path: string): string {
  return readFileSync(path, "utf8");
}
