/**
 * Focused checks for the release packaging guards.
 *
 * These tests use throwaway archives and do not run a production build, touch
 * the network, publish anything, or inspect developer state.
 */
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";

const requireScript = createRequire(import.meta.url);
const { REQUIRED_DIRECTORIES, REQUIRED_FILES, verifyBuild } = requireScript("../scripts/verify-build.cjs") as {
  REQUIRED_DIRECTORIES: string[];
  REQUIRED_FILES: string[];
  verifyBuild: (root?: string) => { ok: boolean; errors: string[] };
};
const { parsePackOutput, verifyPackageArchive } = requireScript("../scripts/verify-package.cjs") as {
  parsePackOutput: (stdout: string) => { filename?: string } | undefined;
  verifyPackageArchive: (
    archive: string,
    options?: { root?: string; expectedPackage?: Record<string, unknown> },
  ) => { ok: boolean; errors: string[] };
};

const ROOT = mkdtempSync(join(tmpdir(), "rigscope-packaging-"));
process.on("exit", () => rmSync(ROOT, { recursive: true, force: true }));

function freshDir(name: string): string {
  const path = join(ROOT, `${name}-${Math.random().toString(36).slice(2)}`);
  mkdirSync(path, { recursive: true });
  return path;
}

function write(path: string, content: string): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, content);
}

const PACKAGE_JSON = {
  name: "rigscope",
  version: "0.1.0",
  bin: { rigscope: "bin/rigscope.cjs" },
  files: ["bin/", "dist/", ".next/"],
};

function buildSource(root: string): void {
  write(join(root, "package.json"), `${JSON.stringify(PACKAGE_JSON)}\n`);
  write(join(root, "bin", "rigscope.cjs"), "#!/usr/bin/env node\n");
  write(join(root, "next.config.js"), "module.exports = {};\n");
  for (const name of ["hub", "collector", "ops", "cli", "system"]) {
    write(join(root, "dist", `${name}.cjs`), `// ${name}\n`);
  }
  write(join(root, ".next", "BUILD_ID"), "fixture-build-id\n");
  write(join(root, ".next", "required-server-files.json"), '{"version":1}\n');
  write(join(root, ".next", "server", "app", "page.js"), "module.exports = {};\n");
  write(join(root, ".next", "static", "chunks", "app.js"), "// static\n");
}

function buildArchive(root: string, name: string, options: { omit?: string[]; extra?: Record<string, string> } = {}): string {
  const container = freshDir(`${name}-source`);
  const source = join(container, "package");
  buildSource(source);
  for (const relative of options.omit ?? []) rmSync(join(source, relative), { recursive: true, force: true });
  for (const [relative, content] of Object.entries(options.extra ?? {})) write(join(container, relative), content);
  const archive = join(root, `${name}.tgz`);
  const result = spawnSync("tar", ["-czf", archive, "-C", container, "package"], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  return archive;
}

test("verify-build accepts a complete production artifact tree", () => {
  const root = freshDir("build");
  buildSource(root);
  assert.deepEqual(verifyBuild(root), { ok: true, errors: [] });
  for (const required of [...REQUIRED_FILES, ...REQUIRED_DIRECTORIES]) {
    assert.equal(existsSync(join(root, required)), true, `${required} is required`);
  }
});

test("verify-build rejects missing, empty and malformed artifacts", () => {
  const missing = freshDir("missing-build");
  buildSource(missing);
  rmSync(join(missing, "dist", "cli.cjs"));
  const missingResult = verifyBuild(missing);
  assert.equal(missingResult.ok, false);
  assert.match(missingResult.errors.join("\n"), /dist\/cli\.cjs/);

  const empty = freshDir("empty-build");
  buildSource(empty);
  writeFileSync(join(empty, "dist", "hub.cjs"), "");
  writeFileSync(join(empty, ".next", "BUILD_ID"), "  \n");
  const emptyResult = verifyBuild(empty);
  assert.equal(emptyResult.ok, false);
  assert.match(emptyResult.errors.join("\n"), /dist\/hub\.cjs/);
  assert.match(emptyResult.errors.join("\n"), /BUILD_ID must not be empty/);

  const noServer = freshDir("no-server-build");
  buildSource(noServer);
  rmSync(join(noServer, ".next", "server"), { recursive: true, force: true });
  const noServerResult = verifyBuild(noServer);
  assert.equal(noServerResult.ok, false);
  assert.match(noServerResult.errors.join("\n"), /\.next\/server/);
});

test("verify-package accepts an archive with the built runtime and interface", () => {
  const root = freshDir("good-package");
  write(join(root, "package.json"), `${JSON.stringify(PACKAGE_JSON)}\n`);
  const archive = buildArchive(root, "good");
  const report = verifyPackageArchive(archive, { root });
  assert.deepEqual(report.errors, []);
  assert.equal(report.ok, true);
});

test("verify-package rejects archives missing build output", () => {
  const root = freshDir("incomplete-package");
  write(join(root, "package.json"), `${JSON.stringify(PACKAGE_JSON)}\n`);
  const archive = buildArchive(root, "incomplete", {
    omit: ["dist/ops.cjs", ".next/static", ".next/server", ".next/required-server-files.json"],
  });
  const report = verifyPackageArchive(archive, { root });
  assert.equal(report.ok, false);
  const errors = report.errors.join("\n");
  assert.match(errors, /dist\/ops\.cjs/);
  assert.match(errors, /\.next\/static\//);
  assert.match(errors, /\.next\/server\//);
  assert.match(errors, /required-server-files\.json/);
});

test('licensed release archives require security and upstream notices', () => {
  const root = freshDir('licensed-package');
  const archive = buildArchive(root, 'licensed', {extra: {
    'package/package.json': JSON.stringify({...PACKAGE_JSON, license: 'MIT'}),
    'package/LICENSE': 'MIT fixture',
  }});
  const report = verifyPackageArchive(archive);
  assert.equal(report.ok, false);
  for (const document of ['SECURITY.md', 'THIRD_PARTY_NOTICES.md']) {
    assert.ok(report.errors.some(error => error.includes(document)), document);
  }
});

test("verify-package rejects private, source and transient files", () => {
  const root = freshDir("private-package");
  write(join(root, "package.json"), `${JSON.stringify(PACKAGE_JSON)}\n`);
  const archive = buildArchive(root, "private", {
    extra: {
      "package/.env": "SECRET=1\n",
      "package/src/private.ts": "// source\n",
      "package/node_modules/next/package.json": "{}\n",
      "package/.next/cache/private.txt": "cache\n",
      "package/old-release.tgz": "nested\n",
    },
  });
  const report = verifyPackageArchive(archive, { root });
  assert.equal(report.ok, false);
  const errors = report.errors.join("\n");
  assert.match(errors, /package\/\.env/);
  assert.match(errors, /package\/src\//);
  assert.match(errors, /package\/node_modules\//);
  assert.match(errors, /package\/\.next\/cache\//);
  assert.match(errors, /\.tgz/);
});

test("verify-package checks packaged name and version against the checkout", () => {
  const root = freshDir("metadata-package");
  write(join(root, "package.json"), `${JSON.stringify(PACKAGE_JSON)}\n`);
  const archive = buildArchive(root, "metadata", {
    extra: {
      "package/package.json": `${JSON.stringify({ ...PACKAGE_JSON, version: "9.9.9" })}\n`,
    },
  });
  const report = verifyPackageArchive(archive, { root });
  assert.equal(report.ok, false);
  assert.match(report.errors.join("\n"), /version mismatch/);
});

test("parsePackOutput accepts both npm array and keyed-object JSON", () => {
  assert.deepEqual(parsePackOutput('[{"filename":"rigscope-0.1.0.tgz"}]'), {
    filename: "rigscope-0.1.0.tgz",
  });
  assert.deepEqual(parsePackOutput('{"rigscope":{"filename":"keyed.tgz"}}'), {
    filename: "keyed.tgz",
  });
  assert.throws(() => parsePackOutput("not json"), /Could not parse npm pack output/);
});

test("package wiring builds and verifies before packing but keeps the installer path script-free", () => {
  const root = join(import.meta.dirname, "..");
  const manifest = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
  assert.equal(manifest.scripts.prepack, "node scripts/prepack.cjs");
  assert.equal(manifest.scripts["package:npm"], "npm pack");
  assert.equal(manifest.scripts["verify:package"], "node scripts/verify-package.cjs");
  assert.equal(manifest.scripts["verify:installed"], "node scripts/verify-installed-package.cjs");
  assert.equal(manifest.scripts["build:ui"], "next build --webpack");
  assert.ok(manifest.files.includes("dist/"));
  assert.ok(manifest.files.includes(".next/BUILD_ID"));
  assert.match(manifest.dependencies.next, /^\^16\./);
  assert.equal(manifest.overrides, undefined, "npm overrides only apply to the root project");
  assert.ok(
    !JSON.parse(readFileSync(join(root, "package-lock.json"), "utf8")).packages["node_modules/next"]
      .dependencies.postcss.startsWith("8.4."),
    "the locked Next.js release must itself declare a patched postcss",
  );

  const services = readFileSync(join(root, "src", "cli", "services.ts"), "utf8");
  assert.match(
    services,
    /\['pack', root, '--pack-destination', temporary, '--ignore-scripts', '--json'\]/,
    "the installer must keep using npm pack --ignore-scripts on the installed package",
  );
});
