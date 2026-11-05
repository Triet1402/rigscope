import { spawn } from 'node:child_process';
import { existsSync, mkdirSync, openSync, closeSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createInterface } from 'node:readline/promises';
import { COMPONENTS, type Component, readSetup, componentEnabled } from './config';
import { serviceStatus, systemctl } from './services';
import { runningOwnedComponents, stopOwnedRuntimes } from './reset';
import { startInstallation } from './lifecycle';
import { printWarning, printSuccess } from './output';

/** Restore a trial/foreground runtime in the background when no service exists. */
async function resumeComponent(dir: string, component: Component): Promise<'started' | 'already running'> {
  if (existsSync(join(dir, 'runtime'))) return startInstallation(dir, component);
  const run = join(dir, 'run');
  mkdirSync(run, {recursive: true, mode: 0o700});
  const log = join(run, `${component}.log`);
  const fd = openSync(log, 'a', 0o600);
  let child;
  try {
    child = spawn(process.execPath, [process.argv[1]!, 'run', component, '--state-dir', dir], {
      detached: true, stdio: ['ignore', fd, fd], env: process.env,
    });
  } finally { closeSync(fd); }
  let failure: Error | undefined;
  child.on('error', error => { failure = error; });
  child.unref();
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    if (failure || child.exitCode !== null || child.signalCode !== null) break;
    try {
      const owner = JSON.parse(readFileSync(join(run, `${component}.lock`), 'utf8')) as {pid: number};
      if (owner.pid === child.pid) return 'started';
    } catch { /* wait for the supervisor to acquire its runtime lease */ }
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  child.kill('SIGTERM');
  throw new Error(`Could not resume ${component}; inspect ${log}${failure ? `: ${failure.message}` : ''}.`);
}

export async function confirmStop(message: string): Promise<boolean> {
  if (!process.stdin.isTTY || !process.stderr.isTTY) {
    throw new Error('Running Rigscope processes need to stop. Run again with --yes to approve stopping without a prompt.');
  }
  const reader = createInterface({input: process.stdin, output: process.stderr});
  try { return /^(y|yes)$/i.test((await reader.question(`${message} [y/N] `)).trim()); }
  finally { reader.close(); }
}

export type MutationOptions = {yes?: boolean; noRestart?: boolean; uninstall?: boolean; managesServices?: boolean};
export type MutationDependencies = {
  foreground?: typeof runningOwnedComponents;
  status?: typeof serviceStatus;
  stopService?: (dir: string, component: Component) => void;
  stopForeground?: typeof stopOwnedRuntimes;
  confirm?: typeof confirmStop;
  start?: typeof startInstallation;
};

/** Capture only the components running before the change; never resume uninstall. */
export async function prepareMutation(dir: string, selected: Component[], options: MutationOptions, deps: MutationDependencies = {}) {
  const statuses = new Map(COMPONENTS.map(component => [component, (deps.status ?? serviceStatus)(dir, component)]));
  const foreground = (deps.foreground ?? runningOwnedComponents)(dir).filter(component => !statuses.get(component)!.active);
  // Foreground supervisors may own both components, so stop/resume them together.
  const services = COMPONENTS.filter(component => selected.includes(component) && statuses.get(component)!.active);
  const running = COMPONENTS.filter(component => services.includes(component) || foreground.includes(component));
  if (running.length > 0) {
    printWarning(`The running ${running.join(' and ')} will stop for this operation.`);
    if (!options.yes && !await (deps.confirm ?? confirmStop)('Stop these processes and continue?')) throw new Error('Cancelled; no processes were stopped.');
    // Upgrade and rollback already coordinate their service stop, restart and health checks.
    if (!options.managesServices || options.noRestart) {
      for (const component of services) (deps.stopService ?? ((dir, component) => systemctl(dir, 'stop', false, component)))(dir, component);
    }
    if (foreground.length > 0) {
      // Stop services before signaling identified foreground owners, to avoid killing a supervised PID.
      if (options.managesServices && !options.noRestart) {
        for (const component of services) (deps.stopService ?? ((dir, component) => systemctl(dir, 'stop', false, component)))(dir, component);
      }
      (deps.stopForeground ?? stopOwnedRuntimes)(dir, foreground);
    }
  }
  return {
    running,
    async resume(): Promise<void> {
      if (options.uninstall || options.noRestart) return;
      const config = readSetup(dir);
      if (!config) return;
      const restore = options.managesServices && foreground.length === 0 ? [] : running;
      for (const component of restore) {
        if (!componentEnabled(config, component)) continue;
        await (deps.start ?? resumeComponent)(dir, component);
        printSuccess(`${component === 'hub' ? 'Hub' : 'Collector'} resumed.`);
      }
    },
  };
}
