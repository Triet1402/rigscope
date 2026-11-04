/**
 * Browser user configuration (HUB_BROWSER_USERS) parsing and validation.
 *
 * Expected value: a JSON array of exactly
 *   {username, passwordHash, machineIds}
 * where passwordHash is a salted scrypt hash (see password.ts) — never a
 * plaintext password — and machineIds is either "*" (all machines on this
 * hub) or a hub-local machine-ID array (an empty array grants no machines).
 * An optional `admin: true` marks an explicit administrator. The legacy
 * three-field shape remains valid and is never treated as an administrator.
 *
 * A missing or empty variable configures no browser users: the hub stays up,
 * login reports 503 and every browser data path fails closed. There is never
 * an anonymous fallback. Malformed values fail startup nonzero, matching
 * HUB_COLLECTORS handling.
 */

import { HubConfigError } from "../config";
import { isPasswordHash, MAX_BROWSER_PASSWORD_CHARS } from "./password";
import { DEFAULT_SESSION_TTL_MS } from "./sessions";

/** Bound the bootstrap user directory independently of machine capacity. */
export const MAX_BROWSER_USERS = 50;
/** Same printable-ASCII bound as collector/machine identifiers. */
const MAX_ID_LENGTH = 128;
const USERNAME_PATTERN = /^[\x21-\x7e]{1,128}$/;

export type BrowserMachineScope = "*" | readonly string[];

export type BrowserUserConfig = {
  username: string;
  passwordHash: string;
  machineIds: BrowserMachineScope;
  /**
   * Explicit administrative permission. Omitted/false for legacy users; never
   * inferred from a wildcard machine scope.
   */
  admin?: boolean;
};

/** Shorten fixed development sessions to exercise expiry through a real proxy. */
export function parseDevelopmentSessionTtl(env: Record<string, string | undefined>): number {
  const value = env.HUB_DEV_SESSION_TTL_MS;
  if (value === undefined || value === "") return DEFAULT_SESSION_TTL_MS;
  if (env.NODE_ENV !== "development") {
    throw new HubConfigError("HUB_DEV_SESSION_TTL_MS is only allowed in development");
  }
  const ttl = Number(value);
  if (!/^[1-9]\d*$/.test(value) || !Number.isSafeInteger(ttl) || ttl < 1_000 || ttl > DEFAULT_SESSION_TTL_MS) {
    throw new HubConfigError("HUB_DEV_SESSION_TTL_MS must be an integer from 1000 to 43200000");
  }
  return ttl;
}

export function parseBrowserUsers(
  env: Record<string, string | undefined>,
): BrowserUserConfig[] {
  const value = env.HUB_BROWSER_USERS;
  if (value === undefined || value === "") return [];
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    throw new HubConfigError("HUB_BROWSER_USERS is not valid JSON");
  }
  if (!Array.isArray(parsed)) {
    throw new HubConfigError("HUB_BROWSER_USERS must be a JSON array");
  }
  if (parsed.length > MAX_BROWSER_USERS) {
    throw new HubConfigError(
      `HUB_BROWSER_USERS has ${parsed.length} entries; at most ${MAX_BROWSER_USERS} browser users are supported`,
    );
  }
  const users: BrowserUserConfig[] = [];
  const usernames = new Set<string>();
  for (const [index, item] of parsed.entries()) {
    const message = `HUB_BROWSER_USERS[${index}]`;
    if (item === null || typeof item !== "object" || Array.isArray(item)) {
      throw new HubConfigError(`${message} must be an object`);
    }
    const record = item as Record<string, unknown>;
    const required = ["machineIds", "passwordHash", "username"];
    for (const key of Object.keys(record)) {
      if (key !== "admin" && !required.includes(key)) {
        throw new HubConfigError(
          `${message} must contain exactly username, passwordHash, and machineIds (admin is optional)`,
        );
      }
    }
    for (const key of required) {
      if (record[key] === undefined) {
        throw new HubConfigError(
          `${message} must contain exactly username, passwordHash, and machineIds (admin is optional)`,
        );
      }
    }
    if (record.admin !== undefined && typeof record.admin !== "boolean") {
      throw new HubConfigError(`${message}: admin must be a boolean`);
    }
    const username = boundedId(record.username, "username", message);
    if (usernames.has(username)) {
      throw new HubConfigError(`${message}: duplicate username "${username}"`);
    }
    usernames.add(username);
    const passwordHash = record.passwordHash;
    if (!isPasswordHash(passwordHash)) {
      const detail = typeof passwordHash === "string"
        ? "plaintext passwords are not accepted"
        : "got a non-string value";
      throw new HubConfigError(
        `${message}: passwordHash must be a scrypt hash with the format ` +
          `"scrypt$<N>$<r>$<p>$<salt-hex>$<key-hex>" (scripts/hash-browser-password.cjs); ${detail}`,
      );
    }
    if (passwordHash.length > MAX_BROWSER_PASSWORD_CHARS * 4) {
      throw new HubConfigError(`${message}: passwordHash is too long`);
    }
    const user: BrowserUserConfig = {
      username,
      passwordHash,
      machineIds: parseScope(record.machineIds, message),
    };
    if (record.admin === true) user.admin = true;
    users.push(user);
  }
  return users;
}

function parseScope(value: unknown, message: string): BrowserMachineScope {
  if (value === "*") return "*";
  if (!Array.isArray(value)) {
    throw new HubConfigError(`${message}: machineIds must be "*" or an array of machine IDs`);
  }
  const ids: string[] = [];
  const seen = new Set<string>();
  for (const [i, id] of value.entries()) {
    const machineId = boundedId(id, `machineIds[${i}]`, message);
    if (seen.has(machineId)) {
      throw new HubConfigError(`${message}: duplicate machineIds[${i}] "${machineId}"`);
    }
    seen.add(machineId);
    ids.push(machineId);
  }
  return ids;
}

function boundedId(value: unknown, field: string, message: string): string {
  if (typeof value !== "string" || !USERNAME_PATTERN.test(value)) {
    throw new HubConfigError(
      `${message}: ${field} must be 1..128 printable ASCII characters without spaces`,
    );
  }
  return value;
}
