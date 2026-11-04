import test from 'node:test';
import assert from 'node:assert/strict';
import { openHubDatabase } from '../src/hub/storage/db';
import { CollectorRegistry } from '../src/hub/enrollment/registry';
import { HubState } from '../src/hub/state';

const publicProof = 'synthetic-public-recovery-proof-1234567890';

test('enrollment success can be recovered only with saved proof; revoke invalidates proof', () => {
  const database = openHubDatabase(':memory:');
  try {
    const registry = new CollectorRegistry({ database, legacyIdentities: [] });
    const grant = registry.createGrant('owner');
    const request = { collectorId: 'new-installation', token: grant.token, requestId: 'saved-request', recoverySecret: publicProof };
    const first = registry.enroll(request);
    assert.deepEqual(registry.enroll(request), first, 'lost success reply recovers same credential');
    assert.throws(() => registry.enroll({ collectorId: request.collectorId, token: grant.token }));
    assert.throws(() => registry.enroll({ ...request, recoverySecret: publicProof + 'wrong' }));
    registry.revoke('owner', request.collectorId);
    assert.equal(registry.identities().length, 0);
    assert.throws(() => registry.enroll(request));
    assert.ok(!JSON.stringify(registry.list()).includes(first.token));
    assert.ok(!JSON.stringify(registry.auditEvents()).includes(grant.token));
  } finally { database.close(); }
});

test('explicit replacement rotates token, preserves machine mapping and removes active old credential', () => {
  const database = openHubDatabase(':memory:');
  try {
    const old = { collectorId: 'old-installation', machineId: 'original-machine', token: 'public-old-token' };
    const registry = new CollectorRegistry({ database, legacyIdentities: [old] });
    const state = new HubState({ collectors: registry.identities(), hubRunId: 'hub-run' });
    state.startSession({ sessionId: 'old-session', ...old, runId: 'old-run', bootId: 'boot', hostname: 'same-host', expectedIntervalMs: 5000 });
    const replacement = registry.createGrant('owner', { replaceMachineId: old.machineId });
    const next = registry.enroll({ collectorId: 'replacement-installation', token: replacement.token });
    state.synchronizeIdentities(registry.identities());
    assert.equal(next.machineId, old.machineId);
    assert.notEqual(next.token, old.token);
    assert.equal(state.currentSession('old-session'), null);
    assert.equal(state.identityByToken(old.token), null);
    assert.equal(state.identityByToken(next.token)?.collectorId, next.collectorId);
    assert.equal(state.snapshot().machines.length, 1);
    assert.equal(state.snapshot().machines[0].collectorId, next.collectorId);
    // Restart bootstrap never resurrects the old configured credential.
    const reopened = new CollectorRegistry({ database, legacyIdentities: [old] });
    assert.deepEqual(reopened.identities(), [next]);
  } finally { database.close(); }
});

test('rotation of the same installation requires explicit replacement grant', () => {
  const database = openHubDatabase(':memory:');
  try {
    const old = { collectorId: 'installation', machineId: 'machine', token: 'public-token' };
    const registry = new CollectorRegistry({ database, legacyIdentities: [old] });
    const grant = registry.createGrant('owner');
    assert.throws(() => registry.enroll({ collectorId: old.collectorId, token: grant.token }));
    const rotation = registry.createGrant('owner', { replaceMachineId: old.machineId });
    const next = registry.enroll({ collectorId: old.collectorId, token: rotation.token });
    assert.equal(next.machineId, old.machineId); assert.notEqual(next.token, old.token);
  } finally { database.close(); }
});

test('migration retains spent grant hashes and never makes old bootstrap grants claimable', () => {
  const database = openHubDatabase(':memory:');
  try {
    const { createHash } = require('node:crypto') as typeof import('node:crypto');
    const token = 'public-already-consumed-grant';
    const registry = new CollectorRegistry({ database, legacyIdentities: [],
      consumedGrantHashes: [createHash('sha256').update(token).digest('hex')],
      grants: [{ token, expiresAt: new Date(Date.now() + 600000).toISOString() }] });
    assert.throws(() => registry.enroll({ collectorId: 'another-installation', token }));
  } finally { database.close(); }
});

test('an empty unconfigured first start does not consume registry bootstrap', () => {
  const database = openHubDatabase(':memory:');
  try {
    assert.deepEqual(new CollectorRegistry({ database, legacyIdentities: [] }).identities(), []);
    const identity = { collectorId: 'first-configured', machineId: 'first-machine', token: 'public-bootstrap-token' };
    const configured = new CollectorRegistry({ database, legacyIdentities: [identity] });
    assert.deepEqual(configured.identities(), [identity]);
    configured.revoke('owner', identity.collectorId);
    assert.deepEqual(new CollectorRegistry({ database, legacyIdentities: [identity] }).identities(), []);
  } finally { database.close(); }
});

test('more than fifty enrollments persist, reload and revoke independently', () => {
  const database = openHubDatabase(':memory:');
  try {
    const registry = new CollectorRegistry({ database, legacyIdentities: [] });
    const issued = Array.from({ length: 51 }, (_, index) => {
      const grant = registry.createGrant('owner');
      return registry.enroll({ collectorId: `installation-${index}`, token: grant.token });
    });
    assert.equal(registry.identities().length, 51);
    assert.equal(new Set(issued.map((identity) => identity.machineId)).size, 51);
    assert.equal(registry.list().length, 51);

    // A restart restores every mapping and credential from the database.
    const restarted = new CollectorRegistry({ database, legacyIdentities: [] });
    assert.deepEqual(restarted.identities(), issued);

    // State accepts the larger registry and scopes revocation to one machine.
    const state = new HubState({ collectors: restarted.identities(), hubRunId: 'hub-run' });
    assert.equal(state.snapshot().machines.length, 51);
    const revoked = issued[10]!;
    const survivor = issued[11]!;
    state.startSession({ sessionId: 'revoked-session', ...revoked, runId: 'run', bootId: 'boot', hostname: 'revoked-host', expectedIntervalMs: 5000 });
    state.startSession({ sessionId: 'survivor-session', ...survivor, runId: 'run', bootId: 'boot', hostname: 'survivor-host', expectedIntervalMs: 5000 });
    restarted.revoke('owner', revoked.collectorId);
    state.synchronizeIdentities(restarted.identities());

    assert.equal(restarted.identities().length, 50);
    assert.equal(state.identityByToken(revoked.token), null);
    assert.equal(state.currentSession('revoked-session'), null);
    // A revoked mapping stops receiving credentials and loses its live
    // session; other machines in the larger registry remain fully isolated.
    assert.equal(state.snapshot().machines.find((machine) => machine.machineId === revoked.machineId)?.connected, false);
    assert.equal(state.identityByToken(survivor.token)?.collectorId, survivor.collectorId);
    assert.equal(state.snapshot().machines.some((machine) => machine.machineId === survivor.machineId), true);
  } finally { database.close(); }
});
