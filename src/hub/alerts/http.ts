/**
 * Authenticated alert HTTP surface.
 *
 * Every request requires a browser session. Mutations additionally require
 * an explicit admin, the exact trusted Origin, a CSRF token, and a bounded
 * strict-JSON body. Rule lists and mutations are constrained to the session's
 * machine scope. Webhook responses never include the URL, headers or secret.
 */
import { timingSafeEqual } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";

import {
  alertScopeIncludes,
  type AlertMachineScope,
  type AlertRuleInput,
  type AlertWebhookConfigureInput,
} from "../../alerts/types";
import type { BrowserHttpAuth, AuthOriginRules } from "../auth/http";
import { isTrustedOrigin, readJsonBody, sendJson } from "../auth/http";
import { AlertNotFoundError, AlertValidationError } from "./errors";
import type { AlertEngine } from "./engine";

export type AlertsHttpOptions = {
  engine: AlertEngine;
  auth: BrowserHttpAuth;
  origin: AuthOriginRules;
  isAdmin: (username: string) => boolean;
};

const ALERT_ROOT = "/api/alerts";
const RULES_PATH = "/api/alerts/rules";
const WEBHOOK_PATH = "/api/alerts/webhook";
const RULE_BODY_BYTES = 16 * 1024;
const WEBHOOK_BODY_BYTES = 8 * 1024;
const RULE_KEYS = [
  "id",
  "name",
  "machineId",
  "metric",
  "resourceId",
  "condition",
  "threshold",
  "durationMs",
  "enabled",
  "maintenanceUntil",
] as const;
const RULE_REQUIRED = ["name", "machineId", "metric", "condition", "durationMs"] as const;
const WEBHOOK_CONFIG_KEYS = ["action", "url", "headers", "secret", "enabled"] as const;
const WEBHOOK_ACTION_KEYS = ["action"] as const;

export class AlertsHttp {
  constructor(private readonly options: AlertsHttpOptions) {}

  async handle(req: IncomingMessage, res: ServerResponse, url: URL): Promise<boolean> {
    const path = url.pathname;
    if (path !== ALERT_ROOT && !path.startsWith(`${ALERT_ROOT}/`)) return false;
    const {engine, auth, origin, isAdmin} = this.options;
    const session = auth.requestSession(req);
    if (!session) {
      sendJson(res, 401, {error: "authentication required"});
      return true;
    }
    const method = (req.method ?? "GET").toUpperCase();

    try {
      if (path === ALERT_ROOT) {
        if (method !== "GET") {
          sendJson(res, 405, {error: "GET required"}, {Allow: "GET"});
          return true;
        }
        const limitRaw = url.searchParams.get("limit");
        const limit = limitRaw === null ? 200 : Number(limitRaw);
        if (!Number.isSafeInteger(limit) || limit < 1 || limit > 500) {
          sendJson(res, 400, {error: "limit must be an integer from 1 to 500"});
          return true;
        }
        const response = engine.listResponse(session.scope, isAdmin(session.username));
        response.incidents = engine.listIncidents(session.scope, limit);
        sendJson(res, 200, response);
        return true;
      }

      if (!this.authorizedMutation(req, res, session)) return true;

      if (path === RULES_PATH) {
        if (method === "POST") {
          await this.handleRuleUpsert(req, res, session.scope);
          return true;
        }
        if (method === "DELETE") {
          this.handleRuleDelete(res, url, session.scope);
          return true;
        }
        sendJson(res, 405, {error: "POST or DELETE required"}, {Allow: "POST, DELETE"});
        return true;
      }

      if (path === WEBHOOK_PATH) {
        if (method !== "POST") {
          sendJson(res, 405, {error: "POST required"}, {Allow: "POST"});
          return true;
        }
        await this.handleWebhook(req, res);
        return true;
      }

      sendJson(res, 404, {error: "unknown alerts endpoint"});
      return true;
    } catch (error) {
      this.fail(res, error);
      return true;
    }
  }

  private authorizedMutation(
    req: IncomingMessage,
    res: ServerResponse,
    session: import("../auth/sessions").BrowserSession,
  ): boolean {
    if (!this.options.isAdmin(session.username)) {
      sendJson(res, 403, {error: "administrative access required"});
      return false;
    }
    if (!isTrustedOrigin(req, this.options.origin)) {
      sendJson(res, 403, {error: "origin not allowed"});
      return false;
    }
    const csrf = req.headers["x-csrf-token"];
    if (
      typeof csrf !== "string" ||
      Buffer.byteLength(csrf) !== Buffer.byteLength(session.csrfToken) ||
      !timingSafeEqual(Buffer.from(csrf), Buffer.from(session.csrfToken))
    ) {
      sendJson(res, 403, {error: "missing or invalid csrf token"});
      return false;
    }
    return true;
  }

  private async handleRuleUpsert(
    req: IncomingMessage,
    res: ServerResponse,
    scope: AlertMachineScope,
  ): Promise<void> {
    if (!(req.headers["content-type"] ?? "").startsWith("application/json")) {
      sendJson(res, 415, {error: "JSON required"});
      return;
    }
    const body = await readJsonBody(req, RULE_BODY_BYTES);
    if (!body.ok) {
      sendJson(res, body.status, {error: body.error});
      return;
    }
    if (!isRecord(body.value) || !hasExactKeys(body.value, RULE_KEYS, RULE_REQUIRED)) {
      sendJson(res, 400, {error: "invalid alert rule body"});
      return;
    }
    const record = body.value;
    const machineId = record["machineId"];
    if (typeof machineId !== "string" || !alertScopeIncludes(scope, machineId)) {
      sendJson(res, 403, {error: "machine access denied"});
      return;
    }
    const input = record as unknown as AlertRuleInput;
    const id = record["id"];
    if (id !== undefined) {
      if (typeof id !== "string" || id.length === 0 || id.length > 128) {
        sendJson(res, 400, {error: "invalid rule id"});
        return;
      }
      const existing = this.options.engine.getRule(id);
      if (!alertScopeIncludes(scope, existing.machineId)) {
        sendJson(res, 403, {error: "machine access denied"});
        return;
      }
      const rule = this.options.engine.updateRule(id, input);
      sendJson(res, 200, {rule});
      return;
    }
    const rule = this.options.engine.createRule(input);
    sendJson(res, 201, {rule});
  }

  private handleRuleDelete(res: ServerResponse, url: URL, scope: AlertMachineScope): void {
    const id = url.searchParams.get("id");
    if (id === null || id.length === 0 || id.length > 128) {
      sendJson(res, 400, {error: "a bounded rule id is required"});
      return;
    }
    if (url.searchParams.getAll("id").length !== 1) {
      sendJson(res, 400, {error: "exactly one rule id is required"});
      return;
    }
    const rule = this.options.engine.getRule(id);
    if (!alertScopeIncludes(scope, rule.machineId)) {
      sendJson(res, 403, {error: "machine access denied"});
      return;
    }
    this.options.engine.deleteRule(id);
    sendJson(res, 200, {ok: true});
  }

  private async handleWebhook(req: IncomingMessage, res: ServerResponse): Promise<void> {
    if (!(req.headers["content-type"] ?? "").startsWith("application/json")) {
      sendJson(res, 415, {error: "JSON required"});
      return;
    }
    const body = await readJsonBody(req, WEBHOOK_BODY_BYTES);
    if (!body.ok) {
      sendJson(res, body.status, {error: body.error});
      return;
    }
    if (!isRecord(body.value)) {
      sendJson(res, 400, {error: "invalid webhook request"});
      return;
    }
    const action = body.value["action"];
    if (action === "configure") {
      if (!hasExactKeys(body.value, WEBHOOK_CONFIG_KEYS, ["action", "url"])) {
        sendJson(res, 400, {error: "invalid webhook configuration body"});
        return;
      }
      const input = body.value as unknown as AlertWebhookConfigureInput;
      const status = this.options.engine.configureWebhook(input);
      sendJson(res, 200, {status});
      return;
    }
    if (action === "clear") {
      if (!hasExactKeys(body.value, WEBHOOK_ACTION_KEYS, ["action"])) {
        sendJson(res, 400, {error: "invalid webhook clear body"});
        return;
      }
      sendJson(res, 200, {status: this.options.engine.clearWebhook()});
      return;
    }
    if (action === "test") {
      if (!hasExactKeys(body.value, WEBHOOK_ACTION_KEYS, ["action"])) {
        sendJson(res, 400, {error: "invalid webhook test body"});
        return;
      }
      const result = await this.options.engine.testWebhook();
      sendJson(res, 200, {result, status: this.options.engine.getWebhookStatus()});
      return;
    }
    sendJson(res, 400, {error: "unknown webhook action"});
  }

  private fail(res: ServerResponse, error: unknown): void {
    if (error instanceof AlertValidationError) {
      sendJson(res, 400, {error: error.message});
      return;
    }
    if (error instanceof AlertNotFoundError) {
      sendJson(res, 404, {error: "alert rule not found"});
      return;
    }
    sendJson(res, 503, {error: "alerts request temporarily unavailable"});
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function hasExactKeys(
  value: Record<string, unknown>,
  allowed: readonly string[],
  required: readonly string[],
): boolean {
  const allowedSet = new Set(allowed);
  for (const key of Object.keys(value)) {
    if (!allowedSet.has(key)) return false;
  }
  for (const key of required) {
    if (value[key] === undefined) return false;
  }
  return true;
}
