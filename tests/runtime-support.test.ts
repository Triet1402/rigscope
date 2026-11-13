import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { WebSocket, WebSocketServer, type RawData } from "ws";
import { SseClient, SseParser, type SseEvent } from "./runtime/support/sse";
import { startRelay } from "./runtime/support/relay";
import { baseEnv } from "./runtime/support/env";

test("baseEnv drops ambient runtime overrides and host state", t => {
  const touched: Record<string, string | undefined> = {};
  const set = (key: string, value: string) => {
    touched[key] = process.env[key];
    process.env[key] = value;
    t.after(() => {
      if (touched[key] === undefined) delete process.env[key];
      else process.env[key] = touched[key];
    });
  };
  set("RIGSCOPE_HUB_DATABASE_PATH", "/somewhere");
  set("RIGSCOPE_HUB_PORT", "9999");
  set("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus");
  set("SSL_CERT_FILE", "/host/ca.pem");
  set("HTTPS_PROXY", "http://proxy.invalid:3128");
  set("XDG_RUNTIME_DIR", "/run/user/1000");

  const env = baseEnv({ HOME: "/fixture/home", TMPDIR: "/fixture/tmp" });
  assert.equal(env.RIGSCOPE_HUB_DATABASE_PATH, undefined);
  assert.equal(env.RIGSCOPE_HUB_PORT, undefined);
  assert.equal(env.DBUS_SESSION_BUS_ADDRESS, undefined);
  assert.equal(env.SSL_CERT_FILE, undefined);
  assert.equal(env.HTTPS_PROXY, undefined);
  assert.equal(env.XDG_RUNTIME_DIR, undefined);
  assert.equal(env.HOME, "/fixture/home");
  assert.equal(env.TMPDIR, "/fixture/tmp");
  for (const entry of (env.PATH ?? "").split(":")) {
    assert.ok(entry.startsWith("/"), `PATH entry ${entry} is not absolute`);
  }
});

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

function rawBytes(data: RawData): Buffer {
  return Array.isArray(data) ? Buffer.concat(data) : Buffer.isBuffer(data) ? data : Buffer.from(data as ArrayBuffer);
}

test("relay keeps handshake headers, payload bytes and text/binary frames in both directions", async t => {
  const wss = new WebSocketServer({ noServer: true, perMessageDeflate: false });
  const server = createServer();
  const upstreamHeaders: Record<string, string | string[] | undefined> = {};
  const upstreamMessages: { bytes: Buffer; isBinary: boolean }[] = [];
  let upstream: WebSocket | null = null;

  server.on("upgrade", (request, socket, head) => {
    Object.assign(upstreamHeaders, request.headers);
    // Delay the upstream handshake so collector frames arrive while the relay's
    // upstream socket is still CONNECTING and must be buffered.
    setTimeout(() => {
      wss.handleUpgrade(request, socket, head, (ws) => {
        upstream = ws;
        ws.on("message", (data, isBinary) => upstreamMessages.push({ bytes: rawBytes(data), isBinary }));
      });
    }, 200);
  });
  await new Promise<void>((resolve) => server.listen({ host: "127.0.0.1", port: 0 }, () => resolve()));
  const upstreamPort = (server.address() as { port: number }).port;
  const relay = await startRelay({ upstreamUrl: `ws://127.0.0.1:${upstreamPort}/api/collector/connect` });

  const clientMessages: { bytes: Buffer; isBinary: boolean }[] = [];
  const client = new WebSocket(relay.url, {
    headers: {
      Origin: "http://browser.example",
      Authorization: "Bearer relay-secret",
      "X-Relay-Test": "kept",
    },
    perMessageDeflate: false,
  });
  t.after(async () => {
    client.terminate();
    upstream?.terminate();
    await relay.close();
    await new Promise<void>((resolve) => {
      server.closeAllConnections();
      server.close(() => resolve());
    });
    await new Promise<void>((resolve) => wss.close(() => resolve()));
  });
  client.on("message", (data, isBinary) => clientMessages.push({ bytes: rawBytes(data), isBinary }));
  await new Promise<void>((resolve, reject) => {
    client.once("open", () => resolve());
    client.once("error", reject);
  });

  // Sent while the relay's upstream socket is still handshaking: both frames
  // take the buffered path.
  const textBytes = Buffer.from('{"type":"hello","collectorId":"c","note":"h\u00e9llo"}', "utf8");
  const binaryBytes = Buffer.from([0x00, 0xff, 0xfe, 0x80]);
  client.send(textBytes, { binary: false });
  client.send(binaryBytes, { binary: true });

  const deadline = Date.now() + 5_000;
  while (upstreamMessages.length < 2 && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  assert.equal(upstreamMessages.length, 2, "both buffered frames reached the upstream");
  assert.equal(upstreamMessages[0]!.isBinary, false, "the buffered text frame stays text");
  assert.deepEqual(upstreamMessages[0]!.bytes, textBytes, "the buffered text payload is byte-identical");
  assert.equal(upstreamMessages[1]!.isBinary, true, "the buffered binary frame stays binary");
  assert.deepEqual(upstreamMessages[1]!.bytes, binaryBytes, "the buffered binary payload is byte-identical");

  assert.equal(upstreamHeaders.origin, "http://browser.example", "Origin reaches the upstream unchanged");
  assert.equal(upstreamHeaders.authorization, "Bearer relay-secret", "Authorization reaches the upstream unchanged");
  assert.equal(upstreamHeaders["x-relay-test"], "kept", "custom handshake headers reach the upstream unchanged");
  // The relay is a forward proxy: Host names the origin server, not the relay.
  assert.equal(upstreamHeaders.host, `127.0.0.1:${upstreamPort}`, "the upstream client generates the hub's Host header");

  // Upstream -> collector, text and binary, byte-identical.
  const hubText = Buffer.from('{"type":"welcome","hubRunId":"r","note":"h\u00e9llo"}', "utf8");
  const hubBinary = Buffer.from([0xde, 0xad, 0x00, 0xbe, 0xef]);
  upstream!.send(hubText, { binary: false });
  upstream!.send(hubBinary, { binary: true });
  const hubDeadline = Date.now() + 5_000;
  while (clientMessages.length < 2 && Date.now() < hubDeadline) {
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  assert.equal(clientMessages.length, 2);
  assert.equal(clientMessages[0]!.isBinary, false);
  assert.deepEqual(clientMessages[0]!.bytes, hubText);
  assert.equal(clientMessages[1]!.isBinary, true);
  assert.deepEqual(clientMessages[1]!.bytes, hubBinary);

  const recordedText = relay.frames().find((frame) => frame.direction === "collector" && !frame.isBinary);
  assert.ok(recordedText, "the relay recorded the text frame");
  assert.deepEqual(recordedText!.bytes, textBytes, "the frame log keeps the exact text bytes");
  const recordedBinary = relay.frames().find((frame) => frame.direction === "collector" && frame.isBinary);
  assert.ok(recordedBinary, "the relay recorded the binary frame");
  assert.deepEqual(recordedBinary!.bytes, binaryBytes, "the frame log keeps the exact binary bytes");

  // Held hub->collector acks keep their bytes and text flag until released.
  relay.holdAcks();
  const heldAck = Buffer.from('{"type":"ack","accepted":[],"rejected":[]}', "utf8");
  upstream!.send(heldAck, { binary: false });
  await relay.waitForFrame(
    (frame) => frame.direction === "hub" && frame.delivery === "held",
    5_000,
    "the relay holding the ack",
  );
  assert.equal(clientMessages.length, 2, "a held ack is not delivered early");
  relay.releaseAcks();
  const releasedDeadline = Date.now() + 5_000;
  while (clientMessages.length < 3 && Date.now() < releasedDeadline) {
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  assert.equal(clientMessages.length, 3, "the held ack is delivered on release");
  assert.equal(clientMessages[2]!.isBinary, false);
  assert.deepEqual(clientMessages[2]!.bytes, heldAck, "the held ack payload is byte-identical after release");

  // The detail-lease test hides renewals/cancellation by type; the relay must
  // record them as dropped and never forward them.
  relay.dropHubTypes(["detail.subscribe"]);
  upstream!.send(JSON.stringify({
    type: "detail.subscribe",
    subscriptionId: "s",
    kind: "processes",
    intervalMs: 1_000,
    leaseMs: 20_000,
  }));
  await relay.waitForFrame(
    (frame) => frame.direction === "hub" && frame.delivery === "dropped" && frame.data !== null &&
      (frame.data as { type?: unknown }).type === "detail.subscribe",
    5_000,
    "the relay suppressing a detail.subscribe",
  );
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(clientMessages.length, 3, "a suppressed frame is never forwarded");
  assert.equal(relay.suppressedHubFrames(), 1);
});
