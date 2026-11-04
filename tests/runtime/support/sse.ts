import assert from "node:assert/strict";
import { registerCleanup } from "./cleanup";
import { SSE_EVENT_MS } from "./config";

export type SseEvent = { event: string; data: string; id?: string };

export class SseParser {
  private buffer = "";
  private eventName = "message";
  private dataLines: string[] = [];
  private pendingId: string | undefined;

  push(text: string, emit: (event: SseEvent) => void): void {
    this.buffer += text;
    for (;;) {
      const index = this.buffer.indexOf("\n");
      if (index < 0) break;
      let line = this.buffer.slice(0, index);
      this.buffer = this.buffer.slice(index + 1);
      if (line.endsWith("\r")) line = line.slice(0, -1);
      if (line.startsWith(":")) continue; // SSE comment / keep-alive
      if (line === "") {
        if (this.dataLines.length > 0) {
          emit({ event: this.eventName, data: this.dataLines.join("\n"), id: this.pendingId });
        }
        this.eventName = "message";
        this.dataLines = [];
        this.pendingId = undefined;
        continue;
      }
      const colon = line.indexOf(":");
      const field = colon === -1 ? line : line.slice(0, colon);
      let value = colon === -1 ? "" : line.slice(colon + 1);
      if (value.startsWith(" ")) value = value.slice(1);
      if (field === "event") this.eventName = value;
      else if (field === "data") this.dataLines.push(value);
      else if (field === "id") this.pendingId = value;
    }
  }
}

export class SseClient {
  private readonly controller = new AbortController();
  private readonly parser = new SseParser();
  private queue: SseEvent[] = [];
  private waiter: { resolve: (event: SseEvent) => void; reject: (err: Error) => void } | null = null;
  private finished = false;
  private failure: Error | null = null;
  private readonly finishedPromise: Promise<"done" | "aborted" | "failed">;
  status = 0;
  responseHeaders: Headers | null = null;
  lastEventId: string | undefined;

  constructor(
    private readonly url: string,
    requestHeaders: Record<string, string> = {},
  ) {
    registerCleanup(() => this.close());
    this.finishedPromise = (async () => {
      try {
        const response = await fetch(url, {
          headers: { accept: "text/event-stream", ...requestHeaders },
          signal: this.controller.signal,
        });
        this.status = response.status;
        this.responseHeaders = response.headers;
        if (!response.ok) throw new Error(`SSE ${url} responded ${response.status}`);
        if (response.body === null) throw new Error(`SSE ${url} has no body`);
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          this.ingest(decoder.decode(value, { stream: true }));
        }
        this.ingest(decoder.decode()); // flush
        this.finish(null);
        return "done" as const;
      } catch (err) {
        const failure = err as Error;
        if (failure.name === "AbortError") {
          this.finish(null);
          return "aborted" as const;
        }
        this.finish(failure);
        return "failed" as const;
      }
    })();
  }

  private ingest(text: string): void {
    this.parser.push(text, (event) => {
      if (event.id !== undefined) this.lastEventId = event.id;
      if (this.waiter !== null) {
        this.waiter.resolve(event);
        this.waiter = null;
      } else {
        this.queue.push(event);
      }
    });
  }

  private finish(failure: Error | null): void {
    this.finished = true;
    if (failure !== null && this.failure === null) this.failure = failure;
    if (this.waiter !== null) {
      this.waiter.reject(this.failure ?? new Error(`SSE stream ended before the awaited event (${this.url})`));
      this.waiter = null;
    }
  }

  async next(deadlineMs = SSE_EVENT_MS): Promise<SseEvent> {
    if (this.failure !== null) throw this.failure;
    if (this.queue.length > 0) return this.queue.shift() as SseEvent;
    if (this.finished) throw new Error(`SSE stream already closed (${this.url})`);
    return await new Promise<SseEvent>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.waiter = null;
        this.close(); // bounded deadline: abort the stream immediately
        reject(new Error(`no SSE event arrived within ${deadlineMs}ms from ${this.url}`));
      }, deadlineMs);
      this.waiter = {
        resolve: (event) => {
          clearTimeout(timer);
          resolve(event);
        },
        reject: (err) => {
          clearTimeout(timer);
          reject(err);
        },
      };
    });
  }

  assertResponseHeaders(): void {
    const headers = this.responseHeaders;
    assert.ok(headers, "SSE response headers captured");
    assert.ok(
      (headers.get("content-type") ?? "").toLowerCase().startsWith("text/event-stream"),
      `content-type must be text/event-stream (got ${headers.get("content-type")})`,
    );
    assert.equal(headers.get("x-accel-buffering"), "no", "X-Accel-Buffering: no is required");
    const cacheControl = headers.get("cache-control") ?? "";
    assert.match(cacheControl, /no-cache|no-store/i, `cache must be disabled (got Cache-Control: ${cacheControl})`);
  }

  close(): void {
    this.controller.abort();
  }

  async settled(): Promise<"done" | "aborted" | "failed"> {
    return await this.finishedPromise;
  }
}
