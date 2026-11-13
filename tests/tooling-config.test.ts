/**
 * Tooling configuration: build worker selection and the shared test runner.
 *
 * These tests spawn the configuration files in child processes so the checks
 * use the same environment handling as a real build or test run.
 */
import assert from "node:assert/strict";
import test from "node:test";
import { spawnSync } from "node:child_process";
import { join } from "node:path";

const root = join(__dirname, "..");
const nextConfig = join(root, "next.config.js");
const runner = join(root, "scripts", "test.cjs");
const runtimeRunner = join(root, "scripts", "test-runtime.cjs");

function cleanEnvironment(): NodeJS.ProcessEnv {
  const env = { ...process.env };
  for (const name of [
    "RIGSCOPE_BUILD_WORKERS",
    "RIGSCOPE_TEST_CONCURRENCY",
    "RIGSCOPE_RUNTIME_TEST_CONCURRENCY",
    "RIGSCOPE_TEST_GO_BINARY",
  ]) {
    delete env[name];
  }
  return env;
}

function run(script: string, args: string[], env: NodeJS.ProcessEnv = cleanEnvironment()) {
  return spawnSync(process.execPath, [script, ...args], {
    cwd: root,
    encoding: "utf8",
    env,
  });
}

function nextWorkers(value: string) {
  return spawnSync(
    process.execPath,
    [
      "-e",
      `const config = require(${JSON.stringify(nextConfig)})("phase-production-build"); console.log(config.experimental.cpus);`,
    ],
    { cwd: root, encoding: "utf8", env: { ...cleanEnvironment(), RIGSCOPE_BUILD_WORKERS: value } },
  );
}

test("RIGSCOPE_BUILD_WORKERS selects build workers and rejects invalid values", () => {
  const valid = nextWorkers("3");
  assert.equal(valid.status, 0, valid.stderr);
  assert.equal(valid.stdout.trim(), "3");

  for (const value of ["0", "65", "1.5", "many"]) {
    const invalid = nextWorkers(value);
    assert.notEqual(invalid.status, 0, `${value} must be rejected`);
    assert.match(invalid.stderr, /RIGSCOPE_BUILD_WORKERS must be an integer from 1 to 64/);
  }
});

test("source suite defaults to four workers and forwards the selected files", () => {
  const result = run(runner, ["--dry-run"]);
  assert.equal(result.status, 0, result.stderr);
  const plan = JSON.parse(result.stdout) as { suite: string; concurrency: number; command: string[] };
  assert.equal(plan.suite, "source");
  assert.equal(plan.concurrency, 4);
  assert.ok(plan.command.includes("--test-concurrency=4"));
  const files = plan.command.filter((argument) => argument.startsWith("tests/"));
  assert.ok(files.includes("tests/contracts.test.ts"));
  assert.ok(files.every((file) => file.endsWith(".test.ts") && !file.includes("/runtime/")));
});

test("forwarded test flags precede the selected files", () => {
  const result = run(runner, ["--dry-run", "--test-name-pattern=history"], {
    ...cleanEnvironment(),
    RIGSCOPE_TEST_CONCURRENCY: "2",
  });
  assert.equal(result.status, 0, result.stderr);
  const plan = JSON.parse(result.stdout) as { suite: string; concurrency: number; command: string[] };
  assert.equal(plan.suite, "source");
  assert.equal(plan.concurrency, 2);
  assert.ok(plan.command.includes("--test-concurrency=2"));
  const flag = plan.command.indexOf("--test-name-pattern=history");
  const file = plan.command.indexOf("tests/history-chart.test.ts");
  assert.ok(flag >= 0 && file >= 0 && flag < file, "flags must precede forwarded test files");
});

test("runtime harness requires an absolute executable Go binary", () => {
  const missing = run(runtimeRunner, [], cleanEnvironment());
  assert.notEqual(missing.status, 0);
  assert.match(missing.stderr, /RIGSCOPE_TEST_GO_BINARY=\/absolute\/path\/to\/rigscope is required/);

  const relative = run(runtimeRunner, [], { ...cleanEnvironment(), RIGSCOPE_TEST_GO_BINARY: "rigscope" });
  assert.notEqual(relative.status, 0);
  assert.match(relative.stderr, /RIGSCOPE_TEST_GO_BINARY must be an absolute path/);
});

test("invalid test concurrency settings fail with the environment name and range", () => {  const source = run(runner, ["--dry-run"], { ...cleanEnvironment(), RIGSCOPE_TEST_CONCURRENCY: "0" });
  assert.notEqual(source.status, 0);
  assert.match(source.stderr, /RIGSCOPE_TEST_CONCURRENCY must be an integer from 1 to 16/);

  const runtime = run(runtimeRunner, [], {
    ...cleanEnvironment(),
    RIGSCOPE_TEST_GO_BINARY: process.execPath,
    RIGSCOPE_RUNTIME_TEST_CONCURRENCY: "17",
  });
  assert.notEqual(runtime.status, 0);
  assert.match(runtime.stderr, /RIGSCOPE_RUNTIME_TEST_CONCURRENCY must be an integer from 1 to 16/);
});
