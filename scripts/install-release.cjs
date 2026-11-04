#!/usr/bin/env node
/**
 * Install a packaged release into an isolated prefix, or activate/roll back an
 * already staged release. Never invokes systemctl or starts a service.
 *
 * Usage:
 *   node scripts/install-release.cjs --source ARCHIVE_OR_DIR --prefix DIR \
 *     --state-dir DIR --units-dir DIR [--activate --confirm-stopped]
 *   node scripts/install-release.cjs activate --prefix DIR --state-dir DIR --version V [--confirm-stopped]
 *   node scripts/install-release.cjs rollback --prefix DIR --state-dir DIR --confirm-stopped
 *
 * The first positional argument may be `activate` or `rollback`; otherwise
 * `install` is used.
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
const first = args[0];
if (first === "help" || first === "--help" || first === "-h") {
  const help = spawnSync(process.execPath, [ops, "help"], { stdio: "inherit", env: process.env });
  process.exit(help.status ?? 1);
}
const command = first === "activate" || first === "rollback" || first === "install" ? first : "install";
const rest = ["activate", "rollback", "install"].includes(first) ? args.slice(1) : args;
const result = spawnSync(process.execPath, [ops, command, ...rest], {
  cwd: root,
  stdio: "inherit",
  env: process.env,
});
if (result.error) {
  console.error(result.error.message);
  process.exit(1);
}
process.exit(result.status ?? 1);
