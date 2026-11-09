/**
 * End-to-end clean break: an incompatible managed installation is reset by
 * `rigscope upgrade` or a fresh `rigscope install`, not migrated. The fixture
 * package avoids the registry so the tests prove the command path, not the
 * release pipeline.
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { hashPassword } from '../src/hub/auth/password';
import { combinedUnitName, unitName } from '../src/cli/services';

const ROOT = join(import.meta.dirname, '..');
const CLI = join(ROOT, 'src', 'cli', 'main.ts');

function writeRuntime(runtimeDir: string, version: string): void {
  const app = join(runtimeDir, 'node_modules', 'rigscope');
  for (const directory of ['dist', '.next']) mkdirSync(join(app, directory), { recursive: true });
  writeFileSync(join(app, 'package.json'), `${JSON.stringify({name: 'rigscope', version})}\n`);
  for (const file of ['cli.cjs', 'hub.cjs', 'collector.cjs', 'ops.cjs']) {
    writeFileSync(join(app, 'dist', file), 'old runtime\n');
  }
  writeFileSync(join(app, '.next', 'BUILD_ID'), 'old build\n');
}

function stateRoot(root: string): {dir: string; home: string; configHome: string; binHome: string; bin: string} {
  const dir = join(root, 'state');
  const home = join(root, 'home');
  const configHome = join(home, '.config');
  const binHome = join(home, '.local', 'bin');
  const bin = join(root, 'bin');
  for (const path of [dir, home, configHome, binHome, bin]) mkdirSync(path, {recursive: true, mode: 0o700});
  chmodSync(dir, 0o700);
  writeFileSync(join(bin, 'systemctl'), '#!/bin/sh\nexit 0\n', {mode: 0o755});
  return {dir, home, configHome, binHome, bin};
}

function cliEnvironment(paths: ReturnType<typeof stateRoot>): NodeJS.ProcessEnv {
  return {
    ...process.env,
    HOME: paths.home,
    XDG_CONFIG_HOME: paths.configHome,
    XDG_BIN_HOME: paths.binHome,
    PATH: `${paths.bin}:${process.env.PATH ?? ''}`,
  };
}

function writeFixturePackage(root: string): string {
  const source = join(root, 'fixture', 'package');
  mkdirSync(join(source, 'dist'), {recursive: true});
  mkdirSync(join(source, '.next'), {recursive: true});
  writeFileSync(join(source, 'package.json'), `${JSON.stringify({name: 'rigscope', version: '9.9.9'})}\n`);
  for (const file of ['cli.cjs', 'hub.cjs', 'collector.cjs', 'ops.cjs']) {
    writeFileSync(join(source, 'dist', file), 'fixture\n');
  }
  writeFileSync(join(source, '.next', 'BUILD_ID'), 'fixture\n');
  const archive = join(root, 'fixture.tgz');
  execFileSync('tar', ['-czf', archive, '-C', join(root, 'fixture'), 'package']);
  return archive;
}

test('rigscope upgrade resets an incompatible managed installation', {timeout: 180000}, () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-upgrade-reset-'));
  const dir = join(root, 'state');
  const home = join(root, 'home');
  const configHome = join(home, '.config');
  const binHome = join(home, '.local', 'bin');
  const bin = join(root, 'bin');
  const systemctl = join(bin, 'systemctl');
  try {
    for (const path of [dir, home, configHome, binHome, bin]) mkdirSync(path, {recursive: true, mode: 0o700});
    chmodSync(dir, 0o700);
    writeFileSync(systemctl, '#!/bin/sh\nexit 0\n', {mode: 0o755});

    const oldHash = hashPassword('old-password');
    writeFileSync(join(dir, 'config.json'), `${JSON.stringify({
      version: 2,
      role: 'local',
      port: 8081,
      users: [{username: 'owner', passwordHash: oldHash, machineIds: '*', admin: true}],
      collectors: [{collectorId: 'old-collector', machineId: 'old-machine', token: 'old-token'}],
      components: {hub: {enabled: true}, collector: {enabled: true}},
    }, null, 2)}\n`, {mode: 0o600});
    mkdirSync(join(dir, 'collector'), {recursive: true, mode: 0o700});
    writeFileSync(join(dir, 'collector', 'state.json'), `${JSON.stringify({
      version: 2,
      collectorId: 'old-collector',
      destinations: [],
    })}\n`, {mode: 0o600});
    mkdirSync(join(dir, 'hub'), {recursive: true, mode: 0o700});
    writeFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'old history\n', {mode: 0o600});
    mkdirSync(join(dir, 'backups'), {recursive: true, mode: 0o700});
    writeFileSync(join(dir, 'backups', 'old.sqlite'), 'old backup\n', {mode: 0o600});

    const archive = writeFixturePackage(root);
    const result = spawnSync(process.execPath, [
      '--import', 'tsx', CLI, '--state-dir', dir,
      'upgrade', '--from', archive, '--no-start',
    ], {
      encoding: 'utf8',
      timeout: 150000,
      env: {
        ...process.env,
        HOME: home,
        XDG_CONFIG_HOME: configHome,
        XDG_BIN_HOME: binHome,
        PATH: `${bin}:${process.env.PATH ?? ''}`,
      },
    });
    assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);

    assert.match(result.stdout, /Old installation state detected/);
    assert.match(result.stdout, /Username: owner/);
    assert.match(result.stdout, /Password: /);

    const config = JSON.parse(readFileSync(join(dir, 'config.json'), 'utf8'));
    assert.equal('version' in config, false, 'the reset writes no version field');
    assert.deepEqual(config.components, {hub: {enabled: true}, collector: {enabled: true}});
    assert.equal(config.port, 8081);
    assert.notEqual(config.users[0].passwordHash, oldHash, 'fresh owner credentials replace the old hash');
    assert.deepEqual(config.collectors.length, 1, 'the colocated collector is re-paired fresh');

    assert.equal(existsSync(join(dir, 'hub', 'telemetry.sqlite')), false, 'old history is removed');
    assert.equal(existsSync(join(dir, 'backups', 'old.sqlite')), false, 'old backups are removed');
    assert.equal(existsSync(join(dir, 'machine-data-transition.json')), true, 'the reset records the one-time machine-data transition as complete');

    const collector = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal('version' in collector, false, 'collector state is canonical');
    assert.equal(collector.destinations[0].url, 'ws://127.0.0.1:8081/api/collector/connect');

    const runtime = JSON.parse(readFileSync(join(dir, 'runtime', 'node_modules', 'rigscope', 'package.json'), 'utf8'));
    assert.equal(runtime.version, '9.9.9', 'the requested release is installed');
    assert.equal('rigscopeConfigVersion' in runtime, false, 'the installed package carries no configuration version');
  } finally {
    rmSync(root, {recursive: true, force: true});
  }
});

test('a hub-only reset upgrade retains its preset without adding local collection', {timeout: 180000}, () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-hub-reset-'));
  const paths = stateRoot(root);
  const {dir} = paths;
  try {
    writeRuntime(join(dir, 'runtime'), '0.1.2');
    writeFileSync(join(dir, 'config.json'), JSON.stringify({
      version: 2, role: 'hub', port: 8081,
      users: [{username: 'owner', passwordHash: hashPassword('old-password'), machineIds: '*', admin: true}],
      collectors: [], components: {hub: {enabled: true}, collector: {enabled: false}},
    }), {mode: 0o600});
    const result = spawnSync(process.execPath, ['--import', 'tsx', CLI, '--state-dir', dir,
      'upgrade', '--from', writeFixturePackage(root), '--no-start'], {
      encoding: 'utf8', timeout: 150000, env: cliEnvironment(paths),
    });
    assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
    const config = JSON.parse(readFileSync(join(dir, 'config.json'), 'utf8'));
    assert.equal(config.role, 'hub');
    assert.deepEqual(config.components, {hub: {enabled: true}, collector: {enabled: false}});
    assert.equal(existsSync(join(dir, 'collector', 'state.json')), false);
  } finally { rmSync(root, {recursive: true, force: true}); }
});

test('rigscope install replaces an incompatible runtime and removes owned units and state', {timeout: 180000}, () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-install-reset-'));
  const paths = stateRoot(root);
  const { dir } = paths;
  try {
    writeRuntime(join(dir, 'runtime'), '1.0.0');
    writeFileSync(join(dir, 'runtime', 'old-marker'), 'old runtime\n');
    const oldHash = hashPassword('old-password');
    writeFileSync(join(dir, 'config.json'), `${JSON.stringify({
      version: 2,
      role: 'local',
      port: 8081,
      users: [{username: 'owner', passwordHash: oldHash, machineIds: '*', admin: true}],
      collectors: [{collectorId: 'old-collector', machineId: 'old-machine', token: 'old-token'}],
      components: {hub: {enabled: true}, collector: {enabled: true}},
    }, null, 2)}\n`, {mode: 0o600});
    mkdirSync(join(dir, 'collector'), {recursive: true, mode: 0o700});
    writeFileSync(join(dir, 'collector', 'state.json'), `${JSON.stringify({
      version: 2, collectorId: 'old-collector', destinations: [],
    })}\n`, {mode: 0o600});
    mkdirSync(join(dir, 'hub'), {recursive: true, mode: 0o700});
    writeFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'old history\n', {mode: 0o600});
    mkdirSync(join(dir, 'backups'), {recursive: true, mode: 0o700});
    writeFileSync(join(dir, 'backups', 'old.sqlite'), 'old backup\n', {mode: 0o600});

    const unitDir = join(paths.configHome, 'systemd', 'user');
    mkdirSync(unitDir, {recursive: true});
    const oldTarget = join(dir, 'runtime', 'node_modules', 'rigscope', 'bin', 'rigscope.cjs');
    writeFileSync(join(unitDir, combinedUnitName(dir)), `[Service]\nExecStart=/usr/bin/node ${oldTarget} run --state-dir ${dir}\n`, {mode: 0o600});

    const archive = writeFixturePackage(root);
    const result = spawnSync(process.execPath, [
      '--import', 'tsx', CLI, '--state-dir', dir,
      'install', '--from', archive, '--no-start',
    ], {
      encoding: 'utf8',
      timeout: 150000,
      env: cliEnvironment(paths),
    });
    assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
    assert.match(result.stdout, /Old installation state detected/);

    const runtime = JSON.parse(readFileSync(join(dir, 'runtime', 'node_modules', 'rigscope', 'package.json'), 'utf8'));
    assert.equal(runtime.version, '9.9.9', 'install activates the staged replacement');
    assert.equal(existsSync(join(dir, 'runtime', 'old-marker')), false, 'the incompatible runtime is gone');
    assert.equal(existsSync(join(dir, 'runtime-previous')), false, 'the incompatible runtime is not a rollback target');
    assert.equal(existsSync(join(unitDir, combinedUnitName(dir))), false, 'the historical combined unit is removed');
    assert.ok(existsSync(join(unitDir, unitName(dir, 'hub'))));
    assert.ok(existsSync(join(unitDir, unitName(dir, 'collector'))));
    assert.equal(existsSync(join(dir, 'machine-data-transition.json')), true, 'a fresh install records the one-time transition as complete');

    const config = JSON.parse(readFileSync(join(dir, 'config.json'), 'utf8'));
    assert.equal('version' in config, false);
    assert.deepEqual(config.components, {hub: {enabled: true}, collector: {enabled: true}});
    assert.notEqual(config.users[0].passwordHash, oldHash);
    assert.equal(existsSync(join(dir, 'hub', 'telemetry.sqlite')), false);
    assert.equal(existsSync(join(dir, 'backups', 'old.sqlite')), false);
    const collector = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal('version' in collector, false);
    assert.equal(collector.destinations[0].url, 'ws://127.0.0.1:8081/api/collector/connect');
  } finally {
    rmSync(root, {recursive: true, force: true});
  }
});
