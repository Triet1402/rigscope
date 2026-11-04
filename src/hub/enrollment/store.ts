/**
 * Hub-local enrollment store.
 *
 * Bridges the spike's static `HUB_COLLECTORS` registry toward real enrollment
 * (ARCHITECTURE.md "Identity, enrollment, and messages"): a collector claims a
 * short-lived, single-use bootstrap enrollment grant over the (parent-wired)
 * enrollment route, and the hub issues a hub-local credential in return:
 *
 * - the hub generates the identity, never accepting a caller-supplied
 *   machineId; the machineId is a hub-local UUID, and the bearer token is an
 *   independent random 32-byte secret;
 * - each grant is bound to its first claimant; replay, expiry, unknown
 *   tokens, duplicate collector identities, and duplicate mappings are
 *   rejected explicitly;
 * - the combined identity registry (static + enrolled) is bounded by the
 *   persisted state byte budget, not an arbitrary machine count;
 * - consumed grants (hashed) and identity mappings are persisted to a
 *   restrictive 0600 file inside a private 0700 directory with an atomic
 *   write-before-success protocol; a corrupt or unsafe state file fails
 *   closed at construction instead of being silently discarded or rebuilt.
 *
 * Secrets are never logged: the module performs no console output and no
 * error message contains token material. Bootstrap tokens are persisted only
 * as SHA-256 hashes (the consumed set). The issued collector bearer token is
 * itself a credential the hub must load on restart, so it lives only in the
 * restricted state file, never in logs or in the consumed set.
 *
 * All APIs are synchronous. File operations happen only on construction and
 * enrollment (low-frequency admin operations), never on the telemetry path.
 */

import { createHash, randomBytes, randomUUID } from "node:crypto";
import { closeSync, fsyncSync, mkdirSync, openSync, readFileSync, renameSync, lstatSync, writeFileSync, unlinkSync } from "node:fs";
import { dirname } from "node:path";
import { type HubIdentityConfig } from "../config";
import { acquireProcessLease, ProcessLeaseError } from "../../runtime/process-lease";

const STATE_VERSION = 1;
const MAX_COLLECTOR_ID_LENGTH = 128;
const MAX_MACHINE_ID_LENGTH = 128;
const MAX_TOKEN_LENGTH = 256;
/**
 * Serialized state guard. This is a byte budget rather than an identity
 * count: each bounded enrollment entry is a few hundred bytes, so the budget
 * comfortably holds well over fifty machines while bounding file reads,
 * JSON parsing and atomic rewrites.
 */
const MAX_STATE_BYTES = 128 * 1024;
/** Work bound for bootstrap grants supplied at construction. */
const MAX_GRANTS_BYTES = 64 * 1024;
const BEARER_BYTES = 32;
export const MAX_ENROLLMENT_TTL_MS = 15 * 60 * 1000;

export type EnrollmentGrant = {
  token: string;
  expiresAt: string;
};

export type EnrollmentErrorCode =
  | "invalid-config"
  | "invalid-state"
  | "invalid-input"
  | "cap-reached"
  | "duplicate-collector"
  | "duplicate-mapping"
  | "unknown-token"
  | "token-reused"
  | "token-expired"
  | "persist-failed";

export class EnrollmentStoreError extends Error {
  readonly code: EnrollmentErrorCode;
  constructor(code: EnrollmentErrorCode, message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "EnrollmentStoreError";
    this.code = code;
  }
}

export type EnrollInput = {
  collectorId: string;
  token: string;
};

/** Persisted issued identity; reconstruction of the parent's HubIdentityConfig. */
type PersistedIdentity = {
  collectorId: string;
  machineId: string;
  token: string;
  createdAt: string;
};

/** Persisted grant claim; the bootstrap token appears only as its hash. */
type PersistedConsumption = {
  tokenHash: string;
  collectorId: string;
  claimedAt: string;
};

type PersistedState = {
  version: number;
  enrollments: PersistedIdentity[];
  consumed: PersistedConsumption[];
};

type Grant = {
  tokenHash: string;
  expiresAt: number;
};

export type EnrollmentStoreOptions = {
  /** Path of the state file; its parent directory is kept private (0700). */
  path: string;
  /** Static registration from HUB_COLLECTORS (or an empty array). */
  identities: readonly HubIdentityConfig[];
  /** Short-lived, single-use bootstrap enrollment grants. */
  grants: readonly EnrollmentGrant[];
  /** Injectable clock for tests; real time otherwise. */
  now?: () => Date;
};

export class EnrollmentStore {
  readonly storePath: string;
  private staticIdentities: readonly HubIdentityConfig[] = [];
  private readonly staticByCollector = new Map<string, HubIdentityConfig>();
  private readonly staticByMachine = new Map<string, HubIdentityConfig>();
  private readonly staticTokens = new Set<string>();
  private readonly now: () => Date;
  private readonly grants = new Map<string, Grant>();
  private readonly enrollments: PersistedIdentity[] = [];
  private readonly consumed: PersistedConsumption[] = [];
  private readonly consumedHashes = new Set<string>();

  constructor(options: EnrollmentStoreOptions) {
    if (typeof options.path !== "string" || options.path.length === 0 || options.path.length > 512) {
      throw new EnrollmentStoreError("invalid-config", "state path must be a nonempty path of at most 512 characters");
    }
    this.storePath = options.path;
    this.now = options.now ?? (() => new Date());
    this.applyStaticIdentities(options.identities);
    this.applyGrants(options.grants);
    this.ensurePrivateDirectory();
    this.loadState();
  }

  /**
   * Combined registry for credential lookup: the static identities followed by
   * enrolled ones in enrollment order. Returns defensive copies.
   */
  identities(): HubIdentityConfig[] {
    const enrolled = this.enrollments.map((entry) => ({
      collectorId: entry.collectorId,
      machineId: entry.machineId,
      token: entry.token,
    }));
    return [...this.staticIdentities.map((identity) => ({ ...identity })), ...enrolled];
  }

  /** Number of enrolled (persisted) identities, excluding the static registry. */
  enrolledCount(): number {
    return this.enrollments.length;
  }

  /** Migration exposes only hashes, so spent bootstrap grants stay spent. */
  consumedGrantHashes(): string[] { return [...this.consumedHashes]; }

  /**
   * Claims one bootstrap enrollment grant and issues a hub-local identity.
   * Persists the consumption and mapping atomically before returning success;
   * callers may respond 401/403/409 from the error code.
   */
  enroll(input: EnrollInput): HubIdentityConfig {
    let releaseLock: () => void;
    try {
      // Shared runtime lease: live owners refuse; proven-dead owners recover;
      // incomplete/unsafe legacy lock files fail closed for manual inspection.
      releaseLock = acquireProcessLease(`${this.storePath}.lock`);
    } catch (error) {
      if (error instanceof ProcessLeaseError) {
        if (error.code === "busy") {
          throw new EnrollmentStoreError(
            "persist-failed",
            "enrollment state is locked by another process; stop it before retrying",
          );
        }
        throw new EnrollmentStoreError(
          "persist-failed",
          "enrollment state is locked by an unsafe or incomplete lock file; inspect it before recovery",
        );
      }
      throw new EnrollmentStoreError(
        "persist-failed",
        "enrollment state lock could not be acquired; check the state directory permissions",
      );
    }
    try {
      // Another hub using this file must not overwrite a consumed grant.
      const previousEnrollments = [...this.enrollments];
      const previousConsumed = [...this.consumed];
      try {
        this.enrollments.length = 0;
        this.consumed.length = 0;
        this.consumedHashes.clear();
        this.loadState();
      } catch (error) {
        // A failed reload cannot publish a partially validated registry.
        this.enrollments.splice(0, this.enrollments.length, ...previousEnrollments);
        this.consumed.splice(0, this.consumed.length, ...previousConsumed);
        this.consumedHashes.clear();
        for (const claim of previousConsumed) this.consumedHashes.add(claim.tokenHash);
        throw error;
      }
      return this.issue(input);
    } finally {
      releaseLock();
    }
  }

  private issue(input: EnrollInput): HubIdentityConfig {
    const fields = Object.keys(input).sort();
    const expected = ["collectorId", "token"];
    if (
      fields.length !== expected.length ||
      fields.some((field, index) => field !== expected[index])
    ) {
      throw new EnrollmentStoreError(
        "invalid-input",
        "enrollment input must contain exactly collectorId and token; the machineId is generated by this hub and never accepted from callers",
      );
    }
    const collectorId = boundedField(input.collectorId, "collectorId", MAX_COLLECTOR_ID_LENGTH, "invalid-input");

    if (this.staticByCollector.has(collectorId) || this.enrollments.some((entry) => entry.collectorId === collectorId)) {
      throw new EnrollmentStoreError(
        "duplicate-collector",
        `collectorId "${collectorId}" already has an identity; reassociation is an explicit administrator operation`,
      );
    }

    const token = boundedField(input.token, "token", MAX_TOKEN_LENGTH, "invalid-input");
    const tokenHash = hashToken(token);
    const grant = this.grants.get(tokenHash);
    if (!grant) {
      throw new EnrollmentStoreError("unknown-token", "this enrollment token does not match a grant");
    }
    if (this.consumedHashes.has(tokenHash)) {
      throw new EnrollmentStoreError("token-reused", "this enrollment token has already been claimed");
    }
    if (this.now().getTime() >= grant.expiresAt) {
      throw new EnrollmentStoreError("token-expired", "this enrollment token has expired");
    }

    // Generate hub-local identity material. Independent of each other and of
    // anything the caller sent; regenerate on a (theoretically impossible)
    // collision with an existing mapping or credential.
    let machineId = randomUUID();
    let issued = randomBytes(BEARER_BYTES).toString("base64url");
    if (
      this.staticByMachine.has(machineId) ||
      this.enrollments.some((entry) => entry.machineId === machineId) ||
      this.staticTokens.has(issued) ||
      this.enrollments.some((entry) => entry.token === issued)
    ) {
      machineId = randomUUID();
      issued = randomBytes(BEARER_BYTES).toString("base64url");
    }
    if (
      this.staticByMachine.has(machineId) ||
      this.enrollments.some((entry) => entry.machineId === machineId) ||
      this.staticTokens.has(issued) ||
      this.enrollments.some((entry) => entry.token === issued)
    ) {
      throw new EnrollmentStoreError("duplicate-mapping", "generated identity collided with an existing mapping");
    }

    const enrolled: PersistedIdentity = {
      collectorId,
      machineId,
      token: issued,
      createdAt: this.now().toISOString(),
    };
    const consumption: PersistedConsumption = {
      tokenHash,
      collectorId,
      claimedAt: this.now().toISOString(),
    };

    // Stage into copies and persist first; an I/O failure burns nothing, so a
    // grant stays claimable once storage recovers rather than being lost.
    const enrollments = [...this.enrollments, enrolled];
    const consumed = [...this.consumed, consumption];
    this.persistState({ enrollments, consumed });

    // Persisted file is authoritative at this point; memory follows before
    // success is reported to the caller.
    this.enrollments.push(enrolled);
    this.consumed.push(consumption);
    this.consumedHashes.add(tokenHash);
    return { collectorId, machineId, token: issued };
  }

  private applyStaticIdentities(identities: readonly HubIdentityConfig[]): void {
    const clean: HubIdentityConfig[] = [];
    for (const [index, item] of identities.entries()) {
      if (item === null || typeof item !== "object" || Array.isArray(item)) {
        throw new EnrollmentStoreError("invalid-config", `identity ${index} must be an object`);
      }
      const collectorId = boundedField(item.collectorId, "collectorId", MAX_COLLECTOR_ID_LENGTH, "invalid-config");
      const machineId = boundedField(item.machineId, "machineId", MAX_MACHINE_ID_LENGTH, "invalid-config");
      const token = boundedField(item.token, "token", MAX_TOKEN_LENGTH, "invalid-config");
      if (this.staticByCollector.has(collectorId)) {
        throw new EnrollmentStoreError("invalid-config", `duplicate collectorId "${collectorId}"`);
      }
      if (this.staticByMachine.has(machineId)) {
        throw new EnrollmentStoreError("invalid-config", `duplicate machineId`);
      }
      if (this.staticTokens.has(token)) {
        throw new EnrollmentStoreError("invalid-config", "duplicate credential token");
      }
      this.staticByCollector.set(collectorId, { collectorId, machineId, token });
      this.staticByMachine.set(machineId, { collectorId, machineId, token });
      this.staticTokens.add(token);
      clean.push({ collectorId, machineId, token });
    }
    this.staticIdentities = clean;
  }

  private applyGrants(grants: readonly EnrollmentGrant[]): void {
    if (!Array.isArray(grants)) throw new EnrollmentStoreError("invalid-config", "too many enrollment grants");
    // Bound bootstrap-grant work by the token bytes they carry (plus a small
    // per-entry overhead) rather than by the number of machines they could
    // enroll.
    let grantBytes = 2;
    for (const item of grants) {
      if (item !== null && typeof item === "object" && !Array.isArray(item) && typeof item.token === "string") {
        grantBytes += Buffer.byteLength(item.token, "utf8") + 64;
      }
    }
    if (grantBytes > MAX_GRANTS_BYTES) throw new EnrollmentStoreError("invalid-config", "too many enrollment grants");
    for (const [index, item] of grants.entries()) {
      if (item === null || typeof item !== "object" || Array.isArray(item)) {
        throw new EnrollmentStoreError("invalid-config", `grant ${index} must be an object`);
      }
      const keys = Object.keys(item).sort();
      if (keys.length !== 2 || keys[0] !== "expiresAt" || keys[1] !== "token") {
        throw new EnrollmentStoreError("invalid-config", "grant must contain exactly token and expiresAt");
      }
      const token = boundedField(item.token, "token", MAX_TOKEN_LENGTH, "invalid-config");
      const tokenHash = hashToken(token);
      if (this.grants.has(tokenHash)) {
        throw new EnrollmentStoreError("invalid-config", "duplicate grant token");
      }
      if (this.staticTokens.has(token)) {
        throw new EnrollmentStoreError("invalid-config", "a grant token must not equal a static credential token");
      }
      const expiresAt = parseExpiry(item.expiresAt);
      if (Number.isNaN(expiresAt)) {
        throw new EnrollmentStoreError("invalid-config", "grant expiresAt must be an ISO 8601 UTC instant");
      }
      if (expiresAt - this.now().getTime() > MAX_ENROLLMENT_TTL_MS) throw new EnrollmentStoreError("invalid-config", "enrollment grants may live at most fifteen minutes");
      this.grants.set(tokenHash, { tokenHash, expiresAt });
    }
  }

  private ensurePrivateDirectory(): void {
    const parent = dirname(this.storePath);
    mkdirSync(parent, { recursive: true, mode: 0o700 });
    const stat = lstatSync(parent);
    if (!stat.isDirectory() || (stat.mode & 0o077) !== 0) throw new EnrollmentStoreError("invalid-config", "enrollment state requires a private directory");
  }

  private loadState(): void {
    const stats = statOrNull(this.storePath);
    if (!stats) {
      return; // First run: no persisted state yet.
    }
    if (!stats.isFile() || stats.size > MAX_STATE_BYTES) {
      throw new EnrollmentStoreError("invalid-state", "state path does not refer to a regular file");
    }
    // Fail closed on loosened permissions before reading credential material.
    if ((stats.mode & 0o077) !== 0) {
      throw new EnrollmentStoreError("invalid-state", "state file must be private (0600)");
    }
    let raw: string;
    try {
      raw = readFileSync(this.storePath, "utf8");
    } catch (error) {
      throw new EnrollmentStoreError("invalid-state", "state file is unreadable", { cause: error });
    }
    if (Buffer.byteLength(raw, "utf8") > MAX_STATE_BYTES) {
      throw new EnrollmentStoreError("invalid-state", "state file exceeds its size bound");
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      throw new EnrollmentStoreError("invalid-state", "state file is not valid JSON");
    }
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      throw new EnrollmentStoreError("invalid-state", "state file must be a JSON object");
    }
    const state = parsed as Record<string, unknown>;
    if (state.version !== STATE_VERSION) {
      throw new EnrollmentStoreError("invalid-state", "state file has an unsupported version");
    }
    if (!Array.isArray(state.enrollments) || !Array.isArray(state.consumed)) {
      throw new EnrollmentStoreError("invalid-state", "state file must contain enrollments and consumed arrays");
    }
    const unseen = new Set(this.staticTokens);
    const seenCollectors = new Set<string>();
    const seenMachines = new Set<string>();
    for (const [index, item] of state.enrollments.entries()) {
      const entry = parseObject(item, "invalid-state");
      const collectorId = fieldOf(entry, index, "collectorId", MAX_COLLECTOR_ID_LENGTH);
      const machineId = fieldOf(entry, index, "machineId", MAX_MACHINE_ID_LENGTH);
      const token = fieldOf(entry, index, "token", MAX_TOKEN_LENGTH);
      const createdAt = fieldOf(entry, index, "createdAt", 32);
      if (seenCollectors.has(collectorId) || this.staticByCollector.has(collectorId)) {
        throw new EnrollmentStoreError("invalid-state", "state file contains a duplicate collector identity");
      }
      if (seenMachines.has(machineId) || this.staticByMachine.has(machineId)) {
        throw new EnrollmentStoreError("invalid-state", "state file contains a duplicate machine mapping");
      }
      if (unseen.has(token)) {
        throw new EnrollmentStoreError("invalid-state", "state file contains a duplicate credential token");
      }
      seenCollectors.add(collectorId);
      seenMachines.add(machineId);
      unseen.add(token);
      this.enrollments.push({
        collectorId,
        machineId,
        token,
        createdAt,
      });
    }
    const seenHashes = new Set<string>();
    for (const [index, item] of state.consumed.entries()) {
      const entry = parseObject(item, "invalid-state");
      const tokenHash = fieldOf(entry, index, "tokenHash", 64);
      if (!/^[0-9a-f]{64}$/.test(tokenHash)) {
        throw new EnrollmentStoreError("invalid-state", "state file contains a malformed token hash");
      }
      const consumedCollectorId = fieldOf(entry, index, "collectorId", MAX_COLLECTOR_ID_LENGTH);
      const claimedAt = fieldOf(entry, index, "claimedAt", 32);
      if (seenHashes.has(tokenHash)) {
        throw new EnrollmentStoreError("invalid-state", "state file contains a duplicate consumed token");
      }
      seenHashes.add(tokenHash);
      this.consumed.push({
        tokenHash,
        collectorId: consumedCollectorId,
        claimedAt,
      });
      this.consumedHashes.add(tokenHash);
    }
    if (this.consumed.length !== this.enrollments.length ||
        this.enrollments.some(identity => this.consumed.filter(claim => claim.collectorId === identity.collectorId).length !== 1)) {
      throw new EnrollmentStoreError("invalid-state", "enrollment claims do not match identity mappings");
    }
  }

  private persistState(next: { enrollments: PersistedIdentity[]; consumed: PersistedConsumption[] }): void {
    const state: PersistedState = {
      version: STATE_VERSION,
      enrollments: next.enrollments,
      consumed: next.consumed,
    };
    let body: string;
    try {
      body = JSON.stringify(state);
    } catch (error) {
      throw new EnrollmentStoreError("persist-failed", "state could not be serialized", { cause: error });
    }
    if (Buffer.byteLength(body, "utf8") > MAX_STATE_BYTES) {
      throw new EnrollmentStoreError("persist-failed", "state would exceed its size bound");
    }
    const tempPath = `${this.storePath}.tmp-${randomBytes(8).toString("hex")}`;
    let fd: number | null = null;
    try {
      fd = openSync(tempPath, "wx", 0o600);
      writeFileSync(fd, body, "utf8");
      fsyncSync(fd);
      closeSync(fd);
      fd = null;
      renameSync(tempPath, this.storePath);
      fsyncDirectory(dirname(this.storePath));
    } catch (error) {
      if (fd !== null) closeSync(fd);
      try {
        unlinkSync(tempPath);
      } catch {
        // Nothing more to clean; the failure is reported either way.
      }
      throw new EnrollmentStoreError("persist-failed", "state could not be persisted atomically", { cause: error });
    }
  }
}

function parseObject(value: unknown, code: EnrollmentErrorCode): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new EnrollmentStoreError(code, "unexpected non-object entry in state file");
  }
  return value as Record<string, unknown>;
}

function statOrNull(pathItem: string): import("node:fs").Stats | undefined {
  try {
    return lstatSync(pathItem);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw new EnrollmentStoreError("invalid-state", "state file cannot be inspected");
  }
}

function fieldOf(
  entry: Record<string, unknown>,
  index: number,
  field: string,
  maxLength: number,
): string {
  const value = entry[field];
  if (typeof value !== "string" || value.length === 0 || value.length > maxLength || !/^[\x21-\x7e]+$/.test(value)) {
    throw new EnrollmentStoreError("invalid-state", `state entry ${index} has an invalid ${field}`);
  }
  return value;
}

function boundedField(
  value: unknown,
  field: string,
  maxLength: number,
  code: EnrollmentErrorCode,
): string {
  if (typeof value !== "string" || value.length === 0 || value.length > maxLength || !/^[\x21-\x7e]+$/.test(value)) {
    throw new EnrollmentStoreError(
      code,
      `${field} must be a nonempty string of at most ${maxLength} characters`,
    );
  }
  return value;
}

function parseExpiry(value: unknown): number {
  if (typeof value !== "string") return Number.NaN;
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return Number.NaN;
  // Require a canonical UTC ISO instant so expiry cannot be ambiguous.
  return date.toISOString() === value ? date.getTime() : Number.NaN;
}

function hashToken(token: string): string {
  return createHash("sha256").update(token, "utf8").digest("hex");
}

function fsyncDirectory(directory: string): void {
  let fd: number | null = null;
  try {
    fd = openSync(directory, "r");
    fsyncSync(fd);
  } catch {
    // Directory fsync is best-effort portability hygiene; the file fsync and
    // the rename order already give the required write-before-success.
  } finally {
    if (fd !== null) closeSync(fd);
  }
}
