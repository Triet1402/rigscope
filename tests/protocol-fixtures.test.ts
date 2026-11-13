/**
 * Golden wire fixtures shared by the Go protocol package and these parsers.
 *
 * Direction is encoded in the file-name prefix: `collector-*` messages go
 * collector -> hub and are parsed with parseCollectorMessage; `hub-*` messages
 * go hub -> collector and are parsed with parseHubMessage. `valid/` fixtures
 * must parse; `invalid/` fixtures must throw. `reencoded/` holds the same valid
 * fixtures after the Go decoder and encoder round trip them, which proves the
 * Go encoder produces JSON both parsers accept.
 */
import { strict as assert } from "node:assert";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it } from "node:test";

import { MAX_DETAIL_BYTES, MAX_PROCESS_ROWS, parseCollectorMessage, parseHubMessage } from "../src/contracts/telemetry";

const ROOT = join(import.meta.dirname, "..", "testdata", "protocol");

type Direction = "collector" | "hub";

function directionOf(name: string): Direction {
  if (name.startsWith("collector-")) return "collector";
  if (name.startsWith("hub-")) return "hub";
  throw new Error(`fixture ${name} has no collector-/hub- direction prefix`);
}

function parseFixture(name: string, value: unknown): { type: string } {
  return directionOf(name) === "collector" ? parseCollectorMessage(value) : parseHubMessage(value);
}

function fixtureNames(directory: string): string[] {
  return readdirSync(join(ROOT, directory))
    .filter((entry) => entry.endsWith(".json"))
    .map((entry) => entry.slice(0, -".json".length))
    .sort();
}

function readFixture(directory: string, name: string): unknown {
  return JSON.parse(readFileSync(join(ROOT, directory, `${name}.json`), "utf8"));
}

const validFixtures = fixtureNames("valid");
const invalidFixtures = fixtureNames("invalid");
// Byte-size limits are enforced by the Go decode entry point on the raw frame;
// the TypeScript parsers receive already-parsed JSON objects. This fixture is
// still a valid message for the object parsers, so it is exercised separately.
const TRANSPORT_ONLY_FIXTURES = new Set(["collector-hello-exceeds-message-bytes"]);

describe("protocol golden fixtures", () => {
  it("covers at least forty fixtures", () => {
    assert.ok(
      validFixtures.length + invalidFixtures.length >= 40,
      `expected at least 40 fixtures, found ${validFixtures.length + invalidFixtures.length}`,
    );
  });

  it("covers every message type with a valid fixture", () => {
    const seen = new Set<string>();
    for (const name of validFixtures) seen.add(parseFixture(name, readFixture("valid", name)).type);
    for (const type of [
      "hello",
      "samples",
      "inventory",
      "status",
      "detail.result",
      "detail.ended",
      "welcome",
      "ack",
      "detail.subscribe",
      "detail.cancel",
    ]) {
      assert.ok(seen.has(type), `no valid fixture covers message type ${type}`);
    }
  });

  for (const name of validFixtures) {
    it(`accepts valid/${name}.json`, () => {
      assert.equal(typeof parseFixture(name, readFixture("valid", name)).type, "string");
    });
  }

  for (const name of invalidFixtures) {
    if (TRANSPORT_ONLY_FIXTURES.has(name)) continue;
    it(`rejects invalid/${name}.json`, () => {
      assert.throws(() => parseFixture(name, readFixture("invalid", name)));
    });
  }

  it("keeps the oversized fixture valid for the object parsers", () => {
    const name = "collector-hello-exceeds-message-bytes";
    const raw = readFileSync(join(ROOT, "invalid", `${name}.json`), "utf8");
    assert.ok(raw.length > 262_144, "fixture must exceed the message byte limit");
    // Its only defect is size; the fields themselves parse.
    assert.equal(parseFixture(name, JSON.parse(raw)).type, "hello");
  });

  it("round-trips the protocol field present, absent and invalid", () => {
    const absent = parseCollectorMessage(readFixture("valid", "collector-hello-minimal"));
    assert.equal(Object.hasOwn(absent, "protocol"), false);

    const present = parseCollectorMessage(readFixture("valid", "collector-hello-protocol"));
    assert.equal((present as { protocol?: number }).protocol, 1);

    for (const name of [
      "collector-hello-protocol-zero",
      "collector-hello-protocol-negative",
      "collector-hello-protocol-string",
      "collector-hello-protocol-null",
      "collector-hello-protocol-float",
      "hub-welcome-protocol-zero",
      "hub-welcome-protocol-float",
      "hub-welcome-protocol-string",
    ]) {
      assert.throws(() => parseFixture(name, readFixture("invalid", name)), /protocol/);
    }
  });
});

describe("Go re-encoded fixtures", () => {
  it("has a re-encoded copy of every valid fixture and nothing else", () => {
    assert.deepEqual(fixtureNames("reencoded"), validFixtures);
  });

  for (const name of validFixtures) {
    it(`accepts reencoded/${name}.json`, () => {
      const original = parseFixture(name, readFixture("valid", name));
      const reencoded = parseFixture(name, readFixture("reencoded", name));
      assert.deepEqual(reencoded, original);
    });
  }
});

/**
 * Assembled messages are produced by the Go collector's metrics assembler (the
 * Go test writes them from the captured procfs fixture, the GPU fixtures, the
 * process fixture and synthetic mode). Every file must be accepted by the
 * TypeScript collector parser without hand-editing.
 */
describe("Go assembled collector messages", () => {
  const assembledDir = "assembled";
  const assembled = fixtureNames(assembledDir);

  it("covers metrics, inventory and process detail", () => {
    assert.ok(assembled.length >= 8, `expected the assembled fixtures, found ${assembled.length}`);
    const types = new Set<string>();
    for (const name of assembled) types.add(parseFixture(name, readFixture(assembledDir, name)).type);
    for (const type of ["samples", "inventory", "detail.result"]) {
      assert.ok(types.has(type), `no assembled fixture covers message type ${type}`);
    }
  });

  for (const name of assembled) {
    it(`accepts assembled/${name}.json`, () => {
      assert.ok(name.startsWith("collector-"), `assembled fixture ${name} must be a collector message`);
      const message = parseFixture(name, readFixture(assembledDir, name));
      assert.equal(typeof message.type, "string");
    });
  }

  it("accepts the oversized process table after the scheduler trims it", () => {
    const name = "collector-detail-result-assembled-trimmed";
    const raw = readFileSync(join(ROOT, assembledDir, `${name}.json`), "utf8");
    assert.ok(raw.length <= MAX_DETAIL_BYTES, `trimmed detail is ${raw.length} bytes, limit ${MAX_DETAIL_BYTES}`);
    const message = parseCollectorMessage(JSON.parse(raw)) as {
      type: string;
      snapshot: { truncated: boolean; processes: unknown[] };
    };
    assert.equal(message.type, "detail.result");
    assert.equal(message.snapshot.truncated, true);
    assert.ok(message.snapshot.processes.length > 0);
    assert.ok(message.snapshot.processes.length <= MAX_PROCESS_ROWS);
  });
});
