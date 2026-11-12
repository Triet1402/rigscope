/**
 * Staged CLI upgrades and supported rollback.
 *
 * The order is deliberate:
 *   1. stage and inspect the new package without touching the active runtime;
 *   2. snapshot and stop active components, then hold all runtime leases;
 *   3. copy the database and record which runtime that copy belongs to;
 *   4. with components stopped and leases held, run the one-time machine-data
 *      transition (see ./machine-transition) if it has not already completed;
 *   5. activate, start, then verify the expected version, database schema and
 *      previously active components within one bounded deadline, and only then
 *      mark the transition complete;
 *   6. on failure restore the previous runtime, undo the transition — rename
 *      the staged collector queue back and restore the pre-upgrade database
 *      copy even when the previous runtime could still read the schema,
 *      because the transition removed rows, not schema — and when the old
 *      runtime cannot read the new schema anyway, restore the copy there too.
 *
 * A successful systemctl call or a generic HTTP 200 is never the final check.
 * The database copy replaces a schema-incompatible database only through the
 * guarded restore path, which itself preserves the replaced copy.
 */

import { existsSync, renameSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { backupDatabase, clearMachineHistory, inspectDatabase, restoreDatabase } from '../operations/database';
import { OperationsError, assertNoActiveHubLock, ensurePrivateDir, errorMessage, timestampSlug } from '../operations/fs';
import {
  completeMachineDataCleanup,
  performMachineDataCleanup,
  undoMachineDataCleanup,
  type MachineDataCleanup,
  type MachineDataPaths,
  type MachineDataReport,
} from './machine-transition';
import { acquireProcessLease } from '../runtime/process-lease';
import { componentEnabled, enabledComponents, readPrivate, readSetup, type Component, type Setup } from './config';
import { healthTarget, waitForHealth, type HealthTarget, type WaitForHealthResult } from './health';
import {
  activatePackage,
  serviceStatus,
  stagePackage,
  startupStatus,
  swapRuntimes,
  systemctl,
  type ServiceStatus,
} from './services';
import {
  assertRuntimeArtifacts,
  databasePath,
  emptyRuntimeState,
  inspectDatabaseWithRuntime,
  readRuntimeState,
  runtimeVersion,
  writeRuntimeState,
  type RuntimeBackupRecord,
  type RuntimeInspect,
  type RuntimeState,
} from './release-state';

export type ServiceController = {
  status(dir: string, component?: Component): ServiceStatus;
  stop(dir: string, component?: Component): void;
  start(dir: string, component?: Component): void;
  startup?(dir: string, component?: Component): boolean;
  setStartup?(dir: string, enabled: boolean, component?: Component): void;
};

export const defaultServiceController: ServiceController = {
  status: serviceStatus,
  stop: (dir, component) => systemctl(dir, 'stop', false, component),
  start: (dir, component) => systemctl(dir, 'start', false, component),
  startup: (dir, component) => startupStatus(dir, component).enabled,
  setStartup: (dir, enabled, component) => systemctl(dir, enabled ? 'enable' : 'disable', false, component),
};

export type UpgradeDependencies = {
  service?: ServiceController;
  stage?: typeof stagePackage;
  backup?: typeof backupDatabase;
  restore?: typeof restoreDatabase;
  inspect?: typeof inspectDatabase;
  inspectRuntime?: typeof inspectDatabaseWithRuntime;
  clearHistory?: typeof clearMachineHistory;
  waitHealth?: typeof waitForHealth;
  activate?: typeof activatePackage;
  swap?: typeof swapRuntimes;
  acquireLease?: typeof acquireProcessLease;
  now?: () => number;
};

export type Verification = {
  version: string;
  health: WaitForHealthResult | null;
  database: RuntimeInspect | null;
};

export type UpgradeReport = {
  version: string;
  previousVersion: string;
  backupPath: string | null;
  schemaVersion: number | null;
  serviceRestarted: boolean;
  verification: Verification;
  /**
   * Result of the one-time machine-data transition. Only the first upgrade
   * after the RAM-first release performs it; later upgrades report
   * `transitioned: false` and preserve durable history and queues. Accounts,
   * enrollment, settings, alert rules, machine names/tags and recordings are
   * always preserved. A failed activation restores the cleared rows and the
   * staged queue, so this describes a completed transition.
   */
  machineHistory: MachineDataReport;
};

export type RollbackReport = {
  version: string;
  previousVersion: string;
  restoredBackup: string | null;
  preRestoreBackup: string | null;
  serviceRestarted: boolean;
  verification: Verification;
};

const DEFAULT_HEALTH_DEADLINE_MS = 25_000;

function dependencies(deps: UpgradeDependencies = {}) {
  return {
    service: deps.service ?? defaultServiceController,
    stage: deps.stage ?? stagePackage,
    backup: deps.backup ?? backupDatabase,
    restore: deps.restore ?? restoreDatabase,
    inspect: deps.inspect ?? inspectDatabase,
    inspectRuntime: deps.inspectRuntime ?? inspectDatabaseWithRuntime,
    clearHistory: deps.clearHistory ?? clearMachineHistory,
    waitHealth: deps.waitHealth ?? waitForHealth,
    activate: deps.activate ?? activatePackage,
    swap: deps.swap ?? swapRuntimes,
    acquireLease: deps.acquireLease ?? acquireProcessLease,
    now: deps.now ?? Date.now,
  };
}

function acquireInstallLocks(dir: string, acquireLease: typeof acquireProcessLease): () => void {
  const releases: Array<() => void> = [];
  try {
    // Hold every supervisor lease so no process can start during the shared
    // runtime and database switch.
    releases.push(acquireLease(join(dir, 'run', 'hub.lock')));
    releases.push(acquireLease(join(dir, 'run', 'collector.lock')));
    releases.push(acquireLease(join(dir, 'run', 'rigscope.lock')));
    releases.push(acquireLease(join(dir, 'collector', 'collector-runtime.lock')));
  } catch (error) {
    for (const release of releases.reverse()) {
      try {
        release();
      } catch {
        // Preserve the acquisition failure.
      }
    }
    throw new OperationsError(
      'runtime-active',
      `another runtime still owns this installation: ${errorMessage(error)}; stop it before upgrading`,
      { cause: error },
    );
  }
  let released = false;
  return () => {
    if (released) return;
    released = true;
    for (const release of releases.reverse()) {
      try {
        release();
      } catch {
        // Locks are advisory; release every lease even if cleanup fails.
      }
    }
  };
}

const COMPONENT_LIST: readonly Component[] = ['hub', 'collector'];

type ComponentSnapshot = {
  running: Component[];
  enabled: Partial<Record<Component, boolean>>;
  statuses: Partial<Record<Component, ServiceStatus>>;
};

/** Actual activity, not just enabled configuration, for both components. */
function snapshotComponents(dir: string, deps: ReturnType<typeof dependencies>): ComponentSnapshot {
  const running: Component[] = [];
  const enabled: Partial<Record<Component, boolean>> = {};
  const statuses: Partial<Record<Component, ServiceStatus>> = {};
  for (const component of COMPONENT_LIST) {
    const status = deps.service.status(dir, component);
    statuses[component] = status;
    if (status.active) running.push(component);
    const flag = typeof status.enabled === 'boolean' ? status.enabled : deps.service.startup?.(dir, component);
    if (typeof flag === 'boolean') enabled[component] = flag;
  }
  return { running, enabled, statuses };
}

function restoreStartup(
  dir: string,
  deps: ReturnType<typeof dependencies>,
  enabled: Partial<Record<Component, boolean>>,
  tolerateFailure: boolean,
): string[] {
  if (!deps.service.setStartup) return [];
  const problems: string[] = [];
  for (const component of COMPONENT_LIST) {
    const flag = enabled[component];
    if (typeof flag !== 'boolean') continue;
    try { deps.service.setStartup(dir, flag, component); }
    catch (error) {
      if (!tolerateFailure) throw error;
      problems.push(errorMessage(error));
    }
  }
  return problems;
}

function stopComponents(
  dir: string,
  deps: ReturnType<typeof dependencies>,
  components: readonly Component[],
): void {
  for (const component of components) deps.service.stop(dir, component);
}

function assertComponentsStopped(
  dir: string,
  deps: ReturnType<typeof dependencies>,
  components: readonly Component[],
): void {
  for (const component of components) {
    const status = deps.service.status(dir, component);
    if (status.active) {
      throw new OperationsError('service-active', `service ${status.unit} is still active after stop`);
    }
  }
}

/** Where the user installation keeps the data the one-time transition targets. */
function machineDataPaths(dir: string): MachineDataPaths {
  return { database: databasePath(dir), queueTree: join(dir, 'collector', 'outbox') };
}

/** Read one readiness file, naming the missing path so a collector that never
 * started is not reported as an unexplained ENOENT. */
function readReadinessFile(path: string, label: string): string {
  try {
    return readPrivate(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') {
      throw new Error(`${label} is missing (${path}); the collector is not running`);
    }
    throw error;
  }
}

function collectorStateCheck(dir: string): void {
  const path = join(dir, 'collector', 'state.json');
  let raw: string;
  try {
    raw = readPrivate(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return;
    throw new OperationsError('collector-state-invalid', `collector state is not readable/private: ${path}`, { cause: error });
  }
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch (error) {
    throw new OperationsError('collector-state-invalid', `collector state is not valid JSON: ${path}`, { cause: error });
  }
  const record = value as Record<string, unknown>;
  if (
    record === null ||
    typeof record !== 'object' ||
    typeof record.collectorId !== 'string' ||
    !Array.isArray(record.destinations) ||
    record.destinations.length > 8
  ) {
    throw new OperationsError('collector-state-invalid', `collector state is invalid: ${path}`);
  }
}

function hubTarget(config: Setup, dir: string): HealthTarget {
  return healthTarget({
    role: 'hub',
    port: config.port,
    origin: config.origin,
    socket: config.socket ?? join(dir, 'run', 'hub.sock'),
  });
}

export async function verifyRunningInstallation(options: {
  dir: string;
  config: Setup;
  expectedVersion: string;
  expectedSchema?: number;
  /** Rollback may restore an older backup that the previous runtime migrates on start. */
  allowOlderSchema?: boolean;
  deadlineMs?: number;
  requireServiceActive?: boolean;
  /** Verify only these components. Defaults to the enabled components. */
  components?: Component[];
  /** Components that must be active. Defaults to every component in `components`. */
  requireActive?: Component[];
  deps?: UpgradeDependencies;
}): Promise<Verification> {
  const d = dependencies(options.deps);
  const deadline = Date.now() + (options.deadlineMs ?? DEFAULT_HEALTH_DEADLINE_MS);
  const runtime = join(options.dir, 'runtime');
  const version = runtimeVersion(runtime);
  if (version !== options.expectedVersion) {
    throw new OperationsError(
      'version-mismatch',
      `active runtime reports ${version}, expected ${options.expectedVersion}`,
    );
  }
  assertRuntimeArtifacts(runtime, 'active runtime');
  const components = options.components ?? enabledComponents(options.config);
  const required = options.requireActive
    ?? (options.requireServiceActive === false ? [] : components);
  const statuses = new Map<Component, ServiceStatus>();
  for (const component of new Set([...components, ...required])) {
    statuses.set(component, d.service.status(options.dir, component));
  }
  for (const component of required) {
    const status = statuses.get(component)!;
    if (!status.active) {
      throw new OperationsError(
        'service-inactive',
        `service ${status.unit} is not active after activation (${status.subState})`,
      );
    }
  }
  let health: WaitForHealthResult | null = null;
  let database: RuntimeInspect | null = null;
  if (components.includes('hub')) {
    const status = statuses.get('hub')!;
    if (status.active) {
      health = await d.waitHealth(hubTarget(options.config, options.dir), {
        deadlineMs: Math.max(100, deadline - Date.now()),
        expectedVersion: options.expectedVersion,
      });
      if (!health.ok) {
        throw new OperationsError('health-failed',
          `hub did not become healthy within the activation deadline: ${health.last.error ?? 'no healthy response'}`);
      }
    }
    const path = databasePath(options.dir);
    if (!existsSync(path)) {
      if (status.active) {
        throw new OperationsError('database-missing', `hub database is missing after activation: ${path}`);
      }
    } else {
      database = d.inspectRuntime(runtime, path, {timeoutMs: Math.max(100, deadline - Date.now())});
      if (!database.integrityOk) {
        throw new OperationsError('database-integrity', `database integrity check failed after activation: ${path}`);
      }
      if (database.newerSchema) {
        throw new OperationsError(
          'newer-schema',
          `active runtime supports schema ${database.supportedSchemaVersion}, but the database is schema ${database.schemaVersion}`,
        );
      }
      if (database.ledgerProblem !== null) {
        throw new OperationsError('invalid-database', `database migration history is unusable: ${database.ledgerProblem}`);
      }
      const expected = options.expectedSchema ?? database.supportedSchemaVersion;
      if (status.active && database.schemaVersion !== expected && options.allowOlderSchema !== true) {
        throw new OperationsError(
          'schema-mismatch',
          `database schema ${database.schemaVersion} does not match expected schema ${expected} after activation`,
        );
      }
      if (options.allowOlderSchema === true && database.schemaVersion > database.supportedSchemaVersion) {
        throw new OperationsError(
          'newer-schema',
          `database schema ${database.schemaVersion} is newer than rolled-back runtime support ${database.supportedSchemaVersion}`,
        );
      }
    }
  }
  if (components.includes('collector')) {
    const status = statuses.get('collector')!;
    collectorStateCheck(options.dir);
    if (status.active) {
      let problem = 'collector has not sampled';
      const healthPath = join(options.dir, 'run', 'collector-health.json');
      const lockPath = join(options.dir, 'collector', 'collector-runtime.lock');
      while (Date.now() < deadline) {
        try {
          const marker = JSON.parse(readReadinessFile(healthPath, 'collector readiness marker'));
          const owner = JSON.parse(readReadinessFile(lockPath, 'collector runtime lock'));
          if (marker.version !== options.expectedVersion) throw new Error('collector version does not match the activated release');
          if (marker.pid !== owner.pid || !Number.isSafeInteger(marker.pid)) throw new Error('collector readiness belongs to another process');
          process.kill(marker.pid, 0);
          if (!Number.isFinite(marker.sampledAt) || Date.now() - marker.sampledAt > 15_000) throw new Error('collector sampling is stale');
          problem = '';
          break;
        } catch (error) { problem = errorMessage(error); }
        await new Promise(resolve => setTimeout(resolve, 100));
      }
      if (problem) throw new OperationsError('collector-unhealthy', `collector did not become ready: ${problem}`);
    }
  }
  return { version, health, database };
}

async function recoverActivation(options: {
  dir: string;
  config: Setup;
  previousVersion: string;
  backupPath: string | null;
  machineData: MachineDataCleanup | null;
  running: Component[];
  startup: Partial<Record<Component, boolean>>;
  activated: boolean;
  deps: ReturnType<typeof dependencies>;
}): Promise<string> {
  const runtime = join(options.dir, 'runtime');
  const messages: string[] = [];
  let releaseLocks: (() => void) | null = null;
  try {
    // Stop every component before touching a shared runtime or database.
    for (const component of COMPONENT_LIST) {
      const status = options.deps.service.status(options.dir, component);
      if (status.active) options.deps.service.stop(options.dir, component);
    }
    assertComponentsStopped(options.dir, options.deps, COMPONENT_LIST);
    if (existsSync(databasePath(options.dir))) assertNoActiveHubLock(databasePath(options.dir));
    releaseLocks = acquireInstallLocks(options.dir, options.deps.acquireLease);
    // Undo the one-time transition while its effects are still reversible:
    // the staged queue tree renames back, and the cleared rows come back with
    // the pre-upgrade database copy below. A pending cleanup warning travels
    // into the failure message so the operator sees it even here.
    messages.push(...undoMachineDataCleanup(machineDataPaths(options.dir), options.machineData?.stagedQueues ?? null));
    if (options.machineData?.report.warning) {
      messages.push(`machine-data cleanup was incomplete: ${options.machineData.report.warning}`);
    }
    if (options.activated) {
      try {
        if (existsSync(runtime)) options.deps.swap(options.dir);
        const restoredVersion = runtimeVersion(join(options.dir, 'runtime'));
        if (restoredVersion !== options.previousVersion) {
          throw new Error(`restored runtime reports ${restoredVersion}, expected ${options.previousVersion}`);
        }
        messages.push(`restored runtime ${options.previousVersion}`);
      } catch (error) {
        messages.push(`runtime recovery failed: ${errorMessage(error)}`);
        return messages.join('; ');
      }
    }
    const path = databasePath(options.dir);
    // The transition cleared rows from this database. Its pre-upgrade copy is
    // the only source for them, so restore it even when the previous runtime
    // could still read the schema: the loss here is the rows, not the schema.
    const transitionClearedHistory = options.machineData?.databaseCleared === true;
    if (options.backupPath !== null && existsSync(options.backupPath) && existsSync(path)) {
      try {
        const old = options.deps.inspectRuntime(join(options.dir, 'runtime'), path);
        if (transitionClearedHistory || !old.integrityOk || old.newerSchema || old.ledgerProblem !== null) {
          const restored = await options.deps.restore({
            input: options.backupPath,
            database: path,
            replace: true,
            confirmStopped: true,
          });
          messages.push(
            `restored pre-upgrade database ${options.backupPath}; the failed database copy is ${restored.preRestoreBackup ?? 'not available'}`,
          );
        } else {
          messages.push('kept the current database because the previous runtime can still read its schema');
        }
      } catch (error) {
        messages.push(`database recovery failed: ${errorMessage(error)}; pre-upgrade copy is ${options.backupPath}`);
        return messages.join('; ');
      }
    } else if (options.backupPath === null && existsSync(path)) {
      // The upgrade created this database during the failed activation. There is
      // no pre-upgrade copy, but the old runtime may not understand the new
      // schema. Preserve the file instead of deleting it, then let the previous
      // runtime create its own fresh database.
      try {
        const old = options.deps.inspectRuntime(join(options.dir, 'runtime'), path);
        if (!old.integrityOk || old.newerSchema || old.ledgerProblem !== null) {
          const backups = join(options.dir, 'backups');
          ensurePrivateDir(backups);
          const preserved = join(backups, `failed-activation-${timestampSlug(options.deps.now())}.sqlite`);
          renameSync(path, preserved);
          for (const sidecar of [`${path}-wal`, `${path}-shm`]) {
            if (existsSync(sidecar)) renameSync(sidecar, `${preserved}${sidecar.slice(path.length)}`);
          }
          messages.push(`preserved the failed activation database at ${preserved}; the previous runtime will create a fresh database`);
        } else {
          messages.push('kept the database created during activation because the previous runtime can read it');
        }
      } catch (error) {
        messages.push(`database recovery failed: ${errorMessage(error)}`);
        return messages.join('; ');
      }
    }
    releaseLocks();
    releaseLocks = null;
    for (const problem of restoreStartup(options.dir, options.deps, options.startup, true)) {
      messages.push(`startup flag recovery failed: ${problem}`);
    }
    if (options.running.length > 0) {
      try {
        for (const component of options.running) options.deps.service.start(options.dir, component);
        await verifyRunningInstallation({dir: options.dir, config: options.config,
          expectedVersion: options.previousVersion, allowOlderSchema: true,
          components: options.running, requireActive: options.running, deps: options.deps});
        messages.push('verified the previous service healthy');
      } catch (error) {
        messages.push(`previous service did not restart: ${errorMessage(error)}`);
      }
    }
    return messages.join('; ');
  } catch (error) {
    return `${messages.join('; ')}; recovery refused: ${errorMessage(error)}`;
  } finally {
    releaseLocks?.();
  }
}

export async function upgradeInstallation(
  options: { root: string; dir: string; from?: string; healthDeadlineMs?: number },
  injected: UpgradeDependencies = {},
): Promise<UpgradeReport> {
  const deps = dependencies(injected);
  const config = readSetup(options.dir);
  if (!config) throw new OperationsError('not-installed', 'No installation found. Run rigscope install first.');
  const runtime = join(options.dir, 'runtime');
  if (!existsSync(runtime)) {
    throw new OperationsError('not-installed', 'Install a managed runtime before upgrading.');
  }
  const currentVersion = runtimeVersion(runtime);
  assertRuntimeArtifacts(runtime, 'active runtime');
  const state = readRuntimeState(options.dir) ?? emptyRuntimeState(options.dir);

  const stage = deps.stage(options.root, options.dir, options.from);
  let expectedVersion: string;
  let expectedSchema: number | null = null;
  try {
    expectedVersion = runtimeVersion(stage);
    assertRuntimeArtifacts(stage, 'staged runtime');
    const path = databasePath(options.dir);
    if (existsSync(path)) {
      const staged = deps.inspectRuntime(stage, path);
      if (!staged.integrityOk) {
        throw new OperationsError('database-integrity', `database integrity check failed before upgrade: ${path}`);
      }
      if (staged.ledgerProblem !== null) {
        throw new OperationsError('invalid-database', `database migration history is unusable: ${staged.ledgerProblem}`);
      }
      if (staged.newerSchema) {
        throw new OperationsError(
          'newer-schema',
          `database schema ${staged.schemaVersion} is newer than staged runtime ${expectedVersion} supports (${staged.supportedSchemaVersion}); use the release that created it`,
        );
      }
      expectedSchema = staged.supportedSchemaVersion;
    }
  } catch (error) {
    rmSync(stage, { recursive: true, force: true });
    throw error;
  }

  const snapshot = snapshotComponents(options.dir, deps);
  const running = snapshot.running;
  let releaseLocks: (() => void) | null = null;
  const releaseOperation = deps.acquireLease(join(options.dir, 'run', 'operation.lock'));
  let backupPath: string | null = null;
  let activated = false;
  let machineData: MachineDataCleanup | null = null;
  try {
    stopComponents(options.dir, deps, running);
    assertComponentsStopped(options.dir, deps, running);
    // Check the database owner's lease before taking the component leases.
    if (running.includes('hub') || componentEnabled(config, 'hub') || existsSync(databasePath(options.dir))) {
      assertNoActiveHubLock(databasePath(options.dir));
    }
    releaseLocks = acquireInstallLocks(options.dir, deps.acquireLease);
    if (existsSync(databasePath(options.dir))) {
      const backups = join(options.dir, 'backups');
      ensurePrivateDir(backups);
      backupPath = join(
        backups,
        `pre-upgrade-${currentVersion}-to-${expectedVersion}-${timestampSlug(deps.now())}.sqlite`,
      );
      const backup = await deps.backup({ database: databasePath(options.dir), output: backupPath, now: deps.now });
      expectedSchema = expectedSchema ?? backup.sourceSchemaVersion;
      // Persist the pairing before activation, including for process death
      // between switching code and accepting its health.
      state.backups[expectedVersion] = {path: backupPath, fromVersion: currentVersion,
        toVersion: expectedVersion, schemaVersion: backup.sourceSchemaVersion,
        fromSchema: backup.sourceSchemaVersion, toSchema: expectedSchema,
        createdAt: new Date(deps.now()).toISOString()};
      writeRuntimeState(options.dir, {...state, currentVersion, previousVersion: state.previousVersion,
        lastActivation: {version: expectedVersion, previousVersion: currentVersion,
          backupPath, activatedAt: new Date(deps.now()).toISOString()}});
    }
    deps.activate(options.dir, stage);
    activated = true;
    // The one-time machine-data transition runs with the old components stopped
    // and every lease still held: clearing the hub database is only safe while
    // no hub owns it, and a failed activation must still be able to restore.
    // The transition is reversible here — the queue tree is only renamed aside
    // and the pre-upgrade database copy above can restore the cleared rows.
    machineData = performMachineDataCleanup(options.dir, machineDataPaths(options.dir), deps.now, {
      clear: deps.clearHistory,
    });
    releaseLocks();
    releaseLocks = null;
    for (const component of running) deps.service.start(options.dir, component);
    const verification = await verifyRunningInstallation({
      dir: options.dir,
      config,
      expectedVersion,
      expectedSchema: expectedSchema ?? undefined,
      deadlineMs: options.healthDeadlineMs,
      components: running,
      requireActive: running,
      deps,
    });
    restoreStartup(options.dir, deps, snapshot.enabled, false);

    const now = new Date(deps.now()).toISOString();
    if (backupPath !== null && existsSync(backupPath)) {
      const info = deps.inspect(backupPath);
      const record: RuntimeBackupRecord = {
        path: backupPath,
        fromVersion: currentVersion,
        toVersion: expectedVersion,
        schemaVersion: info.schemaVersion,
        fromSchema: info.schemaVersion,
        toSchema: verification.database?.schemaVersion ?? expectedSchema ?? info.schemaVersion,
        createdAt: now,
      };
      state.backups[expectedVersion] = record;
    }
    writeRuntimeState(options.dir, {
      ...state,
      currentVersion: expectedVersion,
      previousVersion: currentVersion,
      lastActivation: {
        version: expectedVersion,
        previousVersion: currentVersion,
        backupPath,
        activatedAt: now,
      },
    });
    // Healthy and durable: the transition may now be recorded as complete.
    // An incomplete cleanup leaves the marker unwritten so the next upgrade
    // retries it, and keeps the staged queue copy for inspection.
    completeMachineDataCleanup(options.dir, machineData, expectedVersion);
    return {
      version: expectedVersion,
      previousVersion: currentVersion,
      backupPath,
      schemaVersion: verification.database?.schemaVersion ?? expectedSchema,
      serviceRestarted: running.length > 0,
      verification,
      machineHistory: machineData.report,
    };
  } catch (error) {
    releaseLocks?.();
    releaseLocks = null;
    const recovery = await recoverActivation({
      dir: options.dir,
      config,
      previousVersion: currentVersion,
      backupPath,
      machineData,
      running,
      startup: snapshot.enabled,
      activated,
      deps,
    });
    throw new OperationsError(
      'upgrade-failed',
      `upgrade to ${expectedVersion} failed: ${errorMessage(error)}. ${recovery}`,
      { cause: error },
    );
  } finally {
    releaseLocks?.();
    releaseOperation();
    if (existsSync(stage)) rmSync(stage, { recursive: true, force: true });
  }
}

function rollbackBackup(options: {
  currentVersion: string;
  previousVersion: string;
  previousSupport: RuntimeInspect;
  state: RuntimeState;
  explicitBackup?: string;
  deps: ReturnType<typeof dependencies>;
}): string {
  if (options.explicitBackup !== undefined) {
    const path = options.explicitBackup;
    if (!existsSync(path)) {
      throw new OperationsError('backup-missing', `rollback backup does not exist: ${path}`);
    }
    const info = options.deps.inspect(path);
    if (info.schemaVersion > options.previousSupport.supportedSchemaVersion) {
      throw new OperationsError(
        'backup-mismatch',
        `backup schema ${info.schemaVersion} is newer than previous runtime ${options.previousVersion} supports (${options.previousSupport.supportedSchemaVersion})`,
      );
    }
    return path;
  }
  const recorded = options.state.backups[options.currentVersion];
  if (recorded === undefined || recorded.toVersion !== options.currentVersion || recorded.fromVersion !== options.previousVersion) {
    throw new OperationsError(
      'backup-missing',
      `database schema ${options.previousSupport.schemaVersion} is newer than previous runtime ${options.previousVersion} (schema ${options.previousSupport.supportedSchemaVersion}); no paired pre-upgrade backup is recorded for ${options.currentVersion}. Re-run with --backup PATH naming a copy that matches schema ${options.previousSupport.supportedSchemaVersion} or older`,
    );
  }
  if (!existsSync(recorded.path)) {
    throw new OperationsError(
      'backup-missing',
      `paired pre-upgrade backup for ${options.currentVersion} is missing: ${recorded.path}`,
    );
  }
  const info = options.deps.inspect(recorded.path);
  if (info.schemaVersion !== recorded.schemaVersion) {
    throw new OperationsError('backup-mismatch', 'paired rollback backup no longer has its recorded schema');
  }
  if (info.schemaVersion > options.previousSupport.supportedSchemaVersion) {
    throw new OperationsError(
      'backup-mismatch',
      `paired backup schema ${info.schemaVersion} is newer than previous runtime ${options.previousVersion} supports (${options.previousSupport.supportedSchemaVersion})`,
    );
  }
  return recorded.path;
}

export async function rollbackInstallation(
  options: { dir: string; backup?: string; healthDeadlineMs?: number },
  injected: UpgradeDependencies = {},
): Promise<RollbackReport> {
  const deps = dependencies(injected);
  const config = readSetup(options.dir);
  if (!config) throw new OperationsError('not-installed', 'No installation found. Run rigscope install first.');
  const runtime = join(options.dir, 'runtime');
  const previousRuntime = join(options.dir, 'runtime-previous');
  if (!existsSync(runtime)) throw new OperationsError('not-installed', 'No active runtime is installed.');
  if (!existsSync(previousRuntime)) {
    throw new OperationsError('no-previous-release', 'No previous runtime is available for rollback.');
  }
  const currentVersion = runtimeVersion(runtime);
  const previousVersion = runtimeVersion(previousRuntime);
  assertRuntimeArtifacts(runtime, 'active runtime');
  assertRuntimeArtifacts(previousRuntime, 'previous runtime');
  const state = readRuntimeState(options.dir) ?? emptyRuntimeState(options.dir);
  const snapshot = snapshotComponents(options.dir, deps);
  const running = snapshot.running;
  let releaseLocks: (() => void) | null = null;
  const releaseOperation = deps.acquireLease(join(options.dir, 'run', 'operation.lock'));
  let restoredBackup: string | null = null;
  let preRestoreBackup: string | null = null;
  let swapped = false;
  try {
    stopComponents(options.dir, deps, running);
    assertComponentsStopped(options.dir, deps, running);
    if (running.includes('hub') || componentEnabled(config, 'hub') || existsSync(databasePath(options.dir))) {
      assertNoActiveHubLock(databasePath(options.dir));
    }
    releaseLocks = acquireInstallLocks(options.dir, deps.acquireLease);
    const path = databasePath(options.dir);
    if (existsSync(path)) {
      const previousSupport = deps.inspectRuntime(previousRuntime, path);
      if (!previousSupport.integrityOk || previousSupport.newerSchema || previousSupport.ledgerProblem !== null) {
        const backup = rollbackBackup({
          currentVersion,
          previousVersion,
          previousSupport,
          state,
          explicitBackup: options.backup,
          deps,
        });
        const restored = await deps.restore({
          input: backup,
          database: path,
          replace: true,
          confirmStopped: true,
          now: deps.now,
        });
        restoredBackup = backup;
        preRestoreBackup = restored.preRestoreBackup;
      }
    }
    deps.swap(options.dir);
    swapped = true;
    releaseLocks();
    releaseLocks = null;
    for (const component of running) deps.service.start(options.dir, component);
    const verification = await verifyRunningInstallation({
      dir: options.dir,
      config,
      expectedVersion: previousVersion,
      expectedSchema: undefined,
      allowOlderSchema: true,
      deadlineMs: options.healthDeadlineMs,
      components: running,
      requireActive: running,
      deps,
    });
    restoreStartup(options.dir, deps, snapshot.enabled, false);
    const now = new Date(deps.now()).toISOString();
    writeRuntimeState(options.dir, {
      ...state,
      currentVersion: previousVersion,
      previousVersion: currentVersion,
      lastRollback: {
        version: previousVersion,
        previousVersion: currentVersion,
        restoredBackup,
        preRestoreBackup,
        restoredAt: now,
      },
    });
    return {
      version: previousVersion,
      previousVersion: currentVersion,
      restoredBackup,
      preRestoreBackup,
      serviceRestarted: running.length > 0,
      verification,
    };
  } catch (error) {
    const messages: string[] = [];
    let safeToRestart = true;
    // Stop the attempted runtime before moving files or restoring a database.
    // Failure to quiesce must leave every runtime/database copy untouched.
    releaseLocks?.();
    releaseLocks = null;
    try {
      for (const component of COMPONENT_LIST) {
        const status = deps.service.status(options.dir, component);
        if (status.active) deps.service.stop(options.dir, component);
      }
      assertComponentsStopped(options.dir, deps, COMPONENT_LIST);
      if (existsSync(databasePath(options.dir))) assertNoActiveHubLock(databasePath(options.dir));
      releaseLocks = acquireInstallLocks(options.dir, deps.acquireLease);
    } catch (stopError) {
      throw new OperationsError('rollback-failed', `rollback failed: ${errorMessage(error)}. Recovery refused while runtime ownership is uncertain: ${errorMessage(stopError)}`, {cause: error});
    }
    if (swapped) {
      try {
        deps.swap(options.dir);
        messages.push(`restored runtime ${currentVersion}`);
      } catch (swapError) {
        messages.push(`runtime roll-forward failed: ${errorMessage(swapError)}`);
        safeToRestart = false;
      }
    }
    if (preRestoreBackup !== null && existsSync(preRestoreBackup)) {
      try {
        await deps.restore({
          input: preRestoreBackup,
          database: databasePath(options.dir),
          replace: true,
          confirmStopped: true,
          now: deps.now,
        });
        messages.push(`restored the pre-rollback database ${preRestoreBackup}`);
      } catch (restoreError) {
        messages.push(`database roll-forward failed: ${errorMessage(restoreError)}; copy is ${preRestoreBackup}`);
        safeToRestart = false;
      }
    }
    if (safeToRestart && running.includes('hub') && existsSync(databasePath(options.dir))) {
      try {
        const info = deps.inspectRuntime(runtime, databasePath(options.dir));
        if (!info.integrityOk || info.newerSchema || info.ledgerProblem !== null) throw new Error('runtime/database compatibility is not established');
      } catch (compatibilityError) {
        safeToRestart = false;
        messages.push(`service kept stopped: ${errorMessage(compatibilityError)}`);
      }
    }
    releaseLocks();
    releaseLocks = null;
    if (safeToRestart) {
      for (const problem of restoreStartup(options.dir, deps, snapshot.enabled, true)) {
        messages.push(`startup flag recovery failed: ${problem}`);
      }
    }
    if (running.length > 0 && safeToRestart) {
      try {
        for (const component of running) deps.service.start(options.dir, component);
        await verifyRunningInstallation({dir: options.dir, config, expectedVersion: currentVersion,
          allowOlderSchema: true, deadlineMs: options.healthDeadlineMs,
          components: running, requireActive: running, deps});
        messages.push('verified the prior service healthy');
      } catch (startError) {
        messages.push(`service did not restart: ${errorMessage(startError)}`);
      }
    }
    throw new OperationsError(
      'rollback-failed',
      `rollback to ${previousVersion} failed: ${errorMessage(error)}${messages.length > 0 ? `. ${messages.join('; ')}` : ''}`,
      { cause: error },
    );
  } finally {
    releaseLocks?.();
    releaseOperation();
  }
}
