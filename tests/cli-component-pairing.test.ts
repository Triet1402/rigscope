/** Real hub registry + enrollment + WebSocket, isolated from host services. */
import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { configureComponent, initialize, pairColocatedCollector } from '../src/cli/config';
import { startChild } from './runtime/support/process';
import { closedLoopbackPort } from './runtime/support/http';
import { wsUpgradeUnix } from './runtime/support/unix';
import { bindCollectorSession, wsTerminate, wsUpgrade } from './runtime/support/ws';
import { rawCollector } from './runtime/support/wire';
import type WebSocket from 'ws';

for (const remote of [false, true]) {
  test(`hub-first pairing reaches a live ${remote ? 'private socket' : 'loopback'} registry without restarting the hub`, {timeout: 20000}, async () => {
    const dir = mkdtempSync(join(tmpdir(), 'rigscope-pair-'));
    const port = remote ? undefined : await closedLoopbackPort();
    const origin = 'https://hub.example.test:8081';
    const socket = join(dir, 'run', 'hub.sock');
    initialize(dir, {role: 'hub', ...(remote ? {origin, socket} : {port: String(port)})});
    const hub = startChild('hub-first pairing', [join(process.cwd(), 'bin', 'rigscope.cjs'), 'run', 'hub', '--state-dir', dir], process.env);
    let ws: WebSocket | undefined;
    try {
      await hub.waitForLine(line => /hub listening/.test(line), 15000, 'hub listener');
      const leasePath = join(dir, 'hub', 'telemetry.sqlite.hub.lock');
      const lease = readFileSync(leasePath, 'utf8');
      await pairColocatedCollector(dir);
      configureComponent(dir, 'collector', {enabled: true});
      const state = JSON.parse(readFileSync(join(dir, 'collector', 'state.json'), 'utf8'));
      assert.equal(state.destinations.length, 1);
      const headers = {Authorization: `Bearer ${state.destinations[0].token}`};
      const upgrade = remote
        ? await wsUpgradeUnix(socket, '/api/collector/connect', new URL(origin).host, headers)
        : await wsUpgrade(state.destinations[0].url, headers);
      assert.equal(upgrade.kind, 'open', JSON.stringify(upgrade.kind === 'rejected' ? upgrade : {}));
      if (upgrade.kind !== 'open') return;
      ws = upgrade.ws;
      await bindCollectorSession(ws, rawCollector(state.collectorId, 'paired-machine'), 'new colocated collector');
      assert.equal(readFileSync(leasePath, 'utf8'), lease, 'the original hub process and lease survive pairing');
      assert.equal(hub.proc.exitCode, null);
    } finally {
      wsTerminate(ws);
      await hub.stop('SIGTERM', 8000);
      rmSync(dir, {recursive: true, force: true});
    }
  });
}
