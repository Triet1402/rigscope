import { randomBytes, randomUUID } from 'node:crypto';
import { existsSync, lstatSync, mkdirSync, readFileSync } from 'node:fs';
import { request } from 'node:http';
import { homedir } from 'node:os';
import { join, resolve } from 'node:path';
import { hashPassword, isPasswordHash } from '../hub/auth/password';
import { parseHubConfig } from '../hub/config';
import { parseBrowserUsers } from '../hub/auth/config';
import { updateInstallation } from '../../collector/local-state';
import { enrollCollector } from '../../collector/enroll';
import { parseCollectorConfig } from '../../collector/config';
import { openHubDatabase } from '../hub/storage/db';
import { CollectorRegistry } from '../hub/enrollment/registry';
import { writePrivateFile } from '../operations/fs';
import { databasePath } from './release-state';

export type Component = 'hub' | 'collector';
export type ComponentState = { enabled: boolean };
export type Components = Record<Component, ComponentState>;
export const COMPONENTS: readonly Component[] = ['hub', 'collector'];
export const RESET_PENDING_FILE = 'reset-pending.json';

export type Setup = {
  role: 'local' | 'hub' | 'collector';
  port: number;
  origin?: string;
  socket?: string;
  maxCollectorConnections?: number;
  users: { username: string; passwordHash: string; machineIds: '*'; admin: true }[];
  collectors: { collectorId: string; machineId: string; token: string }[];
  components: Components;
};
export const DEFAULT_SHARING = {cpu: true, memory: true, swap: true, gpus: true, disks: true, network: true,
  networkAddresses: false, processes: true, processArguments: false};

export function stateDirectory(value?: string): string {
  return resolve(value ?? process.env.RIGSCOPE_STATE_DIR ?? join(process.env.XDG_STATE_HOME || join(homedir(), '.local', 'state'), 'rigscope'));
}
export function privateDirectory(path: string): void {
  mkdirSync(path, {recursive: true, mode: 0o700});
  const stat = lstatSync(path);
  if (!stat.isDirectory() || (stat.mode & 0o077) !== 0 || stat.uid !== process.getuid?.()) {
    throw new Error(`Use an owned private directory (0700): ${path}`);
  }
}
export function readPrivate(path: string): string {
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.size > 64 * 1024 || (stat.mode & 0o077) !== 0 || stat.uid !== process.getuid?.()) {
    throw new Error(`Use an owned private regular file (0600): ${path}`);
  }
  return readFileSync(path, 'utf8');
}

export function defaultComponents(role: Setup['role']): Components {
  return {
    hub: {enabled: role !== 'collector'},
    collector: {enabled: role !== 'hub'},
  };
}
function isComponentState(value: unknown): value is ComponentState {
  return value !== null && typeof value === 'object' && typeof (value as {enabled?: unknown}).enabled === 'boolean';
}
function requireComponents(value: unknown): Components {
  if (value === null || typeof value !== 'object') throw new Error('Invalid config.json; restore its private backup.');
  const record = value as Record<string, unknown>;
  if (!isComponentState(record.hub) || !isComponentState(record.collector)) {
    throw new Error('Invalid config.json; restore its private backup.');
  }
  return {hub: {enabled: record.hub.enabled}, collector: {enabled: record.collector.enabled}};
}

export function componentEnabled(config: Setup, component: Component): boolean {
  return config.components[component].enabled;
}
export function enabledComponents(config: Setup): Component[] {
  return COMPONENTS.filter(component => config.components[component].enabled);
}
function hubConfigured(config: Setup): boolean {
  return componentEnabled(config, 'hub') && config.users.length > 0;
}

export function readSetup(dir: string): Setup | null {
  let raw: string;
  try { raw = readPrivate(join(dir, 'config.json')); }
  catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null; throw error; }
  const parsed = JSON.parse(raw) as Record<string, unknown>;
  if ('version' in parsed || 'formatVersion' in parsed) {
    throw new Error('This installation uses an older configuration shape. Run rigscope install or rigscope upgrade to reset it.');
  }
  if (!['local', 'hub', 'collector'].includes(parsed.role as string) ||
      typeof parsed.port !== 'number' || !Number.isInteger(parsed.port) || parsed.port < 1 || parsed.port > 65535 ||
      !Array.isArray(parsed.users) || !Array.isArray(parsed.collectors) ||
      (parsed.users as {passwordHash?: unknown}[]).some(user => !isPasswordHash(user.passwordHash))) {
    throw new Error('Invalid config.json; restore its private backup.');
  }
  const config: Setup = {
    role: parsed.role as Setup['role'],
    port: parsed.port,
    ...(parsed.origin === undefined ? {} : {origin: parsed.origin as string}),
    ...(parsed.socket === undefined ? {} : {socket: parsed.socket as string}),
    ...(parsed.maxCollectorConnections === undefined ? {} : {maxCollectorConnections: parsed.maxCollectorConnections as number}),
    users: parsed.users as Setup['users'],
    collectors: parsed.collectors as Setup['collectors'],
    components: requireComponents(parsed.components),
  };
  parseHubConfig(hubEnvironment(dir, config));
  parseBrowserUsers(hubEnvironment(dir, config));
  if (componentEnabled(config, 'hub') && config.users.length === 0) throw new Error('Configure at least one hub user.');
  return config;
}

type JsonFile = {
  present: boolean;
  value: Record<string, unknown> | null;
};

function readJsonFile(path: string): JsonFile {
  let raw: string;
  try { raw = readPrivate(path); }
  catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return {present: false, value: null}; throw error; }
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('not an object');
    return {present: true, value: parsed as Record<string, unknown>};
  } catch {
    throw new Error(`Invalid saved state at ${path}; restore it or cleanly uninstall and reinstall. No state was reset.`);
  }
}

function isCanonicalSetupFile(value: Record<string, unknown>): boolean {
  if ('version' in value || 'formatVersion' in value) return false;
  if (!['local', 'hub', 'collector'].includes(value.role as string)) return false;
  if (typeof value.port !== 'number' || !Number.isInteger(value.port) || value.port < 1 || value.port > 65535) return false;
  if (!Array.isArray(value.users) || !Array.isArray(value.collectors)) return false;
  if ((value.users as {passwordHash?: unknown}[]).some(user => !isPasswordHash(user?.passwordHash))) return false;
  try { requireComponents(value.components); }
  catch { return false; }
  return true;
}

function isCanonicalCollectorFile(value: Record<string, unknown>): boolean {
  if ('version' in value || 'formatVersion' in value) return false;
  if (typeof value.collectorId !== 'string' || !/^[\x21-\x7e]{1,128}$/.test(value.collectorId)) return false;
  if (!Array.isArray(value.destinations)) return false;
  try {
    if (value.destinations.length > 0) parseCollectorConfig({COLLECTOR_HUBS: JSON.stringify(value.destinations)});
    if (Array.isArray(value.pendingEnrollments)) {
      for (const pending of value.pendingEnrollments) {
        if (pending === null || typeof pending !== 'object') return false;
        parseCollectorConfig({COLLECTOR_HUBS: JSON.stringify([{url: pending.url, token: 'pending-enrollment'}])});
      }
    }
  } catch { return false; }
  return value.pendingEnrollments === undefined || Array.isArray(value.pendingEnrollments);
}

function isCanonicalReleaseFile(dir: string, value: Record<string, unknown>): boolean {
  if ('version' in value || 'formatVersion' in value) return false;
  if (value.stateDir !== dir) return false;
  return value.backups === undefined ||
    (value.backups !== null && typeof value.backups === 'object' && !Array.isArray(value.backups));
}

function fileNeedsReset(path: string, canonical: (value: Record<string, unknown>) => boolean): boolean {
  const file = readJsonFile(path);
  if (!file.present || file.value === null) return false;
  if (canonical(file.value)) return false;
  if ('version' in file.value || 'formatVersion' in file.value) return true;
  throw new Error(`Invalid saved state at ${path}; restore it or cleanly uninstall and reinstall. No state was reset.`);
}

/**
 * True when this directory holds configuration or state that the canonical
 * reader cannot use. Detection is structural: required fields and the absence
 * of a format version, never a version-number map. Reset is deliberately not
 * performed here; install and upgrade coordinate it after replacement is staged.
 */
export function installationNeedsReset(dir: string): boolean {
  return existsSync(join(dir, RESET_PENDING_FILE)) ||
    fileNeedsReset(join(dir, 'config.json'), isCanonicalSetupFile) ||
    fileNeedsReset(join(dir, 'collector', 'state.json'), isCanonicalCollectorFile) ||
    fileNeedsReset(join(dir, 'hub', 'enrollment.json'), value =>
      Object.keys(value).every(key => key === 'enrollments' || key === 'consumed') &&
      Array.isArray(value.enrollments) && Array.isArray(value.consumed)) ||
    fileNeedsReset(join(dir, 'release-state.json'), value => isCanonicalReleaseFile(dir, value));
}

/** Reuse the selected installation preset when replacing its settings. */
export function installationRole(dir: string): Setup['role'] | undefined {
  for (const name of [RESET_PENDING_FILE, 'config.json']) {
    const role = readJsonFile(join(dir, name)).value?.role;
    if (role === 'local' || role === 'hub' || role === 'collector') return role;
  }
  return undefined;
}

/** Persist the one canonical configuration shape. */
export function writeSetup(dir: string, config: Setup): void {
  privateDirectory(dir);
  const next: Setup = {
    role: config.role,
    port: config.port,
    ...(config.origin === undefined ? {} : {origin: config.origin}),
    ...(config.socket === undefined ? {} : {socket: config.socket}),
    ...(config.maxCollectorConnections === undefined ? {} : {maxCollectorConnections: config.maxCollectorConnections}),
    users: config.users,
    collectors: config.collectors,
    components: requireComponents(config.components),
  };
  writePrivateFile(join(dir, 'config.json'), `${JSON.stringify(next, null, 2)}\n`);
}

export function hubEnvironment(dir: string, config: Setup): Record<string, string> {
  return {
    ...(!config.origin ? {HUB_HOST: '127.0.0.1', HUB_PORT: String(config.port)} : {}),
    HUB_STATE_FILE: join(dir, 'hub', 'enrollment.json'), HUB_DATABASE_PATH: join(dir, 'hub', 'telemetry.sqlite'),
    HUB_BROWSER_USERS: JSON.stringify(config.users), HUB_COLLECTORS: JSON.stringify(config.collectors),
    ...(config.maxCollectorConnections === undefined ? {} : {HUB_MAX_COLLECTOR_CONNECTIONS: String(config.maxCollectorConnections)}),
    ...(config.origin ? {HUB_PUBLIC_ORIGIN: config.origin, HUB_SOCKET_PATH: config.socket ?? join(dir, 'run', 'hub.sock')} : {}),
  };
}

export function validatePort(value: string | undefined, fallback = 8081): number {
  const port = Number(value ?? String(fallback));
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('--port must be 1–65535.');
  return port;
}

function collectorStatePath(dir: string): string {
  return join(dir, 'collector', 'state.json');
}

export function collectorHasDestinations(dir: string): boolean {
  try {
    const state = JSON.parse(readPrivate(collectorStatePath(dir))) as {destinations?: unknown[]};
    return Array.isArray(state.destinations) && state.destinations.length > 0;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return false;
    throw error;
  }
}

function colocatedEndpoint(config: Setup): string {
  return config.origin ? connectUrl(config.origin) : `ws://127.0.0.1:${config.port}/api/collector/connect`;
}

function hubDatabase(dir: string): string {
  return databasePath(dir);
}

/**
 * True when the authoritative hub process holds its database lease. systemd
 * state is not enough: a foreground trial owns the same lease.
 */
export function hubLeaseLive(dir: string): boolean {
  const path = `${hubDatabase(dir)}.hub.lock`;
  let raw: string;
  try { raw = readPrivate(path); }
  catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return false; throw error; }
  try {
    const pid = (JSON.parse(raw) as {pid?: unknown}).pid;
    if (typeof pid !== 'number' || !Number.isSafeInteger(pid) || pid <= 0) return true;
    try { process.kill(pid, 0); return true; }
    catch (error) { return (error as NodeJS.ErrnoException).code === 'EPERM'; }
  } catch {
    // An unreadable or malformed lease is ambiguous; assume the hub is live so
    // callers never write the registry behind a running process.
    return true;
  }
}

function grantForCollector(dir: string, config: Setup, collectorId: string): string {
  const database = openHubDatabase(hubDatabase(dir));
  try {
    const registry = new CollectorRegistry({database, legacyIdentities: config.collectors});
    const existing = registry.identities(true).find(item => item.collectorId === collectorId);
    return registry.createGrant('local-cli', existing ? {replaceMachineId: existing.machineId} : {}).token;
  } finally { database.close(); }
}

/** Offline pairing: write the authoritative registry and the matching outbox destination. */
function pairCollectorOffline(dir: string, config: Setup, collectorId: string): void {
  const database = openHubDatabase(hubDatabase(dir));
  let identity: {collectorId: string; machineId: string; token: string};
  try {
    const registry = new CollectorRegistry({database, legacyIdentities: config.collectors});
    const existing = registry.identities(true).find(item => item.collectorId === collectorId);
    const grant = registry.createGrant('local-cli', existing ? {replaceMachineId: existing.machineId} : {});
    identity = registry.enroll({collectorId, token: grant.token});
  } finally { database.close(); }
  const endpoint = colocatedEndpoint(config);
  updateInstallation(collectorStatePath(dir), current => current.destinations.length > 0 ? current : ({
    ...current,
    destinations: [{url: endpoint, token: identity.token, sharing: DEFAULT_SHARING}],
  }));
}

async function enrollOverUnixSocket(dir: string, config: Setup, collectorId: string, token: string): Promise<void> {
  const url = new URL(colocatedEndpoint(config));
  const path = collectorStatePath(dir);
  const state = updateInstallation(path);
  const requestId = randomUUID();
  const recoverySecret = randomBytes(32).toString('base64url');
  const body = JSON.stringify({collectorId, requestId, recoverySecret});
  const response = await new Promise<{status: number; text: string}>((resolveResponse, reject) => {
    const req = request({
      socketPath: config.socket ?? join(dir, 'run', 'hub.sock'),
      path: '/api/collector/enroll', method: 'POST',
      headers: {
        Host: url.host, Authorization: `Bearer ${token}`, 'Content-Type': 'application/json',
        'Content-Length': Buffer.byteLength(body), Connection: 'close',
      },
    }, res => {
      const chunks: Buffer[] = [];
      let bytes = 0;
      res.on('data', (chunk: Buffer) => {
        bytes += chunk.byteLength;
        if (bytes > 4096) { req.destroy(new Error('oversized enrollment response')); return; }
        chunks.push(chunk);
      });
      res.on('end', () => resolveResponse({status: res.statusCode ?? 0, text: Buffer.concat(chunks).toString('utf8')}));
    });
    req.setTimeout(5000, () => req.destroy(new Error('enrollment request timed out')));
    req.once('error', reject);
    req.end(body);
  });
  if (response.status !== 201) throw new Error(`Enrollment rejected (${response.status}); check the hub grant and state.`);
  const result = JSON.parse(response.text) as {collectorId?: unknown; machineId?: unknown; token?: unknown};
  if (result.collectorId !== state.collectorId || typeof result.machineId !== 'string' || !result.machineId ||
      typeof result.token !== 'string') throw new Error('Invalid enrollment response.');
  updateInstallation(path, current => current.destinations.length > 0 ? current : ({
    ...current,
    destinations: [{url: url.href, token: result.token as string, sharing: DEFAULT_SHARING}],
  }));
}

/**
 * Fresh-installation pairing: the hub has not created its authoritative
 * registry yet, so HUB_COLLECTORS bootstrap remains valid. Anything after the
 * first hub start must use the registry instead.
 */
function pairColocatedCollectorConfig(dir: string, config: Setup): Setup {
  const state = updateInstallation(collectorStatePath(dir));
  if (state.destinations.length > 0) return config;
  const endpoint = colocatedEndpoint(config);
  const existing = config.collectors.find(item => item.collectorId === state.collectorId);
  const token = existing?.token ?? randomBytes(32).toString('base64url');
  const machineId = existing?.machineId ?? randomUUID();
  const updated = updateInstallation(collectorStatePath(dir), current => current.destinations.length > 0 ? current : ({
    ...current,
    destinations: [{url: endpoint, token, sharing: DEFAULT_SHARING}],
  }));
  const mapping = {collectorId: updated.collectorId, machineId, token};
  return {...config, collectors: [
    ...config.collectors.filter(item => item.collectorId !== mapping.collectorId), mapping,
  ]};
}

/**
 * Pair a colocated collector with no destinations. A running hub is updated
 * through a one-use registry grant and the normal enrollment HTTP endpoint, so
 * its in-memory registry sees the new machine immediately. A stopped hub with
 * an existing registry is updated directly in SQLite for the next start.
 */
export async function pairColocatedCollector(dir: string): Promise<Setup> {
  const config = readSetup(dir);
  if (!config) throw new Error('No saved installation. Run rigscope install first.');
  if (!hubConfigured(config)) throw new Error('Configure and enable the hub before pairing a colocated collector.');
  const state = updateInstallation(collectorStatePath(dir));
  if (state.destinations.length > 0) return config;
  if (!existsSync(hubDatabase(dir))) {
    const paired = pairColocatedCollectorConfig(dir, config);
    writeSetup(dir, paired);
    return readSetup(dir)!;
  }
  if (hubLeaseLive(dir)) {
    const token = grantForCollector(dir, config, state.collectorId);
    if (config.origin) await enrollOverUnixSocket(dir, config, state.collectorId, token);
    else await enrollCollector(collectorStatePath(dir), colocatedEndpoint(config), token, DEFAULT_SHARING);
  } else {
    pairCollectorOffline(dir, config, state.collectorId);
  }
  return readSetup(dir)!;
}

type SetupOptions = {role?: string; port?: string; origin?: string; socket?: string};

/** Validate fresh-setup options without touching the installation directory. */
export function validateSetupOptions(options: SetupOptions = {}): {role: Setup['role']; port: number; origin?: string; socket?: string} {
  const role = (options.role ?? 'local') as Setup['role'];
  if (!['local', 'hub', 'collector'].includes(role)) throw new Error('--role must be local, hub, or collector.');
  const port = validatePort(options.port);
  if (options.socket && !options.origin) throw new Error('--socket requires --origin.');
  if (options.origin && role === 'collector') throw new Error('--origin requires a hub; use rigscope connect for a collector.');
  return {
    role,
    port,
    ...(options.origin === undefined ? {} : {origin: options.origin}),
    ...(options.socket === undefined ? {} : {socket: options.socket}),
  };
}

function baseConfig(dir: string, role: Setup['role'], port: number, password?: string, options: {origin?: string; socket?: string} = {}): Setup {
  return {
    role,
    port,
    ...(options.origin ? {origin: options.origin, socket: options.socket ?? join(dir, 'run', 'hub.sock')} : {}),
    users: password ? [{username: 'owner', passwordHash: hashPassword(password), machineIds: '*', admin: true}] : [],
    collectors: [],
    components: defaultComponents(role),
  };
}

export function initialize(dir: string, options: SetupOptions = {}): {config: Setup; password?: string; created: boolean} {
  privateDirectory(dir);
  // Never mix the canonical configuration with an older state file. The
  // coordinated install/upgrade reset owns that destructive transition.
  if (installationNeedsReset(dir)) {
    throw new Error('This installation uses an older configuration or state shape. Run rigscope install or rigscope upgrade to reset it.');
  }
  const existing = readSetup(dir);
  if (existing) {
    if ((options.role && options.role !== existing.role) || (options.port && Number(options.port) !== existing.port) ||
        (options.origin && options.origin !== existing.origin) || (options.socket && options.socket !== existing.socket)) {
      throw new Error('Setup already exists with different settings. To change access while preserving data, run rigscope configure --origin https://your-hub (or --local). To delete all saved data and choose a new role, run rigscope uninstall first (deletes saved data). Use another --state-dir for a separate installation.');
    }
    return {config: existing, created: false};
  }
  const selected = validateSetupOptions(options);
  const password = componentEnabled(baseConfig(dir, selected.role, selected.port), 'hub') ? randomBytes(18).toString('base64url') : undefined;
  let config = baseConfig(dir, selected.role, selected.port, password, {origin: selected.origin, socket: selected.socket});
  parseHubConfig(hubEnvironment(dir, config));
  for (const name of ['hub', 'collector', 'run']) privateDirectory(join(dir, name));
  if (componentEnabled(config, 'hub') && componentEnabled(config, 'collector') && !existsSync(hubDatabase(dir))) {
    config = pairColocatedCollectorConfig(dir, config);
  }
  writeSetup(dir, config);
  return {config, password, created: true};
}

/**
 * Enable or disable one component and complete the minimum setup it needs.
 * Disabling only flips the flag: credentials, destinations and data stay put.
 */
export function configureComponent(dir: string, component: Component, options: {
  enabled: boolean; origin?: string; socket?: string; port?: string;
  /** Internal: `connect` enrolls a remote hub instead of pairing locally. */
  pairColocated?: boolean;
}): {config: Setup; password?: string} {
  privateDirectory(dir);
  const existing = readSetup(dir);
  const port = options.port === undefined ? (existing?.port ?? 8081) : validatePort(options.port);
  let config: Setup = existing ?? baseConfig(dir, component, port);
  let password: string | undefined;

  if (component === 'hub') {
    if (options.origin !== undefined) {
      if (options.origin === '') throw new Error('Use --origin https://your-hub.');
      config.origin = options.origin;
      config.socket = options.socket ?? join(dir, 'run', 'hub.sock');
    } else if (options.socket) {
      if (!config.origin) throw new Error('--socket requires --origin.');
      config.socket = options.socket;
    }
    config.port = port;
    config.components = {...config.components, hub: {enabled: options.enabled}};
    if (options.enabled && config.users.length === 0) {
      password = randomBytes(18).toString('base64url');
      config.users = [{username: 'owner', passwordHash: hashPassword(password), machineIds: '*', admin: true}];
    }
    for (const name of ['hub', 'run']) privateDirectory(join(dir, name));
    parseHubConfig(hubEnvironment(dir, config));
    parseBrowserUsers(hubEnvironment(dir, config));
  } else {
    if (options.origin !== undefined || options.socket !== undefined || options.port !== undefined) {
      throw new Error('Access options apply to the hub component.');
    }
    config.components = {...config.components, collector: {enabled: options.enabled}};
    for (const name of ['collector', 'run']) privateDirectory(join(dir, name));
    if (options.enabled && options.pairColocated !== false && hubConfigured(config) && !existsSync(hubDatabase(dir))) {
      config = pairColocatedCollectorConfig(dir, config);
    }
  }

  writeSetup(dir, config);
  return {config: readSetup(dir)!, ...(password ? {password} : {})};
}

export function connectUrl(value: string): string {
  let url: URL;
  try { url = new URL(value); } catch { throw new Error('Use a hub URL such as https://hub.example.com.'); }
  if (url.protocol === 'https:') url.protocol = 'wss:';
  else if (url.protocol === 'http:') url.protocol = 'ws:';
  if (url.hostname === 'localhost') url.hostname = '127.0.0.1';
  if (url.pathname === '/') url.pathname = '/api/collector/connect';
  // Reuse the collector's canonical URL/TLS boundary before consuming a grant.
  return url.href;
}
