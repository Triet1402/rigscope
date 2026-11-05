/** Filesystem selection, PCI/GPU join and inventory contract fixtures. */
import test from 'node:test';
import assert from 'node:assert/strict';

import { parseInventory } from '../src/contracts';
import { buildInventory } from '../collector/sampling/inventory';
import { parseMountinfo } from '../collector/sampling/procfs';
import { selectFilesystems } from '../collector/sampling/storage';
import {
  createReaders,
  fakeGpuPoller,
  meminfo,
  pciDeviceFiles,
  procStatLine,
} from './sampling-helpers';

test('selectFilesystems: real filesystems only, deduplicated by device', () => {
  const entries = parseMountinfo([
    '1 0 0:1 / /tmp rw - tmpfs tmpfs rw',
    '2 0 259:2 / / rw - ext4 /dev/nvme0n1p2 rw',
    '3 0 259:2 /home /home rw - ext4 /dev/nvme0n1p2 rw',
    '4 0 8:18 / /srv/storage rw - btrfs /dev/sdb2 rw',
    '5 0 8:18 /sub /srv/storage/sub rw - btrfs /dev/sdb2 rw',
    '6 0 259:1 / /boot/efi rw - vfat /dev/nvme0n1p1 rw',
    '7 0 7:0 / /snap/core ro - squashfs /dev/loop0 ro',
    '8 0 0:99 / /var/lib/docker/overlay2 rw - overlay overlay rw',
    '9 0 8:32 / /data rw - fuseblk /dev/sdc1 rw',
  ].join('\n'));
  const selected = selectFilesystems(entries);
  assert.deepEqual(selected.map((entry) => entry.id), ['fs:/', 'fs:/boot/efi', 'fs:/srv/storage']);
  assert.equal(selected[0]!.device, '/dev/nvme0n1p2');
  assert.equal(selected[2]!.device, '/dev/sdb2');

  const withFuse = selectFilesystems(entries, {allowFsTypes: ['fuseblk']});
  assert.ok(withFuse.some((entry) => entry.id === 'fs:/data'));
});

function inventoryFixture() {
  const files: Record<string, string> = {
    '/proc/stat': [
      `cpu  ${procStatLine({user: 100, system: 50, idle: 1_000})}`,
      `cpu0 ${procStatLine({user: 50, system: 25, idle: 500})}`,
      `cpu1 ${procStatLine({user: 50, system: 25, idle: 500})}`,
    ].join('\n'),
    '/proc/meminfo': meminfo({MemTotal: 64_000_000, MemAvailable: 32_000_000, SwapTotal: 8_000_000, SwapFree: 6_000_000}),
    '/proc/cpuinfo': 'processor\t: 0\nmodel name\t: AMD EPYC 7402P 24-Core Processor\n',
    '/etc/os-release': 'NAME="Ubuntu"\nPRETTY_NAME="Ubuntu 26.10"\n',
    '/proc/self/mountinfo': [
      '49 1 259:2 / / rw,relatime - ext4 /dev/nvme0n1p2 rw',
      '65 49 259:1 / /boot/efi rw,relatime - vfat /dev/nvme0n1p1 rw',
      '70 49 8:18 / /srv/storage rw,noatime - btrfs /dev/sdb2 rw',
      '41 49 7:0 / /snap/core ro - squashfs /dev/loop0 ro',
    ].join('\n'),
    '/sys/block/nvme0n1/device/model': 'Samsung SSD 990 PRO 2TB\n',
    '/sys/block/nvme0n1/queue/rotational': '0\n',
    '/sys/block/nvme0n1/size': '3907029168\n',
    '/sys/block/sda/device/model': 'ST4000NM000A\n',
    '/sys/block/sda/queue/rotational': '1\n',
    '/sys/block/sda/size': '7814037168\n',
    '/sys/class/net/eno1np0/address': 'aa:bb:cc:dd:ee:01\n',
    '/sys/class/net/eno1np0/operstate': 'up\n',
    '/sys/class/net/eno1np0/speed': '1000\n',
    '/sys/class/net/tailscale0/address': 'aa:bb:cc:dd:ee:02\n',
    '/sys/class/net/tailscale0/operstate': 'unknown\n',
    '/sys/class/net/tailscale0/speed': '4294967295\n',
    '/sys/class/net/lo/address': '00:00:00:00:00:00\n',
    '/sys/class/net/lo/operstate': 'unknown\n',
    '/sys/class/net/enxe617146945f1/address': 'aa:bb:cc:dd:ee:03\n',
    '/sys/class/net/enxe617146945f1/operstate': 'down\n',
    ...pciDeviceFiles('0000:44:00.0', '0x1a03'),
    ...pciDeviceFiles('0000:c1:00.0', '0x10de'),
    ...pciDeviceFiles('0000:c2:00.0', '0x10de'),
  };
  const readers = createReaders(files, {
    dirs: {
      '/sys/bus/pci/devices': ['0000:44:00.0', '0000:c1:00.0', '0000:c2:00.0'],
      '/sys/block': ['nvme0n1', 'sda', 'loop0'],
      '/sys/class/net': ['eno1np0', 'tailscale0', 'lo', 'enxe617146945f1'],
      '/sys/devices/virtual/net': ['tailscale0', 'lo'],
    },
    statfs: {
      '/': {bsize: 4096, blocks: 1000, bfree: 400},
      '/boot/efi': {bsize: 512, blocks: 1000, bfree: 900},
      '/srv/storage': {bsize: 4096, blocks: 2000, bfree: 500},
    },
    networkInterfaces: () =>
      ({
        eno1np0: [
          {
            address: '192.0.2.5',
            netmask: '255.255.255.0',
            family: 'IPv4',
            mac: 'aa:bb:cc:dd:ee:01',
            internal: false,
            cidr: '192.0.2.5/24',
          },
        ],
        tailscale0: [
          {
            address: 'fd7a::1',
            netmask: 'ffff:ffff:ffff:ffff::',
            family: 'IPv6',
            mac: 'aa:bb:cc:dd:ee:02',
            internal: false,
            cidr: 'fd7a::1/64',
          },
        ],
        lo: [
          {
            address: '127.0.0.1',
            netmask: '255.0.0.0',
            family: 'IPv4',
            mac: '00:00:00:00:00:00',
            internal: true,
            cidr: '127.0.0.1/8',
          },
        ],
        enxe617146945f1: [],
      }) as unknown as NodeJS.Dict<import('node:os').NetworkInterfaceInfo[]>,
  });
  const gpu = fakeGpuPoller({
    availability: 'yes',
    info: [
      {
        uuid: 'GPU-aaaaaaaa-1111',
        name: 'NVIDIA GeForce RTX 3090',
        pciBusId: '00000000:C1:00.0',
        memoryTotalBytes: 24 * 1024 * 1024 * 1024,
      },
      {
        uuid: 'GPU-bbbbbbbb-2222',
        name: 'NVIDIA GeForce RTX 3090',
        pciBusId: '00000000:C2:00.0',
        memoryTotalBytes: 24 * 1024 * 1024 * 1024,
      },
    ],
  });
  return {readers, gpu};
}

test('buildInventory: PCI enumeration joins NVIDIA by bus id, ASPEED stays unsupported', () => {
  const {readers, gpu} = inventoryFixture();
  const inventory = buildInventory(readers, gpu);
  assert.doesNotThrow(() => parseInventory(inventory));

  assert.equal(inventory.os.platform, 'linux');
  assert.equal(inventory.os.release, 'fixture-kernel');
  assert.equal(inventory.os.distro, 'Ubuntu 26.10');
  assert.equal(inventory.cpu.model, 'AMD EPYC 7402P 24-Core Processor');
  assert.equal(inventory.cpu.logicalCores, 2);
  assert.equal(inventory.memory.totalBytes, 64_000_000 * 1024);
  assert.equal(inventory.memory.swapTotalBytes, 8_000_000 * 1024);

  const aspeed = inventory.gpus.find((entry) => entry.vendor === 'aspeed');
  assert.ok(aspeed);
  assert.equal(aspeed.telemetry, 'unsupported');
  assert.equal(aspeed.id, 'pci:0000:44:00.0');
  assert.match(aspeed.reason ?? '', /management adapter; no telemetry/);

  const nvidia = inventory.gpus.filter((entry) => entry.vendor === 'nvidia');
  assert.equal(nvidia.length, 2);
  assert.ok(nvidia.every((entry) => entry.telemetry === 'supported'));
  assert.ok(nvidia.every((entry) => entry.id.startsWith('gpu:GPU-')));
  assert.ok(nvidia.every((entry) => entry.memoryTotalBytes === 24 * 1024 * 1024 * 1024));
  assert.deepEqual(
    nvidia.map((entry) => entry.pciBusId).sort(),
    ['0000:c1:00.0', '0000:c2:00.0'],
  );

  assert.deepEqual(
    inventory.filesystems.map((entry) => entry.id),
    ['fs:/', 'fs:/boot/efi', 'fs:/srv/storage'],
  );
  assert.equal(inventory.filesystems[0]!.totalBytes, 1000 * 4096);
  assert.deepEqual(inventory.disks.map((entry) => entry.id), ['disk:nvme0n1', 'disk:sda']);
  assert.equal(inventory.disks[1]!.rotational, true);
  assert.equal(inventory.disks[0]!.sizeBytes, 3_907_029_168 * 512);

  const eno = inventory.network.find((entry) => entry.name === 'eno1np0');
  assert.ok(eno);
  assert.equal(eno.virtual, false);
  assert.deepEqual(eno.addresses, [{family: 'ipv4', address: '192.0.2.5', prefix: 24}]);
  assert.equal(eno.speedMbps, 1000);
  assert.equal(eno.state, 'up');
  const tailscale = inventory.network.find((entry) => entry.name === 'tailscale0');
  assert.ok(tailscale);
  assert.equal(tailscale.virtual, true);
  assert.equal(tailscale.speedMbps, null);
  assert.deepEqual(tailscale.addresses, [{family: 'ipv6', address: 'fd7a::1', prefix: 64}]);
  assert.equal(inventory.capabilities.gpus.status, 'supported');
  assert.equal(inventory.capabilities.processes.status, 'supported');
});

test('buildInventory: NVIDIA without tooling is inventoried as unsupported, not sampled', () => {
  const {readers} = inventoryFixture();
  const gpu = fakeGpuPoller({availability: 'no', info: []});
  const inventory = buildInventory(readers, gpu);
  const nvidia = inventory.gpus.filter((entry) => entry.vendor === 'nvidia');
  assert.equal(nvidia.length, 2);
  assert.ok(nvidia.every((entry) => entry.telemetry === 'unsupported'));
  assert.ok(nvidia.every((entry) => entry.id.startsWith('pci:0000:c')));
  assert.ok(nvidia.every((entry) => /nvidia-smi not found/.test(entry.reason ?? '')));
  assert.equal(inventory.capabilities.gpus.status, 'unsupported');
  assert.doesNotThrow(() => parseInventory(inventory));
});

test('buildInventory: no display devices means no GPUs detected', () => {
  const {readers} = inventoryFixture();
  readers.dirs['/sys/bus/pci/devices'] = [];
  const inventory = buildInventory(readers, null);
  assert.equal(inventory.gpus.length, 0);
  assert.equal(inventory.capabilities.gpus.status, 'unsupported');
  assert.equal(inventory.capabilities.gpus.reason, 'no GPUs detected');
  assert.doesNotThrow(() => parseInventory(inventory));
});

test('PCI matching normalizes NVIDIA domain width without merging distinct domains', async () => {
  const { normalizePciBusId } = await import('../collector/sampling/inventory');
  assert.equal(normalizePciBusId('00000000:C1:00.0'), normalizePciBusId('0000:c1:00.0'));
  assert.notEqual(normalizePciBusId('0001:c1:00.0'), normalizePciBusId('0000:c1:00.0'));
});
