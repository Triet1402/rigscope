#!/usr/bin/env node
/**
 * Rigscope operations CLI (`dist/ops.cjs` after the runtime build).
 *
 * Commands are deliberately explicit about paths and confirmation:
 *   inspect  --database PATH
 *   migrate  --database PATH [--backup-dir DIR] --confirm-stopped
 *   backup   --database PATH --output PATH [--replace]
 *   restore  --input PATH --database PATH [--replace --confirm-stopped]
 *   package  [--source DIR] [--output ARCHIVE] [--version V] [--replace]
 *   install  --source ARCHIVE_OR_DIR --prefix DIR --state-dir DIR --units-dir DIR
 *            [--database PATH] [--activate --confirm-stopped] [--install-deps]
 *   activate --prefix DIR --state-dir DIR --version V [--confirm-stopped]
 *   rollback --prefix DIR --state-dir DIR --confirm-stopped
 *   recover-admin --database PATH --password-file PATH [--user NAME] [--enable]
 *   measure  [--machines N] [--batches N] [--records-per-batch N] [--gpus N] [--json]
 *
 * Nothing here invokes systemctl, starts a service, or contacts a hub.
 */

import { parseArgs } from "node:util";
import { resolve } from "node:path";
import {
  backupDatabase,
  inspectDatabase,
  migrateDatabase,
  resetTelemetryHistory,
  restoreDatabase,
} from "./database";
import { activateRelease, installRelease, packageRelease, rollbackRelease } from "./release";
import { recoverAdminPassword } from "./recovery";
import { measure, type MeasureReport } from "./measure";
import { OperationsError, assertPrivateRegularFile, errorMessage, readTextFile } from "./fs";

const USAGE = `Rigscope operations

Usage:
  ops inspect  --database PATH [--json]
  ops migrate  --database PATH [--backup-dir DIR] --confirm-stopped [--json]
  ops backup   --database PATH --output PATH [--replace] [--json]
  ops restore  --input PATH --database PATH [--replace --confirm-stopped] [--json]
  ops package  [--source DIR] [--output ARCHIVE] [--replace] [--json]
  ops install  --source ARCHIVE_OR_DIR --prefix DIR --state-dir DIR --units-dir DIR
               [--database PATH] [--activate --confirm-stopped] [--install-deps] [--json]
  ops activate --prefix DIR --state-dir DIR --version V [--confirm-stopped] [--json]
  ops rollback --prefix DIR --state-dir DIR --confirm-stopped [--json]
  ops reset-telemetry --database PATH [--backup-dir DIR] --confirm-stopped [--json]
  ops recover-admin --database PATH --password-file PATH [--user NAME] [--enable] [--json]
  ops measure  [--machines N] [--batches N] [--records-per-batch N] [--gpus N]
               [--viewers N] [--history-concurrency N] [--history-points N] [--mode live|replay|mixed]
               [--dir PATH] [--keep] [--json]

Safety:
  - Database backup is a consistent node:sqlite online backup of the single
    hub file (telemetry, registry, admin, alerts).
  - Restore validates integrity/schema, refuses an active hub lock, stages and
    verifies a copy, backs up the current target, then atomically replaces it.
  - Installation never runs systemctl. Activation is explicit and switches the
    current symlink only after database backup/migration verification.
`;

function optionString(values: Record<string, unknown>, key: string, required = false): string | undefined {
  const value = values[key];
  if (value === undefined) {
    if (required) throw new OperationsError("missing-argument", `missing --${key.replace(/_/g, "-")}`);
    return undefined;
  }
  if (typeof value !== "string" || value.length === 0) {
    throw new OperationsError("invalid-argument", `--${key.replace(/_/g, "-")} requires a value`);
  }
  return value;
}

function optionBoolean(values: Record<string, unknown>, key: string): boolean {
  return values[key] === true;
}

function formatBytes(bytes: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return `${value >= 10 || index === 0 ? Math.round(value) : value.toFixed(1)} ${units[index]}`;
}

function humanInspect(report: ReturnType<typeof inspectDatabase>): void {
  console.log(`database: ${report.path}`);
  console.log(`schema:   ${report.schemaVersion} (operations supports ${report.supportedSchemaVersion}${report.newerSchema ? ", NEWER" : ""})`);
  console.log(`integrity: ${report.integrityOk ? "ok" : report.integrity}`);
  console.log(`permissions: ${report.mode}; journal ${report.journalMode}; pages ${report.pageCount} × ${report.pageSize}`);
  console.log(`sizes:    main ${formatBytes(report.fileBytes)}, wal ${formatBytes(report.walBytes)}, shm ${formatBytes(report.shmBytes)}`);
  console.log(`tables:   ${report.tables.length}; registry ${report.registryPresent ? "yes" : "no"}; admin ${report.adminPresent ? "yes" : "no"}; alerts ${report.alertsPresent ? "yes" : "no"}`);
  if (report.ledgerProblem !== null) console.log(`ledger:   ${report.ledgerProblem}`);
  const counted = report.tables.filter((table) => table.rows !== undefined);
  if (counted.length > 0) {
    console.log(`row counts: ${counted.map((table) => `${table.name}=${table.rows}`).join(", ")}`);
  }
  console.log("backup now with: ops backup --database <path> --output <private-file>");
}

function humanMeasure(report: MeasureReport): void {
  console.log(`synthetic measurement: ${report.config.machines} machines, ${report.config.totalRecords} records, ${report.config.batches} batches`);
  console.log(`accepted ${report.results.accepted}, rejected ${report.results.rejected}, busy ${report.results.busy}`);
  console.log(`wall ${report.results.wallMs.toFixed(1)} ms; ingest avg ${report.results.ingestLatencyMs.avg.toFixed(1)} ms, p95 ${report.results.ingestLatencyMs.p95.toFixed(1)} ms`);
  console.log(`event-loop delay p50 ${report.results.eventLoopDelayMs.p50.toFixed(2)} ms, p99 ${report.results.eventLoopDelayMs.p99.toFixed(2)} ms, max ${report.results.eventLoopDelayMs.max.toFixed(2)} ms`);
  console.log(`RSS start ${formatBytes(report.results.rssBytes.start)}, end ${formatBytes(report.results.rssBytes.end)}, peak ${formatBytes(report.results.rssBytes.peak)}`);
  console.log(`database ${formatBytes(report.results.database.bytes)} (+ wal ${formatBytes(report.results.database.walBytes)})`);
  console.log(`history query avg ${report.results.history.queryMs.toFixed(1)} ms (max ${report.results.history.maxQueryMs.toFixed(1)} ms, ${report.results.history.viewers} viewer${report.results.history.viewers === 1 ? "" : "s"}): ${report.results.history.series} series, ${report.results.history.points} points, tier ${report.results.history.tier}${report.results.history.truncated ? " (truncated)" : ""}`);
  console.log(report.disclaimer);
}

async function run(): Promise<number> {
  const argv = process.argv.slice(2);
  const wantsJson = argv.includes("--json");
  const command = argv[0];
  if (command === undefined || command === "--help" || command === "-h" || command === "help") {
    console.log(USAGE);
    return 0;
  }
  const options = argv.slice(1);
  if (command === "inspect") {
    const parsed = parseArgs({
      args: options,
      options: { database: { type: "string" }, json: { type: "boolean" } },
      allowPositionals: false,
    });
    const report = inspectDatabase(optionString(parsed.values, "database", true)!);
    if (optionBoolean(parsed.values, "json")) console.log(JSON.stringify(report, null, 2));
    else humanInspect(report);
    return report.integrityOk ? 0 : 1;
  }
  if (command === "migrate") {
    const parsed = parseArgs({
      args: options,
      options: {
        database: { type: "string" },
        "backup-dir": { type: "string" },
        "confirm-stopped": { type: "boolean" },
        json: { type: "boolean" },
      },
      allowPositionals: false,
    });
    const report = await migrateDatabase({
      database: optionString(parsed.values, "database", true)!,
      backupDir: optionString(parsed.values, "backup-dir"),
      confirmStopped: optionBoolean(parsed.values, "confirm-stopped"),
    });
    if (optionBoolean(parsed.values, "json")) console.log(JSON.stringify(report, null, 2));
    else {
      console.log(report.migrated ? `migrated ${report.database}: schema ${report.fromSchemaVersion} -> ${report.toSchemaVersion}` : `database already at schema ${report.toSchemaVersion}; no migration applied`);
      if (report.backupPath !== null) console.log(`pre-migration backup: ${report.backupPath}`);
      console.log(`integrity: ${report.integrity}`);
    }
    return 0;
  }
  if (command === "backup") {
    const parsed = parseArgs({
      args: options,
      options: {
        database: { type: "string" },
        output: { type: "string" },
        replace: { type: "boolean" },
        json: { type: "boolean" },
      },
      allowPositionals: false,
    });
    const report = await backupDatabase({
      database: optionString(parsed.values, "database", true)!,
      output: optionString(parsed.values, "output", true)!,
      replace: optionBoolean(parsed.values, "replace"),
    });
    if (optionBoolean(parsed.values, "json")) console.log(JSON.stringify(report, null, 2));
    else {
      console.log(`backup written: ${report.output}`);
      console.log(`schema ${report.sourceSchemaVersion}; ${formatBytes(report.bytes)}; sha256 ${report.sha256}`);
      console.log(`tables: ${report.tables.length}; integrity ${report.integrity}; source was not migrated`);
    }
    return 0;
  }
  if (command === "restore") {
    const parsed = parseArgs({
      args: options,
      options: {
        input: { type: "string" },
        database: { type: "string" },
        replace: { type: "boolean" },
        "confirm-stopped": { type: "boolean" },
        json: { type: "boolean" },
      },
      allowPositionals: false,
    });
    const report = await restoreDatabase({
      input: optionString(parsed.values, "input", true)!,
      database: optionString(parsed.values, "database", true)!,
      replace: optionBoolean(parsed.values, "replace"),
      confirmStopped: optionBoolean(parsed.values, "confirm-stopped"),
    });
    if (optionBoolean(parsed.values, "json")) console.log(JSON.stringify(report, null, 2));
    else {
      console.log(`restored ${report.database} from ${report.input}`);
      console.log(`schema ${report.restoredSchemaVersion} (operations supports ${report.supportedSchemaVersion}); ${formatBytes(report.bytes)}; integrity ${report.integrity}`);
      if (report.preRestoreBackup !== null) console.log(`pre-restore backup: ${report.preRestoreBackup}`);
      console.log("Start the hub only after confirming the service is stopped and the database is the intended copy.");
    }
    return 0;
  }
  if (command === "package") {
    const parsed = parseArgs({
      args: options,
      options: {
        source: { type: "string" },
        output: { type: "string" },
        replace: { type: "boolean" },
        json: { type: "boolean" },
      },
      allowPositionals: false,
    });
    const report = await packageRelease({
      source: optionString(parsed.values, "source") ?? process.cwd(),
      output: optionString(parsed.values, "output"),
      replace: optionBoolean(parsed.values, "replace"),
    });
    if (optionBoolean(parsed.values, "json")) console.log(JSON.stringify(report, null, 2));
    else {
      console.log(`release archive: ${report.archive}`);
      console.log(`manifest: ${report.manifestPath}`);
      console.log(`version ${report.manifest.version}; schema ${report.manifest.schemaVersion}; ${report.files} files; ${formatBytes(report.bytes)}; sha256 ${report.sha256}`);
      console.log(`install production dependencies in the staged release with: ${report.manifest.dependencyInstall}`);
    }
    return 0;
  }
  if (command === "install") {
    const parsed = parseArgs({
      args: options,
      options: {
        source: { type: "string" },
        prefix: { type: "string" },
        "state-dir": { type: "string" },
        "units-dir": { type: "string" },
        database: { type: "string" },
        activate: { type: "boolean" },
        "confirm-stopped": { type: "boolean" },
        "install-deps": { type: "boolean" },
        "replace-release": { type: "boolean" },
        node: { type: "string" },
        json: { type: "boolean" },
      },
      allowPositionals: false,
    });
    const report = await installRelease({
      source: optionString(parsed.values, "source", true)!,
      prefix: optionString(parsed.values, "prefix", true)!,
      stateDir: optionString(parsed.values, "state-dir", true)!,
      unitsDir: optionString(parsed.values, "units-dir", true)!,
      database: optionString(parsed.values, "database"),
      activate: optionBoolean(parsed.values, "activate"),
      confirmStopped: optionBoolean(parsed.values, "confirm-stopped"),
      installDeps: optionBoolean(parsed.values, "install-deps"),
      replaceRelease: optionBoolean(parsed.values, "replace-release"),
      nodePath: optionString(parsed.values, "node"),
    });
    if (optionBoolean(parsed.values, "json")) console.log(JSON.stringify(report, null, 2));
    else {
      console.log(`release ${report.version} staged at ${report.releaseDir} (schema ${report.releaseSchemaVersion})`);
      console.log(`current: ${report.current ?? "not activated"}; previous: ${report.previous ?? "none"}`);
      console.log(`database: ${report.database}; dependencies: ${report.dependencyState}`);
      if (report.backupPath !== null) console.log(`pre-activation/migration backup: ${report.backupPath}`);
      console.log(`units: ${report.units.join(", ")}`);
      console.log(`private env: ${report.hubEnv}, ${report.collectorEnv}`);
      if (!report.activated) {
        console.log("Activation was not requested. After installing production dependencies and stopping any old hub:");
        console.log(`  ${process.execPath} dist/ops.cjs activate --prefix ${report.prefix} --state-dir ${report.stateDir} --version ${report.version} --confirm-stopped`);
      }
      console.log("Review before enabling; this command did not run systemctl:");
      for (const line of report.activationCommands) console.log(`  ${line}`);
    }
    return 0;
  }
  if (command === "activate") {
    const parsed = parseArgs({
      args: options,
      options: {
        prefix: { type: "string" },
        "state-dir": { type: "string" },
        "units-dir": { type: "string" },
        database: { type: "string" },
        version: { type: "string" },
        "confirm-stopped": { type: "boolean" },
        json: { type: "boolean" },
      },
      allowPositionals: false,
    });
    const report = await activateRelease({
      prefix: optionString(parsed.values, "prefix", true)!,
      stateDir: optionString(parsed.values, "state-dir", true)!,
      unitsDir: optionString(parsed.values, "units-dir"),
      database: optionString(parsed.values, "database"),
      version: optionString(parsed.values, "version", true)!,
      confirmStopped: optionBoolean(parsed.values, "confirm-stopped"),
    });
    if (optionBoolean(parsed.values, "json")) console.log(JSON.stringify(report, null, 2));
    else {
      console.log(`activated ${report.version}: ${report.current}`);
      console.log(`previous: ${report.previous ?? "none"}; schema ${report.schemaFrom ?? "none"} -> ${report.schemaTo}`);
      if (report.backupPath !== null) console.log(`backup: ${report.backupPath}`);
      console.log("This command did not run systemctl; review units, run daemon-reload, then enable explicitly.");
    }
    return 0;
  }
  if (command === "rollback") {
    const parsed = parseArgs({
      args: options,
      options: {
        prefix: { type: "string" },
        "state-dir": { type: "string" },
        database: { type: "string" },
        "confirm-stopped": { type: "boolean" },
        json: { type: "boolean" },
      },
      allowPositionals: false,
    });
    const report = await rollbackRelease({
      prefix: optionString(parsed.values, "prefix", true)!,
      stateDir: optionString(parsed.values, "state-dir", true)!,
      database: optionString(parsed.values, "database"),
      confirmStopped: optionBoolean(parsed.values, "confirm-stopped"),
    });
    if (optionBoolean(parsed.values, "json")) console.log(JSON.stringify(report, null, 2));
    else {
      console.log(`rolled back to ${report.version}: ${report.current}`);
      console.log(`previous current: ${report.previous ?? "none"}; schema ${report.schemaFrom ?? "none"} -> ${report.schemaTo ?? "none"}`);
      if (report.restoredBackup !== null) console.log(`restored matching pre-upgrade backup; replaced database was backed up at ${report.restoredBackup}`);
      console.log("This command did not run systemctl; enable/restart explicitly after review.");
    }
    return 0;
  }
  if (command === "reset-telemetry") {
    const parsed = parseArgs({
      args: options,
      options: {
        database: { type: "string" },
        "backup-dir": { type: "string" },
        "confirm-stopped": { type: "boolean" },
        json: { type: "boolean" },
      },
      allowPositionals: false,
    });
    const database = optionString(parsed.values, "database", true)!;
    const backupDir = optionString(parsed.values, "backup-dir");
    let backupPath: string | null = null;
    if (backupDir !== undefined) {
      const report = await backupDatabase({
        database,
        output: resolve(backupDir, `telemetry-pre-reset-${Date.now()}.sqlite`),
      });
      backupPath = report.output;
    }
    const report = resetTelemetryHistory({
      database,
      confirmStopped: optionBoolean(parsed.values, "confirm-stopped"),
    });
    if (optionBoolean(parsed.values, "json")) {
      console.log(JSON.stringify({ ...report, backupPath }, null, 2));
    } else {
      console.log(`telemetry history reset: ${report.database}`);
      console.log(`deleted rows: ${Object.entries(report.deleted).map(([table, rows]) => `${table}=${rows}`).join(", ")}`);
      console.log(`preserved: ${report.preserved.join(", ")}`);
      console.log(`integrity: ${report.integrity}`);
      if (backupPath !== null) console.log(`pre-reset backup: ${backupPath}`);
    }
    return 0;
  }
  if (command === "recover-admin") {
    const parsed = parseArgs({
      args: options,
      options: {
        database: { type: "string" },
        user: { type: "string" },
        "password-file": { type: "string" },
        enable: { type: "boolean" },
        json: { type: "boolean" },
      },
      allowPositionals: false,
    });
    const passwordFile = optionString(parsed.values, "password-file", true)!;
    assertPrivateRegularFile(passwordFile, "password file");
    const password = readTextFile(passwordFile).replace(/\r?\n$/, "");
    const report = await recoverAdminPassword({
      database: optionString(parsed.values, "database", true)!,
      username: optionString(parsed.values, "user"),
      password,
      enable: optionBoolean(parsed.values, "enable"),
    });
    if (optionBoolean(parsed.values, "json")) console.log(JSON.stringify(report, null, 2));
    else {
      console.log(`password reset for ${report.username}: ${report.database}`);
      console.log(`audit ${report.auditAction} at ${report.auditAt}; sessions invalidated (epoch ${report.sessionEpoch})`);
      console.log("Start the hub only after this offline recovery completes.");
    }
    return 0;
  }
  if (command === "measure") {
    const parsed = parseArgs({
      args: options,
      options: {
        machines: { type: "string" },
        batches: { type: "string" },
        "records-per-batch": { type: "string" },
        gpus: { type: "string" },
        viewers: { type: "string" },
        "history-concurrency": {type: "string"},
        "history-points": { type: "string" },
        mode: { type: "string" },
        dir: { type: "string" },
        keep: { type: "boolean" },
        json: { type: "boolean" },
      },
      allowPositionals: false,
    });
    const mode = optionString(parsed.values, "mode");
    const report = await measure({
      machines: optionString(parsed.values, "machines") as unknown as number,
      batches: optionString(parsed.values, "batches") as unknown as number,
      recordsPerBatch: optionString(parsed.values, "records-per-batch") as unknown as number,
      gpus: optionString(parsed.values, "gpus") as unknown as number,
      viewers: optionString(parsed.values, "viewers") as unknown as number,
      historyConcurrency: optionString(parsed.values,"history-concurrency") as unknown as number,
      historyPoints: optionString(parsed.values, "history-points") as unknown as number,
      mode: mode === undefined ? undefined : (mode as "live" | "replay" | "mixed"),
      dir: optionString(parsed.values, "dir"),
      keep: optionBoolean(parsed.values, "keep"),
    });
    if (optionBoolean(parsed.values, "json")) console.log(JSON.stringify(report, null, 2));
    else humanMeasure(report);
    return 0;
  }
  throw new OperationsError("unknown-command", `unknown command ${JSON.stringify(command)}; run ops help`);
}

function isMainModule(): boolean {
  try {
    return require.main === module;
  } catch {
    return true;
  }
}

if (isMainModule()) {
  run()
    .then((code) => {
      process.exitCode = code;
    })
    .catch((error: unknown) => {
      if (process.argv.includes("--json")) {
        console.log(JSON.stringify({ ok: false, error: errorMessage(error), code: error instanceof OperationsError ? error.code : "error" }, null, 2));
      } else {
        console.error(`operations error: ${errorMessage(error)}`);
        if (process.env.RIGSCOPE_OPS_DEBUG === "1" && error instanceof Error && error.stack) console.error(error.stack);
      }
      process.exitCode = 1;
    });
}

export { run as runOperations };
