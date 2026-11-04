/**
 * Component CLI fixtures. systemctl/loginctl/journalctl are tiny local stubs,
 * so no user services are installed and no host state is touched.
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import { execFile, execFileSync, spawnSync } from 'node:child_process';
import { createServer } from 'node:http';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';
import { collectorHasDestinations, initialize, readSetup } from '../src/cli/config';
import { unitName } from '../src/cli/services';

const cli = join(process.cwd(), 'bin', 'rigscope.cjs');

function stubs(root: string, active = true): NodeJS.ProcessEnv {
  const bin = join(root, 'bin-tools');
  mkdirSync(bin, {recursive: true});
  const systemctlLog = join(root, 'systemctl.log');
  const journalLog = join(root, 'journal.log');
  writeFileSync(systemctlLog, '');
  writeFileSync(journalLog, '');
  writeFileSync(join(bin, 'systemctl'), `#!/bin/sh
printf '%s\\n' "$*" >> "${systemctlLog}"
case " $* " in
  *" show "*) printf 'ActiveState=${active ? 'active' : 'inactive'}\\nSubState=running\\nExecMainPID=4242\\nUnitFileState=enabled\\n' ;;
  *" is-enabled "*) printf 'enabled\\n' ;;
esac
exit 0
`, {mode: 0o755});
  writeFileSync(join(bin, 'loginctl'), `#!/bin/sh
printf 'no\\n'
`, {mode: 0o755});
  writeFileSync(join(bin, 'journalctl'), `#!/bin/sh
printf '%s\\n' "$*" >> "${journalLog}"
`, {mode: 0o755});
  return {
    ...process.env,
    PATH: `${bin}:${process.env.PATH ?? ''}`,
    XDG_CONFIG_HOME: join(root, 'config'),
    XDG_BIN_HOME: join(root, 'install-bin'),
    RIGSCOPE_STATE_DIR: '',
  };
}

test('CLI enables, disables, starts, stops, restarts and reads logs per component', () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-components-'));
  const dir = join(root, 'state');
  const env = stubs(root);
  delete env.RIGSCOPE_STATE_DIR;
  const run = (args: string[]) => execFileSync(process.execPath, [cli, ...args, '--state-dir', dir], {env, encoding: 'utf8', timeout: 10000});
  const unit = (component: 'hub' | 'collector') => join(root, 'config', 'systemd', 'user', unitName(dir, component));
  const systemctl = () => readFileSync(join(root, 'systemctl.log'), 'utf8');
  const journal = () => readFileSync(join(root, 'journal.log'), 'utf8');
  try {
    const hub = initialize(dir, {role: 'hub', port: '9141'});
    assert.ok(hub.password);
    mkdirSync(join(dir, 'runtime'), {recursive: true});
    const accessChange = spawnSync(process.execPath, [cli, 'enable', 'hub', '--no-start', '--origin', 'https://rig.example.com', '--state-dir', dir], {env, encoding: 'utf8'});
    assert.equal(accessChange.status, 1);
    assert.match(accessChange.stderr, /rigscope configure/);
    assert.equal(readSetup(dir)!.origin, undefined);

    const enabledHub = run(['enable', 'hub', '--no-start']);
    assert.match(enabledHub, /Hub enabled/);
    assert.ok(!enabledHub.includes('Password:'), 'the owner password is created once');
    assert.ok(existsSync(unit('hub')));
    assert.match(readFileSync(unit('hub'), 'utf8'), /run hub --state-dir/);

    const enabledCollector = run(['enable', 'collector', '--no-start']);
    assert.match(enabledCollector, /Collector enabled/);
    assert.ok(!enabledCollector.includes('Password:'));
    assert.match(readFileSync(unit('collector'), 'utf8'), /run collector --state-dir/);
    const both = readSetup(dir)!;
    assert.equal(both.components?.hub.enabled, true);
    assert.equal(both.components?.collector.enabled, true);
    assert.equal(both.users.length, 1);
    assert.equal(JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8')).destinations.length, 1);
    assert.match(systemctl(), /enable .*hub\.service/);
    assert.match(systemctl(), /enable .*collector\.service/);

    const disabled = run(['disable', 'collector']);
    assert.match(disabled, /Collector disabled/);
    const retained = readSetup(dir)!;
    assert.equal(retained.components?.collector.enabled, false);
    assert.deepEqual(retained.users, both.users);
    assert.deepEqual(retained.collectors, both.collectors);
    assert.ok(existsSync(unit('collector')), 'disabling keeps the unit available for re-enable');
    assert.equal(JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8')).destinations.length, 1);

    const restartMark = systemctl().length;
    const restarted = run(['restart']);
    assert.match(restarted, /Hub service restarted/);
    assert.ok(!restarted.includes('Collector'));
    assert.match(systemctl().slice(restartMark), /restart .*hub\.service/);
    assert.ok(!systemctl().slice(restartMark).includes('collector.service'));

    const logMark = journal().length;
    run(['logs']);
    assert.match(journal().slice(logMark), /--unit .*hub\.service/);
    assert.ok(!journal().slice(logMark).includes('collector.service'));

    const stopMark = systemctl().length;
    const stopped = run(['stop']);
    assert.match(stopped, /Hub service stopped/);
    assert.ok(!stopped.includes('Collector'));
    assert.match(systemctl().slice(stopMark), /stop .*hub\.service/);
    assert.ok(!systemctl().slice(stopMark).includes('collector.service'));

    const reenabled = run(['enable', 'collector', '--no-start']);
    assert.match(reenabled, /Collector enabled/);
    assert.ok(!reenabled.includes('Password:'));
    const state = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
    assert.equal(state.destinations.length, 1, 're-enabling does not duplicate the colocated destination');

    const disabledHub = run(['disable', 'hub']);
    assert.match(disabledHub, /Hub disabled/);
    const afterHub = readSetup(dir)!;
    assert.equal(afterHub.components?.hub.enabled, false);
    assert.deepEqual(afterHub.users, both.users);
    assert.deepEqual(afterHub.collectors, both.collectors);
    assert.ok(existsSync(unit('hub')));

    const startMark = systemctl().length;
    const started = run(['start']);
    assert.match(started, /Collector service already running/);
    assert.ok(!started.includes('Hub'));
    assert.ok(!systemctl().slice(startMark).includes('hub.service'));

    const componentLogMark = journal().length;
    run(['logs', 'collector']);
    assert.match(journal().slice(componentLogMark), /--unit .*collector\.service/);
  } finally { rmSync(root, {recursive: true, force: true}); }
});

test('connect keeps the collector disabled when enrollment fails', () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-connect-fail-'));
  const dir = join(root, 'state');
  const env = stubs(root);
  delete env.RIGSCOPE_STATE_DIR;
  try {
    initialize(dir, {role: 'hub', port: '9143'});
    const token = join(root, 'grant');
    writeFileSync(token, 'one-time-grant', {mode: 0o600});
    const result = spawnSync(process.execPath, [cli, 'connect', 'http://127.0.0.1:1', '--token-file', token, '--state-dir', dir],
      {env, encoding: 'utf8', timeout: 10000});
    assert.equal(result.status, 1);
    const config = readSetup(dir)!;
    assert.equal(config.components?.collector.enabled, false, 'a rejected grant must not enable the collector');
    assert.equal(collectorHasDestinations(dir), false);

    // A live collector is refused before any configuration or enrollment work.
    mkdirSync(join(root, 'config', 'systemd', 'user'), {recursive: true});
    writeFileSync(join(root, 'config', 'systemd', 'user', unitName(dir, 'collector')), 'fixture');
    const busy = spawnSync(process.execPath, [cli, 'connect', 'http://127.0.0.1:1', '--token-file', token, '--state-dir', dir],
      {env, encoding: 'utf8', timeout: 10000});
    assert.equal(busy.status, 1);
    assert.match(busy.stderr, /stop collector/);
    assert.equal(readSetup(dir)!.components?.collector.enabled, false);
  } finally { rmSync(root, {recursive: true, force: true}); }
});

test('successful connect enables the collector service for startup without touching the hub', async () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-connect-startup-'));
  const dir = join(root, 'state');
  const env = stubs(root, false);
  delete env.RIGSCOPE_STATE_DIR;
  const server = createServer((req, res) => {
    let body = '';
    req.on('data', chunk => { body += chunk; });
    req.on('end', () => {
      res.writeHead(201, {'Content-Type': 'application/json'});
      res.end(JSON.stringify({collectorId: JSON.parse(body).collectorId, machineId: 'connected-machine', token: 'collector-credential'}));
    });
  });
  try {
    await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
    const address = server.address();
    assert.ok(address && typeof address === 'object');
    const initial = initialize(dir, {role: 'hub'});
    mkdirSync(join(dir, 'runtime'));
    const token = join(root, 'grant');
    writeFileSync(token, 'one-use-grant', {mode: 0o600});
    await promisify(execFile)(process.execPath, [cli, 'connect', `http://127.0.0.1:${address.port}`,
      '--token-file', token, '--no-start', '--state-dir', dir], {env, timeout: 10000});
    assert.equal(readSetup(dir)!.components?.collector.enabled, true);
    assert.deepEqual(readSetup(dir)!.users, initial.config.users);
    const log = readFileSync(join(root, 'systemctl.log'), 'utf8');
    assert.match(log, new RegExp(`enable ${unitName(dir, 'collector').replaceAll('.', '\\.')}`));
    assert.doesNotMatch(log, /(?:start|stop|restart|enable|disable) .*hub\.service/);
    assert.doesNotMatch(log, /(?:start|restart) .*collector\.service/, '--no-start is respected');
  } finally {
    await new Promise<void>(resolve => server.close(() => resolve()));
    rmSync(root, {recursive: true, force: true});
  }
});
