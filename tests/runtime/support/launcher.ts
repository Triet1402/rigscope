/**
 * Executable launcher for the runtime suites.
 *
 * The suites describe a collector or hub logically (mode, destinations, state,
 * interval) and this module turns that description into the single 1.0
 * implementation:
 *
 *   <binary> collector --config <stateDir>/config.json --state-dir <stateDir>
 *   <binary> hub       --config <stateDir>/config.json --state-dir <stateDir>
 *
 * Go state layout written for a test (all private files are 0600):
 *
 *   <stateDir>/config.json                       version 1, components.collector
 *   <stateDir>/collector/identity.json           collectorId (+ hostId once read)
 *   <stateDir>/collector/credentials.json        per-destination token
 *
 * The enrollment grant is never written to disk by the launcher: the explicit
 * enrollment operation receives it on its command line, exactly like an
 * operator would. The installer's staged-grant file remains the production
 * path (`rigscope install --hub URL --token TOKEN` then a restart).
 *
 * Config v1 translation:
 *
 *   components.collector.enabled            always true
 *   components.collector.destinations       [{url, caFile?}] — exactly one in 1.0;
 *                                           the bearer token lives only in
 *                                           credentials.json
 *   components.collector.samplingInterval   "<n>ms" / "<n>s" from intervalMs
 *   components.collector.sharing            the collector-local policy
 *   components.collector.synthetic          mock/synthetic mode only
 *   components.collector.hardware           {"gpuRegisters": false}
 *
 * The advertised hostname is not part of the config schema: the CLI takes
 * `--hostname` for the operational override the tests need. TLS trust travels
 * on `destinations[].caFile`, never on a collector-wide `tls` object. The child
 * is spawned without SSL_CERT_FILE / NODE_EXTRA_CA_CERTS, so a pinned CA file
 * is the only way the TLS fixtures can succeed and the test actually exercises
 * pinning.
 *
 * A 1.0 collector has exactly one destination and a bounded RAM queue. The
 * launcher rejects multi-destination specs so a test that forgot to cut a
 * multi-hub assertion fails with a clear message.
 */
import { randomBytes, randomUUID } from "node:crypto";
import { createRequire } from "node:module";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import type { TestContext } from "node:test";
import { BROWSER_PASSWORD } from "./browser";
import { ROOT, type CredentialConfig } from "./config";
import { baseEnv } from "./env";
import { goBinaryPath } from "./implementation";
import { ChildHandle, startProcess } from "./process";

export type SharingInput = Record<string, boolean>;

export type DestinationSpec = {
  url: string;
  token: string;
  /** Collector-local policy. 1.0 has one policy for the one destination. */
  sharing?: SharingInput;
  /** Sampling cadence for the one destination. */
  intervalMs?: number;
};

export type CollectorSpec = {
  label?: string;
  mode?: "real" | "mock";
  /** Collector state directory. A temporary one is created when omitted. */
  stateDir?: string;
  collectorId?: string;
  hostname?: string;
  hubs?: DestinationSpec[];
  intervalMs?: number;
  sharing?: SharingInput;
  /** Extra environment, e.g. NODE_EXTRA_CA_CERTS for the TLS fixture. */
  env?: Record<string, string | undefined>;
};

export type EnrollmentSpec = {
  label?: string;
  stateDir?: string;
  url: string;
  token: string;
  sharing?: SharingInput;
  env?: Record<string, string | undefined>;
};

export type HubLaunchSpec = {
  telemetryMode?: "memory" | "persistent";
  label?: string;
  /** Hub state directory (config, bootstrap files). Temporary when omitted. */
  stateDir?: string;
  host?: string;
  port?: number;
  databasePath?: string;
  /** null exercises the product default; omitted uses the fast 50 ms fixture delay. */
  flushMs?: number | null;
  collectors?: CredentialConfig[];
  grants?: { token: string; expiresAt: string }[];
  /** `false` starts a hub without a browser account. */
  browserUsers?: false | { username: string; machineIds: "*" | string[]; admin?: boolean }[] | string;
};

type CollectorState = {
  collectorId: string;
  destinations: DestinationSpec[];
};

const CONFIG_FILE = "config.json";
const IDENTITY_FILE = "collector/identity.json";
const CREDENTIALS_FILE = "collector/credentials.json";

function privateDirectory(path: string): void {
  mkdirSync(path, { recursive: true, mode: 0o700 });
  chmodSync(path, 0o700);
}

function writePrivateJson(path: string, value: unknown): void {
  privateDirectory(dirname(path));
  writeFileSync(path, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  chmodSync(path, 0o600);
}

function writePrivateText(path: string, text: string): void {
  privateDirectory(dirname(path));
  writeFileSync(path, text, { mode: 0o600 });
  chmodSync(path, 0o600);
}

function readJson(path: string): unknown | null {
  if (!existsSync(path)) return null;
  try {
    return JSON.parse(readFileSync(path, "utf8")) as unknown;
  } catch (error) {
    throw new Error(`invalid JSON in ${path}: ${error instanceof Error ? error.message : String(error)}`);
  }
}

function asRecord(value: unknown, what: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${what} must be a JSON object`);
  }
  return value as Record<string, unknown>;
}

function destinationOf(value: unknown, what: string): DestinationSpec {
  const record = asRecord(value, what);
  if (typeof record.url !== "string" || typeof record.token !== "string") {
    throw new Error(`${what} must contain string url and token fields`);
  }
  return {
    url: record.url,
    token: record.token,
    ...(record.sharing === undefined ? {} : { sharing: record.sharing as SharingInput }),
    ...(record.intervalMs === undefined ? {} : { intervalMs: Number(record.intervalMs) }),
  };
}

/** Read the identity and credentials files the collector maintains. */
export function readCollectorState(stateDir: string): CollectorState | null {
  const identity = readJson(join(stateDir, IDENTITY_FILE));
  const parsed = readJson(join(stateDir, CREDENTIALS_FILE));
  if (identity === null && parsed === null) return null;
  let collectorId: string | null = null;
  if (identity !== null) {
    const record = asRecord(identity, `${stateDir}/${IDENTITY_FILE}`);
    if (typeof record.collectorId !== "string") {
      throw new Error(`${stateDir}/${IDENTITY_FILE} must contain a string collectorId`);
    }
    collectorId = record.collectorId;
  }
  const destinations: DestinationSpec[] = [];
  if (parsed !== null) {
    const record = asRecord(parsed, `${stateDir}/${CREDENTIALS_FILE}`);
    const raw = Array.isArray(record.credentials) ? record.credentials : [];
    for (const [index, entry] of raw.entries()) {
      destinations.push(destinationOf(entry, `${stateDir}/${CREDENTIALS_FILE}[${index}]`));
    }
  }
  if (collectorId === null) {
    throw new Error(`${stateDir}/${IDENTITY_FILE} is missing, so the collector identity is unknown`);
  }
  return { collectorId, destinations };
}

function stateDirectory(t: TestContext, explicit: string | undefined, prefix: string): string {
  if (explicit !== undefined) return explicit;
  const created = mkdtempSync(join(tmpdir(), `rigscope-${prefix}-`));
  t.after(() => rmSync(created, { recursive: true, force: true }));
  return created;
}

function resolveCollectorState(spec: CollectorSpec, stateDir: string): CollectorState {
  if (spec.hubs !== undefined) {
    if (spec.hubs.length > 1) {
      throw new Error("the 1.0 collector accepts exactly one destination; cut the multi-hub assertion");
    }
    return {
      collectorId:
        spec.collectorId ??
        readCollectorState(stateDir)?.collectorId ??
        randomUUID(),
      destinations: spec.hubs,
    };
  }
  const fromGo = readCollectorState(stateDir);
  if (fromGo !== null) return fromGo;
  if (spec.collectorId !== undefined) return { collectorId: spec.collectorId, destinations: [] };
  throw new Error(
    "the collector launcher needs a destination (spec.hubs), an existing credentials file, " +
      "or an explicit collectorId",
  );
}

/** Read the collector config the enrollment left behind, if any. */
function readGoCollectorConfig(stateDir: string): {
  sharing?: SharingInput;
  samplingInterval?: string;
} | null {
  const parsed = readJson(join(stateDir, CONFIG_FILE));
  if (parsed === null) return null;
  const root = asRecord(parsed, `${stateDir}/${CONFIG_FILE}`);
  const components = asRecord(root.components, `${stateDir}/${CONFIG_FILE} components`);
  const collector = asRecord(components.collector, `${stateDir}/${CONFIG_FILE} components.collector`);
  return {
    ...(collector.sharing === undefined ? {} : { sharing: collector.sharing as SharingInput }),
    ...(typeof collector.samplingInterval === "string" ? { samplingInterval: collector.samplingInterval } : {}),
  };
}

/** Serialize a millisecond cadence as the Go config's duration string. */
function goDuration(ms: number): string {
  return ms % 1000 === 0 ? `${ms / 1000}s` : `${ms}ms`;
}

/** Parse the Go duration subset the harness writes ("<n>ms" or "<n>s"). */
function durationMs(value: string | undefined): number | undefined {
  if (value === undefined) return undefined;
  const match = /^(\d+)(ms|s)$/.exec(value);
  if (match === null) return undefined;
  const amount = Number(match[1]);
  return match[2] === "s" ? amount * 1000 : amount;
}

/** The harness's CA knob: TLS trust comes from destinations[].caFile. */
function caFileFor(env: Record<string, string | undefined> | undefined): string | undefined {
  const value = env?.NODE_EXTRA_CA_CERTS;
  return value === undefined || value === "" ? undefined : value;
}

/**
 * The version-1 schema requires every sharing key. Fill the collector defaults
 * around the test's overrides: metric families and process visibility on,
 * network addresses and arguments off, and swap following memory when the test
 * does not name it.
 */
function goSharing(input: SharingInput | undefined): Record<string, boolean> {
  const memory = input?.memory ?? true;
  return {
    cpu: input?.cpu ?? true,
    memory,
    swap: input?.swap ?? memory,
    gpus: input?.gpus ?? true,
    disks: input?.disks ?? true,
    network: input?.network ?? true,
    networkAddresses: input?.networkAddresses ?? false,
    processes: input?.processes ?? true,
    processArguments: input?.processArguments ?? false,
  };
}

/** Write the version-1 configuration the collector loads. */
function writeGoCollectorConfig(
  stateDir: string,
  options: {
    destinations: { url: string; caFile?: string }[];
    intervalMs?: number;
    sharing?: SharingInput;
    synthetic?: boolean;
  },
): void {
  writePrivateJson(join(stateDir, CONFIG_FILE), {
    version: 1,
    components: {
      collector: {
        enabled: true,
        destinations: options.destinations,
        samplingInterval: goDuration(options.intervalMs ?? 2000),
        ...(options.synthetic === true ? { synthetic: true } : {}),
        sharing: goSharing(options.sharing),
        // Register temperatures are opt-in at install time; the harness never
        // grants that access.
        hardware: { gpuRegisters: false },
      },
    },
  });
}

/** Write the collector-private identity and credentials the runtime reads. */
function writeGoState(stateDir: string, state: CollectorState): void {
  writePrivateJson(join(stateDir, IDENTITY_FILE), { collectorId: state.collectorId });
  if (state.destinations.length === 0) return;
  writePrivateJson(join(stateDir, CREDENTIALS_FILE), {
    credentials: state.destinations.map((destination) => ({ url: destination.url, token: destination.token })),
  });
}

function goEnvironment(env: Record<string, string | undefined> | undefined): NodeJS.ProcessEnv {
  const merged = baseEnv({ ...env });
  // Pinned trust must come from the translated config, not from the ambient
  // trust store: an inherited SSL_CERT_FILE/NODE_EXTRA_CA_CERTS would let a
  // collector pass the TLS fixtures without reading destinations[].caFile.
  delete merged.SSL_CERT_FILE;
  delete merged.SSL_CERT_DIR;
  delete merged.NODE_EXTRA_CA_CERTS;
  return merged;
}

/** Start a collector. */
export function startCollectorProcess(t: TestContext, spec: CollectorSpec): ChildHandle {
  const label = spec.label ?? "collector";
  const stateDir = stateDirectory(t, spec.stateDir, `${spec.mode ?? "real"}-collector`);
  const state = resolveCollectorState(spec, stateDir);
  const caFile = caFileFor(spec.env);
  // An enrollment writes the installation's sharing policy and cadence to
  // config.json. A later start without explicit overrides must preserve them,
  // exactly like a real restart does.
  const existing = readGoCollectorConfig(stateDir);
  writeGoCollectorConfig(stateDir, {
    destinations: state.destinations.map((destination) => ({
      url: destination.url,
      ...(caFile === undefined ? {} : { caFile }),
    })),
    intervalMs: spec.intervalMs
      ?? (state.destinations.length === 1 ? state.destinations[0].intervalMs : undefined)
      ?? durationMs(existing?.samplingInterval),
    sharing: spec.sharing
      ?? (state.destinations.length === 1 ? state.destinations[0].sharing : undefined)
      ?? existing?.sharing,
    synthetic: spec.mode === "mock",
  });
  writeGoState(stateDir, state);
  const child = startProcess(
    label,
    goBinaryPath(),
    [
      "collector",
      "--config", join(stateDir, CONFIG_FILE),
      "--state-dir", stateDir,
      ...(spec.hostname === undefined ? [] : ["--hostname", spec.hostname]),
    ],
    goEnvironment(spec.env),
    { cwd: ROOT },
  );
  t.after(() => child.stop());
  return child;
}

/**
 * Start an explicit enrollment operation:
 *
 *   rigscope collector enroll --config <file> --state-dir <dir> --grant <token>
 */
export function startEnrollmentProcess(t: TestContext, spec: EnrollmentSpec): ChildHandle {
  const label = spec.label ?? "collector enrollment";
  const stateDir = stateDirectory(t, spec.stateDir, "collector");
  const caFile = caFileFor(spec.env);
  writeGoCollectorConfig(stateDir, {
    destinations: [{ url: spec.url, ...(caFile === undefined ? {} : { caFile }) }],
    // The local sharing policy is part of the installation, not of one
    // enrollment attempt: persist it so a later restart keeps memory/arguments
    // withheld.
    sharing: spec.sharing,
  });
  // The grant is an argument, not a file: `install --hub/--token` staging is
  // the production path, and an explicit enrollment accepts a grant directly.
  const child = startProcess(
    label,
    goBinaryPath(),
    [
      "collector", "enroll",
      "--config", join(stateDir, CONFIG_FILE),
      "--state-dir", stateDir,
      "--grant", spec.token,
    ],
    goEnvironment(spec.env),
    { cwd: ROOT },
  );
  t.after(() => child.stop());
  return child;
}

/**
 * Start a hub:
 *
 *   rigscope hub --config <stateDir>/config.json --state-dir <stateDir>
 *
 * The launcher owns the fixture translation:
 *
 *   host/port                      -> components.hub.listen / port (port 0 asks
 *                                     for the RIGSCOPE_HUB_PORT=0 ephemeral override)
 *   databasePath                   -> RIGSCOPE_HUB_DATABASE_PATH
 *   flushMs                        -> RIGSCOPE_HUB_FLUSH_MS (50 ms by default so
 *                                     ack latency is comparable at a 100 ms cadence)
 *   collectors/grants              -> <stateDir>/collectors.json, imported once
 *   browserUsers                   -> <stateDir>/users.json, imported once
 *
 * Bootstrap files are written 0600. The fixture password hash is scrypt while
 * the Go hub stores argon2id, so a valid scrypt fixture hash is replaced with
 * an argon2id hash of the same public fixture password. A malformed value is
 * passed through and rejected at startup, preserving configuration-validation
 * cases. `browserUsers: false` sets RIGSCOPE_HUB_DISABLE_BOOTSTRAP so the hub
 * really starts without a browser account instead of printing an owner
 * password.
 */
export function startHubProcess(t: TestContext, spec: HubLaunchSpec): ChildHandle {
  const host = spec.host ?? "127.0.0.1";
  const configuredPort = spec.port ?? 0;
  // The installer config schema requires 1..65535, while the harness asks for
  // an ephemeral listener. The CLI accepts RIGSCOPE_HUB_PORT=0 after loading
  // the config.
  const port = configuredPort === 0 ? 1 : configuredPort;
  const stateDir = stateDirectory(t, spec.stateDir, "hub");
  privateDirectory(stateDir);
  writePrivateJson(join(stateDir, CONFIG_FILE), {
    version: 1,
    components: {
      hub: {
        enabled: true,
        ...(spec.telemetryMode ? { telemetryMode: spec.telemetryMode } : {}),
        listen: host,
        port,
        origin: "",
        tlsCert: "",
        tlsKey: "",
        trustedProxies: [],
      },
      // The hub-only harness still validates the whole document. Collector
      // settings are the schema defaults; the component is disabled because
      // the suites launch their own collector process.
      collector: {
        enabled: false,
        destinations: [],
        samplingInterval: "2s",
        sharing: {
          cpu: true,
          memory: true,
          swap: true,
          gpus: true,
          disks: true,
          network: true,
          networkAddresses: false,
          processes: true,
          processArguments: false,
        },
        hardware: { gpuRegisters: false },
      },
    },
  });

  if (spec.collectors !== undefined || spec.grants !== undefined) {
    // Invalid entries are passed through rather than thrown here: the
    // configuration-validation cases require the hub itself to exit nonzero
    // before printing a listening line.
    writePrivateJson(join(stateDir, "collectors.json"), {
      collectors: spec.collectors ?? [],
      grants: spec.grants ?? [],
    });
  }

  const childEnv: Record<string, string | undefined> = {};
  if (spec.browserUsers === false) {
    childEnv.RIGSCOPE_HUB_DISABLE_BOOTSTRAP = "1";
  } else if (typeof spec.browserUsers === "string") {
    writePrivateText(join(stateDir, "users.json"), spec.browserUsers);
  } else if (spec.browserUsers !== undefined) {
    writePrivateJson(join(stateDir, "users.json"), { users: translateBrowserUsers(spec.browserUsers) });
  }
  if (spec.flushMs !== null) childEnv.RIGSCOPE_HUB_FLUSH_MS = String(spec.flushMs ?? 50);
  if (spec.databasePath !== undefined) childEnv.RIGSCOPE_HUB_DATABASE_PATH = spec.databasePath;
  if (configuredPort === 0) childEnv.RIGSCOPE_HUB_PORT = "0";

  const child = startProcess(
    spec.label ?? "hub",
    goBinaryPath(),
    ["hub", "--config", join(stateDir, CONFIG_FILE), "--state-dir", stateDir],
    baseEnv(childEnv),
    { cwd: ROOT },
  );
  t.after(() => child.stop());
  return child;
}

type Argon2Sync = (
  algorithm: "argon2id",
  parameters: {
    message: Buffer;
    nonce: Buffer;
    parallelism: number;
    tagLength: number;
    memory: number;
    passes: number;
  },
) => Buffer;

// @types/node here predates crypto.argon2Sync, but Node 26 provides it. The
// parameters match internal/hub/auth's fixed argon2id profile.
const argon2Sync = (createRequire(import.meta.url)("node:crypto") as { argon2Sync: Argon2Sync }).argon2Sync;

function goPasswordHash(password: string): string {
  const salt = randomBytes(16);
  const key = argon2Sync("argon2id", {
    message: Buffer.from(password, "utf8"),
    nonce: salt,
    parallelism: 2,
    tagLength: 32,
    memory: 32 * 1024,
    passes: 3,
  });
  const raw = (value: Buffer): string => value.toString("base64").replace(/=+$/, "");
  return `$argon2id$v=19$m=32768,t=3,p=2$${raw(salt)}$${raw(key)}`;
}

/**
 * Translate fixture browser users into the Go users.json bootstrap. A fixture
 * scrypt hash is re-derived in the Go format from the same public fixture
 * password; anything that is not a recognised hash is passed through so
 * startup validation still fails.
 */
function translateBrowserUsers(
  users: { username: string; machineIds: "*" | string[]; admin?: boolean }[],
): unknown[] {
  const hash = goPasswordHash(BROWSER_PASSWORD);
  return users.map((user) => ({
    username: user.username,
    passwordHash: hash,
    machineIds: user.machineIds,
    admin: user.admin === true,
  }));
}
