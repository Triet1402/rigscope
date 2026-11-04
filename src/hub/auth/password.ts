/**
 * Browser user password hashing for the hub (milestone 3 browser access).
 *
 * Format: `scrypt$<N>$<r>$<p>$<salt-hex>$<derived-key-hex>` with fixed
 * parameters and only the salt/key varying. HUB_BROWSER_USERS carries these
 * hashes; plaintext passwords are never accepted in runtime configuration
 * and are never logged. Operators generate hashes with
 * `scripts/hash-browser-password.cjs` (password via stdin).
 *
 * SECURITY-NOTE: keep the format constants in sync with
 * scripts/hash-browser-password.cjs.
 */

import { randomBytes, scrypt, scryptSync, timingSafeEqual } from "node:crypto";

// Node's documented baseline parameters: 16 MiB of memory (~50ms) per guess;
// deliberately not tuned per-core so hashes verify identically everywhere.
const SCRYPT_N = 16384;
const SCRYPT_R = 8;
const SCRYPT_P = 1;
const SALT_BYTES = 16;
const KEY_BYTES = 64;
const MAX_PASSWORD_CHARS = 1024;

const HASH_PREFIX = `scrypt$${SCRYPT_N}$${SCRYPT_R}$${SCRYPT_P}$`;
const HASH_PATTERN = new RegExp(
  `^scrypt\\$${SCRYPT_N}\\$${SCRYPT_R}\\$${SCRYPT_P}\\$` +
    `[0-9a-f]{${SALT_BYTES * 2}}\\$[0-9a-f]{${KEY_BYTES * 2}}$`,
);

export const MAX_BROWSER_PASSWORD_CHARS = MAX_PASSWORD_CHARS;
export const BROWSER_HASH_PREFIX = HASH_PREFIX;

/** True only for strings in the exactly-shaped scrypt hash format above. */
export function isPasswordHash(value: unknown): value is string {
  if (typeof value !== "string") return false;
  if (value.length !== HASH_PREFIX.length + SALT_BYTES * 2 + 1 + KEY_BYTES * 2) return false;
  return HASH_PATTERN.test(value);
}

/** Derives a storable password hash; identical input yields a new salt. */
export function hashPassword(password: string): string {
  if (typeof password !== "string" || password.length === 0 || password.length > MAX_PASSWORD_CHARS) {
    throw new Error(`password must be a string of 1..${MAX_PASSWORD_CHARS} characters`);
  }
  const salt = randomBytes(SALT_BYTES);
  const key = scryptSync(password.normalize("NFKC"), salt, KEY_BYTES, {
    N: SCRYPT_N,
    r: SCRYPT_R,
    p: SCRYPT_P,
  });
  return `${HASH_PREFIX}${salt.toString("hex")}$${key.toString("hex")}`;
}

/** Constant-shape comparison: incorrect hashes cost the same work. */
export function verifyPassword(password: string, hash: string): boolean {
  if (typeof password !== "string" || password.length === 0 || password.length > MAX_PASSWORD_CHARS) {
    return false;
  }
  if (!isPasswordHash(hash)) return false;
  const parts = hash.split("$");
  const salt = Buffer.from(parts[4], "hex");
  const expected = Buffer.from(parts[5], "hex");
  try {
    const key = scryptSync(password.normalize("NFKC"), salt, KEY_BYTES, {
      N: SCRYPT_N,
      r: SCRYPT_R,
      p: SCRYPT_P,
    });
    return key.length === expected.length && timingSafeEqual(key, expected);
  } catch {
    return false;
  }
}

/** Login verification runs off the event loop so telemetry keeps flowing. */
export async function verifyPasswordAsync(password: string, hash: string): Promise<boolean> {
  if (password.length === 0 || password.length > MAX_PASSWORD_CHARS || !isPasswordHash(hash)) return false;
  const parts = hash.split("$");
  const expected = Buffer.from(parts[5], "hex");
  return new Promise<boolean>((resolve) => {
    scrypt(password.normalize("NFKC"), Buffer.from(parts[4], "hex"), KEY_BYTES,
      { N: SCRYPT_N, r: SCRYPT_R, p: SCRYPT_P }, (error, key) => {
        resolve(!error && key.length === expected.length && timingSafeEqual(key, expected));
      });
  });
}
