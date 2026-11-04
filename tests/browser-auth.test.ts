/**
 * Unit tests for browser authentication internals: password hashing,
 * HUB_BROWSER_USERS parsing, and session store behavior (fixed expiration,
 * bounded store, logout/expiry notifications). Run via the Node test runner
 * through tsx.
 */

import { strict as assert } from "node:assert";
import { describe, it } from "node:test";
import { execFileSync, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { HubConfigError } from "../src/hub/config";
import { hashPassword, isPasswordHash, verifyPassword } from "../src/hub/auth/password";
import { parseBrowserUsers } from "../src/hub/auth/config";
import {
  BrowserSessionStore,
  type BrowserSession,
  type SessionEndReason,
} from "../src/hub/auth/sessions";

describe("password hashing", () => {
  it("operator hash script produces usable hashes and rejects overlong passwords", () => {
    const script = fileURLToPath(new URL("../scripts/hash-browser-password.cjs", import.meta.url));
    const password = "demo-cli-password-密码";
    const hash = execFileSync(process.execPath, [script], { input: `${password}\n`, encoding: "utf8" }).trim();
    assert.equal(verifyPassword(password, hash), true);
    const overlong = spawnSync(process.execPath, [script], { input: "x".repeat(1025), encoding: "utf8" });
    assert.equal(overlong.status, 1);
    assert.equal(overlong.stdout, "");
  });
  it("round-trips a password and rejects every wrong password", () => {
    const hash = hashPassword("demo-password-browser-local-only");
    assert.match(hash, /^scrypt\$16384\$8\$1\$[0-9a-f]{32}\$[0-9a-f]{128}$/);
    assert.equal(verifyPassword("demo-password-browser-local-only", hash), true);
    assert.equal(verifyPassword("demo-password-browser-local-onl", hash), false);
    assert.equal(verifyPassword("demo-password-browser-local-only了两", hash), false);
    assert.equal(verifyPassword("", hash), false);
  });

  it("produces fresh salts so identical passwords do not share hashes", () => {
    const first = hashPassword("same-password");
    const second = hashPassword("same-password");
    assert.notEqual(first, second);
    assert.equal(verifyPassword("same-password", first), true);
    assert.equal(verifyPassword("same-password", second), true);
  });

  it("refuses plaintext-format or malformed hashes without crashing", () => {
    assert.equal(verifyPassword("demo-password-browser-local-only", "demo-password-browser-local-only"), false);
    assert.equal(verifyPassword("x", "scrypt$16384$8$1$zz$zz"), false);
    assert.equal(verifyPassword("x", "scrypt$16384$8$1$ab$cd"), false);
    assert.equal(verifyPassword("x", ""), false);
  });

  it("rejects invalid hash creation input", () => {
    assert.throws(() => hashPassword(""));
    assert.throws(() => hashPassword("x".repeat(1025)));
    assert.throws(() => hashPassword(undefined as unknown as string));
  });

  it("isPasswordHash accepts only exactly-shaped hashes", () => {
    assert.equal(isPasswordHash(hashPassword("roundtrip")), true);
    assert.equal(isPasswordHash("scrypt$16384$8$1$00$00"), false);
    assert.equal(isPasswordHash(42), false);
    assert.equal(isPasswordHash(null), false);
  });
});

describe("parseBrowserUsers", () => {
  it("treats a missing or empty variable as no browser users", () => {
    assert.deepEqual(parseBrowserUsers({}), []);
    assert.deepEqual(parseBrowserUsers({ HUB_BROWSER_USERS: undefined }), []);
    assert.deepEqual(parseBrowserUsers({ HUB_BROWSER_USERS: "" }), []);
  });

  it("accepts wildcard and explicit scopes, including an empty array", () => {
    const hash = hashPassword("fixture-password");
    const users = parseBrowserUsers({
      HUB_BROWSER_USERS: JSON.stringify([
        { username: "all", passwordHash: hash, machineIds: "*" },
        { username: "one", passwordHash: hash, machineIds: ["m-1"] },
        { username: "none", passwordHash: hash, machineIds: [] },
      ]),
    });
    assert.deepEqual(users.map((user) => user.username), ["all", "one", "none"]);
    assert.equal(users[0].machineIds, "*");
    assert.deepEqual(users[1].machineIds, ["m-1"]);
    assert.deepEqual(users[2].machineIds, []);
  });

  it("fails startup for malformed JSON, wrong types, and plaintext passwords", () => {
    assert.throws(() => parseBrowserUsers({ HUB_BROWSER_USERS: "not-json" }), HubConfigError);
    assert.throws(
      () => parseBrowserUsers({ HUB_BROWSER_USERS: '{"username":"demo"}' }),
      HubConfigError,
    );
    assert.throws(
      () => parseBrowserUsers({
        HUB_BROWSER_USERS: JSON.stringify([{ username: "demo", passwordHash: "demo-password-local", machineIds: "*" }]),
      }),
      /passwordHash/,
    );
  });

  it("enforces exact fields, bounded identifiers, and duplicate rejection", () => {
    const hash = hashPassword("fixture-password");
    const cases: unknown[] = [
      [{ username: "demo", passwordHash: hash, machineIds: "*", extra: true }],
      [{ username: "", passwordHash: hash, machineIds: "*" }],
      [{ username: "a b", passwordHash: hash, machineIds: "*" }],
      [{ username: "demo", passwordHash: hash, machineIds: ["dup", "dup"] }],
      [{ username: "demo", passwordHash: hash, machineIds: "" }],
      [{ username: "demo", passwordHash: hash, machineIds: 42 }],
    ];
    for (const value of cases) {
      assert.throws(
        () => parseBrowserUsers({ HUB_BROWSER_USERS: JSON.stringify(value) }),
        HubConfigError,
      );
    }
    assert.throws(
      () => parseBrowserUsers({
        HUB_BROWSER_USERS: JSON.stringify([
          { username: "demo", passwordHash: hash, machineIds: "*" },
          { username: "demo", passwordHash: hash, machineIds: "*" },
        ]),
      }),
      /duplicate username/,
    );
    assert.throws(
      () => parseBrowserUsers({
        HUB_BROWSER_USERS: JSON.stringify(
          Array.from({ length: 51 }, (_, i) => ({ username: `u${i}`, passwordHash: hash, machineIds: "*" })),
        ),
      }),
      /at most 50/,
    );
  });
});

describe("browser session store", () => {
  function makeStore(overrides: {
    ttlMs?: number;
    maxSessions?: number;
    now?: () => number;
    onEnd?: (session: BrowserSession, reason: SessionEndReason) => void;
  } = {}) {
    const ended: { username: string; reason: SessionEndReason }[] = [];
    const store = new BrowserSessionStore({
      sweepMs: 0, // no timer in unit tests; sweepExpired is called directly
      onEnd: (session, reason) => {
        ended.push({ username: session.username, reason });
        overrides.onEnd?.(session, reason);
      },
      ...overrides,
    });
    return { store, ended };
  }

  it("creates opaque random tokens and csrf values with fixed deadlines", () => {
    let clock = 1_000_000;
    const { store } = makeStore({ ttlMs: 60_000, now: () => clock });
    const session = store.create("viewer", "*");
    assert.match(session.id, /^[A-Za-z0-9_-]{43}$/);
    assert.match(session.csrfToken, /^[A-Za-z0-9_-]{43}$/);
    assert.notEqual(session.id, session.csrfToken);
    assert.equal(session.expiresAt, clock + 60_000);
    assert.equal(store.find(session.id)?.username, "viewer");
    assert.equal(store.ttlMs(), 60_000);
  });

  it("copies explicit scopes so later mutation cannot change a session", () => {
    const scope = ["m-1"];
    const { store } = makeStore({ now: () => 0 });
    const session = store.create("viewer", scope);
    (scope as string[]).push("attacker-machine");
    assert.deepEqual(session.scope, ["m-1"]);
  });

  it("expires sessions at the fixed deadline and notifies as expired", () => {
    let clock = 0;
    const { store, ended } = makeStore({ ttlMs: 10_000, now: () => clock });
    const session = store.create("viewer", "*");
    clock = 9_999;
    assert.notEqual(store.find(session.id), null);
    clock = 10_000;
    assert.equal(store.find(session.id), null);
    assert.deepEqual(ended, [{ username: "viewer", reason: "expired" }]);
    assert.equal(store.size(), 0);
  });

  it("sweeps multiple expired sessions and again after logout", () => {
    let clock = 0;
    const { store, ended } = makeStore({ ttlMs: 10_000, now: () => clock });
    const a = store.create("a", "*");
    const b = store.create("b", ["m-1"]);
    clock = 11_000;
    assert.equal(store.sweepExpired(), 2);
    assert.deepEqual(ended.map((entry) => entry.reason), ["expired", "expired"]);
    assert.equal(store.end(a.id, "logout"), false, "already removed");
    assert.equal(store.end(b.id, "logout"), false, "already removed");
  });

  it("logout ends only the exact session and never another user's", () => {
    const { store, ended } = makeStore({ now: () => 0 });
    const mine = store.create("me", "*");
    const other = store.create("other", "*");
    assert.equal(store.end(other.id), true);
    assert.deepEqual(ended, [{ username: "other", reason: "logout" }]);
    assert.notEqual(store.find(mine.id), null);
    assert.equal(store.end(`${mine.id}x`), false);
  });

  it("bounds the store by evicting the oldest session as an expiry", () => {
    let clock = 0;
    const { store, ended } = makeStore({ ttlMs: 100_000, maxSessions: 2, now: () => clock });
    const first = store.create("first", "*");
    clock += 10;
    store.create("second", "*");
    clock += 10;
    const third = store.create("third", "*");
    assert.equal(store.size(), 2);
    assert.equal(store.find(first.id), null, "oldest session evicted");
    assert.equal(third.username, "third");
    assert.deepEqual(ended, [{ username: "first", reason: "expired" }]);
    clock += 100_000;
    const remaining = store.sweepExpired();
    assert.equal(remaining, 2, "surviving sessions still expire normally");
  });

  it("listener failures never break session removal", () => {
    const store = new BrowserSessionStore({
      sweepMs: 0,
      onEnd: () => {
        throw new Error("listener bug");
      },
    });
    const session = store.create("viewer", "*");
    assert.equal(store.end(session.id), true);
    assert.equal(store.size(), 0);
    store.dispose();
  });
});
