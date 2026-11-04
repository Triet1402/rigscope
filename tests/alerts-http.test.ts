/** Alert HTTP auth, origin, CSRF, scope, strict-body and secrecy tests. */
import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { once } from "node:events";
import type { AddressInfo } from "node:net";

import { BrowserHttpAuth, type AuthOriginRules } from "../src/hub/auth/http";
import { BrowserSessionStore } from "../src/hub/auth/sessions";
import { AlertEngine } from "../src/hub/alerts/engine";
import { AlertsHttp } from "../src/hub/alerts/http";
import { openHubDatabase } from "../src/hub/storage/db";
import { basePayload, metricsRecord } from "./alerts-fixtures";

type Harness = {
  base: string;
  origin: string;
  admin: {cookie: string; csrf: string};
  adminScoped: {cookie: string; csrf: string};
  viewer: {cookie: string; csrf: string};
  engine: AlertEngine;
  close: () => Promise<void>;
};

async function harness(t: test.TestContext): Promise<Harness> {
  const database = openHubDatabase(":memory:");
  const engine = new AlertEngine({
    database,
    now: () => 6_000_000,
    sendWebhook: async () => ({ok: true, status: 200, error: null}),
  });
  const sessions = new BrowserSessionStore({ttlMs: 60_000, sweepMs: 0});
  const adminSession = sessions.create("admin", "*", true);
  const scopedAdminSession = sessions.create("admin-scoped", ["m1"], true);
  const viewerSession = sessions.create("viewer", ["m1"], false);
  const originPort = 0;
  const origin: AuthOriginRules = {originPort, originHostnames: ["127.0.0.1"]};
  const auth = new BrowserHttpAuth({users: [], sessions, origin});
  let alerts: AlertsHttp;
  const server = createServer((req, res) => {
    const url = new URL(req.url ?? "/", `http://${req.headers.host ?? "127.0.0.1"}`);
    void alerts.handle(req, res, url).then((handled) => {
      if (!handled) {
        res.writeHead(404);
        res.end();
      }
    });
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const port = (server.address() as AddressInfo).port;
  // Rebuild the origin with the actual bound port.
  const actualOrigin: AuthOriginRules = {originPort: port, originHostnames: ["127.0.0.1"]};
  const actualAuth = new BrowserHttpAuth({users: [], sessions, origin: actualOrigin});
  alerts = new AlertsHttp({
    engine,
    auth: actualAuth,
    origin: actualOrigin,
    isAdmin: (username) => username === "admin" || username === "admin-scoped",
  });
  t.after(async () => {
    engine.stop();
    database.close();
    sessions.dispose();
    await new Promise<void>((resolve) => server.close(() => resolve()));
  });
  const base = `http://127.0.0.1:${port}`;
  return {
    base,
    origin: base,
    admin: {
      cookie: `rigscope_session=${adminSession.id}`,
      csrf: adminSession.csrfToken,
    },
    adminScoped: {
      cookie: `rigscope_session=${scopedAdminSession.id}`,
      csrf: scopedAdminSession.csrfToken,
    },
    viewer: {
      cookie: `rigscope_session=${viewerSession.id}`,
      csrf: viewerSession.csrfToken,
    },
    engine,
    close: async () => {},
  };
}

async function json(response: Response): Promise<Record<string, unknown>> {
  return (await response.json()) as Record<string, unknown>;
}

test("GET /api/alerts requires a session and filters rules/incidents by machine scope", async (t) => {
  const h = await harness(t);
  h.engine.createRule({
    name: "m1 rule",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 80,
    durationMs: 0,
  });
  h.engine.createRule({
    name: "m2 rule",
    machineId: "m2",
    metric: "cpu.usage",
    condition: "above",
    threshold: 80,
    durationMs: 0,
  });
  h.engine.createRule({
    name: "m1 fire",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 1,
    durationMs: 0,
  });
  h.engine.observe(
    "m1",
    metricsRecord(basePayload({cpu: {status: "ok", usagePercent: 99, perCore: [99, 99]}}), {sequence: 1}),
    6_000_000,
  );
  h.engine.configureWebhook({url: "http://127.0.0.1:9/sink", secret: "never-returned"});

  const unauthorized = await fetch(`${h.base}/api/alerts`);
  assert.equal(unauthorized.status, 401);

  const response = await fetch(`${h.base}/api/alerts`, {headers: {cookie: h.viewer.cookie}});
  assert.equal(response.status, 200);
  const body = await json(response);
  const rules = body.rules as Array<{machineId: string; name: string}>;
  assert.ok(rules.length >= 1);
  assert.ok(rules.every((rule) => rule.machineId === "m1"));
  const incidents = body.incidents as Array<{machineId: string}>;
  assert.ok(incidents.every((incident) => incident.machineId === "m1"));
  assert.equal(body.canAdmin, false);
  const serialized = JSON.stringify(body);
  assert.equal(serialized.includes("http://127.0.0.1:9"), false);
  assert.equal(serialized.includes("never-returned"), false);
  assert.equal((body.webhook as {configured: boolean}).configured, true);
});

test("mutations require admin, exact Origin and CSRF; rules stay machine-scoped", async (t) => {
  const h = await harness(t);
  const body = JSON.stringify({
    name: "Scoped rule",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 90,
    durationMs: 0,
  });

  const viewer = await fetch(`${h.base}/api/alerts/rules`, {
    method: "POST",
    headers: {
      cookie: h.viewer.cookie,
      origin: h.origin,
      "x-csrf-token": h.viewer.csrf,
      "content-type": "application/json",
    },
    body,
  });
  assert.equal(viewer.status, 403);

  const outOfScope = await fetch(`${h.base}/api/alerts/rules`, {
    method: "POST",
    headers: {
      cookie: h.adminScoped.cookie,
      origin: h.origin,
      "x-csrf-token": h.adminScoped.csrf,
      "content-type": "application/json",
    },
    body: JSON.stringify({...JSON.parse(body), machineId: "m2"}),
  });
  assert.equal(outOfScope.status, 403);

  const badOrigin = await fetch(`${h.base}/api/alerts/rules`, {
    method: "POST",
    headers: {
      cookie: h.admin.cookie,
      origin: "http://evil.example",
      "x-csrf-token": h.admin.csrf,
      "content-type": "application/json",
    },
    body,
  });
  assert.equal(badOrigin.status, 403);

  const missingCsrf = await fetch(`${h.base}/api/alerts/rules`, {
    method: "POST",
    headers: {
      cookie: h.admin.cookie,
      origin: h.origin,
      "content-type": "application/json",
    },
    body,
  });
  assert.equal(missingCsrf.status, 403);

  const unknownKey = await fetch(`${h.base}/api/alerts/rules`, {
    method: "POST",
    headers: {
      cookie: h.admin.cookie,
      origin: h.origin,
      "x-csrf-token": h.admin.csrf,
      "content-type": "application/json",
    },
    body: JSON.stringify({...JSON.parse(body), destinationUrl: "https://evil.example"}),
  });
  assert.equal(unknownKey.status, 400);

  const created = await fetch(`${h.base}/api/alerts/rules`, {
    method: "POST",
    headers: {
      cookie: h.admin.cookie,
      origin: h.origin,
      "x-csrf-token": h.admin.csrf,
      "content-type": "application/json",
    },
    body,
  });
  assert.equal(created.status, 201);
  const createdBody = await json(created);
  const rule = createdBody.rule as {id: string; machineId: string};
  assert.equal(rule.machineId, "m1");

  const deleted = await fetch(`${h.base}/api/alerts/rules?id=${encodeURIComponent(rule.id)}`, {
    method: "DELETE",
    headers: {
      cookie: h.admin.cookie,
      origin: h.origin,
      "x-csrf-token": h.admin.csrf,
    },
  });
  assert.equal(deleted.status, 200);
  const missing = await fetch(`${h.base}/api/alerts/rules?id=${encodeURIComponent(rule.id)}`, {
    method: "DELETE",
    headers: {
      cookie: h.admin.cookie,
      origin: h.origin,
      "x-csrf-token": h.admin.csrf,
    },
  });
  assert.equal(missing.status, 404);
});

test("webhook configuration never returns URL/secret and test is an explicit action", async (t) => {
  const h = await harness(t);
  const headers = {
    cookie: h.admin.cookie,
    origin: h.origin,
    "x-csrf-token": h.admin.csrf,
    "content-type": "application/json",
  };
  const configured = await fetch(`${h.base}/api/alerts/webhook`, {
    method: "POST",
    headers,
    body: JSON.stringify({action: "configure", url: "http://127.0.0.1:9/sink", secret: "hidden-secret"}),
  });
  assert.equal(configured.status, 200);
  const configuredBody = await json(configured);
  const serialized = JSON.stringify(configuredBody);
  assert.equal(serialized.includes("http://127.0.0.1:9"), false);
  assert.equal(serialized.includes("hidden-secret"), false);
  assert.equal((configuredBody.status as {configured: boolean}).configured, true);

  const bad = await fetch(`${h.base}/api/alerts/webhook`, {
    method: "POST",
    headers,
    body: JSON.stringify({action: "configure", url: "http://example.com/sink"}),
  });
  assert.equal(bad.status, 400);

  const cleared = await fetch(`${h.base}/api/alerts/webhook`, {
    method: "POST",
    headers,
    body: JSON.stringify({action: "clear"}),
  });
  assert.equal(cleared.status, 200);
  assert.equal((await json(cleared)).status instanceof Object, true);
});
