const { existsSync, readdirSync } = require("node:fs");
const { spawn } = require("node:child_process");
const { join } = require("node:path");

const root = join(__dirname, "..");
const rawConcurrency = process.env.RIGSCOPE_RUNTIME_TEST_CONCURRENCY;
const concurrency = rawConcurrency === undefined || rawConcurrency === "" ? 4 : Number(rawConcurrency);
if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 16) {
  console.error(
    `RIGSCOPE_RUNTIME_TEST_CONCURRENCY must be an integer from 1 to 16 (received ${JSON.stringify(rawConcurrency)}). ` +
      "Leave it unset for the default 4.",
  );
  process.exit(1);
}
if (!existsSync(join(root, ".next", "BUILD_ID"))) {
  console.error("Production UI build missing. Run npm run build:ui once before test:runtime.");
  process.exit(1);
}
const files = readdirSync(join(root, "tests", "runtime"))
  .filter(name => name.endsWith(".integration.ts"))
  .sort()
  .map(name => `tests/runtime/${name}`);
const child = spawn(process.execPath, [
  "--import", "tsx", "--test", `--test-concurrency=${concurrency}`,
  ...process.argv.slice(2), ...files,
], { cwd: root, stdio: "inherit" });
child.on("error", error => { console.error(error); process.exitCode = 1; });
child.on("exit", (code, signal) => {
  if (signal !== null) console.error(`runtime test runner: stopped by ${signal}`);
  process.exitCode = code ?? 1;
});
for (const signal of ["SIGINT", "SIGTERM"]) {
  process.on(signal, () => {
    if (child.exitCode === null && child.signalCode === null) child.kill(signal);
  });
}
