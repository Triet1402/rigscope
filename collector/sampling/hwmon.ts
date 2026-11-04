/**
 * CPU temperature (hwmon) and package power (RAPL) readers.
 *
 * Both are optional telemetry: an absent sensor, an unreadable sysfs file or a
 * kernel without the interface yields null, never a fabricated zero. Reads are
 * small, synchronous sysfs file reads — no subprocess per sensor per sample.
 *
 * Temperature is limited to CPU-labelled hwmon drivers and prefers package /
 * Tctl / Tdie labels over individual cores. Power uses only top-level RAPL
 * package energy counters (`intel-rapl:N`, not `intel-rapl:N:M` subdomains) and
 * derives watts from the delta over monotonic elapsed time.
 */
import { readTextOrNull, type ReadTextFile } from './procfs';

export type ReadDir = (path: string) => string[];

const HWMON_ROOT = '/sys/class/hwmon';
const RAPL_ROOT = '/sys/class/powercap';
const MAX_HWMON_DEVICES = 64;
const MAX_HWMON_TEMPS = 32;
const MAX_RAPL_DOMAINS = 16;
/** Contract ceiling for a CPU package reading (watts). */
const MAX_PLAUSIBLE_WATTS = 10_000;
/**
 * A real RAPL wrap is seen just after the previous sample approached the range
 * ceiling. A decrease from anywhere else is a counter reset, not a wrap.
 */
const WRAP_MIN_PREVIOUS_FRACTION = 0.9;

/** hwmon drivers that describe CPU thermal sensors, not unrelated devices. */
const CPU_HWMON_NAMES = new Set([
  'coretemp',
  'k10temp',
  'zenpower',
  'cpu_thermal',
  'soc_thermal',
]);

function readCounter(read: ReadTextFile, path: string): number | null {
  const text = readTextOrNull(read, path);
  if (text === null || text.trim() === '') return null;
  const value = Number(text.trim());
  if (!Number.isSafeInteger(value) || value < 0) return null;
  return value;
}

function round2(value: number): number {
  return Math.round(value * 100) / 100;
}

function labelRank(label: string | null): number {
  if (label === null) return 2;
  const text = label.trim().toLowerCase();
  if (text === '') return 2;
  if (text.includes('package') || text.startsWith('tctl') || text.startsWith('tdie')) return 0;
  if (text === 'core' || text.startsWith('core ')) return 1;
  // A known CPU driver may expose other legitimate die/CCD labels.
  return 3;
}

function hwmonOrder(name: string): number {
  const match = /^hwmon(\d+)$/.exec(name);
  return match === null ? Number.MAX_SAFE_INTEGER : Number(match[1]);
}

function readCandidateTemperature(read: ReadTextFile, path: string): number | null {
  const text = readTextOrNull(read, path);
  if (text === null || text.trim() === '') return null;
  const millidegrees = Number(text.trim());
  if (!Number.isFinite(millidegrees)) return null;
  const celsius = millidegrees / 1000;
  if (!Number.isFinite(celsius) || celsius < -50 || celsius > 200) return null;
  return round2(celsius);
}

/**
 * Reads the best available CPU temperature. The most specific label class wins
 * (package/Tctl/Tdie, then core, then an unlabelled CPU sensor); within a class
 * the hottest socket/core is reported. Returns null when no CPU hwmon sensor is
 * present or readable.
 */
export function readCpuTemperature(read: ReadTextFile, readdir: ReadDir): number | null {
  let entries: string[];
  try {
    entries = readdir(HWMON_ROOT);
  } catch {
    return null;
  }
  const devices = entries
    .filter((entry) => /^hwmon\d+$/.test(entry))
    .sort((a, b) => hwmonOrder(a) - hwmonOrder(b))
    .slice(0, MAX_HWMON_DEVICES);

  const candidates: Array<{ rank: number; celsius: number }> = [];
  for (const device of devices) {
    const base = `${HWMON_ROOT}/${device}`;
    const driver = readTextOrNull(read, `${base}/name`);
    if (driver === null || !CPU_HWMON_NAMES.has(driver.trim().toLowerCase())) continue;

    let files: string[];
    try {
      files = readdir(base);
    } catch {
      continue;
    }
    const inputs = files
      .map((file) => /^temp(\d+)_input$/.exec(file))
      .filter((match): match is RegExpExecArray => match !== null)
      .sort((a, b) => Number(a[1]) - Number(b[1]))
      .slice(0, MAX_HWMON_TEMPS);
    for (const match of inputs) {
      const celsius = readCandidateTemperature(read, `${base}/${match[0]}`);
      if (celsius === null) continue;
      const label = readTextOrNull(read, `${base}/temp${match[1]}_label`);
      const trimmed = label === null ? null : label.trim();
      candidates.push({ rank: labelRank(trimmed), celsius });
    }
  }

  if (candidates.length === 0) return null;
  const bestRank = Math.min(...candidates.map((candidate) => candidate.rank));
  return Math.max(...candidates.filter((candidate) => candidate.rank === bestRank).map((candidate) => candidate.celsius));
}

type RaplBaseline = { energyUj: number; atMs: number };

export type RaplPowerMeter = {
  /** Package watts for this interval, or null while warming up/unavailable. */
  sample(atMs: number | null): number | null;
};

/**
 * Stateful package-power meter. Each top-level RAPL package contributes its own
 * delta, so multiple sockets are summed, while subdomains are never added to
 * their parent. A counter decrease is interpreted as a documented wrap only
 * when `max_energy_range_uj` is available; implausible power (a reset) is
 * rejected and the baseline restarts on the next sample.
 */
export function createRaplPowerMeter(read: ReadTextFile, readdir: ReadDir): RaplPowerMeter {
  const baselines = new Map<string, RaplBaseline>();

  return {
    sample(atMs: number | null): number | null {
      if (atMs === null || !Number.isFinite(atMs)) return null;

      let entries: string[];
      try {
        entries = readdir(RAPL_ROOT);
      } catch {
        baselines.clear();
        return null;
      }
      const domains = entries
        .filter((entry) => /^intel-rapl:\d+$/.test(entry))
        .sort()
        .slice(0, MAX_RAPL_DOMAINS);

      const current = new Map<string, { energyUj: number; maxRangeUj: number | null }>();
      for (const domain of domains) {
        const base = `${RAPL_ROOT}/${domain}`;
        const energyUj = readCounter(read, `${base}/energy_uj`);
        // An unreadable package counter (often root-only) or one package
        // missing makes whole-package power unknown rather than partial.
        if (energyUj === null) {
          baselines.clear();
          return null;
        }
        current.set(domain, { energyUj, maxRangeUj: readCounter(read, `${base}/max_energy_range_uj`) });
      }
      if (current.size === 0) {
        baselines.clear();
        return null;
      }

      let totalWatts = 0;
      let ready = true;
      for (const [domain, reading] of current) {
        const previous = baselines.get(domain);
        baselines.set(domain, { energyUj: reading.energyUj, atMs });
        if (previous === undefined) {
          ready = false;
          continue;
        }
        const elapsedMs = atMs - previous.atMs;
        if (!(elapsedMs > 0)) {
          ready = false;
          continue;
        }
        let deltaUj = reading.energyUj - previous.energyUj;
        if (deltaUj < 0) {
          if (
            reading.maxRangeUj === null ||
            reading.maxRangeUj <= reading.energyUj ||
            previous.energyUj > reading.maxRangeUj ||
            previous.energyUj < reading.maxRangeUj * WRAP_MIN_PREVIOUS_FRACTION
          ) {
            ready = false;
            continue;
          }
          deltaUj = reading.maxRangeUj - previous.energyUj + reading.energyUj;
        }
        // 1 J = 1e6 uJ; watts = uJ / (1000 * ms).
        const watts = deltaUj / (1000 * elapsedMs);
        if (!Number.isFinite(watts) || watts < 0 || watts > MAX_PLAUSIBLE_WATTS) {
          // Most often a counter reset that looked like a wrap; restart cleanly.
          ready = false;
          continue;
        }
        totalWatts += watts;
      }

      for (const domain of [...baselines.keys()]) {
        if (!current.has(domain)) baselines.delete(domain);
      }
      if (!ready) return null;
      return round2(totalWatts);
    },
  };
}
