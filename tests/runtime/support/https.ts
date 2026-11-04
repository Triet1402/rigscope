import http from "node:http";
import https from "node:https";
import { SseEvent, SseParser } from "./sse";

export function httpsJson(port: number, path: string, caPem: string, deadlineMs = 5_000): Promise<{ status: number; body: unknown }> {
  return new Promise((resolve, reject) => {
    const request = https.request({ host: "127.0.0.1", port, path, ca: [caPem], timeout: deadlineMs }, (response) => {
      const chunks: Buffer[] = [];
      response.on("data", (chunk: Buffer) => chunks.push(chunk));
      response.on("end", () => {
        const text = Buffer.concat(chunks).toString("utf8");
        let body: unknown = text;
        try {
          body = JSON.parse(text);
        } catch {
          /* text */
        }
        resolve({ status: response.statusCode ?? 0, body });
      });
    });
    request.on("timeout", () => request.destroy(new Error(`https ${path} timed out`)));
    request.on("error", reject);
    request.end();
  });
}

export class HttpsSseStream {
  private readonly parser = new SseParser();
  private queue: SseEvent[] = [];
  private waiter: { resolve: (event: SseEvent | null) => void } | null = null;
  private closed = false;

  constructor(
    private readonly request: http.ClientRequest,
    response: http.IncomingMessage,
  ) {
    response.setEncoding("utf8");
    response.on("data", (chunk: string) => {
      this.parser.push(chunk, (event) => {
        if (this.waiter !== null) {
          this.waiter.resolve(event);
          this.waiter = null;
        } else {
          this.queue.push(event);
        }
      });
    });
    response.on("end", () => {
      this.closed = true;
      this.waiter?.resolve(null);
      this.waiter = null;
    });
    response.on("error", () => {
      this.closed = true;
      this.waiter?.resolve(null);
      this.waiter = null;
    });
  }

  async next(deadlineMs: number): Promise<SseEvent | null> {
    if (this.queue.length > 0) return this.queue.shift() as SseEvent;
    if (this.closed) return null;
    return await new Promise<SseEvent | null>((resolve) => {
      const timer = setTimeout(() => {
        this.waiter = null;
        resolve(null); // deadline: the caller decides what missing data means
      }, deadlineMs);
      this.waiter = {
        resolve: (event) => {
          clearTimeout(timer);
          resolve(event);
        },
      };
    });
  }

  destroy(): void {
    this.closed = true;
    this.waiter?.resolve(null);
    this.waiter = null;
    try {
      this.request.destroy(new Error("proxied SSE client closed"));
    } catch {
      /* already destroyed */
    }
  }
}

export type OpenHttpsSse = { stream: HttpsSseStream; response: http.IncomingMessage };

export async function openHttpsSse(
  port: number,
  path: string,
  caPem: string,
  deadlineMs = 8_000,
  headers: Record<string, string> = {},
): Promise<OpenHttpsSse> {
  const request = https.request(
    { host: "127.0.0.1", port, path, ca: [caPem], timeout: deadlineMs, headers: { accept: "text/event-stream", ...headers } },
    (response) => {
      /* resolved below */
    },
  );
  const response = await new Promise<http.IncomingMessage>((resolve, reject) => {
    request.once("response", (incoming) => {
      // SSE is idle-tolerant: disable the socket timeout once headers arrive so
      // slow keep-alive intervals cannot tear down the stream mid-read.
      request.socket?.setTimeout(0);
      resolve(incoming);
    });
    request.on("timeout", () => request.destroy(new Error(`proxied SSE ${path} timed out`)));
    request.on("error", reject);
    request.end();
  });
  response.setEncoding("utf8");
  return { stream: new HttpsSseStream(request, response), response };
}
