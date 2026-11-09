import test from 'node:test';
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {collectorLaunchCommand} from '../src/cli/collector-launch';

const groups = [{name: 'rigscope-rapl', gid: 975, members: ['triet']}];
test('collector refreshes newly granted membership without restarting the user manager', () => {
  assert.deepEqual(collectorLaunchCommand('/usr/bin/node', '/app/collector.cjs', 'triet', [1000], groups), {
    command: 'sg', args: ['rigscope-rapl', '-c', "exec '/usr/bin/node' '/app/collector.cjs'"],
  });
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
