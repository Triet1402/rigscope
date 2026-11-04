/** Isolated packaged CLI smoke: actual auth, real collection, enrollment and shutdown. */
import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { hashPassword } from '../src/hub/auth/password';
import { initialize, readSetup } from '../src/cli/config';
import { startChild } from './runtime/support/process';
import { closedLoopbackPort, expectPortClosed } from './runtime/support/http';
import { loginBrowser } from './runtime/support/browser';
import { SseClient } from './runtime/support/sse';

const cli = join(process.cwd(), 'bin', 'rigscope.cjs');
test('bare CLI shows status without creating state, starting a server, or waiting for a terminal', () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-status-'));
  const state = join(root, 'not-created');
  try {
    const output = execFileSync(process.execPath, [cli, '--state-dir', state], {encoding: 'utf8', timeout: 5000});
    assert.match(output, /No user installation/);
    assert.match(output, /rigscope install --boot/);
    assert.equal(existsSync(state), false);
    assert.ok(!output.includes('Password:'));
  } finally { rmSync(root, {recursive: true, force: true}); }
});

test('status reports components from a version 1 config without rewriting it', () => {
  const root = mkdtempSync(join(tmpdir(), 'rigscope-status-v1-'));
  const dir = join(root, 'state');
  try {
    mkdirSync(dir, {mode: 0o700});
    const legacy = {version: 1, role: 'local', port: 9138,
      users: [{username: 'owner', passwordHash: hashPassword('retained-password'), machineIds: '*', admin: true}],
      collectors: []};
    const raw = `${JSON.stringify(legacy, null, 2)}\n`;
    writeFileSync(join(dir, 'config.json'), raw, {mode: 0o600});
    const output = execFileSync(process.execPath, [cli, 'status', '--state-dir', dir], {
      encoding: 'utf8', timeout: 5000, env: {...process.env, XDG_CONFIG_HOME: join(root, 'config')},
    });
    assert.match(output, /Components:/);
    assert.match(output, /Hub: enabled/);
    assert.match(output, /Collector: enabled/);
    assert.match(output, /Destinations: 0\/8 \(awaiting connection\)/);
    assert.equal(readFileSync(join(dir, 'config.json'), 'utf8'), raw);
  } finally { rmSync(root, {recursive: true, force: true}); }
});

test('CLI local trial authenticates, streams real data, joins another collector, and shuts down both roles', {timeout: 30000}, async () => {
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-cli-'));
  const port = await closedLoopbackPort();
  const base = `http://127.0.0.1:${port}`;
  const child = startChild('rigscope local', [cli, 'try', '--state-dir', join(dir, 'local'), '--port', String(port)], process.env);
  let stream: SseClient | undefined;
  try {
    const passwordLine = await child.waitForLine(line => line.startsWith('Password: '), 5000, 'generated password');
    await child.waitForLine(line => /hub listening/.test(line), 15000, 'hub listener');
    const password = passwordLine.slice('Password: '.length);
    const auth = await loginBrowser(base, 'owner', password);
    assert.equal((await fetch(`${base}/login`)).status, 200);
    assert.match(await (await fetch(`${base}/login`)).text(), /Rigscope/);
    stream = new SseClient(`${base}/api/live`, auth.headers);
    let observed = false;
    for (let i = 0; i < 12; i++) {
      const event = await stream.next(5000);
      if (event.data.includes('"status":"ok"') && event.data.includes('"memory"')) { observed = true; break; }
    }
    assert.equal(observed, true, 'real collector data reaches the browser stream');
    const response = await fetch(`${base}/api/admin/collector-grants`, {method: 'POST',
      headers: {...auth.headers, Origin: base, 'Content-Type': 'application/json', 'X-CSRF-Token': auth.csrfToken}, body: JSON.stringify({ttlMs: 300000})});
    assert.equal(response.status, 201);
    const grant = await response.json() as {token: string};
    const tokenFile = join(dir, 'grant');
    writeFileSync(tokenFile, grant.token, {mode: 0o600});
    // Connecting also works in an installation that started as hub-only.
    const remoteSetup = initialize(join(dir, 'remote'), {role: 'hub'});
    const connect = execFileSync(process.execPath, [cli, 'connect', base, '--state-dir', join(dir, 'remote'), '--token-file', tokenFile], {encoding: 'utf8', timeout: 10000});
    assert.match(connect, /Hub connected/);
    assert.ok(!connect.includes(grant.token));
    const remote = JSON.parse(readFileSync(join(dir, 'remote', 'collector', 'state.json'), 'utf8'));
    assert.equal(remote.destinations.length, 1);
    const connectedConfig = readSetup(join(dir, 'remote'))!;
    assert.equal(connectedConfig.components?.collector.enabled, true);
    assert.deepEqual(connectedConfig.users, remoteSetup.config.users, 'remote enrollment keeps the existing hub owner');
    assert.equal(remote.destinations[0].url, `ws://127.0.0.1:${port}/api/collector/connect`);
    assert.notEqual(remote.destinations[0].token, grant.token);
    const status = execFileSync(process.execPath, [cli, 'status', '--state-dir', join(dir, 'remote')], {encoding: 'utf8'});
    assert.match(status, /Destinations: 1\/8/);
    assert.ok(!status.includes(remote.destinations[0].token));
    const configBefore = readFileSync(join(dir, 'local', 'config.json'), 'utf8');
    stream.close();
    stream = undefined;
    const exit = await child.stop('SIGTERM', 9000);
    assert.equal(exit.code, 0, child.dump('supervisor shutdown failed'));
    await expectPortClosed(port);
    assert.equal(readFileSync(join(dir, 'local', 'config.json'), 'utf8'), configBefore);
  } finally { stream?.close(); await child.stop('SIGTERM', 9000); rmSync(dir, {recursive: true, force: true}); }
});
