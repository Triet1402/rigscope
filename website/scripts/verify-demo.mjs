import {build} from 'esbuild';
import {mkdir, rm} from 'node:fs/promises';
import {fileURLToPath, pathToFileURL} from 'node:url';
import {resolve} from 'node:path';

const root = fileURLToPath(new URL('..', import.meta.url));
const output = resolve(root, '.build/verify-demo.mjs');
await mkdir(resolve(root, '.build'), {recursive:true});
try {
  await build({absWorkingDir:root, stdin:{contents:`
    import assert from 'node:assert/strict';
    import {makeMachines, makeHistory, makeProcesses} from './src/product-demo/fixtures';
    import {parseHubSnapshot, parseCollectorMessage} from '../src/contracts';
    import {parseHistoryResult} from '../src/history/presentation';
    import {HISTORY_METRICS} from '../src/history/metrics';
    import {METRICS} from '../src/hub/storage/series';
    const now = Date.parse('2026-11-06T12:00:00Z');
    const machines = makeMachines(now);
    const snapshot = parseHubSnapshot({hubRunId:'synthetic-demo', machines});
    assert.equal(snapshot.machines.length, 2);
    const metricsKey = HISTORY_METRICS.map(m => m.metric).join(',');
    for (const machine of machines) {
      const payload = machine.metrics.payload;
      assert.equal(payload.cpu.perCore.length, machine.inventory.cpu.logicalCores);
      assert.equal(payload.memory.totalBytes, machine.inventory.memory.totalBytes);
      const history = makeHistory(machine.machineId, metricsKey, '10m', now);
      assert.ok(parseHistoryResult(history), 'Product history parser accepts synthetic history');
      assert.ok(history.series.some(s => s.metric === 'cpu.core'));
      for (const series of history.series) {
        assert.equal(series.unit, METRICS[series.metric].unit);
        assert.equal(series.points.length, 121);
        assert.equal(series.points.at(-1).t, now);
      }
      const selected = makeHistory(machine.machineId, 'cpu.core', '1h', now, 'core:0,core:2');
      assert.deepEqual(selected.series.map(s => s.resourceId), ['core:0','core:2']);
      const processes = parseCollectorMessage(makeProcesses(machine.machineId, now));
      assert.equal(processes.type, 'detail.result');
      assert.equal(processes.snapshot.processes.length, processes.snapshot.totalProcesses);
      assert.ok(processes.snapshot.processes.every(p => !('command' in p)), 'No command lines or real host data');
    }
    console.log('Synthetic demo fixtures satisfy the product telemetry, history, and process contracts.');
  `, resolveDir:root, sourcefile:'verify-demo.ts', loader:'ts'}, bundle:true, platform:'node', format:'esm', outfile:output, tsconfigRaw:{}});
  await import(pathToFileURL(output).href);
} finally {
  await rm(output, {force:true});
}
