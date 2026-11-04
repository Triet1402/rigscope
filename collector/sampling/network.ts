/**
 * Network interface inventory for telemetry.
 *
 * Addresses come from `os.networkInterfaces()` (prefix parsed from `cidr`,
 * falling back to the netmask), link attributes from sysfs. Loopback is kept,
 * and virtual links (tailscale0, veth, bridges, ...) are explicitly marked so
 * the UI never counts them as physical hardware.
 */
import type { NetworkInterfaceInfo } from 'node:os';
import type { InterfaceAddress, InterfaceInventory } from '../../src/contracts';
import { MAX_ADDRESSES_PER_INTERFACE, MAX_INTERFACES } from '../../src/contracts';
import type { ReadTextFile } from './procfs';

export type NetworkInterfacesReader = () => NodeJS.Dict<NetworkInterfaceInfo[]>;

function readOptional(read: ReadTextFile, path: string): string | null {
  try {
    return read(path);
  } catch {
    return null;
  }
}

export function parseSpeedMbps(raw: string | null): number | null {
  if (raw === null) return null;
  const value = Number(raw.trim());
  if (!Number.isSafeInteger(value) || value <= 0 || value > 10_000_000) return null;
  return value;
}

export function parseInterfaceState(raw: string | null): 'up' | 'down' | 'unknown' {
  if (raw === null) return 'unknown';
  const state = raw.trim().toLowerCase();
  if (state === 'up') return 'up';
  if (state === 'down' || state === 'lowerlayerdown' || state === 'dormant' || state === 'notpresent') {
    return 'down';
  }
  return 'unknown';
}

function netmaskToPrefix(netmask: string | null | undefined, family: 'ipv4' | 'ipv6'): number | null {
  if (typeof netmask !== 'string' || netmask === '') return null;
  if (family === 'ipv4') {
    const parts = netmask.split('.').map(Number);
    if (parts.length !== 4 || parts.some((part) => !Number.isInteger(part) || part < 0 || part > 255)) {
      return null;
    }
    let prefix = 0;
    for (const part of parts) {
      prefix += ((part >>> 0).toString(2).match(/1/g) ?? []).length;
    }
    return prefix;
  }
  // IPv6 netmask fallback: count binary ones in each hextet.
  const groups = netmask.split(':');
  if (groups.length === 0) return null;
  let prefix = 0;
  for (const group of groups) {
    if (group === '') continue;
    const value = Number.parseInt(group, 16);
    if (!Number.isInteger(value) || value < 0 || value > 0xffff) return null;
    prefix += (value.toString(2).match(/1/g) ?? []).length;
  }
  return prefix;
}

function prefixFromCidr(cidr: string | null | undefined, family: 'ipv4' | 'ipv6', netmask: string | null | undefined): number {
  if (typeof cidr === 'string') {
    const slash = cidr.lastIndexOf('/');
    if (slash >= 0) {
      const parsed = Number(cidr.slice(slash + 1));
      const max = family === 'ipv4' ? 32 : 128;
      if (Number.isInteger(parsed) && parsed >= 0 && parsed <= max) return parsed;
    }
  }
  const fromMask = netmaskToPrefix(netmask, family);
  if (fromMask !== null) return fromMask;
  return family === 'ipv4' ? 32 : 128;
}

function addressesFor(entries: NetworkInterfaceInfo[]): InterfaceAddress[] {
  const out: InterfaceAddress[] = [];
  const seen = new Set<string>();
  for (const entry of entries) {
    const family = String(entry.family).toLowerCase() === 'ipv4' ? 'ipv4' : String(entry.family).toLowerCase() === 'ipv6' ? 'ipv6' : null;
    if (family === null) continue;
    const address = entry.address;
    if (typeof address !== 'string' || address.length < 2 || address.length > 64) continue;
    const prefix = prefixFromCidr(entry.cidr, family, entry.netmask);
    const key = `${family}:${address}/${prefix}`;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({family, address, prefix});
    if (out.length >= MAX_ADDRESSES_PER_INTERFACE) break;
  }
  return out;
}

/**
 * Enumerates interfaces from `os.networkInterfaces()`, augmented with names
 * present in sysfs (down links with no addresses still show up).
 */
export function discoverInterfaces(
  interfaces: NetworkInterfacesReader,
  read: ReadTextFile,
  readdir: (path: string) => string[],
): InterfaceInventory[] {
  let byName: NodeJS.Dict<NetworkInterfaceInfo[]>;
  try {
    byName = interfaces();
  } catch {
    byName = {};
  }
  const names = new Set<string>(Object.keys(byName));
  try {
    for (const name of readdir('/sys/class/net')) names.add(name);
  } catch {
    // sysfs may be unavailable in a container; os.networkInterfaces still works.
  }
  let virtualNames = new Set<string>();
  try {
    virtualNames = new Set(readdir('/sys/devices/virtual/net'));
  } catch {
    virtualNames = new Set();
  }

  const result: InterfaceInventory[] = [];
  for (const name of [...names].sort()) {
    if (!/^[A-Za-z0-9_.:@-]{1,64}$/.test(name)) continue;
    const base = `/sys/class/net/${name}`;
    const macRaw = readOptional(read, `${base}/address`);
    const mac = macRaw === null ? null : macRaw.trim() === '' ? null : macRaw.trim().slice(0, 64);
    const speed = parseSpeedMbps(readOptional(read, `${base}/speed`));
    const state = parseInterfaceState(readOptional(read, `${base}/operstate`));
    result.push({
      id: `net:${name}`,
      name,
      mac,
      addresses: addressesFor(byName[name] ?? []),
      virtual: virtualNames.has(name),
      state,
      speedMbps: speed,
    });
    if (result.length >= MAX_INTERFACES) break;
  }
  return result;
}
