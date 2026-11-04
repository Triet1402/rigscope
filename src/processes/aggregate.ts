/**
 * Browser-safe application aggregation for process rows.
 *
 * This module is intentionally free of Node imports: the Processes screen
 * imports it directly. `aggregateApplications` assigns every row to exactly
 * one group (unit, application tree, or kernel), so summing group counts
 * never double-counts a process.
 *
 * Metric totals are sums over the members that reported a value. A total is
 * `null` only when no member reported that metric. Rows that do not expose a
 * metric contribute nothing, so a non-null total with `partial[metric] = true`
 * means "at least this much". `partial` is false both when the value is
 * complete and when the metric is entirely unknown (the total is then null).
 */
import type { ProcessRow } from '../contracts';

export type AppGroup = {
  key: string;
  label: string;
  kind: 'unit' | 'tree' | 'kernel';
  members: ProcessRow[];
  cpuPercent: number | null;
  memoryRssBytes: number | null;
  gpuMemoryBytes: number | null;
  ioReadBytesPerSec: number | null;
  ioWriteBytesPerSec: number | null;
  count: number;
  users: string[];
  /** True when at least one, but not all, members had a value for that metric. */
  partial: Record<AggregateMetric, boolean>;
};

export type AggregateMetric =
  | 'cpuPercent'
  | 'memoryRssBytes'
  | 'gpuMemoryBytes'
  | 'ioReadBytesPerSec'
  | 'ioWriteBytesPerSec';

export type SortKey = 'cpu' | 'memory' | 'gpuMemory' | 'ioRead' | 'ioWrite' | 'name' | 'count' | 'pid' | 'user';

export type TreeNode = { row: ProcessRow; depth: number; children: number };

type GroupDescriptor = {
  key: string;
  label: string;
  kind: AppGroup['kind'];
  /** Row that identifies a tree group; for unit/kernel groups this is the first member. */
  leader: ProcessRow;
};

const SHELL_NAMES = new Set(['bash', 'zsh', 'fish', 'sh', 'dash', 'ksh', 'mksh', 'tcsh', 'csh', 'ash', 'busybox']);
const SESSION_ROOT_NAMES = new Set([
  'sshd',
  'sshd-session',
  'dropbear',
  'tmux',
  'screen',
  'systemd',
]);

const METRICS: AggregateMetric[] = [
  'cpuPercent',
  'memoryRssBytes',
  'gpuMemoryBytes',
  'ioReadBytesPerSec',
  'ioWriteBytesPerSec',
];

/** Group rows into one unit/tree/kernel group each, preserving first-seen order. */
export function aggregateApplications(rows: ProcessRow[]): AppGroup[] {
  const byPid = new Map<number, ProcessRow>();
  for (const row of rows) {
    if (!byPid.has(row.pid)) byPid.set(row.pid, row);
  }

  const groups = new Map<string, { descriptor: GroupDescriptor; members: ProcessRow[] }>();
  for (const row of rows) {
    const descriptor = groupFor(row, byPid);
    const existing = groups.get(descriptor.key);
    if (existing) {
      existing.members.push(row);
    } else {
      groups.set(descriptor.key, { descriptor, members: [row] });
    }
  }

  return [...groups.values()].map(({ descriptor, members }) => buildGroup(descriptor, members));
}

/** Sort groups without mutating the input. Null/unknown values always go last. */
export function sortGroups(groups: AppGroup[], key: SortKey, dir: 'asc' | 'desc'): AppGroup[] {
  return groups.slice().sort((a, b) => compare(sortValueForGroup(a, key), sortValueForGroup(b, key), dir));
}

/** Sort rows without mutating the input. Null/unknown values always go last. */
export function sortRows(rows: ProcessRow[], key: SortKey, dir: 'asc' | 'desc'): ProcessRow[] {
  return rows.slice().sort((a, b) => compare(sortValueForRow(a, key), sortValueForRow(b, key), dir));
}

/** Case-insensitive substring match over name, user, pid, unit, and command. */
export function filterRows(rows: ProcessRow[], query: string): ProcessRow[] {
  const needle = query.trim().toLowerCase();
  if (needle.length === 0) return rows.slice();
  return rows.filter((row) => {
    if (row.name.toLowerCase().includes(needle)) return true;
    if (row.user !== null && row.user.toLowerCase().includes(needle)) return true;
    if (String(row.pid).includes(needle)) return true;
    if (row.unit !== null && row.unit.toLowerCase().includes(needle)) return true;
    if (row.command !== undefined && row.command.toLowerCase().includes(needle)) return true;
    return false;
  });
}

/**
 * Flatten rows into a parent-before-child forest.
 *
 * Siblings keep the input order, orphans become roots at depth 0, and a
 * parent cycle is broken deterministically so every row still appears exactly
 * once. `children` is the number of direct children (not descendants).
 */
export function buildTree(rows: ProcessRow[]): TreeNode[] {
  const count = rows.length;
  const byPid = new Map<number, number>();
  for (let index = 0; index < count; index++) {
    if (!byPid.has(rows[index].pid)) byPid.set(rows[index].pid, index);
  }

  const parent: Array<number | null> = rows.map((row) => {
    const candidate = byPid.get(row.ppid);
    return candidate === undefined ? null : candidate;
  });

  // Break cycles in the functional parent graph (each node has at most one
  // parent). The edge that closes the cycle is removed, making that node a
  // root. Input order makes the choice stable.
  const state = new Uint8Array(count); // 0 = unvisited, 1 = on current path, 2 = settled
  for (let start = 0; start < count; start++) {
    if (state[start] !== 0) continue;
    const path: number[] = [];
    let current: number | null = start;
    while (current !== null && state[current] === 0) {
      state[current] = 1;
      path.push(current);
      current = parent[current];
    }
    if (current !== null && state[current] === 1) {
      const last = path[path.length - 1];
      parent[last] = null;
    }
    for (const index of path) state[index] = 2;
  }

  const children: number[][] = Array.from({ length: count }, () => []);
  const roots: number[] = [];
  for (let index = 0; index < count; index++) {
    const p = parent[index];
    if (p === null) roots.push(index);
    else children[p].push(index);
  }

  const out: TreeNode[] = [];
  // Depth-first pre-order, children pushed in reverse so input order is kept.
  const stack: Array<{ index: number; depth: number }> = [];
  for (let i = roots.length - 1; i >= 0; i--) stack.push({ index: roots[i], depth: 0 });
  while (stack.length > 0) {
    const { index, depth } = stack.pop() as { index: number; depth: number };
    out.push({ row: rows[index], depth, children: children[index].length });
    const childIndexes = children[index];
    for (let i = childIndexes.length - 1; i >= 0; i--) {
      stack.push({ index: childIndexes[i], depth: depth + 1 });
    }
  }
  return out;
}

function groupFor(row: ProcessRow, byPid: Map<number, ProcessRow>): GroupDescriptor {
  if (isKernel(row, byPid)) return { key: 'kernel', label: 'kernel', kind: 'kernel', leader: row };

  const unit = distinctiveUnit(row.unit);
  if (unit !== null) {
    return { key: `unit:${unit}`, label: readableUnitName(unit), kind: 'unit', leader: row };
  }

  // Climb to the top-most ancestor below a session root. Unit-grouped
  // ancestors that are not themselves session roots absorb their children,
  // so a child in a generic/no cgroup still rolls into its application.
  let current = row;
  const visited = new Set<number>();
  for (;;) {
    if (visited.has(current.pid)) {
      // Parent cycle: use the smallest visible pid as a stable group leader.
      let leader = current;
      for (const pid of visited) {
        const candidate = byPid.get(pid);
        if (candidate && (candidate.pid < leader.pid || (candidate.pid === leader.pid && candidate.startTicks < leader.startTicks))) {
          leader = candidate;
        }
      }
      return treeDescriptor(leader);
    }
    visited.add(current.pid);

    const parent = byPid.get(current.ppid);
    if (!parent) return treeDescriptor(current);

    if (isSessionRoot(parent, byPid)) return treeDescriptor(current);
    const parentUnit = distinctiveUnit(parent.unit);
    if (parentUnit !== null) {
      return { key: `unit:${parentUnit}`, label: readableUnitName(parentUnit), kind: 'unit', leader: parent };
    }
    current = parent;
  }
}

function isKernel(row: ProcessRow, byPid: Map<number, ProcessRow>): boolean {
  if (row.pid === 2 || row.ppid === 2) return true;
  let current: ProcessRow | undefined = row;
  const visited = new Set<number>();
  while (current && !visited.has(current.pid)) {
    if (current.pid === 2) return true;
    visited.add(current.pid);
    current = byPid.get(current.ppid);
  }
  return false;
}

function isSessionRoot(row: ProcessRow, byPid: Map<number, ProcessRow>): boolean {
  if (row.pid === 1) return true;
  const name = row.name.toLowerCase();
  if (SHELL_NAMES.has(name)) return true;
  if (SESSION_ROOT_NAMES.has(name)) return true;
  // tmux's server comm is commonly "tmux: server"; tty screen variants may
  // be prefixed but the bare names are the common cases.
  if (name.startsWith('tmux:')) return true;
  if (isGenericUnit(row.unit)) {
    // Only the top-most process of a generic systemd unit is the session
    // boundary. Children inherit the same unit, but they are applications
    // inside that session, not new sessions themselves.
    const parent = byPid.get(row.ppid);
    return parent === undefined || parent.unit !== row.unit;
  }
  return false;
}

function isGenericUnit(unit: string | null): boolean {
  if (unit === null) return false;
  if (unit === 'init.scope') return true;
  if (/^session-.+\.scope$/.test(unit)) return true;
  if (/^user@.+\.service$/.test(unit)) return true;
  return false;
}

function distinctiveUnit(unit: string | null): string | null {
  if (unit === null) return null;
  if (!unit.endsWith('.scope') && !unit.endsWith('.service')) return null;
  if (isGenericUnit(unit)) return null;
  return unit;
}

/** Human label for a unit: drop common app prefixes, the transient PID suffix, and the unit type. */
export function readableUnitName(unit: string): string {
  let label = unit.replace(/\.(scope|service)$/, '');
  if (label.startsWith('app-gnome-')) label = label.slice('app-gnome-'.length);
  else if (label.startsWith('app-')) label = label.slice('app-'.length);
  label = label.replace(/-\d+$/, '');
  return label.length > 0 ? label : unit;
}

function treeDescriptor(leader: ProcessRow): GroupDescriptor {
  return {
    key: `tree:${leader.pid}:${leader.startTicks}`,
    label: leader.name,
    kind: 'tree',
    leader,
  };
}

function buildGroup(descriptor: GroupDescriptor, members: ProcessRow[]): AppGroup {
  const totals: Record<AggregateMetric, { sum: number; known: number }> = {
    cpuPercent: { sum: 0, known: 0 },
    memoryRssBytes: { sum: 0, known: 0 },
    gpuMemoryBytes: { sum: 0, known: 0 },
    ioReadBytesPerSec: { sum: 0, known: 0 },
    ioWriteBytesPerSec: { sum: 0, known: 0 },
  };
  const users = new Set<string>();

  for (const member of members) {
    if (member.user !== null) users.add(member.user);
    for (const metric of METRICS) {
      const value = member[metric];
      if (value === null || !Number.isFinite(value)) continue;
      totals[metric].sum += value;
      totals[metric].known++;
    }
  }

  const valueOf = (metric: AggregateMetric): number | null =>
    totals[metric].known === 0 ? null : totals[metric].sum;
  const partial = {} as Record<AggregateMetric, boolean>;
  for (const metric of METRICS) {
    partial[metric] = totals[metric].known > 0 && totals[metric].known < members.length;
  }

  return {
    key: descriptor.key,
    label: descriptor.label,
    kind: descriptor.kind,
    // Leader first keeps unit/tree focus readable; members remain row-identical.
    members: descriptor.leader !== members[0]
      ? [descriptor.leader, ...members.filter((member) => member !== descriptor.leader)]
      : members,
    cpuPercent: valueOf('cpuPercent'),
    memoryRssBytes: valueOf('memoryRssBytes'),
    gpuMemoryBytes: valueOf('gpuMemoryBytes'),
    ioReadBytesPerSec: valueOf('ioReadBytesPerSec'),
    ioWriteBytesPerSec: valueOf('ioWriteBytesPerSec'),
    count: members.length,
    users: [...users].sort(),
    partial,
  };
}

type SortValue = number | string | null;

function sortValueForGroup(group: AppGroup, key: SortKey): SortValue {
  switch (key) {
    case 'cpu':
      return group.cpuPercent;
    case 'memory':
      return group.memoryRssBytes;
    case 'gpuMemory':
      return group.gpuMemoryBytes;
    case 'ioRead':
      return group.ioReadBytesPerSec;
    case 'ioWrite':
      return group.ioWriteBytesPerSec;
    case 'name':
      return group.label;
    case 'count':
      return group.count;
    case 'pid':
      return minPid(group.members);
    case 'user':
      return group.users.length > 0 ? group.users[0] : null;
  }
}

function sortValueForRow(row: ProcessRow, key: SortKey): SortValue {
  switch (key) {
    case 'cpu':
      return row.cpuPercent;
    case 'memory':
      return row.memoryRssBytes;
    case 'gpuMemory':
      return row.gpuMemoryBytes;
    case 'ioRead':
      return row.ioReadBytesPerSec;
    case 'ioWrite':
      return row.ioWriteBytesPerSec;
    case 'name':
      return row.name;
    case 'count':
      // A row has no aggregate count; every row compares equal so the input
      // order is preserved.
      return 1;
    case 'pid':
      return row.pid;
    case 'user':
      return row.user;
  }
}

function minPid(rows: ProcessRow[]): number | null {
  let min: number | null = null;
  for (const row of rows) {
    if (min === null || row.pid < min) min = row.pid;
  }
  return min;
}

function compare(a: SortValue, b: SortValue, dir: 'asc' | 'desc'): number {
  const aMissing = a === null || a === undefined;
  const bMissing = b === null || b === undefined;
  if (aMissing && bMissing) return 0;
  // Unknown values stay last in both directions.
  if (aMissing) return 1;
  if (bMissing) return -1;

  let base: number;
  if (typeof a === 'number' && typeof b === 'number') {
    base = a - b;
  } else {
    const left = String(a).toLowerCase();
    const right = String(b).toLowerCase();
    base = left < right ? -1 : left > right ? 1 : 0;
  }
  return dir === 'asc' ? base : -base;
}
