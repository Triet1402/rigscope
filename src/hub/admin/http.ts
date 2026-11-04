/**
 * Administrative HTTP endpoints for the persistent browser user directory.
 *
 * Routes: GET/POST/DELETE /api/admin/users and GET /api/admin/audit.
 * Every route requires a live browser session and an explicit active-admin
 * lookup in AdminStore. Nothing in the request body grants permission; a
 * wildcard machine scope is not administrative access.
 *
 * Mutations additionally require the exact same origin configured for auth and
 * a constant-time match of the session CSRF token. Responses are bounded,
 * cache-disabled and secret-free: no password, hash or raw request echo.
 */

import type { IncomingMessage, ServerResponse } from "node:http";
import {
  type AuthOriginRules,
  type BrowserHttpAuth,
  isTrustedOrigin,
  readJsonBody,
  sendJson,
} from "../auth/http";
import { MAX_BROWSER_PASSWORD_CHARS } from "../auth/password";
import { AdminError, type AdminStore, type AdminUserInput } from "./store";

export const ADMIN_USERS_PATH = "/api/admin/users";
export const ADMIN_AUDIT_PATH = "/api/admin/audit";
export const MAX_ADMIN_BODY_BYTES = 8 * 1024;

const MAX_AUDIT_LIMIT = 1000;
const USERNAME_PATTERN = /^[\x21-\x7e]{1,128}$/;

export type AdminChanged = (touchedUsernames: string[]) => void;

export class AdminHttp {
  private readonly store: AdminStore;
  private readonly auth: BrowserHttpAuth;
  private readonly origin: AuthOriginRules;
  private readonly onChanged?: AdminChanged;
  private readonly extraAudit?: () => import('./store').AdminAuditEvent[];

  constructor(input: {
    store: AdminStore;
    auth: BrowserHttpAuth;
    origin: AuthOriginRules;
    onChanged?: AdminChanged;
    extraAudit?: () => import('./store').AdminAuditEvent[];
  }) {
    this.store = input.store;
    this.auth = input.auth;
    this.origin = input.origin;
    this.onChanged = input.onChanged;
    this.extraAudit = input.extraAudit;
  }

  /** Handles /api/admin/*; returns false for unrelated paths. */
  async handle(req: IncomingMessage, res: ServerResponse, url: URL): Promise<boolean> {
    const path = url.pathname;
    if (path !== ADMIN_USERS_PATH && path !== ADMIN_AUDIT_PATH) return false;

    if (!this.auth.configured) {
      sendJson(res, 503, { error: "browser access is not configured on this hub" });
      return true;
    }
    const session = this.auth.requestSession(req);
    if (!session) {
      sendJson(res, 401, { error: "authentication required" });
      return true;
    }

    try {
      // Explicit store lookup on every request: a session snapshot is never an
      // authorization source, and a wildcard scope is not an admin grant.
      if (!this.store.isAdmin(session.username)) {
        sendJson(res, 403, { error: "administrative access required" });
        return true;
      }
      if (path === ADMIN_USERS_PATH) {
        await this.handleUsers(req, res, url, session.username, session.csrfToken);
      } else {
        this.handleAudit(req, res, url);
      }
    } catch (error) {
      sendAdminFailure(res, error);
    }
    return true;
  }

  private async handleUsers(
    req: IncomingMessage,
    res: ServerResponse,
    url: URL,
    actor: string,
    csrfToken: string,
  ): Promise<void> {
    if (req.method === "GET") {
      sendJson(res, 200, { users: this.store.listUsers() });
      return;
    }
    if (req.method === "POST") {
      this.requireMutationProof(req, csrfToken);
      const type = req.headers["content-type"] ?? "";
      if (!type.startsWith("application/json")) {
        sendJson(res, 415, { error: "JSON required" });
        return;
      }
      const body = await readJsonBody(req, MAX_ADMIN_BODY_BYTES);
      if (!body.ok) {
        sendJson(res, body.status, { error: body.error }, { Connection: "close" });
        return;
      }
      const input = parseUserInput(body.value);
      await this.store.upsertUser(actor, input);
      this.afterMutation([input.username]);
      sendJson(res, 200, { ok: true });
      return;
    }
    if (req.method === "DELETE") {
      this.requireMutationProof(req, csrfToken);
      const values = url.searchParams.getAll("username");
      if (values.length !== 1) {
        sendJson(res, 400, { error: "exactly one username is required" });
        return;
      }
      const username = validateUsername(values[0]);
      this.store.deleteUser(actor, username);
      this.afterMutation([username]);
      sendJson(res, 200, { ok: true });
      return;
    }
    sendJson(res, 405, { error: "method not allowed" }, { Allow: "GET, POST, DELETE" });
  }

  private handleAudit(req: IncomingMessage, res: ServerResponse, url: URL): void {
    if (req.method !== "GET") {
      sendJson(res, 405, { error: "GET required" }, { Allow: "GET" });
      return;
    }
    const values = url.searchParams.getAll("limit");
    let limit: number | undefined;
    if (values.length > 1) {
      sendJson(res, 400, { error: "at most one limit is allowed" });
      return;
    }
    if (values.length === 1) {
      if (!/^[1-9]\d*$/.test(values[0]!)) {
        sendJson(res, 400, { error: "limit must be a positive integer" });
        return;
      }
      limit = Math.min(Number(values[0]), MAX_AUDIT_LIMIT);
      if (!Number.isSafeInteger(limit)) {
        sendJson(res, 400, { error: "limit must be a positive integer" });
        return;
      }
    }
    const events = [...this.store.audit(limit), ...(this.extraAudit?.() ?? [])]
      .sort((a, b) => Date.parse(b.at) - Date.parse(a.at)).slice(0, limit ?? 100);
    sendJson(res, 200, { events });
  }

  private requireMutationProof(req: IncomingMessage, csrfToken: string): void {
    if (!isTrustedOrigin(req, this.origin)) {
      throw new AdminError("forbidden", "origin not allowed");
    }
    const header = req.headers["x-csrf-token"];
    if (
      typeof header !== "string" ||
      header.length !== csrfToken.length ||
      !constantTimeEquals(header, csrfToken)
    ) {
      throw new AdminError("forbidden", "missing or invalid csrf token");
    }
  }

  /** Reload auth and let the composition root revoke touched sessions. */
  private afterMutation(touchedUsernames: string[]): void {
    try {
      this.auth.replaceUsers(this.store.users());
    } catch (error) {
      // The durable commit already succeeded; keep the response truthful and
      // let the next login/read path re-evaluate.
      console.error("admin auth directory reload failed:", error);
    }
    if (!this.onChanged) return;
    try {
      this.onChanged(touchedUsernames);
    } catch (error) {
      console.error("admin change listener failed:", error);
    }
  }
}

function parseUserInput(value: unknown): AdminUserInput {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new AdminError("invalid", "invalid user request");
  }
  const record = value as Record<string, unknown>;
  const allowed = new Set(["username", "machineIds", "admin", "disabled", "password"]);
  for (const key of Object.keys(record)) {
    if (!allowed.has(key)) throw new AdminError("invalid", "unexpected user field");
  }
  for (const required of ["username", "machineIds", "admin"]) {
    if (!(required in record)) throw new AdminError("invalid", "missing required user field");
  }
  const username = validateUsername(record.username);
  const machineIds = parseScope(record.machineIds);
  if (typeof record.admin !== "boolean") throw new AdminError("invalid", "admin must be a boolean");
  if (record.disabled !== undefined && typeof record.disabled !== "boolean") {
    throw new AdminError("invalid", "disabled must be a boolean");
  }
  if (record.password !== undefined) {
    if (
      typeof record.password !== "string" ||
      record.password.length === 0 ||
      record.password.length > MAX_BROWSER_PASSWORD_CHARS
    ) {
      throw new AdminError(
        "invalid",
        `password must be a string of 1..${MAX_BROWSER_PASSWORD_CHARS} characters`,
      );
    }
  }
  return {
    username,
    machineIds,
    admin: record.admin,
    ...(record.disabled !== undefined ? { disabled: record.disabled } : {}),
    ...(record.password !== undefined ? { password: record.password } : {}),
  };
}

function parseScope(value: unknown): "*" | string[] {
  if (value === "*") return "*";
  if (!Array.isArray(value)) {
    throw new AdminError("invalid", `machineIds must be "*" or an array of IDs`);
  }
  const seen = new Set<string>();
  const ids: string[] = [];
  for (const id of value) {
    const machineId = validateUsername(id);
    if (seen.has(machineId)) throw new AdminError("invalid", "machineIds contains a duplicate");
    seen.add(machineId);
    ids.push(machineId);
  }
  return ids;
}

function validateUsername(value: unknown): string {
  if (typeof value !== "string" || !USERNAME_PATTERN.test(value)) {
    throw new AdminError("invalid", "invalid identifier");
  }
  return value;
}

function sendAdminFailure(res: ServerResponse, error: unknown): void {
  if (!(error instanceof AdminError)) {
    sendJson(res, 503, { error: "administration temporarily unavailable" });
    return;
  }
  switch (error.code) {
    case "invalid":
      sendJson(res, 400, { error: error.message });
      return;
    case "forbidden":
      sendJson(res, 403, { error: "administrative access required" });
      return;
    case "conflict":
    case "not-found":
      sendJson(res, error.code === "conflict" ? 409 : 404, { error: error.message });
      return;
    case "unavailable":
    default:
      sendJson(res, 503, { error: "administration is busy; retry shortly" });
  }
}

function constantTimeEquals(a: string, b: string): boolean {
  let diff = 0;
  for (let index = 0; index < a.length; index += 1) {
    diff |= a.charCodeAt(index) ^ b.charCodeAt(index);
  }
  return diff === 0;
}
