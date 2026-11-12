import {execFileSync} from 'node:child_process';
import {accessSync, constants, readFileSync} from 'node:fs';
import {delimiter, resolve} from 'node:path';

const quote = (value: string) => "'" + value.replace(/'/g, "'\\''") + "'";
type Group = {name: string; gid: number; members: string[]};
export type CollectorLaunch = {command: string; args: string[]; warning?: string};

/** True when `command` is executable somewhere on PATH. */
export function commandOnPath(command: string, path = process.env.PATH): boolean {
  const directories = (path ?? '').split(delimiter).filter(directory => directory !== '');
  // Match the C library default used when PATH is unset, rather than refusing
  // to look for helpers in the usual locations.
  if (directories.length === 0) directories.push('/usr/local/bin', '/usr/bin', '/bin');
  return directories.some(directory => {
    try {
      accessSync(resolve(directory, command), constants.X_OK);
      return true;
    } catch {
      return false;
    }
  });
}

/** sg reads the current group database, unlike the long-lived systemd user
 * manager. Only enter a group the collector owner is already a member of;
 * no password, root process, or new login session is needed.
 *
 * sg is a setuid helper from shadow-utils/util-linux and is not present on every
 * minimal distribution. A missing helper must degrade to a collector without
 * the granted groups, never stop the collector from starting: sensor data can
 * be absent, telemetry cannot.
 */
export function collectorLaunchCommand(node: string, entry: string, user: string, effective: number[],
  groups: Group[], groupHelperAvailable = true): CollectorLaunch {
  const missing = groups.find(group => group.members.includes(user) && !effective.includes(group.gid));
  if (!missing) return {command: node, args: [entry]};
  if (!groupHelperAvailable) {
    return {
      command: node,
      args: [entry],
      warning: `group ${missing.name} is granted but sg(1) is not installed; hardware sensors stay unavailable until the next login`,
    };
  }
  return {command: 'sg', args: [missing.name, '-c', `exec ${quote(node)} ${quote(entry)}`]};
}

export function collectorLaunch(node: string, entry: string): CollectorLaunch {
  const groups: Group[] = [];
  for (const rule of ['90-rigscope-rapl.rules', '90-rigscope-gpu-temps.rules']) {
    try {
      const contents = readFileSync(`/etc/udev/rules.d/${rule}`, 'utf8');
      if (!contents.includes('Managed by Rigscope')) continue;
      const name = contents.match(/chgrp\s+([a-zA-Z0-9_-]+)/)?.[1];
      if (!name) continue;
      const fields = execFileSync('getent', ['group', name], {encoding: 'utf8', timeout: 1000}).trim().split(':');
      groups.push({name, gid: Number(fields[2]), members: (fields[3] ?? '').split(',')});
    } catch { /* no grant installed */ }
  }
  if (groups.length === 0) return {command: node, args: [entry]};
  let user: string;
  try {
    user = execFileSync('id', ['-un'], {encoding: 'utf8', timeout: 1000}).trim();
  } catch {
    return {command: node, args: [entry]};
  }
  return collectorLaunchCommand(node, entry, user, process.getgroups?.() ?? [], groups, commandOnPath('sg'));
}
