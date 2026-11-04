import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { enabledComponents, readPrivate, readSetup, type Component } from './config';
import { runtimeVersion } from './release-state';
import { migrateLegacyService, serviceStatus, systemctl, unitPath } from './services';
import { verifyRunningInstallation } from './upgrade';
import { acquireProcessLease } from '../runtime/process-lease';

export function collectorEnrolled(dir: string): boolean {
  const path = join(dir, 'collector', 'state.json');
  if (!existsSync(path)) return false;
  const state = JSON.parse(readPrivate(path)) as {destinations?: unknown[]};
  return Array.isArray(state.destinations) && state.destinations.length > 0;
}

export type StartDependencies = {
  status?: typeof serviceStatus;
  start?: typeof systemctl;
  migrate?: typeof migrateLegacyService;
  verify?: typeof verifyRunningInstallation;
};

const AWAITING_CONNECTION = 'Collector is awaiting connection. Run rigscope connect https://your-hub first.';

function assertNoCompetingRuntime(dir: string, component: Component): void {
  const locks = [
    join(dir, 'run', component === 'hub' ? 'hub.lock' : 'collector.lock'),
    join(dir, 'run', 'rigscope.lock'),
    ...(component === 'collector' ? [join(dir, 'collector', 'collector-runtime.lock')] : []),
  ];
  const releases: Array<() => void> = [];
  try {
    for (const lock of locks) releases.push(acquireProcessLease(lock));
  } catch (error) {
    throw new Error(`Another runtime still owns this installation: ${error instanceof Error ? error.message : 'lease busy'}. Stop it before starting.`);
  } finally {
    for (const release of releases.reverse()) {
      try { release(); } catch { /* lock files are advisory */ }
    }
  }
}

/**
 * Start the requested component, or every enabled component when omitted.
 * Verification covers only components this call started, so an unrelated
 * component's state cannot block it. An unconfigured collector stays awaiting
 * connection instead of entering systemd's restart loop.
 */
export async function startInstallation(
  dir: string,
  component?: Component,
  injected: StartDependencies = {},
): Promise<'started' | 'already running'> {
  const config = readSetup(dir);
  if (!config) throw new Error('No background service installed. Run rigscope install first.');
  if (!existsSync(join(dir, 'runtime'))) throw new Error('No background service installed. Run rigscope install first.');
  try { (injected.migrate ?? migrateLegacyService)(dir); }
  catch (error) {
    throw new Error(`${error instanceof Error ? error.message : 'Legacy service migration failed.'}`);
  }
  const selected = component ? [component] : enabledComponents(config);
  if (selected.length === 0) throw new Error('No enabled components. Run rigscope enable hub or rigscope enable collector.');
  for (const target of selected) {
    if (!existsSync(unitPath(dir, target))) throw new Error('No background service installed. Run rigscope install first.');
  }
  const status = injected.status ?? serviceStatus;
  const start = injected.start ?? systemctl;
  const awaiting = selected.filter(target => target === 'collector' && !collectorEnrolled(dir));
  if (component && awaiting.length > 0) throw new Error(AWAITING_CONNECTION);
  const runnable = selected.filter(target => !awaiting.includes(target));
  const already: Component[] = [];
  const started: Component[] = [];
  const failures: Component[] = [];
  for (const target of runnable) {
    if (status(dir, target).active) { already.push(target); continue; }
    assertNoCompetingRuntime(dir, target);
    try {
      start(dir, 'start', false, target);
      started.push(target);
    } catch { failures.push(target); }
  }
  if (started.length > 0) {
    try {
      await (injected.verify ?? verifyRunningInstallation)({
        dir,
        config,
        expectedVersion: runtimeVersion(join(dir, 'runtime')),
        components: started,
        requireActive: started,
      });
    } catch (error) {
      for (const target of started) {
        try { start(dir, 'stop', false, target); } catch { /* report the original failure */ }
      }
      throw new Error(`Service did not become healthy: ${error instanceof Error ? error.message : 'startup failed'}. Run rigscope logs.`);
    }
  }
  if (failures.length > 0) {
    throw new Error(`Could not start ${failures.join(', ')}. Run rigscope logs.`);
  }
  if (started.length > 0) return 'started';
  if (already.length > 0) return 'already running';
  throw new Error(AWAITING_CONNECTION);
}
