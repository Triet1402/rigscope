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
  // Production is a static export: the Go hub embeds `out/` and serves it
  // itself, so Next is build-time only. `next dev` is only for UI-only
  // iteration against no hub (its /api requests have nothing behind them).
  ...(phase === PHASE_DEVELOPMENT_SERVER ? {} : { output: "export" }),
  // Keep dev writes separate from the production export the hub embeds.
  distDir: phase === PHASE_DEVELOPMENT_SERVER ? ".next-dev" : ".next",
  // A small static-generation worker pool for this page set.
  experimental: { cpus: workers },
});

module.exports = nextConfig;
