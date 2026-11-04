import { strict as assert } from "node:assert";
import { describe, it } from "node:test";
import {
  AdminApiError,
  createAdminApi,
  isAbortError,
  type AuditEvent,
  type CollectorRecord,
} from "../src/admin/api";
import {
  MAX_SCOPE_BYTES,
  formatAdminTimestamp,
  formatAuditAction,
  formatMachineLabel,
  grantCommand,
  parseMachineMetaDraft,
  parseTagsInput,
  scopeIds,
  scopeSummary,
  toggleScopeSelection,
  unknownScopeIds,
  validateUserDraft,
} from "../src/admin/presentation";

type Call = { url: string; init: RequestInit };

function fakeApi(handler: (call: Call) => Response | Promise<Response>) {
  const calls: Call[] = [];
  const fetchImpl = (async (input: string | URL | Request, init: RequestInit = {}) => {
    const call = { url: String(input), init };
    calls.push(call);
    return handler(call);
  }) as typeof fetch;
  return { api: createAdminApi({ fetchImpl }), calls };
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("admin presentation helpers", () => {
  it("parses comma-separated tags, trims, and deduplicates", () => {
    assert.deepEqual(parseTagsInput(" prod, desk ,,prod, lab "), {
      tags: ["prod", "desk", "lab"],
      error: null,
    });
    assert.deepEqual(parseTagsInput("   "), { tags: [], error: null });
    assert.match(parseTagsInput("x".repeat(65)).error ?? "", /64/);
    assert.match(parseTagsInput("bad\u0000tag").error ?? "", /control/);
    assert.match(
      parseTagsInput(Array.from({ length: 33 }, (_, index) => `t${index}`).join(",")).error ?? "",
      /32/,
    );
  });

  it("validates machine metadata on submit", () => {
    assert.deepEqual(parseMachineMetaDraft("  Desk  ", "prod, desk"), {
      displayName: "Desk",
      tags: ["prod", "desk"],
      errors: {},
    });
    assert.deepEqual(parseMachineMetaDraft("   ", ""), {
      displayName: null,
      tags: [],
      errors: {},
    });
    assert.match(parseMachineMetaDraft("x".repeat(129), "").errors.displayName ?? "", /128/);
    assert.match(parseMachineMetaDraft("bad\u0007name", "").errors.displayName ?? "", /control/);
  });

  it("requires a password for new users but not for existing users", () => {
    const base = {
      username: "new-user",
      password: "",
      machineIds: "*" as const,
      admin: false,
      disabled: false,
    };
    const created = validateUserDraft(base, { existing: false });
    assert.equal(created.firstInvalid, "password");
    assert.match(created.errors.password ?? "", /Choose a password/);

    assert.deepEqual(validateUserDraft(base, { existing: true }), {
      errors: {},
      firstInvalid: null,
    });

    const tooLong = validateUserDraft(
      { ...base, password: "x".repeat(1025) },
      { existing: false },
    );
    assert.match(tooLong.errors.password ?? "", /1024/);
  });

  it("validates identifiers and scopes strictly", () => {
    const draft = {
      username: "bad name",
      password: "password",
      machineIds: ["dup", "dup"],
      admin: false,
      disabled: false,
    };
    const result = validateUserDraft(draft, { existing: false });
    assert.equal(result.firstInvalid, "username");
    assert.ok(result.errors.username);
    assert.ok(result.errors.scope);

    assert.equal(
      validateUserDraft(
        {
          ...draft,
          username: "ok",
          machineIds: Array.from({ length: 60 }, (_, i) => `m-${i}`),
        },
        { existing: false },
      ).firstInvalid,
      null,
    );
    const oversized = validateUserDraft(
      {
        ...draft,
        username: "ok",
        machineIds: Array.from({ length: 100 }, (_, i) => `m-${i}-${"x".repeat(110)}`),
      },
      { existing: false },
    );
    assert.equal(oversized.firstInvalid, "scope");
    assert.match(oversized.errors.scope ?? "", new RegExp(`${MAX_SCOPE_BYTES} bytes`));
    assert.equal(
      validateUserDraft(
        { ...draft, username: "ok", machineIds: ["m-1", "m-2"] },
        { existing: false },
      ).firstInvalid,
      null,
    );
  });

  it("summarizes scopes and tracks known machines without dropping unknown IDs", () => {
    assert.equal(scopeSummary("*"), "All machines");
    assert.equal(scopeSummary([]), "No machines");
    assert.equal(scopeSummary(["m-1"]), "m-1");
    assert.equal(scopeSummary(["m-1", "m-2"]), "2 machines");
    assert.deepEqual(scopeIds(["a", "b"]), ["a", "b"]);
    assert.deepEqual(unknownScopeIds(["known", "old"], ["known"]), ["old"]);
    assert.deepEqual(toggleScopeSelection(["a"], "b"), ["a", "b"]);
    assert.deepEqual(toggleScopeSelection(["a", "b"], "a"), ["b"]);
  });

  it("builds enrollment commands for the current hub without a secret argument", () => {
    const created = grantCommand("new", "https://rig.example.com:8443");
    assert.equal(created.command, "rigscope connect 'https://rig.example.com:8443'");
    assert.doesNotMatch(created.command, /--replace|TOKEN|grant-secret/);
    assert.match(created.note, /prompt/);

    const replacement = grantCommand("replacement");
    assert.match(replacement.command, /--replace$/);
    assert.match(replacement.note, /stays active until/);
    assert.throws(() => grantCommand("new", "https://user:secret@rig.example.com"));
    assert.doesNotMatch(replacement.command, /grant-secret/);
  });

  it("formats audit actions, timestamps, and machine labels", () => {
    assert.equal(formatAuditAction("user.create"), "Created user");
    assert.equal(formatAuditAction("user.update-password"), "Changed password");
    assert.equal(formatAuditAction("collector.revoke"), "collector revoke");
    assert.equal(formatAdminTimestamp("not-a-date"), "Unknown time");
    assert.match(formatAdminTimestamp("2026-11-02T12:00:00.000Z"), /2026/);

    assert.equal(
      formatMachineLabel({ machineId: "m-1", hostname: "host-1", displayName: "Desk" }),
      "Desk",
    );
    assert.equal(formatMachineLabel({ machineId: "m-1", hostname: "host-1" }), "host-1");
    assert.equal(formatMachineLabel({ machineId: "m-1" }), "m-1");
  });
});

describe("admin API client", () => {
  it("loads public user records with cache disabled and same-origin credentials", async () => {
    const { api, calls } = fakeApi(() =>
      json({
        users: [{ username: "owner", machineIds: "*", admin: true, disabled: false }],
      }),
    );
    const users = await api.listUsers();
    assert.deepEqual(users, [
      { username: "owner", machineIds: "*", admin: true, disabled: false },
    ]);
    assert.equal(calls[0]!.url, "/api/admin/users");
    assert.equal(calls[0]!.init.method, "GET");
    assert.equal(calls[0]!.init.cache, "no-store");
    assert.equal(calls[0]!.init.credentials, "same-origin");
    assert.equal((calls[0]!.init.headers as Record<string, string>)["X-CSRF-Token"], undefined);
  });

  it("sends user mutations with CSRF in a header, never in the URL", async () => {
    const { api, calls } = fakeApi(() => json({ ok: true }));
    await api.saveUser(
      {
        username: "new-user",
        machineIds: ["m-1"],
        admin: false,
        password: "plain-secret",
      },
      "csrf-token",
    );
    assert.equal(calls[0]!.url, "/api/admin/users");
    assert.equal(calls[0]!.init.method, "POST");
    const headers = calls[0]!.init.headers as Record<string, string>;
    assert.equal(headers["X-CSRF-Token"], "csrf-token");
    assert.equal(headers["Content-Type"], "application/json");
    assert.match(String(calls[0]!.init.body), /plain-secret/);
    assert.doesNotMatch(calls[0]!.url, /plain-secret|password/);

    await api.deleteUser("user with space", "csrf-token");
    assert.equal(calls[1]!.url, "/api/admin/users?username=user%20with%20space");
    assert.equal(calls[1]!.init.method, "DELETE");
  });

  it("bounds audit reads and sends exact collector/grant/meta payloads", async () => {
    const audit: AuditEvent[] = [
      { actor: "owner", action: "user.create", target: "new", at: "2026-11-02T12:00:00.000Z" },
    ];
    const collectors: CollectorRecord[] = [
      {
        collectorId: "collector-1",
        machineId: "machine-1",
        revoked: false,
        updatedAt: "2026-11-02T12:00:00.000Z",
      },
    ];
    const { api, calls } = fakeApi((call) => {
      if (call.url.startsWith("/api/admin/audit")) return json({ events: audit });
      if (call.url === "/api/admin/collectors" && call.init.method === "GET") {
        return json({ collectors });
      }
      if (call.url === "/api/admin/collector-grants") {
        return json({ token: "grant-secret", expiresAt: "2026-11-02T13:00:00.000Z" }, 201);
      }
      return json({ ok: true });
    });

    assert.deepEqual(await api.listAudit(5000), audit);
    assert.match(calls[0]!.url, /limit=1000$/);
    assert.deepEqual(await api.listCollectors(), collectors);

    const grant = await api.createGrant({ replaceMachineId: "machine-1", ttlMs: 300000 }, "csrf");
    assert.deepEqual(grant, {
      token: "grant-secret",
      expiresAt: "2026-11-02T13:00:00.000Z",
    });
    assert.deepEqual(JSON.parse(String(calls[2]!.init.body)), {
      replaceMachineId: "machine-1",
      ttlMs: 300000,
    });

    await api.revokeCollector("collector-1", "csrf");
    assert.deepEqual(JSON.parse(String(calls[3]!.init.body)), {
      action: "revoke",
      collectorId: "collector-1",
    });

    await api.saveMachineMeta(
      "machine with space",
      { displayName: "Desk", tags: ["prod"] },
      "csrf",
    );
    assert.equal(calls[4]!.url, "/api/machines/meta?machineId=machine%20with%20space");
    assert.deepEqual(JSON.parse(String(calls[4]!.init.body)), {
      displayName: "Desk",
      tags: ["prod"],
    });
  });

  it("surfaces meaningful server failures without exposing secrets", async () => {
    const conflict = fakeApi(() =>
      json({ error: "the last active administrator cannot be removed or demoted" }, 409),
    );
    await assert.rejects(conflict.api.saveUser({ username: "owner", machineIds: "*", admin: false }, "csrf"), (error: unknown) => {
      assert.ok(error instanceof AdminApiError);
      assert.equal(error.status, 409);
      assert.match(error.message, /last active administrator/);
      return true;
    });

    const expired = fakeApi(() => json({ error: "authentication required" }, 401));
    await assert.rejects(expired.api.listUsers(), (error: unknown) => {
      assert.ok(error instanceof AdminApiError);
      assert.equal(error.status, 401);
      return true;
    });

    assert.equal(isAbortError({ name: "AbortError" }), true);
    assert.equal(isAbortError(new Error("other")), false);
  });
});
