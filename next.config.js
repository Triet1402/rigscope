const { PHASE_DEVELOPMENT_SERVER } = require("next/constants");
const { availableParallelism } = require("node:os");

const workers = Number(process.env.RIGSCOPE_BUILD_WORKERS ?? Math.min(4, availableParallelism()));
if (!Number.isInteger(workers) || workers < 1 || workers > 64) {
  throw new Error(
    `RIGSCOPE_BUILD_WORKERS must be an integer from 1 to 64 (received ${JSON.stringify(process.env.RIGSCOPE_BUILD_WORKERS)}). ` +
      "Set it to the number of build workers, or leave it unset to auto-select up to four.",
  );
}

/** @param {string} phase @returns {import("next").NextConfig} */
const nextConfig = (phase) => ({
  // Keep dev writes separate from the packaged hub's production artifact.
  // Use a small static-generation worker pool for this six-page scaffold.
  distDir: phase === PHASE_DEVELOPMENT_SERVER ? ".next-dev" : ".next",
  experimental: { cpus: workers },
  // Remote development goes through an outer TLS terminator and a Unix
  // socket, so the browser's Origin hostname is the configured public origin,
  // not localhost. Keep an explicit hostname allowlist for /_next assets and
  // HMR (Next also adds the createNextApp hostname). Configuring this list
  // makes Next's dev guard block unlisted origins instead of only warning.
  // The hub validates the full exact origin before delegation.
  allowedDevOrigins: phase === PHASE_DEVELOPMENT_SERVER ? publicDevOrigin() : undefined,
});

function publicDevOrigin() {
  const value = process.env.HUB_PUBLIC_ORIGIN;
  if (value === undefined || value === "") return undefined;
  try {
    const origin = new URL(value);
    return origin.protocol === "https:" ? [origin.hostname] : undefined;
  } catch {
    return undefined;
  }
}

module.exports = nextConfig;
