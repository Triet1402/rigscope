/**
 * Persistent browser user/scope administration and bounded audit.
 *
 * The durable directory is the source of truth after the first bootstrap. The
 * `HUB_BROWSER_USERS` configuration is imported exactly once (marker row in
 * `admin_meta`); later starts never resurrect a deleted/disabled user and never
 * overwrite an operator-managed password, scope or admin flag.
 *
 * Security invariants:
 * - every service mutation rechecks that the actor is an active administrator;
 * - the last active administrator cannot be demoted, disabled or deleted;
 * - password hashes are salted scrypt values produced off the event loop, and
 *   at most two hashes run concurrently per store;
 * - audit rows contain only actor/action/target/time — never passwords, hashes
 *   or request payloads — and are pruned to the newest 1000 entries.
 *
 * AdminStore owns its tables. `ADMIN_MIGRATIONS` is exported for the
 * composition root to append as version 3 of the shared schema; constructing
 * AdminStore also applies the idempotent `adminSchema` so a standalone test or
 * an early composition remains safe.
 */

import { randomBytes, scrypt } from "node:crypto";
import type { DatabaseSync } from "node:sqlite";
import type { HubDatabase } from "../storage/db";
import type { Migration } from "../storage/migrations";
import {
  type BrowserMachineScope,
  type BrowserUserConfig,
  MAX_BROWSER_USERS,
} from "../auth/config";
import {
  BROWSER_HASH_PREFIX,
  isPasswordHash,
  MAX_BROWSER_PASSWORD_CHARS,
} from "../auth/password";

/**
 * Machine scopes are bounded by serialized bytes, not a machine count. The
 * administrative HTTP body is already capped at 8 KiB; this guard also covers
 * persisted rows and direct store callers while allowing well over fifty
 * explicit machine IDs.
 */
const MAX_SCOPE_BYTES = 16 * 1024;
const MAX_AUDIT_EVENTS = 1000;
const DEFAULT_AUDIT_LIMIT = 100;
const MAX_CONCURRENT_HASHES = 2;
const SALT_BYTES = 16;
const KEY_BYTES = 64;
const BOOTSTRAP_MARKER = "bootstrap_v1";

const USERNAME_PATTERN = /^[\x21-\x7e]{1,128}$/;

export type AdminUserRecord = {
  username: string;
  machineIds: BrowserMachineScope;
  admin: boolean;
  disabled: boolean;
};

export type AdminUserInput = {
  username: string;
  machineIds: BrowserMachineScope;
  admin: boolean;
  disabled?: boolean;
  /** Plaintext is hashed before storage and never written to audit. */
  password?: string;
};

export type AdminAuditEvent = {
  actor: string;
  action: string;
  target: string;
  /** ISO 8601 UTC. */
  at: string;
};

export type AdminErrorCode = "invalid" | "forbidden" | "conflict" | "not-found" | "unavailable";

export class AdminError extends Error {
  readonly code: AdminErrorCode;
  constructor(code: AdminErrorCode, message: string) {
    super(message);
    this.name = "AdminError";
    this.code = code;
  }
}

export type AdminStoreOptions = {
  database: HubDatabase;
  bootstrapUsers: BrowserUserConfig[];
  /** One-time explicit elevation for a known existing owner username. */
  bootstrapAdminUsernames?: string[];
  /** Called after COMMIT with the active auth directory. */
  onUsersChanged?: (users: BrowserUserConfig[]) => void;
  now?: () => number;
};

type UserRow = {
  username: string;
  password_hash: string;
  scope_json: string;
  is_admin: number | bigint;
  disabled: number | bigint;
};

/** Append as version 3 of the shared hub migrations. */
export const ADMIN_MIGRATIONS: Migration[] = [
  {
    version: 3,
    name: "admin",
    up: adminSchema,
  },
];

/** Idempotent administrative schema; safe to apply on every construction. */
export function adminSchema(db: DatabaseSync): void {
  db.exec(`
    CREATE TABLE IF NOT EXISTS users (
      username TEXT PRIMARY KEY,
      password_hash TEXT NOT NULL,
      scope_json TEXT NOT NULL,
      is_admin INTEGER NOT NULL DEFAULT 0,
      disabled INTEGER NOT NULL DEFAULT 0
    );

    CREATE TABLE IF NOT EXISTS admin_audit (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      actor TEXT NOT NULL,
      action TEXT NOT NULL,
      target TEXT NOT NULL,
      at_ms INTEGER NOT NULL
    );

    CREATE TABLE IF NOT EXISTS admin_meta (
      key TEXT PRIMARY KEY,
      value TEXT NOT NULL
    );

    CREATE INDEX IF NOT EXISTS admin_audit_at ON admin_audit (at_ms DESC);
  `);
}

export class AdminStore {
  private readonly database: HubDatabase;
  private readonly db: DatabaseSync;
  private readonly now: () => number;
  private readonly onUsersChanged?: (users: BrowserUserConfig[]) => void;
  private activeHashes = 0;

  private readonly selectUser: ReturnType<DatabaseSync["prepare"]>;
  private readonly selectActive: ReturnType<DatabaseSync["prepare"]>;
  private readonly selectAll: ReturnType<DatabaseSync["prepare"]>;
  private readonly insertUser: ReturnType<DatabaseSync["prepare"]>;
  private readonly updateUser: ReturnType<DatabaseSync["prepare"]>;
  private readonly promoteUser: ReturnType<DatabaseSync["prepare"]>;
  private readonly deleteUserStatement: ReturnType<DatabaseSync["prepare"]>;
  private readonly countActiveAdmins: ReturnType<DatabaseSync["prepare"]>;
  private readonly countUsers: ReturnType<DatabaseSync["prepare"]>;
  private readonly insertAudit: ReturnType<DatabaseSync["prepare"]>;
  private readonly pruneAudit: ReturnType<DatabaseSync["prepare"]>;
  private readonly selectAudit: ReturnType<DatabaseSync["prepare"]>;
  private readonly selectMeta: ReturnType<DatabaseSync["prepare"]>;
  private readonly insertMeta: ReturnType<DatabaseSync["prepare"]>;

  constructor(options: AdminStoreOptions) {
    if (!options || typeof options !== "object") throw new Error("AdminStore options are required");
    this.database = options.database;
    this.db = options.database.db;
    this.now = options.now ?? (() => Date.now());
    this.onUsersChanged = options.onUsersChanged;

    adminSchema(this.db);

    this.selectUser = this.db.prepare(
      "SELECT username, password_hash, scope_json, is_admin, disabled FROM users WHERE username = ?",
    );
    this.selectActive = this.db.prepare(
      "SELECT username, password_hash, scope_json, is_admin, disabled FROM users " +
        "WHERE disabled = 0 ORDER BY username",
    );
    this.selectAll = this.db.prepare(
      "SELECT username, password_hash, scope_json, is_admin, disabled FROM users ORDER BY username",
    );
    this.insertUser = this.db.prepare(
      "INSERT OR IGNORE INTO users (username, password_hash, scope_json, is_admin, disabled) " +
        "VALUES (?, ?, ?, ?, ?)",
    );
    this.updateUser = this.db.prepare(
      "UPDATE users SET password_hash = ?, scope_json = ?, is_admin = ?, disabled = ? WHERE username = ?",
    );
    this.promoteUser = this.db.prepare("UPDATE users SET is_admin = 1 WHERE username = ?");
    this.deleteUserStatement = this.db.prepare("DELETE FROM users WHERE username = ?");
    this.countActiveAdmins = this.db.prepare(
      "SELECT COUNT(*) AS n FROM users WHERE is_admin = 1 AND disabled = 0 AND username <> ?",
    );
    this.countUsers = this.db.prepare("SELECT COUNT(*) AS n FROM users");
    this.insertAudit = this.db.prepare(
      "INSERT INTO admin_audit (actor, action, target, at_ms) VALUES (?, ?, ?, ?)",
    );
    this.pruneAudit = this.db.prepare(
      "DELETE FROM admin_audit WHERE id NOT IN " +
        "(SELECT id FROM admin_audit ORDER BY id DESC LIMIT ?)",
    );
    this.selectAudit = this.db.prepare(
      "SELECT actor, action, target, at_ms FROM admin_audit ORDER BY id DESC LIMIT ?",
    );
    this.selectMeta = this.db.prepare("SELECT value FROM admin_meta WHERE key = ?");
    this.insertMeta = this.db.prepare(
      "INSERT INTO admin_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
    );

    this.bootstrap(options.bootstrapUsers ?? [], options.bootstrapAdminUsernames ?? []);
  }

  /** Active users for the authentication directory (includes hashes). */
  users(): BrowserUserConfig[] {
    const rows = this.selectActive.all() as UserRow[];
    const users: BrowserUserConfig[] = [];
    for (const row of rows) {
      const user = userFromRow(row);
      if (user) users.push(user);
    }
    return users;
  }

  /** Public directory records: no password hashes. */
  listUsers(): AdminUserRecord[] {
    const rows = this.selectAll.all() as UserRow[];
    const users: AdminUserRecord[] = [];
    for (const row of rows) {
      const scope = parseScope(row.scope_json);
      if (!scope) continue;
      users.push({
        username: row.username,
        machineIds: scope,
        admin: Number(row.is_admin) === 1,
        disabled: Number(row.disabled) === 1,
      });
    }
    return users;
  }

  /** True only for an existing, enabled, explicitly administrative user. */
  isAdmin(username: string): boolean {
    if (typeof username !== "string" || username.length === 0) return false;
    const row = this.selectUser.get(username) as UserRow | undefined;
    return row !== undefined && Number(row.is_admin) === 1 && Number(row.disabled) === 0;
  }

  /**
   * Creates or updates one user. The actor must be an active administrator,
   * checked again inside the write transaction. A password is required for a
   * new user and optional (preserve current hash) for an existing one.
   */
  async upsertUser(actor: string, input: AdminUserInput): Promise<void> {
    this.assertAdmin(actor);
    const clean = validateInput(input);
    let passwordHash: string | undefined;
    if (clean.password !== undefined) {
      if (this.activeHashes >= MAX_CONCURRENT_HASHES) {
        throw new AdminError("unavailable", "password hashing is busy; retry shortly");
      }
      this.activeHashes += 1;
      try {
        passwordHash = await hashPasswordAsync(clean.password);
      } finally {
        this.activeHashes -= 1;
      }
    }

    this.db.exec("BEGIN IMMEDIATE");
    try {
      // A concurrent disable/delete of the actor invalidates this mutation too.
      this.assertAdmin(actor);
      const existing = this.selectUser.get(clean.username) as UserRow | undefined;
      if (!existing && passwordHash === undefined) {
        throw new AdminError("invalid", "a password is required for a new user");
      }
      if (!existing && this.number(this.countUsers.get()) >= MAX_BROWSER_USERS) {
        throw new AdminError("conflict", `at most ${MAX_BROWSER_USERS} users are supported`);
      }
      const disabled = clean.disabled ?? (existing ? Number(existing.disabled) === 1 : false);
      if (
        existing &&
        Number(existing.is_admin) === 1 &&
        Number(existing.disabled) === 0 &&
        (!clean.admin || disabled)
      ) {
        this.requireAnotherAdmin(clean.username);
      }
      const scopeJson = serializeScope(clean.machineIds);
      if (existing) {
        this.updateUser.run(
          passwordHash ?? existing.password_hash,
          scopeJson,
          clean.admin ? 1 : 0,
          disabled ? 1 : 0,
          clean.username,
        );
      } else {
        this.insertUser.run(
          clean.username,
          passwordHash!,
          scopeJson,
          clean.admin ? 1 : 0,
          disabled ? 1 : 0,
        );
      }
      const action = !existing
        ? "user.create"
        : passwordHash !== undefined
          ? "user.update-password"
          : "user.update";
      this.writeAudit(actor, action, clean.username);
      this.db.exec("COMMIT");
    } catch (error) {
      try {
        this.db.exec("ROLLBACK");
      } catch {
        // Preserve the original failure.
      }
      throw error;
    }
    this.notifyChanged();
  }

  /** Deletes one user; the last active administrator cannot be removed. */
  deleteUser(actor: string, username: string): void {
    this.assertAdmin(actor);
    validateUsername(username, "username");
    this.db.exec("BEGIN IMMEDIATE");
    try {
      this.assertAdmin(actor);
      const existing = this.selectUser.get(username) as UserRow | undefined;
      if (!existing) throw new AdminError("not-found", "user not found");
      if (Number(existing.is_admin) === 1 && Number(existing.disabled) === 0) {
        this.requireAnotherAdmin(username);
      }
      this.deleteUserStatement.run(username);
      this.writeAudit(actor, "user.delete", username);
      this.db.exec("COMMIT");
    } catch (error) {
      try {
        this.db.exec("ROLLBACK");
      } catch {
        // Preserve the original failure.
      }
      throw error;
    }
    this.notifyChanged();
  }

  /** Newest-first bounded audit; never contains hashes or request payloads. */
  audit(limit?: number): AdminAuditEvent[] {
    let selected = DEFAULT_AUDIT_LIMIT;
    if (limit !== undefined) {
      if (!Number.isSafeInteger(limit) || limit < 1) {
        throw new AdminError("invalid", "audit limit must be a positive integer");
      }
      selected = Math.min(limit, MAX_AUDIT_EVENTS);
    }
    const rows = this.selectAudit.all(selected) as Array<{
      actor: string;
      action: string;
      target: string;
      at_ms: number | bigint;
    }>;
    return rows.map((row) => ({
      actor: row.actor,
      action: row.action,
      target: row.target,
      at: new Date(Number(row.at_ms)).toISOString(),
    }));
  }

  private bootstrap(users: readonly BrowserUserConfig[], adminUsernames: readonly string[]): void {
    const marker = this.selectMeta.get(BOOTSTRAP_MARKER) as { value: string } | undefined;
    if (marker !== undefined) return;

    if (!Array.isArray(users) || users.length > MAX_BROWSER_USERS) {
      throw new AdminError("invalid", "bootstrap user directory exceeds its bound");
    }
    const adminSet = new Set<string>();
    for (const username of adminUsernames) {
      validateUsername(username, "bootstrap admin username");
      adminSet.add(username);
    }
    const validated: BrowserUserConfig[] = [];
    const seen = new Set<string>();
    for (const user of users) {
      validateBootstrapUser(user);
      if (seen.has(user.username)) throw new AdminError("invalid", "duplicate bootstrap username");
      seen.add(user.username);
      validated.push(user);
    }
    // An unconfigured first start must stay configurable on the next restart.
    // Once users exist the normal one-time marker prevents resurrection.
    if (validated.length === 0 && this.number(this.countUsers.get()) === 0) return;

    this.db.exec("BEGIN IMMEDIATE");
    try {
      const current = this.selectMeta.get(BOOTSTRAP_MARKER) as { value: string } | undefined;
      if (current !== undefined) {
        this.db.exec("COMMIT");
        return;
      }
      let changed = false;
      for (const user of validated) {
        const admin = user.admin === true || adminSet.has(user.username);
        const result = this.insertUser.run(
          user.username,
          user.passwordHash,
          serializeScope(user.machineIds),
          admin ? 1 : 0,
          0,
        );
        if (this.number(result.changes) === 1) {
          changed = true;
        } else if (admin) {
          // Existing row from an earlier pre-release database: preserve its
          // hash/scope but honor the one-time explicit owner elevation.
          this.promoteUser.run(user.username);
          changed = true;
        }
      }
      this.insertMeta.run(BOOTSTRAP_MARKER, String(this.now()));
      this.db.exec("COMMIT");
      if (changed) this.notifyChanged();
    } catch (error) {
      try {
        this.db.exec("ROLLBACK");
      } catch {
        // Preserve the original failure.
      }
      throw error;
    }
  }

  private assertAdmin(actor: string): void {
    validateUsername(actor, "actor");
    const row = this.selectUser.get(actor) as UserRow | undefined;
    if (!row || Number(row.is_admin) !== 1 || Number(row.disabled) !== 0) {
      throw new AdminError("forbidden", "administrative access required");
    }
  }

  private requireAnotherAdmin(excludedUsername: string): void {
    const row = this.countActiveAdmins.get(excludedUsername) as { n: number | bigint } | undefined;
    if (Number(row?.n ?? 0) === 0) {
      throw new AdminError("conflict", "the last active administrator cannot be removed or demoted");
    }
  }

  private writeAudit(actor: string, action: string, target: string): void {
    this.insertAudit.run(actor, action, target, this.now());
    this.pruneAudit.run(MAX_AUDIT_EVENTS);
  }

  private notifyChanged(): void {
    if (!this.onUsersChanged) return;
    try {
      this.onUsersChanged(this.users());
    } catch (error) {
      // The commit already succeeded; a listener failure must not turn it into
      // a reported failure. Avoid logging any user material.
      console.error("admin user change listener failed:", error);
    }
  }

  private number(row: unknown): number {
    return Number((row as { n?: number | bigint } | undefined)?.n ?? 0);
  }
}

function validateBootstrapUser(user: BrowserUserConfig): void {
  if (user === null || typeof user !== "object") throw new AdminError("invalid", "invalid bootstrap user");
  validateUsername(user.username, "bootstrap username");
  if (!isPasswordHash(user.passwordHash)) {
    throw new AdminError("invalid", "bootstrap user passwordHash is not a scrypt hash");
  }
  validateScope(user.machineIds, "bootstrap machineIds");
  if (user.admin !== undefined && typeof user.admin !== "boolean") {
    throw new AdminError("invalid", "bootstrap admin must be a boolean");
  }
}

function validateInput(input: AdminUserInput): AdminUserInput {
  if (input === null || typeof input !== "object" || Array.isArray(input)) {
    throw new AdminError("invalid", "invalid user request");
  }
  const allowed = new Set(["username", "machineIds", "admin", "disabled", "password"]);
  for (const key of Object.keys(input)) {
    if (!allowed.has(key)) throw new AdminError("invalid", "unexpected user field");
  }
  validateUsername(input.username, "username");
  validateScope(input.machineIds, "machineIds");
  if (typeof input.admin !== "boolean") throw new AdminError("invalid", "admin must be a boolean");
  if (input.disabled !== undefined && typeof input.disabled !== "boolean") {
    throw new AdminError("invalid", "disabled must be a boolean");
  }
  if (input.password !== undefined) {
    if (
      typeof input.password !== "string" ||
      input.password.length === 0 ||
      input.password.length > MAX_BROWSER_PASSWORD_CHARS
    ) {
      throw new AdminError(
        "invalid",
        `password must be a string of 1..${MAX_BROWSER_PASSWORD_CHARS} characters`,
      );
    }
  }
  return {
    username: input.username,
    machineIds: normalizeScope(input.machineIds),
    admin: input.admin,
    ...(input.disabled !== undefined ? { disabled: input.disabled } : {}),
    ...(input.password !== undefined ? { password: input.password } : {}),
  };
}

function validateUsername(value: unknown, field: string): string {
  if (typeof value !== "string" || !USERNAME_PATTERN.test(value)) {
    throw new AdminError(
      "invalid",
      `${field} must be 1..128 printable ASCII characters without spaces`,
    );
  }
  return value;
}

function validateScope(value: unknown, field: string): void {
  if (value === "*") return;
  if (!Array.isArray(value)) {
    throw new AdminError("invalid", `${field} must be "*" or an array of IDs`);
  }
  const seen = new Set<string>();
  let bytes = 2;
  for (const id of value) {
    if (typeof id !== "string" || !USERNAME_PATTERN.test(id)) {
      throw new AdminError("invalid", `${field} contains an invalid identifier`);
    }
    bytes += Buffer.byteLength(id, "utf8") + 3;
    if (bytes > MAX_SCOPE_BYTES) {
      throw new AdminError("invalid", `${field} exceeds its size budget`);
    }
    if (seen.has(id)) throw new AdminError("invalid", `${field} contains a duplicate identifier`);
    seen.add(id);
  }
}

function normalizeScope(scope: BrowserMachineScope): BrowserMachineScope {
  return scope === "*" ? "*" : [...scope];
}

function serializeScope(scope: BrowserMachineScope): string {
  return scope === "*" ? "*" : JSON.stringify(scope);
}

function parseScope(json: string): BrowserMachineScope | null {
  if (json === "*") return "*";
  if (Buffer.byteLength(json, "utf8") > MAX_SCOPE_BYTES) return null;
  try {
    const value: unknown = JSON.parse(json);
    if (!Array.isArray(value)) return null;
    const ids: string[] = [];
    for (const id of value) {
      if (typeof id !== "string" || !USERNAME_PATTERN.test(id)) return null;
      ids.push(id);
    }
    return ids;
  } catch {
    return null;
  }
}

function userFromRow(row: UserRow): BrowserUserConfig | null {
  const scope = parseScope(row.scope_json);
  if (!scope || !isPasswordHash(row.password_hash)) return null;
  return {
    username: row.username,
    passwordHash: row.password_hash,
    machineIds: scope,
    ...(Number(row.is_admin) === 1 ? { admin: true } : {}),
  };
}

/** Async salted scrypt using the exact hash format of auth/password.ts. */
async function hashPasswordAsync(password: string): Promise<string> {
  const parts = BROWSER_HASH_PREFIX.split("$");
  const n = Number(parts[1]);
  const r = Number(parts[2]);
  const p = Number(parts[3]);
  const salt = randomBytes(SALT_BYTES);
  const key = await new Promise<Buffer>((resolve, reject) => {
    scrypt(
      password.normalize("NFKC"),
      salt,
      KEY_BYTES,
      { N: n, r, p, maxmem: 64 * 1024 * 1024 },
      (error, derived) => (error ? reject(error) : resolve(derived)),
    );
  });
  const hash = `${BROWSER_HASH_PREFIX}${salt.toString("hex")}$${key.toString("hex")}`;
  if (!isPasswordHash(hash)) throw new Error("derived password hash has an unexpected shape");
  return hash;
}
