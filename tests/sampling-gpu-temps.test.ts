/** Junction/VRAM register decoding, sysfs discovery, merge and series tests. */
import test from 'node:test';
import assert from 'node:assert/strict';

import {
  GpuTempsPoller,
  decodeBlackwell,
  decodeGddr6Junction,
  decodeGddr6Vram,
  decodeGddr7,
  discoverNvidiaDisplays,
  normalizeBusFunctionKey,
  readGpuRegisterTemps,
  selectSensorProfile,
  type GpuRegisterReaders,
  type GpuSensorMap,
} from '../collector/sampling/gpu-temps';
import { GpuPoller, type ExecFileFn } from '../collector/sampling/gpu';
import { flattenMetrics } from '../src/hub/storage/series';
import { makePayload } from './storage-fixtures';

// --- normalization ----------------------------------------------------------

test('normalizeBusFunctionKey: nvidia-smi, sysfs and junk inputs', () => {
  assert.equal(normalizeBusFunctionKey('00000000:C1:00.0'), 'c1:00.0');
  assert.equal(normalizeBusFunctionKey('0000:01:00.0'), '01:00.0');
  assert.equal(normalizeBusFunctionKey(' 0000:01:00.0 '), '01:00.0');
  assert.equal(normalizeBusFunctionKey('0000:1a:00.1'), '1a:00.1');
  assert.equal(normalizeBusFunctionKey('01:00.0'), '01:00.0');
  assert.equal(normalizeBusFunctionKey(null), null);
  assert.equal(normalizeBusFunctionKey(undefined), null);
  assert.equal(normalizeBusFunctionKey('not-a-bdf'), null);
  assert.equal(normalizeBusFunctionKey('0000:01:00.9'), null);
});

// --- decoding ----------------------------------------------------------------

test('selectSensorProfile: Blackwell ids use the GDDR7 map, Ampere uses GDDR6', () => {
  assert.equal(selectSensorProfile(0x2204), 'gddr6'); // RTX 3090
  assert.equal(selectSensorProfile(0x2b85), 'gddr7'); // RTX 5090
  assert.equal(selectSensorProfile(0x2c02), 'gddr7'); // RTX 5070
  assert.equal(selectSensorProfile(0x2d19), 'gddr7'); // RTX 5060 Laptop
  assert.equal(selectSensorProfile(0x2d59), 'gddr7'); // RTX 5060 Laptop variant
});

test('decodeGddr6Junction: temperature byte at bits 8..15', () => {
  assert.equal(decodeGddr6Junction(0x3400), 52); // (0x3400 >> 8) & 0xff = 52
  assert.equal(decodeGddr6Junction(0x0), null); // all-zero: registers do not answer
  assert.equal(decodeGddr6Junction(0x1), null); // 0 °C is not a real reading
  assert.equal(decodeGddr6Junction(0x7f00), null); // >= 127 is invalid
  assert.equal(decodeGddr6Junction(0xffffffff), null);
  assert.equal(decodeGddr6Junction(null), null);
});

test('decodeGddr6Vram: 12-bit ADC divided by 32', () => {
  assert.equal(decodeGddr6Vram(0x900), 72); // 2304 / 32
  assert.equal(decodeGddr6Vram(0x170), 11.5); // 368 / 32
  assert.equal(decodeGddr6Vram(0x0), null); // all-zero: registers do not answer
  assert.equal(decodeGddr6Vram(0xfff), null); // 127.97.. rounds >= 127
  assert.equal(decodeGddr6Vram(0xffffffff), null);
  assert.equal(decodeGddr6Vram(null), null);
});

test('decodeBlackwell: Q8.8 fixed point, zero and 150C+ are invalid', () => {
  assert.equal(decodeBlackwell(68 * 256), 68);
  assert.equal(decodeBlackwell(0x2200), 34);
  assert.equal(decodeBlackwell(0x1180), 17.5);
  assert.equal(decodeBlackwell(0x0), null);
  assert.equal(decodeBlackwell(151 * 256), null);
  assert.equal(decodeBlackwell(0xffffffff), null);
});

test('decodeGddr7: 2C-per-code register with poison and range guards', () => {
  assert.equal(decodeGddr7(56 << 16), 72); // 72/2 + 20 = 56
  assert.equal(decodeGddr7(21 << 16), 2);
  assert.equal(decodeGddr7(80 << 16), 120);
  assert.equal(decodeGddr7(20 << 16), null); // below range
  assert.equal(decodeGddr7(81 << 16), null); // above range
  assert.equal(decodeGddr7(0xbadf0000 | (56 << 16)), null); // poison
  assert.equal(decodeGddr7(0xffffffff), null);
});

// --- fake sysfs fixture -------------------------------------------------------

const SYSFS = '/sys/bus/pci/devices';

type FakeSysfs = {
  attrs: Map<string, string>;
  registers: Map<string, number>;
  readers: GpuRegisterReaders;
};

function fakeSysfs(deviceIds: Record<string, string>): FakeSysfs {
  const attrs = new Map<string, string>();
  const registers = new Map<string, number>();
  for (const [bdf, deviceId] of Object.entries(deviceIds)) {
    attrs.set(`${SYSFS}/${bdf}/vendor`, '0x10de');
    attrs.set(`${SYSFS}/${bdf}/class`, '0x030200');
    attrs.set(`${SYSFS}/${bdf}/device`, deviceId);
  }
  const readers: GpuRegisterReaders = {
    readdir: (path) => (path === SYSFS ? Object.keys(deviceIds) : []),
    readAttribute: (path) => attrs.get(path) ?? null,
    readRegister: (resourcePath, offset) => registers.get(`${resourcePath}@0x${offset.toString(16)}`) ?? null,
  };
  return {attrs, registers, readers};
}

test('discoverNvidiaDisplays: only NVIDIA display functions, ids parsed', () => {
  const sysfs = fakeSysfs({
    '0000:01:00.0': '0x10f1', // HDMI audio function, same chip
    '0000:02:00.0': '0x744c', // AMD GPU injected below
    '0000:03:00.0': '0x2b85', // RTX 5090
  });
  sysfs.attrs.set(`${SYSFS}/0000:01:00.0/class`, '0x040300');
  sysfs.attrs.set(`${SYSFS}/0000:02:00.0/vendor`, '0x1002');
  const devices = discoverNvidiaDisplays(sysfs.readers, SYSFS);
  assert.deepEqual(devices, [{bdf: '0000:03:00.0', deviceId: 0x2b85}]);
});

for (const blackwellId of ['0x2b85', '0x2d19', '0x2d59']) {
  test(`readGpuRegisterTemps: GDDR6 and GDDR7 ${blackwellId} devices decode together`, () => {
    const sysfs = fakeSysfs({'0000:c1:00.0': '0x2204', '0000:c2:00.0': blackwellId});

    const r6 = `${SYSFS}/0000:c1:00.0/resource0`;
    sysfs.registers.set(`${r6}@0x2046c`, 52 << 8);
    sysfs.registers.set(`${r6}@0xe2a8`, Math.round(72.0 * 32));

    const r7 = `${SYSFS}/0000:c2:00.0/resource0`;
    sysfs.registers.set(`${r7}@0xad0a90`, 61 * 256); // channel 0
    sysfs.registers.set(`${r7}@0xad0a94`, 68 * 256); // channel 1 (hottest)
    sysfs.registers.set(`${r7}@0xad0aa0`, 65 * 256); // hardware hotspot (below channel 1)
    // FBPA 0: status marks sub-register 0 valid at 72C; FBPA 1 is poisoned.
    sysfs.registers.set(`${r7}@0x9024d0`, 1 << 24);
    sysfs.registers.set(`${r7}@0x9024c0`, 56 << 16);
    sysfs.registers.set(`${r7}@0x9064d0`, 0xbadf0001);

    const sensors = readGpuRegisterTemps(sysfs.readers, SYSFS);
    assert.equal(sensors.size, 2);
    assert.deepEqual(sensors.get('c1:00.0'), {junctionTemperatureC: 52, vramTemperatureC: 72});
    assert.deepEqual(sensors.get('c2:00.0'), {junctionTemperatureC: 68, vramTemperatureC: 72});
  });

}

test('readGpuRegisterTemps: unreadable registers stay absent, not null series', () => {
  const sysfs = fakeSysfs({'0000:c1:00.0': '0x2204'});
  const sensors = readGpuRegisterTemps(sysfs.readers, SYSFS);
  assert.equal(sensors.size, 0);
});

test('readGpuRegisterTemps: all-zero registers report nothing, not 0 °C', () => {
  // Unsupported cards (for example RTX 3070) answer with zeros; those must
  // not become a bogus junction 0 °C / VRAM 0 °C series (docs/monitoring.md).
  const sysfs = fakeSysfs({'0000:c1:00.0': '0x2204'});
  const r6 = `${SYSFS}/0000:c1:00.0/resource0`;
  sysfs.registers.set(`${r6}@0x2046c`, 0x0);
  sysfs.registers.set(`${r6}@0xe2a8`, 0x0);
  assert.equal(readGpuRegisterTemps(sysfs.readers, SYSFS).size, 0);
});

test('readGpuRegisterTemps: GDDR7 batch path decodes the hottest registers', () => {
  const sysfs = fakeSysfs({'0000:c2:00.0': '0x2b85'});
  const resource = `${SYSFS}/0000:c2:00.0/resource0`;
  const words = new Map<number, number>();
  // Blackwell channels: 61, 68 (hottest), 62, 63; hotspot sits below 68.
  const therm = 0xad0a90;
  words.set(therm, 61 * 256);
  words.set(therm + 4, 68 * 256);
  words.set(therm + 8, 62 * 256);
  words.set(therm + 12, 63 * 256);
  words.set(therm + 0x10, 65 * 256);
  // FBPA 0 marks sub-registers 0 (72C) and 1 (80C, hottest) valid; FBPA 1 is poisoned.
  words.set(0x9024d0, (1 << 24) | (1 << 25));
  words.set(0x9024c0, 56 << 16);
  words.set(0x9024c4, 60 << 16);
  words.set(0x9064d0, 0xbadf0001);

  const calls: Array<{path: string; offsets: readonly number[]}> = [];
  const readers: GpuRegisterReaders = {
    ...sysfs.readers,
    readRegisters: (resourcePath, offsets) => {
      calls.push({path: resourcePath, offsets});
      return offsets.map((offset) => words.get(offset) ?? null);
    },
  };

  const sensors = readGpuRegisterTemps(readers, SYSFS);
  assert.deepEqual(sensors.get('c2:00.0'), {junctionTemperatureC: 68, vramTemperatureC: 80});
  assert.equal(calls.length, 2);
  assert.ok(calls.every((call) => call.path === resource));
  assert.ok(calls.length <= 2);
});

test('GpuTempsPoller: latest() exposes readings and diagnostics log once per state change', async () => {
  const sysfs = fakeSysfs({'0000:c1:00.0': '0x2204'});
  const events: string[] = [];
  const poller = new GpuTempsPoller({
    readers: sysfs.readers,
    sysfsDir: SYSFS,
    onEvent: (line) => events.push(line),
  });

  // No register access: hint appears exactly once across repeated cycles.
  await poller.pollOnce();
  await poller.pollOnce();
  assert.deepEqual(events, [
    'gpu register temps: GPU BAR0 mapping failed (needs Python 3 and read access); '
      + 'run rigscope gpu-temps status. If access was granted after this collector started, it applies after a reboot',
  ]);

  // Access granted: readings appear and the healthy line logs exactly once,
  // never again per cycle; losing access later re-arms the hint.
  const r6 = `${SYSFS}/0000:c1:00.0/resource0`;
  sysfs.registers.set(`${r6}@0x2046c`, 60 << 8);
  await poller.pollOnce();
  await poller.pollOnce();
  const latest: GpuSensorMap = poller.latest();
  assert.deepEqual(latest.get('c1:00.0'), {junctionTemperatureC: 60, vramTemperatureC: null});
  assert.deepEqual(events.slice(1), ['gpu register temps: 1 device(s)']);

  sysfs.registers.delete(`${r6}@0x2046c`);
  await poller.pollOnce();
  assert.equal(events.length, 3);
  assert.ok(events[2]?.includes('mapping failed'));
});

test('GpuTempsPoller: readable but unsupported registers report unsupported, not access', async () => {
  const sysfs = fakeSysfs({'0000:c2:00.0': '0x2b85'});
  const events: string[] = [];
  const poller = new GpuTempsPoller({
    readers: {...sysfs.readers, canRead: () => true},
    sysfsDir: SYSFS,
    onEvent: (line) => events.push(line),
  });

  await poller.pollOnce();
  await poller.pollOnce();
  assert.deepEqual(events, [
    'gpu register temps: no supported thermal registers on 1 device(s)',
  ]);
  assert.ok(!events.some((line) => line.includes('not readable')));
});

test('GpuTempsPoller: a batch-only reader keeps unsupported detection on the injected path', async () => {
  const sysfs = fakeSysfs({'0000:c2:00.0': '0x2b85'});
  const events: string[] = [];
  const poller = new GpuTempsPoller({
    readers: {
      readdir: sysfs.readers.readdir,
      readAttribute: sysfs.readers.readAttribute,
      readRegisters: (_path, offsets) => offsets.map(() => 0),
    },
    sysfsDir: SYSFS,
    onEvent: (line) => events.push(line),
  });

  await poller.pollOnce();
  assert.deepEqual(events, [
    'gpu register temps: no supported thermal registers on 1 device(s)',
  ]);
});

// --- merge into nvidia-smi readings -------------------------------------------

const GPU_CSV = [
  'GPU-aaaaaaaa-1111, NVIDIA GeForce RTX 3090, 00000000:C1:00.0, 25, 15232, 24576, 30, 22.35',
  'GPU-bbbbbbbb-2222, NVIDIA GeForce RTX 3090, 00000000:C2:00.0, 10, 1000, 24576, 31, 20.0',
].join('\n');

test('GpuPoller: register temperatures merge by bus id; unmatched devices stay absent', async () => {
  const execFile: ExecFileFn = (_file, args, _options, callback) => {
    if (args.some((arg) => arg.startsWith('--query-gpu='))) {
      callback(null, GPU_CSV, '');
      return;
    }
    callback(null, '', '');
  };
  const sensors: GpuSensorMap = new Map([
    ['c1:00.0', {junctionTemperatureC: 71, vramTemperatureC: 74.5}],
  ]);
  let polls = 0;
  const poller = new GpuPoller({
    intervalMs: 1_000,
    execFile,
    sensorSource: {
      pollOnce: async () => {
        polls += 1;
      },
      latest: () => new Map(sensors),
    },
  });
  await poller.pollOnce();
  assert.equal(polls, 1);
  const snapshot = poller.latest();
  assert.ok(snapshot);
  const first = snapshot.devices[0]!;
  const second = snapshot.devices[1]!;
  assert.equal(first.status, 'ok');
  assert.equal(second.status, 'ok');
  if (first.status === 'ok' && second.status === 'ok') {
    assert.equal(first.junctionTemperatureC, 71);
    assert.equal(first.vramTemperatureC, 74.5);
    assert.equal(second.junctionTemperatureC, undefined);
    assert.equal(second.vramTemperatureC, undefined);
  }
});

test('GpuPoller: a partially supported sensor leaves absent fields undefined', async () => {
  const execFile: ExecFileFn = (_file, args, _options, callback) => {
    if (args.some((arg) => arg.startsWith('--query-gpu='))) {
      callback(null, GPU_CSV, '');
      return;
    }
    callback(null, '', '');
  };
  const sensors: GpuSensorMap = new Map([
    ['c1:00.0', {junctionTemperatureC: 60, vramTemperatureC: null}],
  ]);
  const poller = new GpuPoller({
    intervalMs: 1_000,
    execFile,
    sensorSource: {
      pollOnce: async () => undefined,
      latest: () => new Map(sensors),
    },
  });

  await poller.pollOnce();
  const snapshot = poller.latest();
  assert.ok(snapshot);
  const first = snapshot.devices[0]!;
  assert.equal(first.status, 'ok');
  if (first.status === 'ok') {
    assert.equal(first.junctionTemperatureC, 60);
    assert.equal(first.vramTemperatureC, undefined);
  }
});

test('GpuPoller: a stalling sensor cycle cannot hold up the snapshot', async () => {
  const execFile: ExecFileFn = (_file, _args, _options, callback) => callback(null, GPU_CSV, '');
  const sensors: GpuSensorMap = new Map([['c1:00.0', {junctionTemperatureC: 71, vramTemperatureC: 74.5}]]);
  const events: string[] = [];
  const poller = new GpuPoller({
    intervalMs: 1_000,
    timeoutMs: 5,
    execFile,
    onEvent: (line) => events.push(line),
    sensorSource: {
      // Never settles: only the bounded budget can end the wait.
      pollOnce: () => new Promise<void>(() => undefined),
      latest: () => new Map(sensors),
    },
  });
  await poller.pollOnce();
  const snapshot = poller.latest();
  assert.ok(snapshot);
  const first = snapshot.devices[0]!;
  assert.equal(first.status, 'ok');
  if (first.status === 'ok') assert.equal(first.junctionTemperatureC, 71);
  assert.ok(events.some((line) => line.includes('did not settle')));
});

test('GpuPoller: sensor failures never invalidate nvidia-smi readings', async () => {
  const execFile: ExecFileFn = (_file, _args, _options, callback) => callback(null, GPU_CSV, '');
  const poller = new GpuPoller({
    intervalMs: 1_000,
    execFile,
    sensorSource: {
      pollOnce: async () => {
        throw new Error('simulated sysfs failure');
      },
      latest: () => new Map(),
    },
  });
  await poller.pollOnce();
  const snapshot = poller.latest();
  assert.ok(snapshot);
  assert.equal(snapshot.devices.length, 2);
  assert.equal(poller.available(), 'yes');
});

// --- hub series ----------------------------------------------------------------

test('flattenMetrics: junction and VRAM points appear only when reported', () => {
  const reported = makePayload({
    gpus: [{id: 'gpu:GPU-a', utilization: 5, temperature: 60, junction: 71, vram: 74}],
  });
  const points = flattenMetrics(reported);
  assert.equal(points.find((p) => p.metric === 'gpu.junction.temperature')?.value, 71);
  assert.equal(points.find((p) => p.metric === 'gpu.vram.temperature')?.value, 74);

  // A machine without the feature stores nothing new.
  const absent = flattenMetrics(makePayload({gpus: [{id: 'gpu:GPU-a', utilization: 5}]}));
  assert.equal(absent.some((p) => p.metric === 'gpu.junction.temperature'), false);
  assert.equal(absent.some((p) => p.metric === 'gpu.vram.temperature'), false);

  // Once a series exists, a missing reading becomes an explicit null.
  const known = [
    {metric: 'gpu.junction.temperature', resourceId: 'gpu:GPU-a'},
    {metric: 'gpu.vram.temperature', resourceId: 'gpu:GPU-a'},
  ];
  const broken = flattenMetrics(makePayload({gpus: [{id: 'gpu:GPU-a', utilization: 5}]}), known);
  assert.equal(broken.find((p) => p.metric === 'gpu.junction.temperature')?.value, null);
  assert.equal(broken.find((p) => p.metric === 'gpu.vram.temperature')?.value, null);
});

test('flattenMetrics: an unavailable GPU nulls its known junction/VRAM series', () => {
  const known = [
    {metric: 'gpu.junction.temperature', resourceId: 'gpu:GPU-a'},
    {metric: 'gpu.vram.temperature', resourceId: 'gpu:GPU-a'},
  ];
  const points = flattenMetrics(makePayload({gpus: [{id: 'gpu:GPU-a', unavailable: true}]}), known);
  for (const metric of ['gpu.junction.temperature', 'gpu.vram.temperature']) {
    const point = points.find((p) => p.metric === metric && p.resourceId === 'gpu:GPU-a');
    assert.equal(point?.value, null, metric);
  }
});
