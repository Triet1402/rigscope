/** Browser-safe application grouping, sorting, filtering, and tree tests. */
import assert from 'node:assert/strict';
import test from 'node:test';

import type { ProcessRow } from '../src/contracts';
import {
  aggregateApplications,
  buildTree,
  filterRows,
  readableUnitName,
  sortGroups,
  sortRows,
  type AppGroup,
} from '../src/processes/aggregate';

function row(pid: number, overrides: Partial<ProcessRow> = {}): ProcessRow {
  return {
    pid,
    ppid: 1,
    startTicks: pid * 10,
    name: `proc-${pid}`,
    user: 'alice',
    state: 'S',
    cpuPercent: 1,
    memoryRssBytes: 1024,
    gpuMemoryBytes: null,
    ioReadBytesPerSec: null,
    ioWriteBytesPerSec: null,
    threads: 1,
    unit: null,
    ...overrides,
  };
}

function group(groups: AppGroup[], key: string): AppGroup {
  const found = groups.find((candidate) => candidate.key === key);
  assert.ok(found, `expected group ${key}; got ${groups.map((candidate) => candidate.key).join(', ')}`);
  return found;
}

function pids(groupValue: AppGroup): number[] {
  return groupValue.members.map((member) => member.pid).sort((a, b) => a - b);
}

test('unit groups sum members, label readable unit names, and expose partial metrics', () => {
  const rows = [
    row(10, {
      unit: 'app-gnome-firefox-1234.scope',
      user: 'alice',
      cpuPercent: 20,
      memoryRssBytes: 100,
      ioReadBytesPerSec: 1,
      ioWriteBytesPerSec: 2,
    }),
    row(11, {
      unit: 'app-gnome-firefox-1234.scope',
      user: 'bob',
      cpuPercent: null,
      memoryRssBytes: 300,
      gpuMemoryBytes: 50,
    }),
    row(12, { unit: 'app-org.gnome.Terminal-42.scope', user: null, cpuPercent: null, memoryRssBytes: null }),
    row(13, { unit: 'example.service', user: 'carol', memoryRssBytes: null }),
    row(14, { unit: 'app-all-null-1.scope', cpuPercent: null, memoryRssBytes: null, gpuMemoryBytes: null, ioReadBytesPerSec: null, ioWriteBytesPerSec: null }),
    row(15, { unit: 'app-all-null-1.scope', cpuPercent: null, memoryRssBytes: null, gpuMemoryBytes: null, ioReadBytesPerSec: null, ioWriteBytesPerSec: null }),
  ];
  const groups = aggregateApplications(rows);

  const firefox = group(groups, 'unit:app-gnome-firefox-1234.scope');
  assert.equal(firefox.kind, 'unit');
  assert.equal(firefox.label, 'firefox');
  assert.equal(firefox.count, 2);
  assert.deepEqual(pids(firefox), [10, 11]);
  assert.equal(firefox.cpuPercent, 20);
  assert.equal(firefox.memoryRssBytes, 400);
  assert.equal(firefox.gpuMemoryBytes, 50);
  assert.equal(firefox.ioReadBytesPerSec, 1);
  assert.equal(firefox.ioWriteBytesPerSec, 2);
  assert.deepEqual(firefox.users, ['alice', 'bob']);
  assert.deepEqual(firefox.partial, {
    cpuPercent: true,
    memoryRssBytes: false,
    gpuMemoryBytes: true,
    ioReadBytesPerSec: true,
    ioWriteBytesPerSec: true,
  });

  assert.equal(group(groups, 'unit:app-org.gnome.Terminal-42.scope').label, 'org.gnome.Terminal');
  assert.equal(group(groups, 'unit:example.service').label, 'example');

  // All-null metrics stay null, not zero, and are not marked partial.
  const unknown = group(groups, 'unit:app-all-null-1.scope');
  assert.equal(unknown.cpuPercent, null);
  assert.equal(unknown.memoryRssBytes, null);
  assert.equal(unknown.gpuMemoryBytes, null);
  assert.equal(unknown.ioReadBytesPerSec, null);
  assert.equal(unknown.ioWriteBytesPerSec, null);
  assert.deepEqual(unknown.partial, {
    cpuPercent: false,
    memoryRssBytes: false,
    gpuMemoryBytes: false,
    ioReadBytesPerSec: false,
    ioWriteBytesPerSec: false,
  });

  assert.equal(readableUnitName('app-gnome-firefox-1234.scope'), 'firefox');
  assert.equal(readableUnitName('app-org.gnome.Nautilus-9.scope'), 'org.gnome.Nautilus');
  assert.equal(readableUnitName('example.service'), 'example');
});

test('tree groups stop at shells, sshd, tmux, and systemd; children roll into their app', () => {
  const rows = [
    row(1, { ppid: 0, name: 'systemd', unit: 'init.scope', user: 'root' }),
    row(100, { ppid: 1, name: 'sshd', unit: null, user: 'root' }),
    row(200, { ppid: 100, name: 'bash', unit: 'session-2.scope', user: 'alice' }),
    row(300, { ppid: 200, name: 'node', unit: 'session-2.scope', user: 'alice' }),
    row(301, { ppid: 300, name: 'worker', unit: 'session-2.scope', user: 'alice' }),
    row(400, { ppid: 200, name: 'vim', unit: 'session-2.scope', user: 'alice' }),
    row(500, { ppid: 1, name: 'chrome', unit: 'app-gnome-chrome-77.scope', user: 'alice' }),
    row(501, { ppid: 500, name: 'renderer', unit: 'session-9.scope', user: 'alice' }),
    row(600, { ppid: 1, name: 'tmux: server', unit: null, user: 'alice' }),
    row(601, { ppid: 600, name: 'zsh', unit: null, user: 'alice' }),
    row(602, { ppid: 601, name: 'tool', unit: null, user: 'alice' }),
  ];
  const groups = aggregateApplications(rows);

  assert.equal(group(groups, 'tree:1:10').label, 'systemd');
  assert.equal(group(groups, 'tree:100:1000').label, 'sshd');
  assert.equal(group(groups, 'tree:200:2000').label, 'bash');
  assert.deepEqual(pids(group(groups, 'tree:300:3000')), [300, 301], 'node app owns its worker');
  assert.deepEqual(pids(group(groups, 'tree:400:4000')), [400]);
  assert.deepEqual(
    pids(group(groups, 'unit:app-gnome-chrome-77.scope')),
    [500, 501],
    'a child with a generic cgroup rolls into its unit-grouped application',
  );
  assert.equal(group(groups, 'tree:600:6000').label, 'tmux: server');
  assert.equal(group(groups, 'tree:601:6010').label, 'zsh');
  assert.equal(group(groups, 'tree:602:6020').label, 'tool');
});

test('kernel threads form one kernel group and every row belongs to exactly one group', () => {
  const rows = [
    row(2, { ppid: 0, name: 'kthreadd', user: 'root' }),
    row(3, { ppid: 2, name: 'kworker/0:1', user: 'root' }),
    row(4, { ppid: 3, name: 'kworker/0:2', user: 'root' }),
    row(5, { ppid: 1, unit: 'app-gnome-firefox-1.scope', name: 'firefox' }),
    row(6, { ppid: 1, name: 'lonely' }),
  ];
  const groups = aggregateApplications(rows);
  const kernel = group(groups, 'kernel');
  assert.equal(kernel.kind, 'kernel');
  assert.equal(kernel.label, 'kernel');
  assert.equal(kernel.count, 3);
  assert.deepEqual(pids(kernel), [2, 3, 4]);

  assert.equal(groups.reduce((total, candidate) => total + candidate.count, 0), rows.length);
  const identities = new Set<string>();
  for (const candidate of groups) {
    for (const member of candidate.members) identities.add(`${member.pid}:${member.startTicks}`);
  }
  assert.equal(identities.size, rows.length, 'no process appears in two groups');
});

test('sorting is non-mutating, handles every key, and keeps unknown values last', () => {
  const rows = [
    row(1, { name: 'Zulu', cpuPercent: null, memoryRssBytes: 300, user: 'zoe' }),
    row(2, { name: 'alpha', cpuPercent: 30, memoryRssBytes: null, user: 'amy' }),
    row(3, { name: 'Mike', cpuPercent: 10, memoryRssBytes: 100, user: null }),
  ];
  const original = rows.map((candidate) => candidate.pid);
  const byCpuDesc = sortRows(rows, 'cpu', 'desc');
  assert.deepEqual(byCpuDesc.map((candidate) => candidate.pid), [2, 3, 1]);
  const byCpuAsc = sortRows(rows, 'cpu', 'asc');
  assert.deepEqual(byCpuAsc.map((candidate) => candidate.pid), [3, 2, 1]);
  const byMemoryDesc = sortRows(rows, 'memory', 'desc');
  assert.deepEqual(byMemoryDesc.map((candidate) => candidate.pid), [1, 3, 2]);
  assert.deepEqual(sortRows(rows, 'name', 'asc').map((candidate) => candidate.name), ['alpha', 'Mike', 'Zulu']);
  assert.deepEqual(sortRows(rows, 'pid', 'desc').map((candidate) => candidate.pid), [3, 2, 1]);
  assert.deepEqual(rows.map((candidate) => candidate.pid), original, 'input is untouched');

  const groups = aggregateApplications([
    row(10, { unit: 'app-bravo-1.scope', memoryRssBytes: 200, cpuPercent: 5 }),
    row(11, { unit: 'app-alpha-1.scope', memoryRssBytes: null, cpuPercent: 40 }),
    row(12, { unit: 'app-charlie-1.scope', memoryRssBytes: 100, cpuPercent: null }),
  ]);
  const byName = sortGroups(groups, 'name', 'asc');
  assert.deepEqual(byName.map((candidate) => candidate.label), ['alpha', 'bravo', 'charlie']);
  const byMemory = sortGroups(groups, 'memory', 'desc');
  assert.deepEqual(byMemory.map((candidate) => candidate.label), ['bravo', 'charlie', 'alpha']);
  assert.equal(sortGroups(groups, 'user', 'asc')[2].users.length > 0, true);
  for (const key of ['cpu', 'memory', 'gpuMemory', 'ioRead', 'ioWrite', 'count', 'pid'] as const) {
    assert.equal(sortGroups(groups, key, 'asc').length, groups.length);
  }
});

test('filterRows is case-insensitive across name, user, pid, unit, and command', () => {
  const rows = [
    row(1234, { name: 'Firefox', user: 'Alice', unit: 'app-firefox-1.scope', command: 'run --Flag' }),
    row(55, { name: 'bash', user: null, unit: null }),
  ];
  assert.deepEqual(filterRows(rows, 'firefox').map((candidate) => candidate.pid), [1234]);
  assert.deepEqual(filterRows(rows, 'ALICE').map((candidate) => candidate.pid), [1234]);
  assert.deepEqual(filterRows(rows, '234').map((candidate) => candidate.pid), [1234]);
  assert.deepEqual(filterRows(rows, 'firefox-1').map((candidate) => candidate.pid), [1234]);
  assert.deepEqual(filterRows(rows, '--flag').map((candidate) => candidate.pid), [1234]);
  assert.deepEqual(filterRows(rows, 'zzz'), []);
  assert.deepEqual(filterRows(rows, '   ').map((candidate) => candidate.pid), [1234, 55]);
});

test('buildTree orders parents before children and handles orphans, self-parents, and cycles', () => {
  const rows = [
    row(1, { ppid: 0 }),
    row(2, { ppid: 1 }),
    row(3, { ppid: 2 }),
    row(4, { ppid: 1 }),
    row(9, { ppid: 999 }),
    row(20, { ppid: 21 }),
    row(21, { ppid: 20 }),
    row(30, { ppid: 30 }),
  ];
  const tree = buildTree(rows);
  assert.equal(tree.length, rows.length);
  assert.equal(new Set(tree.map((node) => node.row.pid)).size, rows.length);

  const depth = new Map(tree.map((node) => [node.row.pid, node.depth]));
  assert.equal(depth.get(1), 0);
  assert.equal(depth.get(2), 1);
  assert.equal(depth.get(3), 2);
  assert.equal(depth.get(4), 1);
  assert.equal(depth.get(9), 0, 'orphan becomes a root');
  assert.equal(depth.get(30), 0, 'self-parent becomes a root');

  // The 20/21 cycle is broken deterministically: input-order first is the
  // edge removed, so 21 becomes root and 20 its child.
  assert.equal(depth.get(21), 0);
  assert.equal(depth.get(20), 1);

  const index = new Map(tree.map((node, position) => [node.row.pid, position]));
  assert.ok((index.get(1) as number) < (index.get(2) as number));
  assert.ok((index.get(2) as number) < (index.get(3) as number));
  assert.ok((index.get(1) as number) < (index.get(4) as number));

  const children = new Map(tree.map((node) => [node.row.pid, node.children]));
  assert.equal(children.get(1), 2);
  assert.equal(children.get(2), 1);
  assert.equal(children.get(21), 1);
  assert.equal(children.get(3), 0);

  assert.deepEqual(buildTree([]), []);
});
