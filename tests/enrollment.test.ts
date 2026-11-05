/**
 * Unit tests for the persistent hub-local enrollment store
 * (src/hub/enrollment/store.ts). Run via the Node test runner through tsx.
 *
 * Covers enrollment success and identity generation, persistence across
 * restart, replay/expiry/duplicate/cap rejection, fail-closed handling of
 * corrupt state, atomic write-before-success behavior, restrictive file
 * permissions, and the hashed-only persistence of bootstrap tokens.
 */

import { strict as assert } from "node:assert";
import { createHash } from "node:crypto";
import { chmodSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, describe, it } from "node:test";
import { type HubIdentityConfig } from "../src/hub/config";
import { EnrollmentStore, EnrollmentStoreError, type EnrollmentGrant } from "../src/hub/enrollment/store";

const FUTURE = "2026-11-02T00:10:00.000Z";
const PAST = "2020-01-01T00:00:00.000Z";
const FIXED_NOW = new Date("2026-11-02T00:00:00.000Z");

let tempRoot: string;
let pathCounter = 0;

before(() => {
  tempRoot = mkdtempSync(join(tmpdir(), "enrollment-tests-"));
});

after(() => {
  rmSync(tempRoot, { recursive: true, force: true });
});

function freshPath(): string {
  pathCounter += 1;
  return join(tempRoot, `state-${pathCounter}.json`);
}

function grant(n: number, expiresAt: string = FUTURE): EnrollmentGrant {
  return { token: `bootstrap-grant-${n}-local-test-fixture-token`, expiresAt };
}

function grantExpiresAt(n: number, expiresAt: string): EnrollmentGrant {
  return { token: `at-${n}-token`, expiresAt };
}

function staticIdentity(n: number): HubIdentityConfig {
  return { collectorId: `static-collector-${n}`, machineId: `static-machine-${n}`, token: `static-token-${n}` };
}

function makeStore(options: {
  path?: string;
  identities?: HubIdentityConfig[];
  grants?: EnrollmentGrant[];
  now?: () => Date;
} = {}): EnrollmentStore {
  return new EnrollmentStore({
    path: options.path ?? freshPath(),
    identities: options.identities ?? [],
    grants: options.grants ?? [grant(1)],
    now: options.now ?? (() => new Date(FIXED_NOW.getTime())),
  });
}

function errCode(action: () => unknown): EnrollmentStoreError {
  try {
    action();
  } catch (error) {
    if (error instanceof EnrollmentStoreError) return error;
    throw error;
  }
  throw new Error("expected the action to throw an EnrollmentStoreError");
}

function sha256(value: string): string {
  return createHash("sha256").update(value, "utf8").digest("hex");
}

describe("enrollment success and identity generation", () => {
  it("issues an independent hub-local identity for a valid grant", () => {
    const store = makeStore();
    const issued = store.enroll({ collectorId: "collector-a", token: grant(1).token });
    assert.match(issued.machineId, /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    assert.match(issued.token, /^[A-Za-z0-9_-]{43}$/); // 32 bytes, base64url
    assert.notEqual(issued.token, grant(1).token);
    assert.equal(issued.collectorId, "collector-a");
    assert.equal(store.enrolledCount(), 1);
  });

  it("two enrollments produce independent token and machineId material", () => {
    const store = makeStore({ grants: [grant(1), grant(2)] });
    const first = store.enroll({ collectorId: "collector-a", token: grant(1).token });
    const second = store.enroll({ collectorId: "collector-b", token: grant(2).token });
    assert.notEqual(first.machineId, second.machineId);
    assert.notEqual(first.token, second.token);
    assert.equal(store.enrolledCount(), 2);
  });

  it("identities() exposes the static registry plus every enrolled identity", () => {
    const store = makeStore({ identities: [staticIdentity(1)] });
    assert.deepEqual(store.identities(), [staticIdentity(1)]);
    const issued = store.enroll({ collectorId: "collector-a", token: grant(1).token });
    const listed = store.identities();
    assert.equal(listed.length, 2);
    assert.deepEqual(listed[0], staticIdentity(1));
    assert.deepEqual(
      { collectorId: listed[1].collectorId, machineId: listed[1].machineId, token: listed[1].token },
      issued,
    );
    // Defensive copies: mutating the result must not affect the store.
    listed[1].token = "mutated";
    assert.equal(store.identities()[1].token, issued.token);
  });

  it("records createdAt/claimedAt from the injected clock", () => {
    const store = makeStore();
    store.enroll({ collectorId: "collector-a", token: grant(1).token });
    const state = JSON.parse(readFileSync(store.storePath, "utf8"));
    assert.equal(state.enrollments[0].createdAt, FIXED_NOW.toISOString());
    assert.equal(state.consumed[0].claimedAt, FIXED_NOW.toISOString());
  });
});

describe("grant binding: expiry, replay, and unknown tokens", () => {
  it("rejects an unknown enrollment token", () => {
    const store = makeStore();
    assert.equal(errCode(() => store.enroll({ collectorId: "collector-a", token: "no-such-token" })).code, "unknown-token");
    assert.equal(store.enrolledCount(), 0);
  });

  it("rejects an expired grant and allows a still-valid one at the same instant", () => {
    let clock = FIXED_NOW.getTime();
    const store = makeStore({
      grants: [grantExpiresAt(9, PAST), grantExpiresAt(10, new Date(clock).toISOString()), grant(11)],
      now: () => new Date(clock),
    });
    assert.equal(errCode(() => store.enroll({ collectorId: "collector-past", token: `at-9-token` })).code, "token-expired");
    // Expiry instant itself counts as expired (>=).
    assert.equal(errCode(() => store.enroll({ collectorId: "collector-boundary", token: `at-10-token` })).code, "token-expired");
    clock += 1;
    const issued = store.enroll({ collectorId: "collector-future", token: grant(11).token });
    assert.equal(issued.collectorId, "collector-future");
  });

  it("binds the grant to the first claimant: replay is rejected even for a different collectorId", () => {
    const store = makeStore();
    store.enroll({ collectorId: "collector-a", token: grant(1).token });
    assert.equal(
      errCode(() => store.enroll({ collectorId: "collector-b", token: grant(1).token })).code,
      "token-reused",
    );
    assert.equal(store.enrolledCount(), 1);
    assert.ok(store.identities().every((identity) => identity.collectorId === "collector-a"));
  });
});

describe("duplicate identities and caller-supplied identity material", () => {
  it("rejects enrollment of a collectorId already present in the static registry", () => {
    const store = makeStore({ identities: [staticIdentity(1)] });
    const error = errCode(() => store.enroll({ collectorId: "static-collector-1", token: grant(1).token }));
    assert.equal(error.code, "duplicate-collector");
    assert.equal(store.enrolledCount(), 0);
  });

  it("rejects a second grant claim for an already-enrolled collectorId", () => {
    const store = makeStore({ grants: [grant(1), grant(2)] });
    store.enroll({ collectorId: "collector-a", token: grant(1).token });
    assert.equal(
      errCode(() => store.enroll({ collectorId: "collector-a", token: grant(2).token })).code,
      "duplicate-collector",
    );
    assert.equal(store.enrolledCount(), 1);
  });

  it("never accepts a caller-supplied machineId or extra input fields", () => {
    const store = makeStore();
    assert.equal(
      // Cast: the route parser (parent-owned) is the first line, but the store
      // itself must also refuse caller identity material even from raw input.
      errCode(() => store.enroll({ collectorId: "collector-a", token: grant(1).token, machineId: "attacker-chosen" } as never)).code,
      "invalid-input",
    );
    assert.equal(errCode(() => store.enroll({ collectorId: "collector-a", token: "" })).code, "invalid-input");
    assert.equal(errCode(() => store.enroll({ collectorId: "", token: grant(1).token })).code, "invalid-input");
    assert.equal(store.enrolledCount(), 0);
  });

  it("rejects malformed constructor configuration", () => {
    const path = freshPath();
    assert.equal(
      errCode(() => new EnrollmentStore({ path, identities: [staticIdentity(1), staticIdentity(1)], grants: [] })).code,
      "invalid-config",
    );
    assert.equal(
      errCode(() => new EnrollmentStore({ path, identities: [staticIdentity(1)], grants: [grant(1), grant(1)] })).code,
      "invalid-config",
    );
    assert.equal(
      errCode(() =>
        new EnrollmentStore({
          path,
          identities: [{ collectorId: "c", machineId: "m", token: grant(1).token }],
          grants: [grant(1)],
        }),
      ).code,
      "invalid-config",
    );
    assert.equal(
      errCode(() => new EnrollmentStore({ path, identities: [], grants: [{ token: "t", expiresAt: "2026-11-02" }] })).code,
      "invalid-config",
    );
    assert.equal(
      errCode(() => new EnrollmentStore({ path, identities: [], grants: [{ token: "t", expiresAt: "not-a-date" }] })).code,
      "invalid-config",
    );
  });
});

describe("registry beyond fifty identities", () => {
  it("enrolls machine #51, persists it, and restores the larger registry", () => {
    const path = freshPath();
    const statics = Array.from({ length: 50 }, (_, i) => staticIdentity(i));
    const grants = [grant(1), grant(2), grant(3)];
    const store = makeStore({ path, identities: statics, grants });

    const fiftyFirst = store.enroll({ collectorId: "collector-51", token: grant(1).token });
    assert.equal(store.identities().length, 51);
    const fiftySecond = store.enroll({ collectorId: "collector-52", token: grant(2).token });
    assert.equal(store.identities().length, 52);

    // Restart against the same private state file: every enrollment survives,
    // including the entries beyond the old arbitrary machine cap.
    const restarted = makeStore({ path, identities: statics, grants: [grant(1), grant(3)] });
    assert.equal(restarted.identities().length, 52);
    const restored = restarted.identities();
    assert.deepEqual(
      restored.slice(50).map(({ collectorId, machineId, token }) => ({ collectorId, machineId, token })),
      [fiftyFirst, fiftySecond],
    );
    // Spent grants stay spent after the restart.
    assert.equal(
      errCode(() => restarted.enroll({ collectorId: "collector-replay", token: grant(1).token })).code,
      "token-reused",
    );
    // A fresh grant still enrolls machine #53.
    const fiftyThird = restarted.enroll({ collectorId: "collector-53", token: grant(3).token });
    assert.equal(restarted.identities().length, 53);
    assert.equal(restarted.identities().at(-1)?.collectorId, fiftyThird.collectorId);
  });

  it("restores a state file containing more than fifty enrollments", () => {
    const path = freshPath();
    const grants = Array.from({ length: 60 }, (_, i) => grant(100 + i));
    const first = makeStore({ path, identities: [], grants });
    for (let i = 0; i < 60; i += 1) {
      first.enroll({ collectorId: `bulk-collector-${i}`, token: grants[i]!.token });
    }
    assert.equal(first.enrolledCount(), 60);
    const restarted = makeStore({ path, identities: [], grants: [] });
    assert.equal(restarted.enrolledCount(), 60);
  });
});

describe("persistence and restart", () => {
  it("restores enrolled identities across a restart on the same state file", () => {
    const path = freshPath();
    const statics = [staticIdentity(1), staticIdentity(2)];
    const first = makeStore({ path, identities: statics, grants: [grant(1), grant(2)] });
    const issued = first.enroll({ collectorId: "collector-a", token: grant(1).token });
    assert.equal(first.enrolledCount(), 1);

    const second = makeStore({ path, identities: statics, grants: [grant(1), grant(2)] });
    assert.equal(second.enrolledCount(), 1);
    const listed = second.identities();
    assert.equal(listed.length, 3);
    assert.deepEqual(listed[0], statics[0]);
    assert.deepEqual(listed[1], statics[1]);
    assert.deepEqual(
      { collectorId: listed[2].collectorId, machineId: listed[2].machineId, token: listed[2].token },
      issued,
    );

    // A new grant can still be claimed after restart.
    const another = second.enroll({ collectorId: "collector-b", token: grant(2).token });
    assert.notEqual(another.machineId, issued.machineId);
  });

  it("rejects replay of a grant consumed before the restart (consumed set is durable)", () => {
    const path = freshPath();
    const grants = [grant(1)];
    const first = makeStore({ path, identities: [], grants });
    first.enroll({ collectorId: "collector-a", token: grant(1).token });

    // Same grant mistakenly reconfigured after the hub restarts.
    const second = makeStore({ path, identities: [], grants });
    assert.equal(
      errCode(() => second.enroll({ collectorId: "collector-b", token: grant(1).token })).code,
      "token-reused",
    );
    assert.equal(second.enrolledCount(), 1);
  });
});

describe("secure persistence constraints", () => {
  it("persists bootstrap tokens only as SHA-256 hashes", () => {
    const store = makeStore();
    const issued = store.enroll({ collectorId: "collector-a", token: grant(1).token });
    const raw = readFileSync(store.storePath, "utf8");
    const state = JSON.parse(raw);
    assert.equal(Object.prototype.hasOwnProperty.call(state, "version"), false);
    assert.equal(state.consumed.length, 1);
    assert.equal(state.consumed[0].tokenHash, sha256(grant(1).token));
    assert.match(state.consumed[0].tokenHash, /^[0-9a-f]{64}$/);
    assert.ok(!raw.includes(grant(1).token), "bootstrap token must not be persisted in plaintext");
    // The issued bearer is a credential the hub reloads on restart; it belongs
    // in the restricted state file but never in the consumed set.
    assert.ok(state.consumed.every((entry: { tokenHash: string }) => entry.tokenHash !== sha256(issued.token)));
  });

  it("keeps restrictive permissions: 0600 state file inside a 0700 directory", () => {
    const store = makeStore({ path: join(tempRoot, "perms", "state.json") });
    store.enroll({ collectorId: "collector-a", token: grant(1).token });
    assert.equal(statSync(store.storePath).mode & 0o777, 0o600);
    assert.equal(statSync(join(tempRoot, "perms")).mode & 0o777, 0o700);
  });

  it("leaves no temporary files behind after a successful write", () => {
    const store = makeStore();
    store.enroll({ collectorId: "collector-a", token: grant(1).token });
    const siblings = readdirSync(tempRoot).filter((name) => name.startsWith("state-") && name.includes(".tmp-"));
    assert.deepEqual(siblings, []);
    assert.equal(statSync(store.storePath).isFile(), true);
  });

  it("never leaks secrets in error messages", () => {
    const store = makeStore();
    const secretText = grant(1).token;
    const error = errCode(() => store.enroll({ collectorId: "collector-a", token: "wrong-guess-token" }));
    assert.ok(!error.message.includes(secretText));
    assert.ok(!error.stack?.includes(secretText));
  });
});

describe("atomic write-before-success and failure handling", () => {
  it("fails closed when persistence fails and does not burn the grant", (t) => {
    if (process.getuid?.() === 0) { t.skip("root can write through POSIX directory mode restrictions"); return; }
    const directory = join(tempRoot, "readonly");
    const store = makeStore({ path: join(directory, "state.json") });
    // Lock the private directory before attempting enrollment.
    assert.equal(statSync(directory).mode & 0o777, 0o700);
    chmodSync(directory, 0o500);
    try {
      assert.equal(
        errCode(() => store.enroll({ collectorId: "collector-a", token: grant(1).token })).code,
        "persist-failed",
      );
      assert.equal(store.enrolledCount(), 0);
      assert.equal(store.identities().length, 0);
    } finally {
      chmodSync(directory, 0o700);
    }
    // Storage recovered: the same grant is still claimable and now persists.
    const issued = store.enroll({ collectorId: "collector-a", token: grant(1).token });
    assert.equal(issued.collectorId, "collector-a");
    const restarted = makeStore({ path: store.storePath, identities: [], grants: [grant(1)] });
    assert.equal(restarted.enrolledCount(), 1);
  });
});

describe("fail closed on corrupt persisted state", () => {
  function corruptStore(raw: string, path = freshPath()): EnrollmentStoreError {
    writeFileSync(path, raw, { mode: 0o600 });
    return errCode(() => new EnrollmentStore({ path, identities: [], grants: [grant(1)] }));
  }

  it("rejects unreadable or non-JSON state", () => {
    assert.equal(corruptStore("{not json").code, "invalid-state");
    assert.equal(corruptStore("[]").code, "invalid-state");
    assert.equal(corruptStore("null").code, "invalid-state");
  });

  it("rejects missing arrays", () => {
    assert.equal(corruptStore(JSON.stringify({})).code, "invalid-state");
    assert.equal(corruptStore(JSON.stringify({ enrollments: [] })).code, "invalid-state");
    assert.equal(corruptStore(JSON.stringify({ consumed: [] })).code, "invalid-state");
  });

  it("rejects unfinished versioned state without rewriting it", () => {
    for (const field of ['version', 'formatVersion']) {
      const path = freshPath();
      const raw = JSON.stringify({[field]: 1, enrollments: [], consumed: []});
      assert.equal(corruptStore(raw, path).code, 'invalid-state');
      assert.equal(readFileSync(path, 'utf8'), raw);
    }
  });

  it("rejects malformed entries, duplicates, and oversized state", () => {
    const good = { collectorId: "c", machineId: "m", token: "t".repeat(43), createdAt: FIXED_NOW.toISOString() };
    assert.equal(
      corruptStore(JSON.stringify({ enrollments: [{ collectorId: "c" }], consumed: [] })).code,
      "invalid-state",
    );
    assert.equal(
      corruptStore(JSON.stringify({ enrollments: [good, good], consumed: [] })).code,
      "invalid-state",
    );
    assert.equal(
      corruptStore(JSON.stringify({ enrollments: [], consumed: [{ tokenHash: "deadbeef", collectorId: "c", claimedAt: FIXED_NOW.toISOString() }] })).code,
      "invalid-state",
    );
    // Over-budget state fails closed rather than being trimmed. The bound is
    // serialized bytes, so this stays valid JSON while exceeding the file cap.
    const enrollments = Array.from({ length: 400 }, (_, i) => ({
      collectorId: `c-${i}-${"x".repeat(110)}`,
      machineId: `m-${i}-${"y".repeat(110)}`,
      token: `t-${i}-${"z".repeat(40)}`,
      createdAt: FIXED_NOW.toISOString(),
    }));
    const oversized = JSON.stringify({ enrollments, consumed: [] });
    assert.ok(Buffer.byteLength(oversized, "utf8") > 128 * 1024);
    assert.equal(corruptStore(oversized).code, "invalid-state");
  });

  it("rejects a state file that collides with the static registry", () => {
    const path = freshPath();
    writeFileSync(
      path,
      JSON.stringify({
        enrollments: [{ collectorId: "static-collector-1", machineId: "m", token: "t".repeat(43), createdAt: FIXED_NOW.toISOString() }],
        consumed: [],
      }),
      { mode: 0o600 },
    );
    assert.equal(
      errCode(() => new EnrollmentStore({ path, identities: [staticIdentity(1)], grants: [grant(1)] })).code,
      "invalid-state",
    );
  });

  it("rejects a directory in place of the state file", () => {
    const path = join(tempRoot, "state-is-a-directory.json");
    mkdirSync(path);
    assert.equal(errCode(() => new EnrollmentStore({ path, identities: [], grants: [] })).code, "invalid-state");
  });
});
