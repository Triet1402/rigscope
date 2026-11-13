const { readdirSync, statSync } = require("node:fs");
const { spawn } = require("node:child_process");
const { isAbsolute, join } = require("node:path");

const root = join(__dirname, "..");

// The runtime suites exercise exactly one implementation: the static Go
// binary. There is no Node fallback and no implementation selector.
const binary = (process.env.RIGSCOPE_TEST_GO_BINARY ?? "").trim();
const requirement =
  "RIGSCOPE_TEST_GO_BINARY=/absolute/path/to/rigscope is required. " +
  'Build the UI and the binary with "npm run build:ui && go generate ./internal/hub/webui && ' +
  'go build -o ./rigscope ./cmd/rigscope" and point the variable at it.';
if (binary === "") {
  console.error(requirement);
  process.exit(1);
}
if (!isAbsolute(binary)) {
  console.error(`RIGSCOPE_TEST_GO_BINARY must be an absolute path (received ${JSON.stringify(binary)}). ${requirement}`);
  process.exit(1);
}
let stat;
try {
  stat = statSync(binary);
} catch {
  console.error(`RIGSCOPE_TEST_GO_BINARY points at ${binary}, which does not exist. ${requirement}`);
  process.exit(1);
}
if (!stat.isFile() || (stat.mode & 0o111) === 0) {
  console.error(`RIGSCOPE_TEST_GO_BINARY points at ${binary}, which is not an executable regular file. ${requirement}`);
  process.exit(1);
}

const rawConcurrency = process.env.RIGSCOPE_RUNTIME_TEST_CONCURRENCY;
const concurrency = rawConcurrency === undefined || rawConcurrency === "" ? 4 : Number(rawConcurrency);
if (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 16) {
  console.error(
    `RIGSCOPE_RUNTIME_TEST_CONCURRENCY must be an integer from 1 to 16 (received ${JSON.stringify(rawConcurrency)}). ` +
      "Leave it unset for the default 4.",
  );
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
