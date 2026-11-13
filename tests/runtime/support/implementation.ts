/**
 * The Go executable the runtime suites exercise.
 *
 * There is no implementation selection any more: 1.0 has one implementation,
 * the static Go binary. `RIGSCOPE_TEST_GO_BINARY` must point at an absolute,
 * executable `rigscope` built from this tree. A missing or unusable binary is a
 * configuration error, never a reason to skip.
 */
import { statSync } from "node:fs";
import { isAbsolute } from "node:path";

export const GO_BINARY_ENV = "RIGSCOPE_TEST_GO_BINARY";

/**
 * Resolve and validate the Go binary path. Every launch calls this, so a
 * missing or non-executable binary fails at the first test that needs it with
 * an actionable message instead of timing out on a process that never started.
 */
export function goBinaryPath(): string {
  const raw = process.env[GO_BINARY_ENV]?.trim() ?? "";
  if (raw === "") {
    throw new Error(
      `${GO_BINARY_ENV}=/absolute/path/to/rigscope is required. ` +
        'Build it with "npm run build:ui && go generate ./internal/hub/webui && ' +
        'go build -o ./rigscope ./cmd/rigscope" and point the variable at it.',
    );
  }
  if (!isAbsolute(raw)) {
    throw new Error(
      `${GO_BINARY_ENV} must be an absolute path (received ${JSON.stringify(raw)}), ` +
        "because runtime tests spawn from the project root.",
    );
  }
  let stat;
  try {
    stat = statSync(raw);
  } catch {
    throw new Error(`${GO_BINARY_ENV} points at ${raw}, which does not exist. Build the Go binary first.`);
  }
  if (!stat.isFile()) {
    throw new Error(`${GO_BINARY_ENV} points at ${raw}, which is not a regular file.`);
  }
  if ((stat.mode & 0o111) === 0) {
    throw new Error(`${GO_BINARY_ENV} points at ${raw}, which is not executable (chmod +x it).`);
  }
  return raw;
}
