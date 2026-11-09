/** Linux memory BARs require mmap; read(2)/pread(2) return EIO.
 * Python's standard library supplies mmap without a compiled Node addon.
 * The helper is unprivileged, read-only, time bounded, and isolated so a
 * device disappearing (SIGBUS) cannot crash the collector.
 */
import {spawnSync} from 'node:child_process';

const READER = `
import json, mmap, os, struct, sys
fd = os.open(sys.argv[1], os.O_RDONLY | os.O_SYNC)
values = []
try:
    for offset in json.loads(sys.argv[2]):
        page = offset - offset % mmap.PAGESIZE
        try:
            with mmap.mmap(fd, mmap.PAGESIZE, flags=mmap.MAP_SHARED, prot=mmap.PROT_READ, offset=page) as region:
                values.append(struct.unpack_from('<I', region, offset - page)[0])
        except (OSError, ValueError):
            values.append(None)
finally:
    os.close(fd)
print(json.dumps(values))
`;

export function readMappedPciRegisters(path: string, offsets: readonly number[]): Array<number | null> {
  const missing = () => offsets.map(() => null);
  if (offsets.length === 0) return [];
  if (offsets.length > 512 || offsets.some(offset => !Number.isSafeInteger(offset) || offset < 0 || offset % 4 !== 0 || offset > 0x1000000)) return missing();
  const result = spawnSync('python3', ['-I', '-c', READER, path, JSON.stringify(offsets)], {
    encoding: 'utf8', timeout: 1500, maxBuffer: 16384, windowsHide: true,
  });
  if (result.status !== 0 || result.error) return missing();
  try {
    const values: unknown = JSON.parse(result.stdout);
    if (!Array.isArray(values) || values.length !== offsets.length) return missing();
    return values.map(value => Number.isInteger(value) && value >= 0 && value <= 0xffffffff ? value : null);
  } catch {
    return missing();
  }
}
