import { existsSync, readFileSync, rmSync } from 'node:fs';
import { basename, dirname, join, resolve } from 'node:path';
import { parseArgs } from 'node:util';
import { createInterface } from 'node:readline';
import { Writable } from 'node:stream';
import { enrollCollector } from '../../collector/enroll';
import { parseCollectorConfig } from '../../collector/config';
import {
  COMPONENTS, DEFAULT_SHARING, componentEnabled, configureComponent, connectUrl, enabledComponents,
  initialize, installationNeedsReset, installationRole, pairColocatedCollector, readPrivate, readSetup, validateSetupOptions,
  stateDirectory, type Component,
} from './config';
import { resetInstallation } from './reset';
import { uninstallInstallation } from './uninstall';
import { prepareMutation } from './mutation';
import { printHelp, printSuccess, printWarning, printError, bold, colorize } from './output';
import { combinedLeasePath, componentLeasePath, runInstallation } from './runtime';
import { activatePackage, enableBootStartup, installAlias, serviceActive, serviceStatus, showLogs, stagePackage, startupStatus, systemctl, unitName, unitPath, writeService } from './services';
import { diagnoseInstallation, formatDoctorReport } from './doctor';
import { upgradeInstallation, rollbackInstallation } from './upgrade';
import { emptyRuntimeState, readRuntimeState, runtimeVersion, writeRuntimeState } from './release-state';
import { recoverAdminPassword } from '../operations/recovery';
import { acquireProcessLease } from '../runtime/process-lease';
import { runSystemCommand } from './system';
import { selectInstallation, systemInstallationPresent } from './selection';
import { collectorEnrolled, startInstallation } from './lifecycle';
import { probeHealth, healthTarget } from './health';
import { configureAccess, planAccessChange } from './access';

const ROOT = resolve(__dirname, '..');
const HELP = `Rigscope — your machines, in view

Usage: rigscope <command> [options]

  (no command) Show status; never starts or installs anything
  try         Run an isolated local trial in this terminal (port 8082)
  install     Install, enable and start the selected components
  enable      Complete setup, enable startup and start hub|collector
  disable     Stop and disable hub|collector; keep settings and data
  connect URL Join a hub; prompts privately, then starts an installed collector
  configure   Change local/HTTPS access or the local hub port
  start       Start enabled components, or the named component
  stop        Stop enabled components, or the named component
  restart     Restart enabled components, or the named component
  status      Show component state, hub, destinations and readiness
  logs        Show service logs for enabled components (--follow for live output)
  doctor      Diagnose runtime, service, database, ports, destinations and sensors
  upgrade     Update in place, or reset an older install and install latest
  rollback    Restore the previous runtime with its paired database backup
  recover-password
              Reset a managed user password; coordinate stopping the hub
  uninstall   Remove services, command and all saved data (default)
  system      Optional system installation (plan first; apply requires sudo)
  config      Show the private configuration path

Options:
  --state-dir PATH           Independent installation directory
  --scope user|system        Select an installation when both scopes exist
  --role local|hub|collector Initial preset; defaults to local
  --port PORT               Local hub port; defaults to 8081 (install/configure)
  --origin https://HOST     HTTPS access (install/configure/enable hub)
  --local                   Restore localhost access (configure only)
  --socket PATH             Private proxy socket (install/configure/enable hub)
  --foreground              Run without systemd (start only)
  --enable                  Enable login startup (default for install)
  --boot                    Enable startup at boot and operation after logout
  --no-enable               Do not enable login startup (install only)
  --no-start                Install/connect/enable/reset upgrade without starting
  --follow                  Follow service logs
  --token-file PATH         Read the grant from a private file
  --keep-data               Retain saved data when uninstalling (opt in)
  --purge                   Delete all saved data (the uninstall default)
  --yes                     Approve stopping running processes without a prompt
  --no-restart              Leave processes stopped after a change
  --replace                 Replace this collector's enrollment at the hub
  --from PACKAGE            Upgrade from a local .tgz or npm package spec
  --timeout SECONDS         Upgrade/rollback health deadline; defaults to 25
  --backup PATH             Explicit rollback database copy when none is paired
  --user NAME               Managed user for recover-password; defaults to owner
  --password-file PATH      Read a recovery password from a private file
  --json                    Machine-readable doctor output

Examples:
  rigscope install --boot
  rigscope try
  rigscope install --role hub
  rigscope enable collector
  rigscope install --role collector
  rigscope connect https://hub.example.com
  rigscope configure --origin https://hub.example.com
  rigscope configure --port 8086
  rigscope start collector
  rigscope doctor
  rigscope upgrade --from ./rigscope-VERSION.tgz
  rigscope rollback
  rigscope recover-password --user owner
  rigscope install --role hub --origin https://hub.example.com

Requires Linux + Node.js 26+. Remote hubs need an HTTPS proxy to the private
socket. Supports many machines per hub, bounded by hub resources, and eight
hubs per collector.
`;

async function readGrant(): Promise<string> {
  if (!process.stdin.isTTY || !process.stdout.isTTY) throw new Error('Use --token-file with a private file when no terminal is available.');
  process.stdout.write('One-time grant: ');
  // readline still handles editing, while the writable suppresses secret echo.
  const sink = new Writable({write(_chunk, _encoding, callback) { callback(); }});
  const reader = createInterface({input: process.stdin, output: sink, terminal: true});
  try {
    return await new Promise<string>((resolveGrant, reject) => {
      reader.once('line', value => resolveGrant(value.trim()));
      reader.once('close', () => reject(new Error('Grant entry cancelled.')));
      reader.once('SIGINT', () => reject(new Error('Grant entry cancelled.')));
    });
  } finally { reader.close(); process.stdout.write('\n'); }
}
async function readPassword(): Promise<string> {
  if (!process.stdin.isTTY || !process.stdout.isTTY) {
    throw new Error('Use --password-file with a private (0600) file when no terminal is available.');
  }
  const sink = new Writable({write(_chunk, _encoding, callback) { callback(); }});
  const reader = createInterface({input: process.stdin, output: sink, terminal: true});
  const ask = (label: string): Promise<string> => new Promise<string>((resolveValue, reject) => {
    const line = (value: string): void => {
      reader.off('line', line);
      resolveValue(value);
    };
    reader.once('line', line);
    reader.once('close', () => reject(new Error('Password entry cancelled.')));
    reader.once('SIGINT', () => reject(new Error('Password entry cancelled.')));
    process.stdout.write(label);
  });
  try {
    const first = await ask('New password: ');
    process.stdout.write('\n');
    const second = await ask('Repeat password: ');
    process.stdout.write('\n');
    if (first !== second) throw new Error('Passwords do not match.');
    return first;
  } finally { reader.close(); }
}
function passwordFromFile(path: string): string {
  const raw = readPrivate(resolve(path));
  return raw.replace(/\r?\n$/, '');
}
function healthDeadline(value: string | undefined): number | undefined {
  if (value === undefined) return undefined;
  if (!/^[1-9]\d*$/.test(value)) throw new Error('--timeout must be a whole number of seconds.');
  const seconds = Number(value);
  if (!Number.isSafeInteger(seconds) || seconds < 1 || seconds > 120) throw new Error('--timeout must be between 1 and 120 seconds.');
  return seconds * 1000;
}
function shellQuote(value: string): string {
  return "'" + value.replace(/'/g, "'\\''") + "'";
}
function label(component: Component): string {
  return component === 'hub' ? 'Hub' : 'Collector';
}
function parseComponent(value: string | undefined): Component | undefined {
  if (value === undefined) return undefined;
  if (value === 'hub' || value === 'collector') return value;
  throw new Error('Choose hub or collector.');
}
function requiredComponent(value: string | undefined): Component {
  const component = parseComponent(value);
  if (!component) throw new Error('Choose hub or collector.');
  return component;
}
function targets(config: ReturnType<typeof readSetup>, component: Component | undefined): Component[] {
  if (component) {
    if (!componentEnabled(config!, component)) throw new Error(`${label(component)} is disabled. Run rigscope enable ${component}.`);
    return [component];
  }
  const enabled = enabledComponents(config!);
  if (enabled.length === 0) throw new Error('No components are enabled. Run rigscope enable hub or rigscope enable collector.');
  return enabled;
}
function announce(setup: ReturnType<typeof initialize>, dir: string): void {
  if (setup.password) {
    console.log(`Username: owner\nPassword: ${setup.password}`);
    printWarning('Save this password; it is shown once.');
  }
  console.log(`Configuration: ${join(dir, 'config.json')}`);
}
async function main(): Promise<number> {
  const argv = process.argv.slice(2);
  if (argv[0] === 'system') return runSystemCommand(argv.slice(1));
  if (argv[0] === 'help' && argv[1] === 'system') return runSystemCommand(['help']);
  const {values, positionals} = parseArgs({options: {
    'state-dir': {type: 'string'}, role: {type: 'string'}, port: {type: 'string'}, origin: {type: 'string'}, socket: {type: 'string'},
    'token-file': {type: 'string'}, from: {type: 'string'}, foreground: {type: 'boolean'}, enable: {type: 'boolean'}, replace: {type: 'boolean'},
    timeout: {type: 'string'}, backup: {type: 'string'}, user: {type: 'string'}, 'password-file': {type: 'string'}, json: {type: 'boolean'},
    scope: {type: 'string'}, boot: {type: 'boolean'}, 'no-enable': {type: 'boolean'}, 'no-start': {type: 'boolean'}, follow: {type: 'boolean'},
    local: {type: 'boolean'}, purge: {type: 'boolean'}, 'keep-data': {type: 'boolean'},
    yes: {type: 'boolean', short: 'y'}, 'no-restart': {type: 'boolean'},
    help: {type: 'boolean', short: 'h'}, version: {type: 'boolean', short: 'v'},
  }, allowPositionals: true});
  if (values.help || positionals[0] === 'help') { printHelp(HELP); return 0; }
  if (values.version) { console.log((JSON.parse(readFileSync(join(ROOT, 'package.json'), 'utf8')) as {version: string}).version); return 0; }
  if (process.platform !== 'linux') throw new Error('Rigscope currently supports Linux.');
  let command = positionals[0] ?? 'status';
  const known = ['try','install','enable','disable','connect','configure','start','stop','restart','status','logs','config','doctor','upgrade','rollback','recover-password','uninstall','run'];
  if (!known.includes(command)) throw new Error('Unknown command. Run rigscope --help.');
  const componentCommands = ['enable','disable','start','stop','restart','logs','run'];
  const maxPositionals = command === 'connect' || componentCommands.includes(command) ? 2 : 1;
  if (positionals.length > maxPositionals) throw new Error('Unexpected argument. Run rigscope --help.');
  const component = command === 'connect' ? undefined : parseComponent(positionals[1]);
  const permitted: Record<string, string[]> = {
    try: ['role','port'], install: ['role','port','origin','socket','enable','boot','no-enable','no-start','from'],
    connect: ['token-file','replace','no-start'], start: ['foreground'],
    enable: ['origin','socket','port','boot','no-start'],
    logs: ['follow'], uninstall: ['purge','keep-data'],
    configure: ['origin','socket','local','port'],
    doctor: ['json'], upgrade: ['from','timeout','no-start'], rollback: ['backup','timeout'],
    'recover-password': ['user','password-file','enable'],
  };
  const mutationCommands = ['install','configure','connect','upgrade','rollback','recover-password','uninstall'];
  for (const name of mutationCommands) permitted[name] = [...(permitted[name] ?? []), 'yes', 'no-restart'];
  if (values.purge && values['keep-data']) throw new Error('Choose --keep-data or --purge, not both.');
  for (const [key, value] of Object.entries(values)) {
    if (value !== undefined && key !== 'state-dir' && key !== 'scope' && !(permitted[command] ?? []).includes(key)) throw new Error(`--${key} is not available for ${command}.`);
  }
  // Browser-copied root URLs commonly include '/'; persist the strict origin.
  // Remove only one final slash, so paths, query strings and other aliases
  // still pass through the hub's existing canonical-origin validation.
  if (values.origin?.endsWith('/')) values.origin = values.origin.slice(0, -1);
  const managedState = basename(ROOT) === 'rigscope' && basename(dirname(ROOT)) === 'node_modules' && basename(resolve(ROOT, '../..')) === 'runtime'
    ? resolve(ROOT, '../../..') : undefined;
  const dir = stateDirectory(values['state-dir'] ?? (command === 'try' ? `${stateDirectory()}-trial` : process.env.RIGSCOPE_STATE_DIR ?? managedState));
  const explicitState = Boolean(values['state-dir'] || process.env.RIGSCOPE_STATE_DIR || managedState || command === 'try' || command === 'run');
  const scope = selectInstallation({scope: values.scope ?? (explicitState ? 'user' : undefined), explicitState: Boolean(values['state-dir']),
    userPresent: existsSync(join(dir, 'config.json')), systemPresent: systemInstallationPresent()});
  if (scope === 'system') {
    if (command === 'status' || command === 'logs') {
      return runSystemCommand([command, ...(positionals[1] ? [positionals[1]] : []), ...(values.follow ? ['--follow'] : [])]);
    }
    throw new Error(`System installation selected. Use rigscope system ${command} (mutations require sudo and --apply), or --scope user for a user installation.`);
  }
  if (command === 'config') { console.log(join(dir, 'config.json')); return 0; }
  // An incompatible installation is replaced, not upgraded. The install path
  // stages and validates the replacement before any destructive work.
  if (command === 'upgrade' && installationNeedsReset(dir)) {
    values.role = installationRole(dir);
    values.from = values.from ?? 'rigscope@latest';
    command = 'install';
  }
  let selected: Component[] = [...COMPONENTS];
  if (command === 'configure') {
    const plan = planAccessChange(dir, {origin: values.origin, socket: values.socket, local: values.local, port: values.port});
    selected = plan.changed ? (plan.collectorChanged ? [...COMPONENTS] : ['hub']) : [];
  }
  if (command === 'connect') {
    if (!positionals[1]) throw new Error('Use rigscope connect https://your-hub.');
    connectUrl(positionals[1]);
    selected = ['collector'];
  }
  if (command === 'recover-password') selected = ['hub'];
  if (command === 'install') {
    validateSetupOptions({role: values.role, port: values.port, origin: values.origin, socket: values.socket});
    if (!installationNeedsReset(dir) && readSetup(dir)) {
      initialize(dir, {role: values.role, port: values.port, origin: values.origin, socket: values.socket});
    }
  }
  // Validate input before interrupting a running installation.
  if (command === 'upgrade' || command === 'rollback') healthDeadline(values.timeout);
  if (command === 'upgrade' && values['no-start']) throw new Error('Use --no-restart to upgrade without resuming services.');
  if (command === 'install') {
    if (values.enable && values['no-enable']) throw new Error('Choose --enable or --no-enable.');
    if (values.boot && values['no-enable']) throw new Error('--boot requires startup to be enabled.');
  }
  const grant = command === 'connect'
    ? (values['token-file'] ? readPrivate(resolve(values['token-file'])).trim() : await readGrant()) : undefined;
  let recoveryPassword: string | undefined;
  if (command === 'recover-password') {
    const saved = readSetup(dir);
    if (!saved || !componentEnabled(saved, 'hub')) throw new Error('This installation has no hub component.');
    recoveryPassword = values['password-file'] ? passwordFromFile(values['password-file']) : await readPassword();
  }
  const mutation = mutationCommands.includes(command) && selected.length > 0
    ? await prepareMutation(dir, selected, {yes: values.yes, noRestart: values['no-restart'] || values['no-start'],
        uninstall: command === 'uninstall', managesServices: command === 'upgrade' || command === 'rollback'}) : undefined;
  const execute = async (): Promise<number> => {
    if (command === 'configure') {
      if (!readSetup(dir)) throw new Error('No saved installation. Run rigscope install first.');
      const plan = planAccessChange(dir, {origin: values.origin, socket: values.socket, local: values.local, port: values.port});
      if (!plan.changed) { printSuccess(`Configuration unchanged: ${plan.url}`); return 0; }
      const releases: Array<() => void> = [];
      try {
        // The installation-wide lease detects a foreground command that owns both roles.
        releases.push(acquireProcessLease(combinedLeasePath(dir)));
        releases.push(acquireProcessLease(componentLeasePath(dir, 'hub')));
        if (plan.collectorChanged) releases.push(acquireProcessLease(componentLeasePath(dir, 'collector')));
        const result = configureAccess(dir, {origin: values.origin, socket: values.socket, local: values.local, port: values.port});
        printSuccess(`${result.changed ? 'Configuration updated' : 'Configuration unchanged'}: ${result.url}`);
        if (result.backup) console.log(`Previous settings: ${result.backup}`);
        if (values.origin) console.log(`Forward ${result.url} to Unix socket ${readSetup(dir)!.socket}.`);
        if (values['no-restart']) console.log('Left stopped. Run rigscope start when ready.');
      } finally { for (const release of releases.reverse()) release(); }
      return 0;
    }
    if (command === 'try') {
      if (values.role && values.role !== 'local') throw new Error('Use install for a hub or collector.');
      if (existsSync(join(dir, 'runtime'))) throw new Error('This is an installed runtime. Use rigscope start, or choose a separate --state-dir for a trial.');
      const setup = initialize(dir, {role: 'local', port: values.port ?? '8082'});
      console.log('Trial: foreground hub + collector. Ctrl+C stops both.');
      announce(setup, dir);
      return runInstallation(ROOT, dir);
    }
    if (command === 'connect') {
      if (!positionals[1]) throw new Error('Use rigscope connect https://your-hub.');
      const url = connectUrl(positionals[1]);
      parseCollectorConfig({COLLECTOR_HUBS: JSON.stringify([{url, token: 'validation', sharing: DEFAULT_SHARING}])});
      // Read/validate URL and grant before creating local state or consuming a grant.
      const token = grant!;
      const existing = readSetup(dir);
      // Validate the running service and complete enrollment before enabling the
      // component, so a rejected grant or busy collector leaves settings intact.
      if (!existing) configureComponent(dir, 'collector', {enabled: false, pairColocated: false});
      const releases: Array<() => void> = [];
      try {
        releases.push(acquireProcessLease(componentLeasePath(dir, 'collector')));
        releases.push(acquireProcessLease(join(dir, 'collector', 'collector-runtime.lock')));
        await enrollCollector(join(dir, 'collector', 'state.json'), url, token, DEFAULT_SHARING, values.replace);
      } finally { for (const release of releases.reverse()) release(); }
      // Explicit remote enrollment never creates a colocated destination.
      configureComponent(dir, 'collector', {enabled: true, pairColocated: false});
      if (existsSync(join(dir, 'runtime'))) {
        writeService(dir, 'collector');
        systemctl(dir, 'enable', false, 'collector');
      }
      if (!values['no-start'] && !values['no-restart'] && existsSync(unitPath(dir, 'collector'))) {
        console.log(`Hub connected. Background service ${await startInstallation(dir, 'collector')}.`);
      } else {
        console.log(`Hub connected. Run rigscope ${existsSync(unitPath(dir, 'collector')) ? 'start collector' : `install --state-dir ${shellQuote(dir)}`}.`);
      }
      return 0;
    }
    if (command === 'enable' || command === 'disable') {
      const target = requiredComponent(positionals[1]);
      const existing = readSetup(dir);
      if (!existing) throw new Error('No saved installation. Run rigscope install first.');
      if (command === 'enable') {
        if (!existsSync(join(dir, 'runtime'))) throw new Error('Install the runtime first: rigscope install --no-start.');
        if (target === 'hub' && (existing.users.length > 0 || existing.origin) &&
            (values.origin !== undefined || values.socket !== undefined || values.port !== undefined)) {
          const nextOrigin = values.origin !== undefined ? values.origin : existing.origin;
          const nextSocket = values.origin !== undefined ? (values.socket ?? join(dir, 'run', 'hub.sock'))
            : (values.socket ?? existing.socket);
          const nextPort = values.port !== undefined ? Number(values.port) : existing.port;
          if (nextOrigin !== existing.origin || nextSocket !== existing.socket || nextPort !== existing.port) {
            throw new Error('Change hub access with rigscope configure.');
          }
        }
        // Complete live pairing before flipping the enabled flag. A failed
        // grant or enrollment therefore leaves the collector disabled.
        if (target === 'collector' && !collectorEnrolled(dir) &&
            existing.users.length > 0 && componentEnabled(existing, 'hub')) {
          await pairColocatedCollector(dir);
        }
        const result = configureComponent(dir, target, {enabled: true, origin: values.origin, socket: values.socket, port: values.port});
        if (result.password) console.log(`Username: owner\nPassword: ${result.password}\nSave this password; it is shown once.`);
        writeService(dir, target);
        installAlias(dir, ROOT);
        systemctl(dir, 'enable', false, target);
        if (values.boot) enableBootStartup();
        console.log(`${label(target)} enabled.`);
        if (target === 'collector' && !collectorEnrolled(dir)) {
          console.log('Collector awaiting connection. Run rigscope connect https://your-hub.');
        } else if (!values['no-start']) {
          console.log(`${label(target)} service ${await startInstallation(dir, target)}.`);
        } else console.log(`Run rigscope start ${target} to start it.`);
      } else {
        configureComponent(dir, target, {enabled: false});
        if (serviceStatus(dir, target).installed) {
          systemctl(dir, 'stop', false, target);
          systemctl(dir, 'disable', false, target);
        }
        console.log(`${label(target)} disabled; configuration and data retained.`);
      }
      return 0;
    }
    if (command === 'install') {
      if (values.enable && values['no-enable']) throw new Error('Choose --enable or --no-enable.');
      if (values.boot && values['no-enable']) throw new Error('--boot requires startup to be enabled.');
      const resetting = installationNeedsReset(dir);
      if (resetting) values.role = values.role ?? installationRole(dir);
      // Registering a trial is allowed only after its foreground processes stop.
      // A reset coordinates its own shutdown and leases.
      const release = resetting || (serviceActive(dir) && existsSync(join(dir, 'runtime')))
        ? () => {} : acquireProcessLease(combinedLeasePath(dir));
      let alias: string;
      let installedVersion: string;
      try {
        if (resetting) {
          console.log('Old installation state detected. Resetting configuration and history, then installing the replacement…');
          // Stage and validate the replacement before the coordinated reset
          // stops anything or deletes state. The incompatible runtime is never
          // reused.
          validateSetupOptions({role: values.role, port: values.port, origin: values.origin, socket: values.socket});
          const source = values.from ? (/^[./]/.test(values.from) ? resolve(values.from) : values.from) : undefined;
          const stage = stagePackage(ROOT, dir, source);
          let setup: ReturnType<typeof initialize>;
          try {
            setup = resetInstallation({
              dir,
              stage,
              setup: {role: values.role, port: values.port, origin: values.origin, socket: values.socket},
            });
          } catch (error) {
            // The replacement was not activated. Remove only the staged tree so
            // a retry is not blocked; the original installation was validated
            // before any delete and remains intact on a rejected package.
            rmSync(stage, {recursive: true, force: true});
            throw error;
          }
          announce(setup, dir);
          installedVersion = runtimeVersion(join(dir, 'runtime'));
          writeRuntimeState(dir, {...(readRuntimeState(dir) ?? emptyRuntimeState(dir)), currentVersion: installedVersion});
        } else {
          const setup = initialize(dir, {role: values.role, port: values.port, origin: values.origin, socket: values.socket});
          announce(setup, dir);
          if (existsSync(join(dir, 'runtime'))) {
            if (values.from) throw new Error('Runtime already installed. Use rigscope upgrade --from <package>.');
          } else {
            console.log('Installing runtime…');
            const source = values.from ? (/^[./]/.test(values.from) ? resolve(values.from) : values.from) : undefined;
            const stage = stagePackage(ROOT, dir, source);
            activatePackage(dir, stage);
          }
          installedVersion = runtimeVersion(join(dir, 'runtime'));
          writeRuntimeState(dir, {...(readRuntimeState(dir) ?? emptyRuntimeState(dir)), currentVersion: installedVersion});
        }
        writeService(dir);
        alias = installAlias(dir, ROOT);
        const installedConfig = readSetup(dir)!;
        const components = enabledComponents(installedConfig);
        for (const target of components) systemctl(dir, values['no-enable'] ? 'disable' : 'enable', false, target);
        if (values.boot) enableBootStartup();
        console.log(`Runtime: rigscope ${installedVersion}\nCommand: ${alias}\nServices: ${components.map(target => unitName(dir, target)).join(', ')}`);
        if (installedConfig.origin) console.log(`Forward ${installedConfig.origin} to Unix socket ${installedConfig.socket}.`);
      } finally { release(); }
      let installed = readSetup(dir)!;
      if (enabledComponents(installed).includes('collector') && !collectorEnrolled(dir) &&
          installed.users.length > 0 && componentEnabled(installed, 'hub')) {
        try { await pairColocatedCollector(dir); }
        catch (error) { console.log(`Collector pairing deferred: ${error instanceof Error ? error.message : 'try rigscope enable collector'}`); }
        installed = readSetup(dir)!;
      }
      const components = enabledComponents(installed);
      const awaiting = components.includes('collector') && !collectorEnrolled(dir);
      if (awaiting) {
        console.log(`Installed; collector waiting for enrollment. Run ${shellQuote(alias)} connect https://your-hub.`);
      }
      if (!values['no-start'] && !values['no-restart'] && !mutation?.running.length) {
        let started = 0;
        let already = 0;
        for (const target of components) {
          if (target === 'collector' && !collectorEnrolled(dir)) continue;
          const state = await startInstallation(dir, target);
          if (state === 'started') started += 1; else already += 1;
        }
        if (started > 0) printSuccess('Background service started. You can close this terminal.');
        else if (already > 0) console.log('Background service already running.');
        if (components.includes('hub')) console.log(`Open ${installed.origin ?? `http://127.0.0.1:${installed.port}`}`);
      } else {
        const active = components.some(target => serviceActive(dir, target));
        console.log(active ? 'Installed; running in the background.' : `Installed; stopped. Run ${shellQuote(alias)} start.`);
      }
      const startup = startupStatus(dir);
      console.log(`Startup: ${!startup.enabled ? 'manual' : startup.linger ? 'at boot; continues after logout' : 'at login; use install --boot to continue after logout'}`);
      if (!(process.env.PATH ?? '').split(':').includes(resolve(alias, '..'))) console.log(`Add ${resolve(alias, '..')} to PATH, or use the command path above.`);
      return 0;
    }
    if (command === 'doctor') {
      const report = await diagnoseInstallation(dir);
      console.log(values.json ? JSON.stringify(report, null, 2) : formatDoctorReport(report));
      return report.ok ? 0 : 1;
    }
    if (command === 'uninstall') {
      uninstallInstallation(dir, !values['keep-data']);
      printSuccess(!values['keep-data']
        ? `User services, command and all saved data removed from ${dir}.`
        : `User services, command and runtimes removed. Configuration, history and backups remain at ${dir}. Run install to register them again, or uninstall to delete all saved data.`);
      return 0;
    }
    const config = readSetup(dir);
    if (!config) {
      if (command === 'status') {
        console.log(`Rigscope ${JSON.parse(readFileSync(join(ROOT, 'package.json'), 'utf8')).version}\nNo user installation.\nInstall background monitoring: rigscope install --boot\nConnect to a hub: rigscope install --role collector --boot\nTry in this terminal: rigscope try`);
        return 0;
      }
      throw new Error('No installation found. Run rigscope install, or rigscope try for a foreground trial.');
    }
    if (command === 'run' || (command === 'start' && values.foreground)) return runInstallation(ROOT, dir, component);
    if (command === 'status') {
      const startup = startupStatus(dir);
      console.log(`${bold('Scope')}: user\n${bold('Role')}: ${config.role}\n${bold('Components')}:`);
      for (const target of COMPONENTS) {
        const service = serviceStatus(dir, target);
        const state = service.active ? 'active' : service.subState;
        console.log(`  ${bold(label(target))}: ${componentEnabled(config, target) ? 'enabled' : 'disabled'}, ${colorize(state, service.active ? 'green' : 'gray')}, startup ${service.enabled ? 'on' : 'off'}${service.installed ? '' : ' (not installed)'}`);
        if (service.active && service.execMainPid) console.log(`    PID ${service.execMainPid}`);
      }
      console.log(`Configuration: ${join(dir, 'config.json')}\nStartup: ${!startup.enabled ? 'manual' : startup.linger ? 'at boot; continues after logout' : 'at login only'}\nCLI version: ${JSON.parse(readFileSync(join(ROOT, 'package.json'), 'utf8')).version}`);
      if (existsSync(join(dir, 'runtime'))) {
        try { console.log(`Runtime: rigscope ${runtimeVersion(join(dir, 'runtime'))}`); } catch { console.log('Runtime: invalid (run rigscope doctor)'); }
      }
      if (existsSync(join(dir, 'runtime-previous'))) {
        try { console.log(`Previous runtime: rigscope ${runtimeVersion(join(dir, 'runtime-previous'))}`); } catch { console.log('Previous runtime: invalid'); }
      }
      const releaseState = readRuntimeState(dir);
      const paired = releaseState ? Object.values(releaseState.backups).filter(record => existsSync(record.path)) : [];
      if (paired.length > 0) console.log(`Paired database backups: ${paired.length}`);
      const hubConfigured = config.users.length > 0;
      if (hubConfigured) console.log(`Hub: ${config.origin ?? `http://127.0.0.1:${config.port}`}`);
      if (hubConfigured && componentEnabled(config, 'hub') && serviceStatus(dir, 'hub').active) {
        const health = await probeHealth(healthTarget({role: 'local', port: config.port, origin: config.origin, socket: config.socket}));
        console.log(`Health: ${health.ok ? 'ok' : health.error}\nRunning version: ${health.body?.version ?? 'unavailable'}`);
      }
      const state = join(dir, 'collector', 'state.json');
      if (existsSync(state)) {
        const saved = JSON.parse(readPrivate(state)) as {destinations: {url: string}[]};
        console.log(`Destinations: ${saved.destinations.length}/8${saved.destinations.length === 0 ? ' (awaiting connection)' : ''}`);
        for (const destination of saved.destinations) console.log(`  ${destination.url}`);
      } else if (componentEnabled(config, 'collector')) {
        console.log('Destinations: 0/8 (awaiting connection)');
      }
      return 0;
    }
    if (command === 'logs') { showLogs(dir, values.follow, component); return 0; }
    if (command === 'start') {
      for (const target of targets(config, component)) {
        if (target === 'collector' && !collectorEnrolled(dir)) {
          console.log('Collector awaiting connection. Run rigscope connect https://your-hub.');
          continue;
        }
        console.log(`${label(target)} service ${await startInstallation(dir, target)}.`);
      }
      return 0;
    }
    if (command === 'stop') {
      for (const target of component ? [component] : enabledComponents(config)) {
        if (!serviceStatus(dir, target).installed) { console.log(`${label(target)} service is not installed.`); continue; }
        systemctl(dir, 'stop', false, target);
        console.log(`${label(target)} service stopped.`);
      }
      return 0;
    }
    if (command === 'restart') {
      for (const target of targets(config, component)) {
        if (target === 'collector' && !collectorEnrolled(dir)) {
          console.log('Collector awaiting connection. Run rigscope connect https://your-hub.');
          continue;
        }
        if (!serviceStatus(dir, target).installed) { console.log(`${label(target)} service is not installed.`); continue; }
        systemctl(dir, 'restart', false, target);
        console.log(`${label(target)} service restarted.`);
      }
      return 0;
    }
    if (command === 'upgrade') {
      if (!existsSync(join(dir, 'runtime'))) throw new Error('Install a managed runtime before upgrading.');
      console.log('Staging upgrade…');
      const source = values.from ? (/^[./]/.test(values.from) ? resolve(values.from) : values.from) : 'rigscope@latest';
      const report = await upgradeInstallation({
        root: ROOT, dir, from: source, healthDeadlineMs: healthDeadline(values.timeout),
      });
      printSuccess(`Upgrade installed: rigscope ${report.version} (previous ${report.previousVersion}).`);
      if (report.backupPath) console.log(`Pre-upgrade database backup: ${report.backupPath}`);
      if (report.schemaVersion !== null) console.log(`Database schema: ${report.schemaVersion}`);
      if (report.verification.health) console.log(`Health: ${report.verification.health.attempts} probe(s) in ${report.verification.health.elapsedMs}ms`);
      else console.log('Health: skipped because the service was inactive; start it explicitly.');
      return 0;
    }
    if (command === 'rollback') {
      console.log('Rolling back…');
      const report = await rollbackInstallation({
        dir,
        backup: values.backup ? resolve(values.backup) : undefined,
        healthDeadlineMs: healthDeadline(values.timeout),
      });
      printSuccess(`Rolled back: rigscope ${report.version} (previous ${report.previousVersion}).`);
      if (report.restoredBackup) {
        console.log(`Restored paired database backup: ${report.restoredBackup}`);
        console.log(`The newer database was preserved at: ${report.preRestoreBackup ?? 'not available'}`);
      } else {
        console.log('Database kept: the previous runtime can read the current schema.');
      }
      if (report.verification.health) console.log(`Health: ${report.verification.health.attempts} probe(s) in ${report.verification.health.elapsedMs}ms`);
      else console.log('Health: skipped because the service was inactive; start it explicitly.');
      return 0;
    }
    if (command === 'recover-password') {
      if (!componentEnabled(config, 'hub')) throw new Error('This installation has no hub component.');
      const password = recoveryPassword!;
      const report = await recoverAdminPassword({
        database: join(dir, 'hub', 'telemetry.sqlite'),
        username: values.user ?? 'owner',
        password,
        enable: values.enable === true,
      });
      printSuccess(`Password reset for ${report.username}. Audit: ${report.auditAction} at ${report.auditAt}.`);
      console.log(`Sessions invalidated (epoch ${report.sessionEpoch}); sign in again.`);
      return 0;
    }

    return 0;
  };
  const result = await execute();
  if (result === 0) await mutation?.resume();
  return result;
}
main().then(code => { process.exitCode = code; }).catch((error: unknown) => {
  printError(`Rigscope: ${error instanceof Error ? error.message : 'Command failed.'}`);
  process.exitCode = 1;
});
