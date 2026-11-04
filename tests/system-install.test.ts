/**
 * System-scope installation, adoption, lifecycle and RAPL privilege-boundary
 * fixtures. No test runs sudo, useradd, systemctl, udevadm or npm. Every
 * mutation uses temporary directories and a recording command runner with a
 * fixture uid of 0; a separate case proves a non-root apply fails closed.
 */
import assert from "node:assert/strict";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { createServer } from 'node:http';
import { spawnSync } from 'node:child_process';
import test from "node:test";
import {
  DEFAULT_SYSTEM_ACCOUNTS,
  SystemError,
  collectorStatePath,
  connectSystem,
  createSystemHost,
  defaultSystemPaths,
  detectUserInstallations,
  disableSystem,
  enableSystem,
  grantPower,
  hubDatabasePath,
  hubEnvPath,
  inspectPowerAccess,
  installSystem,
  powerSysusersPath,
  raplRulePath,
  renderPowerSysusers,
  renderRaplRule,
  renderSysusers,
  renderSystemCollectorUnit,
  renderSystemHubUnit,
  revokePower,
  rollbackSystem,
  startSystem,
  stopSystem,
  stateFilePath,
  sysusersPath,
  systemStatus,
  uninstallSystem,
  upgradeSystem,
  validateAccounts,
  validateSystemPaths,
  verifySystemHealth,
  readSystemState,
  type SystemHost,
  type SystemPaths,
} from "../src/cli/system";

const ROOT = mkdtempSync(join(tmpdir(), "rigscope-system-fixtures-"));
process.on("exit", () => rmSync(ROOT, { recursive: true, force: true }));

let sequence = 0;
function freshRoot(name: string): string {
  sequence += 1;
  const path = join(ROOT, `${name}-${sequence}`);
  mkdirSync(path, { recursive: true });
  return path;
}

function write(path: string, content: string, mode = 0o644): void {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, content, { mode });
  chmodSync(path, mode);
}

function pathsFor(root: string): SystemPaths {
  return defaultSystemPaths({
    prefix: join(root, "opt", "rigscope"),
    configDir: join(root, "etc", "rigscope"),
    stateDir: join(root, "var", "rigscope"),
    unitsDir: join(root, "etc", "systemd", "system"),
    sysusersDir: join(root, "etc", "sysusers.d"),
    udevRulesDir: join(root, "etc", "udev", "rules.d"),
    powercapRoot: join(root, "powercap"),
  });
}

type FakeHost = { host: SystemHost; commands: string[][]; active: Set<string>; enabled: Set<string> };
function fakeHost(options: { uid?: number; active?: string[]; enabled?: boolean } = {}): FakeHost {
  const commands: string[][] = [];
  const active = new Set(options.active ?? []);
  const enabled = new Set<string>();
  const host: SystemHost = {
    uid: () => options.uid ?? 0,
    now: () => Date.now(),
    run: (command, args, runOptions) => {
      commands.push([command, ...args]);
      if (command === 'getent') {
        const users = ['rigscope-hub:x:1400:1400:hub:/nonexistent:/usr/sbin/nologin','rigscope-collector:x:1401:1401:collector:/nonexistent:/usr/sbin/nologin'];
        return {status:0,stderr:'',stdout:args[0] === 'group' ? 'rigscope-rapl:x:1500:rigscope-collector\n' : users.filter(line => !args[1] || line.startsWith(args[1]+':')).join('\n')+'\n'};
      }
      if (command === "systemctl" && args[0] === "is-active") {
        return { status: active.has(args[2] ?? "") ? 0 : 1, stdout: "", stderr: "" };
      }
      if (command === "systemctl" && args[0] === "is-enabled") {
        return { status: enabled.has(args[2] ?? "") ? 0 : 1, stdout: "", stderr: "" };
      }
      if (command === 'systemctl' && args[0] === 'start') active.add(args[1]!);
      if (command === 'systemctl' && args[0] === 'stop') active.delete(args[1]!);
      if (command === 'systemctl' && args[0] === 'enable') enabled.add(args[1]!);
      if (command === 'systemctl' && args[0] === 'disable') enabled.delete(args[1]!);
      if (command === 'runuser') {
        const stateFile = runOptions?.env?.COLLECTOR_STATE_FILE;
        const destination = runOptions?.env?.COLLECTOR_ENROLL_URL;
        if (stateFile && destination) {
          write(stateFile, `${JSON.stringify({version:1,collectorId:'connected-collector',destinations:[{url:destination,token:'fixture-connected-token',sharing:{cpu:true,memory:true}}]})}\n`, 0o600);
        }
        return { status: 0, stdout: 'collector enrollment saved\n', stderr: '' };
      }
      if (args[0] === "--version") return { status: 0, stdout: "v26.13.0\n", stderr: "" };
      return { status: 0, stdout: "", stderr: "" };
    },
  };
  return { host, commands, active, enabled };
}

function fixtureNode(root: string): string {
  const node = join(root, "fixture-node");
  write(node, "#!/bin/sh\necho v26.13.0\n", 0o755);
  return node;
}

function fixtureRelease(root: string, version: string, schemaVersion = 1): string {
  const source = join(root, `release-${version}`);
  write(join(source, "package.json"), `${JSON.stringify({ name: "rigscope", version, dependencies: {} })}\n`);
  write(join(source, "manifest.json"), `${JSON.stringify({ name: "rigscope", version, schemaVersion })}\n`);
  for (const entry of ["hub", "collector", "ops", "cli"]) write(join(source, "dist", `${entry}.cjs`), `// ${entry} ${version}\n`);
  write(join(source, ".next", "BUILD_ID"), "fixture-build\n");
  write(join(source, ".next", "server", "app.js"), "// server\n");
  write(join(source, ".next", "static", "app.js"), "// static\n");
  write(join(source, "README.md"), "fixture release\n");
  return source;
}

function manifestize(source: string): void {
  const artifacts: Array<{ path: string; bytes: number; sha256: string }> = [];
  const visit = (directory: string): void => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const absolute = join(directory, entry.name);
      if (entry.isDirectory()) {
        visit(absolute);
        continue;
      }
      const relative = absolute.slice(source.length + 1).split("\\").join("/");
      if (relative === "manifest.json") continue;
      const content = readFileSync(absolute);
      artifacts.push({ path: relative, bytes: content.byteLength, sha256: createHash("sha256").update(content).digest("hex") });
    }
  };
  visit(source);
  write(join(source, "manifest.json"), `${JSON.stringify({ name: "rigscope", version: "1.0.0", schemaVersion: 1, artifacts })}\n`);
}

function createDatabase(path: string, schema: number): void {
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
  const db = new DatabaseSync(path);
  try {
    db.exec("CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL)");
    db.prepare("INSERT OR REPLACE INTO schema_migrations (version, name) VALUES (?, ?)").run(schema, "fixture");
  } finally {
    db.close();
  }
  chmodSync(path, 0o600);
}

function createHubRegistry(path: string): void {
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
  const db = new DatabaseSync(path);
  try {
    db.exec(`
      CREATE TABLE IF NOT EXISTS collector_registry (
        collector_id TEXT PRIMARY KEY, machine_id TEXT NOT NULL UNIQUE, token TEXT NOT NULL UNIQUE,
        revoked INTEGER NOT NULL DEFAULT 0, created_at_ms INTEGER NOT NULL, updated_at_ms INTEGER NOT NULL
      );
      CREATE TABLE IF NOT EXISTS collector_registry_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS collector_grants (
        token_hash TEXT PRIMARY KEY, expires_at_ms INTEGER NOT NULL, replace_machine_id TEXT,
        consumed_by TEXT, request_id TEXT, recovery_hash TEXT, recovery_until_ms INTEGER, actor TEXT NOT NULL
      );
      CREATE TABLE IF NOT EXISTS collector_audit (
        id INTEGER PRIMARY KEY, at_ms INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL
      );
    `);
  } finally {
    db.close();
  }
  chmodSync(path, 0o600);
}

function setEnvLine(path: string, key: string, value: string): void {
  const lines = readFileSync(path, "utf8").split("\n").filter((line) => !line.startsWith(`${key}=`));
  lines.push(`${key}=${JSON.stringify(value)}`);
  write(path, `${lines.join("\n").replace(/\n+$/, "")}\n`, 0o600);
}

async function startEnrollmentServer(): Promise<{
  url: string;
  requests: Array<{ grant: string; collectorId: string }>;
  close: () => Promise<void>;
}> {
  const requests: Array<{ grant: string; collectorId: string }> = [];
  const server = createServer(async (req, res) => {
    if (req.method !== "POST" || req.url !== "/api/collector/enroll") {
      res.writeHead(404).end();
      return;
    }
    let body = "";
    for await (const chunk of req) body += chunk.toString();
    const parsed = JSON.parse(body) as { collectorId: string };
    const grant = /^Bearer (.+)$/.exec(req.headers.authorization ?? "")?.[1] ?? "";
    requests.push({ grant, collectorId: parsed.collectorId });
    res.writeHead(201, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ collectorId: parsed.collectorId, machineId: `machine-${requests.length}`, token: `issued-token-${requests.length}` }));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  assert.ok(address && typeof address === "object");
  return {
    url: `http://127.0.0.1:${address.port}`,
    requests,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

function createPowercap(root: string, options: { energyMode?: number; packageName?: string } = {}): string {
  const domain = join(root, "intel-rapl:0");
  mkdirSync(domain, { recursive: true });
  write(join(domain, "name"), `${options.packageName ?? "package-0"}\n`);
  write(join(domain, "energy_uj"), "1000000\n", options.energyMode ?? 0o440);
  write(join(domain, "max_energy_range_uj"), "65532610987\n", 0o440);
  write(join(root, "intel-rapl:0:0", "name"), "core\n");
  write(join(root, "intel-rapl:0:0", "energy_uj"), "500000\n", 0o444);
  return domain;
}

function writeIdentityFixtures(root: string, options: { groupMembers?: boolean } = {}): { passwd: string; group: string } {
  const passwd = join(root, "passwd");
  const group = join(root, "group");
  write(passwd, [
    "root:x:0:0:root:/root:/bin/bash",
    "rigscope-hub:x:1400:1400:Rigscope hub:/nonexistent:/usr/sbin/nologin",
    "rigscope-collector:x:1401:1401:Rigscope collector:/nonexistent:/usr/sbin/nologin",
    "",
  ].join("\n"));
  write(group, [
    "root:x:0:",
    "rigscope-hub:x:1400:",
    "rigscope-collector:x:1401:",
    `rigscope-rapl:x:1500:${options.groupMembers === false ? "" : "rigscope-collector"}`,
    "",
  ].join("\n"));
  return { passwd, group };
}

async function installFixture(options: {
  root: string;
  version?: string;
  schemaVersion?: number;
  role?: "local" | "hub" | "collector";
  adoptFrom?: string;
  homeRoots?: string[];
  host?: FakeHost;
  enable?: boolean;
  start?: boolean;
  power?: boolean;
}): Promise<{ report: Awaited<ReturnType<typeof installSystem>>; paths: SystemPaths; host: FakeHost; source: string }> {
  const paths = pathsFor(options.root);
  const host = options.host ?? fakeHost();
  const source = fixtureRelease(options.root, options.version ?? "1.0.0", options.schemaVersion ?? 1);
  const report = await installSystem({
    apply: true,
    source,
    role: options.role ?? "local",
    paths,
    host: host.host,
    nodePath: fixtureNode(options.root),
    trustedNode: true,
    adoptFrom: options.adoptFrom,
    homeRoots: options.homeRoots ?? [],
    start: options.start ?? false,
    enable: options.enable ?? true,
    power: options.power,
  });
  return { report, paths, host, source };
}

test("system units run as dedicated unprivileged accounts without capabilities", () => {
  const paths = defaultSystemPaths();
  const accounts = { ...DEFAULT_SYSTEM_ACCOUNTS };
  const hub = renderSystemHubUnit({
    paths,
    accounts,
    nodePath: "/opt/rigscope/node/node",
    database: "/var/lib/rigscope/hub/telemetry.sqlite",
    enrollment: "/var/lib/rigscope/hub/enrollment.json",
    socket: "/var/lib/rigscope/hub/run/hub.sock",
    remote: true,
    powerAccess: false,
  });
  const collector = renderSystemCollectorUnit({
    paths,
    accounts,
    nodePath: "/opt/rigscope/node/node",
    collectorState: "/var/lib/rigscope/collector/state.json",
    powerAccess: true,
  });
  for (const [name, unit, user] of [["hub", hub, "rigscope-hub"], ["collector", collector, "rigscope-collector"]] as const) {
    const [servicePart, installPart = ""] = unit.split("[Install]");
    assert.match(servicePart!, new RegExp(`^User=${user}$`, "m"), `${name} must use its own account`);
    assert.match(servicePart!, new RegExp(`^Group=${user}$`, "m"));
    assert.doesNotMatch(unit, /^User=root$/m);
    assert.doesNotMatch(unit, /systemctl --user/);
    assert.match(servicePart!, /^NoNewPrivileges=yes$/m);
    assert.match(servicePart!, /^ProtectSystem=strict$/m);
    assert.match(servicePart!, /^ProtectHome=yes$/m);
    assert.match(servicePart!, /^CapabilityBoundingSet=$/m);
    assert.match(servicePart!, /^AmbientCapabilities=$/m);
    assert.doesNotMatch(servicePart!, /AmbientCapabilities=CAP_/);
    assert.match(servicePart!, /^EnvironmentFile=\/etc\/rigscope\/[a-z]+\.env$/m);
    assert.match(servicePart!, /^ReadWritePaths=/m, `${name} writable paths must stay in [Service]`);
    assert.match(installPart, /^WantedBy=multi-user\.target$/m, "system scope must boot without lingering");
    assert.doesNotMatch(installPart, /default\.target/);
    assert.match(servicePart!, /^ExecStart="\/opt\/rigscope\/node\/node" /m);
  }
  assert.match(collector, /^SupplementaryGroups=rigscope-rapl$/m);
  assert.doesNotMatch(hub, /^SupplementaryGroups=/m);
  assert.match(hub, /^PrivateDevices=yes$/m);
  assert.doesNotMatch(collector, /^PrivateDevices=yes$/m, "collector still needs real GPU/device telemetry");
});

test("the RAPL rule grants only top-level package energy and range counters", () => {
  const accounts = { ...DEFAULT_SYSTEM_ACCOUNTS };
  const rule = renderRaplRule(accounts);
  assert.match(rule, /SUBSYSTEM=="powercap"/);
  assert.match(rule, /KERNEL=="intel-rapl:\[0-9\]\*"/);
  assert.match(rule, /ATTR\{name\}=="package\*"/);
  assert.match(rule, /chgrp rigscope-rapl \/sys%p\/energy_uj/);
  assert.match(rule, /chgrp rigscope-rapl \/sys%p\/max_energy_range_uj/);
  assert.match(rule, /chmod 0440 \/sys%p\/energy_uj/);
  assert.doesNotMatch(rule, /chmod 0644/);
  assert.doesNotMatch(rule, /chmod -R/);
  assert.doesNotMatch(rule, /chmod g\+/);
  assert.doesNotMatch(rule, /constraint_/);
  assert.doesNotMatch(rule, /power_limit/);
  assert.doesNotMatch(rule, /intel-rapl:\[0-9\]\*:\[0-9\]\*/, "subdomains must not receive the rule");
  assert.doesNotMatch(rule, /capability/i);

  const sysusers = renderPowerSysusers(defaultSystemPaths(), accounts);
  assert.match(sysusers, /^g rigscope-rapl - -$/m);
  assert.match(sysusers, /^m rigscope-collector rigscope-rapl$/m);
  assert.doesNotMatch(sysusers, /^m rigscope-hub/m);
  const users = renderSysusers(defaultSystemPaths(), accounts, "local");
  assert.match(users, /^u rigscope-hub - .* \/usr\/sbin\/nologin$/m);
  assert.match(users, /^u rigscope-collector - .* \/usr\/sbin\/nologin$/m);
  assert.doesNotMatch(users, /root/);
});

test("RAPL inspection distinguishes narrow group access from world-readable or absent counters", () => {
  const root = freshRoot("rapl");
  const paths = pathsFor(root);
  const domain = createPowercap(paths.powercapRoot);
  const identity = writeIdentityFixtures(root);
  write(join(root, "etc", "udev", "rules.d", "90-rigscope-rapl.rules"), renderRaplRule(DEFAULT_SYSTEM_ACCOUNTS));
  write(join(root, "etc", "sysusers.d", "rigscope-rapl.conf"), renderPowerSysusers(paths, DEFAULT_SYSTEM_ACCOUNTS));
  mkdirSync(join(root, "modules", "intel_rapl_msr"), { recursive: true });
  mkdirSync(join(root, "modules", "intel_rapl_common"), { recursive: true });
  const statOverride = (path: string): { mode: number; uid: number; gid: number } | null => {
    if (!existsSync(path)) return null;
    if (path.endsWith("energy_uj") && path.includes("intel-rapl:0")) {
      const mode = statSync(path).mode & 0o777;
      return { mode, uid: 0, gid: 1500 };
    }
    if (path.endsWith("max_energy_range_uj") && path.includes("intel-rapl:0")) {
      return { mode: 0o440, uid: 0, gid: 1500 };
    }
    return null;
  };
  const good = inspectPowerAccess({
    paths,
    passwdFile: identity.passwd,
    groupFile: identity.group,
    moduleRoot: join(root, "modules"),
    rulePath: join(root, "etc", "udev", "rules.d", "90-rigscope-rapl.rules"),
    sysusersPath: join(root, "etc", "sysusers.d", "rigscope-rapl.conf"),
    statOverride,
  });
  assert.equal(good.domains.length, 1, "subdomain counters are ignored");
  assert.equal(good.domains[0]!.name, "package-0");
  assert.equal(good.domains[0]!.energy?.mode, "0440");
  assert.equal(good.domains[0]!.energy?.worldReadable, false);
  assert.equal(good.domains[0]!.energy?.writable, false);
  assert.equal(good.collectorCanRead, true);
  assert.equal(good.persistent, true);
  assert.deepEqual(good.driver, ["intel_rapl_msr", "intel_rapl_common"]);

  chmodSync(join(domain, "energy_uj"), 0o644);
  const loose = inspectPowerAccess({
    paths,
    passwdFile: identity.passwd,
    groupFile: identity.group,
    moduleRoot: join(root, "modules"),
    rulePath: join(root, "etc", "udev", "rules.d", "90-rigscope-rapl.rules"),
    sysusersPath: join(root, "etc", "sysusers.d", "rigscope-rapl.conf"),
    statOverride,
  });
  assert.equal(loose.worldReadable, true);
  assert.ok(loose.problems.some((problem) => /world-readable/.test(problem)));

  chmodSync(join(domain, "energy_uj"), 0o440);
  const identityWithoutMembership = writeIdentityFixtures(root, { groupMembers: false });
  const denied = inspectPowerAccess({
    paths,
    passwdFile: identityWithoutMembership.passwd,
    groupFile: identityWithoutMembership.group,
    moduleRoot: join(root, "modules"),
    rulePath: join(root, "etc", "udev", "rules.d", "90-rigscope-rapl.rules"),
    sysusersPath: join(root, "etc", "sysusers.d", "rigscope-rapl.conf"),
    statOverride,
  });
  assert.equal(denied.collectorCanRead, false);
  assert.equal(denied.persistent, false);
  assert.ok(denied.problems.some((problem) => /not a member/.test(problem)));

  rmSync(join(domain, "energy_uj"));
  const missing = inspectPowerAccess({
    paths,
    passwdFile: identity.passwd,
    groupFile: identity.group,
    moduleRoot: join(root, "modules"),
    rulePath: join(root, "etc", "udev", "rules.d", "90-rigscope-rapl.rules"),
    sysusersPath: join(root, "etc", "sysusers.d", "rigscope-rapl.conf"),
    statOverride,
  });
  assert.equal(missing.domains[0]!.energyExists, false);
  assert.equal(missing.collectorCanRead, false, "an unreadable counter is unavailable, never a fabricated zero");
});

test("system paths and non-root applies fail closed before any mutation", async () => {
  assert.throws(() => validateSystemPaths({ prefix: "/home/triet/rigscope" }), /must not live under/);
  assert.throws(() => validateSystemPaths({ prefix: "/opt/x", stateDir: "/opt/x/state" }), /separate paths/);
  assert.throws(() => validateAccounts({ hub: "root" }), /must not run as root/);
  const root = freshRoot("nonroot");
  const paths = pathsFor(root);
  const source = fixtureRelease(root, "1.0.0");
  const host = fakeHost({ uid: 1000 });
  await assert.rejects(
    installSystem({
      apply: true,
      source,
      role: "local",
      paths,
      host: host.host,
      nodePath: fixtureNode(root),
      trustedNode: true,
      homeRoots: [],
    }),
    /requires root/,
  );
  assert.equal(existsSync(paths.prefix), false, "a denied apply must not create the prefix");
  assert.equal(host.commands.length, 0, "a denied apply must not run commands");
  assert.throws(() => validateSystemPaths({ prefix: "/" }), /filesystem root/);
});

test("existing user units are detected and require explicit adoption", async () => {
  const root = freshRoot("user-conflict");
  const home = join(root, "home", "operator");
  write(
    join(home, ".config", "systemd", "user", "rigscope-hub.service"),
    "[Service]\nExecStart=/home/operator/.local/lib/node /repo/dist/hub.cjs\n",
  );
  mkdirSync(join(home, ".local", "state", "rigscope"), { recursive: true });
  const found = detectUserInstallations({ homeRoots: [join(root, "home")] });
  assert.equal(found.length, 1);
  assert.equal(found[0]!.unitName, "rigscope-hub.service");
  await assert.rejects(
    installSystem({
      apply: true,
      source: fixtureRelease(root, "1.0.0"),
      paths: pathsFor(root),
      host: fakeHost().host,
      nodePath: fixtureNode(root),
      trustedNode: true,
      homeRoots: [join(root, "home")],
    }),
    /existing user service/,
  );
});

test("install stages a root-owned layout, private env files and boot units", async () => {
  const root = freshRoot("install");
  const { report, paths, host } = await installFixture({ root, enable: false });
  assert.equal(report.ok, true);
  assert.equal(report.apply, true);
  assert.equal(report.version, "1.0.0");
  assert.equal(existsSync(join(paths.prefix, "current")), true);
  const activeEnvLines = readFileSync(hubEnvPath(paths), "utf8").split("\n").filter((line) => line !== "" && !line.startsWith("#"));
  assert.equal(activeEnvLines.some((line) => line.startsWith("HUB_BROWSER_USERS=")), false, "fresh config fails closed");
  assert.equal(statSync(hubEnvPath(paths)).mode & 0o777, 0o600);
  assert.equal(statSync(join(paths.configDir, "collector.env")).mode & 0o777, 0o600);
  assert.equal(statSync(join(paths.unitsDir, "rigscope-hub.service")).mode & 0o777, 0o644);
  assert.equal(statSync(sysusersPath(paths)).mode & 0o777, 0o644);
  assert.equal(statSync(raplRulePath(paths)).mode & 0o777, 0o644);
  assert.equal(statSync(stateFilePath(paths)).mode & 0o777, 0o600);
  const state = JSON.parse(readFileSync(stateFilePath(paths), "utf8")) as { currentVersion: string; nodePath: string; power: unknown };
  assert.equal(state.currentVersion, "1.0.0");
  assert.match(state.nodePath, /\/opt\/rigscope\/node|\/fixture-node/);
  assert.notEqual(state.power, null);
  const cmds = host.commands.map((command) => command.join(" "));
  assert.ok(cmds.some((command) => command === "systemd-sysusers"));
  assert.ok(cmds.some((command) => command.startsWith("chown -R root:root")));
  assert.ok(cmds.some((command) => command.startsWith("udevadm trigger")));
  assert.ok(cmds.every((command) => !command.includes("--user")));
});

test("lifecycle: install -> start refusal on duplicate runtime -> upgrade -> rollback -> uninstall keeps history", async () => {
  const root = freshRoot("lifecycle");
  const { paths, host } = await installFixture({ root, version: "1.0.0", schemaVersion: 1 });
  const database = hubDatabasePath(paths);
  createDatabase(database, 1);

  // A live lock from another runtime must prevent a duplicate start.
  const lockPath = `${database}.hub.lock`;
  write(lockPath, `${JSON.stringify({ pid: process.pid, instance: "fixture" })}\n`, 0o600);
  await assert.rejects(
    () => startSystem({ apply: true, paths, host: host.host }),
    /already owns/,
  );
  rmSync(lockPath);

  const v2 = fixtureRelease(root, "2.0.0", 2);
  const upgraded = await upgradeSystem({ apply: true, source: v2, paths, host: host.host, start: false });
  assert.equal(upgraded.version, "2.0.0");
  assert.equal(upgraded.previous, join(paths.prefix, "releases", "1.0.0"));
  assert.equal(typeof upgraded.backupPath, "string");
  assert.ok(existsSync(upgraded.backupPath as string), "pre-upgrade backup succeeds");
  const stateAfterUpgrade = JSON.parse(readFileSync(stateFilePath(paths), "utf8")) as { currentVersion: string; previousVersion: string };
  assert.equal(stateAfterUpgrade.currentVersion, "2.0.0");
  assert.equal(stateAfterUpgrade.previousVersion, "1.0.0");

  const rolled = await rollbackSystem({ apply: true, paths, host: host.host, start: false });
  assert.equal(rolled.version, "1.0.0");
  assert.equal(rolled.current, join(paths.prefix, "releases", "1.0.0"));
  assert.ok(existsSync(database), "the database is preserved");

  const uninstalled = uninstallSystem({ apply: true, paths, host: host.host });
  assert.equal(uninstalled.apply, true);
  assert.equal(existsSync(join(paths.unitsDir, "rigscope-hub.service")), false);
  assert.equal(existsSync(join(paths.unitsDir, "rigscope-collector.service")), false);
  assert.equal(existsSync(raplRulePath(paths)), false);
  assert.equal(existsSync(join(paths.prefix, "releases")), false, "default uninstall removes the application");
  assert.equal(existsSync(join(paths.prefix, "current")), false);
  assert.equal(existsSync(database), true, "default uninstall must not purge history");
  assert.equal(existsSync(hubEnvPath(paths)), true, "configuration is retained with the data");
  const afterState = JSON.parse(readFileSync(stateFilePath(paths), "utf8")) as {
    active: boolean;
    components: { hub: { enabled: boolean }; collector: { enabled: boolean } };
  };
  assert.equal(afterState.active, false);
  assert.deepEqual(afterState.components, { hub: { enabled: false }, collector: { enabled: false } });

  assert.throws(() => uninstallSystem({ apply: true, paths, host: host.host, purgeData: true }), /confirm-purge-history/);
  const purged = uninstallSystem({
    apply: true,
    paths,
    host: host.host,
    purgeData: true,
    confirmPurgeHistory: true,
    purgeProgram: true,
  });
  assert.equal(purged.apply, true);
  assert.equal(existsSync(database), false);
  assert.equal(existsSync(paths.stateDir), false);
});

test("adoption is copy-only, refuses live owners, and leaves the user installation untouched", async () => {
  const root = freshRoot("adoption");
  const home = join(root, "home", "operator");
  const userState = join(home, ".local", "state", "rigscope");
  const userDb = join(userState, "hub", "telemetry.sqlite");
  createDatabase(userDb, 1);
  write(join(userState, "hub", "enrollment.json"), '{"collectors":[]}\n', 0o600);
  write(join(userState, "collector", "state.json"), `${JSON.stringify({ collectorId: "c1", destinations: [] })}\n`, 0o600);
  write(join(userState, "collector", "outbox", "abc", "outbox.sqlite"), "fixture-outbox\n", 0o600);
  write(join(userState, "config.json"), `${JSON.stringify({
    version: 1,
    role: "local",
    port: 8081,
    users: [{ username: "owner", passwordHash: "fixture-hash", machineIds: "*", admin: true }],
    collectors: [{ collectorId: "c1", machineId: "m1", token: "fixture-token" }],
  })}\n`, 0o600);
  write(
    join(home, ".config", "systemd", "user", "rigscope-hub.service"),
    "[Service]\nExecStart=/home/operator/.local/lib/node /repo/dist/hub.cjs\n",
  );
  const paths = pathsFor(root);

  // A live runtime lock makes adoption ambiguous and must fail closed.
  const liveLock = join(userState, "hub", "telemetry.sqlite.hub.lock");
  write(liveLock, `${JSON.stringify({ pid: process.pid, instance: "live" })}\n`, 0o600);
  await assert.rejects(
    installSystem({
      apply: true,
      source: fixtureRelease(root, "0.9.0"),
      paths,
      host: fakeHost().host,
      nodePath: fixtureNode(root),
      trustedNode: true,
      adoptFrom: userState,
      homeRoots: [join(root, "home")],
    }),
    /live or ambiguous runtime lock/,
  );
  rmSync(liveLock);

  const { report } = await installFixture({
    root,
    version: "1.0.0",
    adoptFrom: userState,
    homeRoots: [join(root, "home")],
  });
  assert.equal(report.ok, true);
  assert.equal(readFileSync(join(userState, "config.json"), "utf8").includes("fixture-hash"), true, "source remains untouched");
  assert.equal(existsSync(userDb), true, "source database remains untouched");
  const targetDatabase = hubDatabasePath(paths);
  assert.equal(existsSync(targetDatabase), true);
  const db = new DatabaseSync(targetDatabase, { readOnly: true });
  try {
    const row = db.prepare("SELECT MAX(version) AS version FROM schema_migrations").get() as { version: number };
    assert.equal(row.version, 1);
  } finally {
    db.close();
  }
  const hubEnv = readFileSync(hubEnvPath(paths), "utf8");
  assert.match(hubEnv, /HUB_BROWSER_USERS="\[/);
  assert.match(hubEnv, /fixture-hash/);
  assert.equal(existsSync(collectorStatePath(paths)), true);
  assert.equal(existsSync(join(paths.stateDir, "collector", "outbox", "abc", "outbox.sqlite")), true);
  const state = JSON.parse(readFileSync(stateFilePath(paths), "utf8")) as {
    adoption: { source: string; files: Array<{ target: string }>; reversible: boolean } | null;
  };
  assert.equal(state.adoption?.reversible, true);
  assert.equal(state.adoption?.source, userState);
  assert.ok((state.adoption?.files.length ?? 0) >= 4);
  assert.ok(existsSync(join(home, ".config", "systemd", "user", "rigscope-hub.service")), "user units are not edited");
});

test("power grant/revoke plans and status do not mutate until applied", () => {
  const root = freshRoot("power-plan");
  const paths = pathsFor(root);
  createPowercap(paths.powercapRoot);
  const host = fakeHost();
  const planned = grantPower({ apply: false, paths, host: host.host });
  assert.equal(planned.apply, false);
  assert.equal(existsSync(raplRulePath(paths)), false);
  assert.equal(host.commands.length, 0);
  const grant = grantPower({ apply: true, paths, host: host.host });
  assert.equal(grant.apply, true);
  assert.equal(existsSync(raplRulePath(paths)), true);
  assert.equal(existsSync(powerSysusersPath(paths)), true);
  assert.equal(existsSync(join(paths.stateDir, "power-access.json")), true);
  const status = systemStatus({ paths, host: host.host, homeRoots: [] });
  // The host may now have a real Rigscope account. Validate this fixture's
  // rule independently of whether that external account happens to exist.
  assert.equal((status.power as {rule:{compliant:boolean}}).rule.compliant, true);
  const revoke = revokePower({ apply: true, paths, host: host.host });
  assert.equal(revoke.apply, true);
  assert.equal(existsSync(raplRulePath(paths)), false);
  assert.equal(existsSync(join(paths.stateDir, "power-access.json")), false);
});

test("status is read-only and reports absent installations without creating state", () => {
  const root = freshRoot("status");
  const paths = pathsFor(root);
  const host = fakeHost();
  const status = systemStatus({ paths, host: host.host, homeRoots: [] });
  assert.equal(status.installed, false);
  assert.equal(existsSync(paths.prefix), false);
  assert.equal(existsSync(paths.stateDir), false);
  assert.equal(host.commands.some((command) => command.includes("daemon-reload")), false);
});

test('system status identifies a release and its role without private installation metadata', () => {
  const root = freshRoot('public-status');
  const paths = pathsFor(root);
  const release = join(paths.prefix, 'releases', '0.2.0');
  write(join(release, 'package.json'), JSON.stringify({name: 'rigscope', version: '0.2.0'}));
  write(join(paths.unitsDir, 'rigscope-collector.service'), '[Service]\n');
  symlinkSync(release, join(paths.prefix, 'current'));
  // Existing files can be discovered without opening credentials or a root-only registry.
  const status = systemStatus({paths, host: fakeHost({active: ['rigscope-collector.service']}).host, homeRoots: []});
  assert.equal(status.installed, true);
  assert.equal(status.role, 'collector');
  assert.equal(status.currentVersion, '0.2.0');
  assert.equal(existsSync(paths.stateDir), false);
});

test("createSystemHost wraps commands without a shell", () => {
  const host = createSystemHost();
  const result = host.run(process.execPath, ["--version"]);
  assert.equal(result.status, 0);
  assert.match(result.stdout, /^v26\./);
  assert.throws(() => validateSystemPaths({ prefix: "/home/example" }), SystemError);
});
test("hub-only preset installs both runtimes but enables only the hub", async () => {
  const root = freshRoot("hub-only");
  const { paths, report } = await installFixture({ root, role: "hub", version: "1.0.0" });
  assert.equal(report.ok, true);
  const sysusers = readFileSync(sysusersPath(paths), "utf8");
  assert.match(sysusers, /^u rigscope-hub /m);
  assert.match(sysusers, /^u rigscope-collector /m);
  assert.equal(existsSync(raplRulePath(paths)), false);
  assert.equal(existsSync(powerSysusersPath(paths)), false);
  const state = JSON.parse(readFileSync(stateFilePath(paths), "utf8")) as {
    power: unknown;
    components: { hub: { enabled: boolean }; collector: { enabled: boolean } };
  };
  assert.equal(state.power, null);
  assert.deepEqual(state.components, { hub: { enabled: true }, collector: { enabled: false } });
  assert.equal(existsSync(join(paths.unitsDir, "rigscope-hub.service")), true);
  assert.equal(existsSync(join(paths.unitsDir, "rigscope-collector.service")), true);
  assert.doesNotMatch(readFileSync(join(paths.unitsDir, "rigscope-collector.service"), "utf8"), /^SupplementaryGroups=/m);
  const status = systemStatus({ paths, host: fakeHost().host, homeRoots: [] });
  assert.equal((status.components as Record<string, { enabled: boolean }>).hub.enabled, true);
  assert.equal((status.components as Record<string, { enabled: boolean }>).collector.enabled, false);
});

test("adoption rejects a loose source configuration before copying credentials", async () => {
  const root = freshRoot("loose-adoption");
  const userState = join(root, "user-state");
  write(join(userState, "config.json"), `${JSON.stringify({ version: 1, role: "collector", port: 8081, users: [], collectors: [] })}\n`, 0o644);
  await assert.rejects(
    installSystem({
      apply: true,
      source: fixtureRelease(root, "1.0.0"),
      paths: pathsFor(root),
      host: fakeHost().host,
      nodePath: fixtureNode(root),
      trustedNode: true,
      adoptFrom: userState,
      homeRoots: [],
    }),
    /must be private/,
  );
});

test("a group/world-writable system state directory is refused", async () => {
  const root = freshRoot("loose-dir");
  const paths = pathsFor(root);
  mkdirSync(paths.stateDir, { recursive: true });
  chmodSync(paths.stateDir, 0o777);
  await assert.rejects(
    installSystem({
      apply: true,
      source: fixtureRelease(root, "1.0.0"),
      paths,
      host: fakeHost().host,
      nodePath: fixtureNode(root),
      trustedNode: true,
      homeRoots: [],
    }),
    /group\/world-writable/,
  );
});
test("legacy hub-config.json adoption preserves origin, users and remaps the socket", async () => {
  const root = freshRoot("legacy-adoption");
  const userState = join(root, "legacy-state");
  createDatabase(join(userState, "hub", "telemetry.sqlite"), 1);
  write(join(userState, "hub", "enrollment.json"), '{"collectors":[]}\n', 0o600);
  write(join(userState, "collector", "state.json"), `${JSON.stringify({ version: 1, collectorId: "legacy", destinations: [] })}\n`, 0o600);
  const users = JSON.stringify([{ username: "owner", passwordHash: "legacy-hash", machineIds: "*", admin: true }]);
  write(join(userState, "hub-config.json"), `${JSON.stringify({
    NODE_ENV: "production",
    HUB_PUBLIC_ORIGIN: "https://rig.example.com",
    HUB_SOCKET_PATH: join(userState, "run", "hub.sock"),
    HUB_STATE_FILE: join(userState, "hub", "enrollment.json"),
    HUB_BROWSER_USERS: users,
  })}\n`, 0o600);
  const paths = pathsFor(root);
  const { report } = await installFixture({ root, version: "1.0.0", adoptFrom: userState, homeRoots: [] });
  assert.equal(report.ok, true);
  const hubEnv = readFileSync(hubEnvPath(paths), "utf8");
  assert.match(hubEnv, /HUB_PUBLIC_ORIGIN="https:\/\/rig\.example\.com"/);
  assert.match(hubEnv, /HUB_BROWSER_USERS=".*legacy-hash.*"/);
  assert.ok(hubEnv.includes(`HUB_SOCKET_PATH="${join(paths.stateDir, "hub", "run", "hub.sock")}"`));
  assert.equal(hubEnv.includes(userState), false, "the old socket path is not copied as the system listener");
  assert.equal(readFileSync(join(userState, "hub-config.json"), "utf8").includes("legacy-hash"), true);
});
test("a tampered release artifact is refused before activation", async () => {
  const root = freshRoot("tamper");
  const source = fixtureRelease(root, "1.0.0");
  manifestize(source);
  write(join(source, "dist", "hub.cjs"), "// tampered after manifest\n");
  await assert.rejects(
    installSystem({
      apply: true,
      source,
      paths: pathsFor(root),
      host: fakeHost().host,
      nodePath: fixtureNode(root),
      trustedNode: true,
      homeRoots: [],
    }),
    /artifact hash changed|artifact size changed/,
  );
});

test('system upgrade health failure restores paired data and verifies the former runtime', async () => {
  const root = freshRoot('failed-health');
  const {paths,host} = await installFixture({root,role:'hub',schemaVersion:1});
  const database = hubDatabasePath(paths);
  createDatabase(database,1);
  host.active.add('rigscope-hub.service');
  const versions: Array<string|null> = [];
  host.host.verifyHealth = async (_paths,state) => {
    versions.push(state.currentVersion);
    if (state.currentVersion === '2.0.0') {
      createDatabase(database,2);
      throw new Error('fixture migration/startup rejection');
    }
    const db = new DatabaseSync(database,{readOnly:true});
    assert.equal(db.prepare('SELECT max(version) v FROM schema_migrations').get()!.v,1);
    db.close();
  };
  await assert.rejects(upgradeSystem({apply:true,source:fixtureRelease(root,'2.0.0',2),paths,host:host.host}),/startup rejection/);
  assert.deepEqual(versions,['2.0.0','1.0.0']);
  assert.equal(readSystemState(paths)?.currentVersion,'1.0.0');
  assert.equal(host.active.has('rigscope-hub.service'),true);
  assert.ok(readdirSync(join(paths.stateDir,'backups')).some(file=>file.startsWith('failed-upgrade-')));
});

test('system health requires the live expected version and a ready managed database', async () => {
  const root = freshRoot('health-version');
  const {paths,host} = await installFixture({root,role:'hub'});
  const server = createServer((_req,res)=>res.end(JSON.stringify({status:'ok',version:advertised})));
  let advertised = '9.0.0';
  await new Promise<void>(resolve=>server.listen(0,'127.0.0.1',resolve));
  const address=server.address();assert.ok(address&&typeof address==='object');
  write(hubEnvPath(paths),`HUB_PORT="${address.port}"\n`,0o600);
  host.active.add('rigscope-hub.service');
  const original=host.host.run;
  host.host.run=(command,args,options)=>args.includes('inspect')
    ? {status:0,stderr:'',stdout:JSON.stringify({integrityOk:true,newerSchema:false,ledgerProblem:null,schemaVersion:1,supportedSchemaVersion:1,tables:[{name:'users',rows:1}]})}
    : original(command,args,options);
  try {
    await assert.rejects(verifySystemHealth(paths,readSystemState(paths)!,host.host,150,['hub']),/readiness failed/);
    advertised='1.0.0';
    await verifySystemHealth(paths,readSystemState(paths)!,host.host,1000,['hub']);
  } finally {await new Promise<void>(resolve=>server.close(()=>resolve()));}
});

test('existing root-equivalent service accounts and permission reversal failures fail closed', async () => {
  const root=freshRoot('account-rejection');const host=fakeHost();const original=host.host.run;
  host.host.run=(command,args,options)=>command==='getent'&&args[0]==='passwd'&&args[1]
    ? {status:0,stderr:'',stdout:`${args[1]}:x:0:0:root:/nonexistent:/usr/sbin/nologin\n`}
    : original(command,args,options);
  await assert.rejects(installFixture({root,host}),/must be non-root/);
  const paths=pathsFor(freshRoot('revoke-failure'));createPowercap(paths.powercapRoot);
  const normal=fakeHost();grantPower({apply:true,paths,host:normal.host});
  const runner=normal.host.run;
  normal.host.run=(command,args,options)=>command==='chmod'
    ? {status:1,stdout:'',stderr:'fixture denied'} : runner(command,args,options);
  assert.throws(()=>revokePower({apply:true,paths,host:normal.host}),/restore counter mode failed/);
  assert.ok(existsSync(join(paths.stateDir,'power-access.json')),'keep original metadata until reversal succeeds');
});

test('collector-only adoption preserves its identity while the independent hub stays running', async () => {
  const root=freshRoot('collector-adoption');const source=join(root,'source');
  write(join(source,'config.json'),JSON.stringify({version:1,role:'local',port:8081,users:[],collectors:[]}),0o600);
  write(join(source,'collector/state.json'),JSON.stringify({version:1,collectorId:'preserved-id',destinations:[]}),0o600);
  write(join(source,'hub/telemetry.sqlite.hub.lock'),JSON.stringify({pid:process.pid,instance:'live-hub'}),0o600);
  const {paths}=await installFixture({root,role:'collector',adoptFrom:source});
  assert.equal(JSON.parse(readFileSync(collectorStatePath(paths),'utf8')).collectorId,'preserved-id');
  assert.equal(existsSync(hubDatabasePath(paths)),false);
  assert.ok(existsSync(join(source,'hub/telemetry.sqlite.hub.lock')));
});

test('legacy v1 role state is read-only in status and persists as v2 on a lifecycle mutation', async () => {
  const root = freshRoot('legacy-state');
  const { paths, host } = await installFixture({ root, role: 'hub' });
  const current = JSON.parse(readFileSync(stateFilePath(paths), 'utf8')) as Record<string, unknown>;
  const legacy: Record<string, unknown> = {
    ...current,
    formatVersion: 1,
    role: 'hub',
  };
  delete legacy.components;
  write(stateFilePath(paths), `${JSON.stringify(legacy, null, 2)}\n`, 0o600);

  const status = systemStatus({ paths, host: host.host, homeRoots: [] });
  assert.equal(status.role, 'hub');
  const components = status.components as Record<string, { enabled: boolean }>;
  assert.equal(components.hub.enabled, true);
  assert.equal(components.collector.enabled, false);
  assert.equal(existsSync(paths.configDir), true);
  const unchanged = JSON.parse(readFileSync(stateFilePath(paths), 'utf8')) as { formatVersion: number; components?: unknown };
  assert.equal(unchanged.formatVersion, 1, 'read-only status must not rewrite legacy state');
  assert.equal(unchanged.components, undefined);

  host.host.verifyHealth = async () => {};
  disableSystem({ apply: true, component: 'hub', paths, host: host.host });
  const migrated = JSON.parse(readFileSync(stateFilePath(paths), 'utf8')) as {
    formatVersion: number;
    components: { hub: { enabled: boolean }; collector: { enabled: boolean } };
  };
  assert.equal(migrated.formatVersion, 2);
  assert.deepEqual(migrated.components, { hub: { enabled: false }, collector: { enabled: false } });
});

test('enable hub creates the owner once and prints its password only in that mutation', async () => {
  const root = freshRoot('owner-once');
  const { paths, host } = await installFixture({ root, role: 'hub', enable: false });
  host.host.verifyHealth = async () => {};
  const before = readFileSync(hubEnvPath(paths), 'utf8');
  const commandsBefore = host.commands.length;
  const planned = await enableSystem({ apply: false, component: 'hub', paths, host: host.host });
  assert.equal(planned.apply, false);
  assert.equal(planned.password, undefined);
  assert.equal(host.commands.length, commandsBefore, 'a dry run must not run commands');
  assert.equal(readFileSync(hubEnvPath(paths), 'utf8'), before);

  const first = await enableSystem({ apply: true, component: 'hub', paths, host: host.host });
  assert.equal(typeof first.password, 'string');
  assert.ok((first.password as string).length >= 20);
  const env = readFileSync(hubEnvPath(paths), 'utf8');
  assert.match(env, /HUB_BROWSER_USERS="\[/);
  const rawUsers = env.split('\n').find((line) => line.startsWith('HUB_BROWSER_USERS='))!.slice('HUB_BROWSER_USERS='.length);
  const users = JSON.parse(JSON.parse(rawUsers)) as Array<{ username: string }>;
  assert.equal(users[0]!.username, 'owner');
  assert.equal(env.includes(first.password as string), false, 'only the scrypt hash is persisted');
  const hashLine = env.split('\n').find((line) => line.startsWith('HUB_BROWSER_USERS='))!;
  assert.equal(host.enabled.has('rigscope-hub.service'), true);
  assert.equal(host.active.has('rigscope-hub.service'), true);

  const second = await enableSystem({ apply: true, component: 'hub', paths, host: host.host });
  assert.equal(second.password, undefined, 'password is never reissued');
  assert.equal(readFileSync(hubEnvPath(paths), 'utf8').split('\n').find((line) => line.startsWith('HUB_BROWSER_USERS=')), hashLine);

  const disabled = disableSystem({ apply: true, component: 'hub', paths, host: host.host });
  assert.equal(disabled.apply, true);
  assert.equal(host.active.has('rigscope-hub.service'), false);
  assert.equal(host.enabled.has('rigscope-hub.service'), false);
  assert.equal(readFileSync(hubEnvPath(paths), 'utf8').includes(hashLine), true, 'credentials are retained while disabled');
});

test('adding a collector to a live hub enrolls through the API without restarting the hub', async () => {
  const root = freshRoot('add-collector-live');
  const { paths, host } = await installFixture({ root, role: 'hub' });
  host.host.verifyHealth = async () => {};
  const database = hubDatabasePath(paths);
  createDatabase(database, 1);
  createHubRegistry(database);
  host.active.add('rigscope-hub.service');
  const server = await startEnrollmentServer();
  const port = new URL(server.url).port;
  setEnvLine(hubEnvPath(paths), 'HUB_PORT', port);
  host.commands.length = 0;
  try {
    const report = await enableSystem({ apply: true, component: 'collector', paths, host: host.host });
    assert.equal(report.apply, true);
    assert.equal(server.requests.length, 1, 'the live hub enrollment API was used');
    assert.match(server.requests[0]!.grant, /^[A-Za-z0-9_-]{32,}$/);
    const state = readSystemState(paths)!;
    assert.deepEqual(state.components, { hub: { enabled: true }, collector: { enabled: true } });
    const collector = JSON.parse(readFileSync(collectorStatePath(paths), 'utf8')) as {
      collectorId: string;
      destinations: Array<{ url: string; token: string }>;
    };
    assert.equal(collector.collectorId, server.requests[0]!.collectorId, 'enrollment kept the local collector identity');
    assert.equal(collector.destinations[0]!.url, `ws://127.0.0.1:${port}/api/collector/connect`);
    assert.equal(collector.destinations[0]!.token, 'issued-token-1', 'the hub-issued credential is authoritative');
    assert.equal(host.active.has('rigscope-hub.service'), true);
    assert.equal(host.active.has('rigscope-collector.service'), true);
    assert.equal(host.enabled.has('rigscope-collector.service'), true);
    assert.equal(existsSync(raplRulePath(paths)), true);
    assert.match(readFileSync(join(paths.unitsDir, 'rigscope-collector.service'), 'utf8'), /^SupplementaryGroups=rigscope-rapl$/m);
    const commands = host.commands.map((command) => command.join(' '));
    assert.equal(commands.some((command) => command === 'systemctl stop rigscope-hub.service'), false);
    assert.equal(commands.some((command) => command === 'systemctl start rigscope-hub.service'), false);
    assert.equal(commands.some((command) => command === 'systemctl restart rigscope-hub.service'), false);
    const status = systemStatus({ paths, host: host.host, homeRoots: [] });
    const components = status.components as Record<string, { readiness: string; destinations?: number }>;
    assert.equal(components.hub!.readiness, 'ready');
    assert.equal(components.collector!.readiness, 'ready');
    assert.equal(components.collector!.destinations, 1);

    const grant = new DatabaseSync(database, { readOnly: true });
    try {
      assert.equal((grant.prepare('SELECT COUNT(*) AS n FROM collector_grants').get() as { n: number }).n, 1);
    } finally {
      grant.close();
    }
  } finally {
    await server.close();
  }
});

test('adding a collector to a stopped hub writes the authoritative registry without starting it', async () => {
  const root = freshRoot('add-collector-stopped');
  const { paths, host } = await installFixture({ root, role: 'hub' });
  host.host.verifyHealth = async () => {};
  const database = hubDatabasePath(paths);
  createDatabase(database, 1);
  createHubRegistry(database);
  assert.equal(host.active.has('rigscope-hub.service'), false);
  host.commands.length = 0;
  const report = await enableSystem({ apply: true, component: 'collector', paths, host: host.host });
  assert.equal(report.apply, true);
  assert.equal(host.active.has('rigscope-hub.service'), false, 'enabling the collector must not start the hub');
  assert.equal(host.active.has('rigscope-collector.service'), true);
  assert.equal(host.commands.some((command) => command.join(' ') === 'systemctl start rigscope-hub.service'), false);
  const state = readSystemState(paths)!;
  assert.deepEqual(state.components, { hub: { enabled: true }, collector: { enabled: true } });
  const collector = JSON.parse(readFileSync(collectorStatePath(paths), 'utf8')) as {
    collectorId: string;
    destinations: Array<{ token: string; url: string }>;
  };
  const db = new DatabaseSync(database, { readOnly: true });
  try {
    const row = db.prepare('SELECT collector_id,token FROM collector_registry WHERE collector_id=?').get(collector.collectorId) as
      | { collector_id: string; token: string }
      | undefined;
    assert.ok(row, 'the registry has the new collector before the next hub start');
    assert.equal(row!.token, collector.destinations[0]!.token);
  } finally {
    db.close();
  }
});

test('an unconfigured collector awaits connection, then connect uses a private token file once', async () => {
  const root = freshRoot('collector-connect');
  const { paths, host } = await installFixture({ root, role: 'collector' });
  host.host.verifyHealth = async () => {};
  const awaiting = systemStatus({ paths, host: host.host, component: 'collector', homeRoots: [] });
  assert.equal(awaiting.selected, 'collector');
  assert.equal((awaiting.units as unknown[]).length, 1);
  const components = awaiting.components as Record<string, { readiness: string }>;
  assert.equal(components.collector!.readiness, 'awaiting-connection');
  assert.equal(host.active.has('rigscope-collector.service'), false);

  const server = await startEnrollmentServer();
  const tokenFile = join(root, 'grant');
  write(tokenFile, 'one-time-private-grant\n', 0o600);
  try {
    const commandsBefore = host.commands.length;
    const planned = await connectSystem({
      apply: false,
      url: server.url,
      tokenFile,
      paths,
      host: host.host,
    });
    assert.equal(planned.apply, false);
    assert.equal(host.commands.length, commandsBefore);

    const report = await connectSystem({
      apply: true,
      url: server.url,
      tokenFile,
      paths,
      host: host.host,
    });
    assert.equal(report.apply, true);
    assert.equal(server.requests.length, 1);
    const collector = JSON.parse(readFileSync(collectorStatePath(paths), 'utf8')) as {
      destinations: Array<{ token: string; url: string }>;
    };
    assert.equal(collector.destinations[0]!.token, 'issued-token-1');
    assert.equal(collector.destinations[0]!.url, `ws://127.0.0.1:${new URL(server.url).port}/api/collector/connect`);
    assert.equal(host.active.has('rigscope-collector.service'), true);

    await assert.rejects(
      connectSystem({ apply: true, url: server.url, tokenFile, paths, host: host.host }),
      /already has a destination/,
    );
    const destination = collector.destinations[0]!;
    disableSystem({ apply: true, component: 'collector', paths, host: host.host });
    await enableSystem({ apply: true, component: 'collector', paths, host: host.host });
    assert.equal(server.requests.length, 1, 'a configured collector never contacts the hub again');
    const preserved = JSON.parse(readFileSync(collectorStatePath(paths), 'utf8')) as { destinations: Array<{ token: string; url: string }> };
    assert.deepEqual(preserved.destinations[0], destination);
  } finally {
    await server.close();
  }
});

test('upgrade and rollback restore the exact enabled and running component set', async () => {
  const root = freshRoot('component-upgrade');
  const { paths, host } = await installFixture({ root, role: 'local' });
  host.host.verifyHealth = async () => {};
  const database = hubDatabasePath(paths);
  createDatabase(database, 1);
  host.active.add('rigscope-hub.service');
  assert.equal(host.active.has('rigscope-collector.service'), false);
  host.commands.length = 0;
  const upgraded = await upgradeSystem({
    apply: true,
    source: fixtureRelease(root, '2.0.0', 2),
    paths,
    host: host.host,
    start: true,
  });
  assert.equal(upgraded.version, '2.0.0');
  let state = readSystemState(paths)!;
  assert.deepEqual(state.components, { hub: { enabled: true }, collector: { enabled: true } });
  assert.equal(host.active.has('rigscope-hub.service'), true);
  assert.equal(host.active.has('rigscope-collector.service'), false, 'a stopped component stays stopped after upgrade');
  let commands = host.commands.map((command) => command.join(' '));
  assert.equal(commands.some((command) => command === 'systemctl start rigscope-collector.service'), false);
  host.commands.length = 0;
  const rolled = await rollbackSystem({ apply: true, paths, host: host.host, start: true });
  assert.equal(rolled.version, '1.0.0');
  state = readSystemState(paths)!;
  assert.deepEqual(state.components, { hub: { enabled: true }, collector: { enabled: true } });
  assert.equal(host.active.has('rigscope-hub.service'), true);
  assert.equal(host.active.has('rigscope-collector.service'), false, 'a stopped component stays stopped after rollback');
  commands = host.commands.map((command) => command.join(' '));
  assert.equal(commands.some((command) => command === 'systemctl start rigscope-collector.service'), false);
});

test('disabling one component leaves the other running and retains its data', async () => {
  const root = freshRoot('mixed-disable');
  const { paths, host } = await installFixture({ root, role: 'local' });
  host.host.verifyHealth = async () => {};
  host.active.add('rigscope-hub.service');
  host.active.add('rigscope-collector.service');
  const before = readFileSync(collectorStatePath(paths), 'utf8');
  const disabled = disableSystem({ apply: true, component: 'hub', paths, host: host.host });
  assert.equal(disabled.apply, true);
  assert.equal(host.active.has('rigscope-hub.service'), false);
  assert.equal(host.enabled.has('rigscope-hub.service'), false);
  assert.equal(host.active.has('rigscope-collector.service'), true, 'the collector keeps running');
  assert.equal(host.enabled.has('rigscope-collector.service'), true);
  const state = readSystemState(paths)!;
  assert.deepEqual(state.components, { hub: { enabled: false }, collector: { enabled: true } });
  assert.equal(readFileSync(collectorStatePath(paths), 'utf8'), before);
});


test('native systemd parser accepts generated hub and collector path directives', t => {
  const check=spawnSync('systemd-analyze',['--version'],{encoding:'utf8'});
  if (check.error) return t.skip('systemd-analyze is not installed');
  const root=freshRoot('native-units');const paths=pathsFor(root);const accounts={...DEFAULT_SYSTEM_ACCOUNTS};
  const collector=renderSystemCollectorUnit({paths,accounts,nodePath:process.execPath,collectorState:collectorStatePath(paths),powerAccess:true});
  const hub=renderSystemHubUnit({paths,accounts,nodePath:process.execPath,database:hubDatabasePath(paths),enrollment:join(paths.stateDir,'hub/enrollment.json'),socket:join(paths.stateDir,'hub/run/hub.sock'),remote:true,powerAccess:false});
  const files=[join(root,'rigscope-native-hub.service'),join(root,'rigscope-native-collector.service')];
  write(files[0]!,hub);write(files[1]!,collector);
  const parsed=spawnSync('systemd-analyze',['verify','--man=no',...files],{encoding:'utf8'});
  assert.equal(parsed.status,0,parsed.stderr);
  assert.doesNotMatch(parsed.stderr,/path is not absolute|bad unit file|fatal error|unknown (?:key|lvalue)|ignoring/i);
});
