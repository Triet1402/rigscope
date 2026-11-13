/**
 * Cross-language conformance for the browser ↔ hub API contract.
 *
 * (a) Every captured Node-hub fixture in testdata/api is validated with the
 *     generated strict validators.
 * (b) Every Go-encoded fixture in testdata/api/reencoded (written by
 *     `go generate ./internal/api`) is validated with the same validators and,
 *     for SSE snapshots, with the browser parser `parseHubSnapshot`.
 *
 * A fixture whose manifest entry is missing, whose validator rejects it, or
 * whose Go re-encoding disagrees with the browser parser fails this test.
 */

import { strict as assert } from "node:assert";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, it } from "node:test";

import { parseHubSnapshot } from "../src/contracts/index";
import {
  API_RESPONSE_VALIDATORS,
} from "../src/contracts/api.validators.generated";

const ROOT = join(import.meta.dirname, "..", "testdata", "api");
const REENCODED = join(ROOT, "reencoded");

type Shape = "json" | "sse-envelope" | "sse-raw";

type ManifestEntry = {
  path: string;
  status: number;
  headers: Record<string, string>;
  validator: string;
  shape: Shape;
};

type Validator = (value: unknown, what?: string) => unknown;

const validators = API_RESPONSE_VALIDATORS as unknown as Record<string, Validator>;

function readJSON<T>(path: string): T {
  return JSON.parse(readFileSync(path, "utf8")) as T;
}

function manifestAt(root: string): ManifestEntry[] {
  return readJSON<ManifestEntry[]>(join(root, "manifest.json"));
}

function validatorFor(entry: ManifestEntry): Validator {
  const validator = validators[entry.validator];
  assert.equal(typeof validator, "function", `fixture ${entry.path}: unknown validator ${entry.validator}`);
  return validator;
}

function validateSnapshot(value: unknown, what: string): void {
  const validator = validators.HubSnapshot;
  assert.equal(typeof validator, "function", "missing HubSnapshot validator");
  validator(value, what);
  // The browser parser must accept the same payload and preserve it.
  const parsed = parseHubSnapshot(value);
  assert.deepEqual(parsed, value, `${what}: parseHubSnapshot changed the payload`);
}

function validateJSONFixture(entry: ManifestEntry, root: string): void {
  const value = readJSON<unknown>(join(root, entry.path));
  validatorFor(entry)(value, entry.path);
}

function validateEnvelopeFixture(entry: ManifestEntry, root: string): void {
  const envelope = readJSON<{ id: string; event: string; data: unknown }>(join(root, entry.path));
  assert.equal(typeof envelope.event, "string", `${entry.path}: missing event`);
  assert.ok(envelope.id.length > 0, `${entry.path}: missing id`);
  validatorFor(entry)(envelope.data, `${entry.path}.data`);
  if (entry.validator === "HubSnapshot") {
    const parsed = parseHubSnapshot(envelope.data);
    assert.deepEqual(parsed, envelope.data, `${entry.path}: parseHubSnapshot changed the payload`);
  }
}

function validateRawFrame(entry: ManifestEntry, root: string): void {
  const text = readFileSync(join(root, entry.path), "utf8");
  const lines = text.split("\n");
  assert.ok(lines[0]?.startsWith("id: "), `${entry.path}: raw frame must start with an id line`);
  assert.ok(lines[1]?.startsWith("event: "), `${entry.path}: raw frame must carry an event line`);
  assert.ok(lines[2]?.startsWith("data: "), `${entry.path}: raw frame must carry a data line`);
  assert.equal(lines[3], "", `${entry.path}: raw frame must end with a blank line`);
  const data = JSON.parse(lines[2]!.slice("data: ".length)) as unknown;
  validatorFor(entry)(data, `${entry.path}.data`);
  validateSnapshot(data, `${entry.path}.data`);
}

function validateManifest(root: string, entries: ManifestEntry[], requireResponseMeta: boolean): void {
  assert.ok(entries.length > 0, `${root}: empty fixture manifest`);
  for (const entry of entries) {
    if (requireResponseMeta) {
      assert.equal(typeof entry.status, "number", `${entry.path}: missing expected status`);
      assert.equal(typeof entry.headers, "object", `${entry.path}: missing expected headers`);
    }
    switch (entry.shape) {
      case "json":
        validateJSONFixture(entry, root);
        break;
      case "sse-envelope":
        validateEnvelopeFixture(entry, root);
        break;
      case "sse-raw":
        validateRawFrame(entry, root);
        break;
      default:
        assert.fail(`${entry.path}: unknown shape ${String(entry.shape)}`);
    }
  }
}

function jsonFiles(root: string, skip: (relative: string) => boolean): string[] {
  const out: string[] = [];
  const visit = (directory: string, prefix: string): void => {
    for (const name of readdirSync(directory)) {
      const path = join(directory, name);
      const relative = prefix === "" ? name : `${prefix}/${name}`;
      if (skip(relative)) continue;
      if (statSync(path).isDirectory()) visit(path, relative);
      else if (relative.endsWith(".json")) out.push(relative);
    }
  };
  visit(root, "");
  return out.sort();
}

function sseFiles(root: string): string[] {
  const live = join(root, "live");
  return readdirSync(live)
    .filter((name) => name.endsWith(".sse"))
    .map((name) => `live/${name}`)
    .sort();
}

describe("captured Node hub fixtures", () => {
  const entries = manifestAt(ROOT);
  const byPath = new Map(entries.map((entry) => [entry.path, entry]));

  it("records every fixture and its expected status/headers", () => {
    const expected = [
      ...jsonFiles(ROOT, (relative) => relative.startsWith("reencoded/") || relative === "manifest.json").map((path) => path),
      ...sseFiles(ROOT),
    ];
    assert.deepEqual([...byPath.keys()].sort(), expected.sort());
    for (const entry of entries) {
      assert.ok(Object.keys(entry.headers).length > 0, `${entry.path}: no expected headers recorded`);
    }
  });

  it("validates every captured fixture", () => {
    validateManifest(ROOT, entries, true);
  });
});

describe("Go re-encoded fixtures", () => {
  const entries = manifestAt(REENCODED);

  it("covers every captured response fixture and the constructed variants", () => {
    const original = manifestAt(ROOT).filter((entry) => entry.shape === "json");
    const reencoded = entries.filter((entry) => entry.shape === "json");
    const originalPaths = new Set(original.map((entry) => entry.path));
    for (const entry of reencoded) {
      if (entry.path.startsWith("constructed/")) continue;
      assert.ok(originalPaths.has(entry.path), `re-encoded fixture ${entry.path} has no captured original`);
    }
    for (const entry of original) {
      assert.ok(entries.some((candidate) => candidate.path === entry.path),
        `captured fixture ${entry.path} has no Go re-encoding`);
    }
  });

  it("validates every Go-encoded fixture with the generated validators", () => {
    validateManifest(REENCODED, entries, false);
  });
});

describe("Go constructors cover the response variant matrix", () => {
  const entries = manifestAt(REENCODED);
  const paths = new Set(entries.map((entry) => entry.path));
  for (const path of [
    "constructed/health.json",
    "constructed/history-status-persistent.json",
    "constructed/history-query-unavailable.json",
    "constructed/alerts-list-incident.json",
    "constructed/alerts-webhook-test.json",
    "constructed/processes-read-with-processes.json",
    "constructed/errors-process-budget.json",
    "constructed/machines-meta-tags.json",
  ]) {
    it(`includes ${path}`, () => {
      assert.ok(paths.has(path), `missing ${path}`);
    });
  }
});
