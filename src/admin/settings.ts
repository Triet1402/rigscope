/**
 * Presentation helpers for Administration → Hub settings.
 *
 * The hub runs unprivileged and cannot rewrite its configuration or restart
 * itself, so a settings change is handed to `rigscope configure` on the hub
 * machine. These helpers build that exact command and the plain-language
 * consequences shown before an administrator runs it.
 */

import type { HubStorageMode, InstallScope } from "@/contracts/api.generated";

/** The terminal prefix for configure in this installation's scope. */
export function configurePrefix(scope: InstallScope): string {
  return scope === "system" ? "sudo rigscope configure --system" : "rigscope configure";
}

/** The command that switches hub history storage to `mode`. */
export function storageCommand(scope: InstallScope, mode: HubStorageMode): string {
  return `${configurePrefix(scope)} --telemetry-mode ${mode}`;
}

export type StorageChoice = {
  mode: HubStorageMode;
  label: string;
  summary: string;
};

export const STORAGE_CHOICES: readonly StorageChoice[] = [
  {
    mode: "memory",
    label: "Memory",
    summary: "Keeps history in RAM. No disk writes for readings; history is lost whenever the hub restarts.",
  },
  {
    mode: "persistent",
    label: "Persistent",
    summary: "Saves summary history to the hub's disk so it survives restarts, within the retention limits.",
  },
];

/**
 * What switching from `current` to `next` does to existing history. Returns
 * null when nothing changes.
 */
export function storageChangeWarning(current: HubStorageMode, next: HubStorageMode): string | null {
  if (current === next) return null;
  if (next === "persistent") {
    return "History in RAM is not copied to disk. When the hub restarts to switch, the current history is lost and saving starts from then on (plus any disk history kept from an earlier persistent period).";
  }
  return "Disk history is kept but hidden until you switch back. From the restart on, history lives in RAM only and is lost at every hub restart.";
}

/** Joins sharing family names for display, or a dash for none. */
export function familyList(families: readonly string[]): string {
  return families.length === 0 ? "none" : families.join(", ");
}

/** Formats a sampling interval in ms as "2 s", "500 ms" or "1 min". */
export function formatInterval(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(1))} s`;
  return `${Number((ms / 60_000).toFixed(1))} min`;
}
