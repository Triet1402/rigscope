import assert from "node:assert/strict";
import WebSocket from "ws";
import { registerCleanup } from "./cleanup";
import { WS_DEADLINE_MS } from "./config";
import { RawCollector, WelcomeShape, assertWelcome, helloOf } from "./wire";

export type WsOpen = { kind: "open"; ws: WebSocket };

export type WsRejected = { kind: "rejected"; status?: number; message: string };

export type WsOutcome = WsOpen | WsRejected;

export function isWsOpen(ws: WebSocket | undefined): boolean {
  return ws !== undefined && ws.readyState === WebSocket.OPEN;
}

export async function wsUpgrade(url: string, headers: Record<string, string> = {}, caPem?: string): Promise<WsOutcome> {
  return wsUpgradeWithDeadline(url, headers, caPem, WS_DEADLINE_MS);
}

export function wsUpgradeWithDeadline(url: string, headers: Record<string, string>, caPem: string | undefined, deadlineMs: number): Promise<WsOutcome> {
  return new Promise<WsOutcome>((resolve, reject) => {
    let settled = false;
    const settle = (outcome: WsOutcome): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve(outcome);
    };
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      const abandon = (): void => {
        try {
          ws.terminate(); // abandon half-open upgrade attempts on deadline
        } catch {
          /* socket never materialized */
        }
      };
      abandon();
      reject(new Error(`WS upgrade to ${url} neither opened nor was rejected within ${deadlineMs}ms`));
    }, deadlineMs);

    const ws = new WebSocket(url, {
      headers,
      ...(caPem === undefined ? {} : { ca: [caPem] }), // custom trust anchor; rejectUnauthorized stays default true
    });
    registerCleanup(() => wsTerminate(ws));
    ws.once("open", () => settle({ kind: "open", ws }));
    ws.once("unexpected-response", (_req: unknown, res: { statusCode?: number }) => {
      settle({ kind: "rejected", status: res.statusCode, message: `unexpected response ${res.statusCode}` });
    });
    ws.once("error", (err: Error) => {
      const statusMatch = err.message.match(/Unexpected server response: (\d+)/);
      if (statusMatch !== null) settle({ kind: "rejected", status: Number(statusMatch[1]), message: err.message });
      else settle({ kind: "rejected", message: err.message });
      ws.terminate();
    });
  });
}

export function nextWsJson(ws: WebSocket, deadlineMs: number): Promise<{ data: unknown } | null> {
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      ws.off("message", onMessage);
      resolve(null);
    }, deadlineMs);
    const onMessage = (data: WebSocket.RawData): void => {
      clearTimeout(timer);
      ws.off("message", onMessage);
      let parsed: unknown = null;
      try {
        parsed = JSON.parse(String(data));
      } catch {
        parsed = null;
      }
      resolve({ data: parsed });
    };
    ws.once("message", onMessage);
  });
}

export function expectClose(ws: WebSocket, what: string, deadlineMs = 10_000): Promise<number> {
  return new Promise((resolve, reject) => {
    if (ws.readyState === WebSocket.CLOSED) {
      resolve(0);
      return;
    }
    const timer = setTimeout(() => {
      ws.off("close", onClose);
      reject(new Error(`${what}: socket did not close within ${deadlineMs}ms`));
    }, deadlineMs);
    const onClose = (code: number): void => {
      clearTimeout(timer);
      resolve(Number(code));
    };
    ws.once("close", onClose);
  });
}

export function wsTerminate(ws: WebSocket | undefined): void {
  if (ws !== undefined && ws.readyState !== WebSocket.CLOSED) {
    try {
      ws.terminate();
    } catch {
      /* already gone */
    }
  }
}

export async function bindCollectorSession(
  ws: WebSocket,
  collector: RawCollector,
  what: string,
): Promise<{ welcome: WelcomeShape }> {
  // The protocol is hello-first. Install the listener before sending, then
  // proceed as soon as welcome arrives, without an optional-message delay.
  const replyPromise = nextWsJson(ws, WS_DEADLINE_MS);
  ws.send(JSON.stringify(helloOf(collector)));
  const reply = await replyPromise;
  assert.ok(reply !== null && reply.data !== null, `${what}: hub must acknowledge hello with welcome`);
  return { welcome: assertWelcome(reply.data, `${what} welcome`) };
}
