import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

test('system collector exits cleanly before and after an incomplete first enrollment', () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-await-'));
  const state = join(dir, 'state.json');
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('COLLECTOR_')));
  const run = (args: string[]) => spawnSync(process.execPath, [join(process.cwd(), 'dist', 'collector.cjs'), ...args], {
    env: {...env, NODE_ENV: 'production', COLLECTOR_MODE: 'real', COLLECTOR_STATE_FILE: state}, encoding: 'utf8', timeout: 5000,
  });
  try {
    const initial = run(['--await-connection']);
    assert.equal(initial.status, 0, initial.stderr);
    assert.match(initial.stdout, /awaiting connection/);
    assert.equal(existsSync(state), true, 'the installation identity is retained');
    const retry = run(['--await-connection']);
    assert.equal(retry.status, 0, retry.stderr);
    assert.match(retry.stdout, /awaiting connection/);
    assert.equal(existsSync(join(dir, 'collector-runtime.lock')), false, 'collection never starts');
    assert.equal(run([]).status, 1, 'ordinary standalone startup still rejects missing destinations');
  } finally {
    rmSync(dir, {recursive: true, force: true});
  }
});
