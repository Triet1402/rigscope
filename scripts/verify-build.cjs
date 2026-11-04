#!/usr/bin/env node
/**
 * Verify that the production artifacts required by the npm package exist and
 * are not empty. `prepack` runs this after building so a package can never ship
 * a missing or truncated `dist/` or `.next/` directory.
 *
 * Usage:
 *   node scripts/verify-build.cjs [ROOT]
 */
const { existsSync, readdirSync, readFileSync, statSync } = require("node:fs");
const { join, resolve } = require("node:path");

const REQUIRED_FILES = [
  "bin/rigscope.cjs",
  "dist/hub.cjs",
  "dist/collector.cjs",
  "dist/ops.cjs",
  "dist/cli.cjs",
  "dist/system.cjs",
  ".next/BUILD_ID",
];
const REQUIRED_DIRECTORIES = [".next/server", ".next/static"];

function nonEmptyDirectory(path) {
  try {
    return readdirSync(path).length > 0;
  } catch {
    return false;
  }
}

/**
 * @param {string} [root]
 * @returns {{ ok: boolean, errors: string[] }}
 */
function verifyBuild(root = process.cwd()) {
  const base = resolve(root);
  const errors = [];

  for (const relative of REQUIRED_FILES) {
    const path = join(base, relative);
    let stat;
    try {
      stat = statSync(path);
    } catch {
      errors.push(`required build artifact is missing: ${relative}`);
      continue;
    }
    if (!stat.isFile()) {
      errors.push(`required build artifact is not a file: ${relative}`);
    } else if (stat.size === 0) {
      errors.push(`required build artifact is empty: ${relative}`);
    }
  }

  for (const relative of REQUIRED_DIRECTORIES) {
    const path = join(base, relative);
    let stat;
    try {
      stat = statSync(path);
    } catch {
      errors.push(`required build directory is missing: ${relative}`);
      continue;
    }
    if (!stat.isDirectory()) {
      errors.push(`required build directory is not a directory: ${relative}`);
    } else if (!nonEmptyDirectory(path)) {
      errors.push(`required build directory is empty: ${relative}`);
    }
  }

  const buildIdPath = join(base, ".next", "BUILD_ID");
  if (existsSync(buildIdPath)) {
    const buildId = readFileSync(buildIdPath, "utf8").trim();
    if (!buildId) errors.push(".next/BUILD_ID must not be empty");
    else if (/\s/.test(buildId)) errors.push(".next/BUILD_ID must not contain whitespace");
  }

  return { ok: errors.length === 0, errors };
}

if (require.main === module) {
  const result = verifyBuild(process.argv[2] || process.cwd());
  if (!result.ok) {
    for (const error of result.errors) console.error(`verify-build: ${error}`);
    process.exit(1);
  }
  console.log("verify-build: production runtime and interface artifacts are present.");
}

module.exports = { REQUIRED_DIRECTORIES, REQUIRED_FILES, verifyBuild };
