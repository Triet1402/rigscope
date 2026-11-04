import test from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, mkdtempSync, rmSync, writeFileSync, existsSync, readFileSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { acquireProcessLease, ProcessLeaseError } from '../src/runtime/process-lease';

test('runtime lease prevents concurrent owners, releases cleanly and recovers a proven dead owner', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-lease-')); const path = join(dir, 'hub.db.hub.lock');
  try {
    const release = acquireProcessLease(path);
    assert.throws(() => acquireProcessLease(path), /another process owns/);
    release(); assert.equal(existsSync(path), false);
    writeFileSync(path, JSON.stringify({ pid: 2147483647 }), { mode: 0o600 });
    acquireProcessLease(path)(); assert.equal(existsSync(path), false);
    writeFileSync(path, '', { mode: 0o600 });
    assert.throws(() => acquireProcessLease(path), /incomplete/);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('runtime lease fails closed on a symlink without altering its target', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-lease-'));
  try {
    const target = join(dir, 'target'); writeFileSync(target, 'keep', { mode: 0o600 });
    const lock = join(dir, 'lock'); symlinkSync(target, lock);
    assert.throws(() => acquireProcessLease(lock), /unsafe/);
    assert.equal(existsSync(target), true);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('runtime lease distinguishes live owners, dead owners and ambiguous lock content', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-lease-'));
  const lock = join(dir, 'state.lock');
  try {
    const release = acquireProcessLease(lock);
    try {
      assert.throws(
        () => acquireProcessLease(lock),
        (error: unknown) => error instanceof ProcessLeaseError && error.code === 'busy',
      );
    } finally { release(); }

    writeFileSync(lock, JSON.stringify({ pid: 2147483647, instance: 'dead-owner' }), { mode: 0o600 });
    const recovered = acquireProcessLease(lock);
    try {
      assert.match(readFileSync(lock, 'utf8'), new RegExp(`"pid":${process.pid}`));
    } finally { recovered(); }
    assert.equal(existsSync(lock), false);

    writeFileSync(lock, JSON.stringify({ pid: process.pid, instance: 'live-owner' }), { mode: 0o600 });
    assert.throws(
      () => acquireProcessLease(lock),
      (error: unknown) => error instanceof ProcessLeaseError && error.code === 'busy',
    );
    rmSync(lock);

    writeFileSync(lock, JSON.stringify({ pid: 'not-a-pid' }), { mode: 0o600 });
    assert.throws(
      () => acquireProcessLease(lock),
      (error: unknown) => error instanceof ProcessLeaseError && error.code === 'invalid',
    );
    rmSync(lock);

    writeFileSync(lock, 'interrupted write', { mode: 0o600 });
    assert.throws(
      () => acquireProcessLease(lock),
      (error: unknown) => error instanceof ProcessLeaseError && error.code === 'incomplete',
    );
    rmSync(lock);

    writeFileSync(lock, JSON.stringify({ pid: 2147483647 }), { mode: 0o600 });
    chmodSync(lock, 0o644);
    assert.throws(
      () => acquireProcessLease(lock),
      (error: unknown) => error instanceof ProcessLeaseError && error.code === 'unsafe',
    );
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('runtime lease release removes its exit listener instead of accumulating', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-lease-'));
  const lock = join(dir, 'updates.lock');
  try {
    const before = process.listenerCount('exit');
    for (let index = 0; index < 20; index += 1) {
      acquireProcessLease(lock)();
    }
    assert.equal(process.listenerCount('exit'), before);
    assert.equal(existsSync(lock), false);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});
