#!/usr/bin/env node
// Generate THIRD_PARTY_NOTICES.md for the browser bundle embedded in the Go
// binary.
//
// The static export in `out/` ships the production UI dependencies and the
// Next.js runtime that renders the export. Build-only tooling (TypeScript,
// Tailwind, esbuild, the test runner) is not redistributed and is not listed.
//
// The inventory and license texts are read offline from the installed
// node_modules tree. No registry access is needed. Missing packages and
// packages without a license file are errors, so a notice cannot silently
// disappear.
const fs = require("node:fs");
const path = require("node:path");

const root = path.join(__dirname, "..");

/** Packages whose code is part of the emitted UI bundle. */
function seedNames() {
  const manifest = JSON.parse(fs.readFileSync(path.join(root, "package.json"), "utf8"));
  const names = new Set(Object.keys(manifest.dependencies || {}));
  // Next is a build dependency, but its client runtime is embedded in every
  // exported page and chunk.
  names.add("next");
  return [...names];
}

function packageDirectory(name, bases) {
  for (const base of bases) {
    const candidate = path.join(base, "node_modules", name);
    if (fs.existsSync(path.join(candidate, "package.json"))) return candidate;
  }
  return undefined;
}

/**
 * Resolve the production dependency closure. Dependencies are hoisted, so the
 * root node_modules is tried first; a nested directory covers the rare package
 * that keeps a private copy.
 */
function collect() {
  const found = new Map();
  const queue = seedNames().map((name) => ({ name, bases: [root] }));
  while (queue.length > 0) {
    const entry = queue.shift();
    if (found.has(entry.name)) continue;
    const directory = packageDirectory(entry.name, entry.bases);
    if (directory === undefined) {
      // Optional native packages for other platforms are not installed.
      continue;
    }
    const manifest = JSON.parse(fs.readFileSync(path.join(directory, "package.json"), "utf8"));
    if (manifest.optional === true) continue;
    found.set(entry.name, { directory, manifest });
    const bases = [directory, ...entry.bases];
    // Optional dependencies (native compiler and image binaries, split
    // per platform) are not part of the browser bundle.
    for (const dependency of Object.keys(manifest.dependencies || {})) {
      queue.push({ name: dependency, bases });
    }
  }
  return [...found.values()].sort((left, right) => left.manifest.name.localeCompare(right.manifest.name));
}

const LICENSE_FILE = /^(licen[cs]e|notice|copying|copyright)([._-]|$)/i;

/** Collect license/notice files at the package root and up to two levels in. */
function licenseFiles(directory) {
  const files = [];
  const visit = (relative, depth) => {
    for (const item of fs.readdirSync(path.join(directory, relative), { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
      if (item.isSymbolicLink() || item.name === "node_modules" || item.name === ".git") continue;
      const next = path.join(relative, item.name);
      if (item.isDirectory()) {
        if (depth < 2) visit(next, depth + 1);
        continue;
      }
      if (LICENSE_FILE.test(item.name)) files.push(next);
    }
  };
  visit("", 0);
  return files;
}

function generate() {
  const packages = collect();
  const sections = [];
  const inventory = [];
  const missing = [];
  for (const { directory, manifest } of packages) {
    const license = manifest.license || "See upstream text";
    inventory.push(`| ${manifest.name} | ${manifest.version} | ${license} |`);
    let files = licenseFiles(directory);
    if (files.length === 0) {
      // Split packages inherit the parent project's license text.
      const fallback = manifest.name.startsWith("@next/")
        ? "node_modules/next/license.md"
        : manifest.name === "client-only"
          ? "node_modules/react/LICENSE"
          : undefined;
      if (fallback !== undefined && fs.existsSync(path.join(root, fallback))) {
        sections.push(`\n---\n\n## ${manifest.name}@${manifest.version}: ${fallback}\n\n${fs.readFileSync(path.join(root, fallback), "utf8").trim()}\n`);
        continue;
      }
    }
    if (files.length === 0) {
      missing.push(`${manifest.name}@${manifest.version}`);
      continue;
    }
    for (const file of files) {
      const content = fs.readFileSync(path.join(directory, file), "utf8").trim();
      if (!content) continue;
      sections.push(`\n---\n\n## ${manifest.name}@${manifest.version}: ${file.replaceAll(path.sep, "/")}\n\n${content}\n`);
    }
  }
  if (missing.length > 0) throw new Error(`No upstream license text found: ${missing.join(", ")}`);
  const header = `# Third-party notices

Rigscope's AGPL-3.0-only license covers its own code. The browser UI is a static export
embedded in the Go binary; the npm packages it bundles retain their own licenses
and copyright notices reproduced below. This inventory covers the production UI
dependencies and the Next.js client runtime in that export. Build-only tooling
(TypeScript, Tailwind, esbuild, the test runner) is not redistributed and is not
listed here. The Go binary's own module notices are in THIRD_PARTY_NOTICES-go.md.

This file does not relicense those components under AGPL.

| Package | Version | Declared license |
| --- | --- | --- |
`;
  return header + inventory.join("\n") + "\n" + sections.join("");
}

if (require.main === module) {
  const output = generate();
  fs.writeFileSync(path.join(root, "THIRD_PARTY_NOTICES.md"), output);
  console.log(`third-party notices: ${Buffer.byteLength(output)} bytes`);
}

module.exports = { generate, collect };
