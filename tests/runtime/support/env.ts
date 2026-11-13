/**
 * Environment construction shared by the launchers and the fixtures.
 *
 * A test run must not inherit the invoking shell's state: a developer's
 * RIGSCOPE_* override, DBUS_*, SYSTEMD_*, XDG_* or proxy/CA variables could
 * silently change a fixture's meaning or reach the product process. Only the
 * values a child needs to run at all are passed through; launchers and fixtures
 * inject HOME, XDG_*, TMPDIR and the RIGSCOPE_* test seams explicitly.
 */

/** Values inherited from the test process. PATH is sanitized below. */
const PASSTHROUGH = ["PATH", "LANG", "LC_ALL", "TZ"] as const;

/**
 * Keep only absolute, unique PATH entries. A relative entry must never resolve
 * a tool from the fixture directory before the real one.
 */
function sanitizedPath(path: string): string {
  const seen = new Set<string>();
  return path
    .split(":")
    .filter((entry) => {
      if (!entry.startsWith("/") || seen.has(entry)) return false;
      seen.add(entry);
      return true;
    })
    .join(":");
}

export function baseEnv(overrides: Record<string, string | undefined>): NodeJS.ProcessEnv {
  const env: Record<string, string | undefined> = {};
  for (const key of PASSTHROUGH) {
    const value = process.env[key];
    if (value === undefined) continue;
    env[key] = key === "PATH" ? sanitizedPath(value) : value;
  }
  for (const [key, value] of Object.entries(overrides)) {
    if (value !== undefined) env[key] = value;
    else delete env[key];
  }
  return env as NodeJS.ProcessEnv;
}
