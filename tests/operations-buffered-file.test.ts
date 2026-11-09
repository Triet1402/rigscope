/**
 * Buffered private-file writes (readiness heartbeats): complete atomic
 * replacement without fsync, owner-only permissions, symlink refusal and
 * temporary-file cleanup. Everything lives in a fresh private temporary
 * directory; no live service state is touched.
 */
import assert from "node:assert/strict";
import test from "node:test";
import {
  chmodSync,
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  readlinkSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Worker } from "node:worker_threads";

import {
  OperationsError,
  formatMode,
  writePrivateFileBuffered,
} from "../src/operations/fs";

const ROOT = mkdtempSync(join(tmpdir(), "rigscope-buffered-file-"));
process.on("exit", () => rmSync(ROOT, { recursive: true, force: true }));

function freshDir(name: string): string {
  const path = join(ROOT, `${name}-${Math.random().toString(36).slice(2)}`);
  mkdirSync(path, { recursive: true, mode: 0o700 });
  return path;
}

function isOperationsError(code: string): (error: unknown) => boolean {
  return (error) => error instanceof OperationsError && error.code === code;
}

test("replaces a heartbeat completely with owner-only permissions", () => {
  const directory = freshDir("replace");
  const file = join(directory, "collector-health.json");
  writeFileSync(file, `{"sequence":0,"padding":"${"x".repeat(4096)}"}\n`, { mode: 0o600 });
  const previousInode = lstatSync(file).ino;

  writePrivateFileBuffered(file, '{"sequence":1}\n');

  assert.equal(readFileSync(file, "utf8"), '{"sequence":1}\n');
  assert.equal(formatMode(lstatSync(file).mode), "0600");
  assert.notEqual(lstatSync(file).ino, previousInode, "content should be published from a fresh inode");
  assert.deepEqual(readdirSync(directory), ["collector-health.json"], "temporary file should be gone");
});

test("creates a new heartbeat file with owner-only permissions", () => {
  const directory = freshDir("create");
  const file = join(directory, "collector-health.json");

  writePrivateFileBuffered(file, "ready\n");

  assert.equal(readFileSync(file, "utf8"), "ready\n");
  assert.equal(formatMode(lstatSync(file).mode), "0600");
  assert.deepEqual(readdirSync(directory), ["collector-health.json"]);
});

test("readers only observe complete old or new content during replacement", async () => {
  const directory = freshDir("atomic");
  const file = join(directory, "collector-health.json");
  const first = JSON.stringify({ sequence: 0, payload: "a".repeat(64 * 1024) });
  const second = JSON.stringify({ sequence: 1, payload: "b".repeat(64 * 1024) });
  writePrivateFileBuffered(file, first);

  const stop = new SharedArrayBuffer(4);
  const reader = new Worker(`
    const { parentPort, workerData } = require("node:worker_threads");
    const { readFileSync } = require("node:fs");
    const stop = new Int32Array(workerData.stop);
    let reads = 0;
    const read = () => {
      const content = readFileSync(workerData.file, "utf8");
      if (content !== workerData.first && content !== workerData.second) {
        throw new Error("reader observed partial heartbeat");
      }
      reads++;
    };
    read();
    parentPort.postMessage("ready");
    while (!Atomics.load(stop, 0)) read();
    parentPort.postMessage(reads);
  `, { eval: true, workerData: { file, first, second, stop } });
  const finished = new Promise<number>((resolve, reject) => {
    reader.on("message", (value) => { if (typeof value === "number") resolve(value); });
    reader.on("error", reject);
    reader.on("exit", (code) => { if (code !== 0) reject(new Error(`reader exited ${code}`)); });
  });
  try {
    await new Promise<void>((resolve, reject) => {
      reader.once("message", () => resolve());
      reader.once("error", reject);
    });
    for (let iteration = 0; iteration < 40; iteration += 1) {
      writePrivateFileBuffered(file, iteration % 2 === 0 ? second : first);
    }
    writePrivateFileBuffered(file, second);
  } finally {
    Atomics.store(new Int32Array(stop), 0, 1);
  }
  assert.ok(await finished > 1, "reader must run concurrently with replacements");
  assert.equal(readFileSync(file, "utf8"), second);
});

test("refuses a destination symlink and leaves its target untouched", () => {
  const directory = freshDir("symlink");
  const target = join(directory, "target.json");
  const link = join(directory, "collector-health.json");
  writeFileSync(target, "sentinel\n", { mode: 0o600 });
  symlinkSync(target, link);

  assert.throws(
    () => writePrivateFileBuffered(link, "heartbeat\n"),
    isOperationsError("unsafe-path"),
  );
  assert.equal(readFileSync(target, "utf8"), "sentinel\n");
  assert.equal(readlinkSync(link), target);
  assert.deepEqual(readdirSync(directory).sort(), ["collector-health.json", "target.json"]);
});

test("refuses a dangling destination symlink", () => {
  const directory = freshDir("dangling");
  const link = join(directory, "collector-health.json");
  symlinkSync(join(directory, "missing.json"), link);

  assert.throws(
    () => writePrivateFileBuffered(link, "heartbeat\n"),
    isOperationsError("unsafe-path"),
  );
  assert.equal(existsSync(join(directory, "missing.json")), false);
  assert.deepEqual(readdirSync(directory), ["collector-health.json"]);
});

test("refuses a non-regular destination path", () => {
  const directory = freshDir("directory");
  const destination = join(directory, "collector-health.json");
  mkdirSync(destination, { mode: 0o700 });

  assert.throws(
    () => writePrivateFileBuffered(destination, "heartbeat\n"),
    isOperationsError("unsafe-path"),
  );
  assert.deepEqual(readdirSync(directory), ["collector-health.json"]);
});

test("refuses an existing destination with insecure permissions", () => {
  const directory = freshDir("insecure");
  const file = join(directory, "collector-health.json");
  writeFileSync(file, "previous\n", { mode: 0o600 });
  chmodSync(file, 0o644);

  assert.throws(
    () => writePrivateFileBuffered(file, "heartbeat\n"),
    isOperationsError("insecure-permissions"),
  );
  assert.equal(readFileSync(file, "utf8"), "previous\n");
  assert.deepEqual(readdirSync(directory), ["collector-health.json"]);
});

test("removes its temporary file when the write fails", () => {
  const directory = freshDir("cleanup");
  const file = join(directory, "collector-health.json");

  assert.throws(
    () => writePrivateFileBuffered(file, {} as unknown as string),
    TypeError,
  );
  assert.equal(existsSync(file), false);
  assert.deepEqual(readdirSync(directory), [], "no temporary file should survive the failure");
});
