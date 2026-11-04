import { strict as assert } from "node:assert";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it, test } from "node:test";
import { parseBrowserUsers } from "../src/hub/auth/config";
import { hashPassword, verifyPassword } from "../src/hub/auth/password";
import { AdminError, AdminStore, ADMIN_MIGRATIONS } from "../src/hub/admin/store";
import { openHubDatabase, type HubDatabase } from "../src/hub/storage/db";

const NOW = Date.parse("2026-11-02T12:00:00Z");
const OWNER_HASH = hashPassword("owner-password");
const VIEWER_HASH = hashPassword("viewer-password");

type Ctx = { dir: string; db: HubDatabase; store: AdminStore; path: string };

function setup(
  options: {
    users?: ConstructorParameters<typeof AdminStore>[0]["bootstrapUsers"];
    adminUsernames?: string[];
    onUsersChanged?: (users: ReturnType<AdminStore["users"]>) => void;
    existing?: Ctx;
  } = {},
): Ctx {
  const dir = options.existing?.dir ?? mkdtempSync(join(tmpdir(), "rigscope-admin-"));
  const path = options.existing?.path ?? join(dir, "hub.db");
  const db = options.existing?.db ?? openHubDatabase(path);
  const store = new AdminStore({
    database: db,
    bootstrapUsers:
      options.users ??
      [
        { username: "owner", passwordHash: OWNER_HASH, machineIds: "*", admin: true },
        { username: "viewer", passwordHash: VIEWER_HASH, machineIds: "*" },
      ],
    bootstrapAdminUsernames: options.adminUsernames,
    onUsersChanged: options.onUsersChanged,
    now: () => NOW,
  });
  return { dir, db, store, path };
}

function close(ctx: Ctx): void {
  ctx.db.close();
  rmSync(ctx.dir, { recursive: true, force: true });
}

describe("browser user config admin flag", () => {
  it("keeps the legacy shape and accepts an explicit boolean admin", () => {
    const legacy = parseBrowserUsers({
      HUB_BROWSER_USERS: JSON.stringify([
        { username: "legacy", passwordHash: OWNER_HASH, machineIds: "*" },
      ]),
    })[0]!;
    assert.equal("admin" in legacy, false);

    const admin = parseBrowserUsers({
      HUB_BROWSER_USERS: JSON.stringify([
        { username: "admin", passwordHash: OWNER_HASH, machineIds: ["m-1"], admin: true },
      ]),
    })[0]!;
    assert.equal(admin.admin, true);

    assert.throws(
      () =>
        parseBrowserUsers({
          HUB_BROWSER_USERS: JSON.stringify([
            { username: "bad", passwordHash: OWNER_HASH, machineIds: "*", admin: "yes" },
          ]),
        }),
      /admin must be a boolean/,
    );
  });

  it("accepts an explicit configuration scope with more than fifty machines", () => {
    const machineIds = Array.from({ length: 60 }, (_, i) => `machine-${i}`);
    const [user] = parseBrowserUsers({
      HUB_BROWSER_USERS: JSON.stringify([
        { username: "scoped", passwordHash: OWNER_HASH, machineIds },
      ]),
    });
    assert.deepEqual(user?.machineIds, machineIds);
  });
});

describe("AdminStore bootstrap", () => {
  it("imports configured users once and preserves exact hashes/scopes", () => {
    const ctx = setup();
    try {
      assert.equal(ADMIN_MIGRATIONS[0]!.version, 3);
      const users = ctx.store.users();
      assert.equal(users.length, 2);
      assert.equal(users[0]!.username, "owner");
      assert.equal(users[0]!.passwordHash, OWNER_HASH);
      assert.equal(users[0]!.machineIds, "*");
      assert.equal(users[0]!.admin, true);
      assert.equal(users[1]!.admin, undefined);
      assert.equal(ctx.store.isAdmin("owner"), true);
      assert.equal(ctx.store.isAdmin("viewer"), false);
    } finally {
      close(ctx);
    }
  });

  it("elevates a legacy owner via bootstrapAdminUsernames only on first import", async () => {
    const ctx = setup({
      users: [
        { username: "owner", passwordHash: OWNER_HASH, machineIds: "*" },
        { username: "viewer", passwordHash: VIEWER_HASH, machineIds: "*" },
      ],
      adminUsernames: ["owner"],
    });
    try {
      assert.equal(ctx.store.isAdmin("owner"), true);
      assert.equal(ctx.store.isAdmin("viewer"), false);

      await ctx.store.upsertUser("owner", {
        username: "viewer",
        machineIds: "*",
        admin: true,
      });
      assert.equal(ctx.store.isAdmin("viewer"), true);

      // A second construction ignores bootstrap elevation (and never
      // overwrites managed state).
      const second = setup({ existing: ctx, adminUsernames: [] });
      assert.equal(second.store.isAdmin("viewer"), true);
      assert.equal(second.store.isAdmin("owner"), true);
    } finally {
      close(ctx);
    }
  });

  it("does not resurrect deleted users or overwrite managed passwords/scopes", async () => {
    const ctx = setup();
    try {
      await ctx.store.upsertUser("owner", {
        username: "owner",
        machineIds: ["m-1"],
        admin: true,
        password: "managed-password",
      });
      const managedHash = ctx.store.users().find((user) => user.username === "owner")!.passwordHash;
      assert.notEqual(managedHash, OWNER_HASH);
      assert.equal(verifyPassword("managed-password", managedHash), true);

      ctx.store.deleteUser("owner", "viewer");
      assert.equal(ctx.store.listUsers().some((user) => user.username === "viewer"), false);

      const second = setup({ existing: ctx });
      try {
        assert.equal(second.store.listUsers().some((user) => user.username === "viewer"), false);
        const owner = second.store.users().find((user) => user.username === "owner")!;
        assert.equal(owner.passwordHash, managedHash);
        assert.deepEqual(owner.machineIds, ["m-1"]);
        assert.equal(owner.admin, true);
      } finally {
        // The outer teardown closes the shared DB.
      }
    } finally {
      close(ctx);
    }
  });

  it("stays closed with an empty configuration", () => {
    const ctx = setup({ users: [] });
    try {
      assert.deepEqual(ctx.store.users(), []);
      assert.deepEqual(ctx.store.listUsers(), []);
      assert.equal(ctx.store.isAdmin("anyone"), false);
    } finally {
      close(ctx);
    }
  });
});

describe("AdminStore mutations", () => {
  it("denies a normal viewer even with a wildcard machine scope", async () => {
    const ctx = setup();
    try {
      await assert.rejects(
        ctx.store.upsertUser("viewer", {
          username: "new-user",
          machineIds: "*",
          admin: false,
          password: "new-password",
        }),
        (error: unknown) => error instanceof AdminError && error.code === "forbidden",
      );
      assert.throws(
        () => ctx.store.deleteUser("viewer", "owner"),
        (error: unknown) => error instanceof AdminError && error.code === "forbidden",
      );
    } finally {
      close(ctx);
    }
  });

  it("creates, updates scope/admin/disabled and rotates passwords", async () => {
    const changes: string[][] = [];
    const ctx = setup({
      onUsersChanged: (users) => changes.push(users.map((user) => user.username)),
    });
    try {
      await ctx.store.upsertUser("owner", {
        username: "bob",
        machineIds: ["m-1", "m-2"],
        admin: false,
        password: "bob-password",
      });
      let bob = ctx.store.users().find((user) => user.username === "bob")!;
      const firstHash = bob.passwordHash;
      assert.equal(verifyPassword("bob-password", firstHash), true);
      assert.deepEqual(bob.machineIds, ["m-1", "m-2"]);
      assert.equal(bob.admin, undefined);
      assert.deepEqual(changes.at(-1), ["bob", "owner", "viewer"].sort());

      await ctx.store.upsertUser("owner", {
        username: "bob",
        machineIds: ["m-3"],
        admin: true,
      });
      bob = ctx.store.users().find((user) => user.username === "bob")!;
      assert.equal(bob.passwordHash, firstHash, "omitted password preserves the hash");
      assert.deepEqual(bob.machineIds, ["m-3"]);
      assert.equal(bob.admin, true);

      await ctx.store.upsertUser("owner", {
        username: "bob",
        machineIds: ["m-3"],
        admin: true,
        password: "another-password",
      });
      bob = ctx.store.users().find((user) => user.username === "bob")!;
      assert.notEqual(bob.passwordHash, firstHash);
      assert.equal(verifyPassword("another-password", bob.passwordHash), true);
      assert.equal(verifyPassword("bob-password", bob.passwordHash), false);

      await ctx.store.upsertUser("owner", {
        username: "bob",
        machineIds: ["m-3"],
        admin: true,
        disabled: true,
      });
      assert.equal(ctx.store.users().some((user) => user.username === "bob"), false);
      assert.equal(ctx.store.isAdmin("bob"), false);
      const listed = ctx.store.listUsers().find((user) => user.username === "bob")!;
      assert.deepEqual(listed, { username: "bob", machineIds: ["m-3"], admin: true, disabled: true });
      assert.ok(changes.length >= 4, "every committed mutation notifies after commit");
    } finally {
      close(ctx);
    }
  });

  it("requires a password for new users and rejects invalid fields/scopes", async () => {
    const ctx = setup();
    try {
      const invalid: Array<Record<string, unknown>> = [
        { username: "bad name", machineIds: "*", admin: false, password: "p" },
        { username: "ok", machineIds: ["dup", "dup"], admin: false, password: "p" },
        { username: "ok", machineIds: "*", admin: "yes", password: "p" },
        { username: "ok", machineIds: "*", admin: false, password: "" },
        { username: "ok", machineIds: "*", admin: false, password: "p", extra: true },
      ];
      for (const input of invalid) {
        await assert.rejects(
          ctx.store.upsertUser("owner", input as never),
          (error: unknown) => error instanceof AdminError && error.code === "invalid",
        );
      }
      await assert.rejects(
        ctx.store.upsertUser("owner", { username: "new", machineIds: "*", admin: false }),
        (error: unknown) => error instanceof AdminError && error.code === "invalid",
      );
    } finally {
      close(ctx);
    }
  });

  it("accepts, persists and reloads an explicit scope beyond fifty machines", async () => {
    const machineIds = Array.from({ length: 60 }, (_, i) => `m-${i}`);
    const ctx = setup();
    try {
      await ctx.store.upsertUser("owner", {
        username: "scoped",
        machineIds,
        admin: false,
        password: "scoped-password",
      });
      const stored = ctx.store.users().find((user) => user.username === "scoped")!;
      assert.deepEqual(stored.machineIds, machineIds);
      assert.deepEqual(
        ctx.store.listUsers().find((user) => user.username === "scoped")?.machineIds,
        machineIds,
      );

      const restarted = setup({ existing: ctx });
      const restored = restarted.store.users().find((user) => user.username === "scoped")!;
      assert.deepEqual(restored.machineIds, machineIds);
      assert.equal(restored.admin, undefined);
    } finally {
      close(ctx);
    }
  });

  it("protects the last active administrator from demotion, disable and deletion", async () => {
    const sole = setup({ users: [{ username: "owner", passwordHash: OWNER_HASH, machineIds: "*", admin: true }] });
    try {
      await assert.rejects(
        sole.store.upsertUser("owner", { username: "owner", machineIds: "*", admin: false }),
        (error: unknown) => error instanceof AdminError && error.code === "conflict",
      );
      await assert.rejects(
        sole.store.upsertUser("owner", {
          username: "owner",
          machineIds: "*",
          admin: true,
          disabled: true,
        }),
        (error: unknown) => error instanceof AdminError && error.code === "conflict",
      );
      assert.throws(
        () => sole.store.deleteUser("owner", "owner"),
        (error: unknown) => error instanceof AdminError && error.code === "conflict",
      );
    } finally {
      close(sole);
    }

    const pair = setup();
    try {
      await pair.store.upsertUser("owner", {
        username: "admin2",
        machineIds: "*",
        admin: true,
        password: "admin2-password",
      });
      await pair.store.upsertUser("owner", {
        username: "owner",
        machineIds: "*",
        admin: false,
      });
      assert.equal(pair.store.isAdmin("owner"), false);
      assert.equal(pair.store.isAdmin("admin2"), true);
      assert.throws(
        () => pair.store.deleteUser("admin2", "admin2"),
        (error: unknown) => error instanceof AdminError && error.code === "conflict",
      );
    } finally {
      close(pair);
    }
  });

  it("bounds the directory at 50 users", async () => {
    const users = Array.from({ length: 50 }, (_, index) => ({
      username: `user-${index}`,
      passwordHash: OWNER_HASH,
      machineIds: "*" as const,
      admin: index === 0,
    }));
    const ctx = setup({ users });
    try {
      assert.equal(ctx.store.listUsers().length, 50);
      await assert.rejects(
        ctx.store.upsertUser("user-0", {
          username: "one-too-many",
          machineIds: "*",
          admin: false,
          password: "password",
        }),
        (error: unknown) => error instanceof AdminError && error.code === "conflict",
      );
    } finally {
      close(ctx);
    }
  });

  it("limits concurrent password hashing to two", async () => {
    const ctx = setup();
    try {
      const first = ctx.store.upsertUser("owner", {
        username: "hash-a",
        machineIds: "*",
        admin: false,
        password: "password-a",
      });
      const second = ctx.store.upsertUser("owner", {
        username: "hash-b",
        machineIds: "*",
        admin: false,
        password: "password-b",
      });
      await assert.rejects(
        ctx.store.upsertUser("owner", {
          username: "hash-c",
          machineIds: "*",
          admin: false,
          password: "password-c",
        }),
        (error: unknown) => error instanceof AdminError && error.code === "unavailable",
      );
      await Promise.all([first, second]);
      assert.equal(ctx.store.listUsers().filter((user) => user.username.startsWith("hash-")).length, 2);
    } finally {
      close(ctx);
    }
  });

  it("audits only actor/action/target/time and keeps the newest 1000 events", async () => {
    const ctx = setup();
    try {
      await ctx.store.upsertUser("owner", {
        username: "bob",
        machineIds: "*",
        admin: false,
        password: "bob-secret",
      });
      const events = ctx.store.audit(10);
      assert.equal(events.length, 1);
      assert.equal(events[0]!.actor, "owner");
      assert.equal(events[0]!.action, "user.create");
      assert.equal(events[0]!.target, "bob");
      assert.match(events[0]!.at, /^2026-11-02T12:00:00\.000Z$/);
      const serialized = JSON.stringify(events);
      assert.equal(serialized.includes("bob-secret"), false);
      assert.equal(serialized.includes(ctx.store.users().find((user) => user.username === "bob")!.passwordHash), false);

      // Directly seed an oversized history, then one real mutation prunes it.
      const insert = ctx.db.db.prepare(
        "INSERT INTO admin_audit (actor, action, target, at_ms) VALUES (?, ?, ?, ?)",
      );
      ctx.db.db.exec("BEGIN");
      for (let index = 0; index < 1200; index += 1) {
        insert.run("owner", "user.update", `target-${index}`, NOW);
      }
      ctx.db.db.exec("COMMIT");
      await ctx.store.upsertUser("owner", { username: "bob", machineIds: "*", admin: false });
      const bounded = ctx.store.audit(5000);
      assert.equal(bounded.length, 1000);
      assert.equal(bounded[0]!.action, "user.update");
      assert.throws(() => ctx.store.audit(0), AdminError);
    } finally {
      close(ctx);
    }
  });
});

test('an empty first start can import the initial configured owner on the next start', () => {
  const db = openHubDatabase(':memory:');
  try {
    const empty = new AdminStore({ database: db, bootstrapUsers: [] });
    assert.deepEqual(empty.users(), []);
    const configured = new AdminStore({ database: db, bootstrapUsers: [
      { username: 'owner', passwordHash: OWNER_HASH, machineIds: '*', admin: true },
    ] });
    assert.equal(configured.isAdmin('owner'), true);
    assert.equal(configured.users()[0].passwordHash, OWNER_HASH);
  } finally { db.close(); }
});
