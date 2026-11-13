import {createSyntheticMetricsSampler} from './synthetic';
import {flattenMetrics} from './flatten';
import type {MachineState, DetailResult, ProcessRow} from '../../../src/contracts';
import type {HistoryResult, HistorySeries} from '../../../src/history/contract';
import {rangePreset, type RangeId} from '../../../src/history/presentation';
import {metricOption} from '../../../src/history/metrics';

const GiB = 1024 ** 3;
export const MACHINE_IDS = ['workstation', 'build-server'] as const;

/** Public, fabricated machines. No local host discovery or real hub access. */
export function makeMachines(now: number): MachineState[] {
  return MACHINE_IDS.map((id, index) => {
    const sampler = createSyntheticMetricsSampler(
      {collectorId:`demo-${id}`, runId:'synthetic-run', bootId:'synthetic-boot'},
      {now:() => now, rand:() => 0.5, sequence:{next:() => Math.floor(now / 5000)}},
    );
    const inventory = sampler.inventory();
    const metrics = sampler.next();
    const p = metrics.payload;
    const cores = index === 0 ? 48 : 16;
    const memory = index === 0 ? 128 : 32;
    inventory.cpu = {model:index === 0 ? 'AMD Ryzen Threadripper 7960X' : 'AMD Ryzen 7 7700', logicalCores:cores};
    inventory.os = {platform:'linux', release:'6.12.0', distro:'Synthetic Linux', arch:'x86_64'};
    inventory.memory = {totalBytes:memory * GiB, swapTotalBytes:8 * GiB};
    if (p.cpu.status === 'ok') {
      p.cpu.perCore = Array.from({length:cores}, (_, core) => Math.round(8 + 60 * (1 + Math.sin(now / 37000 + core * 0.7)) / 2));
      p.cpu.usagePercent = Math.round(p.cpu.perCore.reduce<number>((sum, value) => sum + (value ?? 0), 0) / cores * 10) / 10;
    }
    if (p.memory.status === 'ok') {
      p.memory.totalBytes = memory * GiB;
      p.memory.usedBytes = Math.round((index === 0 ? 24 + 3 * Math.sin(now / 91000) : 12 + Math.sin(now / 91000)) * GiB);
    }
    if (index === 0 && p.gpus.status === 'ok') {
      const gpu = p.gpus.devices[0];
      const inv = inventory.gpus[0];
      inventory.gpus = [0, 1].map(i => ({...inv, id:`gpu:SYNTHETIC-${i}`, name:`NVIDIA GeForce RTX 3090`, vendor:'nvidia', memoryTotalBytes:24 * GiB}));
      p.gpus.devices = [0, 1].map(i => gpu.status === 'ok' ? {...gpu, id:`gpu:SYNTHETIC-${i}`, utilizationPercent:Math.round(45 + 20 * Math.sin(now / 37000 + i)), memoryUsedBytes:(6 + i * 2) * GiB, memoryTotalBytes:24 * GiB} : {...gpu, id:`gpu:SYNTHETIC-${i}`});
    }
    const receivedAt = new Date(now).toISOString();
    return {machineId:id, collectorId:`demo-${id}`, hostname:id, displayName:id,
      connected:true, expectedIntervalMs:5000, lastReceivedAt:receivedAt, inventory,
      metrics:{...metrics, receivedAt}};
  });
}

/** Use the product's own telemetry-to-series mapping, including units/resources. */
export function makeHistory(machineId: string, metricsKey: string, range: RangeId, now: number, coreIdsKey?: string): HistoryResult {
  const toMs = Math.floor(now / 5000) * 5000;
  const fromMs = toMs - rangePreset(range).ms;
  const stepMs = (toMs - fromMs) / 120;
  const requested = new Set(metricsKey.split(','));
  const cores = coreIdsKey === undefined ? null : new Set(coreIdsKey.split(','));
  const series = new Map<string, HistorySeries>();
  for (let i = 0; i <= 120; i++) {
    const t = fromMs + i * stepMs;
    const machine = makeMachines(t).find(m => m.machineId === machineId);
    if (!machine?.metrics) continue;
    for (const point of flattenMetrics(machine.metrics.payload)) {
      if (!requested.has(point.metric)) continue;
      if (point.metric === 'cpu.core' && cores && !cores.has(point.resourceId)) continue;
      const key = `${point.metric}\0${point.resourceId}`;
      let item = series.get(key);
      if (!item) {
        item = {metric:point.metric, resourceId:point.resourceId, unit:metricOption(point.metric)?.unit ?? 'percent', points:[]};
        series.set(key, item);
      }
      item.points.push({t, avg:point.value, min:point.value, max:point.value});
    }
  }
  return {machineId, fromMs, toMs, stepMs, tier:'raw', series:[...series.values()], gaps:[], truncated:false, elapsedMs:1};
}

export function makeProcesses(machineId: string, now: number): DetailResult {
  const names = ['systemd', 'code', 'firefox', 'python', 'ollama', 'postgres', 'node', 'syncthing', 'containerd', 'nginx', 'pipewire', 'dbus-daemon'];
  const processes: ProcessRow[] = Array.from({length:48}, (_, index) => {
    const app = index % names.length;
    return {pid:index === 0 ? 1 : 1200 + index, ppid:index < names.length ? 1 : 1200 + app,
      startTicks:50000 + index, name:names[app], user:app === 0 || app === 8 ? 'root' : 'demo', state:app === 3 ? 'R' : 'S',
      cpuPercent:Math.round((app + 1) * 0.6 * (1 + Math.sin(now / 37000 + index)) * 10) / 10,
      memoryRssBytes:Math.round((60 + app * 90 + (index < 12 ? 300 : 0)) * 1024 ** 2),
      gpuMemoryBytes:app === 4 ? 3 * GiB : null, ioReadBytesPerSec:app * 1024, ioWriteBytesPerSec:app * 512,
      threads:1 + app * 2, unit:app === 0 ? 'init.scope' : `${names[app]}.service`};
  });
  return {type:'detail.result', subscriptionId:`demo-${machineId}`, capturedAt:new Date(now).toISOString(),
    snapshot:{totalProcesses:processes.length, truncated:false, inaccessible:0, processes}};
}
