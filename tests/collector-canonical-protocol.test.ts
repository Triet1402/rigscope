/**
 * Canonical collector protocol behavior.
 *
 * There is exactly one unversioned hello/welcome exchange: hello carries no
 * protocol or collector version, welcome carries no metric-extension list,
 * and every current metric field is canonical on the wire. These tests drive
 * the real collector WebSocket gateway end-to-end and verify that the single
 * canonical shapes are exchanged, that the full metric payload survives the
 * hub parser unchanged, and that obsolete compatibility fields are rejected
 * rather than negotiated, projected, or silently stripped.
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
  parseHubMessage,
  type MetricsRecord,
  type TelemetryRecord,
  type WelcomeMessage,
} from '../src/contracts';

const WELCOME_KEYS = [
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
    hubRunId: 'canonical-hub-run',
    collectors: [
      { collectorId: 'canonical-collector', machineId: 'canonical-machine', token: 'canonical-token' },
      { collectorId: 'rejected-collector', machineId: 'rejected-machine', token: 'rejected-token' },
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
  const headers = {Authorization: 'Bearer canonical-token'};
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

function canonicalRecord(): MetricsRecord {
  return {
    collectorId: 'canonical-collector',
    runId: 'canonical-run',
    bootId: 'canonical-boot',
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

test('a canonical hello receives exactly the unversioned welcome shape', async (t) => {
  const f = await fixture(t);
  const { ws, raw, welcome } = await handshake(f.port, 'canonical-token', {
    collectorId: 'canonical-collector',
    runId: 'canonical-run',
    bootId: 'canonical-boot',
    hostname: 'canonical-host',
    capabilities: ['metrics'],
    expectedIntervalMs: 5_000,
  });
  assert.deepEqual(Object.keys(raw).sort(), WELCOME_KEYS);
  assert.equal(Object.hasOwn(raw, 'metricExtensions'), false);
  assert.deepEqual(welcome, raw);
  ws.close();
  await once(ws, 'close');
});

test('a hello that still carries collectorVersion is rejected', async (t) => {
  const f = await fixture(t);
  const ws = new WebSocket(`ws://127.0.0.1:${f.port}/api/collector/connect`, {
    headers: { Authorization: 'Bearer rejected-token' },
  });
  await once(ws, 'open');
  const closed = once(ws, 'close');
  ws.send(JSON.stringify({
    type: 'hello',
    collectorId: 'rejected-collector',
    runId: 'rejected-run',
    bootId: 'rejected-boot',
    hostname: 'rejected-host',
    capabilities: ['metrics'],
    expectedIntervalMs: 5_000,
    collectorVersion: '0.3.0',
  }));
  const [code, reason] = await closed;
  assert.equal(code, 4003);
  assert.match(String(reason), /collectorVersion/);
});

test('a welcome that still carries metricExtensions is rejected by the strict parser', () => {
  assert.throws(
    () => parseHubMessage({
      type: 'welcome',
      hubRunId: 'canonical-hub-run',
      machineId: 'canonical-machine',
      delivery: 'durable',
      maxInFlightBatches: 4,
      metricExtensions: ['bottleneck-v1'],
    }),
    /unexpected field "metricExtensions"/,
  );
});

test('the hub accepts the canonical full metric payload and acknowledges original refs', async (t) => {
  const f = await fixture(t);
  const { ws } = await handshake(f.port, 'canonical-token', {
    collectorId: 'canonical-collector',
    runId: 'canonical-run',
    bootId: 'canonical-boot',
    hostname: 'canonical-host',
    capabilities: ['metrics'],
    expectedIntervalMs: 5_000,
  });

  const record = canonicalRecord();
  const ack = once(ws, 'message');
  ws.send(JSON.stringify({ type: 'samples', mode: 'live', records: [record] }));
  const parsed = parseHubMessage(JSON.parse(String((await ack)[0])));
  assert.equal(parsed.type, 'ack');
  if (parsed.type !== 'ack') return;
  assert.deepEqual(parsed.accepted, [{ runId: record.runId, sequence: record.sequence }]);
  assert.deepEqual(parsed.rejected, []);

  assert.equal(f.captured.length, 1);
  assert.equal(f.captured[0]!.mode, 'live');
  assert.deepEqual(f.captured[0]!.records, [record], 'full canonical fields survive hub parsing unchanged');
  ws.close();
  await once(ws, 'close');
});

test('optional metric attributes stay optional for hardware that cannot report them', async (t) => {
  const f = await fixture(t);
  const { ws } = await handshake(f.port, 'canonical-token', {
    collectorId: 'canonical-collector',
    runId: 'canonical-run',
    bootId: 'canonical-boot',
    hostname: 'canonical-host',
    capabilities: ['metrics'],
    expectedIntervalMs: 5_000,
  });

  const record: MetricsRecord = {
    ...canonicalRecord(),
    payload: {
      cpu: { status: 'ok', usagePercent: 4, perCore: [4] },
      memory: { status: 'ok', usedBytes: 1_000, totalBytes: 8_000 },
      swap: { status: 'ok', usedBytes: 0, totalBytes: 0 },
      gpus: { status: 'unavailable', reason: 'no GPUs detected' },
      disks: { status: 'ok', filesystems: [], devices: [] },
      network: { status: 'ok', interfaces: [] },
    },
  };
  const ack = once(ws, 'message');
  ws.send(JSON.stringify({ type: 'samples', mode: 'live', records: [record] }));
  const parsed = parseHubMessage(JSON.parse(String((await ack)[0])));
  assert.equal(parsed.type, 'ack');
  if (parsed.type !== 'ack') return;
  assert.deepEqual(parsed.accepted, [{ runId: record.runId, sequence: record.sequence }]);
  assert.deepEqual(f.captured[0]!.records, [record], 'unavailable hardware is not upgraded or rejected');
  ws.close();
  await once(ws, 'close');
});
