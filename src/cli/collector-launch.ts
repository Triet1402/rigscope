import {execFileSync} from 'node:child_process';
import {readFileSync} from 'node:fs';

const quote = (value: string) => "'" + value.replace(/'/g, "'\\''") + "'";
type Group = {name: string; gid: number; members: string[]};

/** sg reads the current group database, unlike the long-lived systemd user
 * manager. Only enter a group the collector owner is already a member of;
 * no password, root process, or new login session is needed.
 */
export function collectorLaunchCommand(node: string, entry: string, user: string, effective: number[], groups: Group[]): {command: string; args: string[]} {
  const missing = groups.find(group => group.members.includes(user) && !effective.includes(group.gid));
  if (!missing) return {command: node, args: [entry]};
  return {command: 'sg', args: [missing.name, '-c', `exec ${quote(node)} ${quote(entry)}`]};
}

export function collectorLaunch(node: string, entry: string): {command: string; args: string[]} {
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
  const user = execFileSync('id', ['-un'], {encoding: 'utf8', timeout: 1000}).trim();
  return collectorLaunchCommand(node, entry, user, process.getgroups?.() ?? [], groups);
}
