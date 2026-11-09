/** `rigscope gpu-temps` / `rigscope cpu-power` against a fake host. */
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import {
  HARDWARE_FEATURES,
  packageRulePath,
  runHardwareAccessCommand,
  type HardwareAccessHost,
  type HardwareProbe,
} from '../src/cli/hardware-access';
import { DEFAULT_SYSTEM_ACCOUNTS, renderRaplRule } from '../src/cli/system';

type FakeHost = HardwareAccessHost & {
  calls: Array<{command: string; args: string[]}>;
  files: Map<string, string>;
  groups: Map<string, string[]>;
  users: Set<string>;
  probeResult: HardwareProbe;
};

function fakeHost(options: {
  uid?: number;
  sudoUser?: string | null;
  probe?: Partial<HardwareProbe>;
  files?: Record<string, string>;
  groups?: Record<string, string[]>;
  users?: string[];
  systemInstallation?: boolean;
  systemPowerEnabled?: boolean;
} = {}): FakeHost {
  const calls: FakeHost['calls'] = [];
  const files = new Map<string, string>(Object.entries(options.files ?? {}));
  const groups = new Map<string, string[]>(Object.entries(options.groups ?? {}));
  const users = new Set<string>(options.users ?? ['triet']);
  const probeResult: HardwareProbe = {present: true, readable: false, detail: 'test detail', ...(options.probe ?? {})};
  const host: FakeHost = {
    uid: () => options.uid ?? 0,
    env: options.sudoUser === null ? {} : {SUDO_USER: options.sudoUser ?? 'triet'},
    calls,
    files,
    groups,
    users,
    probeResult,
    probe: () => probeResult,
    run: (command, args) => {
      calls.push({command, args});
      if (command === 'getent' && args[0] === 'group') {
        const members = groups.get(args[1]);
        if (members === undefined) return {status: 2, stdout: '', stderr: ''};
        return {status: 0, stdout: `${args[1]}:x:1500:${members.join(',')}\n`, stderr: ''};
      }
      if (command === 'id' && args[0] === '-u') {
        return {status: users.has(args[1]!) ? 0 : 1, stdout: users.has(args[1]!) ? '1500\n' : '', stderr: ''};
      }
      if (command === 'id' && args[0] === '-un') return {status: 0, stdout: 'root\n', stderr: ''};
      if (command === 'groupadd') {
        groups.set(args[args.length - 1]!, []);
        return {status: 0, stdout: '', stderr: ''};
      }
      if (command === 'gpasswd' && args[0] === '-a') {
        groups.get(args[2]!)?.push(args[1]!);
        return {status: 0, stdout: '', stderr: ''};
      }
      if (command === 'gpasswd' && args[0] === '-d') {
        const members = groups.get(args[2]!);
        if (members) groups.set(args[2]!, members.filter((member) => member !== args[1]));
        return {status: 0, stdout: '', stderr: ''};
      }
      return {status: 0, stdout: '', stderr: ''};
    },
    readFile: (path) => files.get(path) ?? null,
    writeFile: (path, content) => files.set(path, content),
    removeFile: (path) => files.delete(path),
    systemInstallationPresent: () => options.systemInstallation ?? false,
    systemPowerEnabled: () => options.systemPowerEnabled ?? false,
  };
  return host;
}

const GPU = HARDWARE_FEATURES['gpu-temps'];
const RULE_PATH = '/etc/udev/rules.d/90-rigscope-gpu-temps.rules';
const RULE_SOURCE = packageRulePath(GPU);

test('enable refuses to run without root and changes nothing', () => {
  const host = fakeHost({uid: 1000});
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'enable'], host), 1);
  assert.deepEqual(host.calls, []);
});

test('enable and disable escalate through sudo in an interactive terminal', () => {
  for (const subcommand of ['enable', 'disable'] as const) {
    const host = fakeHost({uid: 1000});
    const escalated: Array<readonly string[]> = [];
    host.escalate = (argv) => {
      escalated.push(argv);
      return 0;
    };
    assert.equal(runHardwareAccessCommand(['gpu-temps', subcommand], host), 0);
    assert.deepEqual(escalated, [['gpu-temps', subcommand]]);
    assert.deepEqual(host.calls, [], 'the unprivileged process must not mutate anything');
  }
});

test('escalation can be declined or fail without mutating anything', () => {
  const declined = fakeHost({uid: 1000});
  declined.escalate = () => null;
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'enable'], declined), 1);
  assert.deepEqual(declined.calls, []);

  const failed = fakeHost({uid: 1000});
  failed.escalate = () => 1;
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'enable'], failed), 1);
  assert.deepEqual(failed.calls, []);
});

test('root runs the mutation directly instead of escalating', () => {
  const host = fakeHost({uid: 0, files: {[RULE_SOURCE]: 'Managed by Rigscope: gpu rule'}});
  let escalated = false;
  host.escalate = () => {
    escalated = true;
    return 0;
  };
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'enable'], host), 0);
  assert.equal(escalated, false);
});

test('gpu-temps enable: creates group, grants sudo user, installs rule, reloads udev', () => {
  const host = fakeHost({files: {[RULE_SOURCE]: 'Managed by Rigscope: gpu rule'}});
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'enable'], host), 0);
  const commands = host.calls.map((call) => [call.command, ...call.args].join(' '));
  assert.ok(commands.includes('groupadd --system rigscope-gpu'));
  assert.ok(commands.includes('gpasswd -a triet rigscope-gpu'));
  assert.equal(host.files.get(RULE_PATH), 'Managed by Rigscope: gpu rule');
  assert.ok(commands.includes('udevadm control --reload-rules'));
  assert.ok(commands.includes('udevadm trigger -s pci'));
});

test('enable is idempotent: existing group and member skip mutation calls', () => {
  const host = fakeHost({
    files: {[RULE_SOURCE]: 'Managed by Rigscope: gpu rule'},
    groups: {'rigscope-gpu': ['triet']},
  });
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'enable'], host), 0);
  const commands = host.calls.map((call) => [call.command, ...call.args].join(' '));
  assert.equal(commands.some((line) => line.startsWith('groupadd')), false);
  assert.equal(commands.some((line) => line.startsWith('gpasswd')), false);
  assert.equal(host.files.get(RULE_PATH), 'Managed by Rigscope: gpu rule');
});

test('enable also grants the system collector account when it exists', () => {
  const host = fakeHost({
    files: {[RULE_SOURCE]: 'Managed by Rigscope: gpu rule'},
    users: ['rigscope-collector'],
  });
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'enable'], host), 0);
  const commands = host.calls.map((call) => [call.command, ...call.args].join(' '));
  assert.ok(commands.includes('gpasswd -a triet rigscope-gpu'));
  assert.ok(commands.includes('gpasswd -a rigscope-collector rigscope-gpu'));
});

test('enable fails cleanly when the packaged rule is missing', () => {
  const host = fakeHost({files: {}});
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'enable'], host), 1);
  assert.equal(host.files.has(RULE_PATH), false);
});

test('enable refuses to overwrite a foreign rule file', () => {
  const host = fakeHost({
    files: {[RULE_PATH]: 'someone else\'s rule', [RULE_SOURCE]: 'Managed by Rigscope: gpu rule'},
  });
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'enable'], host), 1);
  assert.equal(host.files.get(RULE_PATH), 'someone else\'s rule');
  const commands = host.calls.map((call) => [call.command, ...call.args].join(' '));
  assert.equal(commands.some((line) => line.startsWith('groupadd') || line.startsWith('gpasswd') || line.startsWith('udevadm')), false);
});

test('enable keeps a Rigscope-managed rule and grants the group it names', () => {
  // The renderer in src/cli/system.ts is authoritative: an installed
  // Rigscope-managed rule (possibly rendered for custom accounts) is kept,
  // and memberships follow its chgrp target instead of the packaged default.
  const custom = '# Managed by Rigscope\nSUBSYSTEM=="pci", RUN+="/usr/bin/chgrp custom-gpu /sys%p/resource0"\n';
  const host = fakeHost({
    files: {[RULE_PATH]: custom, [RULE_SOURCE]: 'Managed by Rigscope: gpu rule'},
    groups: {'custom-gpu': []},
  });
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'enable'], host), 0);
  assert.equal(host.files.get(RULE_PATH), custom);
  const commands = host.calls.map((call) => [call.command, ...call.args].join(' '));
  assert.ok(commands.includes('gpasswd -a triet custom-gpu'));
  assert.equal(commands.some((line) => line.includes('rigscope-gpu')), false);
});

test('cpu-power enable reserves the RAPL group for the system collector when it exists', () => {
  // `rigscope system` audits this group and fails unless the dedicated
  // collector is its only member, so the sudo user must not be added.
  const source = packageRulePath(HARDWARE_FEATURES['cpu-power']);
  const host = fakeHost({
    files: {[source]: 'Managed by Rigscope: rapl rule'},
    users: ['triet', 'rigscope-collector'],
  });
  assert.equal(runHardwareAccessCommand(['cpu-power', 'enable'], host), 0);
  const commands = host.calls.map((call) => [call.command, ...call.args].join(' '));
  assert.ok(commands.includes('gpasswd -a rigscope-collector rigscope-rapl'));
  assert.equal(commands.includes('gpasswd -a triet rigscope-rapl'), false);
});

test('the real packaged gpu-temps rule passes the disable guard', () => {
  // Regression: the packaged file must carry the exact marker `disable`
  // accepts, or `enable` installs a rule `disable` refuses to remove.
  const contents = readFileSync(RULE_SOURCE, 'utf8');
  assert.ok(contents.includes('Managed by Rigscope'));
  const host = fakeHost({files: {[RULE_PATH]: contents}});
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'disable'], host), 0);
  assert.equal(host.files.has(RULE_PATH), false);
});

test('cpu-power enable uses the RAPL group and powercap subsystem', () => {
  const source = packageRulePath(HARDWARE_FEATURES['cpu-power']);
  const host = fakeHost({files: {[source]: 'Managed by Rigscope: rapl rule'}});
  assert.equal(runHardwareAccessCommand(['cpu-power', 'enable'], host), 0);
  const commands = host.calls.map((call) => [call.command, ...call.args].join(' '));
  assert.ok(commands.includes('groupadd --system rigscope-rapl'));
  assert.ok(commands.includes('gpasswd -a triet rigscope-rapl'));
  assert.ok(commands.includes('udevadm trigger -s powercap'));
  assert.equal(host.files.get('/etc/udev/rules.d/90-rigscope-rapl.rules'), 'Managed by Rigscope: rapl rule');
});

test('disable removes only Rigscope-managed rule files', () => {
  const foreign = fakeHost({files: {[RULE_PATH]: 'someone else\'s rule'}});
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'disable'], foreign), 1);
  assert.equal(foreign.files.has(RULE_PATH), true);

  const managed = fakeHost({
    files: {[RULE_PATH]: '# Managed by Rigscope\nrule'},
    groups: {'rigscope-gpu': ['triet', 'rigscope-collector']},
  });
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'disable'], managed), 0);
  assert.equal(managed.files.has(RULE_PATH), false);
  const commands = managed.calls.map((call) => [call.command, ...call.args].join(' '));
  assert.ok(commands.includes('gpasswd -d triet rigscope-gpu'));
  assert.ok(commands.includes('gpasswd -d rigscope-collector rigscope-gpu'));
});

test('status exits 3 with the fix command when sensors are present but unreadable', () => {
  const host = fakeHost({groups: {'rigscope-gpu': []}});
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'status'], host), 3);
});

test('cpu-power status accepts a system grant reserved for the collector account', () => {
  const host = fakeHost({
    files: {'/etc/udev/rules.d/90-rigscope-rapl.rules': '# Managed by Rigscope\nrule'},
    groups: {'rigscope-rapl': ['rigscope-collector']},
    systemInstallation: true,
    probe: {present: true, readable: false, detail: 'counters not readable by this user'},
  });
  assert.equal(runHardwareAccessCommand(['cpu-power', 'status'], host), 0);
});

test('status explains a stale login session instead of asking to enable again', () => {
  const host = fakeHost({
    files: {[RULE_PATH]: '# Managed by Rigscope\nrule'},
    groups: {'rigscope-gpu': ['triet']},
    probe: {present: true, readable: false},
  });
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'status'], host), 3);
});

test('status exits 0 when sensors are readable', () => {
  const host = fakeHost({probe: {readable: true, detail: '2 device(s): junction 68C'}});
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'status'], host), 0);
});

test('status exits 0 when the machine has no such sensors', () => {
  const host = fakeHost({probe: {present: false, readable: false, detail: 'no RAPL package devices found'}});
  assert.equal(runHardwareAccessCommand(['cpu-power', 'status'], host), 0);
});

test('unknown subcommands and extra arguments are rejected', () => {
  const host = fakeHost();
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'destroy'], host), 2);
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'status', 'extra'], host), 2);
  assert.deepEqual(host.calls, []);
});

test('cpu-power disable refuses when the system installation owns the grant', () => {
  const rulePath = '/etc/udev/rules.d/90-rigscope-rapl.rules';
  const managed = '# Managed by Rigscope\nrule';
  const host = fakeHost({
    files: {[rulePath]: managed},
    groups: {'rigscope-rapl': ['rigscope-collector']},
    systemPowerEnabled: true,
  });
  assert.equal(runHardwareAccessCommand(['cpu-power', 'disable'], host), 1);
  // Nothing changed: the rule and membership stay, and no commands ran.
  assert.equal(host.files.get(rulePath), managed);
  assert.deepEqual(host.groups.get('rigscope-rapl'), ['rigscope-collector']);
  assert.deepEqual(host.calls, []);
});

test('gpu-temps disable still works when the system installation owns package power', () => {
  const host = fakeHost({
    files: {[RULE_PATH]: '# Managed by Rigscope\nrule'},
    systemPowerEnabled: true,
  });
  assert.equal(runHardwareAccessCommand(['gpu-temps', 'disable'], host), 0);
  assert.equal(host.files.has(RULE_PATH), false);
});

test('the packaged RAPL rule stays byte-identical to the system renderer', () => {
  assert.equal(
    readFileSync(packageRulePath(HARDWARE_FEATURES['cpu-power']), 'utf8'),
    renderRaplRule(DEFAULT_SYSTEM_ACCOUNTS),
  );
});
