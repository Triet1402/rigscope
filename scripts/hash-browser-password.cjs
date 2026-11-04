#!/usr/bin/env node
/**
 * Generates a salted scrypt password hash for HUB_BROWSER_USERS.
 *
 * Usage (password is read from stdin only — never arguments, never echoed,
 * never logged):
 *
 *   printf 'password' | node scripts/hash-browser-password.cjs
 *
 * Stdout receives exactly one hash line of the form
 *   scrypt$<N>$<r>$<p>$<salt-hex>$<key-hex>
 * suitable as the `passwordHash` field of an HUB_BROWSER_USERS entry:
 *
 *   HUB_BROWSER_USERS='[{"username":"viewer","passwordHash":"<hash>","machineIds":["machine-a"]}]' npm start
 *
 * Keep the parameters identical to src/hub/auth/password.ts (SCRYPT_*).
 */

"use strict";

const { randomBytes, scryptSync } = require("node:crypto");

const SCRYPT_N = 16384;
const SCRYPT_R = 8;
const SCRYPT_P = 1;
const SALT_BYTES = 16;
const KEY_BYTES = 64;
const MAX_PASSWORD_CHARS = 1024;

let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  input += chunk;
  if (input.length > MAX_PASSWORD_CHARS + 2) {
    console.error(`password longer than ${MAX_PASSWORD_CHARS} characters`);
    process.exit(1);
  }
});
process.stdin.on("end", () => {
  const password = input.replace(/(?:\r?\n)+$/, "");
  if (password.length === 0 || password.length > MAX_PASSWORD_CHARS) {
    console.error("usage: printf 'password' | node scripts/hash-browser-password.cjs");
    process.exit(1);
  }
  const salt = randomBytes(SALT_BYTES);
  const key = scryptSync(password.normalize("NFKC"), salt, KEY_BYTES, {
    N: SCRYPT_N,
    r: SCRYPT_R,
    p: SCRYPT_P,
  });
  process.stdout.write(`scrypt$${SCRYPT_N}$${SCRYPT_R}$${SCRYPT_P}$${salt.toString("hex")}$${key.toString("hex")}\n`);
});
process.stdin.on("error", (error) => {
  console.error("failed to read password from stdin:", error.message);
  process.exit(1);
});
