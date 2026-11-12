/** One sampling loop, independently owned Hub connections and expiring detail work. */
import { dirname, join } from 'node:path';
import { tmpdir } from 'node:os';
import { mkdtempSync, rmSync } from 'node:fs';
import { WebSocket } from 'ws';
import type { CollectorConfig } from './config';
import type { CollectorIdentity } from './identity';
import { OutboxBudget } from './destinations/budget';
import { HubConnection } from './destinations/connection';
import { createSequenceAllocator } from './sampling/sequence';
import { createMetricsSampler } from './sampling/metrics';
import { createSyntheticMetricsSampler } from './sampling/synthetic';
import { GpuPoller } from './sampling/gpu';
import { GpuTempsPoller, defaultReaders as pciReaders, discoverNvidiaDisplays, gpuTempsEnabled } from './sampling/gpu-temps';
import { RuntimePmGate } from './sampling/runtime-pm';
import { defaultReadTextFile } from './sampling/procfs';
import { ProcessScanner } from './processes/scan';
import { DetailScheduler } from './processes/scheduler';
import { acquireProcessLease } from '../src/runtime/process-lease';
import { APPLICATION_VERSION } from '../src/runtime/version';
import { writePrivateFileBuffered } from '../src/operations/fs';

/**
 * Readiness-marker rewrite period. `rigscope` commands accept a marker up to
 * 15 s stale, so 5 s keeps startup detection honest without a disk write per
 * sample.
 */
const HEALTH_WRITE_INTERVAL_MS = 5_000;

export async function runCollectorLoop(config: CollectorConfig, identity: CollectorIdentity,
  statePath: string, mock: boolean): Promise<never> {
  const temporary = mock && !process.env.COLLECTOR_STATE_FILE ? mkdtempSync(join(tmpdir(), 'rigscope-mock-')) : null;
  const stateDir = temporary ?? dirname(statePath);
  const releaseLease = acquireProcessLease(join(stateDir, 'collector-runtime.lock'));
  const sequence = createSequenceAllocator();
  const budget = new OutboxBudget();
  const logEvent = (line: string): void => {
    console.log(`collector ${line}`);
  };
  // The register-temps poller owns no timer: GpuPoller drives it per cycle.
  const gpuSensors = mock || !gpuTempsEnabled()
    ? null
    : new GpuTempsPoller({onEvent: logEvent});
  // One gate for every reader of a GPU, so the NVIDIA poller, DRM counters and
  // inventory agree on when an offload GPU must be left asleep.
  const power = mock ? null : new RuntimePmGate({read: defaultReadTextFile, onEvent: logEvent});
  const gpu = mock
    ? null
    : new GpuPoller({
      intervalMs: Math.max(2000, config.intervalMs),
      onEvent: logEvent,
      sensorSource: gpuSensors,
      power: {gate: power!, devices: () => discoverNvidiaDisplays(pciReaders).map(device => device.bdf)},
    });
  const sampler = mock ? createSyntheticMetricsSampler(identity, { sequence }) : createMetricsSampler(identity, { sequence, gpu, power });
  const connections: HubConnection[] = [];
  try {
    for (const [index, endpoint] of config.destinations.entries()) {
      connections.push(HubConnection.open({
        index, endpoint, stateDir, baseIntervalMs: config.intervalMs, identity, queueMode: config.queueMode,
        budget, allocateSequence: () => sequence.next(), sampler,
        socketFactory: (url, options) => new WebSocket(url, { headers: options.headers,
          handshakeTimeout: options.handshakeTimeoutMs, maxPayload: options.maxPayloadBytes, perMessageDeflate: false }),
        onEvent: logEvent,
      }));
    }
  } catch (error) {
    for (const connection of connections) connection.stop();
    releaseLease();
    throw error;
  }
  const scheduler = new DetailScheduler({
    scanner: mock ? { scan: async () => ({ totalProcesses: 0, inaccessible: 0, truncated: false, processes: [] }) }
      : new ProcessScanner({ gpuMemoryByPid: () => gpu?.available() === 'yes' ? gpu.computeApps() : null }),
    policyFor: index => connections[index]!.detailPolicy(),
    send: (index, message) => connections[index]!.sendDetail(message),
  });
  connections.forEach((connection, index) => connection.attachDetails({
    handle: message => scheduler.handle(index, message),
    connectionDisconnected: () => scheduler.cancelDestination(index),
  }));
  let stopped = false;
  let lastHealthWrite = 0;
  const sample = (): void => {
    try {
      const record = sampler.next();
      const at = performance.now();
      // Every connection independently owns filtering, durable queueing,
      // delivery, inventory refresh and its own failure isolation.
      for (const connection of connections) connection.acceptSample(record, at);
      const healthFile = process.env.COLLECTOR_HEALTH_FILE;
      if (healthFile && Date.now() - lastHealthWrite >= HEALTH_WRITE_INTERVAL_MS) {
        // A missed heartbeat is recovered by the next tick, so this is written
        // without fsync; the same reasoning is why the RAM queue is the default.
        writePrivateFileBuffered(healthFile, JSON.stringify({version: APPLICATION_VERSION,
          pid: process.pid, sampledAt: Date.now()}));
        lastHealthWrite = Date.now();
      }
    } catch { console.error('collector: metrics sampling failed; next cadence will retry'); }
  };
  gpu?.start();
  sample();
  const sampleTimer = setInterval(sample, config.intervalMs);
  const statusTimer = setInterval(() => {
    for (const connection of connections) connection.refreshStatus();
  }, 5000);
  const stop = (): void => {
    if (stopped) return;
    stopped = true;
    clearInterval(sampleTimer); clearInterval(statusTimer);
    scheduler.stop(); gpu?.stop();
    for (const connection of connections) connection.stop();
    releaseLease();
    if (temporary) rmSync(temporary, { recursive: true, force: true });
    // The two modes differ exactly here, so the shutdown line must not claim a
    // durability the default mode does not provide.
    console.log(config.queueMode === 'durable'
      ? 'collector stopping; durable pending delivery saved'
      : 'collector stopping; unacknowledged samples in RAM are discarded');
    setTimeout(() => process.exit(0), 250).unref();
    setTimeout(() => process.exit(0), 3000).unref();
  };
  process.on('SIGTERM', stop); process.on('SIGINT', stop);
  for (const connection of connections) connection.start();
  return await new Promise<never>(() => {});
}
