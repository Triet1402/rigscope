#!/usr/bin/env node
/**
 * Shared test runner. Node's test runner gives each file its own process.
 *
 *   node scripts/test.cjs                       # tests/*.test.ts, four files at once
 *   node scripts/test.cjs --suite development   # tests/development/*.integration.ts, one at a time
 *
 * Extra arguments are forwarded to `node --test`:
 *   node scripts/test.cjs --test-name-pattern=session
 *
 * RIGSCOPE_TEST_CONCURRENCY and RIGSCOPE_DEV_TEST_CONCURRENCY accept 1..16.
 */
const { readdirSync } = require("node:fs");
const { spawn } = require("node:child_process");
const { join } = require("node:path");

const root = join(__dirname, "..");
const suites = {
  source: {
    directory: "tests",
    extension: ".test.ts",
    concurrencyEnv: "RIGSCOPE_TEST_CONCURRENCY",
    defaultConcurrency: 4,
  },
  development: {
    directory: "tests/development",
    extension: ".integration.ts",
    concurrencyEnv: "RIGSCOPE_DEV_TEST_CONCURRENCY",
    defaultConcurrency: 1,
  },
};

const USAGE = `Usage: node scripts/test.cjs [--suite source|development] [--dry-run] [node --test flags]

  source       tests/*.test.ts (RIGSCOPE_TEST_CONCURRENCY, default 4)
  development  tests/development/*.integration.ts (RIGSCOPE_DEV_TEST_CONCURRENCY, default 1)
`;

function fail(message) {
  console.error(`test runner: ${message}`);
  process.exitCode = 1;
}

function parseArguments(argv) {
  let suiteName = "source";
  let dryRun = false;
  const forwarded = [];
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "--help" || argument === "-h") {
      console.log(USAGE);
      process.exit(0);
    }
    if (argument === "--suite") {
      const value = argv[index + 1];
      if (value === undefined || value.startsWith("-")) {
        throw new Error("--suite requires source or development");
      }
      suiteName = value;
      index += 1;
      continue;
    }
    if (argument.startsWith("--suite=")) {
      suiteName = argument.slice("--suite=".length);
      continue;
    }
    if (argument === "--dry-run") {
      dryRun = true;
      continue;
    }
    forwarded.push(argument);
  }
  if (!Object.hasOwn(suites, suiteName)) {
    throw new Error(`unknown test suite ${JSON.stringify(suiteName)}; use --suite source or --suite development`);
  }
  return { suite: suites[suiteName], suiteName, dryRun, forwarded };
}

function testFiles(suite) {
  const files = readdirSync(join(root, suite.directory), { withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(suite.extension))
    .map((entry) => `${suite.directory}/${entry.name}`)
    .sort();
  if (files.length === 0) {
    throw new Error(`no test files matched ${suite.directory}/*${suite.extension}`);
  }
  return files;
}

function concurrencyFor(suite) {
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
  const { suite, suiteName, dryRun, forwarded } = parseArguments(process.argv.slice(2));
  const concurrency = concurrencyFor(suite);
  const files = testFiles(suite);
  const args = [
    "--import", "tsx",
    "--test",
    `--test-concurrency=${concurrency}`,
    ...forwarded,
    ...files,
  ];
  if (dryRun) {
    console.log(JSON.stringify({ suite: suiteName, concurrency, command: [process.execPath, ...args] }));
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
