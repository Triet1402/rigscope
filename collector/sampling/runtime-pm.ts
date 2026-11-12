/**
 * Runtime power-management awareness for offload GPUs (hybrid laptops).
 *
 * A discrete GPU on a laptop powers down (runtime D3) only after it has been
 * idle for a while. Every `nvidia-smi` invocation, and every read of amdgpu's
 * `gpu_busy_percent`, `mem_info_*` or hwmon files, resumes the device and
 * restarts that idle timer. Polled every two seconds, the GPU never sleeps.
 *
 * The gate therefore:
 *  - never samples a device whose `power/runtime_status` is suspended (the
 *    status file itself is a kernel bookkeeping read that does not resume it);
 *  - after a few consecutive idle samples of an awake device, pauses sampling
 *    for a quiet window so the driver can suspend it. If it suspends, sampling
 *    stays off until something else wakes it. If it stays awake (a client such
 *    as a compositor or persistence daemon holds it), sampling resumes and the
 *    next pause is backed off exponentially.
 *
 * Devices without runtime PM (`control` = `on`), the boot display adapter, and
 * NVIDIA GPUs whose driver reports Runtime D3 disabled are never gated, so
 * desktops and dGPU-only setups keep full-rate telemetry.
 */
import { readTextOrNull, type ReadTextFile } from './procfs';

export type RuntimePmStatus = 'unmanaged' | 'active' | 'suspended';

export type GateDecision =
  | {sample: true}
  | {sample: false; reason: string};

export const POWERED_DOWN_REASON = 'GPU powered down (runtime suspend)';
export const PAUSED_REASON = 'GPU idle; sampling paused so it can power down';

const PCI_ROOT = '/sys/bus/pci/devices';
const NVIDIA_PROC_ROOT = '/proc/driver/nvidia/gpus';
const IDLE_SAMPLES_BEFORE_YIELD = 3;
const MIN_YIELD_WINDOW_MS = 20_000;
const MAX_YIELD_WINDOW_MS = 120_000;
const INITIAL_YIELD_BACKOFF_MS = 5 * 60_000;
const MAX_YIELD_BACKOFF_MS = 60 * 60_000;

/** Settings fixed for the life of the driver binding: read once per device. */
function staticallyUnmanaged(read: ReadTextFile, address: string, root: string, nvidiaProcRoot: string): boolean {
  // The adapter driving the boot display cannot power down while it scans out.
  if (readTextOrNull(read, `${root}/${address}/boot_vga`)?.trim() === '1') return true;
  // NVIDIA without fine-grained RTD3 holds the device awake regardless of us;
  // gating it would only punch holes in the data.
  const nvidia = readTextOrNull(read, `${nvidiaProcRoot}/${address}/power`);
  if (nvidia !== null && /Runtime D3 status:\s*(Disabled|Not supported)/i.test(nvidia)) return true;
  return false;
}

/** Current runtime-PM state of one PCI device, without resuming it. */
export function readRuntimePm(read: ReadTextFile, address: string, root = PCI_ROOT): RuntimePmStatus {
  const status = readTextOrNull(read, `${root}/${address}/power/runtime_status`)?.trim();
  if (status === 'suspended' || status === 'suspending') return 'suspended';
  if (status === 'active' || status === 'resuming') return 'active';
  return 'unmanaged';
}

type DeviceState = {
  staticallyUnmanaged: boolean | null;
  asleep: boolean;
  idleStreak: number;
  yieldUntil: number | null;
  nextYieldAt: number;
  backoffMs: number;
};

export type RuntimePmGateOptions = {
  read: ReadTextFile;
  monotonicMs?: () => number;
  onEvent?: (line: string) => void;
  pciRoot?: string;
  nvidiaProcRoot?: string;
  idleSamplesBeforeYield?: number;
};

export class RuntimePmGate {
  private readonly read: ReadTextFile;
  private readonly monotonicMs: () => number;
  private readonly onEvent?: (line: string) => void;
  private readonly pciRoot: string;
  private readonly nvidiaProcRoot: string;
  private readonly idleSamplesBeforeYield: number;
  private readonly devices = new Map<string, DeviceState>();

  constructor(options: RuntimePmGateOptions) {
    this.read = options.read;
    this.monotonicMs = options.monotonicMs ?? (() => performance.now());
    this.onEvent = options.onEvent;
    this.pciRoot = options.pciRoot ?? PCI_ROOT;
    this.nvidiaProcRoot = options.nvidiaProcRoot ?? NVIDIA_PROC_ROOT;
    this.idleSamplesBeforeYield = options.idleSamplesBeforeYield ?? IDLE_SAMPLES_BEFORE_YIELD;
  }

  /** True when the device is runtime-suspended right now. */
  poweredDown(address: string): boolean {
    return this.managed(address) && readRuntimePm(this.read, address, this.pciRoot) === 'suspended';
  }

  /**
   * Whether the device may be sampled now. Idempotent within a cycle: only
   * time and observed power state move it, so several callers per sample
   * (inventory and metrics) agree.
   */
  decide(address: string): GateDecision {
    if (!this.managed(address)) return {sample: true};
    const state = this.state(address);
    const now = this.monotonicMs();
    const status = readRuntimePm(this.read, address, this.pciRoot);
    if (status === 'suspended') {
      if (!state.asleep) this.emit(`gpu ${address}: powered down; sampling suspended until it wakes`);
      state.asleep = true;
      state.yieldUntil = null;
      state.idleStreak = 0;
      state.backoffMs = INITIAL_YIELD_BACKOFF_MS;
      state.nextYieldAt = 0;
      return {sample: false, reason: POWERED_DOWN_REASON};
    }
    if (status !== 'active') return {sample: true};
    if (state.asleep) {
      // Woken by another client: report it while it is in use.
      state.asleep = false;
      state.idleStreak = 0;
      this.emit(`gpu ${address}: woke up; sampling resumed`);
      return {sample: true};
    }
    if (state.yieldUntil !== null) {
      if (now < state.yieldUntil) return {sample: false, reason: PAUSED_REASON};
      // Still awake after a quiet window: something else holds it.
      state.yieldUntil = null;
      state.idleStreak = 0;
      state.nextYieldAt = now + state.backoffMs;
      state.backoffMs = Math.min(state.backoffMs * 2, MAX_YIELD_BACKOFF_MS);
    }
    return {sample: true};
  }

  /** Report the outcome of a sample taken after `decide()` allowed it. */
  record(address: string, idle: boolean): void {
    if (!this.managed(address)) return;
    const state = this.state(address);
    if (!idle) {
      state.idleStreak = 0;
      state.nextYieldAt = 0;
      state.backoffMs = INITIAL_YIELD_BACKOFF_MS;
      return;
    }
    state.idleStreak += 1;
    const now = this.monotonicMs();
    if (state.idleStreak >= this.idleSamplesBeforeYield && now >= state.nextYieldAt && state.yieldUntil === null) {
      state.yieldUntil = now + this.yieldWindowMs(address);
    }
  }

  private yieldWindowMs(address: string): number {
    const raw = readTextOrNull(this.read, `${this.pciRoot}/${address}/power/autosuspend_delay_ms`)?.trim();
    const delay = raw !== undefined && /^\d+$/.test(raw) ? Number(raw) : 0;
    return Math.min(Math.max(MIN_YIELD_WINDOW_MS, delay + 10_000), MAX_YIELD_WINDOW_MS);
  }

  /**
   * `power/control` is re-read every time: power managers such as TLP switch
   * it between `on` (AC) and `auto` (battery) when the charger is unplugged.
   */
  private managed(address: string): boolean {
    const state = this.state(address);
    if (state.staticallyUnmanaged === null) {
      state.staticallyUnmanaged = staticallyUnmanaged(this.read, address, this.pciRoot, this.nvidiaProcRoot);
    }
    if (state.staticallyUnmanaged) return false;
    return readTextOrNull(this.read, `${this.pciRoot}/${address}/power/control`)?.trim() === 'auto';
  }

  private state(address: string): DeviceState {
    let state = this.devices.get(address);
    if (state === undefined) {
      state = {staticallyUnmanaged: null, asleep: false, idleStreak: 0, yieldUntil: null, nextYieldAt: 0,
        backoffMs: INITIAL_YIELD_BACKOFF_MS};
      this.devices.set(address, state);
    }
    return state;
  }

  private emit(line: string): void {
    try {
      this.onEvent?.(line);
    } catch {
      // Diagnostics must never break sampling.
    }
  }
}
