/** Per-run and OS boot identities. Persistent installation identity and
 * destination credentials are owned by local-state.ts.
 */
import { randomUUID } from 'node:crypto';
import * as nodeFs from 'node:fs';

export type CollectorIdentity = {
  collectorId: string;
  hostname: string;
  /** Newly generated on every collector launch. */
  runId: string;
  /** OS boot id, or BOOT_ID_UNAVAILABLE when the kernel does not provide one. */
  bootId: string;
};

export type BootIdentity = {
  bootId: string;
  available: boolean;
};

/**
 * Structural filesystem subset so tests can inject a stub; the real
 * `node:fs` module satisfies this shape.
 */
export interface BootIdFileSystem {
  readFileSync(path: string, encoding?: BufferEncoding): string | Buffer;
}

/**
 * Reported when no OS boot identity can be read. Non-empty on purpose: the
 * shared contract requires string boot IDs, and honest sentinels keep an
 * unavailable metric distinguishable from a real UUID.
 */
export const BOOT_ID_UNAVAILABLE = 'unavailable';

const BOOT_ID_PATH = '/proc/sys/kernel/random/boot_id';
const BOOT_ID_PATTERN =
  /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** Reads the Linux boot identity when available. */
export function readBootIdentity(fs: BootIdFileSystem = nodeFs): BootIdentity {
  try {
    const raw = String(fs.readFileSync(BOOT_ID_PATH, 'utf8')).trim();
    if (BOOT_ID_PATTERN.test(raw)) {
      return {bootId: raw.toLowerCase(), available: true};
    }
  } catch {
    // Not Linux, or the file is not readable: fall through to the sentinel.
  }
  return {bootId: BOOT_ID_UNAVAILABLE, available: false};
}

/** Generates a fresh run ID on collector startup. */
export function generateRunId(): string {
  return randomUUID();
}
