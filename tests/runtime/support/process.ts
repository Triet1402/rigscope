import { type ChildProcess, spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { registerCleanup, unregisterCleanup } from "./cleanup";
import { ROOT } from "./config";
import { within } from "./deadline";

export type ExitInfo = { code: number | null; signal: string | null };

export type LineWaiter = { pred: (line: string) => boolean; resolve: (line: string) => void; timer: NodeJS.Timeout };

export interface ChildHandle {
  readonly proc: ChildProcess;
  readonly exit: Promise<ExitInfo>;
  stdoutLines(): string[];
  stderrLines(): string[];
  waitForLine(pred: (line: string) => boolean, deadlineMs: number, what: string): Promise<string>;
  stop(signal?: NodeJS.Signals, graceMs?: number): Promise<ExitInfo>;
  dump(...notes: string[]): string;
}

export function startChild(
  label: string,
  args: string[],
  env: NodeJS.ProcessEnv,
  options: { groupKill?: boolean; cwd?: string } = {},
): ChildHandle {
  const proc = spawn(process.execPath, args, {
    cwd: options.cwd ?? ROOT,
    env,
    stdio: ["ignore", "pipe", "pipe"],
    detached: options.groupKill === true,
  });
  const stdout: string[] = [];
  const stderr: string[] = [];
  const lineWaiters: LineWaiter[] = [];

  const onLine = (sink: string[], line: string): void => {
    sink.push(line);
    if (sink.length > 200) sink.shift();
    if (sink !== stdout) return;
    for (let i = lineWaiters.length - 1; i >= 0; i -= 1) {
      const waiter = lineWaiters[i];
      if (waiter.pred(line)) {
        lineWaiters.splice(i, 1);
        clearTimeout(waiter.timer);
        waiter.resolve(line);
      }
    }
  };

  // Readline preserves log lines split across multiple stdout/stderr chunks.
  createInterface({ input: proc.stdout! }).on("line", line => onLine(stdout, line));
  createInterface({ input: proc.stderr! }).on("line", line => onLine(stderr, line));

  const exit = new Promise<ExitInfo>((resolve) => {
    proc.once("error", error => {
      onLine(stderr, error.message);
      resolve({ code: 1, signal: null });
    });
    proc.once("close", (code, signal) => resolve({ code, signal: signal ?? null }));
  });

  const describe = (...notes: string[]): string => {
    const pretty = (name: string, lines: string[]): string =>
      `\n--- ${label} ${name} ---\n${lines.slice(-40).join("\n") || "(no output)"}`;
    let message = `${label} (pid ${proc.pid ?? "?"}, code ${proc.exitCode}, signal ${proc.signalCode})`;
    for (const note of notes) message += `\n${note}`;
    message += pretty("stdout tail", stdout) + pretty("stderr tail", stderr);
    return message;
  };

  const handle: ChildHandle = {
    proc,
    exit,
    stdoutLines: () => [...stdout],
    stderrLines: () => [...stderr],
    dump: describe,
    waitForLine(pred, deadlineMs, what) {
      const existing = stdout.find(pred);
      if (existing !== undefined) return Promise.resolve(existing);
      return new Promise<string>((resolve, reject) => {
        const timer = setTimeout(() => {
          const index = lineWaiters.indexOf(waiter);
          if (index >= 0) lineWaiters.splice(index, 1);
          reject(new Error(`timeout after ${deadlineMs}ms waiting for ${what}. ${describe()}`));
        }, deadlineMs);
        const waiter: LineWaiter = {
          pred,
          resolve: (line) => {
            clearTimeout(timer);
            resolve(line);
          },
          timer,
        };
        lineWaiters.push(waiter);
        // If the process exits before the line appears, surface that too.
        void exit.then(() => {
          const index = lineWaiters.indexOf(waiter);
          if (index >= 0) lineWaiters.splice(index, 1);
          clearTimeout(timer);
          reject(new Error(`process exited while waiting for ${what}. ${describe()}`));
        });
      });
    },
    async stop(signal: NodeJS.Signals = "SIGTERM", graceMs = 5_000): Promise<ExitInfo> {
      if (proc.exitCode === null && proc.signalCode === null) {
        if (options.groupKill && proc.pid !== undefined) {
          try {
            process.kill(-proc.pid, signal); // detached group: signal the whole tsx process tree
          } catch {
            proc.kill(signal); // group vanished already; fall back
          }
        } else {
          proc.kill(signal);
        }
        await within(exit, graceMs, `${label} shutdown`).catch(() => undefined);
        if (proc.exitCode === null && proc.signalCode === null) {
          // Escalate so a hung signal handler cannot stall the suite. When the
          // caller uses graceMs as the *contract* deadline, a kill here also
          // means the process itself failed the bounded-deadline requirement.
          try {
            if (options.groupKill && proc.pid !== undefined) process.kill(-proc.pid, "SIGKILL");
            else proc.kill("SIGKILL");
          } catch {
            // already gone
          }
          await exit;
        }
      }
      return await exit;
    },
  };

  const cleanup = (): Promise<unknown> => handle.stop("SIGKILL", 500);
  registerCleanup(cleanup);
  const internalStop = handle.stop.bind(handle);
  handle.stop = (async (signal?: NodeJS.Signals, graceMs?: number) => {
    const info = await internalStop(signal, graceMs);
    unregisterCleanup(cleanup);
    return info;
  }) as ChildHandle["stop"];
  return handle;
}
