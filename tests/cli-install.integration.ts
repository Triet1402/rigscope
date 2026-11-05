/** Explicit real-user-systemd check. Never runs in the default fixture suite. */
import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { unitName, unitPath } from '../src/cli/services';

const COMPONENTS = ['hub', 'collector'] as const;
const unit = (dir: string, component: 'hub' | 'collector') => unitName(dir, component);
import { closedLoopbackPort, expectPortClosed } from './runtime/support/http';
import { loginBrowser } from './runtime/support/browser';
import { request } from 'node:http';

test('release installs in the background, survives bootstrap removal, enrolls, and uninstalls cleanly', {timeout: 180000}, async () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-service-'));
  const local = join(root, 'local');
  const collector = join(root, 'collector');
  const bootstrap = join(root, 'bootstrap');
  const version = JSON.parse(readFileSync(join(process.cwd(), 'package.json'), 'utf8')).version;
  const archive = process.env.RIGSCOPE_TEST_ARCHIVE || join(process.cwd(), `rigscope-${version}.tgz`);
  const env: NodeJS.ProcessEnv = {...process.env, XDG_BIN_HOME: join(root, 'bin')};
  delete env.RIGSCOPE_STATE_DIR;
  const port = await closedLoopbackPort();
  const base = `http://127.0.0.1:${port}`;
  const run = (file: string, args: string[], timeout = 60000) => execFileSync(file, args, {env, encoding: 'utf8', timeout, stdio: ['ignore', 'pipe', 'pipe']});
  let launcher = join(root, 'bin', 'rigscope');
  try {
    run('npm', ['install', '--prefix', bootstrap, '--omit=dev', '--ignore-scripts', '--no-audit', '--no-fund', archive]);
    const cli = join(bootstrap, 'node_modules', 'rigscope', 'bin', 'rigscope.cjs');
    const installed = run(process.execPath, [cli, 'install', '--state-dir', local, '--port', String(port)]);
    assert.match(installed, /Background service started/);
    assert.ok(!installed.includes('not healthy'));
    const password = installed.match(/^Password: (.+)$/m)?.[1];
    assert.ok(password);
    renameSync(bootstrap, `${bootstrap}-removed`);
    const status = run(launcher, []);
    assert.match(status, /Health: ok/);
    assert.match(status, /Startup: at (login|boot)/);
    assert.ok(!status.includes(password));
    const pid = run('systemctl', ['--user', 'show', unit(local, 'hub'), '--property=MainPID', '--value']).trim();
    assert.match(run(launcher, ['start']), /already running/);
    assert.equal(run('systemctl', ['--user', 'show', unit(local, 'hub'), '--property=MainPID', '--value']).trim(), pid);
    assert.match(run(launcher, ['install', '--no-start']), /Installed; stopped|Installed; running/);
    // --no-start leaves an existing service's activity unchanged.
    assert.equal(run('systemctl', ['--user', 'show', unit(local, 'hub'), '--property=MainPID', '--value']).trim(), pid);
    run(launcher, ['logs']);
    const serviceProperty = (component: 'hub' | 'collector', property: string) =>
      run('systemctl', ['--user', 'show', unit(local, component), `--property=${property}`, '--value']).trim();
    run(launcher, ['disable', 'collector']);
    assert.equal(serviceProperty('collector', 'ActiveState'), 'inactive');
    assert.equal(serviceProperty('collector', 'UnitFileState'), 'disabled');
    assert.equal(serviceProperty('hub', 'MainPID'), pid, 'disabling collection leaves the hub running');
    const startupBeforeStop = serviceProperty('hub', 'UnitFileState');
    run(launcher, ['stop', 'hub']);
    assert.equal(serviceProperty('hub', 'ActiveState'), 'inactive');
    assert.equal(serviceProperty('hub', 'UnitFileState'), startupBeforeStop, 'stop preserves startup');
    run(launcher, ['start', 'hub']);
    assert.equal(serviceProperty('collector', 'ActiveState'), 'inactive', 'starting the hub leaves collection disabled');
    const restartedHubPid = serviceProperty('hub', 'MainPID');
    run(launcher, ['enable', 'collector']);
    assert.equal(serviceProperty('collector', 'ActiveState'), 'active');
    assert.equal(serviceProperty('collector', 'UnitFileState'), 'enabled');
    assert.equal(serviceProperty('hub', 'MainPID'), restartedHubPid, 'enabling collection leaves the hub running');
    const auth = await loginBrowser(base, 'owner', password);
    const grantResponse = await fetch(`${base}/api/admin/collector-grants`, {method: 'POST', headers: {...auth.headers,
      Origin: base, 'Content-Type': 'application/json', 'X-CSRF-Token': auth.csrfToken}, body: JSON.stringify({ttlMs: 300000})});
    assert.equal(grantResponse.status, 201);
    const grant = await grantResponse.json() as {token: string};
    const token = join(root, 'grant');
    writeFileSync(token, grant.token, {mode: 0o600});
    const pending = run(launcher, ['install', '--state-dir', collector, '--role', 'collector']);
    assert.match(pending, /waiting for enrollment/);
    assert.notEqual(run('systemctl', ['--user', 'show', unit(collector, 'collector'), '--property=ActiveState', '--value']).trim(), 'active');
    const connected = run(launcher, ['connect', base, '--state-dir', collector, '--token-file', token]);
    assert.match(connected, /Background service started/);
    assert.ok(!connected.includes(grant.token));
    assert.equal(run('systemctl', ['--user', 'show', unit(collector, 'collector'), '--property=ActiveState', '--value']).trim(), 'active');
    run(launcher, ['uninstall', '--state-dir', collector]);
    run(launcher, ['uninstall']);
    assert.equal(existsSync(launcher), false);
    // Reproduce uninstall -> reinstall: the runtime tree is removed with the
    // service, so any explicit check must bootstrap again from the archive.
    const secondBootstrap = join(root, 'bootstrap2');
    run('npm', ['install', '--prefix', secondBootstrap, '--omit=dev', '--ignore-scripts', '--no-audit', '--no-fund', archive]);
    const durableCli = join(secondBootstrap, 'node_modules', 'rigscope', 'bin', 'rigscope.cjs');
    assert.throws(() => run(process.execPath, [durableCli, 'install', '--role', 'hub', '--origin', 'https://rig.example.com:8081', '--state-dir', local]), /Uninstall keeps configuration and history/);
    const beforeAccess = JSON.parse(readFileSync(join(local, 'config.json'), 'utf8'));
    run(process.execPath, [durableCli, 'configure', '--origin', 'https://rig.example.com:8081', '--state-dir', local]);
    assert.match(run(process.execPath, [durableCli, 'install', '--no-start', '--state-dir', local]), /Installed; stopped/);
    assert.ok(existsSync(launcher));
    const restarted = run(launcher, ['start']);
    assert.match(restarted, /Hub service started/);
    assert.match(restarted, /Collector service started/);
    const health = await new Promise<number>((resolve, reject) => {
      const req = request({socketPath: join(local, 'run', 'hub.sock'), path: '/api/health', headers: {Host: 'rig.example.com:8081'}}, res => {
        res.resume(); res.on('end', () => resolve(res.statusCode ?? 0));
      });
      req.on('error', reject);
      req.setTimeout(5000, () => req.destroy(new Error('converted hub did not respond')));
      req.end();
    });
    assert.equal(health, 200);
    const configBefore = readFileSync(join(local, 'config.json'), 'utf8');
    assert.deepEqual(JSON.parse(configBefore).users, beforeAccess.users);
    assert.deepEqual(JSON.parse(configBefore).collectors, beforeAccess.collectors);
    run(launcher, ['uninstall']);
    assert.equal(readFileSync(join(local, 'config.json'), 'utf8'), configBefore);
    assert.ok(existsSync(join(local, 'hub', 'telemetry.sqlite')));
    await expectPortClosed(port);
  } finally {
    // Also clean up a partially failed installation; never touch unrelated units.
    for (const dir of [local, collector]) {
      for (const component of COMPONENTS) {
        try { run('systemctl', ['--user', 'stop', unit(dir, component)], 15000); } catch { /* absent */ }
        try { run('systemctl', ['--user', 'disable', unit(dir, component)], 15000); } catch { /* absent */ }
        rmSync(unitPath(dir, component), {force: true});
      }
      rmSync(unitPath(dir, 'hub'), {force: true});
    }
    run('systemctl', ['--user', 'daemon-reload']);
    rmSync(root, {recursive: true, force: true});
  }
});
