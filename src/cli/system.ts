/**
 * Optional system-scope installation with sudo, and persistent narrow CPU
 * package-power access.
 *
 * This module is deliberately separate from the per-user CLI/operations paths.
 * It never changes the developer's `~/.local` state or user units. Every
 * mutation requires `--apply`, an effective uid of 0 and an explicit command;
 * without `--apply` it prints the exact plan, files and commands instead.
 *
 * Scope model:
 *   /opt/rigscope                 root-owned program + versioned releases
 *   /etc/rigscope                 root-owned private EnvironmentFile values
 *   /var/lib/rigscope             state, databases, outboxes and backups
 *   /etc/systemd/system           system units (multi-user.target)
 *   /etc/sysusers.d               dedicated unprivileged account definitions
 *   /etc/udev/rules.d             package-RAPL read rule for the collector group
 *
 * The hub and collector run as separate nologin system accounts. Only the
 * collector joins the power group, and only top-level package energy/range
 * counters are made group-readable. Node is never given file capabilities.
 */
import {
  chmodSync,
  copyFileSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  readlinkSync,
  realpathSync,
  renameSync,
  rmSync,
  statSync,
  symlinkSync,
  unlinkSync,
  writeFileSync,
} from "node:fs";
import { spawnSync } from "node:child_process";
import { createHash, randomBytes, randomUUID } from "node:crypto";
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { DatabaseSync, backup as sqliteBackup } from "node:sqlite";
import { parseArgs } from "node:util";
import { waitForHealth, type HealthTarget } from './health';
import { acquireProcessLease } from '../runtime/process-lease';
import { hashPassword } from '../hub/auth/password';
import { enrollCollector } from '../../collector/enroll';

/**
 * Version 2 keeps independent hub/collector enabled state. Version 1 role
 * state is read through a compatibility mapping and is only rewritten by an
 * explicit lifecycle mutation; read-only status never touches the file.
 */
export const SYSTEM_STATE_FORMAT_VERSION = 2;
export const SYSTEM_SCOPE = "system";
export const DEFAULT_SYSTEM_ACCOUNTS = {
  hub: "rigscope-hub",
  collector: "rigscope-collector",
  powerGroup: "rigscope-rapl",
} as const;
export const DEFAULT_NODE_PATH = "/opt/rigscope/node/node";
export const UNIT_NAMES = {
  hub: "rigscope-hub.service",
  collector: "rigscope-collector.service",
} as const;
export const RAPL_RULE_NAME = "90-rigscope-rapl.rules";
export const SYSUSERS_NAME = "rigscope.conf";
export const POWER_SYSUSERS_NAME = "rigscope-rapl.conf";
export const STATE_FILE_NAME = "system-install.json";
export const POWER_STATE_FILE_NAME = "power-access.json";

export type SystemRole = "local" | "hub" | "collector";
export const COMPONENTS = ["hub", "collector"] as const;
export type Component = (typeof COMPONENTS)[number];
export type ComponentSet = { hub: boolean; collector: boolean };
export type ComponentState = { enabled: boolean };

/** The initial install preset only; runtime selection always uses components. */
export function componentsForRole(role: SystemRole): ComponentSet {
  return { hub: role !== "collector", collector: role !== "hub" };
}

export function componentSet(value?: Component | Component[] | ComponentSet): ComponentSet {
  if (value === undefined) return { hub: false, collector: false };
  if (typeof value === "string") return { hub: value === "hub", collector: value === "collector" };
  if (Array.isArray(value)) return { hub: value.includes("hub"), collector: value.includes("collector") };
  return { hub: value.hub === true, collector: value.collector === true };
}

export function unitFor(component: Component): string {
  return component === "hub" ? UNIT_NAMES.hub : UNIT_NAMES.collector;
}

export function componentEnabled(state: Pick<SystemInstallState, "components">, component: Component): boolean {
  return state.components[component].enabled;
}

export function enabledComponents(state: Pick<SystemInstallState, "components">): Component[] {
  return COMPONENTS.filter((component) => componentEnabled(state, component));
}

/** Local collector sharing defaults; mirrors the user CLI preset. */
const SYSTEM_DEFAULT_SHARING = {
  cpu: true,
  memory: true,
  swap: true,
  gpus: true,
  disks: true,
  network: true,
  networkAddresses: false,
  processes: true,
  processArguments: false,
} as const;

export type SystemPaths = {
  prefix: string;
  configDir: string;
  stateDir: string;
  unitsDir: string;
  sysusersDir: string;
  udevRulesDir: string;
  powercapRoot: string;
};

export type SystemAccounts = {
  hub: string;
  collector: string;
  powerGroup: string;
};

export type CommandResult = {
  status: number | null;
  stdout: string;
  stderr: string;
  error?: string;
};

export type RunOptions = { cwd?: string; env?: NodeJS.ProcessEnv; timeoutMs?: number };

export type SystemHost = {
  uid(): number;
  run(command: string, args: string[], options?: RunOptions): CommandResult;
  now(): number;
  /** Fixture-only replacement for actual listener/process health checks. */
  verifyHealth?: (paths: SystemPaths, state: SystemInstallState, timeoutMs: number) => Promise<void>;
};

export class SystemError extends Error {
  readonly code: string;
  constructor(code: string, message: string) {
    super(message);
    this.name = "SystemError";
    this.code = code;
  }
}

const ACCOUNT_PATTERN = /^[a-z_][a-z0-9_-]{0,31}$/;
const VERSION_PATTERN = /^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/;
const PACKAGE_TOP_LEVEL = new Set([
  "bin",
  "dist",
  ".next",
  "public",
  "packaging",
  "docs",
  "manifest.json",
  "package.json",
  "package-lock.json",
  "next.config.js",
  "next-env.d.ts",
  "tsconfig.json",
  "README.md",
  "LICENSE",
  "LICENSE.md",
  "SECURITY.md",
  "THIRD_PARTY_NOTICES.md",
]);
const REQUIRED_ARTIFACTS = [
  "dist/hub.cjs",
  "dist/collector.cjs",
  "dist/ops.cjs",
  "dist/cli.cjs",
  ".next/BUILD_ID",
  "package.json",
];

export function createSystemHost(overrides: Partial<SystemHost> = {}): SystemHost {
  return {
    uid: () => process.getuid?.() ?? -1,
    now: () => Date.now(),
    run: (command, args, options = {}) => {
      const result = spawnSync(command, args, {
        cwd: options.cwd,
        env: options.env,
        encoding: "utf8",
        timeout: options.timeoutMs ?? 120_000,
        maxBuffer: 16 * 1024 * 1024,
      });
      return {
        status: result.status,
        stdout: result.stdout ?? "",
        stderr: result.stderr ?? "",
        error: result.error?.message,
      };
    },
    ...overrides,
  };
}

export function defaultSystemPaths(overrides: Partial<SystemPaths> = {}): SystemPaths {
  return {
    prefix: resolve(overrides.prefix ?? "/opt/rigscope"),
    configDir: resolve(overrides.configDir ?? "/etc/rigscope"),
    stateDir: resolve(overrides.stateDir ?? "/var/lib/rigscope"),
    unitsDir: resolve(overrides.unitsDir ?? "/etc/systemd/system"),
    sysusersDir: resolve(overrides.sysusersDir ?? "/etc/sysusers.d"),
    udevRulesDir: resolve(overrides.udevRulesDir ?? "/etc/udev/rules.d"),
    powercapRoot: resolve(overrides.powercapRoot ?? "/sys/class/powercap"),
  };
}

export function unitNamesFor(role: SystemRole): string[] {
  if (role === "hub") return [UNIT_NAMES.hub];
  if (role === "collector") return [UNIT_NAMES.collector];
  return [UNIT_NAMES.hub, UNIT_NAMES.collector];
}

export function unitNamesForComponents(components: Component | Component[] | ComponentSet): string[] {
  return COMPONENTS.filter((component) => componentSet(components)[component]).map(unitFor);
}

export function rolesFor(role: SystemRole): { hub: boolean; collector: boolean } {
  return { hub: role !== "collector", collector: role !== "hub" };
}

function isWithin(parent: string, candidate: string): boolean {
  const rel = relative(resolve(parent), resolve(candidate));
  return rel === "" || (rel !== ".." && !rel.startsWith(`..${sep}`) && !isAbsolute(rel));
}

function ensureAbsolute(path: string, label: string): string {
  const absolute = resolve(path);
  if (/[\0\r\n]/.test(absolute)) throw new SystemError("unsafe-path", `${label} contains control characters`);
  if (absolute === "/") throw new SystemError("unsafe-path", `${label} must not be the filesystem root`);
  for (const forbidden of ["/home", "/root", "/proc", "/sys", "/dev", "/boot"]) {
    if (absolute === forbidden || absolute.startsWith(`${forbidden}/`)) {
      throw new SystemError("unsafe-path", `${label} must not live under ${forbidden}: ${absolute}`);
    }
  }
  return absolute;
}

export function validateSystemPaths(input: Partial<SystemPaths> = {}): SystemPaths {
  const paths = defaultSystemPaths(input);
  for (const [label, value] of Object.entries(paths)) {
    if (label === "powercapRoot") {
      // The read-only sensor root is allowed under /sys by design.
      if (!isAbsolute(value) || /[\0\r\n]/.test(value) || resolve(value) === "/") {
        throw new SystemError("unsafe-path", `powercap root is unsafe: ${value}`);
      }
    } else {
      ensureAbsolute(value, label);
    }
  }
  const fixed: Array<[string, string]> = [
    ["prefix", paths.prefix],
    ["configDir", paths.configDir],
    ["stateDir", paths.stateDir],
    ["unitsDir", paths.unitsDir],
    ["sysusersDir", paths.sysusersDir],
    ["udevRulesDir", paths.udevRulesDir],
  ];
  for (let i = 0; i < fixed.length; i += 1) {
    for (let j = i + 1; j < fixed.length; j += 1) {
      const [leftName, left] = fixed[i]!;
      const [rightName, right] = fixed[j]!;
      if (left === right || isWithin(left, right) || isWithin(right, left)) {
        throw new SystemError("unsafe-path", `${leftName} and ${rightName} must be separate paths`);
      }
    }
  }
  return paths;
}

export function validateAccounts(input: Partial<SystemAccounts> = {}): SystemAccounts {
  const accounts = { ...DEFAULT_SYSTEM_ACCOUNTS, ...input };
  for (const [label, value] of Object.entries(accounts)) {
    if (!ACCOUNT_PATTERN.test(value)) throw new SystemError("invalid-account", `${label} account name is invalid: ${value}`);
  }
  if (new Set(Object.values(accounts)).size !== Object.values(accounts).length) {
    throw new SystemError("invalid-account", "hub, collector and power group names must differ");
  }
  if (Object.values(accounts).includes("root")) {
    throw new SystemError("invalid-account", "system services must not run as root");
  }
  return accounts;
}

function assertApply(host: SystemHost): void {
  if (host.uid() !== 0) {
    throw new SystemError("root-required", "This mutation requires root. Re-run the same command with sudo; no change was made.");
  }
}

function runRequired(host: SystemHost, command: string, args: string[], label: string, options?: RunOptions): CommandResult {
  const result = host.run(command, args, options);
  if (result.status !== 0) {
    const detail = (result.stderr || result.stdout || result.error || "").trim().split("\n").slice(-3).join(" ");
    throw new SystemError("command-failed", `${label} failed${detail ? `: ${detail}` : ""}`);
  }
  return result;
}

function runMaybe(host: SystemHost, command: string, args: string[], options?: RunOptions): CommandResult {
  return host.run(command, args, options);
}

function sha256File(path: string): string {
  return createHash("sha256").update(readFileSync(path)).digest("hex");
}

function atomicWrite(path: string, content: string, mode: number): void {
  mkdirSync(dirname(path), { recursive: true, mode: 0o755 });
  const temporary = join(dirname(path), `.${basename(path)}.tmp-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2)}`);
  try {
    writeFileSync(temporary, content, { mode });
    chmodSync(temporary, mode);
    renameSync(temporary, path);
  } catch (error) {
    try {
      unlinkSync(temporary);
    } catch {
      /* preserve the original error */
    }
    throw error;
  }
}

function ensureDirectory(path: string, mode: number, privateDirectory = false): void {
  let stat;
  try {
    stat = lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    mkdirSync(path, { recursive: true, mode });
    chmodSync(path, mode);
    stat = lstatSync(path);
  }
  if (!stat.isDirectory() || stat.isSymbolicLink()) {
    throw new SystemError("unsafe-path", `${path} must be a real directory`);
  }
  const actual = stat.mode & 0o777;
  if (privateDirectory && (actual & 0o077) !== 0) {
    throw new SystemError("insecure-permissions", `${path} must be private (0700 or stricter); found 0${actual.toString(8)}`);
  }
}

function isDefaultSystemLayout(paths: SystemPaths): boolean {
  return paths.prefix === "/opt/rigscope" && paths.configDir === "/etc/rigscope" && paths.stateDir === "/var/lib/rigscope";
}

function assertManagedPathSafety(paths: SystemPaths, enforceRoot: boolean): void {
  for (const path of [paths.prefix, paths.configDir, paths.stateDir, paths.unitsDir, paths.sysusersDir, paths.udevRulesDir]) {
    if (!existsSync(path)) continue;
    const stat = lstatSync(path);
    if (!stat.isDirectory() || stat.isSymbolicLink()) {
      throw new SystemError("unsafe-path", `${path} must be a real directory`);
    }
    if ((stat.mode & 0o022) !== 0) {
      throw new SystemError("insecure-permissions", `${path} must not be group/world-writable; found 0${(stat.mode & 0o777).toString(8)}`);
    }
    if (enforceRoot && stat.uid !== 0) {
      throw new SystemError("insecure-ownership", `${path} must be root-owned for a default system installation`);
    }
  }
}

function writeManagedFile(path: string, content: string, mode: number, replace: boolean): { created: boolean } {
  if (existsSync(path)) {
    const stat = lstatSync(path);
    if (!stat.isFile() || stat.isSymbolicLink()) {
      throw new SystemError("unsafe-path", `${path} must be a regular file, not a symlink`);
    }
    if (!replace) {
      if ((stat.mode & 0o077) !== 0) {
        throw new SystemError("insecure-permissions", `${path} must be private (0600); found 0${(stat.mode & 0o777).toString(8)}`);
      }
      return { created: false };
    }
  }
  atomicWrite(path, content, mode);
  return { created: true };
}

export function systemdQuote(value: string): string {
  if (/[\r\n\0]/.test(value)) throw new SystemError("unsafe-value", "systemd values must not contain control characters");
  return `"${value.replace(/\\/g, "\\\\").replace(/"/g, '\\"').replace(/%/g, "%%").replace(/\$/g, "$$")}"`;
}

function envLine(key: string, value: string): string {
  if (!/^[A-Z][A-Z0-9_]*$/.test(key)) throw new SystemError("invalid-env", `invalid environment key: ${key}`);
  if (/[\r\n\0]/.test(value)) throw new SystemError("invalid-env", `environment value for ${key} contains control characters`);
  return `${key}=${JSON.stringify(value)}`;
}

type EnvDocument = { lines: string[]; values: Record<string, string> };

function parseEnvValue(value: string): string {
  if (value.startsWith('"')) {
    try {
      return JSON.parse(value);
    } catch {
      return value.slice(1, -1);
    }
  }
  if (value.startsWith("'") && value.endsWith("'")) {
    return value.slice(1, -1).replace(/\\'/g, "'").replace(/\\\\/g, "\\");
  }
  return value;
}

function readEnvDocument(path: string): EnvDocument | null {
  let raw: string;
  try {
    raw = readFileSync(path, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return null;
    throw error;
  }
  const lines = raw.split("\n");
  const values: Record<string, string> = {};
  for (const line of lines) {
    const match = /^([A-Z][A-Z0-9_]*)=(.*)$/.exec(line);
    if (match) values[match[1]!] = parseEnvValue(match[2]!);
  }
  return { lines, values };
}

function writeEnvValue(path: string, key: string, value: string): void {
  const document = readEnvDocument(path);
  const lines = (document?.lines ?? ["# Rigscope private environment. Mode 0600; never printed."]).filter(
    (line) => !line.startsWith(`${key}=`),
  );
  while (lines.length > 0 && lines[lines.length - 1] === "") lines.pop();
  const comment = lines.findIndex((line) => line.startsWith(`# ${key}=`) || line.startsWith(`#${key}=`));
  if (comment >= 0) lines.splice(comment + 1, 0, envLine(key, value));
  else lines.push(envLine(key, value));
  atomicWrite(path, `${lines.join("\n")}\n`, 0o600);
}

export function connectDestinationUrl(value: string): string {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new SystemError("invalid-url", "Use a hub URL such as https://hub.example.com.");
  }
  if (url.protocol === "https:") url.protocol = "wss:";
  else if (url.protocol === "http:") url.protocol = "ws:";
  else if (url.protocol !== "ws:" && url.protocol !== "wss:") {
    throw new SystemError("invalid-url", "Use an http(s) or ws(s) hub URL.");
  }
  if (url.username !== "" || url.password !== "") {
    throw new SystemError("invalid-url", "Hub URLs must not contain credentials; use --token-file.");
  }
  if (url.search !== "" || url.hash !== "") throw new SystemError("invalid-url", "Hub URLs must not contain a query or fragment.");
  if (url.hostname === "localhost") url.hostname = "127.0.0.1";
  if (url.pathname === "/") url.pathname = "/api/collector/connect";
  return url.href;
}

export function hubDatabasePath(paths: SystemPaths): string {
  return join(paths.stateDir, "hub", "telemetry.sqlite");
}
export function hubEnrollmentPath(paths: SystemPaths): string {
  return join(paths.stateDir, "hub", "enrollment.json");
}
export function hubSocketPath(paths: SystemPaths): string {
  return join(paths.stateDir, "hub", "run", "hub.sock");
}
export function collectorStatePath(paths: SystemPaths): string {
  return join(paths.stateDir, "collector", "state.json");
}
export function hubEnvPath(paths: SystemPaths): string {
  return join(paths.configDir, "hub.env");
}
export function collectorEnvPath(paths: SystemPaths): string {
  return join(paths.configDir, "collector.env");
}
export function sysusersPath(paths: SystemPaths): string {
  return join(paths.sysusersDir, SYSUSERS_NAME);
}
export function powerSysusersPath(paths: SystemPaths): string {
  return join(paths.sysusersDir, POWER_SYSUSERS_NAME);
}
export function raplRulePath(paths: SystemPaths): string {
  return join(paths.udevRulesDir, RAPL_RULE_NAME);
}
export function stateFilePath(paths: SystemPaths): string {
  return join(paths.stateDir, STATE_FILE_NAME);
}

export type CollectorIdentity = {
  version: 1;
  collectorId: string;
  destinations: Array<{ url: string; token: string; sharing?: Record<string, boolean> }>;
};

function readCollectorIdentity(path: string): CollectorIdentity | null {
  if (!existsSync(path)) return null;
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.isSymbolicLink()) throw new SystemError("unsafe-path", `${path} must be a regular file`);
  if ((stat.mode & 0o077) !== 0) throw new SystemError("insecure-permissions", `${path} must be private (0600)`);
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(path, "utf8"));
  } catch {
    throw new SystemError("invalid-file", `${path} is not valid JSON`);
  }
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) throw new SystemError("invalid-file", `${path} is invalid`);
  const record = raw as Record<string, unknown>;
  if (
    (record.version !== undefined && record.version !== 1) ||
    typeof record.collectorId !== "string" ||
    !/^[\x21-\x7e]{1,128}$/.test(record.collectorId) ||
    !Array.isArray(record.destinations)
  ) {
    throw new SystemError("invalid-file", `${path} is not a collector installation state`);
  }
  const destinations: CollectorIdentity["destinations"] = [];
  for (const entry of record.destinations) {
    if (entry === null || typeof entry !== "object" || Array.isArray(entry)) {
      throw new SystemError("invalid-file", `${path} contains an invalid destination`);
    }
    const destination = entry as Record<string, unknown>;
    if (typeof destination.url !== "string" || typeof destination.token !== "string" || destination.token === "") {
      throw new SystemError("invalid-file", `${path} contains an invalid destination`);
    }
    destinations.push({
      url: destination.url,
      token: destination.token,
      ...(destination.sharing !== null && typeof destination.sharing === "object" && !Array.isArray(destination.sharing)
        ? { sharing: destination.sharing as Record<string, boolean> }
        : {}),
    });
  }
  return { version: 1, collectorId: record.collectorId, destinations };
}

function writeCollectorIdentity(path: string, identity: CollectorIdentity): void {
  ensureDirectory(dirname(path), 0o700, true);
  atomicWrite(path, `${JSON.stringify(identity)}\n`, 0o600);
}

function hubOwnerState(paths: SystemPaths): { configured: boolean; problem?: string } {
  const document = readEnvDocument(hubEnvPath(paths));
  if (document === null) return { configured: false };
  const value = document.values.HUB_BROWSER_USERS;
  if (value === undefined || value === "") return { configured: false };
  try {
    const parsed: unknown = JSON.parse(value);
    if (!Array.isArray(parsed)) return { configured: false, problem: "HUB_BROWSER_USERS must be a JSON array" };
    return { configured: parsed.length > 0 };
  } catch {
    return { configured: false, problem: "HUB_BROWSER_USERS is not valid JSON" };
  }
}

function collectorConfigured(paths: SystemPaths): { configured: boolean; destinations: number; problem?: string } {
  try {
    const identity = readCollectorIdentity(collectorStatePath(paths));
    if (identity === null) return { configured: false, destinations: 0 };
    return { configured: identity.destinations.length > 0, destinations: identity.destinations.length };
  } catch (error) {
    return { configured: false, destinations: 0, problem: error instanceof Error ? error.message : String(error) };
  }
}

function ensureHubOwner(paths: SystemPaths): string | undefined {
  const document = readEnvDocument(hubEnvPath(paths));
  if (document === null) throw new SystemError("missing-config", `hub configuration is missing: ${hubEnvPath(paths)}; run install or enable hub`);
  const existing = document.values.HUB_BROWSER_USERS;
  if (existing !== undefined && existing !== "") {
    let parsed: unknown;
    try {
      parsed = JSON.parse(existing);
    } catch {
      throw new SystemError("invalid-config", "HUB_BROWSER_USERS is not valid JSON; repair the private hub environment");
    }
    if (!Array.isArray(parsed)) throw new SystemError("invalid-config", "HUB_BROWSER_USERS must be a JSON array");
    if (parsed.length > 0) return undefined;
  }
  const password = randomBytes(18).toString("base64url");
  const users = [{ username: "owner", passwordHash: hashPassword(password), machineIds: "*", admin: true }];
  writeEnvValue(hubEnvPath(paths), "HUB_BROWSER_USERS", JSON.stringify(users));
  return password;
}

function hubDestinationUrl(paths: SystemPaths): { url: string; remote: boolean } | null {
  const document = readEnvDocument(hubEnvPath(paths));
  if (document === null) return null;
  const origin = document.values.HUB_PUBLIC_ORIGIN;
  if (origin !== undefined && origin !== "") {
    let parsed: URL;
    try {
      parsed = new URL(origin);
    } catch {
      return null;
    }
    if (parsed.protocol !== "https:") return null;
    parsed.protocol = "wss:";
    parsed.pathname = "/api/collector/connect";
    return { url: parsed.href, remote: true };
  }
  const host = document.values.HUB_HOST ?? "127.0.0.1";
  if (host !== "127.0.0.1" && host !== "::1") return null;
  const port = Number(document.values.HUB_PORT ?? "8081");
  if (!Number.isInteger(port) || port < 1 || port > 65535) return null;
  return { url: `ws://127.0.0.1:${port}/api/collector/connect`, remote: false };
}

function updateHubCollectorRegistry(
  paths: SystemPaths,
  identity: { collectorId: string; machineId: string; token: string },
): { added: boolean } {
  const document = readEnvDocument(hubEnvPath(paths));
  if (document === null) return { added: false };
  let collectors: unknown[] = [];
  const raw = document.values.HUB_COLLECTORS;
  if (raw !== undefined && raw !== "") {
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      throw new SystemError("invalid-config", "HUB_COLLECTORS is not valid JSON; repair the private hub environment");
    }
    if (!Array.isArray(parsed)) throw new SystemError("invalid-config", "HUB_COLLECTORS must be a JSON array");
    collectors = parsed;
  }
  if (collectors.some((entry) => entry !== null && typeof entry === "object" && !Array.isArray(entry) && (entry as Record<string, unknown>).collectorId === identity.collectorId)) {
    return { added: false };
  }
  collectors = [...collectors, { collectorId: identity.collectorId, machineId: identity.machineId, token: identity.token }];
  writeEnvValue(hubEnvPath(paths), "HUB_COLLECTORS", JSON.stringify(collectors));
  return { added: true };
}

function registerCollectorInHubDatabase(
  paths: SystemPaths,
  identity: { collectorId: string; machineId: string; token: string },
  host: SystemHost,
): { inserted: boolean; existing?: { machineId: string; token: string } } {
  const database = hubDatabasePath(paths);
  if (!existsSync(database)) return { inserted: false };
  const db = new DatabaseSync(database, { timeout: 5_000 });
  try {
    const table = db
      .prepare("SELECT name FROM sqlite_master WHERE type='table' AND name='collector_registry'")
      .get();
    if (!table) return { inserted: false };
    const existing = db
      .prepare("SELECT collector_id,machine_id,token FROM collector_registry WHERE collector_id=?")
      .get(identity.collectorId) as { collector_id: string; machine_id: string; token: string } | undefined;
    if (existing) return { inserted: false, existing: { machineId: existing.machine_id, token: existing.token } };
    db.prepare("INSERT INTO collector_registry(collector_id,machine_id,token,revoked,created_at_ms,updated_at_ms) VALUES(?,?,?,0,?,?)")
      .run(identity.collectorId, identity.machineId, identity.token, host.now(), host.now());
    return { inserted: true };
  } finally {
    db.close();
  }
}

/**
 * Create a short-lived bootstrap grant in the running hub's authoritative
 * registry. The collector then enrolls through the normal hub API, which
 * updates live registry state without stopping or restarting the hub.
 */
function findCollectorInHubDatabase(
  paths: SystemPaths,
  collectorId: string,
): { machineId: string; token: string } | null {
  const database = hubDatabasePath(paths);
  if (!existsSync(database)) return null;
  const db = new DatabaseSync(database, { readOnly: true, timeout: 5_000 });
  try {
    const table = db
      .prepare("SELECT name FROM sqlite_master WHERE type='table' AND name='collector_registry'")
      .get();
    if (!table) return null;
    const row = db
      .prepare("SELECT machine_id,token FROM collector_registry WHERE collector_id=? AND revoked=0")
      .get(collectorId) as { machine_id: string; token: string } | undefined;
    return row ? { machineId: row.machine_id, token: row.token } : null;
  } finally {
    db.close();
  }
}

function createLocalEnrollmentGrant(paths: SystemPaths, host: SystemHost): string {
  const database = hubDatabasePath(paths);
  if (!existsSync(database)) throw new SystemError("hub-unavailable", "the hub database does not exist yet");
  const token = randomBytes(32).toString("base64url");
  const tokenHash = createHash("sha256").update(token).digest("hex");
  const expiresAt = host.now() + 10 * 60 * 1000;
  const db = new DatabaseSync(database, { timeout: 5_000 });
  try {
    const table = db
      .prepare("SELECT name FROM sqlite_master WHERE type='table' AND name='collector_grants'")
      .get();
    if (!table) throw new SystemError("hub-unavailable", "the hub registry is not initialized");
    db.exec("BEGIN IMMEDIATE");
    try {
      db.prepare("INSERT OR IGNORE INTO collector_registry_meta VALUES('bootstrapped','1')").run();
      db.prepare("DELETE FROM collector_grants WHERE expires_at_ms < ? AND (recovery_until_ms IS NULL OR recovery_until_ms < ?)").run(host.now(), host.now());
      const count = db.prepare("SELECT COUNT(*) AS n FROM collector_grants").get() as { n: number };
      if (count.n >= 256) throw new SystemError("grant-budget", "the hub enrollment grant budget is exhausted");
      db.prepare(
        "INSERT INTO collector_grants(token_hash,expires_at_ms,replace_machine_id,consumed_by,request_id,recovery_hash,recovery_until_ms,actor) VALUES(?,?,NULL,NULL,NULL,NULL,NULL,?)",
      ).run(tokenHash, expiresAt, "system-installer");
      db.exec("COMMIT");
    } catch (error) {
      try {
        db.exec("ROLLBACK");
      } catch {
        /* preserve the original error */
      }
      throw error;
    }
  } finally {
    db.close();
  }
  return token;
}

type PairingResult = {
  paired: boolean;
  awaiting: boolean;
  hubWasActive: boolean;
  destination?: string;
  note?: string;
};

async function pairCollectorWithHub(options: {
  paths: SystemPaths;
  accounts: SystemAccounts;
  host: SystemHost;
}): Promise<PairingResult> {
  const { paths, accounts, host } = options;
  const existing = readCollectorIdentity(collectorStatePath(paths));
  if (existing !== null && existing.destinations.length > 0) {
    return { paired: false, awaiting: false, hubWasActive: false, note: "configured collector keeps its destinations" };
  }
  const owner = hubOwnerState(paths);
  if (!owner.configured) {
    return {
      paired: false,
      awaiting: true,
      hubWasActive: false,
      note: owner.problem ?? "the local hub has no owner yet",
    };
  }
  const destination = hubDestinationUrl(paths);
  if (destination === null) {
    return {
      paired: false,
      awaiting: true,
      hubWasActive: false,
      note: "the local hub has no reachable loopback or HTTPS destination",
    };
  }
  if (!existsSync(join(paths.unitsDir, UNIT_NAMES.hub))) {
    return { paired: false, awaiting: true, hubWasActive: false, note: "the local hub unit is not installed" };
  }
  const hubWasActive = systemdActive(host, UNIT_NAMES.hub);
  if (hubWasActive) {
    // The live hub updates its in-memory registry on enrollment. Do not stop
    // or restart it merely because a second local component is being enabled.
    if (existing !== null) {
      // Normalize legacy collector state before the enrollment helper reads it.
      writeCollectorIdentity(collectorStatePath(paths), existing);
      const registered = findCollectorInHubDatabase(paths, existing.collectorId);
      if (registered !== null) {
        const adopted: CollectorIdentity = {
          version: 1,
          collectorId: existing.collectorId,
          destinations: [{ url: destination.url, token: registered.token, sharing: { ...SYSTEM_DEFAULT_SHARING } }],
        };
        writeCollectorIdentity(collectorStatePath(paths), adopted);
        chownAccount([join(paths.stateDir, "collector")], accounts.collector, accounts.collector, host);
        return { paired: true, awaiting: false, hubWasActive, destination: destination.url };
      }
    }
    let grant: string;
    try {
      grant = createLocalEnrollmentGrant(paths, host);
    } catch (error) {
      chownAccount([join(paths.stateDir, "collector")], accounts.collector, accounts.collector, host);
      return {
        paired: false,
        awaiting: true,
        hubWasActive,
        note: `the live hub registry is unavailable: ${error instanceof Error ? error.message : String(error)}`,
      };
    }
    try {
      await enrollCollector(collectorStatePath(paths), destination.url, grant, { ...SYSTEM_DEFAULT_SHARING }, false);
    } catch (error) {
      return {
        paired: false,
        awaiting: true,
        hubWasActive,
        note: `enrollment through the live hub failed: ${error instanceof Error ? error.message : String(error)}`,
      };
    } finally {
      chownAccount([join(paths.stateDir, "collector")], accounts.collector, accounts.collector, host);
    }
    if (!collectorConfigured(paths).configured) {
      return { paired: false, awaiting: true, hubWasActive, note: "the hub accepted enrollment but no destination was saved" };
    }
    chownAccount([join(paths.stateDir, "collector")], accounts.collector, accounts.collector, host);
    return { paired: true, awaiting: false, hubWasActive, destination: destination.url };
  }

  const identity = {
    collectorId: existing?.collectorId ?? randomUUID(),
    machineId: randomUUID(),
    token: randomBytes(32).toString("base64url"),
  };
  const registry = registerCollectorInHubDatabase(paths, identity, host);
  let token = identity.token;
  if (registry.existing) {
    token = registry.existing.token;
  } else if (!registry.inserted) {
    // No authoritative registry table is available yet, so the static
    // HUB_COLLECTORS entry becomes the bootstrap identity on first start.
    const added = updateHubCollectorRegistry(paths, identity);
    if (!added.added) {
      const document = readEnvDocument(hubEnvPath(paths));
      const raw = document?.values.HUB_COLLECTORS;
      if (raw !== undefined) {
        const parsed = JSON.parse(raw) as Array<Record<string, unknown>>;
        const match = parsed.find((entry) => entry.collectorId === identity.collectorId);
        if (match && typeof match.machineId === "string" && typeof match.token === "string") token = match.token;
      }
    }
  }
  const next: CollectorIdentity = existing ?? { version: 1, collectorId: identity.collectorId, destinations: [] };
  next.destinations = [
    {
      url: destination.url,
      token,
      sharing: { ...SYSTEM_DEFAULT_SHARING },
    },
  ];
  writeCollectorIdentity(collectorStatePath(paths), next);
  chownAccount([join(paths.stateDir, "collector")], accounts.collector, accounts.collector, host);
  return { paired: true, awaiting: false, hubWasActive, destination: destination.url };
}

export type RenderHubUnitOptions = {
  paths: SystemPaths;
  accounts: SystemAccounts;
  nodePath: string;
  database: string;
  enrollment: string;
  socket: string;
  remote: boolean;
  powerAccess: boolean;
};
export type RenderCollectorUnitOptions = {
  paths: SystemPaths;
  accounts: SystemAccounts;
  nodePath: string;
  collectorState: string;
  powerAccess: boolean;
};

function unitCommon(
  name: string,
  description: string,
  paths: SystemPaths,
  account: string,
  envPath: string,
  execStart: string,
  extraService: string[] = [],
): string[] {
  return [
    "[Unit]",
    `Description=${description}`,
    `Documentation=file:${paths.prefix}/current/README.md`,
    "After=network-online.target",
    "Wants=network-online.target",
    "StartLimitIntervalSec=60",
    "StartLimitBurst=5",
    "",
    "[Service]",
    "Type=simple",
    `User=${account}`,
    `Group=${account}`,
    `WorkingDirectory=${join(paths.prefix, "current").replace(/%/g,'%%')}`,
    `EnvironmentFile=${envPath.replace(/%/g,'%%')}`,
    "Environment=NODE_ENV=production",
    "Restart=on-failure",
    "RestartSec=2s",
    "KillSignal=SIGTERM",
    "UMask=0077",
    "NoNewPrivileges=yes",
    "PrivateTmp=yes",
    "ProtectSystem=strict",
    "ProtectHome=yes",
    "ProtectKernelTunables=yes",
    "ProtectKernelModules=yes",
    "ProtectKernelLogs=yes",
    "ProtectControlGroups=yes",
    "RestrictRealtime=yes",
    "RestrictSUIDSGID=yes",
    "RestrictNamespaces=yes",
    "LockPersonality=yes",
    "RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
    "CapabilityBoundingSet=",
    "AmbientCapabilities=",
    `ExecStart=${execStart}`,
    ...extraService,
    "StandardOutput=journal",
    "StandardError=journal",
    `SyslogIdentifier=${name}`,
    "",
    "[Install]",
    "WantedBy=multi-user.target",
    "",
  ];
}

export function renderSystemHubUnit(options: RenderHubUnitOptions): string {
  const { paths, accounts } = options;
  const hubScript = join(paths.prefix, "current", "dist", "hub.cjs");
  const extra = [
    `Environment=${systemdQuote(`HUB_DATABASE_PATH=${options.database}`)}`,
    `Environment=${systemdQuote(`HUB_STATE_FILE=${options.enrollment}`)}`,
    ...(options.remote ? [`Environment=${systemdQuote(`HUB_SOCKET_PATH=${options.socket}`)}`] : []),
    `ReadWritePaths=${systemdQuote(join(paths.stateDir, "hub"))}`,
    `ReadOnlyPaths=${systemdQuote(join(paths.prefix, "current"))}`,
    "PrivateDevices=yes",
    "TimeoutStopSec=15s",
  ];
  return [
    "# Managed by `rigscope system`. Enable explicitly; do not edit in place.",
    ...unitCommon(
      "rigscope-hub",
      "Rigscope hub (system scope)",
      paths,
      accounts.hub,
      hubEnvPath(paths),
      `${systemdQuote(options.nodePath)} ${systemdQuote(hubScript)}`,
      extra,
    ),
  ].join("\n");
}

export function renderSystemCollectorUnit(options: RenderCollectorUnitOptions): string {
  const { paths, accounts } = options;
  const collectorScript = join(paths.prefix, "current", "dist", "collector.cjs");
  const extra = [
    ...(options.powerAccess ? [`SupplementaryGroups=${accounts.powerGroup}`] : []),
    `Environment=${systemdQuote(`COLLECTOR_MODE=real`)}`,
    `Environment=${systemdQuote(`COLLECTOR_STATE_FILE=${options.collectorState}`)}`,
    `Environment=${systemdQuote(`COLLECTOR_HEALTH_FILE=${join(paths.stateDir, 'collector', 'health.json')}`)}`,
    `ReadWritePaths=${systemdQuote(join(paths.stateDir, "collector"))}`,
    `ReadOnlyPaths=${systemdQuote(join(paths.prefix, "current"))}`,
    "TimeoutStopSec=10s",
  ];
  return [
    "# Managed by `rigscope system`. Enable explicitly after enrollment.",
    "# The collector is nologin and has no capability grants.",
    "# Package power reads use the private group rule installed separately.",
    "# Node receives no capabilities, no root, and no device-control access.",
    ...unitCommon(
      "rigscope-collector",
      "Rigscope collector (system scope)",
      paths,
      accounts.collector,
      collectorEnvPath(paths),
      `${systemdQuote(options.nodePath)} ${systemdQuote(collectorScript)} --await-connection`,
      extra,
    ),
  ].join("\n");
}

export function renderSysusers(paths: SystemPaths, accounts: SystemAccounts, _role: SystemRole = "local"): string {
  // One complete package: both dedicated accounts exist even when a component
  // is disabled, so the other component can be enabled without reinstalling.
  void paths;
  return [
    `# Managed by Rigscope system installation.`,
    "# Accounts are unprivileged and cannot log in.",
    `g ${accounts.hub} - -`,
    `u ${accounts.hub} - "Rigscope hub" /nonexistent /usr/sbin/nologin`,
    `g ${accounts.collector} - -`,
    `u ${accounts.collector} - "Rigscope collector" /nonexistent /usr/sbin/nologin`,
    "",
  ].join("\n");
}

export function renderPowerSysusers(_paths: SystemPaths, accounts: SystemAccounts): string {
  return [
    "# Managed by Rigscope. Grants only the collector account membership in the power group.",
    `g ${accounts.powerGroup} - -`,
    `m ${accounts.collector} ${accounts.powerGroup}`,
    "",
  ].join("\n");
}

/**
 * Package-only RAPL rule. It matches only top-level `intel-rapl:N` devices whose
 * name is `package*`; subdomains (`intel-rapl:N:M`), constraint/power-limit
 * files and the rest of sysfs are untouched. The two counters become 0440 and
 * group-readable; the device is recreated with the same rule at boot.
 */
export function renderRaplRule(accounts: SystemAccounts): string {
  return [
    "# Managed by Rigscope: read-only package energy/range counters for the collector group.",
    "# Top-level package devices only; subdomains and constraint files are not touched.",
    'SUBSYSTEM=="powercap", ACTION=="add|change", KERNEL=="intel-rapl:[0-9]*", KERNEL!="intel-rapl:*:*", ATTR{name}=="package*", \\',
    `  RUN+="/usr/bin/chgrp ${accounts.powerGroup} /sys%p/energy_uj", \\`,
    '  RUN+="/usr/bin/chmod 0440 /sys%p/energy_uj", \\',
    `  RUN+="/usr/bin/chgrp ${accounts.powerGroup} /sys%p/max_energy_range_uj", \\`,
    '  RUN+="/usr/bin/chmod 0440 /sys%p/max_energy_range_uj"',
    "",
  ].join("\n");
}

function hubEnvTemplate(paths: SystemPaths, origin?: string, port = 8081): string {
  const lines = [
    "# Rigscope hub environment. Root-owned private file, mode 0600.",
    "# Credentials remain commented out until an operator adds them: with no",
    "# browser users the hub fails closed and exposes no protected data.",
    "# Generate password hashes locally with scripts/hash-browser-password.cjs.",
    envLine("HUB_DATABASE_PATH", hubDatabasePath(paths)),
    envLine("HUB_STATE_FILE", hubEnrollmentPath(paths)),
    ...(origin
      ? [envLine("HUB_PUBLIC_ORIGIN", origin), envLine("HUB_SOCKET_PATH", hubSocketPath(paths))]
      : [envLine("HUB_HOST", "127.0.0.1"), envLine("HUB_PORT", String(port))]),
    '# HUB_BROWSER_USERS=[{"username":"operator","passwordHash":"<private hash>","machineIds":"*"}]',
    '# HUB_COLLECTORS=[{"collectorId":"...","machineId":"...","token":"..."}]',
    "",
  ];
  return lines.join("\n");
}

function collectorEnvTemplate(): string {
  return [
    "# Rigscope collector environment. Root-owned private file, mode 0600.",
    "# Enrollment writes the durable destination/token state under /var/lib/rigscope.",
    "# COLLECTOR_ENROLL_URL=ws://127.0.0.1:8081/api/collector/connect",
    "# COLLECTOR_ENROLL_TOKEN=<one-use grant>",
    "# COLLECTOR_MODE=real",
    "",
  ].join("\n");
}

export type ReleaseStage = {
  version: string;
  releaseDir: string;
  schemaVersion: number | null;
  artifacts: string[];
};

export type SystemPlan = {
  operation: string;
  apply: boolean;
  role?: SystemRole;
  component?: Component;
  components?: ComponentSet;
  paths: SystemPaths;
  accounts: SystemAccounts;
  commands: string[];
  files: Array<{ path: string; mode: string; purpose: string }>;
  notes: string[];
};

export type SystemInstallOptions = {
  apply?: boolean;
  source: string;
  role?: SystemRole;
  paths?: Partial<SystemPaths>;
  accounts?: Partial<SystemAccounts>;
  nodePath?: string;
  copyNode?: boolean;
  /** Test hook: accept a fixture executable without copying it into the prefix. */
  trustedNode?: boolean;
  port?: number;
  origin?: string;
  adoptFrom?: string;
  installDeps?: boolean;
  start?: boolean;
  enable?: boolean;
  power?: boolean;
  replaceRelease?: boolean;
  installDepsCommand?: string;
  startupTimeoutMs?: number;
  homeRoots?: string[];
  allowUserConflicts?: boolean;
  host?: SystemHost;
  now?: () => number;
};

export type SystemReport = {
  operation: string;
  ok: boolean;
  apply: boolean;
  message: string;
  component?: Component;
  components?: ComponentSet;
  awaitingConnection?: Component[];
  /** Returned only when this mutation created the hub owner. */
  password?: string;
  plan?: SystemPlan;
  version?: string;
  current?: string | null;
  previous?: string | null;
  backupPath?: string | null;
  state?: SystemInstallState | null;
  warnings?: string[];
};

type ReleaseRecord = {
  releaseDir: string;
  schemaVersion: number | null;
  installedAt: string;
};
type BackupRecord = {
  path: string;
  schemaVersion: number | null;
  fromSchema: number | null;
  toSchema: number | null;
  createdAt: string;
  fromVersion?: string;
  toVersion?: string;
};
type AdoptionRecord = {
  source: string;
  copiedAt: string;
  files: Array<{ source: string; target: string; sha256: string; bytes: number }>;
  reversible: true;
};

export type SystemInstallState = {
  formatVersion: 2;
  scope: "system";
  /** Initial install preset retained as compatibility metadata only. */
  role: SystemRole;
  active: boolean;
  components: { hub: ComponentState; collector: ComponentState };
  paths: SystemPaths;
  accounts: SystemAccounts;
  nodePath: string;
  currentVersion: string | null;
  previousVersion: string | null;
  releases: Record<string, ReleaseRecord>;
  backups: Record<string, BackupRecord>;
  adoption: AdoptionRecord | null;
  power: { enabled: boolean; rulePath: string; group: string } | null;
  installedAt: string;
  updatedAt: string;
};

export type PowerDomain = {
  path: string;
  name: string;
  energyPath: string;
  energyExists: boolean;
  energy: { mode: string; uid: number; gid: number; readable: boolean; worldReadable: boolean; writable: boolean } | null;
  maxRangePath: string;
  maxRangeExists: boolean;
  maxRange: { mode: string; uid: number; gid: number } | null;
};

export type PowerAccessReport = {
  available: boolean;
  driver: string[];
  domains: PowerDomain[];
  collectorUser: string;
  collectorUid: number | null;
  collectorGids: number[];
  group: { name: string; exists: boolean; gid: number | null; members: string[] };
  rule: { path: string; present: boolean; compliant: boolean };
  sysusers: { path: string; present: boolean };
  collectorCanRead: boolean;
  persistent: boolean;
  worldReadable: boolean;
  writableByCollector: boolean;
  problems: string[];
};

function assertPrivateSourceFile(path: string, label: string): void {
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.isSymbolicLink()) {
    throw new SystemError("unsafe-adoption", `${label} must be a regular file: ${path}`);
  }
  if ((stat.mode & 0o077) !== 0) {
    throw new SystemError("insecure-permissions", `${label} must be private (0600) before adoption: ${path}`);
  }
}

function readJsonObject(path: string): Record<string, unknown> {
  let raw: string;
  try {
    raw = readFileSync(path, "utf8");
  } catch (error) {
    throw new SystemError("missing-file", `cannot read ${path}: ${(error as Error).message}`);
  }
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    throw new SystemError("invalid-file", `${path} is not valid JSON`);
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new SystemError("invalid-file", `${path} must contain a JSON object`);
  }
  return value as Record<string, unknown>;
}

export function readSystemState(paths: SystemPaths): SystemInstallState | null {
  const path = stateFilePath(paths);
  if (!existsSync(path)) return null;
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.isSymbolicLink()) throw new SystemError("unsafe-path", `${path} must be a regular file`);
  if ((stat.mode & 0o077) !== 0) throw new SystemError("insecure-permissions", `${path} must be 0600`);
  const raw = readJsonObject(path);
  if ((raw.formatVersion !== 1 && raw.formatVersion !== SYSTEM_STATE_FORMAT_VERSION) || raw.scope !== SYSTEM_SCOPE) {
    throw new SystemError("invalid-state", `${path} is not a Rigscope system installation state`);
  }
  const savedPaths = raw.paths as SystemPaths | undefined;
  if (!savedPaths || savedPaths.prefix !== paths.prefix || savedPaths.stateDir !== paths.stateDir) {
    throw new SystemError("invalid-state", "system installation state belongs to different paths");
  }
  const savedAccounts = raw.accounts as Partial<SystemAccounts> | undefined;
  const accounts = validateAccounts(savedAccounts ?? {});
  if (raw.role !== "local" && raw.role !== "hub" && raw.role !== "collector") {
    throw new SystemError("invalid-state", "system installation state has an invalid role");
  }
  let components: SystemInstallState["components"];
  if (raw.formatVersion === 1) {
    // Read-only compatibility fallback: the legacy role maps to components in
    // memory. No file is rewritten until an explicit lifecycle mutation.
    const mapped = componentsForRole(raw.role as SystemRole);
    components = { hub: { enabled: mapped.hub }, collector: { enabled: mapped.collector } };
  } else {
    const saved = raw.components as Partial<Record<Component, Partial<ComponentState>>> | undefined;
    if (
      saved === null ||
      typeof saved !== "object" ||
      saved.hub === null ||
      saved.collector === null ||
      typeof saved.hub !== "object" ||
      typeof saved.collector !== "object" ||
      typeof saved.hub.enabled !== "boolean" ||
      typeof saved.collector.enabled !== "boolean"
    ) {
      throw new SystemError("invalid-state", "system installation state has invalid component enabled state");
    }
    components = { hub: { enabled: saved.hub.enabled }, collector: { enabled: saved.collector.enabled } };
  }
  return {
    ...(raw as unknown as SystemInstallState),
    formatVersion: SYSTEM_STATE_FORMAT_VERSION,
    accounts,
    components,
  };
}

function writeSystemState(paths: SystemPaths, state: SystemInstallState): void {
  ensureDirectory(paths.stateDir, 0o755, false);
  atomicWrite(stateFilePath(paths), `${JSON.stringify({ ...state, updatedAt: new Date().toISOString() }, null, 2)}\n`, 0o600);
}

function emptyState(options: {
  paths: SystemPaths;
  accounts: SystemAccounts;
  role: SystemRole;
  components?: ComponentSet;
  nodePath: string;
  now: () => number;
}): SystemInstallState {
  const timestamp = new Date(options.now()).toISOString();
  const components = options.components ?? componentsForRole(options.role);
  return {
    formatVersion: SYSTEM_STATE_FORMAT_VERSION,
    scope: SYSTEM_SCOPE,
    role: options.role,
    active: true,
    components: { hub: { enabled: components.hub }, collector: { enabled: components.collector } },
    paths: options.paths,
    accounts: options.accounts,
    nodePath: options.nodePath,
    currentVersion: null,
    previousVersion: null,
    releases: {},
    backups: {},
    adoption: null,
    power: null,
    installedAt: timestamp,
    updatedAt: timestamp,
  };
}

function validateVersion(value: unknown): string {
  if (typeof value !== "string" || !VERSION_PATTERN.test(value)) {
    throw new SystemError("invalid-version", `invalid release version: ${String(value)}`);
  }
  return value;
}

function releaseVersion(root: string): string {
  return validateVersion(readJsonObject(join(root, "package.json")).version);
}

function releaseSchemaVersion(root: string): number | null {
  const manifestPath = join(root, "manifest.json");
  if (!existsSync(manifestPath)) return null;
  const manifest = readJsonObject(manifestPath);
  return Number.isSafeInteger(manifest.schemaVersion) ? Number(manifest.schemaVersion) : null;
}

function assertReleaseArtifacts(root: string): void {
  for (const artifact of REQUIRED_ARTIFACTS) {
    const path = join(root, artifact);
    let stat;
    try {
      stat = lstatSync(path);
    } catch {
      throw new SystemError("missing-artifact", `release is missing ${artifact}`);
    }
    if (!stat.isFile() || stat.isSymbolicLink() || stat.size === 0) {
      throw new SystemError("invalid-artifact", `release artifact is not a non-empty regular file: ${artifact}`);
    }
  }
  const buildId = readFileSync(join(root, ".next", "BUILD_ID"), "utf8").trim();
  if (buildId === "" || /\s/.test(buildId)) throw new SystemError("invalid-artifact", ".next/BUILD_ID is invalid");
  for (const directory of [".next/server", ".next/static"]) {
    let stat;
    try {
      stat = lstatSync(join(root, directory));
    } catch {
      throw new SystemError("missing-artifact", `release is missing ${directory}/`);
    }
    if (!stat.isDirectory()) throw new SystemError("invalid-artifact", `${directory} is not a directory`);
  }
}

function verifyReleaseManifest(root: string): void {
  const manifestPath = join(root, "manifest.json");
  if (!existsSync(manifestPath)) return;
  const manifest = readJsonObject(manifestPath);
  const packageJson = readJsonObject(join(root, "package.json"));
  if (manifest.name !== undefined && manifest.name !== packageJson.name) {
    throw new SystemError("manifest-mismatch", "release manifest name does not match package.json");
  }
  if (manifest.version !== undefined && manifest.version !== packageJson.version) {
    throw new SystemError("manifest-mismatch", "release manifest version does not match package.json");
  }
  const artifacts = manifest.artifacts;
  if (!Array.isArray(artifacts) || artifacts.length === 0) return;
  const listed = new Set<string>();
  for (const raw of artifacts) {
    if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
      throw new SystemError("invalid-manifest", "release manifest contains an invalid artifact entry");
    }
    const entry = raw as Record<string, unknown>;
    const relativePath = entry.path;
    if (typeof relativePath !== "string" || relativePath === "" || relativePath.startsWith("/") || relativePath.includes("\\") || relativePath.split("/").includes("..")) {
      throw new SystemError("invalid-manifest", `release manifest contains an unsafe path: ${String(relativePath)}`);
    }
    const absolute = join(root, relativePath);
    const stat = lstatSync(absolute);
    if (!stat.isFile() || stat.isSymbolicLink()) throw new SystemError("invalid-manifest", `release artifact is not a regular file: ${relativePath}`);
    if (entry.bytes !== stat.size) throw new SystemError("artifact-mismatch", `release artifact size changed: ${relativePath}`);
    if (entry.sha256 !== sha256File(absolute)) throw new SystemError("artifact-mismatch", `release artifact hash changed: ${relativePath}`);
    listed.add(relativePath);
  }
  const visit = (current: string): void => {
    for (const entry of readdirSync(current, { withFileTypes: true })) {
      const absolute = join(current, entry.name);
      const rel = relative(root, absolute).split(sep).join("/");
      if (rel === "node_modules" || rel.startsWith("node_modules/") || rel === ".next/cache" || rel.startsWith(".next/cache/")) continue;
      if (entry.isSymbolicLink()) throw new SystemError("unsafe-release", `release contains a symlink: ${rel}`);
      if (entry.isDirectory()) {
        visit(absolute);
        continue;
      }
      if (rel === "manifest.json") continue;
      if (!listed.has(rel)) throw new SystemError("artifact-mismatch", `release contains an unlisted file: ${rel}`);
    }
  };
  visit(root);
}

function assertNoSymlinks(root: string): void {
  const visit = (path: string): void => {
    const stat = lstatSync(path);
    if (stat.isSymbolicLink()) throw new SystemError("unsafe-release", `release contains a symlink: ${relative(root, path)}`);
    if (!stat.isDirectory()) return;
    for (const entry of readdirSync(path)) visit(join(path, entry));
  };
  visit(root);
}

function copyReleaseTree(source: string, destination: string): void {
  const visit = (current: string, target: string): void => {
    const stat = lstatSync(current);
    if (stat.isSymbolicLink()) throw new SystemError("unsafe-release", `source contains a symlink: ${relative(source, current)}`);
    if (stat.isDirectory()) {
      mkdirSync(target, { recursive: true, mode: 0o755 });
      for (const entry of readdirSync(current)) {
        const child = join(current, entry);
        const rel = relative(source, child);
        if (rel === ".git" || rel === ".next-dev" || rel === ".next/cache" || rel === "node_modules" || rel.startsWith("node_modules/")) {
          continue;
        }
        visit(child, join(target, entry));
      }
      return;
    }
    if (!stat.isFile()) throw new SystemError("unsafe-release", `source contains a non-regular file: ${relative(source, current)}`);
    mkdirSync(dirname(target), { recursive: true, mode: 0o755 });
    copyFileSync(current, target);
    chmodSync(target, 0o644);
  };
  visit(source, destination);
}

function copyReleaseAllowlisted(source: string, destination: string): void {
  for (const entry of readdirSync(source)) {
    if (!PACKAGE_TOP_LEVEL.has(entry)) continue;
    if (entry === "node_modules") continue;
    const child = join(source, entry);
    const target = join(destination, entry);
    const stat = lstatSync(child);
    if (stat.isSymbolicLink()) throw new SystemError("unsafe-release", `source contains a symlink: ${entry}`);
    if (stat.isDirectory()) {
      copyReleaseTree(child, target);
    } else if (stat.isFile()) {
      copyFileSync(child, target);
      chmodSync(target, 0o644);
    }
  }
}

function extractArchive(archive: string, stage: string, host: SystemHost): void {
  const listing = runRequired(host, "tar", ["-tvzf", archive], "archive inspection");
  const lines = listing.stdout.split("\n").filter((line) => line.trim() !== "");
  if (lines.length === 0) throw new SystemError("invalid-archive", "release archive is empty");
  let strip = 0;
  const names: string[] = [];
  for (const line of lines) {
    if (/^[lhcbps]/.test(line.trim())) {
      throw new SystemError("unsafe-release", "release archive contains a link or special file");
    }
    const match = / -> /.test(line);
    if (match) throw new SystemError("unsafe-release", "release archive contains a symlink");
    const parts = line.trim().split(/\s+/);
    const name = parts[parts.length - 1] ?? "";
    names.push(name);
  }
  if (names.every((name) => name === "package" || name.startsWith("package/"))) strip = 1;
  for (const name of names) {
    const normalized = name.replace(/^package\//, "").replace(/^\.\//, "");
    if (normalized === "" || normalized === ".") continue;
    if (normalized.startsWith("/") || normalized.split("/").includes("..") || normalized.includes("\\")) {
      throw new SystemError("unsafe-release", `release archive contains an unsafe path: ${name}`);
    }
  }
  const args = ["-xzf", archive, "-C", stage, "--no-same-owner", "--no-same-permissions"];
  if (strip === 1) args.push("--strip-components=1");
  runRequired(host, "tar", args, "archive extraction");
  assertNoSymlinks(stage);
}

function stageRelease(options: {
  source: string;
  paths: SystemPaths;
  host: SystemHost;
  replaceRelease?: boolean;
  now: () => number;
}): ReleaseStage {
  const source = resolve(options.source);
  const stat = lstatSync(source);
  const releasesDir = join(options.paths.prefix, "releases");
  ensureDirectory(releasesDir, 0o755);
  const stage = join(releasesDir, `.stage-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2)}`);
  mkdirSync(stage, { recursive: true, mode: 0o700 });
  try {
    if (stat.isDirectory() && !stat.isSymbolicLink()) {
      copyReleaseAllowlisted(source, stage);
    } else if (stat.isFile() && !stat.isSymbolicLink()) {
      extractArchive(source, stage, options.host);
    } else {
      throw new SystemError("invalid-source", "release source must be a directory or .tar.gz archive");
    }
    assertReleaseArtifacts(stage);
    verifyReleaseManifest(stage);
    const version = releaseVersion(stage);
    const releaseDir = join(releasesDir, version);
    if (existsSync(releaseDir)) {
      const current = currentReleaseDir(options.paths);
      const previous = previousReleaseDir(options.paths);
      if (!options.replaceRelease || current === releaseDir || previous === releaseDir) {
        throw new SystemError(
          "release-exists",
          `release ${version} is already installed${current === releaseDir || previous === releaseDir ? " or retained" : ""}; choose a new version`,
        );
      }
      rmSync(releaseDir, { recursive: true, force: true });
    }
    renameSync(stage, releaseDir);
    ensureDirectory(join(releaseDir, ".next", "cache"), 0o755);
    return { version, releaseDir, schemaVersion: releaseSchemaVersion(releaseDir), artifacts: [...REQUIRED_ARTIFACTS] };
  } catch (error) {
    rmSync(stage, { recursive: true, force: true });
    throw error;
  }
}

function dependencyNames(root: string): string[] {
  const pkg = readJsonObject(join(root, "package.json"));
  const dependencies = pkg.dependencies;
  if (dependencies === null || typeof dependencies !== "object" || Array.isArray(dependencies)) return [];
  return Object.keys(dependencies as Record<string, unknown>).filter((name) => /^[A-Za-z0-9@/._-]+$/.test(name));
}

function missingDependencies(root: string): string[] {
  return dependencyNames(root).filter((name) => !existsSync(join(root, "node_modules", name, "package.json")));
}

function installDependencies(root: string, host: SystemHost, command = "npm"): void {
  const hasLock = existsSync(join(root, "package-lock.json"));
  const args = hasLock
    ? ["ci", "--omit=dev", "--ignore-scripts", "--no-audit", "--no-fund"]
    : ["install", "--omit=dev", "--ignore-scripts", "--no-audit", "--no-fund"];
  // If the installer was started with a user-local Node, prefer its sibling npm
  // and make that Node visible to npm's `#!/usr/bin/env node` shebang. This
  // avoids a root PATH that silently lacks Node while still requiring the
  // operator to pass --install-deps explicitly.
  const siblingNpm = join(dirname(process.execPath), "npm");
  const executable = command === "npm" && existsSync(siblingNpm) ? siblingNpm : command;
  const nodeBin = dirname(process.execPath);
  const path = [nodeBin, process.env.PATH ?? ""].filter(Boolean).join(":");
  runRequired(host, executable, args, "production dependency installation", {
    cwd: root,
    env: { ...process.env, NODE_ENV: "production", PATH: path },
    timeoutMs: 600_000,
  });
}

function currentReleaseDir(paths: SystemPaths): string | null {
  const link = join(paths.prefix, "current");
  try {
    const target = readlinkSync(link);
    const absolute = isAbsolute(target) ? target : resolve(paths.prefix, target);
    return isWithin(join(paths.prefix, "releases"), absolute) ? absolute : null;
  } catch {
    return null;
  }
}

function previousReleaseDir(paths: SystemPaths): string | null {
  const link = join(paths.prefix, "previous");
  try {
    const target = readlinkSync(link);
    const absolute = isAbsolute(target) ? target : resolve(paths.prefix, target);
    return isWithin(join(paths.prefix, "releases"), absolute) ? absolute : null;
  } catch {
    return null;
  }
}

function replaceSymlink(link: string, target: string, host: SystemHost): void {
  const temporary = `${link}.tmp-${process.pid}-${Date.now()}`;
  try {
    symlinkSync(target, temporary);
    renameSync(temporary, link);
  } catch (error) {
    try {
      unlinkSync(temporary);
    } catch {
      /* ignore */
    }
    throw error;
  }
  void host;
}

function removeSymlink(link: string): void {
  try {
    const stat = lstatSync(link);
    if (stat.isSymbolicLink()) unlinkSync(link);
  } catch {
    /* already absent */
  }
}

function nodeVersion(host: SystemHost, nodePath: string): string {
  const result = host.run(nodePath, ["--version"], { timeoutMs: 5_000 });
  const output = result.stdout.trim();
  if (result.status !== 0 || !/^v26\.\d+\.\d+$/.test(output)) {
    throw new SystemError("unsupported-node", `selected executable must run Node 26; got ${output || "no version"}`);
  }
  return output;
}

function ensureRootOwnedNode(options: {
  paths: SystemPaths;
  host: SystemHost;
  nodePath?: string;
  copyNode?: boolean;
  trustedNode?: boolean;
  apply: boolean;
}): string {
  const selected = resolve(options.nodePath ?? process.execPath);
  const real = realpathSync(selected);
  nodeVersion(options.host, real);
  const stat = statSync(real);
  const unsafe = stat.uid !== 0 || (stat.mode & 0o022) !== 0 || options.copyNode === true;
  if (!unsafe || options.trustedNode === true) return real;
  const target = join(options.paths.prefix, "node", "node");
  if (!options.apply) return target;
  ensureDirectory(join(options.paths.prefix, "node"), 0o755);
  copyFileSync(real, target);
  chmodSync(target, 0o755);
  runRequired(options.host, "chown", ["root:root", target], "node ownership");
  nodeVersion(options.host, target);
  return target;
}

type AdoptionSetup = {
  role: SystemRole;
  port: number;
  origin?: string;
  usersJson?: string;
  collectorsJson?: string;
  maxConnections?: number;
  legacy: boolean;
};

function normalizedJsonArray(value: unknown, label: string): string | undefined {
  if (value === undefined || value === null || value === "") return undefined;
  const candidate = typeof value === "string" ? value : JSON.stringify(value);
  if (typeof candidate !== "string") throw new SystemError("invalid-adoption", `${label} must be JSON`);
  let parsed: unknown;
  try {
    parsed = JSON.parse(candidate);
  } catch {
    throw new SystemError("invalid-adoption", `${label} is not valid JSON`);
  }
  if (!Array.isArray(parsed)) throw new SystemError("invalid-adoption", `${label} must be a JSON array`);
  return JSON.stringify(parsed);
}

function validatedOrigin(value: unknown): string | undefined {
  if (value === undefined || value === null || value === "") return undefined;
  if (typeof value !== "string") throw new SystemError("invalid-adoption", "adopted hub origin is not a URL");
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    throw new SystemError("invalid-adoption", "adopted hub origin is not a URL");
  }
  if (parsed.protocol !== "https:") throw new SystemError("invalid-adoption", "adopted hub origin must be https");
  return value;
}

function hasCollectorIdentity(source: string): boolean {
  return existsSync(join(source, "collector", "state.json")) || existsSync(join(source, "collector", "collector.json"));
}

function readAdoptionSetup(source: string): AdoptionSetup {
  const modernPath = join(source, "config.json");
  const legacyPath = join(source, "hub-config.json");
  if (existsSync(modernPath)) {
    assertPrivateSourceFile(modernPath, "adopted setup");
    const setup = readJsonObject(modernPath);
    const role = setup.role;
    if (setup.version !== 1 || typeof role !== "string" || !["local", "hub", "collector"].includes(role)) {
      throw new SystemError("invalid-adoption", "adopted setup version or role is incompatible");
    }
    const port = Number(setup.port ?? 8081);
    if (!Number.isInteger(port) || port < 1 || port > 65535) throw new SystemError("invalid-adoption", "adopted hub port is invalid");
    return {
      role: role as SystemRole,
      port,
      origin: validatedOrigin(setup.origin),
      usersJson: normalizedJsonArray(setup.users, "HUB_BROWSER_USERS"),
      collectorsJson: normalizedJsonArray(setup.collectors, "HUB_COLLECTORS"),
      maxConnections: Number.isSafeInteger(setup.maxCollectorConnections) ? Number(setup.maxCollectorConnections) : undefined,
      legacy: false,
    };
  }
  if (existsSync(legacyPath)) {
    assertPrivateSourceFile(legacyPath, "adopted hub configuration");
    const legacy = readJsonObject(legacyPath);
    const hasHub = ["HUB_PUBLIC_ORIGIN", "HUB_BROWSER_USERS", "HUB_DATABASE_PATH", "HUB_HOST", "HUB_PORT"].some((key) => key in legacy);
    const hasCollector = hasCollectorIdentity(source);
    if (!hasHub && !hasCollector) throw new SystemError("invalid-adoption", "adopted configuration is neither a hub nor a collector setup");
    return {
      role: hasHub && hasCollector ? "local" : hasHub ? "hub" : "collector",
      port: Number(legacy.HUB_PORT ?? 8081),
      origin: validatedOrigin(legacy.HUB_PUBLIC_ORIGIN),
      usersJson: normalizedJsonArray(legacy.HUB_BROWSER_USERS, "HUB_BROWSER_USERS"),
      collectorsJson: normalizedJsonArray(legacy.HUB_COLLECTORS, "HUB_COLLECTORS"),
      maxConnections: Number.isSafeInteger(Number(legacy.HUB_MAX_COLLECTOR_CONNECTIONS))
        ? Number(legacy.HUB_MAX_COLLECTOR_CONNECTIONS)
        : undefined,
      legacy: true,
    };
  }
  if (hasCollectorIdentity(source) && existsSync(join(source, "collector"))) {
    return { role: "collector", port: 8081, legacy: true };
  }
  throw new SystemError("invalid-adoption", "adopted installation has no config.json or hub-config.json");
}

function resolveRole(options: { role?: SystemRole; adoptFrom?: string }): SystemRole {
  const role = options.role ?? "local";
  if (!["local", "hub", "collector"].includes(role)) {
    throw new SystemError("invalid-role", "--role must be local, hub or collector");
  }
  if (options.adoptFrom) {
    const setup = readAdoptionSetup(options.adoptFrom);
    if (options.role !== undefined && options.role !== setup.role && setup.role !== 'local') {
      throw new SystemError("role-mismatch", `the adopted setup is ${setup.role}; do not change its role during adoption`);
    }
    return options.role ?? setup.role;
  }
  return role;
}

export type UserInstallation = {
  user: string;
  home: string;
  unitPath: string;
  unitName: string;
  execStart: string | null;
  stateDir: string | null;
};

export function detectUserInstallations(options: { homeRoots?: string[] } = {}): UserInstallation[] {
  const roots = options.homeRoots ?? ["/home", "/root"];
  const found: UserInstallation[] = [];
  for (const homeRoot of roots) {
    if (!existsSync(homeRoot)) continue;
    const homes = homeRoot === "/root" ? ["/root"] : readdirSync(homeRoot).map((entry) => join(homeRoot, entry));
    for (const home of homes) {
      let stat;
      try {
        stat = lstatSync(home);
      } catch {
        continue;
      }
      if (!stat.isDirectory()) continue;
      const unitDir = join(home, ".config", "systemd", "user");
      let units: string[] = [];
      try {
        units = readdirSync(unitDir);
      } catch {
        units = [];
      }
      for (const unitName of units) {
        if (!/^rigscope(?:-[0-9a-f]{12})?(?:-(?:hub|collector))?\.service$/.test(unitName)) continue;
        const unitPath = join(unitDir, unitName);
        let execStart: string | null = null;
        try {
          const line = readFileSync(unitPath, "utf8").split("\n").find((item) => item.startsWith("ExecStart="));
          execStart = line ? line.slice("ExecStart=".length) : null;
        } catch {
          /* unreadable units are still reported */
        }
        const stateCandidates = [join(home, ".local", "state", "rigscope")];
        const stateDir = stateCandidates.find((candidate) => existsSync(candidate)) ?? null;
        found.push({ user: basename(home), home, unitPath, unitName, execStart, stateDir });
      }
    }
  }
  return found;
}

function lockIsLive(path: string): { exists: boolean; live: boolean; pid: number | null; ambiguous: boolean } {
  try {
    const stat = lstatSync(path);
    if (!stat.isFile() || stat.size > 4096) return { exists: true, live: false, pid: null, ambiguous: true };
    const owner = JSON.parse(readFileSync(path, "utf8")) as { pid?: unknown };
    const pid = owner.pid;
    if (!Number.isSafeInteger(pid) || (pid as number) < 1) return { exists: true, live: false, pid: null, ambiguous: true };
    try {
      process.kill(pid as number, 0);
      return { exists: true, live: true, pid: pid as number, ambiguous: false };
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code;
      if (code === "ESRCH") return { exists: true, live: false, pid: pid as number, ambiguous: false };
      return { exists: true, live: true, pid: pid as number, ambiguous: true };
    }
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return { exists: false, live: false, pid: null, ambiguous: false };
    return { exists: true, live: false, pid: null, ambiguous: true };
  }
}

function directoryLocks(directory: string): string[] {
  return [
    join(directory, "hub", "telemetry.sqlite.hub.lock"),
    join(directory, "run", "rigscope.lock"),
    join(directory, "collector", "collector-runtime.lock"),
  ];
}

export function assertDirectoryStopped(
  directory: string,
  scope: SystemRole | Component | Component[] | ComponentSet = "local",
): void {
  const components = typeof scope === "string" && (scope === "local" || scope === "hub" || scope === "collector")
    ? componentsForRole(scope)
    : componentSet(scope as Component | Component[] | ComponentSet);
  const locks = directoryLocks(directory).filter((path) =>
    (!path.includes("/collector/") || components.collector) &&
    (path.includes("/collector/") || components.hub));
  for (const lock of locks) {
    const state = lockIsLive(lock);
    if (state.live || state.ambiguous) {
      throw new SystemError(
        "service-active",
        `stop the existing installation before adoption; a live or ambiguous runtime lock exists: ${lock}`,
      );
    }
  }
}

function systemLockState(paths: SystemPaths): { live: boolean; path: string; pid: number | null; ambiguous: boolean } {
  const path = `${hubDatabasePath(paths)}.hub.lock`;
  const state = lockIsLive(path);
  return { live: state.live || state.ambiguous, path, pid: state.pid, ambiguous: state.ambiguous };
}

function assertSystemStartAllowed(paths: SystemPaths, components: Component | Component[] | ComponentSet): void {
  if (componentSet(components).hub) {
    const lock = systemLockState(paths);
    if (lock.live) {
      throw new SystemError("duplicate-runtime", `a hub runtime already owns ${lock.path}; stop it before starting another`);
    }
  }
}

export function detectPowerDriver(moduleRoot = "/sys/module"): string[] {
  const candidates = ["intel_rapl_msr", "intel_rapl_common", "intel_rapl_tpmi", "intel_rapl", "amd_energy"];
  return candidates.filter((name) => existsSync(join(moduleRoot, name)));
}

function readPasswd(path = "/etc/passwd"): Map<string, { uid: number; gid: number }> {
  const users = new Map<string, { uid: number; gid: number }>();
  let lines: string[];
  try {
    lines = readFileSync(path, "utf8").split("\n");
  } catch {
    return users;
  }
  for (const line of lines) {
    const parts = line.split(":");
    if (parts.length < 4) continue;
    const uid = Number(parts[2]);
    const gid = Number(parts[3]);
    if (parts[0] && Number.isSafeInteger(uid) && Number.isSafeInteger(gid)) users.set(parts[0]!, { uid, gid });
  }
  return users;
}

type GroupEntry = { gid: number; members: string[] };
function readGroups(path = "/etc/group"): Map<string, GroupEntry> {
  const groups = new Map<string, GroupEntry>();
  let lines: string[];
  try {
    lines = readFileSync(path, "utf8").split("\n");
  } catch {
    return groups;
  }
  for (const line of lines) {
    const parts = line.split(":");
    if (parts.length < 4) continue;
    const gid = Number(parts[2]);
    if (!parts[0] || !Number.isSafeInteger(gid)) continue;
    groups.set(parts[0], { gid, members: parts[3] ? parts[3].split(",").filter(Boolean) : [] });
  }
  return groups;
}

function modeString(mode: number): string {
  return `0${(mode & 0o777).toString(8).padStart(3, "0")}`;
}

export function inspectPowerAccess(options: {
  paths?: Partial<SystemPaths>;
  accounts?: Partial<SystemAccounts>;
  passwdFile?: string;
  groupFile?: string;
  moduleRoot?: string;
  rulePath?: string;
  sysusersPath?: string;
  /** Fixture hook: substitute device-file ownership and modes without root. */
  statOverride?: (path: string) => { mode: number; uid: number; gid: number } | null;
} = {}): PowerAccessReport {
  const paths = validateSystemPaths(options.paths ?? {});
  const accounts = validateAccounts(options.accounts ?? {});
  const users = readPasswd(options.passwdFile ?? "/etc/passwd");
  const groups = readGroups(options.groupFile ?? "/etc/group");
  const collector = users.get(accounts.collector) ?? null;
  const group = groups.get(accounts.powerGroup) ?? null;
  const collectorGids = collector
    ? [collector.gid, ...(group?.members.includes(accounts.collector) ? [group.gid] : [])]
    : [];
  const rulePath = options.rulePath ?? raplRulePath(paths);
  const sysusers = options.sysusersPath ?? powerSysusersPath(paths);
  let ruleText: string | null = null;
  try {
    ruleText = readFileSync(rulePath, "utf8");
  } catch {
    ruleText = null;
  }
  const requiredFragments = [
    'SUBSYSTEM=="powercap"',
    'KERNEL=="intel-rapl:[0-9]*"',
    'ATTR{name}=="package*"',
    `/sys%p/energy_uj`,
    `/sys%p/max_energy_range_uj`,
    `chgrp ${accounts.powerGroup}`,
    "chmod 0440",
  ];
  const compliant = ruleText !== null && requiredFragments.every((fragment) => ruleText!.includes(fragment));

  const domains: PowerDomain[] = [];
  let entries: string[] = [];
  try {
    entries = readdirSync(paths.powercapRoot);
  } catch {
    entries = [];
  }
  for (const entry of entries.filter((name) => /^intel-rapl:\d+$/.test(name)).sort()) {
    const domainPath = join(paths.powercapRoot, entry);
    let name = entry;
    try {
      name = readFileSync(join(domainPath, "name"), "utf8").trim() || entry;
    } catch {
      /* keep the device name */
    }
    if (!/^package/.test(name)) continue;
    const energyPath = join(domainPath, "energy_uj");
    const maxRangePath = join(domainPath, "max_energy_range_uj");
    const readStat = (path: string): { mode: string; uid: number; gid: number } | null => {
      if (options.statOverride) {
        const override = options.statOverride(path);
        return override === null ? null : { mode: modeString(override.mode), uid: override.uid, gid: override.gid };
      }
      try {
        const stat = statSync(path);
        return { mode: modeString(stat.mode), uid: stat.uid, gid: stat.gid };
      } catch {
        return null;
      }
    };
    const energyStat = readStat(energyPath);
    const maxStat = readStat(maxRangePath);
    const energyMode = energyStat === null ? 0 : Number.parseInt(energyStat.mode, 8);
    const readable = energyStat !== null && (
      collector !== null && ((energyStat.uid === collector.uid && (energyMode & 0o400) !== 0) ||
      (collectorGids.includes(energyStat.gid) && (energyMode & 0o040) !== 0) ||
      (energyMode & 0o004) !== 0)
    );
    domains.push({
      path: domainPath,
      name,
      energyPath,
      energyExists: energyStat !== null,
      energy: energyStat === null ? null : {
        ...energyStat,
        readable,
        worldReadable: (energyMode & 0o004) !== 0,
        writable: (energyMode & 0o222) !== 0,
      },
      maxRangePath,
      maxRangeExists: maxStat !== null,
      maxRange: maxStat,
    });
  }
  const problems: string[] = [];
  if (domains.length === 0) problems.push("no top-level package powercap counters were found");
  if (!collector) problems.push(`collector account ${accounts.collector} does not exist`);
  if (!group) problems.push(`power group ${accounts.powerGroup} does not exist`);
  if (collector && group && !group.members.includes(accounts.collector)) {
    problems.push(`collector account ${accounts.collector} is not a member of ${accounts.powerGroup}`);
  }
  if (!compliant) problems.push(`persistent udev rule is missing or not compliant: ${rulePath}`);
  if (!existsSync(sysusers)) problems.push(`power sysusers definition is missing: ${sysusers}`);
  for (const domain of domains) {
    if (!domain.energyExists) problems.push(`${domain.name}: energy_uj is unavailable`);
    else if (domain.energy?.worldReadable) problems.push(`${domain.name}: energy_uj is world-readable`);
    else if (domain.energy?.writable) problems.push(`${domain.name}: energy_uj is writable`);
    else if (!domain.energy?.readable) problems.push(`${domain.name}: collector cannot read energy_uj`);
  }
  const worldReadable = domains.some((domain) => domain.energy?.worldReadable === true);
  const writableByCollector = domains.some((domain) => domain.energy?.writable === true);
  const collectorCanRead = domains.length > 0 && domains.every((domain) => domain.energy?.readable === true);
  return {
    available: domains.length > 0,
    driver: detectPowerDriver(options.moduleRoot ?? "/sys/module"),
    domains,
    collectorUser: accounts.collector,
    collectorUid: collector?.uid ?? null,
    collectorGids,
    group: { name: accounts.powerGroup, exists: group !== null, gid: group?.gid ?? null, members: group?.members ?? [] },
    rule: { path: rulePath, present: ruleText !== null, compliant },
    sysusers: { path: sysusers, present: existsSync(sysusers) },
    collectorCanRead,
    persistent: compliant && group !== null && collector !== null && group.members.includes(accounts.collector),
    worldReadable,
    writableByCollector,
    problems,
  };
}

async function backupDatabase(source: string, output: string, host: SystemHost): Promise<void> {
  ensureDirectory(dirname(output), 0o700, true);
  const db = new DatabaseSync(source, { readOnly: true, timeout: 5_000 });
  try {
    await sqliteBackup(db, output);
  } finally {
    db.close();
  }
  chmodSync(output, 0o600);
  const check = new DatabaseSync(output, { readOnly: true });
  try {
    const result = check.prepare("PRAGMA integrity_check").get() as Record<string, unknown> | undefined;
    if (!result || Object.values(result)[0] !== "ok") throw new SystemError("invalid-backup", `backup integrity check failed: ${output}`);
  } finally {
    check.close();
  }
  void host;
}

function databaseSchema(path: string): number | null {
  if (!existsSync(path)) return null;
  const db = new DatabaseSync(path, { readOnly: true, timeout: 5_000 });
  try {
    const table = db.prepare("SELECT name FROM sqlite_master WHERE type='table' AND name='schema_migrations'").get();
    if (!table) return null;
    const row = db.prepare("SELECT MAX(version) AS version FROM schema_migrations").get() as { version?: unknown } | undefined;
    return row && Number.isSafeInteger(Number(row.version)) ? Number(row.version) : null;
  } finally {
    db.close();
  }
}

function replaceDatabaseFromBackup(backupPath: string, target: string, safetyBackup: string | null, host: SystemHost): void {
  assertPrivateSourceFile(backupPath, 'database backup');
  const backup = new DatabaseSync(backupPath,{readOnly:true});
  try {
    if (Object.values(backup.prepare('PRAGMA integrity_check').get()!)[0] !== 'ok') throw new SystemError('invalid-backup','rollback backup integrity failed');
  } finally {backup.close();}
  ensureDirectory(dirname(target), 0o700, true);
  if (safetyBackup !== null && existsSync(target)) {
    // The caller takes the safety backup asynchronously before this helper.
    void host;
  }
  const temporary = join(dirname(target), `.restore-${process.pid}-${Date.now()}.sqlite`);
  copyFileSync(backupPath, temporary);
  chmodSync(temporary, 0o600);
  for (const sidecar of [`${target}-wal`, `${target}-shm`]) {
    try {
      unlinkSync(sidecar);
    } catch {
      /* absent */
    }
  }
  renameSync(temporary, target);
  chmodSync(target, 0o600);
}

function newestTimestamp(): string {
  return new Date().toISOString().replace(/[:.]/g, "-");
}

type AdoptionResult = {
  hubEnv: string | null;
  collectorEnv: string | null;
  record: AdoptionRecord;
  warnings: string[];
  remote: boolean;
  port: number;
};

function adoptUserState(options: {
  source: string;
  paths: SystemPaths;
  role: SystemRole;
  accounts: SystemAccounts;
  apply: boolean;
  host: SystemHost;
  now: () => number;
}): AdoptionResult {
  const source = resolve(options.source);
  if (!existsSync(source)) throw new SystemError("invalid-adoption", `user installation does not exist: ${source}`);
  if (isWithin(options.paths.prefix, source) || isWithin(options.paths.stateDir, source) || isWithin(source, options.paths.stateDir)) {
    throw new SystemError("invalid-adoption", "adoption source and system paths must not overlap");
  }
  assertDirectoryStopped(source,options.role);
  const setup = readAdoptionSetup(source);
  if (setup.role !== options.role && setup.role !== 'local') throw new SystemError("invalid-adoption", "adopted setup role is incompatible");
  const { port, origin } = setup;
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new SystemError("invalid-adoption", "adopted hub port is invalid");

  const copied: AdoptionRecord["files"] = [];
  const warnings: string[] = [];
  const targetDatabase = hubDatabasePath(options.paths);
  const targetCollector = collectorStatePath(options.paths);
  if (options.apply && existsSync(targetDatabase)) {
    throw new SystemError("destination-exists", `refusing to overwrite ${targetDatabase}`);
  }

  const hubEnv = options.role === "collector"
    ? null
    : [
      "# Adopted by `rigscope system install --adopt-user-state`.",
      "# Credentials were copied into this root-owned 0600 file; the original user setup is untouched.",
      envLine("HUB_DATABASE_PATH", targetDatabase),
      envLine("HUB_STATE_FILE", hubEnrollmentPath(options.paths)),
      ...(origin
        ? [envLine("HUB_PUBLIC_ORIGIN", origin), envLine("HUB_SOCKET_PATH", hubSocketPath(options.paths))]
        : [envLine("HUB_HOST", "127.0.0.1"), envLine("HUB_PORT", String(port))]),
      ...(setup.usersJson !== undefined ? [envLine("HUB_BROWSER_USERS", setup.usersJson)] : []),
      ...(setup.collectorsJson !== undefined ? [envLine("HUB_COLLECTORS", setup.collectorsJson)] : []),
      ...(setup.maxConnections !== undefined
        ? [envLine("HUB_MAX_COLLECTOR_CONNECTIONS", String(setup.maxConnections))]
        : []),
      "",
    ].join("\n");
  const collectorEnv = options.role === "hub"
    ? null
    : [
      "# Adopted by `rigscope system install --adopt-user-state`.",
      envLine("COLLECTOR_MODE", "real"),
      envLine("COLLECTOR_STATE_FILE", targetCollector),
      "",
    ].join("\n");

  void options.accounts;
  return {
    hubEnv,
    collectorEnv,
    record: {
      source,
      copiedAt: new Date(options.now()).toISOString(),
      files: copied,
      reversible: true,
    },
    warnings,
    remote: origin !== undefined,
    port,
  };
}

function copyAdoptedTree(source: string, target: string, copied: AdoptionRecord["files"], skipLocks = true): void {
  const visit = (current: string, destination: string): void => {
    const stat = lstatSync(current);
    if (stat.isSymbolicLink()) return;
    if (stat.isDirectory()) {
      mkdirSync(destination, { recursive: true, mode: 0o700 });
      for (const entry of readdirSync(current)) visit(join(current, entry), join(destination, entry));
      return;
    }
    if (!stat.isFile()) return;
    if (skipLocks && /\.(lock|sock)$/.test(current)) return;
    mkdirSync(dirname(destination), { recursive: true, mode: 0o700 });
    copyFileSync(current, destination);
    chmodSync(destination, 0o600);
    copied.push({ source: current, target: destination, sha256: sha256File(destination), bytes: stat.size });
  };
  visit(source, target);
}

async function performAdoptionCopyAsync(options: {
  source: string;
  paths: SystemPaths;
  role: SystemRole;
  host: SystemHost;
  copy: AdoptionResult;
}): Promise<void> {
  const { source, paths, role, host, copy } = options;
  assertDirectoryStopped(source,role);
  const targetDatabase = hubDatabasePath(paths);
  const sourceDatabase = join(source, "hub", "telemetry.sqlite");
  if (role !== "collector" && existsSync(sourceDatabase) && !existsSync(targetDatabase)) {
    assertPrivateSourceFile(sourceDatabase, "adopted database");
    ensureDirectory(join(paths.stateDir, "hub"), 0o700, true);
    await backupDatabase(sourceDatabase, targetDatabase, host);
    copy.record.files.push({
      source: sourceDatabase,
      target: targetDatabase,
      sha256: sha256File(targetDatabase),
      bytes: statSync(targetDatabase).size,
    });
  }
  const sourceCollectorState = [
    join(source, "collector", "state.json"),
    join(source, "collector", "collector.json"),
  ].find((candidate) => existsSync(candidate));
  if (role !== "hub" && sourceCollectorState) {
    assertPrivateSourceFile(sourceCollectorState, "adopted collector state");
    ensureDirectory(join(paths.stateDir, "collector"), 0o700, true);
    const targetCollector = collectorStatePath(paths);
    copyFileSync(sourceCollectorState, targetCollector);
    chmodSync(targetCollector, 0o600);
    copy.record.files.push({
      source: sourceCollectorState,
      target: targetCollector,
      sha256: sha256File(targetCollector),
      bytes: statSync(targetCollector).size,
    });
    const outbox = join(source, "collector", "outbox");
    if (existsSync(outbox)) copyAdoptedTree(outbox, join(paths.stateDir, "collector", "outbox"), copy.record.files);
  }
  const enrollment = join(source, "hub", "enrollment.json");
  if (role !== "collector" && existsSync(enrollment)) {
    const target = hubEnrollmentPath(paths);
    copyFileSync(enrollment, target);
    chmodSync(target, 0o600);
    copy.record.files.push({ source: enrollment, target, sha256: sha256File(target), bytes: statSync(target).size });
  }
}

function writeSysusersFiles(paths: SystemPaths, accounts: SystemAccounts, role: SystemRole, power: boolean, host: SystemHost): void {
  ensureDirectory(paths.sysusersDir, 0o755);
  writeManagedFile(sysusersPath(paths), renderSysusers(paths, accounts, role), 0o644, true);
  if (power) {
    writeManagedFile(powerSysusersPath(paths), renderPowerSysusers(paths, accounts), 0o644, true);
  }
  runRequired(host, "systemd-sysusers", [], "system account creation");
  assertSystemAccounts(host, accounts, power, [...COMPONENTS]);
}

function assertSystemAccounts(
  host: SystemHost,
  accounts: SystemAccounts,
  power: boolean,
  required: readonly Component[] = COMPONENTS,
): void {
  const checked = required.map((component) => component === "hub" ? accounts.hub : accounts.collector);
  for (const account of checked) {
    const fields = runRequired(host, 'getent', ['passwd', account], 'service account inspection').stdout.trim().split(':');
    if (fields[0] !== account || !(Number(fields[2]) > 0) || !(Number(fields[3]) > 0) || !/\/(nologin|false)$/.test(fields[6] ?? '')) {
      throw new SystemError('unsafe-account', `service account ${account} must be non-root with a nologin shell`);
    }
  }
  if (power) {
    const fields = runRequired(host, 'getent', ['group', accounts.powerGroup], 'power group inspection').stdout.trim().split(':');
    const members = (fields[3] ?? '').split(',').filter(Boolean);
    if (!(Number(fields[2]) > 0) || members.length !== 1 || members[0] !== accounts.collector) {
      throw new SystemError('unsafe-group', 'RAPL group must contain only the dedicated collector');
    }
    const passwd = runRequired(host, 'getent', ['passwd'], 'power group primary membership inspection').stdout;
    if (passwd.split('\n').some(line => {const p=line.split(':');return p[3] === fields[2] && p[0] !== accounts.collector;})) {
      throw new SystemError('unsafe-group', 'another account uses the RAPL group as its primary group');
    }
  }
}

function writePowerRule(paths: SystemPaths, accounts: SystemAccounts): void {
  ensureDirectory(paths.udevRulesDir, 0o755);
  writeManagedFile(raplRulePath(paths), renderRaplRule(accounts), 0o644, true);
}

type PowerOriginals = {
  files: Record<string, { mode: number; uid: number; gid: number }>;
};
function powerOriginalPath(paths: SystemPaths): string {
  return join(paths.stateDir, POWER_STATE_FILE_NAME);
}
function capturePowerOriginals(paths: SystemPaths): void {
  const files: PowerOriginals["files"] = {};
  for (const domain of packageDomainPaths(paths.powercapRoot)) {
    for (const name of ["energy_uj", "max_energy_range_uj"]) {
      const path = join(domain, name);
      try {
        const stat = statSync(path);
        files[path] = { mode: stat.mode & 0o777, uid: stat.uid, gid: stat.gid };
      } catch {
        /* absent counters are not recorded */
      }
    }
  }
  atomicWrite(powerOriginalPath(paths), `${JSON.stringify({ files }, null, 2)}\n`, 0o600);
}
function applyPowerAccess(paths: SystemPaths, accounts: SystemAccounts, host: SystemHost): void {
  for (const domain of packageDomainPaths(paths.powercapRoot)) {
    for (const name of ["energy_uj", "max_energy_range_uj"]) {
      const path = join(domain, name);
      if (!existsSync(path)) continue;
      runRequired(host, "chgrp", [accounts.powerGroup, path], `power counter group ${path}`);
      runRequired(host, "chmod", ["0440", path], `power counter mode ${path}`);
    }
  }
}
function resumePowerRule(paths: SystemPaths, host: SystemHost): void {
  runRequired(host, "udevadm", ["control", "--reload-rules"], 'udev rule reload');
  runRequired(host, "udevadm", ["trigger", "--subsystem-match=powercap", "--action=change"], 'powercap rule application');
  runRequired(host, "udevadm", ["settle", "--timeout=10"], 'powercap event completion');
}
function packageDomainPaths(root: string): string[] {
  try {
    return readdirSync(root)
      .filter((entry) => /^intel-rapl:\d+$/.test(entry))
      .map((entry) => join(root, entry))
      .filter((path) => {
        try {
          return /^package/.test(readFileSync(join(path, "name"), "utf8").trim());
        } catch {
          return false;
        }
      })
      .sort();
  } catch {
    return [];
  }
}

function restorePowerOriginals(paths: SystemPaths, host: SystemHost): string[] {
  const restored: string[] = [];
  let originals: PowerOriginals | null = null;
  try {
    originals = readJsonObject(powerOriginalPath(paths)) as unknown as PowerOriginals;
  } catch {
    originals = null;
  }
  const files = { ...(originals?.files ?? {}) };
  for (const domain of packageDomainPaths(paths.powercapRoot)) {
    for (const name of ["energy_uj", "max_energy_range_uj"]) {
      const path = join(domain, name);
      if (!existsSync(path)) continue;
      const original = files[path];
      if (original) {
        runRequired(host, "chown", [`${original.uid}:${original.gid}`, path], 'restore counter ownership');
        runRequired(host, "chmod", [modeString(original.mode).slice(1), path], 'restore counter mode');
      } else if (name === "energy_uj") {
        runRequired(host, "chown", ["root:root", path], 'restore counter ownership');
        runRequired(host, "chmod", ["0400", path], 'restore counter mode');
      } else {
        runRequired(host, "chown", ["root:root", path], 'restore counter ownership');
        runRequired(host, "chmod", ["0444", path], 'restore counter mode');
      }
      restored.push(path);
    }
  }
  try {
    unlinkSync(powerOriginalPath(paths));
  } catch {
    /* absent */
  }
  return restored;
}

function systemdActive(host: SystemHost, unit: string): boolean {
  return host.run("systemctl", ["is-active", "--quiet", unit], {timeoutMs:1000}).status === 0;
}

function systemdEnabled(host: SystemHost, unit: string): boolean {
  return host.run("systemctl", ["is-enabled", "--quiet", unit]).status === 0;
}

function stopUnits(host: SystemHost, units: string[]): void {
  for (const unit of units) {
    const stopped=host.run('systemctl',['stop',unit]);
    if (stopped.status !== 0) {
      const load=host.run('systemctl',['show',unit,'--property=LoadState','--value']).stdout.trim();
      if (!['not-found','bad-setting','error'].includes(load) || systemdActive(host,unit)) {
        throw new SystemError('stop-failed',`could not stop ${unit}; no runtime files may be restored`);
      }
    }
  }
  if (units.some(unit => systemdActive(host, unit))) throw new SystemError('service-active', 'system services did not stop; no files were restored');
}
function startUnits(host: SystemHost, units: string[]): void {
  runRequired(host, "systemctl", ["daemon-reload"], "systemd daemon-reload");
  for (const unit of units) runRequired(host, "systemctl", ["start", unit], `starting ${unit}`);
}
function enableUnits(host: SystemHost, units: string[]): void {
  runRequired(host, "systemctl", ["daemon-reload"], "systemd daemon-reload");
  for (const unit of units) runRequired(host, "systemctl", ["enable", unit], `enabling ${unit}`);
}

async function waitForUnits(host: SystemHost, units: string[], timeoutMs: number): Promise<boolean> {
  if (timeoutMs <= 0) return units.every((unit) => systemdActive(host, unit));
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (units.every((unit) => systemdActive(host, unit))) return true;
    await new Promise((resolveDelay) => setTimeout(resolveDelay, 250));
  }
  return units.every((unit) => systemdActive(host, unit));
}

function readHubEnvironment(paths: SystemPaths): Record<string, string> {
  return readEnvDocument(hubEnvPath(paths))?.values ?? {};
}

export async function verifySystemHealth(
  paths: SystemPaths,
  state: SystemInstallState,
  host: SystemHost,
  timeoutMs: number,
  components: Component | Component[] | ComponentSet = enabledComponents(state),
): Promise<void> {
  if (host.verifyHealth) return host.verifyHealth(paths, state, timeoutMs);
  const selected = componentSet(components);
  const selectedUnits = COMPONENTS.filter((component) => selected[component]).map(unitFor);
  if (selectedUnits.length === 0) return;
  const deadline = Date.now() + timeoutMs;
  if (!await waitForUnits(host, selectedUnits, timeoutMs)) throw new SystemError('startup-failed', 'system units did not become active');
  if (selected.hub) {
    const env = readHubEnvironment(paths);
    const target: HealthTarget = env.HUB_PUBLIC_ORIGIN
      ? {kind:'unix',socketPath:hubSocketPath(paths),hostHeader:new URL(env.HUB_PUBLIC_ORIGIN).host,path:'/api/health'}
      : {kind:'loopback',host:'127.0.0.1',port:Number(env.HUB_PORT ?? 8081),path:'/api/health'};
    const health = await waitForHealth(target, {expectedVersion:state.currentVersion!,deadlineMs:Math.max(100,deadline-Date.now())});
    if (!health.ok) throw new SystemError('startup-failed', `system hub readiness failed: ${health.last.error}`);
    const report = runRequired(host,state.nodePath,[join(paths.prefix,'current/dist/ops.cjs'),'inspect','--database',hubDatabasePath(paths),'--json'],
      'activated database verification',{timeoutMs:Math.max(100,deadline-Date.now())});
    const info = JSON.parse(report.stdout);
    if (!info.integrityOk || info.newerSchema || info.ledgerProblem || info.schemaVersion !== info.supportedSchemaVersion) {
      throw new SystemError('database-unready', 'activated hub database schema/integrity is not ready');
    }
    if (!info.tables?.some((table: {name:string; rows?:number}) => table.name === 'users' && Number(table.rows)>0)) {
      throw new SystemError('users-unconfigured','hub has no managed browser users; configure its private bootstrap users before first start');
    }
  }
  if (selected.collector) {
    let problem = 'no sampling marker';
    while (Date.now() < deadline) {
      try {
        const marker = readJsonObject(join(paths.stateDir,'collector/health.json'));
        const lease = readJsonObject(join(paths.stateDir,'collector/collector-runtime.lock'));
        if (marker.pid !== lease.pid || !Number.isSafeInteger(marker.pid)) throw new Error('sampling marker belongs to another process');
        process.kill(Number(marker.pid),0);
        if (marker.version !== state.currentVersion || !Number.isFinite(marker.sampledAt) || Date.now()-Number(marker.sampledAt)>15_000) throw new Error('collector version or sampling freshness is wrong');
        return;
      } catch (error) {problem = error instanceof Error ? error.message : String(error);}
      await new Promise(resolve => setTimeout(resolve,100));
    }
    throw new SystemError('collector-unready',`system collector did not sample: ${problem}`);
  }
}

function chownAccount(paths: string[], owner: string, group: string, host: SystemHost): void {
  for (const path of paths) {
    if (!existsSync(path)) continue;
    runRequired(host, "chown", ["-R", `${owner}:${group}`, path], `ownership ${path}`);
  }
}

function chmodTree(path: string, host: SystemHost): void {
  if (!existsSync(path)) return;
  runRequired(host, "chmod", ["-R", "u=rwX,go=rX", path], `release permissions ${path}`);
}

function pickRollbackBackup(state: SystemInstallState, previousSchema: number | null): BackupRecord | null {
  const backups = Object.values(state.backups).sort((a, b) => a.createdAt.localeCompare(b.createdAt));
  if (previousSchema === null) return null;
  return [...backups].reverse().find((backup) => backup.fromVersion === state.previousVersion && backup.toVersion === state.currentVersion && backup.schemaVersion !== null && backup.schemaVersion <= previousSchema) ?? null;
}

async function recoverFailedUpgrade(options: {
  paths: SystemPaths;
  state: SystemInstallState;
  oldCurrent: string;
  oldPrevious: string | null;
  beforeSchema: number | null;
  backupPath: string | null;
  host: SystemHost;
  activeComponents: Component[];
  units: string[];
}): Promise<void> {
  stopUnits(options.host, options.units);
  assertDirectoryStopped(options.paths.stateDir);
  replaceSymlink(join(options.paths.prefix, "current"), options.oldCurrent, options.host);
  if (options.oldPrevious === null) removeSymlink(join(options.paths.prefix, "previous"));
  else replaceSymlink(join(options.paths.prefix, "previous"), options.oldPrevious, options.host);
  const database = hubDatabasePath(options.paths);
  let afterSchema: number | null = null;
  try {
    afterSchema = databaseSchema(database);
  } catch {
    afterSchema = null;
  }
  if (
    options.backupPath !== null &&
    existsSync(options.backupPath) &&
    (afterSchema === null || options.beforeSchema === null || afterSchema > options.beforeSchema)
  ) {
    await backupDatabase(database, join(options.paths.stateDir, "backups", `failed-upgrade-${newestTimestamp()}.sqlite`), options.host);
    replaceDatabaseFromBackup(options.backupPath, database, null, options.host);
    runRequired(options.host,'chown',[`${options.state.accounts.hub}:${options.state.accounts.hub}`,database],'restored database ownership');
  }
  options.state.previousVersion = options.oldPrevious === null ? null : basename(options.oldPrevious);
  options.state.currentVersion = basename(options.oldCurrent);
  writeSystemState(options.paths, options.state);
  if (options.activeComponents.length > 0) {
    startUnits(options.host, options.units);
    await verifySystemHealth(options.paths, options.state, options.host, 20_000, options.activeComponents);
  }
}

export function assertManagedFileOwnership(paths: SystemPaths, path: string): void {
  if (!isDefaultSystemLayout(paths)) return;
  if (!existsSync(path)) return;
  const stat = lstatSync(path);
  if (stat.isSymbolicLink() || !stat.isFile()) throw new SystemError("unsafe-path", `${path} must be a regular managed file`);
  if (stat.uid !== 0) throw new SystemError("insecure-ownership", `${path} must be root-owned before replacement`);
  if ((stat.mode & 0o022) !== 0) throw new SystemError("insecure-permissions", `${path} must not be group/world-writable`);
}

function writeUnitFile(paths: SystemPaths, unit: string, content: string, replace: boolean): void {
  const path = join(paths.unitsDir, unit);
  assertManagedFileOwnership(paths, path);
  writeManagedFile(path, content, 0o644, replace);
}

function currentReleaseOrThrow(paths: SystemPaths): string {
  const current = currentReleaseDir(paths);
  if (current === null) throw new SystemError("invalid-state", "the active release is missing; reinstall or upgrade first");
  return current;
}

function ensureComponentSetup(options: {
  paths: SystemPaths;
  accounts: SystemAccounts;
  nodePath: string;
  component: Component;
  power: boolean;
  host: SystemHost;
}): void {
  const { paths, accounts, nodePath, component, power, host } = options;
  ensureDirectory(paths.prefix, 0o755);
  ensureDirectory(paths.configDir, 0o755);
  ensureDirectory(paths.stateDir, 0o755);
  ensureDirectory(paths.unitsDir, 0o755);
  ensureDirectory(join(paths.stateDir, "hub"), 0o700, true);
  ensureDirectory(join(paths.stateDir, "hub", "run"), 0o700, true);
  ensureDirectory(join(paths.stateDir, "collector"), 0o700, true);
  ensureDirectory(join(paths.stateDir, "backups"), 0o700, true);
  const release = currentReleaseOrThrow(paths);
  void release;
  writeSysusersFiles(paths, accounts, "local", power, host);
  if (component === "hub") {
    if (!existsSync(hubEnvPath(paths))) {
      const env = hubEnvTemplate(paths);
      writeManagedFile(hubEnvPath(paths), env, 0o600, false);
    }
    if (!existsSync(join(paths.unitsDir, UNIT_NAMES.hub))) {
      const env = readHubEnvironment(paths);
      const remote = env.HUB_PUBLIC_ORIGIN !== undefined && env.HUB_PUBLIC_ORIGIN !== "";
      const unit = renderSystemHubUnit({
        paths,
        accounts,
        nodePath,
        database: hubDatabasePath(paths),
        enrollment: hubEnrollmentPath(paths),
        socket: hubSocketPath(paths),
        remote,
        powerAccess: false,
      });
      writeUnitFile(paths, UNIT_NAMES.hub, `${unit}\n`, false);
    }
  } else {
    if (!existsSync(collectorEnvPath(paths))) {
      writeManagedFile(collectorEnvPath(paths), collectorEnvTemplate(), 0o600, false);
    }
    // Always write the current component unit so a legacy or hub-only install
    // gains the current isolation directives and power membership on enable.
    const unit = renderSystemCollectorUnit({
      paths,
      accounts,
      nodePath,
      collectorState: collectorStatePath(paths),
      powerAccess: power,
    });
    writeUnitFile(paths, UNIT_NAMES.collector, `${unit}\n`, true);
  }
  const validation = runRequired(
    host,
    "systemd-analyze",
    ["verify", "--man=no", join(paths.unitsDir, unitFor(component))],
    "native systemd unit validation",
  );
  if (/path is not absolute|bad unit file|fatal error|unknown (?:key|lvalue)|ignoring/i.test(validation.stderr)) {
    throw new SystemError("invalid-unit", "native systemd validation rejected a generated directive");
  }
  if (component === "hub") chownAccount([join(paths.stateDir, "hub")], accounts.hub, accounts.hub, host);
  if (component === "collector") chownAccount([join(paths.stateDir, "collector")], accounts.collector, accounts.collector, host);
}

function ensurePowerAccess(paths: SystemPaths, accounts: SystemAccounts, host: SystemHost): void {
  if (!existsSync(powerOriginalPath(paths))) capturePowerOriginals(paths);
  writePowerRule(paths, accounts);
  resumePowerRule(paths, host);
  applyPowerAccess(paths, accounts, host);
}

function planSystemInstall(options: SystemInstallOptions): SystemPlan {
  const paths = validateSystemPaths(options.paths);
  const accounts = validateAccounts(options.accounts);
  const role = resolveRole(options);
  const preset = componentSet(componentsForRole(role));
  const power = options.power !== false && preset.collector;
  const initial = options.enable === true ? preset : { hub: false, collector: false };
  const files: SystemPlan["files"] = [
    { path: join(paths.prefix, "current"), mode: "symlink", purpose: "active root-owned release" },
    { path: hubEnvPath(paths), mode: "0600", purpose: "root-owned hub configuration" },
    { path: collectorEnvPath(paths), mode: "0600", purpose: "root-owned collector configuration" },
    { path: sysusersPath(paths), mode: "0644", purpose: "dedicated nologin accounts" },
    ...COMPONENTS.map((component) => ({ path: join(paths.unitsDir, unitFor(component)), mode: "0644", purpose: `${component} boot unit` })),
  ];
  if (power) {
    files.push({ path: raplRulePath(paths), mode: "0644", purpose: "package RAPL read rule" });
    files.push({ path: powerSysusersPath(paths), mode: "0644", purpose: "collector-only power group membership" });
  }
  const commands = [
    "systemd-sysusers",
    "systemctl daemon-reload",
    ...(power ? ["udevadm control --reload-rules", "udevadm trigger --subsystem-match=powercap --action=change"] : []),
    ...(options.enable ? unitNamesForComponents(initial).map((unit) => `systemctl enable ${unit}`) : []),
    ...(options.start ? unitNamesForComponents(initial).map((unit) => `systemctl start ${unit}`) : []),
  ];
  const notes = [
    "Nothing is changed without --apply and root.",
    options.adoptFrom
      ? `Adopts ${resolve(options.adoptFrom)} by copy only; the original user setup is preserved.`
      : "Existing user services are not modified.",
    "One complete package: both runtimes, units and accounts are installed; --role only sets the initial enabled preset.",
    "Hub and collector use separate unprivileged accounts; Node gets no capabilities.",
    ...(options.enable && preset.hub ? ["Enabling the hub creates its owner account once and prints the password once."] : []),
    ...(options.enable && preset.collector ? ["A collector with no destination pairs with a configured colocated hub, otherwise it waits for `system connect`."] : []),
  ];
  return { operation: "install", apply: options.apply === true, role, components: initial, paths, accounts, commands, files, notes };
}

export async function installSystem(options: SystemInstallOptions): Promise<SystemReport> {
  const host = options.host ?? createSystemHost();
  const now = options.now ?? Date.now;
  const paths = validateSystemPaths(options.paths);
  const accounts = validateAccounts(options.accounts);
  const role = resolveRole(options);
  const plan = planSystemInstall({ ...options, paths, accounts, role });
  if (options.apply !== true) return { operation: "install", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  const existing = readSystemState(paths);
  if (existing?.active === true) {
    throw new SystemError("already-installed", "a system installation already exists; use `rigscope system upgrade` or uninstall first");
  }
  const conflicts = detectUserInstallations({ homeRoots: options.homeRoots });
  if (conflicts.length > 0 && options.adoptFrom === undefined && options.allowUserConflicts !== true) {
    const detail = conflicts.map((item) => `${item.user}:${item.unitName}`).join(", ");
    throw new SystemError(
      "user-installation-present",
      `existing user service(s) detected (${detail}); stop them and use --adopt-user-state for explicit copy-based adoption`,
    );
  }
  assertSystemStartAllowed(paths, options.start === true ? componentsForRole(role) : { hub: false, collector: false });
  const source = resolve(options.source);
  if (!existsSync(source)) throw new SystemError("missing-source", `release source does not exist: ${source}`);

  ensureDirectory(paths.prefix, 0o755);
  ensureDirectory(join(paths.prefix, "releases"), 0o755);
  ensureDirectory(paths.stateDir, 0o755);
  ensureDirectory(paths.configDir, 0o755);
  ensureDirectory(paths.unitsDir, 0o755);
  ensureDirectory(join(paths.stateDir, "hub"), 0o700, true);
  ensureDirectory(join(paths.stateDir, "collector"), 0o700, true);
  ensureDirectory(join(paths.stateDir, "backups"), 0o700, true);
  ensureDirectory(join(paths.stateDir, "hub", "run"), 0o700, true);
  assertManagedPathSafety(paths, isDefaultSystemLayout(paths));

  const nodePath = ensureRootOwnedNode({ paths, host, nodePath: options.nodePath, copyNode: options.copyNode, trustedNode: options.trustedNode, apply: true });
  const stage = stageRelease({ source, paths, host, replaceRelease: options.replaceRelease, now });
  if (options.installDeps === true) {
    installDependencies(stage.releaseDir, host, options.installDepsCommand ?? "npm");
  }
  const missing = missingDependencies(stage.releaseDir);
  if (missing.length > 0) {
    throw new SystemError(
      "missing-dependencies",
      `production dependencies are missing (${missing.join(", ")}); stage with --install-deps or use a built release directory`,
    );
  }
  chmodTree(stage.releaseDir, host);
  runRequired(host, "chown", ["-R", "root:root", stage.releaseDir], "release ownership");

  let adoption: AdoptionResult | null = null;
  if (options.adoptFrom) {
    adoption = adoptUserState({ source: options.adoptFrom, paths, role, accounts, apply: true, host, now });
    await performAdoptionCopyAsync({ source: options.adoptFrom, paths, role, host, copy: adoption });
  }

  const port = options.port ?? 8081;
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new SystemError("invalid-argument", "--port must be 1-65535");
  const preset = componentSet(componentsForRole(role));
  const power = options.power !== false && preset.collector;
  const hubEnv = adoption?.hubEnv ?? hubEnvTemplate(paths, options.origin, port);
  const collectorEnv = adoption?.collectorEnv ?? collectorEnvTemplate();
  // One complete package: both private environments and both units are always
  // installed. --role only selects the initial enabled preset.
  writeManagedFile(hubEnvPath(paths), hubEnv, 0o600, false);
  writeManagedFile(collectorEnvPath(paths), collectorEnv, 0o600, false);
  writeSysusersFiles(paths, accounts, "local", power, host);

  const hubUnit = renderSystemHubUnit({
    paths,
    accounts,
    nodePath,
    database: hubDatabasePath(paths),
    enrollment: hubEnrollmentPath(paths),
    socket: hubSocketPath(paths),
    remote: adoption?.remote === true || typeof options.origin === "string",
    powerAccess: false,
  });
  const collectorUnit = renderSystemCollectorUnit({
    paths,
    accounts,
    nodePath,
    collectorState: collectorStatePath(paths),
    powerAccess: power,
  });
  writeUnitFile(paths, UNIT_NAMES.hub, `${hubUnit}\n`, true);
  writeUnitFile(paths, UNIT_NAMES.collector, `${collectorUnit}\n`, true);

  const validation = runRequired(host,'systemd-analyze',['verify','--man=no',...COMPONENTS.map(component=>join(paths.unitsDir,unitFor(component)))], 'native systemd unit validation');
  if (/path is not absolute|bad unit file|fatal error|unknown (?:key|lvalue)|ignoring/i.test(validation.stderr)) {
    throw new SystemError('invalid-unit','native systemd validation rejected a generated directive');
  }

  if (power) ensurePowerAccess(paths, accounts, host);

  chownAccount([join(paths.stateDir, "hub")], accounts.hub, accounts.hub, host);
  chownAccount([join(paths.stateDir, "collector")], accounts.collector, accounts.collector, host);

  const state = emptyState({ paths, accounts, role, components: { hub: false, collector: false }, nodePath, now });
  state.currentVersion = stage.version;
  state.releases[stage.version] = {
    releaseDir: stage.releaseDir,
    schemaVersion: stage.schemaVersion,
    installedAt: new Date(now()).toISOString(),
  };
  state.adoption = adoption?.record ?? null;
  state.power = power ? { enabled: true, rulePath: raplRulePath(paths), group: accounts.powerGroup } : null;
  replaceSymlink(join(paths.prefix, "current"), stage.releaseDir, host);
  writeSystemState(paths, state);

  const wantsHub = preset.hub && (options.enable === true || options.start === true);
  const wantsCollector = preset.collector && (options.enable === true || options.start === true);
  let password: string | undefined;
  const awaiting: Component[] = [];
  if (wantsHub) password = ensureHubOwner(paths);
  if (wantsCollector && !collectorConfigured(paths).configured) {
    const pairing = await pairCollectorWithHub({ paths, accounts, host });
    if (pairing.awaiting) awaiting.push("collector");
  }

  if (options.enable === true) {
    enableUnits(host, unitNamesForComponents(preset));
    state.components = { hub: { enabled: preset.hub }, collector: { enabled: preset.collector } };
    writeSystemState(paths, state);
  }
  const started: Component[] = [];
  if (options.start === true) {
    assertSystemStartAllowed(paths, componentSet(preset));
    if (preset.hub) started.push("hub");
    if (preset.collector && !awaiting.includes("collector")) started.push("collector");
    if (started.length > 0) {
      startUnits(host, started.map(unitFor));
      await verifySystemHealth(paths, state, host, options.startupTimeoutMs ?? 20_000, started);
    }
  }
  return {
    operation: "install",
    ok: true,
    apply: true,
    message: `installed ${stage.version} for ${role} preset`,
    version: stage.version,
    current: stage.releaseDir,
    previous: null,
    state,
    password,
    ...(awaiting.length > 0 ? { awaitingConnection: awaiting } : {}),
    warnings: adoption?.warnings ?? [],
  };
}

export type SystemStatusOptions = {
  paths?: Partial<SystemPaths>;
  accounts?: Partial<SystemAccounts>;
  host?: SystemHost;
  json?: boolean;
  component?: Component;
  homeRoots?: string[];
};

export function systemStatus(options: SystemStatusOptions = {}): Record<string, unknown> {
  const host = options.host ?? createSystemHost();
  const paths = validateSystemPaths(options.paths);
  const accounts = validateAccounts(options.accounts);
  let state: SystemInstallState | null = null;
  let stateError: string | null = null;
  try {
    state = readSystemState(paths);
  } catch (error) {
    stateError = error instanceof Error ? error.message : String(error);
  }
  const presentUnits = [UNIT_NAMES.hub, UNIT_NAMES.collector].filter(unit => existsSync(join(paths.unitsDir, unit)));
  const publicRole: SystemRole | null = presentUnits.length === 2 ? 'local'
    : presentUnits[0] === UNIT_NAMES.hub ? 'hub' : presentUnits[0] === UNIT_NAMES.collector ? 'collector' : null;
  const role = state?.role ?? publicRole ?? "local";
  const current = currentReleaseDir(paths);
  let publicVersion: string | null = null;
  if (current) {
    try {
      const manifest = JSON.parse(readFileSync(join(current, 'package.json'), 'utf8'));
      if (typeof manifest.version === 'string') publicVersion = manifest.version;
    } catch { /* private or incomplete release; report unavailable */ }
  }
  const selected: Component[] = options.component ? [options.component] : [...COMPONENTS];
  const hubOwner = hubOwnerState(paths);
  const collector = collectorConfigured(paths);
  const componentReports: Record<string, Record<string, unknown>> = {};
  for (const component of COMPONENTS) {
    const unit = unitFor(component);
    const present = existsSync(join(paths.unitsDir, unit));
    const active = present && systemdActive(host, unit);
    const unitEnabled = present && systemdEnabled(host, unit);
    const enabled = state !== null ? state.components[component].enabled : unitEnabled;
    const configured = component === "hub" ? hubOwner.configured : collector.configured;
    const readiness = !present
      ? "missing"
      : component === "collector" && collector.problem
        ? "invalid-state"
        : component === "hub" && hubOwner.problem
          ? "invalid-config"
          : !configured
            ? component === "collector" ? "awaiting-connection" : "unconfigured"
            : active ? "ready" : "stopped";
    componentReports[component] = {
      component,
      unit,
      path: join(paths.unitsDir, unit),
      present,
      enabled,
      unitEnabled,
      active,
      configured,
      readiness,
      ...(component === "collector" ? { destinations: collector.destinations } : {}),
      ...(component === "hub" ? { ownerConfigured: hubOwner.configured } : {}),
    };
  }
  const units = selected.map((component) => componentReports[component]!);
  const lock = systemLockState(paths);
  const database = hubDatabasePath(paths);
  let schema: number | null = null;
  try {
    schema = databaseSchema(database);
  } catch {
    schema = null;
  }
  const power = inspectPowerAccess({ paths, accounts });
  return {
    scope: SYSTEM_SCOPE,
    installed: state?.active ?? (current !== null && publicRole !== null),
    stateError,
    role: state?.role ?? publicRole,
    currentVersion: state?.currentVersion ?? publicVersion,
    previousVersion: state?.previousVersion ?? null,
    current,
    previous: previousReleaseDir(paths),
    paths,
    accounts,
    nodePath: state?.nodePath ?? null,
    ...(options.component ? { selected: options.component } : {}),
    components: componentReports,
    units: units.map((unit) => ({
      unit: unit.unit as string,
      path: unit.path as string,
      present: unit.present as boolean,
      active: unit.active as boolean,
      enabled: unit.enabled as boolean,
    })),
    database: {
      path: database,
      exists: existsSync(database),
      schema,
      mode: existsSync(database) ? modeString(statSync(database).mode) : null,
      lock,
    },
    socket: { path: hubSocketPath(paths), exists: existsSync(hubSocketPath(paths)) },
    config: {
      dir: paths.configDir,
      hubEnv: { path: hubEnvPath(paths), present: existsSync(hubEnvPath(paths)) },
      collectorEnv: { path: collectorEnvPath(paths), present: existsSync(collectorEnvPath(paths)) },
    },
    adoption: state?.adoption ?? null,
    power,
    userInstallations: detectUserInstallations({ homeRoots: options.homeRoots }),
  };
}

export type SystemLifecycleOptions = {
  apply?: boolean;
  paths?: Partial<SystemPaths>;
  accounts?: Partial<SystemAccounts>;
  host?: SystemHost;
  component?: Component;
  start?: boolean;
  enable?: boolean;
  power?: boolean;
  startupTimeoutMs?: number;
  homeRoots?: string[];
  allowUserConflicts?: boolean;
};

function selectedComponents(state: SystemInstallState, component?: Component): Component[] {
  if (component !== undefined) return [component];
  const enabled = enabledComponents(state);
  if (enabled.length === 0) {
    throw new SystemError("no-enabled-components", "no system components are enabled; run `rigscope system enable hub|collector` first");
  }
  return enabled;
}

function assertComponentConfigured(paths: SystemPaths, components: Component[]): void {
  if (components.includes("hub")) {
    const owner = hubOwnerState(paths);
    if (!owner.configured) {
      throw new SystemError("hub-unconfigured", "the hub has no owner; run `rigscope system enable hub --apply` to create it once");
    }
  }
  if (components.includes("collector")) {
    const collector = collectorConfigured(paths);
    if (!collector.configured) {
      throw new SystemError(
        "collector-awaiting-connection",
        "the collector has no destination; run `rigscope system connect https://<hub> --token-file /private/grant --apply`",
      );
    }
  }
}

export async function startSystem(options: SystemLifecycleOptions = {}): Promise<SystemReport> {
  const host = options.host ?? createSystemHost();
  const paths = validateSystemPaths(options.paths);
  const state = readSystemState(paths);
  if (state === null || !state.active) throw new SystemError("not-installed", "no active system installation; run install first");
  const components = selectedComponents(state, options.component);
  const units = components.map(unitFor);
  const plan: SystemPlan = {
    operation: "start",
    apply: options.apply === true,
    role: state.role,
    ...(options.component ? { component: options.component } : {}),
    components: { hub: components.includes("hub"), collector: components.includes("collector") },
    paths,
    accounts: state.accounts,
    commands: ["systemctl daemon-reload", ...(options.enable ? units.map((unit) => `systemctl enable ${unit}`) : []), ...units.map((unit) => `systemctl start ${unit}`)],
    files: [],
    notes: ["Starting refuses to create a second hub runtime for the same database."],
  };
  if (options.apply !== true) return { operation: "start", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  assertComponentConfigured(paths, components);
  if (state.adoption) assertDirectoryStopped(state.adoption.source, componentSet(components));
  assertSystemStartAllowed(paths, componentSet(components));
  if (options.enable === true) {
    enableUnits(host, units);
    for (const component of components) state.components[component].enabled = true;
  }
  startUnits(host, units);
  await verifySystemHealth(paths,state,host,options.startupTimeoutMs ?? 20_000, components);
  writeSystemState(paths, state);
  return {
    operation: "start",
    ok: true,
    apply: true,
    message: components.length === 1 ? `${components[0]} started` : "both components started",
    ...(options.component ? { component: options.component } : {}),
    components: { hub: components.includes("hub"), collector: components.includes("collector") },
    state,
  };
}

export function stopSystem(options: SystemLifecycleOptions = {}): SystemReport {
  const host = options.host ?? createSystemHost();
  const paths = validateSystemPaths(options.paths);
  const state = readSystemState(paths);
  if (state === null) throw new SystemError("not-installed", "no system installation state");
  const components = selectedComponents(state, options.component);
  const units = components.map(unitFor);
  const plan: SystemPlan = {
    operation: "stop",
    apply: options.apply === true,
    role: state.role,
    ...(options.component ? { component: options.component } : {}),
    components: { hub: components.includes("hub"), collector: components.includes("collector") },
    paths,
    accounts: state.accounts,
    commands: units.map((unit) => `systemctl stop ${unit}`),
    files: [],
    notes: ["Stop is bounded by each unit's TimeoutStopSec."],
  };
  if (options.apply !== true) return { operation: "stop", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  stopUnits(host, units);
  assertDirectoryStopped(paths.stateDir, componentSet(components));
  writeSystemState(paths, state);
  return {
    operation: "stop",
    ok: true,
    apply: true,
    message: components.length === 1 ? `${components[0]} stopped` : "both components stopped",
    ...(options.component ? { component: options.component } : {}),
    components: { hub: components.includes("hub"), collector: components.includes("collector") },
    state,
  };
}

export async function restartSystem(options: SystemLifecycleOptions = {}): Promise<SystemReport> {
  const host = options.host ?? createSystemHost();
  const paths = validateSystemPaths(options.paths);
  const state = readSystemState(paths);
  if (state === null || !state.active) throw new SystemError("not-installed", "no active system installation");
  const components = selectedComponents(state, options.component);
  assertComponentConfigured(paths, components);
  const units = components.map(unitFor);
  const plan: SystemPlan = {
    operation: "restart",
    apply: options.apply === true,
    role: state.role,
    ...(options.component ? { component: options.component } : {}),
    components: { hub: components.includes("hub"), collector: components.includes("collector") },
    paths,
    accounts: state.accounts,
    commands: units.map((unit) => `systemctl restart ${unit}`),
    files: [],
    notes: ["Restart refuses to create a duplicate hub runtime."],
  };
  if (options.apply !== true) return { operation: "restart", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  stopUnits(host, units);
  assertDirectoryStopped(paths.stateDir, componentSet(components));
  assertSystemStartAllowed(paths, componentSet(components));
  startUnits(host, units);
  await verifySystemHealth(paths,state,host,options.startupTimeoutMs ?? 20_000, components);
  writeSystemState(paths, state);
  return {
    operation: "restart",
    ok: true,
    apply: true,
    message: components.length === 1 ? `${components[0]} restarted` : "both components restarted",
    ...(options.component ? { component: options.component } : {}),
    components: { hub: components.includes("hub"), collector: components.includes("collector") },
    state,
  };
}

export type SystemEnableOptions = SystemLifecycleOptions & { component: Component };

export async function enableSystem(options: SystemEnableOptions): Promise<SystemReport> {
  const host = options.host ?? createSystemHost();
  const paths = validateSystemPaths(options.paths);
  const state = readSystemState(paths);
  if (state === null || !state.active) throw new SystemError("not-installed", "no active system installation; run install first");
  const component = options.component;
  const power = component === "collector" && options.power !== false;
  const plan: SystemPlan = {
    operation: "enable",
    apply: options.apply === true,
    role: state.role,
    component,
    components: { hub: component === "hub" || state.components.hub.enabled, collector: component === "collector" || state.components.collector.enabled },
    paths,
    accounts: state.accounts,
    commands: [
      "systemd-sysusers",
      "systemctl daemon-reload",
      ...(power ? ["udevadm control --reload-rules", "udevadm trigger --subsystem-match=powercap --action=change"] : []),
      `systemctl enable ${unitFor(component)}`,
      `systemctl start ${unitFor(component)}`,
    ],
    files: [
      { path: component === "hub" ? hubEnvPath(paths) : collectorEnvPath(paths), mode: "0600", purpose: `${component} configuration` },
      { path: join(paths.unitsDir, unitFor(component)), mode: "0644", purpose: `${component} boot unit` },
    ],
    notes: [
      "Enables only the requested component; the other component keeps running.",
      ...(component === "hub" ? ["Creates the owner account once when the hub has no browser users; the password is printed once."] : []),
      ...(component === "collector" ? ["Pairs with a configured colocated hub when no destination exists; otherwise reports Awaiting connection."] : []),
    ],
  };
  if (options.apply !== true) return { operation: "enable", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  ensureComponentSetup({ paths, accounts: state.accounts, nodePath: state.nodePath, component, power, host });
  if (power) {
    ensurePowerAccess(paths, state.accounts, host);
    state.power = { enabled: true, rulePath: raplRulePath(paths), group: state.accounts.powerGroup };
  }
  let password: string | undefined;
  const awaiting: Component[] = [];
  if (component === "hub") {
    const hubWasActive = systemdActive(host, UNIT_NAMES.hub);
    password = ensureHubOwner(paths);
    if (password !== undefined && hubWasActive) {
      // A newly created owner is read at startup; restart only the hub so the
      // new credential takes effect. An already configured hub is untouched.
      stopUnits(host, [UNIT_NAMES.hub]);
      assertDirectoryStopped(paths.stateDir, "hub");
    }
  } else if (!collectorConfigured(paths).configured) {
    // Enrollment writes private collector state; restart only this component
    // if it happened to be running, never the hub.
    if (systemdActive(host, UNIT_NAMES.collector)) {
      stopUnits(host, [UNIT_NAMES.collector]);
      assertDirectoryStopped(paths.stateDir, "collector");
    }
    const pairing = await pairCollectorWithHub({ paths, accounts: state.accounts, host });
    if (pairing.awaiting) awaiting.push("collector");
  }
  enableUnits(host, [unitFor(component)]);
  state.components[component].enabled = true;
  writeSystemState(paths, state);
  const canStart = component === "hub" || !awaiting.includes("collector");
  if (canStart) {
    startUnits(host, [unitFor(component)]);
    await verifySystemHealth(paths, state, host, options.startupTimeoutMs ?? 20_000, [component]);
  }
  return {
    operation: "enable",
    ok: true,
    apply: true,
    message: component === "hub"
      ? "hub enabled"
      : awaiting.length > 0
        ? "collector enabled; awaiting connection"
        : "collector enabled",
    component,
    components: { hub: state.components.hub.enabled, collector: state.components.collector.enabled },
    ...(awaiting.length > 0 ? { awaitingConnection: awaiting } : {}),
    password,
    state,
  };
}

export type SystemDisableOptions = SystemLifecycleOptions & { component: Component };

export function disableSystem(options: SystemDisableOptions): SystemReport {
  const host = options.host ?? createSystemHost();
  const paths = validateSystemPaths(options.paths);
  const state = readSystemState(paths);
  if (state === null) throw new SystemError("not-installed", "no system installation state");
  const component = options.component;
  const plan: SystemPlan = {
    operation: "disable",
    apply: options.apply === true,
    role: state.role,
    component,
    components: { hub: component === "hub" || state.components.hub.enabled, collector: component === "collector" || state.components.collector.enabled },
    paths,
    accounts: state.accounts,
    commands: [`systemctl stop ${unitFor(component)}`, `systemctl disable ${unitFor(component)}`],
    files: [],
    notes: ["Settings, credentials, history and outboxes are retained while the component is disabled."],
  };
  if (options.apply !== true) return { operation: "disable", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  stopUnits(host, [unitFor(component)]);
  assertDirectoryStopped(paths.stateDir, component);
  runMaybe(host, "systemctl", ["disable", unitFor(component)]);
  state.components[component].enabled = false;
  writeSystemState(paths, state);
  return {
    operation: "disable",
    ok: true,
    apply: true,
    message: `${component} disabled; settings and data retained`,
    component,
    components: { hub: state.components.hub.enabled, collector: state.components.collector.enabled },
    state,
  };
}

export type SystemConnectOptions = SystemLifecycleOptions & { url: string; tokenFile: string };

function readPrivateToken(path: string, read: boolean): string | undefined {
  let stat;
  try {
    stat = lstatSync(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new SystemError("missing-grant", `grant file does not exist: ${path}`);
    }
    throw error;
  }
  if (!stat.isFile() || stat.isSymbolicLink()) throw new SystemError("unsafe-token", `grant must be a regular file: ${path}`);
  if ((stat.mode & 0o077) !== 0) throw new SystemError("insecure-permissions", `grant file must be private (0600): ${path}`);
  if (stat.size < 1 || stat.size > 4096) throw new SystemError("invalid-token", `grant file must contain 1-4096 bytes: ${path}`);
  if (!read) return undefined;
  const token = readFileSync(path, "utf8").trim();
  if (token === "" || /[\r\n\0]/.test(token) || !/^[\x21-\x7e]{1,256}$/.test(token)) {
    throw new SystemError("invalid-token", "grant file contains an invalid token");
  }
  return token;
}

export async function connectSystem(options: SystemConnectOptions): Promise<SystemReport> {
  const host = options.host ?? createSystemHost();
  const paths = validateSystemPaths(options.paths);
  const state = readSystemState(paths);
  if (state === null || !state.active) throw new SystemError("not-installed", "no active system installation; run install first");
  const destination = connectDestinationUrl(options.url);
  const tokenFile = resolve(options.tokenFile);
  const configured = collectorConfigured(paths);
  if (configured.configured) {
    throw new SystemError(
      "already-connected",
      "the collector already has a destination; this command never overwrites configured remote destinations",
    );
  }
  const plan: SystemPlan = {
    operation: "connect",
    apply: options.apply === true,
    role: state.role,
    component: "collector",
    components: { hub: state.components.hub.enabled, collector: true },
    paths,
    accounts: state.accounts,
    commands: ["systemd-sysusers", "systemctl daemon-reload", "stop collector if running", "enroll collector through the hub API", "start collector if enabled"],
    files: [{ path: collectorStatePath(paths), mode: "0600", purpose: "collector identity and destination" }],
    notes: [`Connects to ${destination} using the grant in ${tokenFile}; the grant is consumed once and never printed.`],
  };
  if (options.apply !== true) return { operation: "connect", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  const token = readPrivateToken(tokenFile, true)!;
  ensureComponentSetup({
    paths,
    accounts: state.accounts,
    nodePath: state.nodePath,
    component: "collector",
    power: options.power !== false,
    host,
  });
  if (options.power !== false) {
    ensurePowerAccess(paths, state.accounts, host);
    state.power = { enabled: true, rulePath: raplRulePath(paths), group: state.accounts.powerGroup };
  }
  const existingIdentity = readCollectorIdentity(collectorStatePath(paths));
  if (existingIdentity !== null) {
    // Normalize legacy adopted state before the shared enrollment helper reads it.
    writeCollectorIdentity(collectorStatePath(paths), existingIdentity);
  }
  const wasActive = systemdActive(host, UNIT_NAMES.collector);
  if (wasActive) {
    stopUnits(host, [UNIT_NAMES.collector]);
    assertDirectoryStopped(paths.stateDir, "collector");
  }
  try {
    await enrollCollector(collectorStatePath(paths), destination, token, { ...SYSTEM_DEFAULT_SHARING }, false);
  } catch (error) {
    throw new SystemError(
      "enrollment-failed",
      `collector enrollment failed: ${error instanceof Error ? error.message : String(error)}`,
    );
  } finally {
    chownAccount([join(paths.stateDir, "collector")], state.accounts.collector, state.accounts.collector, host);
  }
  if (!collectorConfigured(paths).configured) {
    throw new SystemError("enrollment-failed", "the hub accepted enrollment but no destination was saved; check private state");
  }
  chownAccount([join(paths.stateDir, "collector")], state.accounts.collector, state.accounts.collector, host);
  writeSystemState(paths, state);
  const enabled = state.components.collector.enabled;
  if (enabled || wasActive) {
    startUnits(host, [UNIT_NAMES.collector]);
    await verifySystemHealth(paths, state, host, options.startupTimeoutMs ?? 20_000, ["collector"]);
  }
  return {
    operation: "connect",
    ok: true,
    apply: true,
    message: enabled ? "collector connected and started" : "collector connected; run `rigscope system start collector`",
    component: "collector",
    components: { hub: state.components.hub.enabled, collector: enabled },
    state,
  };
}

export type SystemUpgradeOptions = SystemLifecycleOptions & {
  source: string;
  installDeps?: boolean;
  installDepsCommand?: string;
  replaceRelease?: boolean;
  now?: () => number;
};

async function upgradeSystemUnlocked(options: SystemUpgradeOptions): Promise<SystemReport> {
  const host = options.host ?? createSystemHost();
  const now = options.now ?? Date.now;
  const paths = validateSystemPaths(options.paths);
  const state = readSystemState(paths);
  if (state === null || !state.active) throw new SystemError("not-installed", "no active system installation; run install first");
  const activeComponents = COMPONENTS.filter((component) => systemdActive(host, unitFor(component)));
  const activeUnits = activeComponents.map(unitFor);
  const plan: SystemPlan = {
    operation: "upgrade",
    apply: options.apply === true,
    role: state.role,
    components: { hub: state.components.hub.enabled, collector: state.components.collector.enabled },
    paths,
    accounts: state.accounts,
    commands: ["stage new release", ...activeUnits.map((unit) => `systemctl stop ${unit}`), "backup database", "switch current", ...activeUnits.map((unit) => `systemctl start ${unit}`)],
    files: [{ path: join(paths.stateDir, "backups"), mode: "0700", purpose: "pre-upgrade database backup" }],
    notes: [
      "Staging happens before any service is stopped.",
      "The exact active component set is restored; --start does not start a component that was stopped.",
      "A failed post-upgrade start restores the previous release and matching database backup.",
    ],
  };
  if (options.apply !== true) return { operation: "upgrade", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  assertManagedPathSafety(paths, isDefaultSystemLayout(paths));
  const source = resolve(options.source);
  if (!existsSync(source)) throw new SystemError("missing-source", `release source does not exist: ${source}`);
  const stage = stageRelease({ source, paths, host, replaceRelease: options.replaceRelease, now });
  if (state.currentVersion === stage.version) {
    throw new SystemError("same-version", `release ${stage.version} is already current; use a different version`);
  }
  if (options.installDeps === true) installDependencies(stage.releaseDir, host, options.installDepsCommand ?? "npm");
  const missing = missingDependencies(stage.releaseDir);
  if (missing.length > 0) throw new SystemError("missing-dependencies", `production dependencies are missing: ${missing.join(", ")}`);
  chmodTree(stage.releaseDir, host);
  runRequired(host, "chown", ["-R", "root:root", stage.releaseDir], "release ownership");

  const oldCurrent = currentReleaseDir(paths);
  if (oldCurrent === null) throw new SystemError("invalid-state", "current release symlink is missing");
  const oldPrevious = previousReleaseDir(paths);
  let backupPath: string | null = null;
  let beforeSchema: number | null = null;
  try {
    stopUnits(host, activeUnits);
    assertDirectoryStopped(paths.stateDir);
    const lock = systemLockState(paths);
    if (lock.live) throw new SystemError("duplicate-runtime", `hub did not stop cleanly; lock remains at ${lock.path}`);
    const database = hubDatabasePath(paths);
    if (existsSync(database)) {
      ensureDirectory(join(paths.stateDir, "backups"), 0o700, true);
      beforeSchema = databaseSchema(database);
      backupPath = join(paths.stateDir, "backups", `pre-upgrade-${stage.version}-${newestTimestamp()}.sqlite`);
      await backupDatabase(database, backupPath, host);
    }
    replaceSymlink(join(paths.prefix, "previous"), oldCurrent, host);
    replaceSymlink(join(paths.prefix, "current"), stage.releaseDir, host);
    state.previousVersion = state.currentVersion;
    state.currentVersion = stage.version;
    state.releases[stage.version] = {
      releaseDir: stage.releaseDir,
      schemaVersion: stage.schemaVersion,
      installedAt: new Date(now()).toISOString(),
    };
    if (backupPath !== null) {
      state.backups[basename(backupPath)] = {
        path: backupPath,
        schemaVersion: beforeSchema,
        fromSchema: beforeSchema,
        toSchema: stage.schemaVersion,
        createdAt: new Date(now()).toISOString(),
        fromVersion: state.previousVersion ?? undefined,
        toVersion: stage.version,
      };
    }
    writeSystemState(paths, state);
    if (activeComponents.length > 0) {
      startUnits(host, activeUnits);
      await verifySystemHealth(paths,state,host,options.startupTimeoutMs ?? 20_000, activeComponents);
    }
  } catch (error) {
      await recoverFailedUpgrade({
        paths,
        state,
        oldCurrent,
        oldPrevious,
        beforeSchema,
        backupPath,
        host,
        activeComponents,
        units: activeUnits,
      });
      throw error;
  }
  return {
    operation: "upgrade",
    ok: true,
    apply: true,
    message: `upgraded ${state.previousVersion ?? "previous"} -> ${stage.version}`,
    version: stage.version,
    current: stage.releaseDir,
    previous: oldCurrent,
    backupPath,
    state,
  };
}

export type SystemRollbackOptions = SystemLifecycleOptions & {
  now?: () => number;
};

async function withSystemOperation<T>(options: SystemLifecycleOptions, operation: () => Promise<T>): Promise<T> {
  if (!options.apply) return operation();
  const host = options.host ?? createSystemHost();
  assertApply(host);
  const paths = validateSystemPaths(options.paths);
  const release = acquireProcessLease(join(paths.stateDir,'backups/operation.lock'));
  try {return await operation();} finally {release();}
}

export function upgradeSystem(options: SystemUpgradeOptions): Promise<SystemReport> {
  return withSystemOperation(options, () => upgradeSystemUnlocked(options));
}

export function rollbackSystem(options: SystemRollbackOptions = {}): Promise<SystemReport> {
  return withSystemOperation(options, () => rollbackSystemUnlocked(options));
}

async function rollbackSystemUnlocked(options: SystemRollbackOptions = {}): Promise<SystemReport> {
  const host = options.host ?? createSystemHost();
  const now = options.now ?? Date.now;
  const paths = validateSystemPaths(options.paths);
  const state = readSystemState(paths);
  if (state === null || !state.active) throw new SystemError("not-installed", "no active system installation");
  const previous = previousReleaseDir(paths);
  if (previous === null || state.previousVersion === null) {
    throw new SystemError("no-previous", "no previous release is retained for rollback");
  }
  const previousVersion = releaseVersion(previous);
  const previousSchema = releaseSchemaVersion(previous);
  const activeComponents = COMPONENTS.filter((component) => systemdActive(host, unitFor(component)));
  const units = activeComponents.map(unitFor);
  const plan: SystemPlan = {
    operation: "rollback",
    apply: options.apply === true,
    role: state.role,
    components: { hub: state.components.hub.enabled, collector: state.components.collector.enabled },
    paths,
    accounts: state.accounts,
    commands: [...units.map((unit) => `systemctl stop ${unit}`), "restore matching database backup if needed", `switch current to ${previousVersion}`, ...units.map((unit) => `systemctl start ${unit}`)],
    files: [],
    notes: [
      "An old release is never run against a newer schema without restoring a matching verified backup.",
      "The exact active component set is restored; a stopped component stays stopped.",
    ],
  };
  if (options.apply !== true) return { operation: "rollback", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  const oldCurrent = currentReleaseDir(paths);
  if (oldCurrent === null) throw new SystemError("invalid-state", "current release symlink is missing");
  const originalState = JSON.parse(JSON.stringify(state)) as SystemInstallState;
  let safetyBackup: string | null = null;
  let restoredDatabase = false;
  try {
    stopUnits(host, units);
    assertDirectoryStopped(paths.stateDir);
    if (systemLockState(paths).live || lockIsLive(join(paths.stateDir,'collector/collector-runtime.lock')).live) {
      throw new SystemError('runtime-active','runtime lease remains after stop; rollback refused');
    }
    const database = hubDatabasePath(paths);
    const currentSchema = existsSync(database) ? databaseSchema(database) : null;
    let backupToRestore: BackupRecord | null = null;
    if (currentSchema !== null && (previousSchema === null || currentSchema > previousSchema)) {
      backupToRestore = pickRollbackBackup(state, previousSchema);
      if (backupToRestore === null || !existsSync(backupToRestore.path)) {
        throw new SystemError(
          "incompatible-schema",
          `current database schema ${currentSchema} is newer than release ${previousVersion}; no matching pre-upgrade backup is recorded`,
        );
      }
    }
    if (existsSync(database)) {
      ensureDirectory(join(paths.stateDir, "backups"), 0o700, true);
      safetyBackup = join(paths.stateDir, "backups", `pre-rollback-${newestTimestamp()}.sqlite`);
      await backupDatabase(database, safetyBackup, host);
    }
    if (backupToRestore !== null) {
      replaceDatabaseFromBackup(backupToRestore.path, database, safetyBackup, host);
      restoredDatabase = true;
      runRequired(host,'chown',[`${state.accounts.hub}:${state.accounts.hub}`,database],'restored database ownership');
    }
    replaceSymlink(join(paths.prefix, "previous"), oldCurrent, host);
    replaceSymlink(join(paths.prefix, "current"), previous, host);
    state.previousVersion = state.currentVersion;
    state.currentVersion = previousVersion;
    state.updatedAt = new Date(now()).toISOString();
    writeSystemState(paths, state);
    if (activeComponents.length > 0) {
      startUnits(host, units);
      await verifySystemHealth(paths,state,host,options.startupTimeoutMs ?? 20_000, activeComponents);
    }
    return {
      operation: "rollback",
      ok: true,
      apply: true,
      message: `rolled back to ${previousVersion}`,
      version: previousVersion,
      current: previous,
      previous: oldCurrent,
      backupPath: backupToRestore?.path ?? safetyBackup,
      state,
    };
  } catch (error) {
    stopUnits(host, units);
    replaceSymlink(join(paths.prefix,'current'),oldCurrent,host);
    replaceSymlink(join(paths.prefix,'previous'),previous,host);
    if (restoredDatabase && safetyBackup) {
      const failed = join(paths.stateDir,'backups',`failed-rollback-${newestTimestamp()}.sqlite`);
      await backupDatabase(hubDatabasePath(paths),failed,host);
      replaceDatabaseFromBackup(safetyBackup,hubDatabasePath(paths),null,host);
      runRequired(host,'chown',[`${state.accounts.hub}:${state.accounts.hub}`,hubDatabasePath(paths)],'restored database ownership');
    }
    writeSystemState(paths,originalState);
    if (activeComponents.length > 0) {
      startUnits(host,units);
      await verifySystemHealth(paths,originalState,host,options.startupTimeoutMs ?? 20_000, activeComponents);
    }
    throw error;
  }
}

export type SystemUninstallOptions = SystemLifecycleOptions & {
  purgeData?: boolean;
  confirmPurgeHistory?: boolean;
  removeUsers?: boolean;
  purgeProgram?: boolean;
  now?: () => number;
};

export function uninstallSystem(options: SystemUninstallOptions = {}): SystemReport {
  const host = options.host ?? createSystemHost();
  const now = options.now ?? Date.now;
  const paths = validateSystemPaths(options.paths);
  const state = readSystemState(paths);
  const units = [UNIT_NAMES.hub, UNIT_NAMES.collector];
  const plan: SystemPlan = {
    operation: "uninstall",
    apply: options.apply === true,
    role: state?.role,
    components: { hub: false, collector: false },
    paths,
    accounts: state?.accounts ?? validateAccounts(options.accounts),
    commands: [
      ...units.map((unit) => `systemctl stop ${unit}`),
      ...units.map((unit) => `systemctl disable ${unit}`),
      "systemctl daemon-reload",
      "remove both Rigscope units",
      "remove releases, copied Node and active links",
      "remove RAPL rule and restore counter permissions",
      ...(options.purgeData ? ["remove /var/lib/rigscope and /etc/rigscope (history)"] : []),
      ...(options.removeUsers ? ["delete service accounts and power group"] : []),
    ],
    files: [],
    notes: [
      "The default uninstall removes both units and the application while preserving the database, history, configuration and accounts.",
      "History is removed only with --purge-data --confirm-purge-history.",
      "Adopted user data remains untouched at the recorded source path.",
    ],
  };
  if (options.apply !== true) return { operation: "uninstall", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  if (options.purgeData === true && options.confirmPurgeHistory !== true) {
    throw new SystemError("confirmation-required", "--purge-data requires --confirm-purge-history");
  }
  if (options.removeUsers === true && options.purgeData !== true) {
    throw new SystemError("order-required", "refusing to delete accounts while their data remains; combine --remove-users with --purge-data");
  }
  const accounts = state?.accounts ?? validateAccounts(options.accounts);
  stopUnits(host, units);
  assertDirectoryStopped(paths.stateDir);
  for (const unit of units) runMaybe(host, "systemctl", ["disable", unit]);
  for (const unit of units) {
    const path = join(paths.unitsDir, unit);
    assertManagedFileOwnership(paths, path);
    try {
      unlinkSync(path);
    } catch {
      /* absent */
    }
  }
  runMaybe(host, "systemctl", ["daemon-reload"]);
  const hadPowerAccess =
    state?.power?.enabled === true ||
    existsSync(raplRulePath(paths)) ||
    existsSync(powerOriginalPath(paths));
  if (hadPowerAccess) {
    try {
      unlinkSync(raplRulePath(paths));
    } catch {
      /* absent */
    }
    try {
      unlinkSync(powerSysusersPath(paths));
    } catch {
      /* absent */
    }
    restorePowerOriginals(paths, host);
    resumePowerRule(paths, host);
  }
  // Default uninstall removes the application; data and configuration remain
  // unless the guarded purge flags are supplied.
  removeSymlink(join(paths.prefix, "current"));
  removeSymlink(join(paths.prefix, "previous"));
  rmSync(join(paths.prefix, "releases"), { recursive: true, force: true });
  rmSync(join(paths.prefix, "node"), { recursive: true, force: true });
  if (state !== null) {
    state.active = false;
    state.power = null;
    state.components = { hub: { enabled: false }, collector: { enabled: false } };
    state.updatedAt = new Date(now()).toISOString();
    writeSystemState(paths, state);
  }
  if (options.removeUsers === true) {
    for (const account of [accounts.hub, accounts.collector]) runMaybe(host, "userdel", [account]);
    for (const group of [accounts.powerGroup, accounts.hub, accounts.collector]) runMaybe(host, "groupdel", [group]);
  }
  if (options.purgeData === true) {
    rmSync(paths.stateDir, { recursive: true, force: true });
    rmSync(paths.configDir, { recursive: true, force: true });
  }
  return {
    operation: "uninstall",
    ok: true,
    apply: true,
    message: options.purgeData === true ? "uninstalled; system data and history removed" : "uninstalled; application removed, data and history preserved",
    ...(options.removeUsers === true ? { components: { hub: false, collector: false } } : {}),
    state,
  };
}

export type SystemPowerOptions = {
  apply?: boolean;
  paths?: Partial<SystemPaths>;
  accounts?: Partial<SystemAccounts>;
  host?: SystemHost;
  passwdFile?: string;
  groupFile?: string;
  moduleRoot?: string;
};

export function powerStatus(options: SystemPowerOptions = {}): PowerAccessReport {
  return inspectPowerAccess({
    paths: options.paths,
    accounts: options.accounts,
    passwdFile: options.passwdFile,
    groupFile: options.groupFile,
    moduleRoot: options.moduleRoot,
  });
}

export function grantPower(options: SystemPowerOptions = {}): SystemReport & { access?: PowerAccessReport } {
  const host = options.host ?? createSystemHost();
  const paths = validateSystemPaths(options.paths);
  const accounts = validateAccounts(options.accounts);
  const plan: SystemPlan = {
    operation: "power-grant",
    apply: options.apply === true,
    paths,
    accounts,
    commands: [
      "systemd-sysusers",
      "udevadm control --reload-rules",
      "udevadm trigger --subsystem-match=powercap --action=change",
      `chgrp ${accounts.powerGroup} <package>/energy_uj`,
      "chmod 0440 <package>/energy_uj",
    ],
    files: [
      { path: raplRulePath(paths), mode: "0644", purpose: "package-only RAPL rule" },
      { path: powerSysusersPath(paths), mode: "0644", purpose: "collector-only power group" },
    ],
    notes: ["Only top-level package energy/range counters are granted; no capabilities are added to Node."],
  };
  if (options.apply !== true) return { operation: "power-grant", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  ensureDirectory(paths.sysusersDir, 0o755);
  ensureDirectory(paths.udevRulesDir, 0o755);
  ensureDirectory(paths.stateDir, 0o755);
  writeManagedFile(powerSysusersPath(paths), renderPowerSysusers(paths, accounts), 0o644, true);
  runRequired(host, "systemd-sysusers", [], "power group creation");
  assertSystemAccounts(host,accounts,true,['collector']);
  if (!existsSync(powerOriginalPath(paths))) capturePowerOriginals(paths);
  writePowerRule(paths, accounts);
  resumePowerRule(paths, host);
  applyPowerAccess(paths, accounts, host);
  const access = inspectPowerAccess({ paths, accounts, passwdFile: options.passwdFile, groupFile: options.groupFile, moduleRoot: options.moduleRoot });
  return { operation: "power-grant", ok: true, apply: true, message: "power access granted", plan, access };
}

export function revokePower(options: SystemPowerOptions & { removeGroup?: boolean } = {}): SystemReport & { access?: PowerAccessReport } {
  const host = options.host ?? createSystemHost();
  const paths = validateSystemPaths(options.paths);
  const accounts = validateAccounts(options.accounts);
  const plan: SystemPlan = {
    operation: "power-revoke",
    apply: options.apply === true,
    paths,
    accounts,
    commands: [
      "remove RAPL rule",
      "udevadm control --reload-rules",
      "udevadm trigger --subsystem-match=powercap --action=change",
      `gpasswd -d ${accounts.collector} ${accounts.powerGroup}`,
      "restore recorded counter modes",
    ],
    files: [],
    notes: ["Restores the exact modes/ownership recorded at grant time; never leaves energy_uj group-readable."],
  };
  if (options.apply !== true) return { operation: "power-revoke", ok: true, apply: false, message: "plan only", plan };
  assertApply(host);
  try {
    unlinkSync(raplRulePath(paths));
  } catch {
    /* absent */
  }
  try {
    unlinkSync(powerSysusersPath(paths));
  } catch {
    /* absent */
  }
  restorePowerOriginals(paths, host);
  resumePowerRule(paths, host);
  runMaybe(host, "gpasswd", ["-d", accounts.collector, accounts.powerGroup]);
  if (options.removeGroup === true) runMaybe(host, "groupdel", [accounts.powerGroup]);
  const access = inspectPowerAccess({ paths, accounts, passwdFile: options.passwdFile, groupFile: options.groupFile, moduleRoot: options.moduleRoot });
  return { operation: "power-revoke", ok: true, apply: true, message: "power access revoked", plan, access };
}

const HELP = `Rigscope system installation (optional, root/sudo)

Usage:
  rigscope system plan   --source ARCHIVE_OR_DIR [options]
  rigscope system install --source ARCHIVE_OR_DIR --apply [options]
  rigscope system status [hub|collector] [--json]
  rigscope system logs [hub|collector] [--follow]
  rigscope system enable  hub|collector --apply
  rigscope system disable hub|collector --apply
  rigscope system start|stop|restart [hub|collector] --apply
  rigscope system connect URL --token-file FILE --apply
  rigscope system upgrade --source ARCHIVE_OR_DIR --apply
  rigscope system rollback --apply
  rigscope system uninstall --apply [--purge-data --confirm-purge-history] [--remove-users]
  rigscope system power status|grant|revoke [--apply]

Options:
  --role local|hub|collector   Initial enabled preset; not a permanent restriction
  --adopt-user-state DIR       Explicit, copy-only adoption of a stopped user installation
  --allow-user-conflicts       Install beside existing user units (they must be stopped; no adoption)
  --node PATH                  Node 26 executable; a non-root-owned binary is copied to /opt/rigscope/node
  --copy-node                  Always copy the selected Node executable into the root-owned prefix
  --install-deps               Run npm ci --omit=dev --ignore-scripts as root while staging
  --without-power              Do not install collector group access to package counters
  --token-file PATH            Read a one-time connection grant from a private (0600) file
  --start / --enable           Start and/or enable the initial preset units explicitly
  --json                       Machine-readable output
  --apply                      Actually mutate; without it only the exact plan is printed

Path overrides (testing/custom hosts):
  --prefix DIR --config-dir DIR --state-dir DIR --units-dir DIR
  --sysusers-dir DIR --udev-rules-dir DIR --powercap-root DIR

One package contains both runtimes. --role only sets the initial enabled
preset; enable or disable hub and collector independently at any time.
Safety: mutations require root, never run systemctl --user, preserve user state,
and refuse duplicate hub runtimes or world-readable RAPL access.`;

type ParsedSystem = {
  command: string;
  subcommand?: string;
  values: Record<string, unknown>;
  positionals: string[];
};

function parseSystemArgs(argv: string[]): ParsedSystem {
  if (argv.length === 0 || argv[0] === "help" || argv[0] === "--help" || argv[0] === "-h") {
    return { command: "help", values: {}, positionals: [] };
  }
  const command = argv[0]!;
  const rest = argv.slice(1);
  let subcommand: string | undefined;
  if (command === "power" && rest[0] !== undefined && !rest[0].startsWith("-")) {
    subcommand = rest[0];
    rest.shift();
  }
  const parsed = parseArgs({
    args: rest,
    allowPositionals: true,
    options: {
      apply: { type: "boolean" },
      source: { type: "string" },
      role: { type: "string" },
      node: { type: "string" },
      "copy-node": { type: "boolean" },
      "install-deps": { type: "boolean" },
      "install-deps-command": { type: "string" },
      "without-power": { type: "boolean" },
      token: { type: "string" },
      "token-file": { type: "string" },
      start: { type: "boolean" },
      enable: { type: "boolean" },
      "adopt-user-state": { type: "string" },
      "allow-user-conflicts": { type: "boolean" },
      "replace-release": { type: "boolean" },
      port: { type: "string" },
      origin: { type: "string" },
      json: { type: "boolean" },
      follow: { type: "boolean" },
      "purge-data": { type: "boolean" },
      "confirm-purge-history": { type: "boolean" },
      "remove-users": { type: "boolean" },
      "purge-program": { type: "boolean" },
      "remove-group": { type: "boolean" },
      "startup-timeout": { type: "string" },
      prefix: { type: "string" },
      "config-dir": { type: "string" },
      "state-dir": { type: "string" },
      "units-dir": { type: "string" },
      "sysusers-dir": { type: "string" },
      "udev-rules-dir": { type: "string" },
      "powercap-root": { type: "string" },
      "passwd-file": { type: "string" },
      "group-file": { type: "string" },
      "module-root": { type: "string" },
    },
  });
  return { command, subcommand, values: parsed.values as Record<string, unknown>, positionals: parsed.positionals };
}

function stringValue(values: Record<string, unknown>, key: string): string | undefined {
  const value = values[key];
  if (value === undefined) return undefined;
  if (typeof value !== "string" || value === "") throw new SystemError("invalid-argument", `--${key} requires a value`);
  return value;
}
function boolValue(values: Record<string, unknown>, key: string): boolean {
  return values[key] === true;
}
function pathsFrom(values: Record<string, unknown>): Partial<SystemPaths> {
  return {
    ...(stringValue(values, "prefix") ? { prefix: stringValue(values, "prefix")! } : {}),
    ...(stringValue(values, "config-dir") ? { configDir: stringValue(values, "config-dir")! } : {}),
    ...(stringValue(values, "state-dir") ? { stateDir: stringValue(values, "state-dir")! } : {}),
    ...(stringValue(values, "units-dir") ? { unitsDir: stringValue(values, "units-dir")! } : {}),
    ...(stringValue(values, "sysusers-dir") ? { sysusersDir: stringValue(values, "sysusers-dir")! } : {}),
    ...(stringValue(values, "udev-rules-dir") ? { udevRulesDir: stringValue(values, "udev-rules-dir")! } : {}),
    ...(stringValue(values, "powercap-root") ? { powercapRoot: stringValue(values, "powercap-root")! } : {}),
  };
}
function roleFrom(values: Record<string, unknown>): SystemRole | undefined {
  const role = stringValue(values, "role");
  if (role === undefined) return undefined;
  if (role !== "local" && role !== "hub" && role !== "collector") throw new SystemError("invalid-role", "--role must be local, hub or collector");
  return role;
}

function componentFrom(parsed: ParsedSystem, required: boolean): Component | undefined {
  if (parsed.positionals.length > 1) {
    throw new SystemError("invalid-argument", `${parsed.command} accepts at most one component (hub or collector)`);
  }
  const value = parsed.positionals[0];
  if (value === undefined) {
    if (required) throw new SystemError("missing-argument", `${parsed.command} requires hub or collector`);
    return undefined;
  }
  if (value !== "hub" && value !== "collector") {
    throw new SystemError("invalid-argument", `${parsed.command} expects hub or collector; got ${JSON.stringify(value)}`);
  }
  return value;
}
function startupTimeoutFrom(values: Record<string, unknown>): number | undefined {
  const raw = stringValue(values, "startup-timeout");
  if (raw === undefined) return undefined;
  const value = Number(raw);
  if (!Number.isInteger(value) || value < 0 || value > 120_000) throw new SystemError("invalid-argument", "--startup-timeout must be 0-120000 ms");
  return value;
}

export function formatSystemReport(report: SystemReport | Record<string, unknown>): string {
  return `${JSON.stringify(report, null, 2)}\n`;
}

function humanPlan(plan: SystemPlan): string {
  const lines = [
    `Operation: ${plan.operation}${plan.apply ? "" : " (plan only; add --apply with sudo to execute)"}`,
    `Role:      ${plan.role ?? "n/a"}${plan.role ? " (initial preset metadata)" : ""}`,
    ...(plan.component ? [`Component: ${plan.component}`] : []),
    ...(plan.components ? [`Enabled:   ${COMPONENTS.filter((component) => plan.components![component]).join(", ") || "none"}`] : []),
    `Prefix:    ${plan.paths.prefix}`,
    `Config:    ${plan.paths.configDir}`,
    `State:     ${plan.paths.stateDir}`,
    `Units:     ${plan.paths.unitsDir}`,
    `Accounts:  hub=${plan.accounts.hub} collector=${plan.accounts.collector} power=${plan.accounts.powerGroup}`,
    "",
    "Files:",
    ...plan.files.map((file) => `  ${file.path} (${file.mode}) - ${file.purpose}`),
    "",
    "Commands:",
    ...plan.commands.map((command) => `  ${command}`),
    "",
    ...plan.notes.map((note) => `- ${note}`),
  ];
  return `${lines.join("\n")}\n`;
}

function humanStatus(status: Record<string, unknown>): string {
  const paths = status.paths as SystemPaths;
  const database = status.database as { path: string; exists: boolean; schema: number | null; lock: { live: boolean } };
  const components = status.components as Record<string, {
    component: Component;
    present: boolean;
    enabled: boolean;
    active: boolean;
    configured: boolean;
    readiness: string;
    destinations?: number;
  }>;
  const power = status.power as PowerAccessReport;
  const adoption = status.adoption as AdoptionRecord | null;
  const lines = [
    `Scope:     system`,
    `Installed: ${status.installed === true ? "yes" : "no"}`,
    `Role:      ${String(status.role ?? "n/a")} (initial preset metadata)`,
    `Current:   ${status.currentVersion === null || status.currentVersion === undefined ? "none" : `${status.currentVersion} (${status.current})`}`,
    `Previous:  ${status.previousVersion === null || status.previousVersion === undefined ? "none" : `${status.previousVersion} (${status.previous})`}`,
    `Prefix:    ${paths.prefix}`,
    `Config:    ${paths.configDir}`,
    `State:     ${paths.stateDir}`,
    `Database:  ${database.path} (${database.exists ? `schema ${database.schema ?? "unknown"}` : "absent"})${database.lock.live ? " RUNNING" : ""}`,
    `Socket:    ${String((status.socket as { path: string }).path)}${(status.socket as { exists: boolean }).exists ? " (present)" : ""}`,
    "",
    "Components:",
    ...(status.selected ? [status.selected as Component] : [...COMPONENTS]).map((name) => {
      const component = components[name]!;
      const detail = name === "collector" ? `, ${component.destinations ?? 0} destination(s)` : "";
      return `  ${name}: ${component.present ? "installed" : "missing"}, ${component.enabled ? "enabled" : "disabled"}, ${component.active ? "active" : "inactive"}, ${component.readiness}${detail}`;
    }),
    "",
    `Power:     ${power.available ? `${power.domains.length} package counter(s)` : "no package counters"}; collector read ${power.collectorCanRead ? "yes" : "no"}; persistent ${power.persistent ? "yes" : "no"}; world-readable ${power.worldReadable ? "YES" : "no"}`,
    ...(power.problems.length > 0 ? ["Power notes:", ...power.problems.map((problem) => `  - ${problem}`)] : []),
    ...(adoption ? ["", `Adoption:  copied from ${adoption.source} (original preserved; reversible by deleting the system copy)`] : []),
    ...(status.stateError ? ["", `State warning: ${String(status.stateError)}`] : []),
  ];
  return `${lines.join("\n")}\n`;
}

function printMutationReport(report: SystemReport): void {
  console.log(report.message);
  if (report.password) console.log(`Owner password: ${report.password} (shown once; store it now)`);
  if (report.awaitingConnection?.includes("collector")) {
    console.log("Awaiting connection: create a collector grant at the hub, save it to a private (0600) file,");
    console.log("then run `rigscope system connect https://<hub> --token-file <file> --apply`.");
  }
}

/**
 * CLI hook used by `src/cli/main.ts` and the standalone `dist/system.cjs`
 * entry. All mutations require `--apply` and root; this function never calls
 * `systemctl --user` and never touches the caller's home directory.
 */
export async function runSystemCommand(argv: string[]): Promise<number> {
  const parsed = parseSystemArgs(argv);
  if (parsed.command === "help") {
    console.log(HELP);
    return 0;
  }
  const values = parsed.values;
  const paths = pathsFrom(values);
  const json = boolValue(values, "json");
  const host = createSystemHost();
  try {
    if (parsed.command === "plan") {
      if (parsed.values.source === undefined) throw new SystemError("missing-argument", "plan requires --source");
      const plan = planSystemInstall({
        apply: false,
        source: stringValue(values, "source")!,
        role: roleFrom(values),
        paths,
        nodePath: stringValue(values, "node"),
        copyNode: boolValue(values, "copy-node"),
        port: stringValue(values, "port") ? Number(stringValue(values, "port")) : undefined,
        origin: stringValue(values, "origin"),
        adoptFrom: stringValue(values, "adopt-user-state"),
        installDeps: boolValue(values, "install-deps"),
        start: boolValue(values, "start"),
        enable: boolValue(values, "enable"),
      });
      console.log(json ? formatSystemReport(plan as unknown as Record<string, unknown>) : humanPlan(plan));
      return 0;
    }
    if (parsed.command === "install") {
      if (stringValue(values, "source") === undefined) throw new SystemError("missing-argument", "install requires --source");
      const report = await installSystem({
        apply: boolValue(values, "apply"),
        source: stringValue(values, "source")!,
        role: roleFrom(values),
        paths,
        nodePath: stringValue(values, "node"),
        copyNode: boolValue(values, "copy-node"),
        port: stringValue(values, "port") ? Number(stringValue(values, "port")) : undefined,
        origin: stringValue(values, "origin"),
        adoptFrom: stringValue(values, "adopt-user-state"),
        installDeps: boolValue(values, "install-deps"),
        installDepsCommand: stringValue(values, "install-deps-command"),
        power: boolValue(values, "without-power") ? false : undefined,
        start: boolValue(values, "start"),
        enable: boolValue(values, "enable"),
        replaceRelease: boolValue(values, "replace-release"),
        allowUserConflicts: boolValue(values, "allow-user-conflicts"),
        startupTimeoutMs: startupTimeoutFrom(values),
        host,
      });
      if (json) console.log(formatSystemReport(report as unknown as Record<string, unknown>));
      else if (report.plan) console.log(`Installed: no (plan only)\n\n${humanPlan(report.plan)}`);
      else {
        printMutationReport(report);
        console.log(`Current: ${report.current ?? "none"}`);
      }
      return 0;
    }
    if (parsed.command === "status") {
      const component = componentFrom(parsed, false);
      const status = systemStatus({ paths, host, json, component, homeRoots: undefined });
      console.log(json ? formatSystemReport(status) : humanStatus(status));
      return 0;
    }
    if (parsed.command === "logs") {
      const selected = validateSystemPaths(paths);
      const component = componentFrom(parsed, false);
      // Unit names are public; viewing logs must not require access to private credentials.
      const units = (component ? [unitFor(component)] : [UNIT_NAMES.hub, UNIT_NAMES.collector])
        .filter(unit => existsSync(join(selected.unitsDir, unit)));
      if (units.length === 0) throw new SystemError('not-installed', component ? `${component} logs are unavailable; its unit is not installed` : 'no system services installed');
      const result = spawnSync('journalctl', [...units.flatMap(unit => ['--unit', unit]), '--lines=100', '--no-pager', ...(boolValue(values, 'follow') ? ['--follow'] : [])], {stdio: 'inherit'});
      if (result.error) throw result.error;
      return result.status ?? 1;
    }
    if (parsed.command === "enable" || parsed.command === "disable") {
      const component = componentFrom(parsed, true)!;
      const options: SystemLifecycleOptions = {
        apply: boolValue(values, "apply"),
        paths,
        host,
        component,
        power: boolValue(values, "without-power") ? false : undefined,
        startupTimeoutMs: startupTimeoutFrom(values),
      };
      const report = parsed.command === "enable"
        ? await enableSystem({ ...options, component })
        : disableSystem({ ...options, component });
      if (json) console.log(formatSystemReport(report as unknown as Record<string, unknown>));
      else if (report.plan) console.log(humanPlan(report.plan));
      else printMutationReport(report);
      return 0;
    }
    if (parsed.command === "connect") {
      const url = parsed.positionals[0];
      if (url === undefined) throw new SystemError("missing-argument", "connect requires a hub URL");
      if (parsed.positionals.length > 1) throw new SystemError("invalid-argument", "connect accepts one hub URL");
      const tokenFile = stringValue(values, "token-file");
      if (tokenFile === undefined) throw new SystemError("missing-argument", "connect requires --token-file with a private (0600) grant file");
      const report = await connectSystem({
        apply: boolValue(values, "apply"),
        paths,
        host,
        url,
        tokenFile,
        power: boolValue(values, "without-power") ? false : undefined,
        startupTimeoutMs: startupTimeoutFrom(values),
      });
      if (json) console.log(formatSystemReport(report as unknown as Record<string, unknown>));
      else if (report.plan) console.log(humanPlan(report.plan));
      else printMutationReport(report);
      return 0;
    }
    if (parsed.command === "start" || parsed.command === "stop" || parsed.command === "restart") {
      const component = componentFrom(parsed, false);
      const options: SystemLifecycleOptions = {
        apply: boolValue(values, "apply"),
        paths,
        host,
        component,
        enable: boolValue(values, "enable"),
        startupTimeoutMs: startupTimeoutFrom(values),
      };
      const report = parsed.command === "start" ? await startSystem(options) : parsed.command === "stop" ? stopSystem(options) : await restartSystem(options);
      if (json) console.log(formatSystemReport(report as unknown as Record<string, unknown>));
      else if (report.plan) console.log(humanPlan(report.plan));
      else printMutationReport(report);
      return 0;
    }
    if (parsed.command === "upgrade") {
      const report = await upgradeSystem({
        apply: boolValue(values, "apply"),
        source: stringValue(values, "source") ?? "",
        paths,
        host,
        start: boolValue(values, "start"),
        installDeps: boolValue(values, "install-deps"),
        installDepsCommand: stringValue(values, "install-deps-command"),
        replaceRelease: boolValue(values, "replace-release"),
        startupTimeoutMs: startupTimeoutFrom(values),
      });
      if (json) console.log(formatSystemReport(report as unknown as Record<string, unknown>));
      else if (report.plan) console.log(humanPlan(report.plan));
      else console.log(`${report.message}\nBackup: ${report.backupPath ?? "none"}`);
      return 0;
    }
    if (parsed.command === "rollback") {
      const report = await rollbackSystem({ apply: boolValue(values, "apply"), paths, host, start: boolValue(values, "start"), startupTimeoutMs: startupTimeoutFrom(values) });
      if (json) console.log(formatSystemReport(report as unknown as Record<string, unknown>));
      else if (report.plan) console.log(humanPlan(report.plan));
      else console.log(`${report.message}\nBackup: ${report.backupPath ?? "none"}`);
      return 0;
    }
    if (parsed.command === "uninstall") {
      const report = uninstallSystem({
        apply: boolValue(values, "apply"),
        paths,
        host,
        purgeData: boolValue(values, "purge-data"),
        confirmPurgeHistory: boolValue(values, "confirm-purge-history"),
        removeUsers: boolValue(values, "remove-users"),
        purgeProgram: boolValue(values, "purge-program"),
      });
      if (json) console.log(formatSystemReport(report as unknown as Record<string, unknown>));
      else if (report.plan) console.log(humanPlan(report.plan));
      else console.log(report.message);
      return 0;
    }
    if (parsed.command === "power") {
      const sub = parsed.subcommand ?? "status";
      if (sub === "status") {
        const access = powerStatus({
          paths,
          passwdFile: stringValue(values, "passwd-file"),
          groupFile: stringValue(values, "group-file"),
          moduleRoot: stringValue(values, "module-root"),
        });
        if (json) console.log(formatSystemReport(access as unknown as Record<string, unknown>));
        else {
          console.log(`Driver:    ${access.driver.length > 0 ? access.driver.join(", ") : "not detected"}`);
          console.log(`Domains:   ${access.domains.length} package counter(s)`);
          for (const domain of access.domains) {
            console.log(`  ${domain.name}: energy ${domain.energy?.mode ?? "missing"} ${domain.energy?.readable ? "collector-readable" : "denied"}${domain.energy?.worldReadable ? " WORLD-READABLE" : ""}`);
          }
          console.log(`Access:    collector read ${access.collectorCanRead ? "yes" : "no"}; persistent ${access.persistent ? "yes" : "no"}; world-readable ${access.worldReadable ? "YES" : "no"}`);
          for (const problem of access.problems) console.log(`  - ${problem}`);
        }
        return access.worldReadable || access.writableByCollector ? 1 : 0;
      }
      if (sub === "grant") {
        const report = grantPower({
          apply: boolValue(values, "apply"),
          paths,
          host,
          passwdFile: stringValue(values, "passwd-file"),
          groupFile: stringValue(values, "group-file"),
          moduleRoot: stringValue(values, "module-root"),
        });
        if (json) console.log(formatSystemReport(report as unknown as Record<string, unknown>));
        else if (report.plan) console.log(humanPlan(report.plan));
        else console.log(report.message);
        return 0;
      }
      if (sub === "revoke") {
        const report = revokePower({
          apply: boolValue(values, "apply"),
          paths,
          host,
          removeGroup: boolValue(values, "remove-group"),
          passwdFile: stringValue(values, "passwd-file"),
          groupFile: stringValue(values, "group-file"),
          moduleRoot: stringValue(values, "module-root"),
        });
        if (json) console.log(formatSystemReport(report as unknown as Record<string, unknown>));
        else if (report.plan) console.log(humanPlan(report.plan));
        else console.log(report.message);
        return 0;
      }
      throw new SystemError("unknown-command", "system power expects status, grant or revoke");
    }
    throw new SystemError("unknown-command", `unknown system command ${JSON.stringify(parsed.command)}; run rigscope system help`);
  } catch (error) {
    if (json) {
      console.log(formatSystemReport({ ok: false, error: error instanceof Error ? error.message : String(error), code: error instanceof SystemError ? error.code : "error" }));
    } else {
      console.error(`rigscope system: ${error instanceof Error ? error.message : String(error)}`);
    }
    return 1;
  }
}

function isMainModule(): boolean {
  try {
    return require.main === module;
  } catch {
    return true;
  }
}

if (isMainModule()) {
  runSystemCommand(process.argv.slice(2))
    .then((code) => {
      process.exitCode = code;
    })
    .catch((error: unknown) => {
      console.error(`rigscope system: ${error instanceof Error ? error.message : String(error)}`);
      process.exitCode = 1;
    });
}
