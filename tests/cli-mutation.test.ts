import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { initialize, type Component } from '../src/cli/config';
import { completeMutation, prepareMutation, type MutationDependencies } from '../src/cli/mutation';

function fixture() {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-mutation-'));
  initialize(dir);
  const events: string[] = [];
  const deps: MutationDependencies = {
    foreground: () => [],
    status: (_dir, component) => ({unit: component!, installed: true, active: component === 'hub', subState: 'running', execMainPid: 1, error: null}),
    stopService: (_dir, component) => { events.push(`stop ${component}`); },
    stopForeground: () => { events.push('stop foreground'); },
    start: async (_dir, component) => { events.push(`start ${component}`); return 'started'; },
    confirm: async () => { events.push('prompt'); return true; },
  };
  return {dir, deps, events, clean: () => rmSync(dir, {recursive: true, force: true})};
}

test('mutation prompts, stops and resumes only previously running components', async () => {
  const f = fixture();
  try {
    const mutation = await prepareMutation(f.dir, ['hub','collector'], {}, f.deps);
    assert.deepEqual(f.events, ['prompt','stop hub']);
    await mutation.resume();
    assert.deepEqual(f.events, ['prompt','stop hub','start hub']);
  } finally { f.clean(); }
});

test('declining leaves processes untouched and --yes bypasses confirmation', async () => {
  const f = fixture();
  try {
    f.deps.confirm = async () => false;
    await assert.rejects(prepareMutation(f.dir, ['hub'], {}, f.deps), /Cancelled/);
    assert.deepEqual(f.events, []);
    await prepareMutation(f.dir, ['hub'], {yes: true}, f.deps);
    assert.deepEqual(f.events, ['stop hub']);
  } finally { f.clean(); }
});

test('uninstall and --no-restart never resume components', async () => {
  for (const options of [{uninstall: true}, {noRestart: true}]) {
    const f = fixture();
    try {
      const mutation = await prepareMutation(f.dir, ['hub'], {yes: true, ...options}, f.deps);
      await mutation.resume();
      assert.deepEqual(f.events, ['stop hub']);
    } finally { f.clean(); }
  }
});

test('upgrade retains its internal service coordination, unless --no-restart is selected', async () => {
  const f = fixture();
  try {
    const mutation = await prepareMutation(f.dir, ['hub'], {yes: true, managesServices: true}, f.deps);
    await mutation.resume();
    assert.deepEqual(f.events, []);
    const stopped = await prepareMutation(f.dir, ['hub'], {yes: true, managesServices: true, noRestart: true}, f.deps);
    await stopped.resume();
    assert.deepEqual(f.events, ['stop hub']);
  } finally { f.clean(); }
});

test('owned foreground runtimes stop after approval and resume after success', async () => {
  const f = fixture();
  try {
    f.deps.status = (_dir, component) => ({unit: component!, installed: false, active: false, subState: 'inactive', execMainPid: null, error: null});
    f.deps.foreground = () => ['hub', 'collector'] as Component[];
    const mutation = await prepareMutation(f.dir, ['hub'], {yes: true}, f.deps);
    await mutation.resume();
    assert.deepEqual(f.events, ['stop foreground','start hub','start collector']);
  } finally { f.clean(); }
});

test('a failed connect resumes the components it stopped; other failures do not', async () => {
  const f = fixture();
  try {
    const reported: string[] = [];
    const failing = async (): Promise<number> => { throw new Error('enrollment rejected'); };
    const mutation = await prepareMutation(f.dir, ['hub'], {yes: true}, f.deps);
    assert.equal(await completeMutation(mutation, failing, true, line => reported.push(line)), 1);
    assert.deepEqual(f.events, ['stop hub', 'start hub']);
    assert.deepEqual(reported, ['Rigscope: enrollment rejected']);

    f.events.length = 0;
    const other = await prepareMutation(f.dir, ['hub'], {yes: true}, f.deps);
    await assert.rejects(completeMutation(other, failing, false), /enrollment rejected/);
    assert.deepEqual(f.events, ['stop hub']);

    f.events.length = 0;
    const succeeded = await prepareMutation(f.dir, ['hub'], {yes: true}, f.deps);
    assert.equal(await completeMutation(succeeded, async () => 0, true), 0);
    assert.deepEqual(f.events, ['stop hub', 'start hub']);
  } finally { f.clean(); }
});
