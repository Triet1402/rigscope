#!/usr/bin/env node
/**
 * Package the built checkout into an installable release archive.
 *
 * Run after `npm run build:ui` and `npm run build:runtime` (including the
 * operations bundle). Usage:
 *   node scripts/package-release.cjs [--source DIR] [--output ARCHIVE] [--replace]
 */
const { existsSync } = require("node:fs");
const { spawnSync } = require("node:child_process");
const { join } = require("node:path");

const root = join(__dirname, "..");
const ops = join(root, "dist", "ops.cjs");
if (!existsSync(ops)) {
  console.error("dist/ops.cjs is missing. Run the production/runtime build first (the ops entry is part of dist).");
  process.exit(1);
}
const args = process.argv.slice(2);
const rest = args[0] === "package" ? args.slice(1) : args;
const result = spawnSync(process.execPath, [ops, "package", ...rest], {
  cwd: root,
  stdio: "inherit",
  env: process.env,
});
if (result.error) {
  console.error(result.error.message);
  process.exit(1);
}
process.exit(result.status ?? 1);
