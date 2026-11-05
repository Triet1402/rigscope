import { existsSync, lstatSync, renameSync } from 'node:fs';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { updateInstallation, type InstallationState } from '../../collector/local-state';
import type { DestinationEndpoint } from '../../collector/config';
import { outboxDirFor } from '../../collector/destinations/outbox';
import { parseHubConfig } from '../hub/config';
import { writePrivateFile } from '../operations/fs';
import { connectUrl, hubEnvironment, privateDirectory, readPrivate, readSetup, validatePort, writeSetup, type Setup } from './config';

export type AccessOptions = {origin?: string; local?: boolean; socket?: string; port?: string};

export type AccessPlan = {
  next: Setup;
  url: string;
  changed: boolean;
  /** True only when this installation's colocated collector destination moves. */
  collectorChanged: boolean;
  oldEndpoint: string;
  newEndpoint: string;
  oldQueue: string | null;
  newQueue: string | null;
  state: InstallationState | null;
  self: DestinationEndpoint | null;
};

function endpointFor(config: Setup): string {
  return config.origin ? connectUrl(config.origin) : `ws://127.0.0.1:${config.port}/api/collector/connect`;
}

function readCollectorState(dir: string): InstallationState | null {
  try { return JSON.parse(readPrivate(join(dir, 'collector', 'state.json'))) as InstallationState; }
  catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null; throw error; }
}

/**
 * Read-only preview of an access change. This lets the CLI stop only the
 * components whose running state actually conflicts with the mutation.
 */
export function planAccessChange(dir: string, options: AccessOptions): AccessPlan {
  if (options.origin && options.port !== undefined) {
    throw new Error('--port is unused with --origin; the hub serves HTTPS on its public origin.');
  }
  if (options.origin && options.local) throw new Error('Choose --origin https://your-hub or --local.');
  if (!options.origin && !options.local && options.port === undefined) {
    throw new Error('Choose --origin https://your-hub, --local, or --port PORT.');
  }
  if (options.socket && !options.origin) throw new Error('--socket requires --origin.');
  const port = options.port === undefined ? undefined : validatePort(options.port);
  const previous = readSetup(dir);
  if (!previous) throw new Error('No saved installation. Run rigscope install first.');
  if (previous.users.length === 0) throw new Error('This installation has no hub configuration. Use rigscope connect to select a collector hub.');
  const next = {...previous, port: port ?? previous.port};
  if (options.local) { delete next.origin; delete next.socket; }
  else if (options.origin) { next.origin = options.origin; next.socket = options.socket ?? join(dir, 'run', 'hub.sock'); }
  // Validate the exact HTTPS identity, port and private socket before writing anything.
  parseHubConfig(hubEnvironment(dir, next));

  const oldEndpoint = endpointFor(previous);
  const newEndpoint = endpointFor(next);
  const state = readCollectorState(dir);
  // Registry-enrolled colocated collectors have no HUB_COLLECTORS mapping, so
  // identify the local destination by its canonical hub endpoint instead.
  const self = state
    ? state.destinations.find(item => item.url === oldEndpoint) ?? null
    : null;
  const collectorChanged = self !== null && oldEndpoint !== newEndpoint;
  const configChanged = previous.origin !== next.origin || previous.socket !== next.socket || previous.port !== next.port;
  return {
    next,
    url: next.origin ?? `http://127.0.0.1:${next.port}`,
    changed: configChanged || collectorChanged,
    collectorChanged,
    oldEndpoint,
    newEndpoint,
    oldQueue: collectorChanged ? outboxDirFor(join(dir, 'collector'), oldEndpoint) : null,
    newQueue: collectorChanged ? outboxDirFor(join(dir, 'collector'), newEndpoint) : null,
    state,
    self,
  };
}

/**
 * Change hub access. The caller holds the hub lease and, when
 * `collectorChanged` is true, the collector lease as well.
 */
export function configureAccess(dir: string, options: AccessOptions): {
  changed: boolean; backup: string | null; url: string;
} {
  const plan = planAccessChange(dir, options);
  if (!plan.changed) return {changed: false, backup: null, url: plan.url};

  const configPath = join(dir, 'config.json');
  const statePath = join(dir, 'collector', 'state.json');
  const beforeConfig = readPrivate(configPath);
  const beforeState = plan.state === null ? null : readPrivate(statePath);
  if (plan.collectorChanged && plan.oldQueue && plan.newQueue && plan.oldQueue !== plan.newQueue && existsSync(plan.newQueue)) {
    throw new Error('The destination already has an outbox. Preserve or replay it before changing access.');
  }
  if (plan.oldQueue && existsSync(plan.oldQueue)) {
    const stat = lstatSync(plan.oldQueue);
    if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error('Collector outbox must be a real directory.');
  }

  const backup = join(dir, 'backups', `access-${randomUUID()}`);
  privateDirectory(backup);
  writePrivateFile(join(backup, 'config.json'), beforeConfig);
  if (beforeState !== null) writePrivateFile(join(backup, 'collector-state.json'), beforeState);
  writePrivateFile(join(backup, 'change.json'), JSON.stringify({
    oldEndpoint: plan.collectorChanged ? plan.oldEndpoint : null,
    newEndpoint: plan.collectorChanged ? plan.newEndpoint : null,
    oldQueue: plan.oldQueue, newQueue: plan.newQueue,
  }));
  let movedQueue = false;
  try {
    if (plan.oldQueue && plan.newQueue && plan.oldQueue !== plan.newQueue && existsSync(plan.oldQueue)) {
      // Move the whole SQLite directory, including WAL, so pending samples remain deliverable.
      renameSync(plan.oldQueue, plan.newQueue);
      movedQueue = true;
    }
    if (plan.collectorChanged && plan.state && plan.self) {
      const collectorId = plan.state.collectorId;
      const token = plan.self.token;
      updateInstallation(statePath, state => {
        if (state.collectorId !== collectorId) throw new Error('Collector identity changed while access was being updated.');
        return {...state, destinations: state.destinations.map(destination =>
          destination.url === plan.oldEndpoint && destination.token === token
            ? {...destination, url: plan.newEndpoint} : destination)};
      });
    }
    writeSetup(dir, plan.next);
  } catch (error) {
    try {
      writePrivateFile(configPath, beforeConfig);
      if (beforeState !== null) writePrivateFile(statePath, beforeState);
      if (movedQueue) renameSync(plan.newQueue!, plan.oldQueue!);
    } catch {
      throw new Error(`Access change could not be recovered. Keep the service stopped and restore configuration from ${backup}.`);
    }
    throw error;
  }
  return {changed: true, backup, url: plan.url};
}
