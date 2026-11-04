import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { commandLauncher, installAlias, uninstallService } from '../src/cli/services';
import { selectInstallation } from '../src/cli/selection';

test('installation selection refuses ambiguity and keeps explicit user state separate from system paths', () => {
  const options = {explicitState: false, userPresent: true, systemPresent: true};
  assert.throws(() => selectInstallation(options), /Both user and system/);
  assert.equal(selectInstallation({...options, scope: 'user'}), 'user');
  assert.equal(selectInstallation({...options, scope: 'system'}), 'system');
  assert.equal(selectInstallation({...options, explicitState: true}), 'user');
  assert.throws(() => selectInstallation({...options, explicitState: true, scope: 'system'}), /state-dir/);
  assert.equal(selectInstallation({...options, userPresent: false}), 'system');
  assert.equal(selectInstallation({...options, userPresent: false, systemPresent: false}), 'user');
  assert.throws(() => selectInstallation({...options, scope: 'other'}), /scope must/);
});

test('managed launcher pins Node, preserves arguments and binds state even with shell metacharacters', () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-launcher-'));
  try {
    const state = join(root, "state ' $(do-not-run)");
    const bin = join(state, 'runtime', 'node_modules', 'rigscope', 'bin');
    mkdirSync(bin, {recursive: true});
    writeFileSync(join(bin, 'rigscope.cjs'), 'console.log(JSON.stringify({state: process.env.RIGSCOPE_STATE_DIR, args: process.argv.slice(2)}));');
    const launcher = join(root, 'rigscope');
    writeFileSync(launcher, commandLauncher(state), {mode: 0o755});
    const env: NodeJS.ProcessEnv = {...process.env, PATH: '/nonexistent'};
    delete env.RIGSCOPE_STATE_DIR;
    const actual = JSON.parse(execFileSync(launcher, ['status', '--state-dir', 'a b'], {env, encoding: 'utf8'}));
    assert.deepEqual(actual, {state, args: ['status', '--state-dir', 'a b']});
    const override = JSON.parse(execFileSync(launcher, [], {env: {...env, RIGSCOPE_STATE_DIR: 'another'}, encoding: 'utf8'}));
    assert.equal(override.state, 'another');
  } finally { rmSync(root, {recursive: true, force: true}); }
});

test('installer replaces its checkout link, preserves unrelated commands, and uninstalls only its own launcher', () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-alias-'));
  const previousBin = process.env.XDG_BIN_HOME;
  const previousConfig = process.env.XDG_CONFIG_HOME;
  try {
    process.env.XDG_BIN_HOME = join(root, 'bin');
    process.env.XDG_CONFIG_HOME = join(root, 'config');
    mkdirSync(process.env.XDG_BIN_HOME);
    const state = join(root, 'state');
    const alias = join(process.env.XDG_BIN_HOME, 'rigscope');
    const source = join(root, 'checkout');
    symlinkSync(join(source, 'bin', 'rigscope.cjs'), alias);
    assert.equal(installAlias(state, source), alias);
    assert.match(readFileSync(alias, 'utf8'), /^#!\/bin\/sh\n# Rigscope installation:/);
    assert.equal(installAlias(state, source), alias, 'repeat install repairs the same launcher');
    uninstallService(join(root, 'other'));
    assert.ok(existsSync(alias), 'another installation cannot remove it');
    uninstallService(state);
    assert.equal(existsSync(alias), false);
    writeFileSync(alias, '#!/bin/sh\n# another program\n', {mode: 0o755});
    assert.notEqual(installAlias(state, source), alias);
    assert.match(readFileSync(alias, 'utf8'), /another program/);
  } finally {
    if (previousBin === undefined) delete process.env.XDG_BIN_HOME; else process.env.XDG_BIN_HOME = previousBin;
    if (previousConfig === undefined) delete process.env.XDG_CONFIG_HOME; else process.env.XDG_CONFIG_HOME = previousConfig;
    rmSync(root, {recursive: true, force: true});
  }
});
