#!/usr/bin/env node
/**
 * Standalone entry for the optional system-scope installer.
 *
 * Use from a built checkout or installed release:
 *
 *   node scripts/system-install.cjs plan --source /path/to/release.tar.gz
 *   sudo node scripts/system-install.cjs install --source /path/to/release.tar.gz --apply
 *
 * Planned and unapplied commands never mutate the host. Mutations require an
 * explicit --apply, root and the built `dist/system.cjs`. This wrapper never
 * invokes a shell, never runs `systemctl --user`, and never reads credentials.
 */
const { existsSync } = require("node:fs");
const { spawnSync } = require("node:child_process");
const { join } = require("node:path");

const root = join(__dirname, "..");
const entry = join(root, "dist", "system.cjs");
if (!existsSync(entry)) {
  console.error("dist/system.cjs is missing. Run `npm run build:runtime` in this checkout first.");
  process.exit(1);
}
const args = process.argv.slice(2);
if (args.length === 0) args.push("help");
const result = spawnSync(process.execPath, [entry, ...args], {
  cwd: process.cwd(),
  stdio: "inherit",
  env: process.env,
});
if (result.error) {
  console.error(result.error.message);
  process.exit(1);
}
process.exit(result.status ?? 1);
