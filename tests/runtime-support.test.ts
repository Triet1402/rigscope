import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { SseClient, SseParser, type SseEvent } from "./runtime/support/sse";

test("SSE fixture parser handles fragmented frames and comments", () => {
  const parser = new SseParser();
  const events: SseEvent[] = [];
  for (const chunk of [": keepalive\n\nid: 1\neven", "t: update\ndata: one\nda", "ta: two\n\n"]) {
    parser.push(chunk, event => events.push(event));
  }
  assert.deepEqual(events, [{ id: "1", event: "update", data: "one\ntwo" }]);
});

test("SSE fixture delivers each event once when a consumer is already waiting", async t => {
  const server = createServer((_, response) => {
    response.writeHead(200, { "Content-Type": "text/event-stream" });
    response.write("id: 1\nevent: snapshot\ndata: {}\n\n");
    response.write("id: 2\nevent: update\ndata: {}\n\n");
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as { port: number }).port;
  const client = new SseClient(`http://127.0.0.1:${port}`);
  t.after(async () => {
    client.close();
    await client.settled();
    server.closeAllConnections();
    await new Promise<void>(resolve => server.close(() => resolve()));
  });
  assert.equal((await client.next()).id, "1");
  assert.equal((await client.next()).id, "2");
});
