/**
 * The one-time machine-data transition machinery shared by the user and system
 * upgrade paths: a version-independent marker, a queue tree that is renamed
 * aside (never deleted outright) so a failed activation can rename it back, a
 * cleared database recoverable from the pre-upgrade copy, and a cleanup that
 * warns rather than fails and stays retriable while the marker is unwritten.
 */
import assert from 'node:assert/strict';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { describe, it } from 'node:test';

import {
  MACHINE_DATA_TRANSITION_FILE,
  STAGED_QUEUE_PREFIX,
  completeMachineDataCleanup,
  performMachineDataCleanup,
  readMachineDataTransition,
  recordMachineDataTransition,
  undoMachineDataCleanup,
} from '../src/cli/machine-transition';

function fresh(): string {
  const path = mkdtempSync(join(tmpdir(), 'rigscope-machine-transition-'));
  chmodSync(path, 0o700);
  return path;
}

/** A private hub-shaped database with one row in a machine-derived table. */
function seedDatabase(path: string): void {
  mkdirSync(join(path, '..'), { recursive: true, mode: 0o700 });
  const db = new DatabaseSync(path);
  try {
    db.exec('CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL)');
    db.exec('CREATE TABLE points_raw (series_id INTEGER, ts_ms INTEGER, value REAL)');
    db.exec('CREATE TABLE machine_meta (machine_id TEXT PRIMARY KEY, display_name TEXT)');
    db.exec("INSERT INTO schema_migrations VALUES (1, 'fixture')");
    db.exec('INSERT INTO points_raw VALUES (1, 1000, 1)');
    db.exec("INSERT INTO machine_meta VALUES ('machine-1', 'Workstation')");
  } finally {
    db.close();
  }
  chmodSync(path, 0o600);
}

function countRows(database: string, table: string): number {
  const db = new DatabaseSync(database, { readOnly: true });
  try {
    return Number((db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).get() as { n: number }).n);
  } finally {
    db.close();
  }
}

/** A stand-in for a durable per-destination collector queue tree. */
function seedQueue(stateRoot: string): { root: string; marker: string } {
  const root = join(stateRoot, 'collector', 'outbox');
  const marker = join(root, 'hash-of-destination-url', 'outbox.sqlite');
  mkdirSync(join(marker, '..'), { recursive: true, mode: 0o700 });
  writeFileSync(marker, 'unacknowledged sample\n', { mode: 0o600 });
  return { root, marker };
}

function layout(stateRoot: string) {
  return { database: join(stateRoot, 'hub', 'telemetry.sqlite'), queueTree: join(stateRoot, 'collector', 'outbox') };
}

describe('machine-data transition marker', () => {
  it('records durably and refuses to reset with a malformed or public marker', () => {
    const dir = fresh();
    assert.equal(readMachineDataTransition(dir), null, 'an absent marker means the transition is pending');
    recordMachineDataTransition(dir, '9.9.9');
    const path = join(dir, MACHINE_DATA_TRANSITION_FILE);
    assert.equal(statSync(path).mode & 0o777, 0o600, 'the marker is private');
    assert.equal(readMachineDataTransition(dir)?.release, '9.9.9');

    writeFileSync(path, 'not json at all\n', { mode: 0o600 });
    assert.throws(() => readMachineDataTransition(dir), /preserving machine data/);
    chmodSync(path, 0o644);
    assert.throws(() => performMachineDataCleanup(dir, layout(dir)), /preserving machine data/);
  });
});

describe('one-time machine-data cleanup', () => {
  it('renames the queue aside and clears rows, but marks complete only after completion', () => {
    const dir = fresh();
    const paths = layout(dir);
    seedDatabase(paths.database);
    const queue = seedQueue(dir);

    const cleanup = performMachineDataCleanup(dir, paths);
    assert.equal(cleanup.report.transitioned, true);
    assert.equal(cleanup.report.warning, null);
    assert.equal(cleanup.report.collectorQueuesRemoved, true);
    assert.equal(cleanup.report.databaseRows?.points_raw, 1);
    assert.equal(cleanup.databaseCleared, true);
    assert.equal(existsSync(queue.root), false, 'the live queue tree is gone');
    assert.ok(cleanup.stagedQueues !== null && cleanup.stagedQueues.includes(STAGED_QUEUE_PREFIX));
    assert.equal(readFileSync(join(cleanup.stagedQueues, 'hash-of-destination-url', 'outbox.sqlite'), 'utf8'), 'unacknowledged sample\n');
    assert.equal(countRows(paths.database, 'points_raw'), 0);
    assert.equal(countRows(paths.database, 'machine_meta'), 1, 'machine names/tags are presentation, not machine history');
    assert.equal(existsSync(join(dir, MACHINE_DATA_TRANSITION_FILE)), false, 'health is not proven yet: the marker is not this call\'s to write');

    completeMachineDataCleanup(dir, cleanup, '9.9.9');
    assert.notEqual(readMachineDataTransition(dir), null, 'a clean transition completes exactly once');
    assert.equal(cleanup.stagedQueues !== null && existsSync(cleanup.stagedQueues), false, 'the staged copy is discarded with the transition');
  });

  it('is skipped entirely once the marker exists, preserving durable data', () => {
    const dir = fresh();
    recordMachineDataTransition(dir, '9.9.9');
    const paths = layout(dir);
    seedDatabase(paths.database);
    const queue = seedQueue(dir);

    const skipped = performMachineDataCleanup(dir, paths);
    assert.equal(skipped.report.transitioned, false);
    assert.equal(skipped.report.databaseRows, null);
    assert.equal(skipped.databaseCleared, false);
    assert.equal(countRows(paths.database, 'points_raw'), 1, 'subsequent upgrades never clear durable history');
    assert.equal(existsSync(queue.marker), true, 'nor do they touch durable queues');
  });

  it('renames the staged queue back when activation failed after the clear', () => {
    const dir = fresh();
    const paths = layout(dir);
    seedDatabase(paths.database);
    const queue = seedQueue(dir);
    const cleanup = performMachineDataCleanup(dir, paths);

    const messages = undoMachineDataCleanup(paths, cleanup.stagedQueues);
    assert.deepEqual(messages.filter((message) => message.includes('queue tree')).length, 1);
    assert.equal(existsSync(queue.marker), true, 'the queue the transition staged aside is back intact');
    assert.equal(readFileSync(queue.marker, 'utf8'), 'unacknowledged sample\n');

    // A queue that reappeared in the meantime must not be silently overwritten.
    const again = performMachineDataCleanup(dir, layout(dir));
    seedQueue(dir); // the replacement runtime already wrote a fresh queue
    const kept = undoMachineDataCleanup(paths, again.stagedQueues);
    assert.ok(kept.some((message) => /replacement runtime queue preserved/.test(message)));
    assert.equal(existsSync(queue.marker), true, 'the original pending queue is restored');
    assert.equal(existsSync(again.stagedQueues!), false);
    assert.ok(readdirSync(join(dir, 'collector')).some((name) => name.startsWith('outbox.failed-transition-')));
  });

  it('warns instead of failing when the clear cannot run, keeps the marker unwritten, and retries later', () => {
    const dir = fresh();
    const paths = layout(dir);
    seedDatabase(paths.database);
    seedQueue(dir);

    const failing = performMachineDataCleanup(dir, paths, () => Date.now(), {
      clear: () => { throw new Error('fixture clear refusal'); },
    });
    assert.equal(failing.report.warning, 'hub database: fixture clear refusal');
    completeMachineDataCleanup(dir, failing, '9.9.9');
    assert.equal(readMachineDataTransition(dir), null, 'an incomplete transition is not recorded as complete');
    assert.ok(failing.stagedQueues !== null && existsSync(failing.stagedQueues), 'the partial copy is kept for inspection');

    // A retry preserves earlier recovery data and finishes the job.
    const retry = performMachineDataCleanup(dir, paths);
    assert.equal(retry.report.warning, null);
    const staged = readdirSync(join(dir, 'collector')).filter((name) => name.startsWith(STAGED_QUEUE_PREFIX));
    assert.equal(staged.length, 1, 'the earlier staged queue remains available for recovery');
    completeMachineDataCleanup(dir, retry, '9.9.9');
    assert.notEqual(readMachineDataTransition(dir), null, 'the retried upgrade completes the transition');
  });
  it('keeps committed-clear recovery conservative when post-commit work throws', () => {
    const dir = fresh();
    const paths = layout(dir);
    seedDatabase(paths.database);
    const cleanup = performMachineDataCleanup(dir, paths, Date.now, {
      clear: () => { throw new Error('post-commit failure'); },
    });
    assert.equal(cleanup.databaseCleared, true, 'activation recovery must restore the backup');
    assert.match(cleanup.report.warning!, /post-commit failure/);
  });

  it('reports completion-marker failure without failing a healthy upgrade', () => {
    const dir = fresh();
    const cleanup = performMachineDataCleanup(dir, layout(dir));
    assert.doesNotThrow(() => completeMachineDataCleanup(dir, cleanup, '9.9.9', () => {
      throw new Error('marker unavailable');
    }));
    assert.match(cleanup.report.warning!, /marker unavailable/);
    assert.equal(readMachineDataTransition(dir), null);
  });

});
