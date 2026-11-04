import { spawn, type ChildProcess } from 'node:child_process';
import { join } from 'node:path';
import { acquireProcessLease } from '../runtime/process-lease';
import { collectorHasDestinations, componentEnabled, enabledComponents, hubEnvironment, readSetup, type Component } from './config';

/** Supervisor lease for one component. The collector child owns its own lock. */
export function componentLeasePath(dir: string, component: Component): string {
  return join(dir, 'run', `${component}.lock`);
}
/** Pre-v2 combined supervisor lease; retained only to detect a migration. */
export function combinedLeasePath(dir: string): string {
  return join(dir, 'run', 'rigscope.lock');
}

function label(component: Component): string {
  return component === 'hub' ? 'Hub' : 'Collector';
}

/**
 * Run the selected components in the foreground. A systemd unit passes one
 * component, so an independent failure only ends that component's supervisor.
 * Omitting the component keeps the pre-v2 combined supervisor working for a
 * trial or a not-yet-migrated unit.
 */
export async function runInstallation(root: string, dir: string, component?: Component): Promise<number> {
  const config = readSetup(dir);
  if (!config) throw new Error('Run rigscope install or rigscope try first.');
  if (component && !componentEnabled(config, component)) {
    // A stale enabled unit must exit cleanly rather than enter a restart loop.
    console.error(`${label(component)} is disabled. Run rigscope enable ${component}.`);
    return 0;
  }
  const selected = component ? [component] : enabledComponents(config);
  if (selected.length === 0) {
    console.error('No components are enabled. Run rigscope enable hub or rigscope enable collector.');
    return 0;
  }
  // An enabled but unconfigured collector must not enter systemd's restart
  // loop. Skip only the collector; a selected hub still runs normally.
  if (selected.includes('collector') && !collectorHasDestinations(dir)) {
    console.log('Collector awaiting connection. Run rigscope connect https://your-hub.');
    const remaining = selected.filter(role => role !== 'collector');
    if (remaining.length === 0) return 0;
    selected.splice(0, selected.length, ...remaining);
  }

  const releaseLeases: Array<() => void> = [];
  try {
    if (!component) releaseLeases.push(acquireProcessLease(combinedLeasePath(dir)));
    for (const role of selected) releaseLeases.push(acquireProcessLease(componentLeasePath(dir, role)));
  } catch (error) {
    for (const release of releaseLeases.reverse()) release();
    throw error;
  }

  const children = new Set<ChildProcess>();
  const environment = Object.fromEntries(Object.entries(process.env).filter(([key]) => !/^(HUB_|COLLECTOR_|NODE_ENV$)/.test(key)));
  let stopping = false;
  let exitCode = 0;
  let forceTimer: ReturnType<typeof setTimeout> | undefined;
  let resolveDone!: (code: number) => void;
  const done = new Promise<number>(resolve => { resolveDone = resolve; });
  function stop(code = 0) {
    if (stopping) return;
    stopping = true;
    exitCode = code;
    for (const child of children) child.kill('SIGTERM');
    forceTimer = setTimeout(() => { exitCode = 1; for (const child of children) child.kill('SIGKILL'); }, 7000);
    forceTimer.unref();
    if (children.size === 0) resolveDone(exitCode);
  }
  const signal = () => stop();
  process.on('SIGTERM', signal);
  process.on('SIGINT', signal);
  function launch(role: Component) {
    const child = spawn(process.execPath, [join(root, 'dist', `${role}.cjs`)], {cwd: root, stdio: 'inherit', env: {
      ...environment, NODE_ENV: 'production',
      ...(role === 'hub' ? hubEnvironment(dir, config!) : {COLLECTOR_MODE: 'real', COLLECTOR_STATE_FILE: join(dir, 'collector', 'state.json'),
        COLLECTOR_HEALTH_FILE: join(dir, 'run', 'collector-health.json')}),
    }});
    children.add(child);
    child.once('error', () => { console.error(`${role} could not start.`); stop(1); });
    child.once('close', code => {
      children.delete(child);
      if (!stopping) stop(code || 1);
      if (children.size === 0) resolveDone(exitCode);
    });
  }
  try {
    for (const role of selected) launch(role);
    if (selected.includes('hub')) console.log(`Open ${config.origin ?? `http://127.0.0.1:${config.port}`}`);
    return await done;
  } finally {
    if (forceTimer) clearTimeout(forceTimer);
    process.off('SIGTERM', signal);
    process.off('SIGINT', signal);
    for (const release of releaseLeases.reverse()) release();
  }
}
