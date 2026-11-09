/**
 * `rigscope gpu-temps` and `rigscope cpu-power`.
 *
 * One-command grants for the two sensor families that are not readable by an
 * unprivileged collector out of the box:
 *
 *   gpu-temps  NVIDIA junction/VRAM registers via sysfs BAR0 (`resource0`)
 *   cpu-power  CPU package energy via RAPL (`intel-rapl:N/energy_uj`)
 *
 * Both grants are the same three durable pieces: a dedicated group, group
 * membership for the accounts that run collectors, and a package-managed udev
 * rule that re-applies the group/mode whenever the kernel recreates the sysfs
 * node. `status` needs no root and answers the only question that matters —
 * can the sensors be read right now — by actually reading them.
 */
import { accessSync, constants, existsSync, readFileSync, renameSync, writeFileSync, rmSync, readdirSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { execFileSync, spawnSync } from 'node:child_process';
import {
  defaultReaders,
  discoverNvidiaDisplays,
  readGpuRegisterTemps,
} from '../../collector/sampling/gpu-temps';
import { printSuccess, printWarning, printError } from './output';
import { systemInstallationPresent } from './selection';
import { DEFAULT_SYSTEM_ACCOUNTS, RAPL_RULE_NAME, defaultSystemPaths, readSystemState } from './system';

const PACKAGE_ROOT = resolve(__dirname, '..');
const UDEV_DIR = '/etc/udev/rules.d';
const SYSFS_PCI_DIR = '/sys/bus/pci/devices';
const COLLECTOR_ACCOUNT = 'rigscope-collector';

export type HardwareCommandResult = {status: number; stdout: string; stderr: string};

/** Injectable command/filesystem host so tests drive every branch. */
export type HardwareAccessHost = {
  uid(): number;
  env: Record<string, string | undefined>;
  run(command: string, args: string[], label: string): HardwareCommandResult;
  readFile(path: string): string | null;
  writeFile(path: string, content: string): void;
  removeFile(path: string): void;
  /** Decisive probe: are this feature's sensors readable right now? */
  probe(feature: HardwareFeature): HardwareProbe;
  /** True for a system-scope installation (changes the re-login advice). */
  systemInstallationPresent(): boolean;
  /** True when a system-scope installation owns the package-power grant. */
  systemPowerEnabled(): boolean;
  /**
   * Re-run this invocation as root when that is possible (interactive terminal,
   * sudo available). Returns the child's exit status, or null to fall back to
   * the printed copy-paste hint. Omitted by test hosts.
   */
  escalate?(argv: readonly string[]): number | null;
};

export type HardwareProbe = {
  /** The machine even exposes these sensors. */
  present: boolean;
  /** This process can read them. */
  readable: boolean;
  /** Human summary for status output. */
  detail: string;
};

export type HardwareFeature = {
  id: 'gpu-temps' | 'cpu-power';
  title: string;
  group: string;
  ruleFile: string;
  /** udev subsystem re-triggered after the rule changes. */
  subsystem: string;
  grantHint: string;
  /**
   * With a system-scope collector account present, the group stays reserved
   * for that account alone: `rigscope system` fails its account audit unless
   * the power group contains exactly the dedicated collector.
   */
  exclusiveToCollector?: boolean;
};

export const HARDWARE_FEATURES: Record<HardwareFeature['id'], HardwareFeature> = {
  'gpu-temps': {
    id: 'gpu-temps',
    title: 'NVIDIA junction/VRAM register temperatures',
    group: 'rigscope-gpu',
    ruleFile: '90-rigscope-gpu-temps.rules',
    subsystem: 'pci',
    grantHint: 'junction and VRAM temperatures will appear within a few collector cycles',
  },
  'cpu-power': {
    id: 'cpu-power',
    title: 'CPU package power (RAPL)',
    group: DEFAULT_SYSTEM_ACCOUNTS.powerGroup,
    ruleFile: RAPL_RULE_NAME,
    subsystem: 'powercap',
    grantHint: 'package power will appear in the next samples',
    exclusiveToCollector: true,
  },
};

/** Path of the packaged udev rule shipped inside the installed package. */
export function packageRulePath(feature: HardwareFeature): string {
  // Walk up from dist/ or src/cli/ to the package that ships packaging/system.
  let dir = __dirname;
  for (let depth = 0; depth < 4; depth += 1) {
    dir = join(dir, '..');
    if (existsSync(join(dir, 'packaging', 'system', feature.ruleFile))) {
      return join(dir, 'packaging', 'system', feature.ruleFile);
    }
  }
  return join(PACKAGE_ROOT, 'packaging', 'system', feature.ruleFile);
}

/**
 * Re-run this exact invocation under sudo, so `rigscope gpu-temps enable`
 * works from any installation layout without the caller reconstructing a sudo
 * command. `sudo` replaces PATH with its own `secure_path`, which cannot find
 * the installer's `~/.local/bin` launcher, but the child is started by absolute
 * path and therefore needs neither the launcher nor sudo's PATH.
 *
 * Only an interactive terminal is elevated. Scripts keep the deterministic
 * printed hint. Returns null when escalation is not possible.
 */
function requestRoot(argv: readonly string[]): number | null {
  if (process.stdin.isTTY !== true) return null;
  const script = process.argv[1];
  if (script === undefined) return null;
  const result = spawnSync('sudo', ['--', process.execPath, ...process.execArgv, resolve(script), ...argv], {
    stdio: 'inherit',
    env: process.env,
  });
  if (result.error !== undefined) return null;
  if (result.signal !== null) return 1;
  return result.status ?? 1;
}

const defaultHost: HardwareAccessHost = {
  uid: () => process.getuid?.() ?? -1,
  env: process.env,
  escalate: requestRoot,
  run: (command, args, _label) => {
    try {
      const stdout = execFileSync(command, args, {encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe']});
      return {status: 0, stdout, stderr: ''};
    } catch (error) {
      const failure = error as {status?: number; stdout?: string; stderr?: string; message?: string};
      return {
        status: failure.status ?? 1,
        stdout: failure.stdout ?? '',
        stderr: (failure.stderr ?? failure.message ?? '').trim(),
      };
    }
  },
  readFile: (path) => {
    try {
      return readFileSync(path, 'utf8');
    } catch {
      return null;
    }
  },
  writeFile: (path, content) => {
    // Write beside the target and rename over it, so a failed write never
    // leaves a half-written udev rule (which would break the next boot).
    const temporary = `${path}.tmp-${process.pid}`;
    writeFileSync(temporary, content, {mode: 0o644});
    renameSync(temporary, path);
  },
  removeFile: (path) => rmSync(path, {force: true}),
  systemInstallationPresent: () => systemInstallationPresent(),
  systemPowerEnabled: () => {
    try {
      return readSystemState(defaultSystemPaths())?.power != null;
    } catch {
      return false;
    }
  },
  probe: (feature) => {
    if (feature.id === 'gpu-temps') {
      const devices = discoverNvidiaDisplays(defaultReaders);
      if (devices.length === 0) return {present: false, readable: false, detail: 'no NVIDIA display devices found'};
      const sensors = readGpuRegisterTemps(defaultReaders);
      if (sensors.size === 0) {
        // Distinguish a missing permission from an unsupported card: if any
        // device's BAR0 file is readable, the registers answered but this card
        // has no decodable thermal sensors.
        const registersReadable = devices.some((device) => {
          try {
            accessSync(`${SYSFS_PCI_DIR}/${device.bdf}/resource0`, constants.R_OK);
            return true;
          } catch {
            return false;
          }
        });
        if (registersReadable) {
          return {present: false, readable: false, detail: `${devices.length} NVIDIA device(s) found, no supported thermal registers`};
        }
        return {present: true, readable: false, detail: `${devices.length} NVIDIA device(s) found, registers not readable`};
      }
      const parts = [...sensors.values()].map((value) => {
        const pieces: string[] = [];
        if (value.junctionTemperatureC !== null) pieces.push(`junction ${value.junctionTemperatureC}C`);
        if (value.vramTemperatureC !== null) pieces.push(`VRAM ${value.vramTemperatureC}C`);
        return pieces.join(', ');
      }).filter((part) => part !== '');
      return {present: true, readable: true, detail: `${sensors.size} device(s): ${parts.join('; ')}`};
    }
    // RAPL: the same top-level package counters the collector samples
    // (hwmon.ts RAPL_ROOT). Subdomains (intel-rapl:N:M) are not sampled.
    const raplRoot = '/sys/class/powercap';
    let entries: string[] = [];
    try {
      entries = readdirSync(raplRoot);
    } catch {
      return {present: false, readable: false, detail: 'no powercap class directory (no RAPL support)'};
    }
    const packages = entries.filter((name) => /^intel-rapl:\d+$/.test(name));
    if (packages.length === 0) {
      return {present: false, readable: false, detail: 'no RAPL package devices exposed by the kernel'};
    }
    let partiallyReadable = false;
    for (const name of packages) {
      const energy = defaultHost.readFile(`${raplRoot}/${name}/energy_uj`);
      if (energy === null) continue;
      const maxRange = defaultHost.readFile(`${raplRoot}/${name}/max_energy_range_uj`);
      if (maxRange === null) {
        // Power needs the range to unwrap the counter; energy alone is not
        // enough to compute watts, so do not claim readability.
        partiallyReadable = true;
        continue;
      }
      const label = defaultHost.readFile(`${raplRoot}/${name}/name`)?.trim();
      return {
        present: true,
        readable: true,
        detail: `${name}${label ? ` (${label})` : ''}: ${energy.trim()} uJ cumulative`,
      };
    }
    if (partiallyReadable) {
      return {
        present: true,
        readable: false,
        detail: `${packages.length} RAPL package device(s), counters only partially readable (max_energy_range_uj missing)`,
      };
    }
    return {
      present: true,
      readable: false,
      detail: `${packages.length} RAPL package device(s), counters not readable`,
    };
  },
};

const SUBCOMMANDS = ['status', 'enable', 'disable'] as const;
type Subcommand = (typeof SUBCOMMANDS)[number];

const HELP = `Usage: rigscope gpu-temps status|enable|disable
       rigscope cpu-power status|enable|disable

Grants read access to hardware sensors the unprivileged collector cannot
reach by default. status never changes anything and requires no sudo.

  status    Report group, rule and an actual sensor read
  enable    Create the group, grant collectors, install the udev rule (prompts for sudo)
  disable   Remove the rule and group memberships (prompts for sudo)

On a system-scope install, prefer ${sudoCommand('system', 'power', 'grant', '--apply')}:
it also records the grant in rigscope system state. Revoke with
${sudoCommand('system', 'power', 'revoke', '--apply')}: cpu-power disable
refuses to remove a system-managed rule so that state stays consistent.

After enabling on a user-scope install, log out and back in (or reboot):
the systemd user manager keeps the old group list until the session is
recreated. See docs/monitoring.md for details.`;

/**
 * The command to show when a mutation needs root.
 *
 * `sudo` replaces PATH with its own `secure_path`, which does not include
 * `~/.local/bin` — where the installer puts the launcher — or a Node runtime
 * installed there for the launcher's shebang. Passing the invoking user's PATH
 * through `env` keeps both resolvable, unlike a bare `sudo rigscope`.
 */
function sudoCommand(...args: string[]): string {
  return `sudo env "PATH=$PATH" ${['rigscope', ...args].join(' ')}`;
}

function groupExists(host: HardwareAccessHost, group: string): boolean {
  return host.run('getent', ['group', group], 'group lookup').status === 0;
}

function groupMembers(host: HardwareAccessHost, group: string): string[] {
  const result = host.run('getent', ['group', group], 'group lookup');
  if (result.status !== 0) return [];
  const fields = result.stdout.trim().split(':');
  return (fields[3] ?? '').split(',').map((member) => member.trim()).filter((member) => member !== '');
}

function userExists(host: HardwareAccessHost, user: string): boolean {
  return host.run('id', ['-u', user], 'user lookup').status === 0;
}

/** Accounts to grant: whoever invoked sudo, plus a system collector account. */
function granteesFor(host: HardwareAccessHost, feature: HardwareFeature): string[] {
  const collectorPresent = userExists(host, COLLECTOR_ACCOUNT);
  if (feature.exclusiveToCollector === true && collectorPresent) {
    // A system-scope install owns this group; adding the sudo user there
    // would break the `rigscope system` account audit for no benefit.
    return [COLLECTOR_ACCOUNT];
  }
  const grantees: string[] = [];
  const sudoUser = host.env.SUDO_USER;
  if (sudoUser !== undefined && sudoUser !== '' && sudoUser !== 'root') grantees.push(sudoUser);
  if (collectorPresent && !grantees.includes(COLLECTOR_ACCOUNT)) {
    grantees.push(COLLECTOR_ACCOUNT);
  }
  if (grantees.length === 0) {
    // No sudo context and no system account: current user, unless root.
    const current = host.run('id', ['-un'], 'user lookup').stdout.trim();
    if (current !== '' && current !== 'root') grantees.push(current);
  }
  return grantees;
}

/**
 * The group a Rigscope-managed udev rule chgrants to. `rigscope system`
 * renders its rule from configurable accounts, so an installed rule is
 * authoritative: grant memberships follow the rule, not the packaged
 * default, whenever a managed rule already exists.
 */
function installedRuleGroup(contents: string): string | null {
  const match = /chgrp (\S+) \/sys%p\//.exec(contents);
  return match?.[1] ?? null;
}

/** Outcome of resolving the rule file to keep or install. */
type RuleResolution =
  | {group: string; /** Contents to install, or null when an existing managed rule is kept. */ install: string | null}
  | {refused: string};

function resolveRule(host: HardwareAccessHost, feature: HardwareFeature): RuleResolution {
  const rulePath = join(UDEV_DIR, feature.ruleFile);
  const existing = host.readFile(rulePath);
  if (existing !== null) {
    if (!existing.includes('Managed by Rigscope')) {
      return {refused: `${rulePath} is not a Rigscope-managed file; refusing to overwrite it`};
    }
    // Managed (including one rendered by `rigscope system` with custom
    // accounts): keep it and let the renderer stay authoritative.
    return {group: installedRuleGroup(existing) ?? feature.group, install: null};
  }
  const source = packageRulePath(feature);
  const contents = host.readFile(source);
  if (contents === null) {
    return {refused: `packaged rule is missing (${source}); reinstall rigscope first`};
  }
  return {group: feature.group, install: contents};
}

/** Reload udev, re-trigger one subsystem and wait for the change to settle. */
function reloadUdev(host: HardwareAccessHost, subsystem: string): number {
  const reload = host.run('udevadm', ['control', '--reload-rules'], 'udev reload');
  if (reload.status !== 0) {
    printError(`udev reload failed: ${reload.stderr}`);
    return 1;
  }
  const trigger = host.run('udevadm', ['trigger', '-s', subsystem], 'udev trigger');
  if (trigger.status !== 0) {
    printError(`udev trigger failed: ${trigger.stderr}`);
    return 1;
  }
  const settle = host.run('udevadm', ['settle', '--timeout=10'], 'udev settle');
  if (settle.status !== 0) {
    printError(`udev settle failed: ${settle.stderr}`);
    return 1;
  }
  return 0;
}

function runEnable(host: HardwareAccessHost, feature: HardwareFeature, lines: string[]): number {
  if (host.uid() !== 0) {
    printError(`This mutation requires root. Run: ${sudoCommand(feature.id, 'enable')} (no change was made)`);
    return 1;
  }
  const grantees = granteesFor(host, feature);
  if (grantees.length === 0) {
    printError('Could not determine which account to grant. Run from a sudo shell of the collector user.');
    return 1;
  }

  const rule = resolveRule(host, feature);
  if ('refused' in rule) {
    printError(rule.refused);
    return 1;
  }
  const group = rule.group;
  const rulePath = join(UDEV_DIR, feature.ruleFile);
  if (rule.install === null) lines.push(`kept installed ${rulePath}`);

  if (!groupExists(host, group)) {
    const created = host.run('groupadd', ['--system', group], 'group creation');
    if (created.status !== 0) {
      printError(`group creation failed: ${created.stderr}`);
      return 1;
    }
    lines.push(`created group ${group}`);
  } else {
    lines.push(`group ${group} already exists`);
  }

  for (const grantee of grantees) {
    const members = groupMembers(host, group);
    if (members.includes(grantee)) {
      lines.push(`${grantee} is already in ${group}`);
      continue;
    }
    const added = host.run('gpasswd', ['-a', grantee, group], 'group membership');
    if (added.status !== 0) {
      printError(`adding ${grantee} to ${group} failed: ${added.stderr}`);
      return 1;
    }
    lines.push(`added ${grantee} to ${group}`);
  }

  if (rule.install !== null) {
    host.writeFile(rulePath, rule.install);
    lines.push(`installed ${rulePath}`);
  }

  const reload = reloadUdev(host, feature.subsystem);
  if (reload !== 0) return reload;
  lines.push(`udev rules reloaded and ${feature.subsystem} devices re-evaluated`);

  for (const line of lines) console.log(line);
  printSuccess(`${feature.title}: grant installed`);
  if (!host.systemInstallationPresent()) {
    printWarning('Log out and back in (or reboot) before restarting the collector: the systemd '
      + 'user manager keeps old group memberships until the session is recreated.');
  } else {
    console.log(`Restart the collector to pick up the new group: systemctl restart rigscope-collector`);
  }
  console.log(`Verify with: rigscope ${feature.id} status — then ${feature.grantHint}.`);
  return 0;
}

function runDisable(host: HardwareAccessHost, feature: HardwareFeature): number {
  if (host.uid() !== 0) {
    printError(`This mutation requires root. Run: ${sudoCommand(feature.id, 'disable')} (no change was made)`);
    return 1;
  }
  if (feature.id === 'cpu-power' && host.systemPowerEnabled()) {
    // The `rigscope system` state owns this grant; removing it here would
    // desynchronize that state from the actual rule and group membership.
    printError('This system-scope installation owns the package-power grant. '
      + `Run: ${sudoCommand('system', 'power', 'revoke', '--apply')}`);
    return 1;
  }
  const rulePath = join(UDEV_DIR, feature.ruleFile);
  const contents = host.readFile(rulePath);
  // A custom-account rule rendered by `rigscope system` names its own group;
  // revoke that group rather than the packaged default.
  const group = contents === null ? feature.group : installedRuleGroup(contents) ?? feature.group;
  if (contents === null) {
    console.log(`no rule installed at ${rulePath}`);
  } else if (!contents.includes('Managed by Rigscope')) {
    printError(`${rulePath} is not a Rigscope-managed file; refusing to remove it`);
    return 1;
  } else {
    host.removeFile(rulePath);
    console.log(`removed ${rulePath}`);
  }

  if (groupExists(host, group)) {
    for (const member of groupMembers(host, group)) {
      const removed = host.run('gpasswd', ['-d', member, group], 'group membership');
      if (removed.status !== 0) {
        printError(`removing ${member} from ${group} failed: ${removed.stderr}`);
        return 1;
      }
      console.log(`removed ${member} from ${group}`);
    }
    console.log(`group ${group} kept (delete it with groupdel if nothing else uses it)`);
  }

  if (reloadUdev(host, feature.subsystem) !== 0) return 1;
  printWarning('Existing sysfs nodes keep their current permissions until reboot or device reprobe; '
    + 'group memberships take full effect after the collector restarts.');
  return 0;
}

function runStatus(host: HardwareAccessHost, feature: HardwareFeature): number {
  const rulePath = join(UDEV_DIR, feature.ruleFile);
  const ruleContents = host.readFile(rulePath);
  const installed = ruleContents !== null;
  // `rigscope system` may render the rule for a custom power group; report the
  // group the installed rule actually names, not the packaged default.
  const group = (ruleContents === null ? null : installedRuleGroup(ruleContents)) ?? feature.group;
  const exists = groupExists(host, group);
  const members = groupMembers(host, group);
  const probe = host.probe(feature);

  console.log(`${feature.title}`);
  console.log(`  sensors:   ${probe.present ? (probe.readable ? 'readable now' : 'present, not readable') : 'not present'} (${probe.detail})`);
  console.log(`  group:     ${exists ? `${group}${members.length > 0 ? ` (members: ${members.join(', ')})` : ' (no members)'}` : `${group} (missing)`}`);
  console.log(`  udev rule: ${installed ? `installed (${rulePath})` : 'not installed'}`);

  if (probe.present && !probe.readable) {
    // The probe runs as the invoking user. On a system install the RAPL group
    // is deliberately reserved for the collector account, so this session may
    // never be able to read it even though the collector can.
    if (installed && members.includes(COLLECTOR_ACCOUNT) && host.systemInstallationPresent()) {
      printSuccess('Grant installed for the rigscope-collector account; this session cannot read it.');
      return 0;
    }
    if (installed && members.length > 0) {
      printWarning('Grant installed, but this login session does not have the group yet. '
        + 'Log out and back in (or restart the collector) to activate it.');
      return 3;
    }
    printWarning(`Not enabled. Run: rigscope ${feature.id} enable`);
    return 3;
  }
  if (!probe.present) {
    printWarning('No compatible sensors on this machine; nothing to enable.');
    return 0;
  }
  printSuccess('Enabled and reading.');
  return 0;
}

/** Entry point for `rigscope gpu-temps …` / `rigscope cpu-power …`. */
export function runHardwareAccessCommand(
  argv: readonly string[],
  host: HardwareAccessHost = defaultHost,
): number {
  const id = argv[0] === 'gpu-temps' || argv[0] === 'cpu-power' ? argv[0] : null;
  if (id === null) {
    printError('Usage: rigscope gpu-temps status|enable|disable');
    return 2;
  }
  const subcommand = argv[1] ?? 'status';
  if (argv.includes('--help') || argv.includes('-h')) {
    console.log(HELP);
    return 0;
  }
  if (!SUBCOMMANDS.includes(subcommand as Subcommand)) {
    printError(`Unknown subcommand "${String(subcommand)}". ${SUBCOMMANDS.join('|')} expected.`);
    return 2;
  }
  if (argv.length > 2) {
    printError('Unexpected argument. Run rigscope gpu-temps --help.');
    return 2;
  }
  const feature = HARDWARE_FEATURES[id];
  if ((subcommand === 'enable' || subcommand === 'disable') && host.uid() !== 0) {
    const elevated = host.escalate?.([id, subcommand]);
    if (elevated != null) return elevated;
  }
  if (subcommand === 'enable') return runEnable(host, feature, []);
  if (subcommand === 'disable') return runDisable(host, feature);
  return runStatus(host, feature);
}
