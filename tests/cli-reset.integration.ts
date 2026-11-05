/** Explicit disposable systemd check for resetting a running unfinished install. */
import test from 'node:test';
import assert from 'node:assert/strict';
import {execFileSync, spawnSync} from 'node:child_process';
import {existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {unitName, unitPath} from '../src/cli/services';
import {closedLoopbackPort} from './runtime/support/http';

test('reset stops a running hub and replaces its unfinished state and runtime', {timeout: 180000}, async () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-running-reset-'));
  const dir = join(root, 'state');
  const bootstrap = join(root, 'bootstrap');
  const version = JSON.parse(readFileSync(join(process.cwd(), 'package.json'), 'utf8')).version;
  const archive = process.env.RIGSCOPE_TEST_ARCHIVE ?? join(process.cwd(), `rigscope-${version}.tgz`);
  const env: NodeJS.ProcessEnv = {...process.env, XDG_BIN_HOME: join(root, 'bin')};
  for (const key of Object.keys(env)) if (/^(HUB_|COLLECTOR_|RIGSCOPE_)/.test(key)) delete env[key];
  const cli = join(bootstrap, 'node_modules', 'rigscope', 'bin', 'rigscope.cjs');
  const run = (file: string, args: string[]) => execFileSync(file, args, {
    env, encoding: 'utf8', timeout: 90000, stdio: ['ignore', 'pipe', 'pipe'],
  });
  const invoke = (args: string[]) => run(process.execPath, [cli, ...args, '--state-dir', dir]);
  try {
    run('npm', ['install', '--prefix', bootstrap, '--omit=dev', '--ignore-scripts', '--no-audit', '--no-fund', archive]);
    const port = await closedLoopbackPort();
    invoke(['install', '--role', 'hub', '--port', String(port)]);
    const hub = unitName(dir, 'hub');
    const oldPid = Number(run('systemctl', ['--user', 'show', hub, '--property=MainPID', '--value']).trim());
    assert.ok(oldPid > 0);
    const path = join(dir, 'config.json');
    const old = JSON.parse(readFileSync(path, 'utf8'));
    writeFileSync(path, JSON.stringify({...old, version: 2}));
    assert.equal(existsSync(join(dir, 'hub', 'telemetry.sqlite')), true);

    const reset = invoke(['upgrade', '--from', archive, '--no-start', '--yes']);
    assert.match(reset, /Resetting configuration and history/);
    assert.match(reset, /Password: /);
    assert.equal(run('systemctl', ['--user', 'show', hub, '--property=ActiveState', '--value']).trim(), 'inactive');
    assert.throws(() => process.kill(oldPid, 0), /ESRCH/);
    assert.equal(existsSync(join(dir, 'hub', 'telemetry.sqlite')), false);
    assert.equal(existsSync(join(dir, 'runtime-previous')), false);
    assert.equal(existsSync(join(dir, 'reset-pending.json')), false);
    const current = JSON.parse(readFileSync(path, 'utf8'));
    assert.equal('version' in current, false);
    assert.equal(current.role, 'hub');
    assert.deepEqual(current.components, {hub: {enabled: true}, collector: {enabled: false}});
    assert.notEqual(current.users[0].passwordHash, old.users[0].passwordHash);
    invoke(['configure', '--port', String(port)]);
    assert.match(invoke(['start', 'hub']), /Hub service started/);
    assert.match(invoke(['status']), /Health: ok/);
  } finally {
    if (existsSync(cli)) spawnSync(process.execPath, [cli, 'uninstall', '--state-dir', dir, '--yes'], {env, stdio: 'ignore', timeout: 30000});
    for (const component of ['hub', 'collector'] as const) {
      const unit = unitName(dir, component);
      spawnSync('systemctl', ['--user', 'stop', unit], {stdio: 'ignore', timeout: 15000});
      spawnSync('systemctl', ['--user', 'disable', unit], {stdio: 'ignore', timeout: 15000});
      rmSync(unitPath(dir, component), {force: true});
      spawnSync('systemctl', ['--user', 'reset-failed', unit], {stdio: 'ignore', timeout: 15000});
    }
    spawnSync('systemctl', ['--user', 'daemon-reload'], {stdio: 'ignore', timeout: 15000});
    rmSync(root, {recursive: true, force: true});
  }
});
