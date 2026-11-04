/**
 * Test client helpers for the hub's private Unix-socket listener. The outer
 * TLS terminator is replaced by direct HTTP over the socket, with an explicit
 * canonical Host header — exactly what Tailscale Serve forwards after it has
 * terminated TLS.
 */

import http from "node:http";
import { connect as netConnect } from "node:net";
import { lstatSync } from "node:fs";
import WebSocket from "ws";
import { registerCleanup } from "./cleanup";
import { HttpsSseStream, type OpenHttpsSse } from "./https";
import type { WsOutcome } from "./ws";

export type UnixResponse = {
  status: number;
  headers: http.IncomingHttpHeaders;
  body: string;
  json: unknown;
};

export type UnixRequestOptions = {
  method?: string;
  path?: string;
  headers?: Record<string, string>;
  body?: string;
  timeoutMs?: number;
};

export function unixRequest(
  socketPath: string,
  options: UnixRequestOptions = {},
): Promise<UnixResponse> {
  return new Promise((resolve, reject) => {
    const request = http.request(
      {
        socketPath,
        path: options.path ?? "/",
        method: options.method ?? "GET",
        headers: options.headers,
        timeout: options.timeoutMs ?? 5_000,
      },
      (response) => {
        const chunks: Buffer[] = [];
        response.on("data", (chunk: Buffer) => chunks.push(chunk));
        response.on("end", () => {
          const body = Buffer.concat(chunks).toString("utf8");
          let json: unknown = body;
          try {
            json = JSON.parse(body);
          } catch {
            /* text body */
          }
          resolve({
            status: response.statusCode ?? 0,
            headers: response.headers,
            body,
            json,
          });
        });
      },
    );
    request.on("timeout", () =>
      request.destroy(new Error(`unix ${options.path ?? "/"} timed out`)),
    );
    request.on("error", reject);
    if (options.body !== undefined) request.write(options.body);
    request.end();
  });
}

export async function openUnixSse(
  socketPath: string,
  path: string,
  headers: Record<string, string>,
  deadlineMs = 8_000,
): Promise<OpenHttpsSse> {
  const request = http.request(
    {
      socketPath,
      path,
      headers: { accept: "text/event-stream", ...headers },
      timeout: deadlineMs,
    },
    () => {
      /* resolved below */
    },
  );
  const response = await new Promise<http.IncomingMessage>((resolve, reject) => {
    request.once("response", (incoming) => {
      request.socket?.setTimeout(0);
      resolve(incoming);
    });
    request.on("timeout", () => request.destroy(new Error(`unix SSE ${path} timed out`)));
    request.on("error", reject);
    request.end();
  });
  response.setEncoding("utf8");
  const stream = new HttpsSseStream(request, response);
  registerCleanup(() => stream.destroy());
  return { stream, response };
}

/**
 * WebSocket upgrade over the Unix socket. The URL supplies the request path;
 * the explicit Host header is the configured canonical public host (never the
 * URL's placeholder host).
 */
export function wsUpgradeUnix(
  socketPath: string,
  path: string,
  hostHeader: string,
  headers: Record<string, string> = {},
  deadlineMs = 10_000,
): Promise<WsOutcome> {
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
      try {
        ws.terminate();
      } catch {
        /* never opened */
      }
      reject(new Error(`Unix WS upgrade ${path} neither opened nor was rejected within ${deadlineMs}ms`));
    }, deadlineMs);

    const ws = new WebSocket(`ws://unix.invalid${path}`, {
      createConnection: () => netConnect(socketPath),
      headers: { host: hostHeader, ...headers },
    });
    registerCleanup(() => {
      try {
        ws.terminate();
      } catch {
        /* already gone */
      }
    });
    ws.once("open", () => settle({ kind: "open", ws }));
    ws.once("unexpected-response", (_req: unknown, res: { statusCode?: number }) => {
      settle({ kind: "rejected", status: res.statusCode, message: `unexpected response ${res.statusCode}` });
    });
    ws.once("error", (error: Error) => {
      const statusMatch = error.message.match(/Unexpected server response: (\d+)/);
      settle({
        kind: "rejected",
        ...(statusMatch === null ? {} : { status: Number(statusMatch[1]) }),
        message: error.message,
      });
      ws.terminate();
    });
  });
}

/** Sends a raw HTTP/1.1 request and returns everything until the socket closes. */
export function rawUnixRequest(
  socketPath: string,
  requestText: string,
  deadlineMs = 5_000,
): Promise<string> {
  return new Promise((resolve, reject) => {
    const socket = netConnect(socketPath);
    const chunks: Buffer[] = [];
    const timer = setTimeout(() => {
      socket.destroy();
      reject(new Error(`raw Unix request timed out after ${deadlineMs}ms`));
    }, deadlineMs);
    socket.once("connect", () => socket.write(requestText));
    socket.on("data", (chunk: Buffer) => chunks.push(chunk));
    socket.once("error", (error) => {
      clearTimeout(timer);
      reject(error);
    });
    socket.once("close", () => {
      clearTimeout(timer);
      resolve(Buffer.concat(chunks).toString("utf8"));
    });
  });
}

export function unixPathStats(socketPath: string): { isSocket: boolean; mode: number } | null {
  try {
    const stat = lstatSync(socketPath);
    return { isSocket: stat.isSocket(), mode: stat.mode & 0o777 };
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return null;
    throw error;
  }
}
