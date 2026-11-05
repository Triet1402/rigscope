/**
 * Read-only installation diagnostics for the managed CLI.
 *
 * Checks are role-appropriate: a hub without a collector is valid, and a
 * collector without a local hub database is valid. Secrets (password hashes,
 * collector tokens, CSRF values, environment contents) are never printed.
 * Every check returns an actionable detail instead of a stack trace.
 */

import { openSync, readSync, closeSync, existsSync, lstatSync, readdirSync, statfsSync } from 'node:fs';
import type { Dirent } from 'node:fs';
import { join } from 'node:path';
import { parseBrowserUsers } from '../hub/auth/config';
import { parseHubConfig } from '../hub/config';
import { inspectDatabase } from '../operations/database';
import { componentEnabled, enabledComponents, readPrivate, readSetup, type Setup } from './config';
import { probeHealth, probeTcp, probeTls, healthTarget } from './health';
import {
  assertRuntimeArtifacts,
  databasePath,
  inspectDatabaseWithRuntime,
  runtimeVersion,
  type RuntimeInspect,
} from './release-state';
import { serviceStatus, type ServiceStatus } from './services';

export type CheckStatus = 'ok' | 'warn' | 'fail' | 'skip';

export type DoctorCheck = {
  id: string;
  label: string;
  status: CheckStatus;
  detail: string;
  action?: string;
};

export type DoctorReport = {
  ok: boolean;
  stateDir: string;
  role: Setup['role'] | null;
  version: string | null;
  checks: DoctorCheck[];
};

export type DoctorDependencies = {
  serviceStatus?: (dir: string, component?: 'hub' | 'collector') => ServiceStatus;
  probeHealth?: typeof probeHealth;
  probeTcp?: typeof probeTcp;
  probeTls?: typeof probeTls;
  inspectRuntime?: typeof inspectDatabaseWithRuntime;
  inspectDatabase?: typeof inspectDatabase;
  diskFree?: (path: string) => { free: number; total: number } | null;
  sensorReadable?: (paths: string[]) => string[];
};

const DEFAULT_DEPENDENCIES: Required<DoctorDependencies> = {
  serviceStatus,
  probeHealth,
  probeTcp,
  probeTls,
  inspectRuntime: inspectDatabaseWithRuntime,
  inspectDatabase,
  diskFree: (path) => {
    try {
      const stats = statfsSync(path);
      return { free: Number(stats.bavail) * Number(stats.bsize), total: Number(stats.blocks) * Number(stats.bsize) };
    } catch {
      return null;
    }
  },
  sensorReadable: (paths) => paths.filter((path) => {
    let fd: number | undefined;
    try {
      fd = openSync(path,'r');
      readSync(fd,Buffer.alloc(1),0,1,0);
      return true;
    } catch {
      return false;
    } finally {if (fd !== undefined) closeSync(fd);}
  }),
};

export async function diagnoseInstallation(
  dir: string,
  injected: DoctorDependencies = {},
): Promise<DoctorReport> {
  const deps = { ...DEFAULT_DEPENDENCIES, ...injected };
  const checks: DoctorCheck[] = [];
  const add = (check: DoctorCheck): void => {
    checks.push(check);
  };

  const major = Number(process.versions.node.split('.')[0]);
  add({
    id: 'node',
    label: 'Node.js',
    status: major >= 26 ? 'ok' : 'fail',
    detail: `${process.versions.node}${major >= 26 ? '' : '; this release requires Node.js 26 or newer'}`,
    ...(major >= 26 ? {} : { action: 'Install Node.js 26+ and restart the service.' }),
  });

  let config: Setup;
  try {
    const loaded = readSetup(dir);
    if (!loaded) throw new Error(`no config.json in ${dir}`);
    config = loaded;
  } catch (error) {
    add({
      id: 'config',
      label: 'Configuration',
      status: 'fail',
      detail: errorText(error),
      action: 'Run rigscope install, or restore the private configuration backup.',
    });
    return finish(dir, null, null, checks);
  }
  const hubEnabled = componentEnabled(config, 'hub');
  const collectorEnabled = componentEnabled(config, 'collector');
  const enabled = enabledComponents(config);
  add({
    id: 'config',
    label: 'Configuration',
    status: 'ok',
    detail: `${config.role}; ${enabled.length > 0 ? enabled.join(', ') : 'no components'} enabled; ${config.origin ?? `http://127.0.0.1:${config.port}`}`,
  });
  try {
    add({
      id: 'config-valid',
      label: 'Configuration values',
      status: 'ok',
      detail: `${config.users.length} browser user(s), ${config.collectors.length} collector credential(s)`,
    });
    parseHubConfig(hubEnvironment(dir, config));
    parseBrowserUsers(hubEnvironment(dir, config));
  } catch (error) {
    checks[checks.length - 1] = {
      id: 'config-valid',
      label: 'Configuration values',
      status: 'fail',
      detail: errorText(error),
      action: 'Restore the private configuration backup; do not edit secrets into shared files.',
    };
  }

  for (const [id, label, path] of [
    ['state-dir', 'State directory', dir],
    ['config-mode', 'Configuration permissions', join(dir, 'config.json')],
  ] as const) {
    try {
      const stat = lstatSync(path);
      const privateEntry = id === 'state-dir' ? stat.isDirectory() : stat.isFile();
      const privateMode = (stat.mode & 0o077) === 0;
      add({
        id,
        label,
        status: privateEntry && privateMode ? 'ok' : 'fail',
        detail: privateEntry && privateMode ? path : `${path} must be ${id === 'state-dir' ? '0700' : '0600'}`,
        ...(privateEntry && privateMode ? {} : { action: `chmod ${id === 'state-dir' ? '700' : '600'} ${JSON.stringify(path)}` }),
      });
    } catch (error) {
      add({ id, label, status: 'fail', detail: errorText(error) });
    }
  }

  let runtime: string | null = null;
  if (!existsSync(join(dir, 'runtime'))) {
    add({
      id: 'runtime',
      label: 'Runtime',
      status: 'fail',
      detail: 'no managed runtime is installed',
      action: 'Run rigscope install.',
    });
  } else {
    try {
      assertRuntimeArtifacts(join(dir, 'runtime'), 'runtime');
      const version = runtimeVersion(join(dir, 'runtime'));
      runtime = join(dir, 'runtime');
      add({ id: 'runtime', label: 'Runtime', status: 'ok', detail: `rigscope ${version}` });
    } catch (error) {
      add({
        id: 'runtime',
        label: 'Runtime',
        status: 'fail',
        detail: errorText(error),
        action: 'Re-run rigscope upgrade --from <package>, or roll back to the previous runtime.',
      });
    }
  }
  if (existsSync(join(dir, 'runtime-previous'))) {
    try {
      add({ id: 'previous-runtime', label: 'Previous runtime', status: 'ok', detail: `rigscope ${runtimeVersion(join(dir, 'runtime-previous'))}` });
    } catch (error) {
      add({ id: 'previous-runtime', label: 'Previous runtime', status: 'warn', detail: errorText(error) });
    }
  } else {
    add({ id: 'previous-runtime', label: 'Previous runtime', status: 'skip', detail: 'none retained for rollback' });
  }

  const services = {
    hub: deps.serviceStatus(dir, 'hub'),
    collector: deps.serviceStatus(dir, 'collector'),
  };
  const awaitingConnection = collectorAwaiting(dir);
  for (const component of ['hub', 'collector'] as const) {
    const service = services[component];
    const enabled = component === 'hub' ? hubEnabled : collectorEnabled;
    const label = component === 'hub' ? 'Hub service' : 'Collector service';
    if (!enabled) {
      add({
        id: `service:${component}`,
        label,
        status: service.active ? 'warn' : 'ok',
        detail: service.active ? `${service.unit}: active while disabled` : 'disabled',
        ...(service.active ? { action: `Run rigscope stop ${component} to stop it.` } : {}),
      });
      continue;
    }
    if (!service.installed) {
      add({
        id: `service:${component}`,
        label,
        status: 'fail',
        detail: `${service.unit}: unit is not installed`,
        action: 'Run rigscope install to write the component units.',
      });
      continue;
    }
    if (service.active) {
      add({ id: `service:${component}`, label, status: 'ok', detail: `${service.unit}: enabled, active` });
      continue;
    }
    if (component === 'collector' && awaitingConnection) {
      add({
        id: `service:${component}`,
        label,
        status: 'warn',
        detail: 'Awaiting connection',
        action: 'Run rigscope connect <hub-url> with a private one-time grant.',
      });
      continue;
    }
    add({
      id: `service:${component}`,
      label,
      status: 'warn',
      detail: `${service.unit}: enabled, ${service.subState}${service.error ? ` (${service.error})` : ''}`,
      action: `Run rigscope start ${component} (or check systemctl --user status ${service.unit}).`,
    });
  }
  const hubActive = services.hub.active;
  const collectorActive = services.collector.active;

  if (hubEnabled || hubActive) {
    const path = databasePath(dir);
    if (!existsSync(path)) {
      add({
        id: 'database',
        label: 'Database',
        status: hubActive ? 'fail' : 'warn',
        detail: `not created yet: ${path}`,
        action: hubActive ? 'Inspect journalctl --user -u ' + services.hub.unit + '.' : 'Start once to create the private database.',
      });
    } else {
      try {
        const info = runtime
          ? deps.inspectRuntime(runtime, path)
          : deps.inspectDatabase(path);
        const details = `schema ${info.schemaVersion}/${info.supportedSchemaVersion}; integrity ${info.integrityOk ? 'ok' : 'failed'}`;
        add({
          id: 'database',
          label: 'Database',
          status: !info.integrityOk ? 'fail' : info.newerSchema || info.ledgerProblem !== null ? 'fail' : 'ok',
          detail: info.ledgerProblem === null ? details : `${details}; ${info.ledgerProblem}`,
          ...(info.integrityOk && !info.newerSchema && info.ledgerProblem === null
            ? {}
            : { action: 'Stop the hub and restore the matching backup; never start an old runtime against a newer schema.' }),
        });
      } catch (error) {
        add({
          id: 'database',
          label: 'Database',
          status: 'fail',
          detail: errorText(error),
          action: 'Stop the hub, inspect the file and restore a verified backup before restarting.',
        });
      }
    }
    const space = deps.diskFree(join(dir, 'hub'));
    if (space !== null) {
      const free = space.free;
      add({
        id: 'disk',
        label: 'Disk space',
        status: free < 128 * 1024 * 1024 ? 'fail' : free < 512 * 1024 * 1024 ? 'warn' : 'ok',
        detail: `${formatBytes(free)} free of ${formatBytes(space.total)}`,
        ...(free < 512 * 1024 * 1024
          ? { action: 'Free space or move backups; a full database blocks ingestion and backups.' }
          : {}),
      });
    }
  }

  if (hubEnabled || hubActive) {
    if (config.origin) {
      const socket = config.socket ?? join(dir, 'run', 'hub.sock');
      try {
        const stat = lstatSync(socket);
        add({
          id: 'socket',
          label: 'Unix socket',
          status: stat.isSocket() ? 'ok' : 'fail',
          detail: stat.isSocket() ? socket : `${socket} is not a socket`,
          ...(stat.isSocket() ? {} : { action: 'Stop the hub and inspect the private run directory.' }),
        });
      } catch (error) {
        add({
          id: 'socket',
          label: 'Unix socket',
          status: hubActive ? 'fail' : 'warn',
          detail: `${socket}: ${errorText(error)}`,
          action: 'Start the hub; if it stays missing, inspect journalctl --user -u ' + services.hub.unit + '.',
        });
      }
      try {
        const origin = new URL(config.origin);
        const probe = await deps.probeTls(origin.hostname, origin.port === '' ? 443 : Number(origin.port), 3_000);
        add({
          id: 'tls',
          label: 'Public TLS',
          status: probe.ok ? 'ok' : 'warn',
          detail: probe.ok ? `${origin.host} certificate validated in ${probe.ms}ms` : `${origin.host}: ${probe.error}`,
          ...(probe.ok ? {} : { action: 'Check DNS, the TLS proxy certificate and its forwarding to the private socket.' }),
        });
      } catch (error) {
        add({ id: 'tls', label: 'Public TLS', status: 'warn', detail: errorText(error) });
      }
    } else {
      const probe = await deps.probeTcp('127.0.0.1', config.port, 1_500);
      add({
        id: 'port',
        label: 'Hub port',
        status: hubActive ? (probe.ok ? 'ok' : 'fail') : probe.ok ? 'warn' : 'ok',
        detail: hubActive
          ? `127.0.0.1:${config.port} ${probe.ok ? 'accepts connections' : `is not listening (${probe.error})`}`
          : probe.ok
            ? `127.0.0.1:${config.port} is already in use while the service is inactive`
            : `127.0.0.1:${config.port} is available`,
        action: hubActive && !probe.ok ? 'Inspect journalctl --user -u ' + services.hub.unit + ' and the configured port.' : undefined,
      });
    }
    if (hubActive) {
      try {
        const target = healthTarget({ component: 'hub', port: config.port, origin: config.origin, socket: config.socket ?? join(dir, 'run', 'hub.sock') });
        const health = await deps.probeHealth(target, 3_000);
        const expected = runtime ? runtimeVersion(runtime) : null;
        const reported = typeof health.body?.version === 'string' ? health.body.version : null;
        const versionMatches = expected === null || reported === expected;
        add({
          id: 'health',
          label: 'Hub health',
          status: health.ok && versionMatches ? 'ok' : 'fail',
          detail: health.ok
            ? `HTTP 200${reported === null ? '' : `; version ${reported}`}${versionMatches ? '' : `; expected ${expected}`}`
            : health.error ?? 'no healthy response',
          ...(health.ok && versionMatches ? {} : { action: 'Inspect journalctl --user -u ' + services.hub.unit + ' and the database check.' }),
        });
      } catch (error) {
        add({ id: 'health', label: 'Hub health', status: 'fail', detail: errorText(error) });
      }
    }
  }

  if (collectorEnabled || collectorActive) {
    const statePath = join(dir, 'collector', 'state.json');
    try {
      const raw = readPrivate(statePath);
      const state = JSON.parse(raw) as { collectorId?: string; destinations?: unknown[] };
      if (typeof state.collectorId !== 'string' || !Array.isArray(state.destinations)) {
        throw new Error('collector state is invalid');
      }
      add({
        id: 'collector',
        label: 'Collector',
        status: state.destinations.length === 0 ? 'warn' : 'ok',
        detail: state.destinations.length === 0 ? 'Awaiting connection' : `${state.destinations.length}/8 destination(s)`,
        ...(state.destinations.length === 0 ? { action: 'Run rigscope connect <hub-url> and paste a one-time grant.' } : {}),
      });
      const seen = new Set<string>();
      for (const item of state.destinations) {
        const destination = item as { url?: unknown };
        if (typeof destination.url !== 'string') continue;
        let url: URL;
        try {
          url = new URL(destination.url);
        } catch {
          add({ id: 'destination', label: 'Destination', status: 'fail', detail: 'invalid destination URL' });
          continue;
        }
        if (seen.has(url.host)) continue;
        seen.add(url.host);
        const probe =
          url.protocol === 'wss:'
            ? await deps.probeTls(url.hostname, url.port === '' ? 443 : Number(url.port), 3_000)
            : await deps.probeTcp(url.hostname, url.port === '' ? 80 : Number(url.port), 3_000);
        add({
          id: `destination:${url.host}`,
          label: 'Destination',
          status: probe.ok ? 'ok' : 'warn',
          detail: `${url.host} ${probe.ok ? `reachable in ${probe.ms}ms` : `unreachable: ${probe.error}`}`,
          ...(probe.ok ? {} : { action: 'Check the hub, DNS, proxy certificate and firewall; the collector keeps retrying.' }),
        });
      }
    } catch (error) {
      const missing = (error as NodeJS.ErrnoException).code === 'ENOENT';
      add({
        id: 'collector',
        label: 'Collector',
        status: missing ? 'warn' : 'fail',
        detail: missing ? 'Awaiting connection' : errorText(error),
        action: missing ? 'Run rigscope connect <hub-url> with a private one-time grant.' : 'Restore the private collector state backup.',
      });
    }
    const required = ['/proc/stat', '/proc/meminfo', '/proc/self/stat'];
    const readable = deps.sensorReadable(required);
    add({
      id: 'sensors',
      label: 'Sensors',
      status: readable.length === required.length ? 'ok' : 'fail',
      detail: readable.length === required.length ? 'procfs readable' : `unreadable: ${required.filter((path) => !readable.includes(path)).join(', ')}`,
      ...(readable.length === required.length ? {} : { action: 'Run the collector as the service user with read access to /proc.' }),
    });
    const powerPaths = powerCounterPaths();
    if (powerPaths.length === 0) {
      add({ id: 'power', label: 'CPU package power', status: 'skip', detail: 'no readable or detected RAPL counter' });
    } else {
      const readablePower = deps.sensorReadable(powerPaths);
      add({
        id: 'power',
        label: 'CPU package power',
        status: readablePower.length > 0 ? 'ok' : 'warn',
        detail: readablePower.length > 0 ? `${readablePower.length} RAPL counter(s) readable` : 'RAPL counters exist but this account cannot read them',
        ...(readablePower.length > 0 ? {} : { action: 'Use the system installation or grant the collector read-only access to the specific energy counters.' }),
      });
    }
  }

  const version = runtime === null ? null : runtimeVersion(runtime);
  return finish(dir, config.role, version, checks);
}

function hubEnvironment(dir: string, config: Setup): Record<string, string> {
  return {
    ...(!config.origin ? { HUB_HOST: '127.0.0.1', HUB_PORT: String(config.port) } : {}),
    HUB_STATE_FILE: join(dir, 'hub', 'enrollment.json'),
    HUB_DATABASE_PATH: databasePath(dir),
    HUB_BROWSER_USERS: JSON.stringify(config.users),
    HUB_COLLECTORS: JSON.stringify(config.collectors),
    ...(config.maxCollectorConnections === undefined ? {} : { HUB_MAX_COLLECTOR_CONNECTIONS: String(config.maxCollectorConnections) }),
    ...(config.origin ? { HUB_PUBLIC_ORIGIN: config.origin, HUB_SOCKET_PATH: config.socket ?? join(dir, 'run', 'hub.sock') } : {}),
  };
}

function collectorAwaiting(dir: string): boolean {
  try {
    const state = JSON.parse(readPrivate(join(dir, 'collector', 'state.json'))) as { destinations?: unknown[] };
    return !Array.isArray(state.destinations) || state.destinations.length === 0;
  } catch (error) {
    return (error as NodeJS.ErrnoException).code === 'ENOENT';
  }
}

function powerCounterPaths(): string[] {
  const paths: string[] = [];
  const visit = (directory: string, depth: number): void => {
    if (depth > 3 || paths.length >= 64) return;
    let entries: Dirent[];
    try {
      entries = readdirSync(directory, { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of entries) {
      if (paths.length >= 64) return;
      const path = join(directory, entry.name);
      if (entry.name === 'energy_uj') {
        paths.push(path);
        continue;
      }
      if (entry.isDirectory() || entry.isSymbolicLink()) visit(path, depth + 1);
    }
  };
  for (const root of ['/sys/class/powercap', '/sys/devices/virtual/powercap']) visit(root, 0);
  return paths;
}

function finish(dir: string, role: Setup['role'] | null, version: string | null, checks: DoctorCheck[]): DoctorReport {
  return {
    ok: checks.every((check) => check.status !== 'fail'),
    stateDir: dir,
    role,
    version,
    checks,
  };
}

export function formatDoctorReport(report: DoctorReport): string {
  const lines = [`Rigscope doctor: ${report.ok ? 'healthy' : 'attention needed'}`, `State: ${report.stateDir}`];
  if (report.role) lines.push(`Role: ${report.role}${report.version ? `; runtime ${report.version}` : ''}`);
  for (const check of report.checks) {
    lines.push(`${check.status.toUpperCase().padEnd(5)} ${check.label}: ${check.detail}`);
    if (check.action) lines.push(`      ${check.action}`);
  }
  return lines.join('\n');
}

export function formatBytes(bytes: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let value = bytes;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return `${value >= 10 || index === 0 ? Math.round(value) : value.toFixed(1)} ${units[index]}`;
}

function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
