/**
 * Mixed-version collector protocol compatibility.
 *
 * Exercises the real collector WebSocket gateway so the welcome wire shape is
 * verified end-to-end: legacy collectors must keep the exact pre-extension
 * welcome, while an exact known extended protocol version opts in to the
 * optional metric-extension list. The hub parser must then accept the extended
 * sample fields and acknowledge the original record refs.
 */
import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import { WebSocket } from 'ws';

import { CollectorGateway } from '../src/hub/collector-ws';
import { HubState } from '../src/hub/state';
import type { SampleIngestion } from '../src/hub/sample-ingestion';
import {
  COLLECTOR_PROTOCOL_VERSION,
  METRIC_EXTENSION_NAMES,
  parseHubMessage,
  type MetricsRecord,
  type TelemetryRecord,
  type WelcomeMessage,
} from '../src/contracts';

const OLD_WELCOME_KEYS = [
  'delivery',
  'hubRunId',
  'machineId',
  'maxInFlightBatches',
  'type',
].sort();

type CapturedBatch = { mode: 'live' | 'replay'; records: TelemetryRecord[] };

function fakeIngestion(captured: CapturedBatch[]): SampleIngestion {
  return {
    acceptInventory: () => {},
    submitSamples: (_sessionId: string, mode: 'live' | 'replay', records: TelemetryRecord[]) => {
      captured.push({ mode, records });
      return {
        status: 'queued' as const,
        committed: Promise.resolve({
          status: 'committed' as const,
          accepted: records.map(({ runId, sequence }) => ({ runId, sequence })),
          rejected: [],
        }),
      };
    },
  } as unknown as SampleIngestion;
}

async function fixture(t: test.TestContext, maxConnections?: number) {
  const state = new HubState({
    hubRunId: 'compat-hub-run',
    collectors: [
      { collectorId: 'old-collector', machineId: 'old-machine', token: 'old-token' },
      { collectorId: 'new-collector', machineId: 'new-machine', token: 'new-token' },
      { collectorId: 'unknown-collector', machineId: 'unknown-machine', token: 'unknown-token' },
    ],
  });
  const captured: CapturedBatch[] = [];
  const gateway = new CollectorGateway({ state, ingestion: fakeIngestion(captured), maxConnections });
  const server = createServer((_req, res) => {
    res.writeHead(426, { Connection: 'close' });
    res.end('upgrade required');
  });
  server.on('upgrade', (req, socket, head) => gateway.handleUpgrade(req, socket, head));
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const port = (server.address() as { port: number }).port;
  t.after(async () => {
    gateway.stop();
    server.closeAllConnections();
    await new Promise<void>((resolve) => server.close(() => resolve()));
  });
  return { state, gateway, port, captured };
}

test('collector connection budget can exceed one hundred while still rejecting excess sockets', async t => {
  const {port} = await fixture(t, 101);
  const url = `ws://127.0.0.1:${port}/api/collector/connect`;
  const headers = {Authorization: 'Bearer new-token'};
  const opened: WebSocket[] = [];
  t.after(() => opened.forEach(ws => ws.terminate()));
  await Promise.all(Array.from({length: 101}, async () => {
    const ws = new WebSocket(url, {headers});
    opened.push(ws);
    await once(ws, 'open');
  }));
  assert.equal(opened.filter(ws => ws.readyState === WebSocket.OPEN).length, 101);
  const rejected = new WebSocket(url, {headers});
  rejected.on('error', () => {});
  t.after(() => rejected.terminate());
  const status = await new Promise<number>(resolve => rejected.once('unexpected-response', (_req, res) => {
    res.resume();
    resolve(res.statusCode!);
  }));
  assert.equal(status, 503);
});

type Hello = {
  collectorId: string;
  runId: string;
  bootId: string;
  hostname: string;
  capabilities: string[];
  expectedIntervalMs: number;
  collectorVersion: string;
};

async function handshake(port: number, token: string, hello: Hello) {
  const ws = new WebSocket(`ws://127.0.0.1:${port}/api/collector/connect`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  await once(ws, 'open');
  const welcome = once(ws, 'message');
  ws.send(JSON.stringify({ type: 'hello', ...hello }));
  const [data] = await welcome;
  const raw = JSON.parse(String(data)) as Record<string, unknown>;
  const parsed = parseHubMessage(raw);
  assert.equal(parsed.type, 'welcome');
  return { ws, raw, welcome: parsed as WelcomeMessage };
}

function extendedRecord(): MetricsRecord {
  return {
    collectorId: 'new-collector',
    runId: 'new-run',
    bootId: 'new-boot',
    sequence: 1,
    capturedAt: new Date().toISOString(),
    kind: 'metrics',
    payload: {
      cpu: {
        status: 'ok',
        usagePercent: 12.5,
        perCore: [10],
        temperatureC: 61.4,
        powerWatts: 88.2,
        iowaitPercent: 1.25,
        pressureSomePercent: 3.5,
      },
      memory: {
        status: 'ok',
        usedBytes: 1_000,
        totalBytes: 8_000,
        pressureSomePercent: 1.5,
        pressureFullPercent: 0.5,
      },
      swap: { status: 'unavailable', reason: 'no swap' },
      gpus: { status: 'ok', devices: [] },
      disks: {
        status: 'ok',
        filesystems: [
          { id: 'fs:/', status: 'ok', usedBytes: 100, totalBytes: 200, inodesUsed: 10, inodesTotal: 100 },
        ],
        devices: [
          {
            id: 'disk:sda',
            status: 'ok',
            readBytesPerSec: 1,
            writeBytesPerSec: 2,
            readLatencyMs: 1.5,
            writeLatencyMs: 2.5,
            ioUtilizationPercent: 12.5,
          },
        ],
        pressureSomePercent: 2.5,
        pressureFullPercent: 1.5,
      },
      network: { status: 'ok', interfaces: [] },
    },
  };
}

test('an old collector hello receives the exact pre-extension welcome shape', async (t) => {
  const f = await fixture(t);
  const { ws, raw } = await handshake(f.port, 'old-token', {
    collectorId: 'old-collector',
    runId: 'old-run',
    bootId: 'old-boot',
    hostname: 'old-host',
    capabilities: ['metrics'],
    expectedIntervalMs: 5_000,
    collectorVersion: '0.2.0',
  });
  assert.deepEqual(Object.keys(raw).sort(), OLD_WELCOME_KEYS);
  assert.equal(Object.hasOwn(raw, 'metricExtensions'), false);
  ws.close();
});

test('only the exact extended protocol version opts in to metric extensions', async (t) => {
  const f = await fixture(t);
  const { ws, raw } = await handshake(f.port, 'new-token', {
    collectorId: 'new-collector',
    runId: 'new-run',
    bootId: 'new-boot',
    hostname: 'new-host',
    capabilities: ['metrics'],
    expectedIntervalMs: 5_000,
    collectorVersion: COLLECTOR_PROTOCOL_VERSION,
  });
  assert.deepEqual(raw.metricExtensions, [...METRIC_EXTENSION_NAMES]);
  assert.deepEqual(Object.keys(raw).sort(), [...OLD_WELCOME_KEYS, 'metricExtensions'].sort());
  ws.close();
});

test('an unknown or near-match collector version keeps the old welcome shape', async (t) => {
  const f = await fixture(t);
  for (const collectorVersion of ['0.3.0-beta', '0.4.0', '9.9.9']) {
    const { ws, raw } = await handshake(f.port, 'unknown-token', {
      collectorId: 'unknown-collector',
      runId: `unknown-run-${collectorVersion}`,
      bootId: 'unknown-boot',
      hostname: 'unknown-host',
      capabilities: ['metrics'],
      expectedIntervalMs: 5_000,
      collectorVersion,
    });
    assert.deepEqual(Object.keys(raw).sort(), OLD_WELCOME_KEYS, `no guessing for ${collectorVersion}`);
    ws.close();
    await once(ws, 'close');
  }
});

test('an upgraded hub accepts extended samples and acknowledges original refs', async (t) => {
  const f = await fixture(t);
  const { ws } = await handshake(f.port, 'new-token', {
    collectorId: 'new-collector',
    runId: 'new-run',
    bootId: 'new-boot',
    hostname: 'new-host',
    capabilities: ['metrics'],
    expectedIntervalMs: 5_000,
    collectorVersion: COLLECTOR_PROTOCOL_VERSION,
  });

  const record = extendedRecord();
  const ack = once(ws, 'message');
  ws.send(JSON.stringify({ type: 'samples', mode: 'live', records: [record] }));
  const parsed = parseHubMessage(JSON.parse(String((await ack)[0])));
  assert.equal(parsed.type, 'ack');
  if (parsed.type !== 'ack') return;
  assert.deepEqual(parsed.accepted, [{ runId: record.runId, sequence: record.sequence }]);
  assert.deepEqual(parsed.rejected, []);

  assert.equal(f.captured.length, 1);
  assert.equal(f.captured[0]!.mode, 'live');
  assert.deepEqual(f.captured[0]!.records, [record], 'extended fields survive hub parsing');
  ws.close();
});
