import assert from "node:assert/strict";
import { connect as netConnect, createServer as netCreateServer } from "node:net";
import { setTimeout as delay } from "node:timers/promises";
import { assertExactKeys } from "./wire";

export async function fetchJson(url: string, deadlineMs = 5_000): Promise<{ status: number; body: unknown }> {
  const response = await fetch(url, { signal: AbortSignal.timeout(deadlineMs) });
  const text = await response.text();
  let body: unknown = text;
  try {
    body = JSON.parse(text);
  } catch {
    /* non-JSON body kept as text */
  }
  return { status: response.status, body };
}

export type HubHealth = { status: string; mode: string; hubRunId: string; version: string };

export function assertHealthShape(body: unknown, what: string): HubHealth {
  assertExactKeys(body, ["hubRunId", "mode", "status", "version"], what);
  const record = body as HubHealth;
  assert.equal(record.status, "ok", `${what}.status`);
  assert.equal(record.mode, "runtime-spike", `${what}.mode`);
  assert.match(record.version, /^\d+\.\d+\.\d+/, `${what}.version`);
  assert.equal(typeof record.hubRunId, "string", `${what}.hubRunId must be a string`);
  assert.ok(record.hubRunId.length > 0, `${what}.hubRunId non-empty`);
  return record;
}

export async function expectHealth(hub: { healthUrl: string }): Promise<HubHealth> {
  const { status, body } = await fetchJson(hub.healthUrl);
  assert.equal(status, 200, "GET /api/health must return 200");
  return assertHealthShape(body, "health body");
}

export async function expectPortClosed(port: number, deadlineMs = 3_000): Promise<void> {
  const end = Date.now() + deadlineMs;
  for (;;) {
    const error = await new Promise<Error | null>((resolve) => {
      const socket = netConnect({ host: "127.0.0.1", port });
      socket.once("connect", () => {
        socket.destroy();
        resolve(null);
      });
      socket.once("error", (err) => resolve(err));
    });
    if (error !== null) return;
    if (Date.now() > end) assert.fail(`port ${port} is still accepting connections after shutdown`);
    await delay(100);
  }
}

export async function closedLoopbackPort(): Promise<number> {
  const server = netCreateServer();
  await new Promise<void>((resolve) => server.listen({ host: "127.0.0.1", port: 0 }, () => resolve()));
  const port = (server.address() as { port: number }).port;
  await new Promise<void>((resolve) => server.close(() => resolve()));
  return port;
}
