/**
 * Isolated packaging/install/upgrade/rollback tests.
 *
 * Fixture releases contain fake built artifacts and fake production
 * dependencies. No service is started, no systemctl command is executed, and
 * no developer state or live database is touched.
 */
import assert from "node:assert/strict";
import test from "node:test";
import { spawnSync } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readlinkSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";

import { MIGRATIONS } from "../src/hub/storage/migrations";
import { parseHubConfig } from "../src/hub/config";
import { parseBrowserUsers } from "../src/hub/auth/config";
import { SUPPORTED_SCHEMA_VERSION, inspectDatabase, schemaVersionOf } from "../src/operations/database";
import {
  activateRelease,
  installRelease,
  packageRelease,
  rollbackRelease,
  type ReleaseManifest,
} from "../src/operations/release";

const ROOT = mkdtempSync(join(tmpdir(), "rigscope-ops-release-"));
process.on("exit", () => rmSync(ROOT, { recursive: true, force: true }));

function freshDir(name: string): string {
  const path = join(ROOT, `${name}-${Math.random().toString(36).slice(2)}`);
  mkdirSync(path, { recursive: true, mode: 0o700 });
  return path;
}

function write(path: string, content: string, mode = 0o644): void {
  mkdirSync(join(path, ".."), { recursive: true, mode: 0o755 });
  writeFileSync(path, content, { mode });
  chmodSync(path, mode);
}

function buildBuiltSource(version: string): string {
  const source = freshDir(`source-${version}`);
  write(join(source, "dist", "hub.cjs"), "// fixture hub\n");
  write(join(source, "dist", "collector.cjs"), "// fixture collector\n");
  write(join(source, "dist", "ops.cjs"), "// fixture ops\n");
  write(join(source, ".next", "BUILD_ID"), "fixture-build-id\n");
  write(join(source, ".next", "server", "app.js"), "// fixture next server\n");
  write(join(source, ".next", "cache", "private-cache.txt"), "cache must not ship\n");
  write(
    join(source, "package.json"),
    `${JSON.stringify(
      {
        name: "rigscope-fixture",
        version,
        private: true,
        dependencies: { next: "15.0.0", ws: "8.18.0" },
      },
      null,
      2,
    )}\n`,
  );
  write(join(source, "package-lock.json"), '{"name":"rigscope-fixture","version":"1.0.0","lockfileVersion":3,"packages":{}}\n');
  write(join(source, "next.config.ts"), "export default {};\n");
  write(join(source, "tsconfig.json"), '{"compilerOptions":{}}\n');
  write(join(source, "README.md"), "# Fixture release\n");
  write(join(source, "docs", "operations.md"), "# Operations fixture\n");
  write(join(source, "packaging", "hub.env.example"), "# public fixture example\n");
  write(join(source, "public", "favicon.ico"), "fixture\n");
  // These must never reach the release archive.
  write(join(source, ".env"), "HUB_BROWSER_USERS=private-secret\n", 0o600);
  write(join(source, "hub-config.json"), '{"passwordHash":"private"}\n', 0o600);
  write(join(source, "state.sqlite"), "private database\n", 0o600);
  write(join(source, ".git", "config"), "private git config\n", 0o600);
  write(join(source, "src", "private.ts"), "// source not shipped\n");
  write(join(source, "node_modules", "next", "package.json"), '{"name":"next","version":"15.0.0"}\n');
  write(join(source, "node_modules", "ws", "package.json"), '{"name":"ws","version":"8.18.0"}\n');
  return source;
}

function archiveEntries(archive: string): string[] {
  const result = spawnSync("tar", ["-tzf", archive], {
    encoding: "utf8",
    env: { ...process.env, LC_ALL: "C" },
  });
  assert.equal(result.status, 0, result.stderr);
  return result.stdout.split("\n").filter(Boolean);
}

function addProductionDependencies(releaseDir: string, manifest: ReleaseManifest): void {
  for (const dependency of manifest.productionDependencies) {
    const directory = join(releaseDir, "node_modules", dependency);
    mkdirSync(directory, { recursive: true, mode: 0o755 });
    writeFileSync(join(directory, "package.json"), JSON.stringify({ name: dependency, version: "0.0.0" }));
  }
}

function readReleaseManifest(releaseDir: string): ReleaseManifest {
  return JSON.parse(readFileSync(join(releaseDir, "manifest.json"), "utf8")) as ReleaseManifest;
}

function patchReleaseSchema(releaseDir: string, schemaVersion: number): void {
  const manifest = readReleaseManifest(releaseDir);
  manifest.schemaVersion = schemaVersion;
  writeFileSync(join(releaseDir, "manifest.json"), `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o644 });
}

function seedDatabaseAt(path: string, version: number): string {
  mkdirSync(join(path, ".."), { recursive: true, mode: 0o700 });
  const db = new DatabaseSync(path);
  try {
    db.exec(`CREATE TABLE schema_migrations (
      version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at_ms INTEGER NOT NULL
    )`);
    const insert = db.prepare("INSERT INTO schema_migrations VALUES (?, ?, ?)");
    for (const migration of MIGRATIONS.filter((item) => item.version <= version)) {
      migration.up(db);
      insert.run(migration.version, migration.name, 1_000 + migration.version);
    }
  } finally {
    db.close();
  }
  chmodSync(path, 0o600);
  return path;
}

function readState(stateDir: string): {
  currentVersion: string | null;
  previousVersion: string | null;
  backups: Record<string, { path: string; schemaVersion: number }>;
} {
  return JSON.parse(readFileSync(join(stateDir, "install-state.json"), "utf8"));
}

test("package archive excludes private/cache/source files and records a verifiable manifest", async () => {
  const source = buildBuiltSource("1.0.0");
  const output = join(freshDir("package-output"), "rigscope-release.tar.gz");
  const report = await packageRelease({ source, output });
  assert.equal(report.manifest.version, "1.0.0");
  assert.equal(report.manifest.schemaVersion, SUPPORTED_SCHEMA_VERSION);
  assert.equal(report.manifest.nodeMajor, 26);
  assert.ok(report.manifest.artifacts.some((artifact) => artifact.path === "dist/ops.cjs"));
  assert.ok(report.manifest.artifacts.some((artifact) => artifact.path === ".next/BUILD_ID"));
  assert.ok(report.manifest.artifacts.some((artifact) => artifact.path === "next.config.js"));
  assert.equal(report.manifest.artifacts.some((artifact) => artifact.path === "next.config.ts"), false);
  // Next requires the actual config value from CommonJS, not an ES module wrapper.
  const exported = report.manifest.artifacts.find(artifact => artifact.path === "next.config.js");
  assert.ok(exported);
  const extractedConfig = spawnSync("tar", ["-xOf", output, "./next.config.js"], { encoding: "utf8" });
  assert.equal(extractedConfig.status, 0, extractedConfig.stderr);
  const configCheck = spawnSync(process.execPath, ["-e", extractedConfig.stdout +
    "\nif (module.exports.default || module.exports.__esModule) process.exit(2);"], { encoding: "utf8" });
  assert.equal(configCheck.status, 0, configCheck.stderr);

  assert.ok(report.manifest.productionDependencies.includes("next"));
  const paths = report.manifest.artifacts.map((artifact) => artifact.path);
  for (const forbidden of [
    ".env",
    "hub-config.json",
    "state.sqlite",
    ".git/config",
    "src/private.ts",
    "node_modules/next/package.json",
    ".next/cache/private-cache.txt",
  ]) {
    assert.equal(paths.includes(forbidden), false, `${forbidden} must not be packaged`);
  }
  const entries = archiveEntries(output);
  assert.ok(entries.some((entry) => entry.replace(/^\.\//, "") === "manifest.json"));
  assert.ok(entries.some((entry) => entry.includes("docs/operations.md")));
  assert.equal(entries.some((entry) => entry.replace(/^\.\//, "").replace(/\/+$/, "") === ".env"), false);
  assert.ok(statSync(output).size > 0);
});

test("install/package reject a missing built artifact", async () => {
  const source = buildBuiltSource("1.0.1");
  rmSync(join(source, "dist", "ops.cjs"));
  await assert.rejects(
    packageRelease({ source, output: join(freshDir("missing-out"), "release.tar.gz") }),
    /required built artifact is missing/,
  );
});

test("install rejects a tampered artifact against the release manifest", async () => {
  const source = buildBuiltSource("1.0.2");
  const archive = join(freshDir("tamper-package"), "release.tar.gz");
  await packageRelease({ source, output: archive });
  const extracted = freshDir("tamper-extracted");
  const result = spawnSync("tar", ["-xzf", archive, "-C", extracted], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  writeFileSync(join(extracted, "dist", "hub.cjs"), "// tampered after packaging\n");
  const root = freshDir("tamper-root");
  await assert.rejects(
    installRelease({
      source: extracted,
      prefix: join(root, "prefix"),
      stateDir: join(root, "state"),
      unitsDir: join(root, "units"),
      database: join(root, "state", "hub", "telemetry.sqlite"),
    }),
    /hash changed|size changed|artifact/i,
  );
});

test("install stages units and private env without activating or running systemctl", async () => {
  const source = buildBuiltSource("1.1.0");
  const output = join(freshDir("stage-package"), "release.tar.gz");
  await packageRelease({ source, output });
  const root = freshDir("stage root with spaces");
  const prefix = join(root, "opt with spaces");
  const stateDir = join(root, "state with spaces");
  const unitsDir = join(root, "units with spaces");
  const database = join(stateDir, "hub", "telemetry.sqlite");
  const nodePath = join(root, "node bin", "node26");
  mkdirSync(join(root, "node bin"), { recursive: true });
  copyFileSync(process.execPath, nodePath);
  const report = await installRelease({
    source: output,
    prefix,
    stateDir,
    unitsDir,
    database,
    nodePath,
  });
  assert.equal(report.activated, false);
  assert.equal(report.dependencyState, "missing");
  assert.equal(existsSync(join(prefix, "current")), false);
  assert.equal(existsSync(join(prefix, "releases", "1.1.0", "manifest.json")), true);
  for (const path of [report.hubEnv, report.collectorEnv, report.units[0]!, report.units[1]!]) {
    assert.equal(existsSync(path), true);
  }
  assert.equal(statSync(report.hubEnv).mode & 0o777, 0o600);
  assert.equal(statSync(report.collectorEnv).mode & 0o777, 0o600);
  const hubEnv = readFileSync(report.hubEnv, "utf8");
  assert.equal(
    hubEnv.split("\n").filter((line) => line.trim() !== "" && !line.trim().startsWith("#")).length,
    0,
    "empty generated hub config fails closed",
  );
  const collectorEnv = readFileSync(report.collectorEnv, "utf8");
  assert.equal(
    collectorEnv.split("\n").filter((line) => line.trim() !== "" && !line.trim().startsWith("#")).length,
    0,
  );
  const hubUnit = readFileSync(report.units[0]!, "utf8");
  assert.equal(report.units[0], join(unitsDir, "rigscope-hub.service"));
  assert.equal(report.units[1], join(unitsDir, "rigscope-collector.service"));
  assert.match(hubUnit, /Restart=on-failure/);
  assert.match(hubUnit, /NoNewPrivileges=yes/);
  assert.doesNotMatch(hubUnit, /^ProtectKernelModules=yes$/m, "target user manager rejects its implied capability drop with exit 218");
  assert.match(hubUnit, /PrivateTmp=yes/);
  assert.match(hubUnit, /UMask=0077/);
  assert.match(hubUnit, /TimeoutStopSec=15s/);
  assert.match(hubUnit, /WorkingDirectory="[^"]*opt with spaces\/current"/);
  assert.match(hubUnit, /EnvironmentFile="[^"]*state with spaces\/hub\.env"/);
  assert.match(hubUnit, /ExecStart="[^"]*node bin\/node26"/);
  assert.match(hubUnit, /HUB_DATABASE_PATH=/);
  assert.match(hubUnit, /ReadWritePaths=.*state with spaces/);
  assert.doesNotMatch(hubUnit, /ExecStart=.*systemctl/);
  const collectorUnit = readFileSync(report.units[1]!, "utf8");
  assert.match(collectorUnit, /TimeoutStopSec=10s/);
  assert.match(collectorUnit, /COLLECTOR_STATE_FILE=/);
  assert.ok(report.activationCommands.some((line) => line.includes("systemctl --user enable")));
  assert.equal(existsSync(database), false, "staging never creates the database");
});

test("an empty hub environment fails closed for browser and collector access", () => {
  const config = parseHubConfig({});
  assert.equal(config.collectors.length, 0);
  assert.deepEqual(parseBrowserUsers({}), []);
});

test("activation refuses missing production dependencies before touching the database", async () => {
  const source = buildBuiltSource("1.2.0");
  const output = join(freshDir("dep-package"), "release.tar.gz");
  await packageRelease({ source, output });
  const root = freshDir("dep root");
  const prefix = join(root, "prefix");
  const stateDir = join(root, "state");
  const unitsDir = join(root, "units");
  const database = join(stateDir, "hub", "telemetry.sqlite");
  const staged = await installRelease({ source: output, prefix, stateDir, unitsDir, database });
  await assert.rejects(
    activateRelease({
      prefix,
      stateDir,
      database,
      version: staged.version,
      confirmStopped: true,
    }),
    /production dependencies are missing/,
  );
  assert.equal(existsSync(join(prefix, "current")), false);
  assert.equal(existsSync(database), false);
});

test("first activation creates a private database and records the release", async () => {
  const source = buildBuiltSource("1.3.0");
  const output = join(freshDir("first-package"), "release.tar.gz");
  await packageRelease({ source, output });
  const root = freshDir("first root");
  const prefix = join(root, "prefix");
  const stateDir = join(root, "state");
  const unitsDir = join(root, "units");
  const database = join(stateDir, "hub", "telemetry.sqlite");
  const staged = await installRelease({ source: output, prefix, stateDir, unitsDir, database });
  const manifest = readReleaseManifest(staged.releaseDir);
  addProductionDependencies(staged.releaseDir, manifest);
  const activation = await activateRelease({
    prefix,
    stateDir,
    database,
    version: staged.version,
    confirmStopped: true,
  });
  assert.equal(activation.version, "1.3.0");
  assert.equal(activation.previous, null);
  assert.equal(schemaVersionOf(database), SUPPORTED_SCHEMA_VERSION);
  assert.equal(inspectDatabase(database).integrityOk, true);
  const state = readState(stateDir);
  assert.equal(state.currentVersion, "1.3.0");
  assert.equal(state.previousVersion, null);
  assert.equal(statSync(database).mode & 0o777, 0o600);
  const currentTarget = readlinkSync(join(prefix, "current"));
  assert.equal(currentTarget, staged.releaseDir);
  await assert.rejects(
    rollbackRelease({ prefix, stateDir, database, confirmStopped: true }),
    /no previous release/,
  );
});

test("schema-changing upgrade backs up, migrates, and rollback restores the matching database", async () => {
  const root = freshDir("upgrade root with spaces");
  const prefix = join(root, "opt with spaces");
  const stateDir = join(root, "state with spaces");
  const unitsDir = join(root, "units with spaces");
  const database = join(stateDir, "hub", "telemetry.sqlite");
  const nodePath = join(root, "node bin", "node26");
  mkdirSync(join(root, "node bin"), { recursive: true });
  copyFileSync(process.execPath, nodePath);

  // v1 is an older release whose manifest supports schema 4. The existing DB is
  // also schema 4, so activating it is a no-migration baseline.
  const v1Source = buildBuiltSource("1.0.0");
  const v1Archive = join(freshDir("v1-package"), "release.tar.gz");
  await packageRelease({ source: v1Source, output: v1Archive });
  seedDatabaseAt(database, 4);
  const v1Install = await installRelease({
    source: v1Archive,
    prefix,
    stateDir,
    unitsDir,
    database,
    nodePath,
  });
  patchReleaseSchema(v1Install.releaseDir, 4);
  addProductionDependencies(v1Install.releaseDir, readReleaseManifest(v1Install.releaseDir));
  const v1Activation = await activateRelease({
    prefix,
    stateDir,
    database,
    version: "1.0.0",
    confirmStopped: true,
  });
  assert.equal(v1Activation.previous, null);
  assert.equal(schemaVersionOf(database), 4);

  const v2Source = buildBuiltSource("2.0.0");
  const v2Archive = join(freshDir("v2-package"), "release.tar.gz");
  await packageRelease({ source: v2Source, output: v2Archive });
  const v2Install = await installRelease({
    source: v2Archive,
    prefix,
    stateDir,
    unitsDir,
    database,
    nodePath,
  });
  addProductionDependencies(v2Install.releaseDir, readReleaseManifest(v2Install.releaseDir));

  const v2Activation = await activateRelease({
    prefix,
    stateDir,
    database,
    version: "2.0.0",
    confirmStopped: true,
  });
  assert.equal(v2Activation.schemaFrom, 4);
  assert.equal(v2Activation.schemaTo, SUPPORTED_SCHEMA_VERSION);
  assert.ok(v2Activation.backupPath !== null);
  assert.equal(existsSync(v2Activation.backupPath!), true);
  assert.equal(schemaVersionOf(database), SUPPORTED_SCHEMA_VERSION);
  let state = readState(stateDir);
  assert.equal(state.currentVersion, "2.0.0");
  assert.equal(state.previousVersion, "1.0.0");
  assert.ok(state.backups["2.0.0"]!.path.length > 0);

  // An old release cannot be activated over the newer database.
  await assert.rejects(
    activateRelease({ prefix, stateDir, database, version: "1.0.0", confirmStopped: true }),
    /newer schema|newer than/i,
  );
  assert.equal(readlinkSync(join(prefix, "current")), v2Install.releaseDir);

  // Rollback restores the schema-4 backup before switching to release v1.
  const rollback = await rollbackRelease({ prefix, stateDir, database, confirmStopped: true });
  assert.equal(rollback.version, "1.0.0");
  assert.equal(rollback.schemaFrom, SUPPORTED_SCHEMA_VERSION);
  assert.equal(rollback.schemaTo, 4);
  assert.ok(rollback.restoredBackup !== null);
  assert.equal(schemaVersionOf(database), 4);
  state = readState(stateDir);
  assert.equal(state.currentVersion, "1.0.0");
  assert.equal(state.previousVersion, "2.0.0");
  const currentTarget = readlinkSync(join(prefix, "current"));
  assert.equal(currentTarget, v1Install.releaseDir);

  // A failed upgrade restores the good schema-4 database and leaves current v1.
  await assert.rejects(
    activateRelease({
      prefix,
      stateDir,
      database,
      version: "2.0.0",
      confirmStopped: true,
      runMigrations: () => {
        throw new Error("synthetic upgrade failure");
      },
    }),
    /synthetic upgrade failure|migration failed/i,
  );
  assert.equal(schemaVersionOf(database), 4);
  assert.equal(readlinkSync(join(prefix, "current")), v1Install.releaseDir);
  state = readState(stateDir);
  assert.equal(state.currentVersion, "1.0.0");
});

test("installation rejects an explicitly selected mismatched Node executable", async () => {
  const source = buildBuiltSource("1.9.0");
  const archive = join(freshDir("wrong-node-package"), "release.tar.gz");
  await packageRelease({ source, output: archive });
  const root = freshDir("wrong-node");
  const nodePath = join(root, "node20");
  write(nodePath, "#!/bin/sh\nprintf '20.0.0'\n", 0o755);
  await assert.rejects(installRelease({ source: archive, prefix: join(root,"prefix"),
    stateDir: join(root,"state"), unitsDir: join(root,"units"), nodePath }), /must run Node 26/);
  assert.equal(existsSync(join(root, "prefix", "current")), false);
});
