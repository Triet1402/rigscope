/**
 * The one-time machine-data transition that accompanies the RAM-first
 * telemetry release, and the durable marker that says it already happened.
 *
 * A release before RAM-first kept machine-derived data on disk: hub telemetry
 * rows and summaries, deduplication receipts, gap records, the last-known live
 * snapshot, captured inventory, machine-derived alert history, and a durable
 * per-destination collector outbox. This transition discards all of that once,
 * including previously opted-in persistent telemetry and queues. This reset
 * always preserves accounts and sessions, collector enrollment and grants, settings, alert rules/jobs/webhook
 * configuration, machine display names/tags (`machine_meta`, still read to
 * present machines) and recordings.
 *
 * The marker is one private file in the installation's state root, deliberately
 * named independently of any release version: every upgrade checks the marker,
 * and only the very first upgrade after this release performs the transition.
 * Subsequent upgrades preserve durable history and queues unconditionally.
 * The marker is written only after the transition ran without any warning and
 * the activated release verified healthy, so a failed or partially failed
 * upgrade retries the transition on the next upgrade.
 *
 * Reversibility matters because the transition runs before the activated
 * release is proven healthy:
 *   - the collector queue tree is renamed aside (`outbox.pre-transition-…`),
 *     never deleted outright, so a failed activation renames it back;
 *   - the cleared database rows are recoverable from the pre-upgrade backup
 *     the upgrade already recorded, and recovery restores it even when the
 *     previous runtime could still read the schema — the rows, not the schema,
 *     are what the transition removed.
 * Staged copies from interrupted attempts remain available for recovery;
 * a retry must never silently delete another attempt's recovery data.
 */

import { existsSync, lstatSync, readFileSync, renameSync, rmSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { clearMachineHistory } from '../operations/database';
import { OperationsError, errorMessage, timestampSlug, writePrivateFile } from '../operations/fs';

/** Marker file name inside the installation state root; version-independent. */
export const MACHINE_DATA_TRANSITION_FILE = 'machine-data-transition.json';

/** Prefix of the staged collector queue tree created by a transition attempt. */
export const STAGED_QUEUE_PREFIX = 'outbox.pre-transition-';

export type MachineDataPaths = {
  /** The hub database whose machine-derived tables are cleared. */
  database: string;
  /** The durable collector queue tree that is renamed aside. */
  queueTree: string;
};

export type MachineDataTransitionMarker = {
  transitionedAt: string;
  /** Release that completed the transition; informational only. */
  release: string | null;
};

/** What an upgrade report tells the operator about machine data. */
export type MachineDataReport = {
  /** True when this upgrade performed the one-time transition now. */
  transitioned: boolean;
  /** Rows removed per hub table; null when there was no database or nothing ran. */
  databaseRows: Record<string, number> | null;
  /** True when a durable collector queue tree was discarded. */
  collectorQueuesRemoved: boolean;
  /**
   * Cleanup/finalization warning; the upgrade itself still succeeded.
   * Without a completion marker the next upgrade retries the transition;
   * a leftover staged copy after marker publication does not repeat the reset.
   */
  warning: string | null;
};

/** Internal transition state: the report plus what recovery must undo. */
export type MachineDataCleanup = {
  report: MachineDataReport;
  /** Staged queue tree a failed activation renames back, or null. */
  stagedQueues: string | null;
  /**
   * True when machine-derived rows were deleted. Recovery then restores the
   * pre-upgrade backup even if the previous runtime could still read the
   * schema, because the loss is the rows, not the schema.
   */
  databaseCleared: boolean;
};

export type MachineDataCleanupDependencies = {
  clear?: (database: string) => { deleted: Record<string, number>; integrity: string };
};

export function machineDataTransitionPath(stateRoot: string): string {
  return join(stateRoot, MACHINE_DATA_TRANSITION_FILE);
}

/**
 * Only an absent marker means the transition is pending. An unreadable or
 * malformed existing marker must never authorize another destructive reset.
 */
export function readMachineDataTransition(stateRoot: string): MachineDataTransitionMarker | null {
  const path = machineDataTransitionPath(stateRoot);
  try {
    const stat = lstatSync(path);
    if (!stat.isFile() || stat.isSymbolicLink() || (stat.mode & 0o077) !== 0) {
      throw new Error('marker must be a private regular file');
    }
    const value = JSON.parse(readFileSync(path, 'utf8')) as unknown;
    if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error('invalid marker');
    const record = value as Record<string, unknown>;
    if (typeof record.transitionedAt !== 'string' || !Number.isFinite(Date.parse(record.transitionedAt))) {
      throw new Error('invalid transition date');
    }
    return { transitionedAt: record.transitionedAt, release: typeof record.release === 'string' ? record.release : null };
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return null;
    throw new OperationsError('invalid-transition-marker',
      `Cannot determine whether the one-time reset completed; preserving machine data. Repair ${path}: ${errorMessage(error)}`,
      { cause: error });
  }
}

/**
 * Record that this installation has completed the machine-data transition.
 * Durably written: losing this file would let a later upgrade treat a
 * post-transition installation as legacy and discard data the operator has
 * opted back into keeping. The system installation passes its own writer
 * because its root-owned state directory is deliberately not 0700.
 */
export function recordMachineDataTransition(
  stateRoot: string,
  release: string | null,
  write: (path: string, content: string) => void = (path, content) => writePrivateFile(path, content),
): void {
  const marker: MachineDataTransitionMarker = { transitionedAt: new Date().toISOString(), release };
  write(machineDataTransitionPath(stateRoot), `${JSON.stringify(marker, null, 2)}\n`);
}

function newMachineDataCleanup(transitioned: boolean): MachineDataCleanup {
  return {
    report: { transitioned, databaseRows: null, collectorQueuesRemoved: false, warning: null },
    stagedQueues: null,
    databaseCleared: false,
  };
}

/**
 * Perform the one-time machine-data transition. A cleanup that
 * cannot finish is reported as a warning so it cannot fail an otherwise
 * healthy upgrade, and the missing marker makes the next upgrade retry it.
 * An invalid existing marker refuses the operation before any cleanup begins.
 * Callers must have stopped every component and hold the installation leases,
 * because clearing the hub database is only safe while no hub owns it.
 */
export function performMachineDataCleanup(
  stateRoot: string,
  paths: MachineDataPaths,
  now: () => number = Date.now,
  deps: MachineDataCleanupDependencies = {},
): MachineDataCleanup {
  if (readMachineDataTransition(stateRoot) !== null) return newMachineDataCleanup(false);
  const cleanup = newMachineDataCleanup(true);
  const clear = deps.clear ?? clearMachineHistory;
  const warnings: string[] = [];

  // Rename the queue tree aside instead of deleting it: a failed activation
  // renames it back. Keep earlier attempts' staged copies for recovery;
  // their presence does not prove that those activations succeeded.
  try {
    if (existsSync(paths.queueTree)) {
      const staged = join(dirname(paths.queueTree), `${STAGED_QUEUE_PREFIX}${timestampSlug(now())}-${randomUUID()}`);
      renameSync(paths.queueTree, staged);
      cleanup.stagedQueues = staged;
      cleanup.report.collectorQueuesRemoved = true;
    }
  } catch (error) {
    warnings.push(`collector queues: ${errorMessage(error)}`);
  }

  try {
    if (existsSync(paths.database)) {
      // The clear can commit and then fail during checkpoint/integrity work.
      // Conservatively restore the backup on activation failure in either case.
      cleanup.databaseCleared = true;
      cleanup.report.databaseRows = clear(paths.database).deleted;
    }
  } catch (error) {
    warnings.push(`hub database: ${errorMessage(error)}`);
  }

  cleanup.report.warning = warnings.length > 0 ? warnings.join('; ') : null;
  return cleanup;
}

/**
 * Undo the reversible part of a transition after a failed activation: rename
 * the staged queue tree back. The cleared rows are restored by the caller from
 * the recorded pre-upgrade backup (see `recoverActivation` and
 * `recoverFailedUpgrade`). Never throws; problems come back as messages.
 */
export function undoMachineDataCleanup(paths: MachineDataPaths, stagedQueues: string | null): string[] {
  if (stagedQueues === null || !existsSync(stagedQueues)) return [];
  try {
    if (existsSync(paths.queueTree)) {
      // A durable replacement runtime may already have created a fresh queue.
      // Keep that evidence separately and restore the original pending samples.
      const failedQueue = `${paths.queueTree}.failed-transition-${randomUUID()}`;
      renameSync(paths.queueTree, failedQueue);
      try {
        renameSync(stagedQueues, paths.queueTree);
      } catch (error) {
        renameSync(failedQueue, paths.queueTree);
        throw error;
      }
      return [`restored the collector queue tree; replacement runtime queue preserved at ${failedQueue}`];
    }
    renameSync(stagedQueues, paths.queueTree);
    return ['restored the collector queue tree the transition had staged aside'];
  } catch (error) {
    return [`machine data rollback could not restore the collector queue: ${errorMessage(error)}`];
  }
}

/**
 * Finalize a transition once the activated release verified healthy: write the
 * marker and discard the staged queue copy. When cleanup reported a warning the
 * marker stays unwritten — the next upgrade retries — and the staged copy is
 * kept so the operator can still inspect the data the partial cleanup removed.
 */
export function completeMachineDataCleanup(
  stateRoot: string,
  cleanup: MachineDataCleanup,
  release: string | null,
  write?: (path: string, content: string) => void,
): void {
  if (!cleanup.report.transitioned) return;
  if (cleanup.report.warning !== null) return;
  try {
    recordMachineDataTransition(stateRoot, release, write);
  } catch (error) {
    cleanup.report.warning = `could not record transition completion: ${errorMessage(error)}`;
    return;
  }
  if (cleanup.stagedQueues !== null) {
    try {
      rmSync(cleanup.stagedQueues, { recursive: true, force: true });
    } catch (error) {
      // Completion is already durable. Do not roll back a healthy installation
      // or clear newly retained history on its next upgrade for a stale copy.
      cleanup.report.warning = `transition completed; staged queue remains at ${cleanup.stagedQueues}: ${errorMessage(error)}`;
    }
  }
}

/** Total rows discarded across every cleared table; null when no database ran. */
export function machineDataReportRows(report: MachineDataReport): number | null {
  if (report.databaseRows === null) return null;
  return Object.values(report.databaseRows).reduce((sum, rows) => sum + rows, 0);
}

/** Human sentence naming what a transition kept, shared by both upgrade paths. */
export function machineDataKeptNote(): string {
  return 'accounts, sessions, enrollment and grants, settings, alert rules/jobs/webhook settings, machine names/tags and recordings were kept';
}

/**
 * One operator-facing summary per upgrade, identical for the user and system
 * paths. The warning is returned separately so each CLI can print it with its
 * own warning styling; it must always be visible because the incomplete
 * cleanup is retried by the next upgrade rather than silently dropped.
 */
export function describeMachineDataReport(report: MachineDataReport): { summary: string; warning: string | null } {
  const summary = report.transitioned
    ? (() => {
        const rows = machineDataReportRows(report);
        const parts = [
          rows === null ? 'no hub database' : `${rows} database row(s)`,
          ...(report.collectorQueuesRemoved ? ['the collector queue tree'] : []),
        ];
        return `Machine data: one-time RAM-first transition discarded ${parts.join(" and ")}. The ${machineDataKeptNote()}.`;
      })()
    : 'Machine data: one-time transition already completed; durable history and collector queues preserved.';
  return {
    summary,
    warning: report.warning === null
      ? null
      : `Machine-data cleanup warning (${report.warning}). The upgrade is healthy; cleanup without a completion marker is retried on the next upgrade.`,
  };
}
