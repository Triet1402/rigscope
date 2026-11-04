const { build } = require("esbuild");

build({
  entryPoints: {
    hub: "src/hub/main.ts",
    collector: "collector/main.ts",
    ops: "src/operations/main.ts",
    cli: "src/cli/main.ts",
    system: "src/cli/system.ts",
  },
  bundle: true,
  platform: "node",
  target: "node26",
  format: "cjs",
  packages: "external",
  outdir: "dist",
  outExtension: { ".js": ".cjs" },
  logLevel: "info",
}).catch(() => { process.exitCode = 1; });
