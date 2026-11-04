import assert from "node:assert/strict";
import test from "node:test";
import { parseHubSnapshot } from "../src/contracts";
import { LiveStream } from "../src/live/stream";

test("snapshot validation rejects malformed rows before they reach the UI", () => {
  assert.deepEqual(parseHubSnapshot({ hubRunId: "hub", machines: [] }), { hubRunId: "hub", machines: [] });
  const row = { machineId: "m", collectorId: "c", hostname: "h", connected: true, lastReceivedAt: null };
  assert.equal(parseHubSnapshot({ hubRunId: "hub", machines: [row] }).machines.length, 1);
  for (const machines of [
    [{}],
    [null],
    [row, row],
    [{ ...row, connected: "yes" }],
    [{ ...row, summary: {} }],
    [{ ...row, protocolVersion: 2 }],
  ]) {
    assert.throws(() => parseHubSnapshot({ hubRunId: "hub", machines }));
  }
});

test("snapshot validation accepts more than fifty machines", () => {
  const machines = Array.from({ length: 75 }, (_, index) => ({
    machineId: `m-${index}`,
    collectorId: `c-${index}`,
    hostname: `host-${index}`,
    connected: index % 2 === 0,
    lastReceivedAt: null,
  }));
  const snapshot = parseHubSnapshot({ hubRunId: "hub", machines });
  assert.equal(snapshot.machines.length, 75);
  assert.equal(snapshot.machines.at(-1)?.machineId, "m-74");
});

test("stream closes and reports malformed frames; superseded sources cannot update the UI", t => {
  class Source extends EventTarget {
    static instances: Source[] = [];
    readyState = 1;
    onerror: (() => void) | null = null;
    constructor() { super(); Source.instances.push(this); }
    close() { this.readyState = 2; }
    emit(value: unknown) {
      this.dispatchEvent(new MessageEvent("snapshot", { data: JSON.stringify(value), lastEventId: "hub:1" }));
    }
  }
  const original = Object.getOwnPropertyDescriptor(globalThis, "EventSource");
  Object.defineProperty(globalThis, "EventSource", { configurable: true, value: Source });
  t.after(() => original ? Object.defineProperty(globalThis, "EventSource", original) : Reflect.deleteProperty(globalThis, "EventSource"));
  const stream = new LiveStream();
  let delivered = 0;
  let failures = 0;
  const handlers = { onSnapshot: () => { delivered++; }, onError: () => { failures++; } };
  stream.start("/api/live", handlers);
  const first = Source.instances[0];
  first.emit({ hubRunId: "hub", machines: [] });
  assert.equal(delivered, 1);
  stream.start("/api/live", handlers);
  assert.equal(first.readyState, 2);
  first.emit({ hubRunId: "hub", machines: [] });
  assert.equal(delivered, 1);
  const second = Source.instances[1];
  second.emit({ hubRunId: "hub", machines: [{}] });
  assert.equal(failures, 1);
  assert.equal(stream.isOpen, false);
  second.emit({ hubRunId: "hub", machines: [] });
  assert.equal(delivered, 1);
});
