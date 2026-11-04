/** Authoritative private collector registry, grants and recoverable enrollment. */
import { createHash, randomBytes, randomUUID, timingSafeEqual } from 'node:crypto';
import type { DatabaseSync } from 'node:sqlite';
import type { HubDatabase } from '../storage/db';
import type { HubIdentityConfig } from '../config';
import type { EnrollmentGrant } from './store';

export function installRegistrySchema(db: DatabaseSync): void {
  db.exec(`
    CREATE TABLE IF NOT EXISTS collector_registry (
      collector_id TEXT PRIMARY KEY, machine_id TEXT NOT NULL UNIQUE, token TEXT NOT NULL UNIQUE,
      revoked INTEGER NOT NULL DEFAULT 0, created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL
    );
    CREATE TABLE IF NOT EXISTS collector_registry_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
    CREATE TABLE IF NOT EXISTS collector_grants (
      token_hash TEXT PRIMARY KEY, expires_at_ms INTEGER NOT NULL, replace_machine_id TEXT,
      consumed_by TEXT, request_id TEXT, recovery_hash TEXT, recovery_until_ms INTEGER, actor TEXT NOT NULL
    );
    CREATE TABLE IF NOT EXISTS collector_audit (
      id INTEGER PRIMARY KEY, at_ms INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL
    );
  `);
}

type GrantRow = { token_hash: string; expires_at_ms: number; replace_machine_id: string | null;
  consumed_by: string | null; request_id: string | null; recovery_hash: string | null; recovery_until_ms: number | null };
export type EnrollRequest = { collectorId: string; token: string; requestId?: string; recoverySecret?: string };
export class RegistryError extends Error {
  constructor(readonly code: string, message: string) { super(message); }
}
const idPattern = /^[\x21-\x7e]{1,128}$/;
const hash = (text: string) => createHash('sha256').update(text).digest('hex');
const secret = () => randomBytes(32).toString('base64url');

export class CollectorRegistry {
  private readonly db: DatabaseSync;
  private readonly now: () => number;
  constructor(options: { database: HubDatabase; legacyIdentities: readonly HubIdentityConfig[];
    consumedGrantHashes?: readonly string[]; grants?: EnrollmentGrant[]; now?: () => number }) {
    this.db = options.database.db; this.now = options.now ?? Date.now;
    // Schema installation normally runs as a versioned migration; explicit use
    // also supports isolated registry tests before the composition root starts.
    installRegistrySchema(this.db);
    this.transaction(() => {
      if (!this.db.prepare("SELECT 1 FROM collector_registry_meta WHERE key='bootstrapped'").get()) {
        const insert = this.db.prepare('INSERT INTO collector_registry VALUES(?,?,?,0,?,?)');
        for (const entry of options.legacyIdentities) insert.run(entry.collectorId, entry.machineId, entry.token, this.now(), this.now());
        if (options.legacyIdentities.length > 0 || (options.grants?.length ?? 0) > 0 || (options.consumedGrantHashes?.length ?? 0) > 0)
          this.db.prepare("INSERT INTO collector_registry_meta VALUES('bootstrapped','1')").run();
      }
      for (const grant of options.grants ?? []) {
        const expiresAt = Date.parse(grant.expiresAt);
        if (!Number.isFinite(expiresAt)) throw new RegistryError('invalid-config', 'invalid enrollment expiry');
        this.db.prepare('INSERT OR IGNORE INTO collector_grants(token_hash,expires_at_ms,actor) VALUES(?,?,?)')
          .run(hash(grant.token), expiresAt, 'bootstrap');
      }
      for (const tokenHash of options.consumedGrantHashes ?? []) {
        if (!/^[a-f0-9]{64}$/.test(tokenHash)) throw new RegistryError('invalid-state', 'invalid legacy grant hash');
        this.db.prepare("INSERT INTO collector_grants(token_hash,expires_at_ms,consumed_by,actor) VALUES(?,0,'legacy-consumed','migration') ON CONFLICT(token_hash) DO UPDATE SET consumed_by=COALESCE(consumed_by,'legacy-consumed')").run(tokenHash);
      }
    });
  }
  identities(includeRevoked = false): HubIdentityConfig[] {
    return (this.db.prepare('SELECT collector_id,machine_id,token FROM collector_registry' + (includeRevoked ? '' : ' WHERE revoked=0')).all() as
      { collector_id: string; machine_id: string; token: string }[]).map(r => ({ collectorId: r.collector_id, machineId: r.machine_id, token: r.token }));
  }
  list(): { collectorId: string; machineId: string; revoked: boolean; updatedAt: string }[] {
    return (this.db.prepare('SELECT collector_id,machine_id,revoked,updated_at_ms FROM collector_registry ORDER BY machine_id').all() as
      { collector_id: string; machine_id: string; revoked: number; updated_at_ms: number }[]).map(r => ({ collectorId: r.collector_id,
        machineId: r.machine_id, revoked: Boolean(r.revoked), updatedAt: new Date(r.updated_at_ms).toISOString() }));
  }
  createGrant(actor: string, options: { replaceMachineId?: string; ttlMs?: number } = {}): { token: string; expiresAt: string } {
    const ttl = options.ttlMs ?? 15 * 60000;
    if (!Number.isSafeInteger(ttl) || ttl < 60000 || ttl > 15 * 60000) throw new RegistryError('invalid-input', 'grant lifetime must be 1 to 15 minutes');
    const target = options.replaceMachineId;
    if (target !== undefined && (!idPattern.test(target) || !this.db.prepare('SELECT 1 FROM collector_registry WHERE machine_id=?').get(target)))
      throw new RegistryError('invalid-input', 'replacement machine does not exist');
    const token = secret(); const expires = this.now() + ttl;
    this.transaction(() => {
      this.db.prepare("INSERT OR IGNORE INTO collector_registry_meta VALUES('bootstrapped','1')").run();
      this.db.prepare('DELETE FROM collector_grants WHERE expires_at_ms < ? AND (recovery_until_ms IS NULL OR recovery_until_ms < ?)').run(this.now(), this.now());
      const count = this.db.prepare('SELECT COUNT(*) AS n FROM collector_grants').get() as { n: number };
      if (count.n >= 256) throw new RegistryError('cap-reached', 'enrollment grant budget exhausted');
      this.db.prepare('INSERT INTO collector_grants(token_hash,expires_at_ms,replace_machine_id,actor) VALUES(?,?,?,?)')
        .run(hash(token), expires, target ?? null, actor);
      this.audit(actor, target ? 'grant-replacement' : 'grant-create', target ?? 'new-installation');
    });
    return { token, expiresAt: new Date(expires).toISOString() };
  }
  enroll(input: EnrollRequest): HubIdentityConfig {
    if (!idPattern.test(input.collectorId) || !/^[\x21-\x7e]{1,256}$/.test(input.token)) throw new RegistryError('invalid-input', 'invalid enrollment input');
    const recoverable = input.requestId !== undefined || input.recoverySecret !== undefined;
    if (recoverable && (!idPattern.test(input.requestId ?? '') || !/^[A-Za-z0-9_-]{32,128}$/.test(input.recoverySecret ?? '')))
      throw new RegistryError('invalid-input', 'invalid enrollment recovery proof');
    return this.transaction(() => {
      const grant = this.db.prepare('SELECT * FROM collector_grants WHERE token_hash=?').get(hash(input.token)) as GrantRow | undefined;
      if (!grant) throw new RegistryError('unknown-token', 'enrollment denied');
      if (grant.consumed_by !== null) {
        const proof = hash(input.recoverySecret ?? '');
        if (grant.consumed_by !== input.collectorId || grant.request_id !== input.requestId ||
          !grant.recovery_hash || !timingSafeEqual(Buffer.from(proof), Buffer.from(grant.recovery_hash)) ||
          (grant.recovery_until_ms ?? 0) <= this.now()) throw new RegistryError('token-reused', 'enrollment denied');
        const existing = this.identities().find(i => i.collectorId === input.collectorId);
        if (!existing) throw new RegistryError('token-reused', 'enrollment denied');
        return existing;
      }
      if (grant.expires_at_ms <= this.now()) throw new RegistryError('token-expired', 'enrollment denied');
      const existing = this.db.prepare('SELECT machine_id FROM collector_registry WHERE collector_id=?').get(input.collectorId) as { machine_id: string } | undefined;
      if (existing && existing.machine_id !== grant.replace_machine_id) throw new RegistryError('duplicate-collector', 'explicit replacement grant required');
      let machineId = grant.replace_machine_id;
      if (machineId) {
        // A replacement grant names an existing hub-local mapping explicitly.
        // Invalidate all old recovery proofs before changing its identity/token.
        const previous = this.db.prepare('SELECT collector_id FROM collector_registry WHERE machine_id=?').get(machineId) as { collector_id: string } | undefined;
        if (!previous) throw new RegistryError('invalid-state', 'replacement mapping disappeared');
        this.db.prepare('UPDATE collector_grants SET recovery_hash=NULL WHERE consumed_by=?').run(previous.collector_id);
        this.db.prepare('DELETE FROM collector_registry WHERE machine_id=?').run(machineId);
      } else {
        // Hub-local machine mappings are identified by UUID and bounded by
        // persisted database/input budgets, not an arbitrary machine count.
        machineId = randomUUID();
      }
      const result = { collectorId: input.collectorId, machineId, token: secret() };
      this.db.prepare('INSERT INTO collector_registry VALUES(?,?,?,0,?,?)')
        .run(result.collectorId, result.machineId, result.token, this.now(), this.now());
      this.db.prepare('UPDATE collector_grants SET consumed_by=?,request_id=?,recovery_hash=?,recovery_until_ms=? WHERE token_hash=?')
        .run(result.collectorId, input.requestId ?? null, recoverable ? hash(input.recoverySecret!) : null,
          recoverable ? this.now() + 24 * 60 * 60000 : null, grant.token_hash);
      this.audit('collector', grant.replace_machine_id ? 'enroll-replacement' : 'enroll', result.collectorId);
      return result;
    });
  }
  revoke(actor: string, collectorId: string): void {
    this.transaction(() => {
      if (!this.db.prepare('SELECT 1 FROM collector_registry WHERE collector_id=?').get(collectorId)) throw new RegistryError('invalid-input', 'collector does not exist');
      this.db.prepare('UPDATE collector_registry SET revoked=1,updated_at_ms=? WHERE collector_id=?').run(this.now(), collectorId);
      this.db.prepare('UPDATE collector_grants SET recovery_hash=NULL WHERE consumed_by=?').run(collectorId);
      this.audit(actor, 'revoke', collectorId);
    });
  }
  auditEvents(): { at: string; actor: string; action: string; target: string }[] {
    return (this.db.prepare('SELECT at_ms,actor,action,target FROM collector_audit ORDER BY id DESC LIMIT 1000').all() as
      { at_ms: number; actor: string; action: string; target: string }[]).map(row =>
      ({ at: new Date(row.at_ms).toISOString(), actor: row.actor, action: row.action, target: row.target }));
  }
  private audit(actor: string, action: string, target: string): void {
    this.db.prepare('INSERT INTO collector_audit(at_ms,actor,action,target) VALUES(?,?,?,?)').run(this.now(), actor, action, target);
    this.db.exec('DELETE FROM collector_audit WHERE id NOT IN (SELECT id FROM collector_audit ORDER BY id DESC LIMIT 1000)');
  }
  private transaction<T>(fn: () => T): T {
    this.db.exec('BEGIN IMMEDIATE');
    try { const value = fn(); this.db.exec('COMMIT'); return value; }
    catch (error) { try { this.db.exec('ROLLBACK'); } catch {} throw error; }
  }
}
