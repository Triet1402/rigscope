#!/usr/bin/env node
/**
 * Verify the contents of the npm package.
 *
 * With an archive argument it inspects that archive. Without one it runs
 * `npm pack` (which triggers `prepack` and therefore a fresh production build)
 * into a temporary directory and inspects the result.
 *
 * Usage:
 *   node scripts/verify-package.cjs [ARCHIVE] [--root ROOT]
 */
const { existsSync, mkdtempSync, readFileSync, rmSync } = require("node:fs");
const { tmpdir } = require("node:os");
const { join, resolve } = require("node:path");
const { spawnSync } = require("node:child_process");

const REQUIRED_FILES = [
  "package/package.json",
  "package/bin/rigscope.cjs",
  "package/next.config.js",
  "package/dist/hub.cjs",
  "package/dist/collector.cjs",
  "package/dist/ops.cjs",
  "package/dist/cli.cjs",
  "package/dist/system.cjs",
  "package/.next/BUILD_ID",
  "package/.next/required-server-files.json",
];
const REQUIRED_DIRECTORIES = ["package/.next/server", "package/.next/static"];
const FORBIDDEN_FILES = [
  "package/.env",
  "package/.npmrc",
  "package/.next/trace",
  "package/.next/trace-build",
];
const FORBIDDEN_PREFIXES = [
  "package/node_modules/",
  "package/src/",
  "package/tests/",
  "package/scripts/",
  "package/.git/",
  "package/.next/cache/",
  "package/.next/dev/",
];

function normalizeEntry(entry) {
  return entry.replace(/^\.\//, "").replace(/\/+$/, "");
}

function archiveEntries(archive) {
  const result = spawnSync("tar", ["-tzf", archive], {
    encoding: "utf8",
    env: { ...process.env, LC_ALL: "C" },
  });
  if (result.error) throw new Error(`Could not read ${archive}: ${result.error.message}`);
  if (result.status !== 0) {
    throw new Error(`Could not list ${archive}: ${(result.stderr || "").trim()}`);
  }
  return result.stdout.split("\n").filter(Boolean).map(normalizeEntry);
}

function extractFile(archive, entry) {
  const result = spawnSync("tar", ["-xOf", archive, entry], {
    encoding: "utf8",
    env: { ...process.env, LC_ALL: "C" },
  });
  if (result.error) throw new Error(`Could not read ${entry}: ${result.error.message}`);
  if (result.status !== 0) {
    throw new Error(`Could not read ${entry} from ${archive}: ${(result.stderr || "").trim()}`);
  }
  return result.stdout;
}

function readJson(path) {
  return JSON.parse(readFileSync(path, "utf8"));
}

function packageEntryPath(entry) {
  return entry.startsWith("./") ? entry.slice(2) : entry;
}

/**
 * @param {string} archive
 * @param {{ root?: string, expectedPackage?: object }} [options]
 * @returns {{ ok: boolean, errors: string[], archive: string, entries: string[] }}
 */
function verifyPackageArchive(archive, options = {}) {
  const errors = [];
  let entries;
  try {
    entries = archiveEntries(archive);
  } catch (error) {
    return { ok: false, errors: [error.message], archive, entries: [] };
  }
  const set = new Set(entries);

  for (const required of REQUIRED_FILES) {
    if (!set.has(required)) errors.push(`required package file is missing: ${required}`);
  }
  for (const required of REQUIRED_DIRECTORIES) {
    if (!entries.some((entry) => entry.startsWith(`${required}/`))) {
      errors.push(`required package directory is empty or missing: ${required}/`);
    }
  }

  for (const forbidden of FORBIDDEN_FILES) {
    if (set.has(forbidden)) errors.push(`private or transient file must not be packaged: ${forbidden}`);
  }
  for (const entry of entries) {
    const prefix = FORBIDDEN_PREFIXES.find((candidate) => entry.startsWith(candidate));
    if (prefix) errors.push(`private or transient path must not be packaged: ${entry}`);
    if (/(^|\/)\.(env|npmrc)(\.|$)/.test(entry)) {
      errors.push(`environment or npm configuration must not be packaged: ${entry}`);
    }
    if (/\.tgz$/i.test(entry)) errors.push(`nested package archive must not be packaged: ${entry}`);
  }

  const packageEntry = entries.find((entry) => packageEntryPath(entry) === "package/package.json");
  if (packageEntry) {
    try {
      const packaged = JSON.parse(extractFile(archive, packageEntry));
      const expected = options.expectedPackage ||
        (options.root ? readJson(join(resolve(options.root), "package.json")) : undefined);
      if (packaged.license) {
        for (const document of ['LICENSE', 'SECURITY.md', 'THIRD_PARTY_NOTICES.md']) {
          if (!set.has(`package/${document}`)) errors.push(`release document is missing: ${document}`);
        }
        if (packaged.license !== 'MIT') errors.push(`unexpected release license: ${packaged.license}`);
      }
      if (expected) {
        if (packaged.name !== expected.name) {
          errors.push(`package name mismatch: expected ${expected.name}, found ${packaged.name}`);
        }
        if (packaged.version !== expected.version) {
          errors.push(`package version mismatch: expected ${expected.version}, found ${packaged.version}`);
        }
        if (packaged.bin?.rigscope !== "bin/rigscope.cjs") {
          errors.push("packaged bin.rigscope must point at bin/rigscope.cjs");
        }
        if (!Array.isArray(packaged.files) || !packaged.files.includes("dist/")) {
          errors.push("packaged files must include dist/");
        }
        if (!Array.isArray(packaged.files) || !packaged.files.some((entry) => entry.startsWith(".next/"))) {
          errors.push("packaged files must include the .next/ build output");
        }
      }
    } catch (error) {
      errors.push(`could not parse packaged package.json: ${error.message}`);
    }
  }

  return { ok: errors.length === 0, errors, archive, entries };
}

function firstPackEntry(parsed) {
  return Array.isArray(parsed) ? parsed[0] : Object.values(parsed)[0];
}

function parsePackOutput(stdout) {
  const text = stdout.trim();
  if (!text) throw new Error("npm pack did not return JSON output.");
  const candidates = [0, text.indexOf("["), text.indexOf("{")]
    .filter((index, position, all) => index >= 0 && all.indexOf(index) === position)
    .sort((a, b) => a - b);
  for (const start of candidates) {
    try {
      const entry = firstPackEntry(JSON.parse(text.slice(start)));
      if (entry?.filename) return entry;
    } catch {
      // Try the next possible JSON start.
    }
  }
  throw new Error(`Could not parse npm pack output: ${text.slice(0, 500)}`);
}

/**
 * Run `npm pack` (using the checkout's prepack build) and verify the archive.
 * @param {string} [root]
 * @returns {{ ok: boolean, errors: string[], archive?: string }}
 */
function packAndVerify(root = process.cwd()) {
  const base = resolve(root);
  const temporary = mkdtempSync(join(tmpdir(), "rigscope-verify-package-"));
  try {
    const result = spawnSync("npm", ["pack", "--json", "--pack-destination", temporary], {
      cwd: base,
      encoding: "utf8",
      env: process.env,
      stdio: ["ignore", "pipe", "inherit"],
    });
    if (result.error) return { ok: false, errors: [`Could not run npm pack: ${result.error.message}`] };
    if (result.status !== 0) return { ok: false, errors: [`npm pack exited with status ${result.status}`] };
    let entry;
    try {
      entry = parsePackOutput(result.stdout || "");
    } catch (error) {
      return { ok: false, errors: [error.message] };
    }
    const archive = join(temporary, entry.filename);
    if (!existsSync(archive)) return { ok: false, errors: [`npm pack did not create ${entry.filename}`] };
    const report = verifyPackageArchive(archive, { root: base });
    return { ok: report.ok, errors: report.errors, archive };
  } finally {
    rmSync(temporary, { recursive: true, force: true });
  }
}

if (require.main === module) {
  const args = process.argv.slice(2);
  const rootIndex = args.indexOf("--root");
  const root = rootIndex >= 0 ? args.splice(rootIndex, 2)[1] : process.cwd();
  const archive = args.find((argument) => !argument.startsWith("--"));
  const report = archive
    ? verifyPackageArchive(resolve(archive), { root })
    : packAndVerify(root);
  if (!report.ok) {
    for (const error of report.errors) console.error(`verify-package: ${error}`);
    process.exit(1);
  }
  console.log(`verify-package: ${archive || "fresh npm pack"} includes all required build artifacts.`);
}

module.exports = {
  FORBIDDEN_FILES,
  FORBIDDEN_PREFIXES,
  REQUIRED_DIRECTORIES,
  REQUIRED_FILES,
  packAndVerify,
  parsePackOutput,
  verifyPackageArchive,
};
