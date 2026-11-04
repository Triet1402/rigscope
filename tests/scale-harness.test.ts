/**
 * Short integration smoke for the standalone sustained scale harness.
 *
 * This spawns the same harness the operator runs, but with two machines and an
 * eight-second measured phase. It proves the isolated hub child, real login,
 * collector reconnect/replay, authenticated SSE reset and concurrent History
 * requests all work together. It never contacts the live service.
 */
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

const execFileAsync = promisify(execFile);
const ROOT = fileURLToPath(new URL("..", import.meta.url));
const SCRIPT = join(ROOT, "scripts", "scale-harness.ts");

for (const sseWorkers of [0, 2]) test(
  `scale harness runs reconnect, replay and SSE reset with ${sseWorkers} stream workers`,
  { timeout: 90_000 },
  async () => {
    const directory = mkdtempSync(join(tmpdir(), "rigscope-scale-test-"));
    const output = join(directory, "report.json");
    try {
      const result = await execFileAsync(
        process.execPath,
        [
          "--import",
          "tsx",
          SCRIPT,
          "--case",
          "fleet-75m-churn",
          "--machines",
          "2",
          "--viewers",
          "1",
          "--sse-clients",
          "2",
          "--sse-workers",
          String(sseWorkers),
          "--churn-machines",
          "1",
          "--churn-every-ms",
          "3000",
          "--duration-ms",
          "8000",
          "--json-out",
          output,
        ],
        {
          cwd: ROOT,
          timeout: 80_000,
          maxBuffer: 20 * 1024 * 1024,
          env: { ...process.env, NODE_ENV: "production" },
        },
      );
      assert.equal(result.stderr.includes("scale harness failed"), false, result.stderr);
      const report = JSON.parse(readFileSync(output, "utf8")) as {
        tool: string;
        synthetic: boolean;
        cases: Array<{
          config: { machines: number; durationMs: number; sseWorkers: number };
          authorization: {anonymousDenied:boolean;scopedHistory:boolean;scopedSnapshot:boolean;scopedReset:boolean};
          ingestion: {
            recordsSent: number;
            recordsAccepted: number;
            replayRecordsSent: number;
            replayRecordsAccepted: number;
            reconnects: number;
            retryAcks: number;
            sampleToCommitLagMs: { count: number };
          };
          history: { queries: number; ok: number; rateLimited: number; errors: number };
          stream: { events: number; snapshots: number; httpStatuses: Record<string, number>; resetsAfterReconnect: number; reconnects: number };
          hub: { window: { eventLoopDelayMs: { p99: number } } | null; final: { database: { bytes: number } } | null };
          artifacts: { kept: boolean; directory: string };
        }>;
      };
      assert.equal(report.tool, "rigscope-scale-harness");
      assert.equal(report.synthetic, true);
      assert.equal(report.cases.length, 1);
      const measurement = report.cases[0]!;
      assert.equal(measurement.config.machines, 2);
      assert.equal(measurement.config.durationMs, 8_000);
      assert.equal(measurement.config.sseWorkers, sseWorkers);
      assert.deepEqual(measurement.authorization,{anonymousDenied:true,scopedHistory:true,scopedSnapshot:true,scopedReset:true});
      assert.ok(measurement.ingestion.recordsSent > 0);
      assert.equal(measurement.ingestion.recordsAccepted, measurement.ingestion.recordsSent);
      assert.equal(measurement.ingestion.retryAcks, 0);
      assert.ok(measurement.ingestion.sampleToCommitLagMs.count > 0);
      assert.ok(measurement.ingestion.reconnects > 0);
      assert.ok(measurement.ingestion.replayRecordsSent > 0);
      assert.equal(measurement.ingestion.replayRecordsAccepted, measurement.ingestion.replayRecordsSent);
      assert.ok(measurement.history.queries > 0);
      assert.ok(measurement.history.ok > 0);
      assert.equal(measurement.history.errors, 0);
      assert.ok(measurement.stream.events > 0);
      assert.equal(measurement.stream.snapshots, 2);
      assert.ok(measurement.stream.httpStatuses["200"] >= 2);
      assert.ok(measurement.stream.reconnects > 0);
      assert.ok(measurement.stream.resetsAfterReconnect > 0);
      assert.ok(measurement.hub.window !== null);
      assert.ok((measurement.hub.final?.database.bytes ?? 0) > 0);
      assert.equal(measurement.artifacts.kept, false);
      assert.equal(existsSync(measurement.artifacts.directory), false);
    } finally {
      rmSync(directory, { recursive: true, force: true });
    }
  },
);
