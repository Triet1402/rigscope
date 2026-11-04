#!/usr/bin/env node
/**
 * npm `prepack` hook: build the runtime and interface once, then verify the
 * artifacts that the package will contain. Running `npm pack` or `npm publish`
 * therefore cannot ship stale or missing `dist/` and `.next/` output.
 *
 * The installer intentionally calls `npm pack --ignore-scripts` on an already
 * built package, which skips this hook and preserves that offline path.
 */
const { spawnSync } = require("node:child_process");
const { join } = require("node:path");
const { verifyBuild } = require("./verify-build.cjs");

const root = join(__dirname, "..");
const npm = process.platform === "win32" ? "npm.cmd" : "npm";
const notices = spawnSync(process.execPath, [join(root, 'scripts/third-party-notices.cjs')], {
  cwd: root, stdio: ['ignore', 'pipe', 'pipe'],
});
if (notices.status !== 0) {
  process.stderr.write(notices.stderr || 'Could not prepare third-party notices.\n');
  process.exit(1);
}
const build = spawnSync(npm, ["run", "build"], {
  cwd: root,
  env: process.env,
  stdio: ["ignore", "pipe", "pipe"],
});
// Keep build logs off stdout so `npm pack --json` still emits parseable JSON.
if (build.stdout) process.stderr.write(build.stdout);
if (build.stderr) process.stderr.write(build.stderr);
if (build.error) {
  console.error(`prepack: could not run the production build: ${build.error.message}`);
  process.exit(1);
}
if (build.status !== 0) {
  console.error(`prepack: production build failed with status ${build.status}.`);
  process.exit(build.status ?? 1);
}

const result = verifyBuild(root);
if (!result.ok) {
  for (const error of result.errors) console.error(`prepack: ${error}`);
  process.exit(1);
}
console.error("prepack: fresh production artifacts verified.");
