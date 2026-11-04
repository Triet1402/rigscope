/**
 * Offline browser-administrator password recovery.
 *
 * The durable `users` table is the source of truth after the first hub start,
 * not `HUB_BROWSER_USERS` or the CLI `config.json`. This helper therefore
 * updates only the managed user store, records an audit row and a durable
 * session-invalidation marker, and never edits bootstrap configuration,
 * scopes, machine grants or collector credentials.
 *
 * Safety:
 * - the database must be a private regular file;
 * - the hub database lease is acquired before any write, so a running hub
 *   refuses recovery instead of racing it;
 * - the password hash and new password are never printed, logged or audited;
 * - the transaction updates one user, audits it, prunes the bounded audit log
 *   and commits before a read-back verification.
 */

import { existsSync } from 'node:fs';
import { resolve } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { hashPassword, isPasswordHash, MAX_BROWSER_PASSWORD_CHARS, verifyPassword } from '../hub/auth/password';
import { acquireProcessLease } from '../runtime/process-lease';
import { inspectDatabase } from './database';
import { OperationsError, assertPrivateRegularFile, findHubLock, resolvePath } from './fs';

const MAX_AUDIT_EVENTS = 1_000;
const AUDIT_ACTOR = 'local-recovery';
const AUDIT_ACTION = 'user.password-recovery';
const USERNAME_PATTERN = /^[\x21-\x7e]{1,128}$/;

export type RecoverAdminOptions = {
  database: string;
  username?: string;
  password: string;
  /** Explicitly clear a disabled flag; admin/scope are never changed here. */
  enable?: boolean;
  now?: () => number;
};

export type RecoverAdminReport = {
  database: string;
  username: string;
  enabled: boolean;
  auditAction: string;
  auditAt: string;
  /** Durable counter/marker for a future hub restart or session check. */
  sessionEpoch: number;
  sessionsInvalidatedAt: string;
};

export async function recoverAdminPassword(options: RecoverAdminOptions): Promise<RecoverAdminReport> {
  const database = resolvePath(options.database, 'database');
  const username = options.username ?? 'owner';
  if (typeof username !== 'string' || !USERNAME_PATTERN.test(username)) {
    throw new OperationsError('invalid-username', 'recovery username must be 1..128 printable ASCII characters without spaces');
  }
  if (typeof options.password !== 'string' || options.password.length === 0 || options.password.length > MAX_BROWSER_PASSWORD_CHARS) {
    throw new OperationsError('invalid-password', `password must be 1..${MAX_BROWSER_PASSWORD_CHARS} characters`);
  }
  if (!existsSync(database)) {
    throw new OperationsError('missing-database', `hub database does not exist: ${database}`);
  }
  assertPrivateRegularFile(database, 'database');

  const ownLock = resolve(`${database}.hub.lock`);
  const otherLock = findHubLock(database);
  if (otherLock !== null && resolve(otherLock) !== ownLock) {
    throw new OperationsError(
      'hub-active',
      `a hub lock exists at ${otherLock}; stop the hub and inspect that lock before recovery`,
    );
  }
  const release = acquireProcessLease(ownLock);
  try {
    const info = inspectDatabase(database);
    if (!info.integrityOk) {
      throw new OperationsError('integrity-failed', `database failed integrity_check: ${info.integrity}`);
    }
    if (info.newerSchema) {
      throw new OperationsError(
        'newer-schema',
        `database schema ${info.schemaVersion} is newer than this build supports (${info.supportedSchemaVersion})`,
      );
    }
    if (info.ledgerProblem !== null) {
      throw new OperationsError('invalid-database', `database migration history is unusable: ${info.ledgerProblem}`);
    }
    const tables = new Set(info.tables.map((table) => table.name));
    if (!tables.has('users') || !tables.has('admin_audit') || !tables.has('admin_meta')) {
      throw new OperationsError(
        'admin-store-missing',
        'managed user store is missing; start the hub once with this release so its admin schema is created, then run recovery again',
      );
    }

    const hash = hashPassword(options.password);
    if (!isPasswordHash(hash) || !verifyPassword(options.password, hash)) {
      throw new Error('derived password hash has an unexpected shape');
    }
    const nowMs = (options.now ?? Date.now)();
    const at = new Date(nowMs).toISOString();
    const db = new DatabaseSync(database, { timeout: 5_000 });
    try {
      db.exec('PRAGMA busy_timeout = 5000');
      db.exec('PRAGMA synchronous = FULL');
      db.exec('BEGIN IMMEDIATE');
      try {
        const existing = db.prepare('SELECT username, disabled FROM users WHERE username = ?').get(username) as
          | { username: string; disabled: number | bigint }
          | undefined;
        if (existing === undefined) {
          throw new OperationsError(
            'user-not-found',
            `managed user "${username}" does not exist; recovery never creates or elevates accounts`,
          );
        }
        const enable = options.enable === true;
        if (enable) db.prepare('UPDATE users SET password_hash = ?, disabled = 0 WHERE username = ?').run(hash, username);
        else db.prepare('UPDATE users SET password_hash = ? WHERE username = ?').run(hash, username);

        db.prepare('INSERT INTO admin_audit (actor, action, target, at_ms) VALUES (?, ?, ?, ?)').run(
          AUDIT_ACTOR,
          AUDIT_ACTION,
          username,
          nowMs,
        );
        db.prepare(
          'DELETE FROM admin_audit WHERE id NOT IN (SELECT id FROM admin_audit ORDER BY id DESC LIMIT ?)',
        ).run(MAX_AUDIT_EVENTS);

        const epochRow = db.prepare("SELECT value FROM admin_meta WHERE key = 'session_epoch'").get() as
          | { value: string }
          | undefined;
        const previousEpoch = epochRow === undefined ? 0 : Number(epochRow.value);
        const sessionEpoch = Number.isSafeInteger(previousEpoch) && previousEpoch >= 0 ? previousEpoch + 1 : 1;
        const putMeta = db.prepare(
          'INSERT INTO admin_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value',
        );
        putMeta.run('session_epoch', String(sessionEpoch));
        putMeta.run('sessions_invalidated_at', String(nowMs));
        putMeta.run('password_recovered_at', String(nowMs));
        db.exec('COMMIT');

        const check = db.prepare('SELECT password_hash FROM users WHERE username = ?').get(username) as
          | { password_hash: string }
          | undefined;
        if (check === undefined || !verifyPassword(options.password, check.password_hash)) {
          throw new OperationsError('write-verification-failed', `password update for "${username}" could not be verified`);
        }
        return {
          database,
          username,
          enabled: enable ? true : Number(existing.disabled) === 0,
          auditAction: AUDIT_ACTION,
          auditAt: at,
          sessionEpoch,
          sessionsInvalidatedAt: at,
        };
      } catch (error) {
        try {
          db.exec('ROLLBACK');
        } catch {
          // Preserve the original failure.
        }
        throw error;
      }
    } finally {
      db.close();
    }
  } finally {
    release();
  }
}
