import { fileURLToPath } from "node:url";

export const ROOT = fileURLToPath(new URL("../../..", import.meta.url));

export const CONNECT_PATH = "/api/collector/connect";

export const HUB_BOOT_MS = 30_000;

export const SSE_EVENT_MS = 15_000;

export const WS_DEADLINE_MS = 10_000;

export type CredentialConfig = { collectorId: string; machineId: string; token: string };

export function credential(collectorId: string, machineId: string, token: string): CredentialConfig {
  return { collectorId, machineId, token };
}

export function demoToken(label: string): string {
  return `demo-token-${label}-${Math.random().toString(16).slice(2, 10)}`;
}
