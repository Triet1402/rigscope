/**
 * Release packaging, isolated installation, activation, and rollback.
 *
 * The release is the built Next.js output plus the esbuild runtime bundles and
 * package metadata. Source, tests, caches, git data and private credentials are
 * excluded. Installation stages a versioned directory and changes `current`
 * only on explicit activation; service files are written to an operator-named
 * units directory and are never installed or started by this code.
 *
 * Database schema changes are handled only during activation: back up first,
 * migrate with the release's schema, and switch `current` after verification.
 * Rollback switches the symlink only when the previous release can read the
 * current schema; otherwise it restores the matching pre-upgrade database
 * backup. There is no claim that an old binary can read a newer schema.
 */

import {
  chmodSync,
  copyFileSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  renameSync,
  rmSync,
  statSync,
} from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { randomUUID } from "node:crypto";
import {
  OperationsError,
  assertNoActiveHubLock,
  currentSymlink,
  ensurePrivateDir,
  ensurePrivateFile,
  ensurePublicDir,
  errorMessage,
  fsyncDir,
  isInside,
  readJsonFile,
  removeIfExists,
  replaceSymlink,
  resolvePath,
  sha256File,
  systemdQuote,
  timestampSlug,
  validateVersion,
  walkFiles,
  writePrivateFile,
  writePublicFile,
} from "./fs";
import {
  SUPPORTED_SCHEMA_VERSION,
  backupDatabase,
  inspectDatabase,
  migrateDatabase,
  restoreDatabase,
  schemaVersionOf,
  type MigrateRunner,
} from "./database";

export const RELEASE_FORMAT_VERSION = 1;
export const INSTALL_STATE_FORMAT_VERSION = 1;
export const HUB_SHUTDOWN_DEADLINE_MS = 5_000;
export const COLLECTOR_SHUTDOWN_DEADLINE_MS = 3_000;

const RELEASE_ENTRYPOINTS = {
  hub: "dist/hub.cjs",
  collector: "dist/collector.cjs",
  ops: "dist/ops.cjs",
} as const;

const REQUIRED_ARTIFACTS = [
  RELEASE_ENTRYPOINTS.hub,
  RELEASE_ENTRYPOINTS.collector,
  RELEASE_ENTRYPOINTS.ops,
  ".next/BUILD_ID",
  "package.json",
  "package-lock.json",
  "next.config.js",
  "tsconfig.json",
];

const PACKAGE_TOP_LEVEL = new Set([
  "bin",
  "dist",
  ".next",
  "public",
  "packaging",
  "docs",
  "package.json",
  "package-lock.json",
  "next.config.js",
  "next-env.d.ts",
  "tsconfig.json",
  "README.md",
  "CHANGELOG.md",
  "ARCHITECTURE.md",
  "DESIGN.md",
  "LICENSE",
  "LICENSE.md",
  "SECURITY.md",
  "THIRD_PARTY_NOTICES.md",
]);

const PACKAGE_DOCS = new Set([
  "docs/installation.md",
  "docs/system-installation.md",
  "docs/configuration.md",
  "docs/monitoring.md",
  "docs/operations.md",
  "docs/development.md",
  "docs/releasing.md",
  "docs/scale-validation.md",
  "docs/scale-validation.json",
  "docs/scale-acceptance.json",
  "docs/bottlenecks.md",
  "docs/exports.md",
]);

const PRIVATE_FILE_PATTERNS = [
  /(^|\/)\.env($|\.)/,
  /(^|\/)hub-config\.json$/,
  /(^|\/)collector\.json$/,
  /(^|\/)install-state\.json$/,
  /\.sqlite($|-)/,
  /\.(pem|key|p12|pfx|jks)$/i,
  /(^|\/)id_(rsa|ed25519)(\.pub)?$/,
  /\.(log|pid|lock|tmp)$/,
];

export type ReleaseArtifact = { path: string; bytes: number; sha256: string };

export type ReleaseManifest = {
  formatVersion: 1;
  name: string;
  version: string;
  createdAt: string;
  platform: string;
  arch: string;
  nodeMajor: number;
  schemaVersion: number;
  entrypoints: { hub: string; collector: string; ops: string };
  dependencyInstall: string;
  productionDependencies: string[];
  requiredArtifacts: string[];
  artifacts: ReleaseArtifact[];
  excluded: string[];
  notes: string[];
};

export type PackageOptions = {
  source: string;
  output?: string;
  replace?: boolean;
  now?: () => number;
};

export type PackageReport = {
  archive: string;
  manifestPath: string;
  manifest: ReleaseManifest;
  bytes: number;
  sha256: string;
  files: number;
};

export type InstallOptions = {
  source: string;
  prefix: string;
  stateDir: string;
  unitsDir: string;
  database?: string;
  activate?: boolean;
  confirmStopped?: boolean;
  installDeps?: boolean;
  replaceRelease?: boolean;
  nodePath?: string;
  now?: () => number;
  runMigrations?: MigrateRunner;
};

export type InstallReport = {
  version: string;
  releaseDir: string;
  releaseSchemaVersion: number;
  prefix: string;
  stateDir: string;
  database: string;
  units: string[];
  hubEnv: string;
  collectorEnv: string;
  activated: boolean;
  current: string | null;
  previous: string | null;
  dependencyState: "present" | "missing";
  schemaFrom: number | null;
  schemaTo: number | null;
  backupPath: string | null;
  activationCommands: string[];
};

type InstallState = {
  formatVersion: 1;
  prefix: string;
  stateDir: string;
  database: string;
  currentVersion: string | null;
  previousVersion: string | null;
  releases: Record<string, { schemaVersion: number; installedAt: string }>;
  backups: Record<
    string,
    { path: string; schemaVersion: number; fromSchema: number | null; toSchema: number; createdAt: string }
  >;
  lastActivation: {
    version: string;
    previousVersion: string | null;
    schemaVersion: number;
    backupPath: string | null;
    backupSchemaVersion: number | null;
    activatedAt: string;
  } | null;
  updatedAt: string;
};

function assertSafeRelativePath(path: string, label: string): string {
  if (typeof path !== "string" || path.length === 0 || path.length > 1024) {
    throw new OperationsError("invalid-manifest", `${label} has an empty or oversized path`);
  }
  if (path.includes("\\") || path.includes("\0") || path.startsWith("/") || /^[A-Za-z]:/.test(path)) {
    throw new OperationsError("invalid-manifest", `${label} contains an unsafe path: ${path}`);
  }
  const parts = path.split("/");
  if (parts.some((part) => part === "" || part === "." || part === "..")) {
    throw new OperationsError("invalid-manifest", `${label} contains an unsafe path: ${path}`);
  }
  return parts.join("/");
}

function isPrivateArtifact(relativePath: string): boolean {
  return PRIVATE_FILE_PATTERNS.some((pattern) => pattern.test(relativePath));
}

function packageInclude(relativePath: string): boolean {
  if (isPrivateArtifact(relativePath)) return false;
  const [top] = relativePath.split("/");
  if (top === undefined || !PACKAGE_TOP_LEVEL.has(top)) return false;
  if (relativePath.startsWith(".next/cache/") || relativePath.startsWith(".next-dev/")) return false;
  if (top === "docs" && !PACKAGE_DOCS.has(relativePath)) return false;
  return true;
}

function copySelectedFiles(
  source: string,
  destination: string,
  include: (relativePath: string) => boolean,
  skipDir?: (relativePath: string) => boolean,
): number {
  const files = walkFiles(source, skipDir);
  let copied = 0;
  for (const file of files) {
    if (!include(file.relative)) continue;
    const target = join(destination, file.relative);
    mkdirSync(dirname(target), { recursive: true, mode: 0o755 });
    copyFileSync(file.path, target);
    chmodSync(target, 0o644);
    copied += 1;
  }
  return copied;
}

/**
 * Next 15 loads next.config.js before next.config.ts. The production release
 * must not depend on the TypeScript compiler being installed with
 * `npm ci --omit=dev`, so package the built config as JavaScript. Transpiling
 * with the build-time esbuild dependency preserves the configured distDir and
 * any future config code instead of hard-coding an empty config.
 */
async function writeProductionNextConfig(source: string, stage: string): Promise<void> {
  const jsConfig = join(source, "next.config.js");
  if (existsSync(jsConfig)) {
    copyFileSync(jsConfig, join(stage, "next.config.js"));
    chmodSync(join(stage, "next.config.js"), 0o644);
    return;
  }
  const tsConfig = join(source, "next.config.ts");
  if (!existsSync(tsConfig)) {
    throw new OperationsError("missing-artifact", "next.config.js or next.config.ts is required");
  }
  let esbuild: typeof import("esbuild");
  try {
    esbuild = await import("esbuild");
  } catch (error) {
    throw new OperationsError(
      "packaging-tool-missing",
      "packaging requires the build-time esbuild dependency to transpile next.config.ts",
      { cause: error },
    );
  }
  let output: string;
  try {
    output = esbuild.transformSync(readFileSync(tsConfig, "utf8"), {
      loader: "ts",
      format: "cjs",
      platform: "node",
      target: "node26",
    }).code;
  } catch (error) {
    throw new OperationsError("config-transpile-failed", `next.config.ts could not be transpiled: ${errorMessage(error)}`, {
      cause: error,
    });
  }
  writePublicFile(join(stage, "next.config.js"), `${output}\nmodule.exports = module.exports.default ?? module.exports;\n`);
}

async function buildManifest(
  root: string,
  manifestBase: Omit<ReleaseManifest, "artifacts">,
): Promise<ReleaseManifest> {
  const files = walkFiles(root, (relativePath) => relativePath === "node_modules" || relativePath.startsWith("node_modules/"));
  const artifacts: ReleaseArtifact[] = [];
  for (const file of files) {
    if (file.relative === "manifest.json") continue;
    artifacts.push({ path: file.relative, bytes: file.bytes, sha256: await sha256File(file.path) });
  }
  return { ...manifestBase, artifacts };
}

function writeManifest(root: string, manifest: ReleaseManifest): string {
  const path = join(root, "manifest.json");
  writePublicFile(path, `${JSON.stringify(manifest, null, 2)}\n`);
  return path;
}

function manifestForPackage(
  packageJson: Record<string, unknown>,
  version: string,
  now: () => number,
): Omit<ReleaseManifest, "artifacts"> {
  const name = typeof packageJson.name === "string" && packageJson.name.length > 0 ? packageJson.name : "rigscope";
  const dependencies =
    packageJson.dependencies !== null && typeof packageJson.dependencies === "object"
      ? Object.keys(packageJson.dependencies as Record<string, unknown>).filter((key) => /^[A-Za-z0-9@/._-]+$/.test(key))
      : [];
  return {
    formatVersion: RELEASE_FORMAT_VERSION,
    name,
    version: validateVersion(version),
    createdAt: new Date(now()).toISOString(),
    platform: process.platform,
    arch: process.arch,
    nodeMajor: 26,
    schemaVersion: SUPPORTED_SCHEMA_VERSION,
    entrypoints: { ...RELEASE_ENTRYPOINTS },
    dependencyInstall: "npm ci --omit=dev",
    productionDependencies: dependencies,
    requiredArtifacts: [...REQUIRED_ARTIFACTS],
    excluded: [
      "src/, collector/, scripts/, tests/",
      ".git/, .next-dev/, .next/cache/",
      "node_modules/ (install with npm ci --omit=dev)",
      "private credentials, environment files, SQLite databases, logs, locks",
    ],
    notes: ["Built artifacts only. Install production dependencies before activation."],
  };
}

function readJsonObject(path: string): Record<string, unknown> {
  let value: unknown;
  try {
    value = JSON.parse(readFileSync(path, "utf8"));
  } catch (error) {
    throw new OperationsError("invalid-json", `${path} is not valid JSON`, { cause: error });
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new OperationsError("invalid-json", `${path} must contain a JSON object`);
  }
  return value as Record<string, unknown>;
}

function runTar(args: string[], cwd?: string): void {
  const result = spawnSync("tar", args, {
    cwd,
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
    env: { ...process.env, LC_ALL: "C" },
  });
  if (result.status !== 0) {
    throw new OperationsError(
      "archive-failed",
      `tar ${args.join(" ")} failed: ${(result.stderr || result.stdout || "unknown error").trim()}`,
    );
  }
}

function listArchive(path: string): string[] {
  const result = spawnSync("tar", ["-tzf", path], {
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
    env: { ...process.env, LC_ALL: "C" },
  });
  if (result.status !== 0) {
    throw new OperationsError("invalid-archive", `cannot list release archive: ${(result.stderr || "").trim()}`);
  }
  return result.stdout.split("\n").filter((line) => line.length > 0);
}

function assertArchiveEntriesSafe(path: string): void {
  const names = listArchive(path);
  for (const raw of names) {
    const name = raw.replace(/^\.\//, "").replace(/\/+$/, "");
    if (name === "" || name === ".") continue;
    assertSafeRelativePath(name, "archive entry");
  }
  const verbose = spawnSync("tar", ["-tvzf", path], {
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
    env: { ...process.env, LC_ALL: "C" },
  });
  if (verbose.status !== 0) {
    throw new OperationsError("invalid-archive", "cannot inspect release archive entry types");
  }
  for (const line of verbose.stdout.split("\n")) {
    if (line.trim() === "") continue;
    const kind = line[0];
    if (kind !== "-" && kind !== "d") {
      throw new OperationsError("invalid-archive", `archive contains a non-regular entry (${kind}): ${line}`);
    }
  }
}

function assertNoSymlinks(root: string): void {
  const visit = (directory: string): void => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const absolute = join(directory, entry.name);
      if (entry.isSymbolicLink()) {
        throw new OperationsError("unsafe-path", `release contains a symlink: ${absolute}`);
      }
      if (entry.isDirectory()) visit(absolute);
    }
  };
  visit(root);
}

function extractArchive(archive: string, destination: string): void {
  assertArchiveEntriesSafe(archive);
  mkdirSync(destination, { recursive: true, mode: 0o700 });
  runTar(["-xzf", archive, "-C", destination, "--no-same-owner", "--no-same-permissions"]);
  assertNoSymlinks(destination);
}

function validateManifest(root: string): ReleaseManifest {
  const manifestPath = join(root, "manifest.json");
  const raw = readJsonObject(manifestPath);
  if (raw.formatVersion !== RELEASE_FORMAT_VERSION) {
    throw new OperationsError("invalid-manifest", `unsupported release manifest format ${String(raw.formatVersion)}`);
  }
  if (typeof raw.name !== "string" || raw.name.length === 0 || raw.name.length > 128) {
    throw new OperationsError("invalid-manifest", "manifest name is invalid");
  }
  const version = validateVersion(String(raw.version));
  if (!Number.isSafeInteger(raw.schemaVersion) || (raw.schemaVersion as number) < 0) {
    throw new OperationsError("invalid-manifest", "manifest schemaVersion is invalid");
  }
  if (raw.nodeMajor !== 26) {
    throw new OperationsError("unsupported-node", `release targets Node ${String(raw.nodeMajor)}; this installer manages Node 26`);
  }
  if (raw.platform !== process.platform || raw.arch !== process.arch) {
    throw new OperationsError(
      "unsupported-platform",
      `release targets ${String(raw.platform)}/${String(raw.arch)}, this host is ${process.platform}/${process.arch}`,
    );
  }
  if (!Array.isArray(raw.artifacts) || !Array.isArray(raw.requiredArtifacts)) {
    throw new OperationsError("invalid-manifest", "manifest artifacts are invalid");
  }
  const artifacts: ReleaseArtifact[] = [];
  const seen = new Set<string>();
  for (const entry of raw.artifacts) {
    if (entry === null || typeof entry !== "object") throw new OperationsError("invalid-manifest", "artifact entry is invalid");
    const record = entry as Record<string, unknown>;
    const path = assertSafeRelativePath(String(record.path), "artifact");
    if (path === "manifest.json" || path.startsWith("node_modules/")) {
      throw new OperationsError("invalid-manifest", `artifact path is not allowed: ${path}`);
    }
    if (seen.has(path)) throw new OperationsError("invalid-manifest", `duplicate artifact path: ${path}`);
    seen.add(path);
    if (!Number.isSafeInteger(record.bytes) || (record.bytes as number) < 0) {
      throw new OperationsError("invalid-manifest", `artifact size is invalid: ${path}`);
    }
    if (typeof record.sha256 !== "string" || !/^[0-9a-f]{64}$/.test(record.sha256)) {
      throw new OperationsError("invalid-manifest", `artifact hash is invalid: ${path}`);
    }
    artifacts.push({ path, bytes: record.bytes as number, sha256: record.sha256 });
  }
  for (const required of REQUIRED_ARTIFACTS) {
    if (!seen.has(required)) {
      throw new OperationsError("invalid-manifest", `required artifact is missing from the manifest: ${required}`);
    }
  }
  const rejected = raw.artifacts
    .map((entry) => (entry as Record<string, unknown>).path)
    .filter((path): path is string => typeof path === "string" && isPrivateArtifact(path));
  if (rejected.length > 0) {
    throw new OperationsError("invalid-manifest", `release contains private-looking artifacts: ${rejected.join(", ")}`);
  }
  const dependencies = Array.isArray(raw.productionDependencies)
    ? raw.productionDependencies.filter((item): item is string => typeof item === "string")
    : [];
  for (const dependency of dependencies) {
    if (!/^(?:@[A-Za-z0-9._-]+\/)?[A-Za-z0-9._-]+$/.test(dependency)) {
      throw new OperationsError("invalid-manifest", `invalid production dependency name: ${dependency}`);
    }
  }
  for (const requiredDependency of ["next", "ws"]) {
    if (!dependencies.includes(requiredDependency)) {
      throw new OperationsError("invalid-manifest", `manifest is missing required production dependency ${requiredDependency}`);
    }
  }
  const manifest: ReleaseManifest = {
    formatVersion: 1,
    name: raw.name,
    version,
    createdAt: typeof raw.createdAt === "string" ? raw.createdAt : "",
    platform: typeof raw.platform === "string" ? raw.platform : "unknown",
    arch: typeof raw.arch === "string" ? raw.arch : "unknown",
    nodeMajor: 26,
    schemaVersion: raw.schemaVersion as number,
    entrypoints: RELEASE_ENTRYPOINTS,
    dependencyInstall: typeof raw.dependencyInstall === "string" ? raw.dependencyInstall : "npm ci --omit=dev",
    productionDependencies: dependencies,
    requiredArtifacts: raw.requiredArtifacts.filter((item): item is string => typeof item === "string"),
    artifacts,
    excluded: Array.isArray(raw.excluded) ? raw.excluded.filter((item): item is string => typeof item === "string") : [],
    notes: Array.isArray(raw.notes) ? raw.notes.filter((item): item is string => typeof item === "string") : [],
  };
  return manifest;
}

function assertManifestMatchesPackage(root: string, manifest: ReleaseManifest): void {
  const packageJson = readJsonObject(join(root, "package.json"));
  if (String(packageJson.name ?? "") !== manifest.name) {
    throw new OperationsError("manifest-mismatch", "release manifest name does not match package.json");
  }
  if (String(packageJson.version ?? "") !== manifest.version) {
    throw new OperationsError("manifest-mismatch", "release manifest version does not match package.json");
  }
}

async function verifyManifestFiles(root: string, manifest: ReleaseManifest): Promise<void> {
  const listed = new Map(manifest.artifacts.map((artifact) => [artifact.path, artifact]));
  for (const artifact of manifest.artifacts) {
    const absolute = join(root, artifact.path);
    let stat;
    try {
      stat = lstatSync(absolute);
    } catch {
      throw new OperationsError("missing-artifact", `release artifact is missing: ${artifact.path}`);
    }
    if (!stat.isFile() || stat.isSymbolicLink()) {
      throw new OperationsError("unsafe-path", `release artifact is not a regular file: ${artifact.path}`);
    }
    if (stat.size !== artifact.bytes) {
      throw new OperationsError("artifact-mismatch", `release artifact size changed: ${artifact.path}`);
    }
    const hash = await sha256File(absolute);
    if (hash !== artifact.sha256) {
      throw new OperationsError("artifact-mismatch", `release artifact hash changed: ${artifact.path}`);
    }
  }
  const files = walkFiles(
    root,
    (relativePath) =>
      relativePath === "node_modules" ||
      relativePath.startsWith("node_modules/") ||
      relativePath === ".next/cache",
  );
  for (const file of files) {
    if (file.relative === "manifest.json") continue;
    if (!listed.has(file.relative)) {
      throw new OperationsError("artifact-mismatch", `release contains an unlisted file: ${file.relative}`);
    }
  }
}

export async function packageRelease(options: PackageOptions): Promise<PackageReport> {
  const source = resolvePath(options.source, "release source");
  const stat = lstatSync(source);
  if (!stat.isDirectory() || stat.isSymbolicLink()) {
    throw new OperationsError("unsafe-path", "release source must be a real directory");
  }
  const packageJson = readJsonObject(join(source, "package.json"));
  const version = validateVersion(String(packageJson.version ?? ""));
  const name = typeof packageJson.name === "string" && packageJson.name.length > 0 ? packageJson.name : "rigscope";
  const output = resolvePath(options.output ?? join(process.cwd(), `${name}-${version}.tar.gz`), "archive output");
  if (isInside(source, output)) {
    // An archive in the source tree is fine only when the package allowlist
    // would not copy it back into itself. The default root-level archive and
    // external manifest are outside the allowlist.
    const relativeOutput = relative(source, output).split(sep).join("/");
    if (packageInclude(relativeOutput)) {
      throw new OperationsError(
        "unsafe-path",
        `release archive path would be packaged into itself: ${relativeOutput}`,
      );
    }
  }
  for (const required of REQUIRED_ARTIFACTS) {
    if (required === "next.config.js") {
      if (!existsSync(join(source, "next.config.js")) && !existsSync(join(source, "next.config.ts"))) {
        throw new OperationsError("missing-artifact", "required built artifact is missing: next.config.js or next.config.ts");
      }
      continue;
    }
    const absolute = join(source, required);
    let fileStat;
    try {
      fileStat = lstatSync(absolute);
    } catch {
      throw new OperationsError("missing-artifact", `required built artifact is missing: ${required}`);
    }
    if (!fileStat.isFile() || fileStat.isSymbolicLink()) {
      throw new OperationsError("unsafe-path", `required artifact must be a regular file: ${required}`);
    }
  }
  if (existsSync(output)) {
    const existing = lstatSync(output);
    if (!existing.isFile()) throw new OperationsError("unsafe-path", "archive output must be a regular file path");
    if (!options.replace) {
      throw new OperationsError("output-exists", `release archive already exists (use --replace): ${output}`);
    }
  }
  ensurePublicDir(dirname(output));

  const stage = join(tmpdir(), `rigscope-package-${randomUUID()}`);
  try {
    mkdirSync(stage, { recursive: true, mode: 0o700 });
    const copied = copySelectedFiles(
      source,
      stage,
      packageInclude,
      (relativePath) => relativePath === ".git" || relativePath === ".next-dev" || relativePath === ".next/cache",
    );
    if (copied === 0) throw new OperationsError("empty-release", "no package files matched the release allowlist");
    await writeProductionNextConfig(source, stage);
    const base = manifestForPackage(packageJson, version, options.now ?? Date.now);
    const manifest = await buildManifest(stage, base);
    writeManifest(stage, manifest);
    const tempArchive = join(dirname(output), `.${basename(output)}.tmp-${randomUUID()}`);
    try {
      runTar(["-czf", tempArchive, "-C", stage, "."]);
      chmodSync(tempArchive, 0o644);
      const bytes = statSync(tempArchive).size;
      const sha256 = await sha256File(tempArchive);
      renameSync(tempArchive, output);
      const archiveStat = statSync(output);
      const externalManifest = `${output}.manifest.json`;
      writePublicFile(externalManifest, `${JSON.stringify(manifest, null, 2)}\n`);
      return {
        archive: output,
        manifestPath: externalManifest,
        manifest,
        bytes: archiveStat.size,
        sha256,
        files: manifest.artifacts.length,
      };
    } catch (error) {
      removeIfExists(tempArchive);
      throw error;
    }
  } finally {
    rmSync(stage, { recursive: true, force: true });
  }
}

function copyReleaseDirectory(source: string, destination: string): void {
  const skip = (relativePath: string): boolean =>
    relativePath === ".git" ||
    relativePath === ".next-dev" ||
    relativePath === ".next/cache" ||
    relativePath === "node_modules/.cache";
  for (const file of walkFiles(source, skip)) {
    const target = join(destination, file.relative);
    mkdirSync(dirname(target), { recursive: true, mode: 0o755 });
    copyFileSync(file.path, target);
    chmodSync(target, 0o644);
  }
}

function missingProductionDependencies(releaseDir: string, manifest: ReleaseManifest): string[] {
  const missing: string[] = [];
  for (const dependency of manifest.productionDependencies) {
    if (!/^[A-Za-z0-9@/._-]+$/.test(dependency)) continue;
    if (!existsSync(join(releaseDir, "node_modules", dependency, "package.json"))) missing.push(dependency);
  }
  return missing;
}

function installStateDefaults(prefix: string, stateDir: string, database: string): InstallState {
  return {
    formatVersion: INSTALL_STATE_FORMAT_VERSION,
    prefix,
    stateDir,
    database,
    currentVersion: null,
    previousVersion: null,
    releases: {},
    backups: {},
    lastActivation: null,
    updatedAt: new Date().toISOString(),
  };
}

function readInstallState(path: string, defaults: InstallState): InstallState {
  if (!existsSync(path)) return defaults;
  const { value } = readJsonFile<InstallState>(path);
  if (value === null || typeof value !== "object" || value.formatVersion !== INSTALL_STATE_FORMAT_VERSION) {
    throw new OperationsError("invalid-state", `installation state has an unsupported format: ${path}`);
  }
  if (value.prefix !== defaults.prefix || value.stateDir !== defaults.stateDir) {
    throw new OperationsError("invalid-state", "installation state belongs to a different prefix or state directory");
  }
  const backups: InstallState["backups"] = {};
  if (value.backups !== null && typeof value.backups === "object" && !Array.isArray(value.backups)) {
    for (const [key, entry] of Object.entries(value.backups as Record<string, unknown>)) {
      if (entry === null || typeof entry !== "object" || Array.isArray(entry)) continue;
      const record = entry as Record<string, unknown>;
      if (typeof record.path !== "string" || !Number.isSafeInteger(record.schemaVersion)) continue;
      backups[key] = {
        path: record.path,
        schemaVersion: record.schemaVersion as number,
        fromSchema: Number.isSafeInteger(record.fromSchema) ? (record.fromSchema as number) : null,
        toSchema: Number.isSafeInteger(record.toSchema) ? (record.toSchema as number) : (record.schemaVersion as number),
        createdAt: typeof record.createdAt === "string" ? record.createdAt : "",
      };
    }
  }
  return {
    ...defaults,
    ...value,
    database: typeof value.database === "string" ? value.database : defaults.database,
    releases:
      value.releases !== null && typeof value.releases === "object" && !Array.isArray(value.releases)
        ? value.releases
        : {},
    backups,
  };
}

function writeInstallState(path: string, state: InstallState): void {
  writePrivateFile(path, `${JSON.stringify({ ...state, updatedAt: new Date().toISOString() }, null, 2)}\n`);
}

function releaseDirFor(prefix: string, version: string): string {
  return join(prefix, "releases", validateVersion(version));
}

function readReleaseManifest(releaseDir: string): ReleaseManifest {
  return validateManifest(releaseDir);
}

function releaseExistsInPrefix(prefix: string, releaseDir: string): boolean {
  return isInside(join(prefix, "releases"), releaseDir);
}

function currentReleaseDir(prefix: string): string | null {
  const target = currentSymlink(join(prefix, "current"));
  if (target === null) return null;
  const absolute = isAbsolute(target) ? target : resolve(prefix, target);
  return releaseExistsInPrefix(prefix, absolute) ? absolute : null;
}

function hubEnvTemplate(): string {
  return [
    "# Rigscope hub environment. Private file, mode 0600.",
    "# This file is intentionally empty of active values so the hub fails closed:",
    "# no browser users means no browser data, and no collector credentials means no fleet.",
    "# Set values below (without copying public demo credentials to a real host):",
    "#   HUB_BROWSER_USERS=[{\"username\":\"operator\",\"passwordHash\":\"...\",\"machineIds\":\"*\"}]",
    "#   HUB_COLLECTORS=[{\"collectorId\":\"...\",\"machineId\":\"...\",\"token\":\"...\"}]",
    "#   HUB_PUBLIC_ORIGIN=https://monitor.example.invalid",
    "#   HUB_SOCKET_PATH=<absolute private Unix socket path>",
    "# Local loopback mode: HUB_HOST=127.0.0.1 HUB_PORT=8081",
    "",
  ].join("\n");
}

function collectorEnvTemplate(): string {
  return [
    "# Rigscope collector environment. Private file, mode 0600.",
    "# Empty configuration fails closed: the collector has no destinations.",
    "# Enroll through the hub before starting a real collector:",
    "#   COLLECTOR_ENROLL_URL=ws://127.0.0.1:8081/api/collector/connect",
    "#   COLLECTOR_ENROLL_TOKEN=<one-use grant>",
    "#   COLLECTOR_MODE=real",
    "",
  ].join("\n");
}

function serviceCommon(
  name: string,
  description: string,
  prefix: string,
  stateDir: string,
  envPath: string,
  readWritePaths: readonly string[],
): string[] {
  const paths = [...new Set([stateDir, ...readWritePaths])];
  return [
    "[Unit]",
    `Description=${description}`,
    "After=network-online.target",
    "Wants=network-online.target",
    "StartLimitIntervalSec=60",
    "StartLimitBurst=5",
    "",
    "[Service]",
    "Type=simple",
    `WorkingDirectory=${systemdQuote(join(prefix, "current"))}`,
    `EnvironmentFile=${systemdQuote(envPath)}`,
    "Environment=NODE_ENV=production",
    "Restart=on-failure",
    "RestartSec=2s",
    "KillSignal=SIGTERM",
    "NoNewPrivileges=yes",
    "PrivateTmp=yes",
    "UMask=0077",
    "ProtectSystem=strict",
    ...paths.map((path) => `ReadWritePaths=${systemdQuote(path)}`),
    "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
    "LockPersonality=yes",
    "ProtectKernelTunables=yes",
    "# ProtectKernelModules is omitted: it requires capability dropping unavailable to some user managers.",
    "ProtectControlGroups=yes",
    "RestrictRealtime=yes",
    "RestrictSUIDSGID=yes",
    "StandardOutput=journal",
    "StandardError=journal",
    `SyslogIdentifier=${name}`,
    "",
  ];
}

export function renderHubUnit(options: {
  prefix: string;
  stateDir: string;
  database: string;
  envPath: string;
  nodePath: string;
}): string {
  const node = resolvePath(options.nodePath, "node path");
  const script = join(options.prefix, "current", "dist", "hub.cjs");
  return [
    "# Managed by Rigscope operations. Enable explicitly with:",
    "#   systemctl --user daemon-reload && systemctl --user enable --now rigscope-hub.service",
    "# Health: curl --fail --silent http://127.0.0.1:8081/api/health",
    "# Remote Unix-socket health: curl --unix-socket <socket> -H 'Host: <public-origin-host>' http://localhost/api/health",
    ...serviceCommon(
      "rigscope-hub",
      "Rigscope hub",
      options.prefix,
      options.stateDir,
      options.envPath,
      [
        dirname(options.database),
        join(options.stateDir, "run"),
        join(options.prefix, "current", ".next", "cache"),
      ],
    ),
    `Environment=${systemdQuote(`HUB_DATABASE_PATH=${options.database}`)}`,
    `TimeoutStopSec=${Math.ceil(HUB_SHUTDOWN_DEADLINE_MS / 1000) + 10}s`,
    `ExecStart=${systemdQuote(node)} ${systemdQuote(script)}`,
    "",
    "[Install]",
    "WantedBy=default.target",
    "",
  ].join("\n");
}

export function renderCollectorUnit(options: {
  prefix: string;
  stateDir: string;
  collectorStateFile: string;
  envPath: string;
  nodePath: string;
}): string {
  const node = resolvePath(options.nodePath, "node path");
  const script = join(options.prefix, "current", "dist", "collector.cjs");
  return [
    "# Managed by Rigscope operations. Enable explicitly after enrollment:",
    "#   systemctl --user daemon-reload && systemctl --user enable --now rigscope-collector.service",
    "# Status/logs: systemctl --user status rigscope-collector.service",
    "#             journalctl --user -u rigscope-collector.service -f",
    ...serviceCommon(
      "rigscope-collector",
      "Rigscope collector",
      options.prefix,
      options.stateDir,
      options.envPath,
      [dirname(options.collectorStateFile)],
    ),
    `Environment=${systemdQuote(`COLLECTOR_STATE_FILE=${options.collectorStateFile}`)}`,
    `TimeoutStopSec=${Math.ceil(COLLECTOR_SHUTDOWN_DEADLINE_MS / 1000) + 7}s`,
    `ExecStart=${systemdQuote(node)} ${systemdQuote(script)}`,
    "",
    "[Install]",
    "WantedBy=default.target",
    "",
  ].join("\n");
}

function writeUnits(options: {
  prefix: string;
  stateDir: string;
  unitsDir: string;
  database: string;
  nodePath: string;
  hubEnv: string;
  collectorEnv: string;
}): { units: string[]; hubUnit: string; collectorUnit: string } {
  ensurePublicDir(options.unitsDir);
  const hubUnit = join(options.unitsDir, "rigscope-hub.service");
  const collectorUnit = join(options.unitsDir, "rigscope-collector.service");
  writePublicFile(
    hubUnit,
    renderHubUnit({
      prefix: options.prefix,
      stateDir: options.stateDir,
      database: options.database,
      envPath: options.hubEnv,
      nodePath: options.nodePath,
    }),
  );
  writePublicFile(
    collectorUnit,
    renderCollectorUnit({
      prefix: options.prefix,
      stateDir: options.stateDir,
      collectorStateFile: join(options.stateDir, "collector", "collector.json"),
      envPath: options.collectorEnv,
      nodePath: options.nodePath,
    }),
  );
  return { units: [hubUnit, collectorUnit], hubUnit, collectorUnit };
}

function activationCommandLines(unitsDir: string): string[] {
  return [
    `# Units are in ${unitsDir}; audit them before enabling.`,
    `systemctl --user daemon-reload`,
    `systemctl --user enable --now rigscope-hub.service`,
    `# For an existing service after activation: systemctl --user restart rigscope-hub.service rigscope-collector.service`,
    `# Enroll/configure the collector, then: systemctl --user enable --now rigscope-collector.service`,
    `# Optional host reboot persistence is an explicit admin step: loginctl enable-linger <user>`,
  ];
}

function nodePathFor(value: string | undefined): string {
  if (value !== undefined && !isAbsolute(value)) {
    throw new OperationsError("invalid-node", "--node must be an absolute Node 26 executable path");
  }
  const node = resolvePath(value ?? process.execPath, "node path");
  const stat = lstatSync(node);
  if (!stat.isFile()) throw new OperationsError("invalid-node", `Node executable is not a regular file: ${node}`);
  if ((stat.mode & 0o111) === 0) throw new OperationsError("invalid-node", `Node executable is not executable: ${node}`);
  const version = spawnSync(node, ["--eval", "process.stdout.write(process.versions.node)"], {
    encoding: "utf8", timeout: 5000, maxBuffer: 1024,
  });
  const actual = version.stdout?.trim() ?? "";
  if (version.status !== 0 || !/^26\.\d+\.\d+$/.test(actual)) {
    throw new OperationsError("unsupported-node", `selected executable must run Node 26 (reported ${/^\d+\.\d+\.\d+$/.test(actual) ? actual : "no valid Node version"})`);
  }
  return node;
}

function ensureEnvFiles(stateDir: string): { hubEnv: string; collectorEnv: string } {
  const hubEnv = join(stateDir, "hub.env");
  const collectorEnv = join(stateDir, "collector.env");
  ensurePrivateFile(hubEnv, hubEnvTemplate());
  ensurePrivateFile(collectorEnv, collectorEnvTemplate());
  return { hubEnv, collectorEnv };
}

async function runInstallDependencies(releaseDir: string): Promise<void> {
  const result = spawnSync("npm", ["ci", "--omit=dev", "--no-audit", "--no-fund"], {
    cwd: releaseDir,
    stdio: "inherit",
    env: { ...process.env, NODE_ENV: "production" },
  });
  if (result.status !== 0) {
    throw new OperationsError("dependency-install-failed", `npm ci --omit=dev failed in ${releaseDir}`);
  }
}

export async function installRelease(options: InstallOptions): Promise<InstallReport> {
  const prefix = resolvePath(options.prefix, "prefix");
  const stateDir = resolvePath(options.stateDir, "state directory");
  const unitsDir = resolvePath(options.unitsDir, "units directory");
  const database = resolvePath(options.database ?? join(stateDir, "hub", "telemetry.sqlite"), "database");
  const now = options.now ?? Date.now;
  const nodePath = nodePathFor(options.nodePath);
  ensurePublicDir(prefix);
  ensurePrivateDir(stateDir);
  for (const child of ["hub", "collector", "run", "backups"]) ensurePrivateDir(join(stateDir, child));
  ensurePublicDir(unitsDir);
  const releasesDir = join(prefix, "releases");
  ensurePublicDir(releasesDir);
  const source = resolvePath(options.source, "release source");
  const sourceStat = lstatSync(source);
  const stage = join(releasesDir, `.stage-${randomUUID()}`);
  try {
    if (sourceStat.isFile()) {
      extractArchive(source, stage);
    } else if (sourceStat.isDirectory()) {
      mkdirSync(stage, { recursive: true, mode: 0o700 });
      copyReleaseDirectory(source, stage);
      assertNoSymlinks(stage);
    } else {
      throw new OperationsError("unsafe-path", "release source must be a directory or .tar.gz archive");
    }
    const manifest = readReleaseManifest(stage);
    assertManifestMatchesPackage(stage, manifest);
    await verifyManifestFiles(stage, manifest);
    const releaseDir = releaseDirFor(prefix, manifest.version);
    if (existsSync(releaseDir)) {
      const current = currentReleaseDir(prefix);
      const previousTarget = currentSymlink(join(prefix, "previous"));
      const previous =
        previousTarget === null
          ? null
          : isAbsolute(previousTarget)
            ? previousTarget
            : resolve(prefix, previousTarget);
      if (!options.replaceRelease || current === releaseDir || previous === releaseDir) {
        throw new OperationsError(
          "release-exists",
          `release ${manifest.version} is already installed${current === releaseDir || previous === releaseDir ? " or retained for rollback" : ""}; choose a new version or --replace-release only for an unreferenced release`,
        );
      }
      rmSync(releaseDir, { recursive: true, force: true });
    }
    renameSync(stage, releaseDir);
    fsyncDir(releasesDir);
    // Next's production runtime may use .next/cache; it is versioned but
    // writable, while the rest of the release remains read-only under
    // ProtectSystem=strict.
    ensurePublicDir(join(releaseDir, ".next", "cache"));
    if (options.installDeps === true) await runInstallDependencies(releaseDir);

    const env = ensureEnvFiles(stateDir);
    const units = writeUnits({
      prefix,
      stateDir,
      unitsDir,
      database,
      nodePath,
      hubEnv: env.hubEnv,
      collectorEnv: env.collectorEnv,
    });
    const statePath = join(stateDir, "install-state.json");
    const state = readInstallState(statePath, installStateDefaults(prefix, stateDir, database));
    state.releases[manifest.version] = {
      schemaVersion: manifest.schemaVersion,
      installedAt: new Date(now()).toISOString(),
    };
    writeInstallState(statePath, state);

    const missing = missingProductionDependencies(releaseDir, manifest);
    let activated = false;
    let current: string | null = null;
    let previous: string | null = null;
    let schemaFrom: number | null = null;
    let schemaTo: number | null = null;
    let backupPath: string | null = null;
    if (options.activate === true) {
      if (missing.length > 0) {
        throw new OperationsError(
          "dependencies-missing",
          `production dependencies are missing in ${releaseDir}: ${missing.join(", ")}. Run "${manifest.dependencyInstall}" in that directory, then activate explicitly.`,
        );
      }
      const activation = await activateRelease({
        prefix,
        stateDir,
        unitsDir,
        database,
        version: manifest.version,
        confirmStopped: options.confirmStopped,
        now,
        runMigrations: options.runMigrations,
      });
      activated = true;
      current = activation.current;
      previous = activation.previous;
      schemaFrom = activation.schemaFrom;
      schemaTo = activation.schemaTo;
      backupPath = activation.backupPath;
    }
    return {
      version: manifest.version,
      releaseDir,
      releaseSchemaVersion: manifest.schemaVersion,
      prefix,
      stateDir,
      database,
      units: units.units,
      hubEnv: env.hubEnv,
      collectorEnv: env.collectorEnv,
      activated,
      current,
      previous,
      dependencyState: missing.length === 0 ? "present" : "missing",
      schemaFrom,
      schemaTo,
      backupPath,
      activationCommands: activationCommandLines(unitsDir),
    };
  } catch (error) {
    rmSync(stage, { recursive: true, force: true });
    throw error;
  }
}

export type ActivateOptions = {
  prefix: string;
  stateDir: string;
  unitsDir?: string;
  database?: string;
  version: string;
  confirmStopped?: boolean;
  now?: () => number;
  runMigrations?: MigrateRunner;
};

export async function activateRelease(options: ActivateOptions): Promise<{
  version: string;
  current: string;
  previous: string | null;
  schemaFrom: number | null;
  schemaTo: number;
  backupPath: string | null;
}> {
  const prefix = resolvePath(options.prefix, "prefix");
  const stateDir = resolvePath(options.stateDir, "state directory");
  const database = resolvePath(options.database ?? join(stateDir, "hub", "telemetry.sqlite"), "database");
  const now = options.now ?? Date.now;
  const version = validateVersion(options.version);
  const releaseDir = releaseDirFor(prefix, version);
  if (!existsSync(releaseDir)) throw new OperationsError("release-missing", `release ${version} is not installed`);
  const manifest = readReleaseManifest(releaseDir);
  if (manifest.version !== version) {
    throw new OperationsError(
      "manifest-mismatch",
      `release directory ${version} contains a manifest for version ${manifest.version}`,
    );
  }
  assertManifestMatchesPackage(releaseDir, manifest);
  await verifyManifestFiles(releaseDir, manifest);
  const missing = missingProductionDependencies(releaseDir, manifest);
  if (missing.length > 0) {
    throw new OperationsError(
      "dependencies-missing",
      `production dependencies are missing in ${releaseDir}: ${missing.join(", ")}; run "${manifest.dependencyInstall}" before activation`,
    );
  }
  if (manifest.schemaVersion > SUPPORTED_SCHEMA_VERSION) {
    throw new OperationsError(
      "newer-schema",
      `release schema ${manifest.schemaVersion} is newer than this operations build (${SUPPORTED_SCHEMA_VERSION}); run the release's own ops binary`,
    );
  }
  ensurePrivateDir(stateDir);
  ensurePublicDir(join(prefix, "releases"));
  ensurePublicDir(prefix);
  ensurePublicDir(join(releaseDir, ".next", "cache"));

  const statePath = join(stateDir, "install-state.json");
  const state = readInstallState(statePath, installStateDefaults(prefix, stateDir, database));
  const oldTarget = currentReleaseDir(prefix);
  const oldVersion = oldTarget === null ? null : basename(oldTarget);
  if (oldTarget !== null) {
    const oldManifest = readReleaseManifest(oldTarget);
    if (oldManifest.version === version) {
      return {
        version,
        current: oldTarget,
        previous: state.previousVersion === null ? null : releaseDirFor(prefix, state.previousVersion),
        schemaFrom: schemaVersionOf(database),
        schemaTo: manifest.schemaVersion,
        backupPath: null,
      };
    }
  }

  const databaseExists = existsSync(database);
  const emptyDatabase = databaseExists && lstatSync(database).size === 0;
  const existingInfo = databaseExists && !emptyDatabase ? inspectDatabase(database) : null;
  if (existingInfo !== null && existingInfo.ledgerProblem !== null) {
    throw new OperationsError(
      "invalid-database",
      `database migration history is not usable: ${existingInfo.ledgerProblem}`,
    );
  }
  const existingSchema = existingInfo === null ? null : existingInfo.schemaVersion;
  if (existingSchema !== null && existingSchema > manifest.schemaVersion) {
    throw new OperationsError(
      "newer-schema",
      `release ${version} supports schema ${manifest.schemaVersion}, but the database is schema ${existingSchema}; use rollback with the matching backup, never point an old binary at a newer schema`,
    );
  }
  if (existingSchema !== null) {
    if (!options.confirmStopped) {
      throw new OperationsError(
        "confirmation-required",
        "activation over an existing database requires --confirm-stopped after the hub is stopped",
      );
    }
    assertNoActiveHubLock(database);
  } else {
    assertNoActiveHubLock(database);
  }

  let schemaFrom = existingSchema;
  let backupPath: string | null = null;
  let migrated = false;
  if (existingSchema === null) {
    if (manifest.schemaVersion !== SUPPORTED_SCHEMA_VERSION) {
      throw new OperationsError(
        "schema-mismatch",
        `cannot create a new database at release schema ${manifest.schemaVersion} with operations schema ${SUPPORTED_SCHEMA_VERSION}; run this release's own ops binary`,
      );
    }
    const migration = await migrateDatabase({
      database,
      confirmStopped: false,
      now,
      runMigrations: options.runMigrations,
    });
    schemaFrom = null;
    migrated = true;
  } else if (existingSchema < manifest.schemaVersion) {
    if (manifest.schemaVersion !== SUPPORTED_SCHEMA_VERSION) {
      throw new OperationsError(
        "schema-mismatch",
        `release schema ${manifest.schemaVersion} is older than the migration target ${SUPPORTED_SCHEMA_VERSION}; run the release's own ops binary for an exact upgrade`,
      );
    }
    const migration = await migrateDatabase({
      database,
      backupDir: join(stateDir, "backups"),
      confirmStopped: true,
      now,
      runMigrations: options.runMigrations,
    });
    backupPath = migration.backupPath;
    migrated = migration.migrated;
  } else {
    // Schema already matches. Keep a pre-activation backup so a later rollback
    // has a known-good copy even when no migration was required.
    ensurePrivateDir(join(stateDir, "backups"));
    backupPath = join(
      stateDir,
      "backups",
      `${basename(database)}.pre-activate-${timestampSlug(now())}-${randomUUID().slice(0, 8)}.sqlite`,
    );
    await backupDatabase({ database, output: backupPath, now });
  }

  const currentPath = join(prefix, "current");
  const previousPath = join(prefix, "previous");
  const currentTarget = oldTarget;
  try {
    if (currentTarget !== null) replaceSymlink(previousPath, currentTarget);
    else removeIfExists(previousPath);
    replaceSymlink(currentPath, releaseDir);
  } catch (error) {
    if (migrated && backupPath !== null) {
      try {
        await restoreDatabase({ input: backupPath, database, replace: true, confirmStopped: true, now });
      } catch (restoreError) {
        throw new OperationsError(
          "activation-failed",
          `activation failed and database recovery also failed: ${errorMessage(error)}; restore ${backupPath} manually (${errorMessage(restoreError)})`,
        );
      }
    }
    throw new OperationsError(
      "activation-failed",
      `activation failed before the release switch completed: ${errorMessage(error)}`,
      { cause: error },
    );
  }

  if (backupPath !== null) {
    state.backups[version] = {
      path: backupPath,
      schemaVersion: existingSchema ?? 0,
      fromSchema: existingSchema,
      toSchema: manifest.schemaVersion,
      createdAt: new Date(now()).toISOString(),
    };
  }
  state.currentVersion = version;
  state.previousVersion = oldVersion;
  state.database = database;
  state.lastActivation = {
    version,
    previousVersion: oldVersion,
    schemaVersion: manifest.schemaVersion,
    backupPath,
    backupSchemaVersion: existingSchema,
    activatedAt: new Date(now()).toISOString(),
  };
  writeInstallState(statePath, state);
  return {
    version,
    current: releaseDir,
    previous: currentTarget,
    schemaFrom,
    schemaTo: manifest.schemaVersion,
    backupPath,
  };
}

export type RollbackOptions = {
  prefix: string;
  stateDir: string;
  database?: string;
  confirmStopped?: boolean;
  now?: () => number;
};

export async function rollbackRelease(options: RollbackOptions): Promise<{
  version: string;
  current: string;
  previous: string | null;
  restoredBackup: string | null;
  schemaFrom: number | null;
  schemaTo: number | null;
}> {
  const prefix = resolvePath(options.prefix, "prefix");
  const stateDir = resolvePath(options.stateDir, "state directory");
  const database = resolvePath(options.database ?? join(stateDir, "hub", "telemetry.sqlite"), "database");
  ensurePrivateDir(stateDir);
  const statePath = join(stateDir, "install-state.json");
  const state = readInstallState(statePath, installStateDefaults(prefix, stateDir, database));
  const previousVersion = state.previousVersion;
  if (previousVersion === null) {
    throw new OperationsError("no-previous-release", "no previous release is recorded for this prefix");
  }
  const previousRelease = releaseDirFor(prefix, previousVersion);
  if (!existsSync(previousRelease)) {
    throw new OperationsError("release-missing", `previous release ${previousVersion} is no longer installed`);
  }
  const previousManifest = readReleaseManifest(previousRelease);
  ensurePublicDir(join(previousRelease, ".next", "cache"));
  const currentRelease = currentReleaseDir(prefix);
  if (currentRelease === null) {
    throw new OperationsError("no-current-release", "the prefix has no current release symlink");
  }
  if (!options.confirmStopped) {
    throw new OperationsError("confirmation-required", "rollback requires --confirm-stopped after the hub is stopped");
  }
  assertNoActiveHubLock(database);

  const databaseInfo = existsSync(database) ? inspectDatabase(database) : null;
  if (databaseInfo !== null && databaseInfo.ledgerProblem !== null) {
    throw new OperationsError(
      "invalid-database",
      `database migration history is not usable: ${databaseInfo.ledgerProblem}`,
    );
  }
  const schemaFrom = databaseInfo === null ? null : databaseInfo.schemaVersion;
  let restoredBackup: string | null = null;
  if (schemaFrom !== null && schemaFrom > previousManifest.schemaVersion) {
    const currentVersion = state.currentVersion ?? basename(currentRelease);
    const recorded = state.backups[currentVersion];
    const backup = recorded?.path ?? state.lastActivation?.backupPath ?? null;
    if (backup === null || !existsSync(backup)) {
      throw new OperationsError(
        "backup-missing",
        `database schema ${schemaFrom} is newer than previous release ${previousVersion} (schema ${previousManifest.schemaVersion}); no matching pre-upgrade backup is recorded, so rollback refuses to point the old release at a newer database`,
      );
    }
    const backupInfo = inspectDatabase(backup);
    if (backupInfo.schemaVersion > previousManifest.schemaVersion) {
      throw new OperationsError(
        "backup-mismatch",
        `recorded backup schema ${backupInfo.schemaVersion} is newer than previous release schema ${previousManifest.schemaVersion}`,
      );
    }
    const restored = await restoreDatabase({
      input: backup,
      database,
      replace: true,
      confirmStopped: true,
      now: options.now,
    });
    restoredBackup = restored.preRestoreBackup;
  }

  const currentPath = join(prefix, "current");
  const previousPath = join(prefix, "previous");
  try {
    replaceSymlink(previousPath, currentRelease);
    replaceSymlink(currentPath, previousRelease);
  } catch (error) {
    throw new OperationsError(
      "rollback-failed",
      `rollback symlink switch failed; current still points at ${currentRelease}: ${errorMessage(error)}`,
      { cause: error },
    );
  }
  state.currentVersion = previousVersion;
  state.previousVersion = basename(currentRelease);
  state.database = database;
  state.lastActivation = {
    version: previousVersion,
    previousVersion: basename(currentRelease),
    schemaVersion: previousManifest.schemaVersion,
    backupPath: null,
    backupSchemaVersion: null,
    activatedAt: new Date(options.now?.() ?? Date.now()).toISOString(),
  };
  writeInstallState(statePath, state);
  return {
    version: previousVersion,
    current: previousRelease,
    previous: currentRelease,
    restoredBackup,
    schemaFrom,
    schemaTo: schemaVersionOf(database),
  };
}
