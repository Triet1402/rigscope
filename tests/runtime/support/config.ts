import { existsSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

export const ROOT = fileURLToPath(new URL("../../..", import.meta.url));

export const CONNECT_PATH = "/api/collector/connect";

export const HUB_BOOT_MS = 30_000;

export const DEV_BOOT_MS = 90_000;

export const SSE_EVENT_MS = 15_000;

export const WS_DEADLINE_MS = 10_000;

export type CredentialConfig = { collectorId: string; machineId: string; token: string };

export function credential(collectorId: string, machineId: string, token: string): CredentialConfig {
  return { collectorId, machineId, token };
}

export function demoToken(label: string): string {
  return `demo-token-${label}-${Math.random().toString(16).slice(2, 10)}`;
}

export function requireArtifacts(): void {
  for (const rel of ["dist/hub.cjs", "dist/collector.cjs"]) {
    if (!existsSync(join(ROOT, rel))) {
      throw new Error(
        `Missing ${rel}: integration tests spawn the packaged binaries from the project root. ` +
          `Run "npm run build" first (next build + build:runtime), then "npm run test:runtime".`,
      );
    }
  }
  if (!existsSync(join(ROOT, ".next", "BUILD_ID"))) {
    throw new Error(
      "Missing production .next build (dist/ exists but .next/BUILD_ID does not). " +
        'The packaged hub serves Next.js in production mode. Run "npm run build" (or "npx next build") first. ' +
        'Runtime-only changes need "npm run build:runtime"; UI changes need "npm run build:ui".',
    );
  }
}
