/**
 * Linux Pressure Stall Information (PSI) readers.
 *
 * `/proc/pressure/{cpu,memory,io}` exposes `some` and `full` lines with avg10
 * percentages. A missing file, missing line or malformed average is unknown,
 * never a successful zero. CPU normally has only the `some` line; memory and
 * I/O may have both.
 */
import { readTextOrNull, type ReadTextFile } from './procfs';

export type PressureSnapshot = {
  /** Missing lines stay null. */
  someAvg10: number | null;
  fullAvg10: number | null;
};

function parseAvg10(fields: string): number | null {
  for (const field of fields.split(/\s+/)) {
    const match = /^avg10=([0-9]+(?:\.[0-9]+)?)$/.exec(field);
    if (match === null) continue;
    const value = Number(match[1]);
    if (!Number.isFinite(value) || value < 0 || value > 100) return null;
    return value;
  }
  return null;
}

/** Parses one PSI file; null when it contains neither recognised line. */
export function parsePressure(text: string): PressureSnapshot | null {
  if (typeof text !== 'string') return null;
  let someAvg10: number | null = null;
  let fullAvg10: number | null = null;
  let sawLine = false;
  for (const line of text.split('\n')) {
    const match = /^(some|full)\s+(.*)$/.exec(line.trim());
    if (match === null) continue;
    sawLine = true;
    const value = parseAvg10(match[2]!);
    if (match[1] === 'some') someAvg10 = value;
    else fullAvg10 = value;
  }
  return sawLine ? {someAvg10, fullAvg10} : null;
}

/** Reads one PSI file; missing/unreadable stays null. */
export function readPressure(read: ReadTextFile, path: string): PressureSnapshot | null {
  const text = readTextOrNull(read, path);
  return text === null ? null : parsePressure(text);
}
