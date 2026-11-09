/** Collector composition root: persistent installation, explicit enrollment,
 * one real (or explicit mock) sampling loop, independently filtered destinations.
 * Delivery is always the acknowledged telemetry protocol; unacknowledged samples
 * wait in RAM by default and only in a per-hub SQLite outbox when
 * COLLECTOR_QUEUE_MODE=durable.
 */
import { hostname as systemHostname } from 'node:os';
import { defaultStatePath, updateInstallation } from './local-state';
import { enrollCollector } from './enroll';
import { ConfigError, MOCK_HOSTNAME, parseCollectorConfig } from './config';
import { generateRunId, readBootIdentity } from './identity';
import { runCollectorLoop } from './runtime';

function log(line: string): void {
  console.log(line);
}

function describeIdentity(available: boolean, bootId: string): string {
  return available ? `boot id ${bootId}` : `boot id unavailable (reported as "${bootId}")`;
}

async function run(): Promise<number> {
  let config;
  try {
    const mode = process.env.COLLECTOR_MODE ?? 'real';
    if (mode !== 'real' && mode !== 'mock') throw new ConfigError(['COLLECTOR_MODE must be real or mock']);
    const statePath = process.env.COLLECTOR_STATE_FILE || defaultStatePath();
    if (process.argv.includes('--enroll')) {
      const sharing = JSON.parse(process.env.COLLECTOR_SHARING ?? '{"cpu":true,"memory":true}') as {cpu: boolean; memory: boolean};
      // Validate local policy before consuming a one-use grant.
      parseCollectorConfig({COLLECTOR_HUBS: JSON.stringify([{url: process.env.COLLECTOR_ENROLL_URL, token: 'validation', sharing}])});
      await enrollCollector(statePath, process.env.COLLECTOR_ENROLL_URL ?? '', process.env.COLLECTOR_ENROLL_TOKEN ?? '', sharing,
        process.argv.includes('--replace-enrollment'));
      log('collector enrollment saved; restart the collector to apply destinations');
      return 0;
    }
    const state = mode === 'real' ? updateInstallation(statePath) : undefined;
    if (process.argv.includes('--await-connection') && state?.destinations.length === 0 &&
        process.env.COLLECTOR_HUBS === undefined) {
      log('Collector awaiting connection. Run rigscope system connect https://your-hub --token-file /private/grant --apply.');
      return 0;
    }
    if (state && process.env.COLLECTOR_ID && process.env.COLLECTOR_ID !== state.collectorId) throw new ConfigError(['COLLECTOR_ID conflicts with persistent installation identity']);
    config = parseCollectorConfig({...process.env,
      COLLECTOR_ID: state?.collectorId ?? process.env.COLLECTOR_ID,
      COLLECTOR_HOSTNAME: process.env.COLLECTOR_HOSTNAME ?? (mode === 'real' ? systemHostname() : undefined),
      COLLECTOR_HUBS: process.env.COLLECTOR_HUBS ?? (state ? JSON.stringify(state.destinations) : undefined),
    });
  } catch (error) {
    if (error instanceof ConfigError) {
      for (const issue of error.issues) {
        console.error(`collector config error: ${issue}`);
      }
    } else {
      console.error(
        'collector configuration or private state could not be loaded',
      );
    }
    return 1;
  }

  const boot = readBootIdentity();
  const runId = generateRunId();
  log(
    `collector ${config.collectorId} starting runId=${runId} intervalMs=${config.intervalMs} ` +
      `destinations=${config.destinations.length}`,
  );
  const hostnameNote = config.hostnameIsMockDefault
    ? `${config.hostname} (default; set COLLECTOR_HOSTNAME to override)`
    : config.hostname;
  const isMock = process.env.COLLECTOR_MODE === 'mock';
  log(isMock ? `mock mode: synthetic metrics only; hostname=${hostnameNote}` : `real mode: host resource metrics; hostname=${config.hostname}`);
  if (isMock && !config.hostnameIsMockDefault && config.hostname === MOCK_HOSTNAME) {
    log('mock mode: hostname explicitly set to the mock default; data remains synthetic');
  }
  log(describeIdentity(boot.available, boot.bootId));

  const identity = {
    collectorId: config.collectorId,
    hostname: config.hostname,
    runId,
    bootId: boot.bootId,
  };

  // One durable runtime owns sampling, outboxes, sessions and shutdown; it
  // runs until a signal arrives and never resolves away from here.
  await runCollectorLoop(config, identity, process.env.COLLECTOR_STATE_FILE || defaultStatePath(), isMock);
  return 0;
}

/** True when this module is the executed entry (tsx or the bundled CJS). */
function isMainModule(): boolean {
  try {
    // Available under CJS (tsx without "type": "module") and in esbuild CJS bundles.
    return require.main === module;
  } catch {
    return true;
  }
}

if (isMainModule()) {
  run()
    .then((code) => {
      process.exit(code);
    })
    .catch((error: unknown) => {
      console.error(`collector fatal: ${error instanceof Error ? error.message : String(error)}`);
      process.exit(1);
    });
}
