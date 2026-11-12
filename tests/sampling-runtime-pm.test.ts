/** Runtime-PM gate: offload GPUs are never woken, and idle ones get to sleep. */
import test from 'node:test';
import assert from 'node:assert/strict';

import { GpuPoller, type ExecFileFn } from '../collector/sampling/gpu';
import { PAUSED_REASON, POWERED_DOWN_REASON, RuntimePmGate, readRuntimePm } from '../collector/sampling/runtime-pm';

const ADDRESS = '0000:01:00.0';

function fakeSysfs(initial: Record<string, string>) {
  const files = new Map(Object.entries(initial));
  const read = (path: string): string => {
    const value = files.get(path);
    if (value === undefined) throw Object.assign(new Error(`ENOENT ${path}`), {code: 'ENOENT'});
    return value;
  };
  const set = (name: string, value: string): void => {
    files.set(`/sys/bus/pci/devices/${ADDRESS}/${name}`, value);
  };
  return {read, set};
}

function laptopGpu(status = 'active') {
  return fakeSysfs({
    [`/sys/bus/pci/devices/${ADDRESS}/power/control`]: 'auto\n',
    [`/sys/bus/pci/devices/${ADDRESS}/power/runtime_status`]: `${status}\n`,
    [`/sys/bus/pci/devices/${ADDRESS}/boot_vga`]: '0\n',
    [`/proc/driver/nvidia/gpus/${ADDRESS}/power`]: 'Runtime D3 status:          Enabled (fine-grained)\n',
  });
}

test('readRuntimePm: maps kernel runtime status without touching the device', () => {
  const sysfs = laptopGpu('suspended');
  assert.equal(readRuntimePm(sysfs.read, ADDRESS), 'suspended');
  sysfs.set('power/runtime_status', 'active');
  assert.equal(readRuntimePm(sysfs.read, ADDRESS), 'active');
  sysfs.set('power/runtime_status', 'unsupported');
  assert.equal(readRuntimePm(sysfs.read, ADDRESS), 'unmanaged');
  assert.equal(readRuntimePm(sysfs.read, '0000:02:00.0'), 'unmanaged');
});

test('gate: desktops, boot display and NVIDIA without RTD3 are never gated', () => {
  for (const [name, value] of [['power/control', 'on'], ['boot_vga', '1']] as const) {
    const sysfs = laptopGpu('suspended');
    sysfs.set(name, value);
    const gate = new RuntimePmGate({read: sysfs.read});
    assert.deepEqual(gate.decide(ADDRESS), {sample: true}, name);
  }
  const sysfs = laptopGpu('suspended');
  const read = (path: string) => path.startsWith('/proc/driver/nvidia')
    ? 'Runtime D3 status:          Disabled by default\n' : sysfs.read(path);
  assert.deepEqual(new RuntimePmGate({read}).decide(ADDRESS), {sample: true});
});

test('gate: power/control is re-read so unplugging the charger takes effect', () => {
  const sysfs = laptopGpu('suspended');
  sysfs.set('power/control', 'on');
  const gate = new RuntimePmGate({read: sysfs.read});
  assert.equal(gate.decide(ADDRESS).sample, true);
  sysfs.set('power/control', 'auto');
  assert.deepEqual(gate.decide(ADDRESS), {sample: false, reason: POWERED_DOWN_REASON});
});

test('gate: idle GPU gets a quiet window, then stays unsampled once suspended', () => {
  const sysfs = laptopGpu('active');
  let now = 0;
  const gate = new RuntimePmGate({read: sysfs.read, monotonicMs: () => now});
  for (let sample = 0; sample < 3; sample += 1) {
    assert.equal(gate.decide(ADDRESS).sample, true);
    gate.record(ADDRESS, true);
    now += 2_000;
  }
  assert.deepEqual(gate.decide(ADDRESS), {sample: false, reason: PAUSED_REASON});
  // Idempotent within a cycle: inventory and metrics both ask.
  assert.deepEqual(gate.decide(ADDRESS), {sample: false, reason: PAUSED_REASON});
  sysfs.set('power/runtime_status', 'suspended');
  now += 10_000;
  assert.deepEqual(gate.decide(ADDRESS), {sample: false, reason: POWERED_DOWN_REASON});
  now += 600_000;
  assert.deepEqual(gate.decide(ADDRESS), {sample: false, reason: POWERED_DOWN_REASON});
  // Another client (a game, prime-run) woke it: sample while it is in use.
  sysfs.set('power/runtime_status', 'active');
  assert.deepEqual(gate.decide(ADDRESS), {sample: true});
});

test('gate: a GPU held awake by someone else is paused rarely, with backoff', () => {
  const sysfs = laptopGpu('active');
  let now = 0;
  const gate = new RuntimePmGate({read: sysfs.read, monotonicMs: () => now});
  const idleSamples = (count: number) => {
    for (let sample = 0; sample < count; sample += 1) {
      if (gate.decide(ADDRESS).sample) gate.record(ADDRESS, true);
      now += 2_000;
    }
  };
  idleSamples(3);
  assert.equal(gate.decide(ADDRESS).sample, false);
  now += 20_000;
  // Window over, still awake: resume, and do not pause again for 5 minutes.
  assert.equal(gate.decide(ADDRESS).sample, true);
  idleSamples(100);
  assert.equal(gate.decide(ADDRESS).sample, true);
  now += 300_000;
  idleSamples(1);
  assert.equal(gate.decide(ADDRESS).sample, false);
  // Real work resets the backoff.
  now += 20_000;
  assert.equal(gate.decide(ADDRESS).sample, true);
  gate.record(ADDRESS, false);
  idleSamples(3);
  assert.equal(gate.decide(ADDRESS).sample, false);
});

test('GpuPoller: never runs nvidia-smi against a runtime-suspended GPU', async () => {
  const sysfs = laptopGpu('suspended');
  const calls: string[][] = [];
  const execFile: ExecFileFn = (_file, args, _options, callback) => {
    calls.push([...args]);
    callback(null, 'GPU-aaaa, NVIDIA GeForce RTX 4060 Laptop GPU, 00000000:01:00.0, 0, 5, 8188, 45, 3.1\n', '');
  };
  let sensorPolls = 0;
  const poller = new GpuPoller({
    intervalMs: 2_000, execFile,
    sensorSource: {pollOnce: async () => { sensorPolls += 1; }, latest: () => new Map()},
    power: {gate: new RuntimePmGate({read: sysfs.read}), devices: () => [ADDRESS]},
  });
  await poller.pollOnce();
  assert.equal(calls.length, 0);
  assert.equal(sensorPolls, 0);
  assert.deepEqual(poller.latest()?.devices, [{id: `pci:${ADDRESS}`, status: 'unavailable', reason: POWERED_DOWN_REASON}]);

  sysfs.set('power/runtime_status', 'active');
  await poller.pollOnce();
  assert.equal(calls.length, 1);
  assert.ok(!calls[0]!.some(arg => arg.startsWith('--id=')), 'single GPU queries without --id');
  assert.equal(poller.latest()?.devices[0]?.status, 'ok');

  // Once known, a sleeping device keeps the nvidia-smi UUID id.
  sysfs.set('power/runtime_status', 'suspended');
  await poller.pollOnce();
  assert.equal(calls.length, 1);
  assert.deepEqual(poller.latest()?.devices, [{id: 'gpu:GPU-aaaa', status: 'unavailable', reason: POWERED_DOWN_REASON}]);
  assert.equal(poller.deviceInfo().length, 1);
});

test('GpuPoller: with one of two GPUs asleep, only the awake one is queried', async () => {
  const other = '0000:02:00.0';
  const sysfs = laptopGpu('suspended');
  const read = (path: string) => path.includes(other) ? (path.endsWith('control') ? 'on' : 'active') : sysfs.read(path);
  const calls: string[][] = [];
  const execFile: ExecFileFn = (_file, args, _options, callback) => {
    calls.push([...args]);
    callback(null, 'GPU-bbbb, NVIDIA RTX A2000, 00000000:02:00.0, 10, 5, 8188, 45, 3.1\n', '');
  };
  const poller = new GpuPoller({
    intervalMs: 2_000, execFile,
    power: {gate: new RuntimePmGate({read}), devices: () => [ADDRESS, other]},
  });
  await poller.pollOnce();
  assert.equal(calls.length, 1);
  assert.ok(calls[0]!.includes('--id=00000000:02:00.0'));
  assert.deepEqual(poller.latest()?.devices.map(device => [device.id, device.status]),
    [['gpu:GPU-bbbb', 'ok'], [`pci:${ADDRESS}`, 'unavailable']]);
});
