/**
 * Durable pairing of a managed CLI runtime with the database backup taken
 * before it was activated.
 *
 * The user installation keeps one `runtime` tree plus `runtime-previous`.
 * That is enough to swap code, but not enough to know which database copy is
 * safe for the previous runtime. This file records exactly that pairing so
 * rollback can either keep a compatible database untouched or restore the
 * matching pre-upgrade copy instead of pointing old code at a newer schema.
 *
 * Existing installations without this file remain usable. Upgrade writes the
 * file first; rollback infers the versions from package.json and refuses an
 * unsafe schema downgrade unless an explicit compatible backup is supplied.
 */

import { existsSync, lstatSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { OperationsError } from '../operations/fs';
import { ensurePrivateDir, writePrivateFile } from '../operations/fs';
import { validateVersion } from '../operations/fs';

export const RUNTIME_STATE_FILE = 'release-state.json';

export type RuntimeBackupRecord = {
  /** Private pre-upgrade database copy. */
  path: string;
  /** Runtime that was active when the copy was taken. */
  fromVersion: string;
  /** Runtime whose activation is paired with this copy. */
  toVersion: string;
  /** Schema of the copied database. */
  schemaVersion: number;
  fromSchema: number | null;
  toSchema: number;
  createdAt: string;
};

export type RuntimeState = {
  stateDir: string;
  currentVersion: string | null;
  previousVersion: string | null;
  backups: Record<string, RuntimeBackupRecord>;
  lastActivation: {
    version: string;
    previousVersion: string | null;
    backupPath: string | null;
    activatedAt: string;
  } | null;
  lastRollback: {
    version: string;
    previousVersion: string;
    restoredBackup: string | null;
    preRestoreBackup: string | null;
    restoredAt: string;
  } | null;
  updatedAt: string;
};

export type RuntimeInspect = {
  supportedSchemaVersion: number;
  schemaVersion: number;
  integrityOk: boolean;
  newerSchema: boolean;
  ledgerProblem: string | null;
};

export function appRoot(runtimeDir: string): string {
  return join(runtimeDir, 'node_modules', 'rigscope');
}

export function runtimeStatePath(dir: string): string {
  return join(dir, RUNTIME_STATE_FILE);
}

export function databasePath(dir: string): string {
  return join(dir, 'hub', 'telemetry.sqlite');
}

export function emptyRuntimeState(dir: string): RuntimeState {
  return {
    stateDir: dir,
    currentVersion: null,
    previousVersion: null,
    backups: {},
    lastActivation: null,
    lastRollback: null,
    updatedAt: new Date().toISOString(),
  };
}

export function readRuntimeState(dir: string): RuntimeState | null {
  const path = runtimeStatePath(dir);
  if (!existsSync(path)) return null;
  const stat = lstatSync(path);
  if (!stat.isFile() || (stat.mode & 0o077) !== 0) {
    throw new OperationsError('insecure-permissions', `runtime state must be a private regular file: ${path}`);
  }
  if (stat.size > 256 * 1024) {
    throw new OperationsError('invalid-state', `runtime state is oversized: ${path}`);
  }
  let value: unknown;
  try {
    value = JSON.parse(readFileSync(path, 'utf8'));
  } catch (error) {
    throw new OperationsError('invalid-state', `runtime state is not valid JSON: ${path}`, { cause: error });
  }
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new OperationsError('invalid-state', `runtime state is not an object: ${path}`);
  }
  const record = value as Record<string, unknown>;
  if (Object.hasOwn(record, 'formatVersion') || Object.hasOwn(record, 'version')) {
    throw new OperationsError('invalid-state', `runtime state must use the canonical shape; reset or reinstall unfinished state: ${path}`);
  }
  if (record.stateDir !== dir) {
    throw new OperationsError('invalid-state', `runtime state belongs to a different installation: ${path}`);
  }
  const backups: Record<string, RuntimeBackupRecord> = {};
  if (record.backups !== null && typeof record.backups === 'object' && !Array.isArray(record.backups)) {
    for (const [key, entry] of Object.entries(record.backups as Record<string, unknown>)) {
      if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) continue;
      const item = entry as Record<string, unknown>;
      if (
        typeof item.path !== 'string' ||
        typeof item.fromVersion !== 'string' ||
        typeof item.toVersion !== 'string' ||
        !Number.isSafeInteger(item.schemaVersion) ||
        !Number.isSafeInteger(item.toSchema) ||
        typeof item.createdAt !== 'string'
      ) {
        continue;
      }
      backups[key] = {
        path: item.path,
        fromVersion: item.fromVersion,
        toVersion: item.toVersion,
        schemaVersion: item.schemaVersion as number,
        fromSchema: Number.isSafeInteger(item.fromSchema) ? (item.fromSchema as number) : null,
        toSchema: item.toSchema as number,
        createdAt: item.createdAt,
      };
    }
  }
  return {
    stateDir: dir,
    currentVersion: typeof record.currentVersion === 'string' ? record.currentVersion : null,
    previousVersion: typeof record.previousVersion === 'string' ? record.previousVersion : null,
    backups,
    lastActivation:
      record.lastActivation !== null && typeof record.lastActivation === 'object'
        ? (record.lastActivation as RuntimeState['lastActivation'])
        : null,
    lastRollback:
      record.lastRollback !== null && typeof record.lastRollback === 'object'
        ? (record.lastRollback as RuntimeState['lastRollback'])
        : null,
    updatedAt: typeof record.updatedAt === 'string' ? record.updatedAt : '',
  };
}

export function writeRuntimeState(dir: string, state: RuntimeState): void {
  ensurePrivateDir(dir);
  const path = runtimeStatePath(dir);
  writePrivateFile(
    path,
    `${JSON.stringify({ ...state, stateDir: dir, updatedAt: new Date().toISOString() }, null, 2)}\n`,
  );
}

/** Read the installed package metadata without following a hostile symlink. */
export function runtimePackage(runtimeDir: string): { name: string; version: string } {
  const path = join(appRoot(runtimeDir), 'package.json');
  let stat;
  try {
    stat = lstatSync(path);
  } catch (error) {
    throw new OperationsError('runtime-invalid', `runtime package metadata is missing: ${path}`, { cause: error });
  }
  if (!stat.isFile() || stat.isSymbolicLink() || stat.size > 128 * 1024) {
    throw new OperationsError('runtime-invalid', `runtime package metadata is not a regular file: ${path}`);
  }
  let value: unknown;
  try {
    value = JSON.parse(readFileSync(path, 'utf8'));
  } catch (error) {
    throw new OperationsError('runtime-invalid', `runtime package metadata is not valid JSON: ${path}`, { cause: error });
  }
  const record = value as Record<string, unknown>;
  if (record === null || typeof record !== 'object' || typeof record.name !== 'string' || typeof record.version !== 'string') {
    throw new OperationsError('runtime-invalid', `runtime package metadata is incomplete: ${path}`);
  }
  validateVersion(record.version);
  if (Object.hasOwn(record, 'rigscopeConfigVersion')) {
    throw new OperationsError('runtime-incompatible', 'Runtime uses unfinished configuration metadata; select a canonical release before resetting or upgrading.');
  }
  return { name: record.name, version: record.version };
}

export function runtimeVersion(runtimeDir: string): string {
  const pkg = runtimePackage(runtimeDir);
  if (pkg.name !== 'rigscope') {
    throw new OperationsError('runtime-invalid', `runtime is ${pkg.name}, expected rigscope`);
  }
  return pkg.version;
}

export function assertRuntimeArtifacts(runtimeDir: string, label: string): void {
  const root = appRoot(runtimeDir);
  for (const relative of ['dist/cli.cjs', 'dist/hub.cjs', 'dist/collector.cjs', 'dist/ops.cjs', '.next/BUILD_ID']) {
    const path = join(root, relative);
    let stat;
    try {
      stat = lstatSync(path);
    } catch (error) {
      throw new OperationsError('runtime-invalid', `${label} is missing ${relative}`, { cause: error });
    }
    if (!stat.isFile() || stat.isSymbolicLink()) {
      throw new OperationsError('runtime-invalid', `${label} has a non-regular ${relative}`);
    }
  }
}

/**
 * Ask the runtime's own operations binary what schema it supports. This is the
 * only honest way to pair an old runtime with a database: the current build's
 * supported schema may be different from the installed or previous one.
 */
export function inspectDatabaseWithRuntime(
  runtimeDir: string,
  database: string,
  options: { timeoutMs?: number } = {},
): RuntimeInspect {
  const ops = join(appRoot(runtimeDir), 'dist', 'ops.cjs');
  if (!existsSync(ops)) {
    throw new OperationsError('runtime-invalid', `runtime operations entry is missing: ${ops}`);
  }
  const result = spawnSync(process.execPath, [ops, 'inspect', '--database', database, '--json'], {
    encoding: 'utf8',
    timeout: options.timeoutMs ?? 15_000,
    maxBuffer: 2 * 1024 * 1024,
    env: { ...process.env, LC_ALL: 'C' },
  });
  if (result.error) {
    throw new OperationsError('database-inspect-failed', `could not inspect database with ${runtimeDir}: ${result.error.message}`);
  }
  const output = (result.stdout ?? '').trim();
  if (output === '') {
    throw new OperationsError(
      'database-inspect-failed',
      `runtime ${runtimeDir} did not report database state: ${(result.stderr ?? '').trim() || 'no output'}`,
    );
  }
  let value: unknown;
  try {
    value = JSON.parse(output);
  } catch (error) {
    throw new OperationsError('database-inspect-failed', `runtime ${runtimeDir} returned invalid inspection JSON`, {
      cause: error,
    });
  }
  const record = value as Record<string, unknown>;
  if (
    record === null ||
    typeof record !== 'object' ||
    !Number.isSafeInteger(record.supportedSchemaVersion) ||
    !Number.isSafeInteger(record.schemaVersion) ||
    typeof record.integrityOk !== 'boolean' ||
    typeof record.newerSchema !== 'boolean'
  ) {
    throw new OperationsError('database-inspect-failed', `runtime ${runtimeDir} returned incomplete inspection data`);
  }
  return {
    supportedSchemaVersion: record.supportedSchemaVersion as number,
    schemaVersion: record.schemaVersion as number,
    integrityOk: record.integrityOk,
    newerSchema: record.newerSchema,
    ledgerProblem: typeof record.ledgerProblem === 'string' ? record.ledgerProblem : null,
  };
}
