#!/usr/bin/env node
/**
 * Shared test runner. Node's test runner gives each file its own process.
 *
 *   node scripts/test.cjs                  # tests/*.test.ts, four files at once
 *
 * Extra arguments are forwarded to `node --test`:
 *   node scripts/test.cjs --test-name-pattern=session
 *
 * RIGSCOPE_TEST_CONCURRENCY accepts 1..16.
 */
const { readdirSync } = require("node:fs");
const { spawn } = require("node:child_process");
const { join } = require("node:path");

const root = join(__dirname, "..");
const suite = {
  directory: "tests",
  extension: ".test.ts",
  concurrencyEnv: "RIGSCOPE_TEST_CONCURRENCY",
  defaultConcurrency: 4,
};

const USAGE = `Usage: node scripts/test.cjs [--dry-run] [node --test flags]

  source       tests/*.test.ts (RIGSCOPE_TEST_CONCURRENCY, default 4)
`;

function fail(message) {
  console.error(`test runner: ${message}`);
  process.exitCode = 1;
}

function parseArguments(argv) {
  let dryRun = false;
  const forwarded = [];
  for (const argument of argv) {
    if (argument === "--help" || argument === "-h") {
      console.log(USAGE);
      process.exit(0);
    }
    if (argument === "--dry-run") {
      dryRun = true;
      continue;
    }
    forwarded.push(argument);
  }
  return { dryRun, forwarded };
}

function testFiles() {
  const files = readdirSync(join(root, suite.directory), { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(suite.extension))
    .map((entry) => `${suite.directory}/${entry.name}`)
    .sort();
  if (files.length === 0) {
    throw new Error(`no test files matched ${suite.directory}/*${suite.extension}`);
  }
  return files;
}

function concurrency() {
  const raw = process.env[suite.concurrencyEnv];
  const value = raw === undefined || raw === "" ? suite.defaultConcurrency : Number(raw);
  if (!Number.isInteger(value) || value < 1 || value > 16) {
    throw new Error(
      `${suite.concurrencyEnv} must be an integer from 1 to 16 (received ${JSON.stringify(raw)}). ` +
        `Leave it unset for the default ${suite.defaultConcurrency}.`,
    );
  }
  return value;
}

function main() {
  const { dryRun, forwarded } = parseArguments(process.argv.slice(2));
  const workers = concurrency();
  const files = testFiles();
  const args = [
    "--import", "tsx",
    "--test",
    `--test-concurrency=${workers}`,
    ...forwarded,
    ...files,
  ];
  if (dryRun) {
    console.log(JSON.stringify({ suite: "source", concurrency: workers, command: [process.execPath, ...args] }));
    return;
  }
  const child = spawn(process.execPath, args, { cwd: root, stdio: "inherit", env: process.env });
  child.on("error", (error) => fail(error.message));
  child.on("exit", (code, signal) => {
    if (signal !== null) console.error(`test runner: stopped by ${signal}`);
    process.exitCode = code ?? 1;
  });
  for (const signal of ["SIGINT", "SIGTERM"]) {
    process.on(signal, () => {
      if (child.exitCode === null && child.signalCode === null) child.kill(signal);
    });
  }
}

try {
  main();
} catch (error) {
  fail(error instanceof Error ? error.message : String(error));
}
