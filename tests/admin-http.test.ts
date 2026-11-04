import { strict as assert } from "node:assert";
import { createServer, type Server } from "node:http";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, it } from "node:test";
import type { BrowserUserConfig } from "../src/hub/auth/config";
import { hashPassword } from "../src/hub/auth/password";
import {
  AUTH_LOGIN_PATH,
  AUTH_PATH_PREFIX,
  BrowserHttpAuth,
  loopbackOriginHostnames,
  sendJson,
} from "../src/hub/auth/http";
import { BrowserSessionStore } from "../src/hub/auth/sessions";
import {
  ADMIN_AUDIT_PATH,
  ADMIN_USERS_PATH,
  AdminHttp,
} from "../src/hub/admin/http";
import { AdminStore } from "../src/hub/admin/store";
import { openHubDatabase, type HubDatabase } from "../src/hub/storage/db";

const NOW = Date.parse("2026-11-02T12:00:00Z");
const OWNER_HASH = hashPassword("owner-password");
const VIEWER_HASH = hashPassword("viewer-password");

const BOOTSTRAP: BrowserUserConfig[] = [
  { username: "owner", passwordHash: OWNER_HASH, machineIds: "*", admin: true },
  { username: "viewer", passwordHash: VIEWER_HASH, machineIds: "*" },
];

type Harness = {
  dir: string;
  db: HubDatabase;
  store: AdminStore;
  auth: BrowserHttpAuth;
  sessions: BrowserSessionStore;
  server: Server;
  origin: string;
  users: BrowserUserConfig[];
  changes: string[][];
  login: (username: string, password: string) => Promise<LoginResult>;
  waitLoginStart: () => Promise<void>;
  request: (path: string, init?: RequestInit) => Promise<Response>;
  adminPost: (
    cookie: string,
    csrfToken: string,
    body: unknown,
    headers?: Record<string, string>,
  ) => Promise<{ status: number; json: unknown }>;
  adminDelete: (
    cookie: string,
    csrfToken: string,
    username: string,
    headers?: Record<string, string>,
  ) => Promise<{ status: number; json: unknown }>;
  adminGet: (cookie: string, path: string) => Promise<{ status: number; json: unknown }>;
  close: () => Promise<void>;
};

type LoginResult = {
  status: number;
  payload: Record<string, unknown> | null;
  cookie: string | null;
};

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

async function startHarness(options: { users?: BrowserUserConfig[] } = {}): Promise<Harness> {
  const dir = mkdtempSync(join(tmpdir(), "rigscope-admin-http-"));
  const db = openHubDatabase(join(dir, "hub.db"));
  const users = options.users ?? BOOTSTRAP;
  const changes: string[][] = [];
  const store = new AdminStore({
    database: db,
    bootstrapUsers: users,
    now: () => NOW,
  });
  const sessions = new BrowserSessionStore({ ttlMs: 60_000, sweepMs: 0, now: () => NOW });
  const loginWaiters: Array<() => void> = [];
  let auth!: BrowserHttpAuth;
  let adminHttp!: AdminHttp;
  let rules!: { originPort: number; originHostnames: string[] };
  const server = createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://hub.invalid");
    if (url.pathname === AUTH_LOGIN_PATH) loginWaiters.shift()?.();
    void (async () => {
      if (url.pathname.startsWith(AUTH_PATH_PREFIX)) {
        await auth.handleApiAuth(req, res, url.pathname);
        return;
      }
      if (await adminHttp.handle(req, res, url)) return;
      sendJson(res, 404, { error: "not found" });
    })().catch(() => {
      if (res.headersSent) res.end();
      else sendJson(res, 500, { error: "internal error" });
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address() as { port: number };
  const origin = `http://127.0.0.1:${address.port}`;
  rules = { originPort: address.port, originHostnames: loopbackOriginHostnames("127.0.0.1") };
  auth = new BrowserHttpAuth({ users: store.users(), sessions, origin: rules });
  adminHttp = new AdminHttp({
    store,
    auth,
    origin: rules,
    onChanged: (touched) => changes.push(touched),
  });

  const request = (path: string, init: RequestInit = {}): Promise<Response> =>
    fetch(`${origin}${path}`, init);

  const login = async (username: string, password: string): Promise<LoginResult> => {
    const response = await request(AUTH_LOGIN_PATH, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: origin },
      body: JSON.stringify({ username, password }),
    });
    let payload: Record<string, unknown> | null = null;
    try {
      payload = (await response.json()) as Record<string, unknown>;
    } catch {
      // Error paths can be empty.
    }
    const setCookie = response.headers.get("set-cookie");
    return {
      status: response.status,
      payload,
      cookie: setCookie ? setCookie.split(";", 1)[0]! : null,
    };
  };

  const adminPost = async (
    cookie: string,
    csrfToken: string,
    body: unknown,
    headers: Record<string, string> = {},
  ): Promise<{ status: number; json: unknown }> => {
    const response = await request(ADMIN_USERS_PATH, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Origin: origin,
        "X-CSRF-Token": csrfToken,
        Cookie: cookie,
        ...headers,
      },
      body: typeof body === "string" ? body : JSON.stringify(body),
    });
    let json: unknown = null;
    try {
      json = await response.json();
    } catch {
      // empty
    }
    return { status: response.status, json };
  };

  const adminDelete = async (
    cookie: string,
    csrfToken: string,
    username: string,
    headers: Record<string, string> = {},
  ): Promise<{ status: number; json: unknown }> => {
    const response = await request(`${ADMIN_USERS_PATH}?username=${encodeURIComponent(username)}`, {
      method: "DELETE",
      headers: { Origin: origin, "X-CSRF-Token": csrfToken, Cookie: cookie, ...headers },
    });
    let json: unknown = null;
    try {
      json = await response.json();
    } catch {
      // empty
    }
    return { status: response.status, json };
  };

  const adminGet = async (cookie: string, path: string): Promise<{ status: number; json: unknown }> => {
    const response = await request(path, cookie ? { headers: { Cookie: cookie } } : {});
    let json: unknown = null;
    try {
      json = await response.json();
    } catch {
      // empty
    }
    return { status: response.status, json };
  };

  return {
    dir,
    db,
    store,
    auth,
    sessions,
    server,
    origin,
    users,
    changes,
    login,
    waitLoginStart: () => new Promise<void>((resolve) => loginWaiters.push(resolve)),
    request,
    adminPost,
    adminDelete,
    adminGet,
    close: async () => {
      sessions.dispose();
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
      db.close();
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

async function ownerSession(hub: Harness): Promise<{ cookie: string; csrf: string }> {
  const result = await hub.login("owner", "owner-password");
  assert.equal(result.status, 200);
  assert.ok(result.cookie);
  assert.equal(typeof result.payload?.csrfToken, "string");
  return { cookie: result.cookie, csrf: result.payload!.csrfToken as string };
}

describe("AdminHttp", () => {
  it("requires a session and explicit admin; a wildcard viewer is denied", async (t) => {
    const hub = await startHarness();
    try {
      assert.equal((await hub.adminGet("", ADMIN_USERS_PATH)).status, 401);

      const viewer = await hub.login("viewer", "viewer-password");
      assert.equal(viewer.status, 200);
      assert.equal(viewer.payload?.admin, undefined, "legacy session payload stays unchanged");
      assert.equal((await hub.adminGet(viewer.cookie!, ADMIN_USERS_PATH)).status, 403);
      assert.equal(
        (await hub.adminPost(viewer.cookie!, viewer.payload!.csrfToken as string, {
          username: "escalated",
          machineIds: "*",
          admin: true,
          password: "password",
        })).status,
        403,
      );
      assert.equal(hub.store.isAdmin("viewer"), false);
    } finally {
      await hub.close();
    }
  });

  it("lists public user records without hashes and creates a login-capable user", async (t) => {
    const hub = await startHarness();
    try {
      const owner = await ownerSession(hub);
      const listed = await hub.adminGet(owner.cookie, ADMIN_USERS_PATH);
      assert.equal(listed.status, 200);
      const users = (listed.json as { users: Array<Record<string, unknown>> }).users;
      assert.equal(users.length, 2);
      assert.equal(JSON.stringify(listed.json).includes(OWNER_HASH), false);
      assert.equal(users.some((user) => "passwordHash" in user), false);
      assert.deepEqual(users.find((user) => user.username === "owner"), {
        username: "owner",
        machineIds: "*",
        admin: true,
        disabled: false,
      });

      const created = await hub.adminPost(owner.cookie, owner.csrf, {
        username: "newbie",
        machineIds: ["m-1"],
        admin: false,
        password: "newbie-password",
      });
      assert.equal(created.status, 200);
      assert.deepEqual(hub.changes.at(-1), ["newbie"]);

      const newbie = await hub.login("newbie", "newbie-password");
      assert.equal(newbie.status, 200);
      assert.equal(newbie.payload?.admin, undefined);
      assert.ok(newbie.cookie);
    } finally {
      await hub.close();
    }
  });

  it("accepts an explicit machine scope beyond fifty IDs", async (t) => {
    const hub = await startHarness();
    try {
      const owner = await ownerSession(hub);
      const machineIds = Array.from({ length: 60 }, (_, index) => `machine-${index}`);
      const created = await hub.adminPost(owner.cookie, owner.csrf, {
        username: "scoped",
        machineIds,
        admin: false,
        password: "scoped-password",
      });
      assert.equal(created.status, 200);
      assert.deepEqual(hub.changes.at(-1), ["scoped"]);

      const listed = await hub.adminGet(owner.cookie, ADMIN_USERS_PATH);
      const users = (listed.json as { users: Array<Record<string, unknown>> }).users;
      assert.deepEqual(
        users.find((user) => user.username === "scoped")?.machineIds,
        machineIds,
      );
      const scoped = await hub.login("scoped", "scoped-password");
      assert.equal(scoped.status, 200);
    } finally {
      await hub.close();
    }
  });

  it("requires exact origin and a constant-time CSRF token for mutations", async (t) => {
    const hub = await startHarness();
    try {
      const owner = await ownerSession(hub);
      const body = { username: "csrf-user", machineIds: "*", admin: false, password: "password" };
      assert.equal((await hub.adminPost(owner.cookie, "", body)).status, 403);
      assert.equal(
        (await hub.adminPost(owner.cookie, owner.csrf, body, { Origin: "http://evil.invalid" })).status,
        403,
      );
      assert.equal((await hub.adminPost(owner.cookie, `${owner.csrf}0`, body)).status, 403);
      assert.equal(hub.store.listUsers().some((user) => user.username === "csrf-user"), false);
      assert.equal(hub.changes.length, 0);
    } finally {
      await hub.close();
    }
  });

  it("rejects invalid fields, scopes, JSON and oversized bodies", async (t) => {
    const hub = await startHarness();
    try {
      const owner = await ownerSession(hub);
      const valid = { username: "valid-name", machineIds: "*", admin: false, password: "password" };
      const cases: unknown[] = [
        { ...valid, extra: true },
        { ...valid, machineIds: ["dup", "dup"] },
        { ...valid, username: "bad name" },
        { ...valid, admin: "yes" },
        { ...valid, password: "" },
      ];
      for (const body of cases) {
        assert.equal((await hub.adminPost(owner.cookie, owner.csrf, body)).status, 400);
      }
      assert.equal((await hub.adminPost(owner.cookie, owner.csrf, "{not json")).status, 400);
      assert.equal(
        (await hub.adminPost(owner.cookie, owner.csrf, {
          ...valid,
          password: "x".repeat(9_000),
        })).status,
        413,
      );
      assert.equal(hub.store.listUsers().length, 2);
    } finally {
      await hub.close();
    }
  });

  it("protects the last admin and returns meaningful conflict codes", async (t) => {
    const hub = await startHarness({
      users: [{ username: "owner", passwordHash: OWNER_HASH, machineIds: "*", admin: true }],
    });
    try {
      const owner = await ownerSession(hub);
      assert.equal((await hub.adminDelete(owner.cookie, owner.csrf, "owner")).status, 409);
      assert.equal(
        (await hub.adminPost(owner.cookie, owner.csrf, {
          username: "second",
          machineIds: "*",
          admin: true,
          password: "second-password",
        })).status,
        200,
      );
      assert.equal((await hub.adminDelete(owner.cookie, owner.csrf, "second")).status, 200);
    } finally {
      await hub.close();
    }
  });

  it("returns bounded, redacted audit events and validates limits", async (t) => {
    const hub = await startHarness();
    try {
      const owner = await ownerSession(hub);
      await hub.adminPost(owner.cookie, owner.csrf, {
        username: "audited",
        machineIds: "*",
        admin: false,
        password: "audited-password",
      });
      await hub.adminDelete(owner.cookie, owner.csrf, "audited");
      const audit = await hub.adminGet(owner.cookie, `${ADMIN_AUDIT_PATH}?limit=10`);
      assert.equal(audit.status, 200);
      const events = (audit.json as { events: Array<Record<string, unknown>> }).events;
      assert.equal(events.length, 2);
      assert.deepEqual(
        events.map((event) => event.action),
        ["user.delete", "user.create"],
      );
      const serialized = JSON.stringify(events);
      assert.equal(serialized.includes("audited-password"), false);
      assert.equal(serialized.includes(OWNER_HASH), false);

      assert.equal((await hub.adminGet(owner.cookie, `${ADMIN_AUDIT_PATH}?limit=0`)).status, 400);
      assert.equal((await hub.adminGet(owner.cookie, `${ADMIN_AUDIT_PATH}?limit=abc`)).status, 400);
      assert.equal((await hub.adminGet(owner.cookie, `${ADMIN_AUDIT_PATH}?limit=5000`)).status, 200);
    } finally {
      await hub.close();
    }
  });

  it("rejects wrong methods and remains closed when no browser users are configured", async (t) => {
    const hub = await startHarness();
    const closed = await startHarness({ users: [] });
    try {
      const owner = await ownerSession(hub);
      assert.equal(
        (await hub.request(ADMIN_USERS_PATH, { method: "PUT", headers: { Cookie: owner.cookie } })).status,
        405,
      );
      assert.equal(
        (await hub.request(ADMIN_AUDIT_PATH, { method: "POST", headers: { Cookie: owner.cookie } })).status,
        405,
      );
      assert.equal((await closed.adminGet("", ADMIN_USERS_PATH)).status, 503);
    } finally {
      await hub.close();
      await closed.close();
    }
  });

  it("limits concurrent password hashing to two at the HTTP boundary", async (t) => {
    const hub = await startHarness({
      users: [{ username: "owner", passwordHash: OWNER_HASH, machineIds: "*", admin: true }],
    });
    try {
      const owner = await ownerSession(hub);
      const first = hub.adminPost(owner.cookie, owner.csrf, {
        username: "hash-one",
        machineIds: "*",
        admin: false,
        password: "password-one",
      });
      const second = hub.adminPost(owner.cookie, owner.csrf, {
        username: "hash-two",
        machineIds: "*",
        admin: false,
        password: "password-two",
      });
      const third = await hub.adminPost(owner.cookie, owner.csrf, {
        username: "hash-three",
        machineIds: "*",
        admin: false,
        password: "password-three",
      });
      assert.equal(third.status, 503);
      assert.equal((await first).status, 200);
      assert.equal((await second).status, 200);
      assert.equal(hub.store.listUsers().filter((user) => user.username.startsWith("hash-")).length, 2);
    } finally {
      await hub.close();
    }
  });

  it("invalidates an in-flight login when scope/admin changes after scrypt starts", async (t) => {
    const hub = await startHarness();
    try {
      const started = hub.waitLoginStart();
      const pending = hub.login("owner", "owner-password");
      await started;
      await sleep(5);
      hub.auth.replaceUsers([
        { username: "owner", passwordHash: OWNER_HASH, machineIds: ["m-2"], admin: false },
        ...(hub.users.filter((user) => user.username !== "owner")),
      ]);
      const result = await pending;
      assert.equal(result.status, 401);
      assert.equal(result.cookie, null);
      assert.equal(hub.sessions.size(), 0);
    } finally {
      await hub.close();
    }
  });

  it("invalidates an in-flight login when the password changes", async (t) => {
    const hub = await startHarness();
    try {
      const started = hub.waitLoginStart();
      const pending = hub.login("owner", "owner-password");
      await started;
      await sleep(5);
      hub.auth.replaceUsers([
        { username: "owner", passwordHash: hashPassword("changed-password"), machineIds: "*", admin: true },
        ...(hub.users.filter((user) => user.username !== "owner")),
      ]);
      const result = await pending;
      assert.equal(result.status, 401);
      assert.equal(hub.sessions.size(), 0);
    } finally {
      await hub.close();
    }
  });

  it("invalidates an in-flight login when the user is disabled/removed", async (t) => {
    const hub = await startHarness();
    try {
      const started = hub.waitLoginStart();
      const pending = hub.login("owner", "owner-password");
      await started;
      hub.auth.replaceUsers(hub.users.filter((user) => user.username !== "owner"));
      const result = await pending;
      assert.equal(result.status, 401);
      assert.equal(hub.sessions.size(), 0);
    } finally {
      await hub.close();
    }
  });
});
