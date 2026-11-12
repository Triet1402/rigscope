import test from 'node:test';
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {chmodSync, mkdtempSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {collectorLaunchCommand, commandOnPath} from '../src/cli/collector-launch';

const groups = [{name: 'rigscope-rapl', gid: 975, members: ['triet']}];
test('collector refreshes newly granted membership without restarting the user manager', () => {
  assert.deepEqual(collectorLaunchCommand('/usr/bin/node', '/app/collector.cjs', 'triet', [1000], groups), {
    command: 'sg', args: ['rigscope-rapl', '-c', "exec '/usr/bin/node' '/app/collector.cjs'"],
  });
});
test('collector still starts when the setuid group helper is unavailable', () => {
  const launch = collectorLaunchCommand('/usr/bin/node', '/app/collector.cjs', 'triet', [1000], groups, false);
  assert.equal(launch.command, '/usr/bin/node');
  assert.deepEqual(launch.args, ['/app/collector.cjs']);
  assert.match(launch.warning ?? '', /rigscope-rapl\) is granted but sg\(1\) is not installed/);
  assert.match(launch.warning ?? '', /until the next reboot/);
  // The warning is only for a membership the collector cannot enter; an
  // already-effective group never depends on the helper.
  assert.deepEqual(collectorLaunchCommand('/usr/bin/node', '/app/collector.cjs', 'triet', [975], groups, false),
    {command: '/usr/bin/node', args: ['/app/collector.cjs']});
});
test('group helper lookup follows PATH', () => {
  assert.equal(commandOnPath('sh'), true);
  assert.equal(commandOnPath('rigscope-definitely-not-a-command'), false);
  assert.equal(commandOnPath('sh', '/nonexistent'), false);
});
test('collector leaves active memberships and unrelated users alone', () => {
  for (const [user, effective] of [['triet', [975]], ['other', [1000]]] as const) {
    assert.deepEqual(collectorLaunchCommand('node', 'entry', user, [...effective], groups), {command: 'node', args: ['entry']});
  }
});
test('collector launch quotes paths containing shell syntax literally', () => {
  const path = "/tmp/a 'quoted' $(exit 9) path";
  const launch = collectorLaunchCommand(process.execPath, path, 'triet', [], groups);
  // Exercise the shell parsing used by sg, replacing only exec with printf.
  const command = launch.args[2]!.replace(/^exec /, "printf '%s\\n' ");
  assert.equal(execFileSync('/bin/sh', ['-c', command], {encoding: 'utf8'}), `${process.execPath}\n${path}\n`);
});
test('collector enters every missing hardware group, not only the first', () => {
  const both = [...groups, {name: 'rigscope-gpu-temps', gid: 976, members: ['triet']}];
  const launch = collectorLaunchCommand('/bin/echo', "/app/it's.cjs", 'triet', [1000], both);
  assert.equal(launch.command, 'sg');
  assert.deepEqual(launch.args.slice(0, 2), ['rigscope-rapl', '-c']);
  // A stand-in sg that records each group and runs its command through sh.
  const dir = mkdtempSync(join(tmpdir(), 'rigscope-sg-'));
  try {
    writeFileSync(join(dir, 'sg'), '#!/bin/sh\nprintf "%s " "$1"\nshift 2\nexec /bin/sh -c "$1"\n');
    chmodSync(join(dir, 'sg'), 0o755);
    const output = execFileSync(join(dir, 'sg'), launch.args, {encoding: 'utf8', env: {...process.env, PATH: `${dir}:/usr/bin:/bin`}});
    assert.equal(output, "rigscope-rapl rigscope-gpu-temps /app/it's.cjs\n");
  } finally { rmSync(dir, {recursive: true, force: true}); }
  const warning = collectorLaunchCommand('/bin/echo', 'entry', 'triet', [1000], both, false).warning ?? '';
  assert.match(warning, /\(rigscope-rapl, rigscope-gpu-temps\)/);
});
