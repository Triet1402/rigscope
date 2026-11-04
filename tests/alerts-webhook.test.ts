/** Controlled local webhook sink and webhook configuration secrecy. */
import test from "node:test";
import assert from "node:assert/strict";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { createHmac } from "node:crypto";
import { once } from "node:events";
import { AddressInfo } from "node:net";

import { openHubDatabase } from "../src/hub/storage/db";
import { AlertEngine } from "../src/hub/alerts/engine";
import { parseWebhookUrl } from "../src/hub/alerts/webhook";
import { basePayload, metricsRecord } from "./alerts-fixtures";

type Received = {headers: IncomingMessage["headers"]; body: string};

async function readBody(req: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(Buffer.from(chunk));
  return Buffer.concat(chunks).toString("utf8");
}

async function sink(t: test.TestContext): Promise<{url: string; received: Received[]}> {
  const received: Received[] = [];
  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    void readBody(req).then((body) => {
      received.push({headers: req.headers, body});
      res.writeHead(204);
      res.end();
    });
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  t.after(() => new Promise<void>((resolve) => server.close(() => resolve())));
  const port = (server.address() as AddressInfo).port;
  return {url: `http://127.0.0.1:${port}/alerts-sink`, received};
}

test("real async delivery uses the deterministic idempotency key and signature", async (t) => {
  const target = await sink(t);
  const database = openHubDatabase(":memory:");
  t.after(() => database.close());
  let clock = 4_000_000;
  const engine = new AlertEngine({
    database,
    now: () => clock,
    deliveryTimeoutMs: 2_000,
  });
  const status = engine.configureWebhook({
    url: target.url,
    headers: {"x-controlled-sink": "yes"},
    secret: "local-test-secret",
  });
  assert.equal(status.configured, true);
  assert.equal(status.transport, "loopback-http");
  assert.equal(JSON.stringify(status).includes("127.0.0.1"), false);
  assert.equal(JSON.stringify(status).includes("local-test-secret"), false);

  engine.createRule({
    name: "Sink CPU",
    machineId: "m1",
    metric: "cpu.usage",
    condition: "above",
    threshold: 1,
    durationMs: 0,
  });
  const high = basePayload({cpu: {status: "ok", usagePercent: 99, perCore: [99, 99]}});
  engine.observe("m1", metricsRecord(high, {sequence: 1}), clock);
  await engine.processNotifications();

  assert.equal(target.received.length, 1);
  const delivered = target.received[0]!;
  const job = engine.notificationSummary().recent[0]!;
  assert.equal(delivered.headers["idempotency-key"], job.id);
  assert.equal(delivered.headers["x-controlled-sink"], "yes");
  assert.equal(delivered.headers["content-type"], "application/json");
  const signature = createHmac("sha256", "local-test-secret").update(delivered.body).digest("hex");
  assert.equal(delivered.headers["x-rigscope-signature"], `sha256=${signature}`);
  const payload = JSON.parse(delivered.body) as {event: string; jobId: string};
  assert.equal(payload.event, "fired");
  assert.equal(payload.jobId, job.id);

  const testResult = await engine.testWebhook();
  assert.equal(testResult.ok, true);
  assert.equal(target.received.length, 2);
  assert.equal(engine.getWebhookStatus().lastTestOk, true);
  assert.equal(JSON.stringify(engine.getWebhookStatus()).includes(target.url), false);
  engine.stop();
});

test("test delivery refuses non-loopback endpoints without sending", async () => {
  const database = openHubDatabase(":memory:");
  const engine = new AlertEngine({database, now: () => 5_000_000});
  try {
    engine.configureWebhook({url: "https://example.invalid/alerts"});
    const result = await engine.testWebhook();
    assert.equal(result.ok, false);
    assert.match(result.error ?? "", /loopback HTTP sink/);
    assert.equal(engine.getWebhookStatus().lastTestOk, false);
  } finally {
    engine.stop();
    database.close();
  }
});

test("webhook URL rules accept HTTPS and local HTTP only", () => {
  assert.equal(parseWebhookUrl("https://example.com/hook").transport, "https");
  assert.equal(parseWebhookUrl("http://127.0.0.1:9999/sink").transport, "loopback-http");
  assert.equal(parseWebhookUrl("http://localhost:9999/sink").transport, "loopback-http");
  assert.equal(parseWebhookUrl("http://[::1]:9999/sink").transport, "loopback-http");
  assert.throws(() => parseWebhookUrl("http://example.com/hook"), /loopback HTTP/);
  assert.throws(() => parseWebhookUrl("ftp://example.com/hook"), /HTTPS/);
  assert.throws(() => parseWebhookUrl("https://user:pass@example.com/hook"), /credentials/);
  assert.throws(() => parseWebhookUrl("https://example.com/hook#fragment"), /fragment/);
});
