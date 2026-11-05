import test from 'node:test';
import assert from 'node:assert/strict';
import { WebSocket } from 'ws';
import { once } from 'node:events';
import { startHub } from './support/fixtures';
import { credential } from './support/config';
import { loginBrowser, BROWSER_PASSWORD } from './support/browser';

/** End-to-end admin changes use the packaged composition root and real cookies/sockets. */
test('packaged administration preserves scoped authorization and revokes existing access immediately', async t => {
  const creds = credential('admin-test-collector', 'admin-test-machine', 'public-admin-collector-token');
  const hub = await startHub(t, { collectors: [creds], stateFile: undefined,
    browserUsers: [{ username: 'owner', machineIds: '*', admin: true }, { username: 'viewer', machineIds: [creds.machineId] }] });
  const owner = await loginBrowser(hub.baseUrl, 'owner', BROWSER_PASSWORD);
  const viewer = await loginBrowser(hub.baseUrl, 'viewer', BROWSER_PASSWORD);
  const headers = { ...owner.headers, Origin: hub.baseUrl, 'X-CSRF-Token': owner.csrfToken, 'Content-Type': 'application/json' };
  assert.equal((await fetch(hub.baseUrl + '/api/admin/users', { headers: viewer.headers })).status, 403);
  assert.equal((await fetch(hub.baseUrl + '/api/admin/users', { headers: owner.headers })).status, 200);
  const socket = new WebSocket(hub.collectorWsUrl, { headers: { Authorization: `Bearer ${creds.token}` } });
  t.after(() => socket.terminate()); await once(socket, 'open');
  const welcome = once(socket, 'message');
  socket.send(JSON.stringify({ type: 'hello', collectorId: creds.collectorId, runId: 'admin-test-run', bootId: 'boot',
    hostname: 'synthetic-admin-test', capabilities: ['metrics'], expectedIntervalMs: 5000 }));
  await welcome;
  const revoked = once(socket, 'close');
  const revoke = await fetch(hub.baseUrl + '/api/admin/collectors', { method: 'POST', headers,
    body: JSON.stringify({ action: 'revoke', collectorId: creds.collectorId }) });
  assert.equal(revoke.status, 200); await revoked;
  const response = await fetch(hub.baseUrl + '/api/admin/collectors', { headers: owner.headers });
  const registry = await response.json() as { collectors: { revoked: boolean }[] };
  assert.equal(registry.collectors[0].revoked, true);
  assert.ok(!JSON.stringify(registry).includes(creds.token));
  const meta = await fetch(hub.baseUrl + '/api/machines/meta?machineId=' + creds.machineId, { method: 'POST', headers,
    body: JSON.stringify({ displayName: 'GPU rig', tags: ['gpu', 'home'] }) });
  assert.equal(meta.status, 200);
  // Changing scope must terminate the viewer's EXISTING cookie, not just future logins.
  const change = await fetch(hub.baseUrl + '/api/admin/users', { method: 'POST', headers,
    body: JSON.stringify({ username: 'viewer', machineIds: [], admin: false }) });
  assert.equal(change.status, 200);
  assert.equal((await fetch(hub.baseUrl + '/api/auth/session', { headers: viewer.headers })).status, 401);
  const audit = await fetch(hub.baseUrl + '/api/admin/audit', { headers: owner.headers });
  const events = await audit.json() as { events: { action: string }[] };
  assert.ok(events.events.some(e => e.action === 'revoke'));
  assert.ok(events.events.some(e => e.action === 'user.update'));
  assert.ok(!JSON.stringify(events).includes(creds.token));
  // Last-admin removal is refused and leaves the acting session usable.
  const remove = await fetch(hub.baseUrl + '/api/admin/users?username=owner', { method: 'DELETE', headers });
  assert.equal(remove.status, 409);
  assert.equal((await fetch(hub.baseUrl + '/api/auth/session', { headers: owner.headers })).status, 200);
});
