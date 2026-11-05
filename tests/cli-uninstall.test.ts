import assert from 'node:assert/strict';
import test from 'node:test';
import { spawn } from 'node:child_process';
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { initialize } from '../src/cli/config';
import { commandLauncher } from '../src/cli/services';
import { uninstallInstallation } from '../src/cli/uninstall';

function freshRoot(name: string): string {
  return mkdtempSync(join(tmpdir(), `rigscope-uninstall-${name}-`));
}

function privateDirectory(path: string): string {
  mkdirSync(path, {recursive: true, mode: 0o700});
  chmodSync(path, 0o700);
  return path;
}

function installAlias(dir: string, binHome: string): string {
  mkdirSync(binHome, {recursive: true});
  const alias = join(binHome, 'rigscope');
  writeFileSync(alias, commandLauncher(dir), {mode: 0o755});
  return alias;
}

function installRuntime(dir: string): void {
  const app = join(dir, 'runtime', 'node_modules', 'rigscope');
  mkdirSync(app, {recursive: true});
  writeFileSync(join(app, 'package.json'), `${JSON.stringify({name: 'rigscope', version: '1.0.0'})}\n`);
  for (const runtime of ['runtime-previous', 'runtime-next']) mkdirSync(join(dir, runtime), {recursive: true});
}

function withBinHome<T>(binHome: string, run: () => T): T {
  const previous = process.env.XDG_BIN_HOME;
  process.env.XDG_BIN_HOME = binHome;
  try {
    return run();
  } finally {
    if (previous === undefined) delete process.env.XDG_BIN_HOME;
    else process.env.XDG_BIN_HOME = previous;
  }
}

test('uninstall purges the installation and its launcher by default', () => {
  const root = freshRoot('purge');
  try {
    const dir = privateDirectory(join(root, 'state'));
    const binHome = join(root, 'bin');
    const alias = withBinHome(binHome, () => installAlias(dir, binHome));
    initialize(dir, {});
    installRuntime(dir);
    mkdirSync(join(dir, 'backups'));
    writeFileSync(join(dir, 'backups', 'history'), 'saved');

    withBinHome(binHome, () => uninstallInstallation(dir));

    assert.equal(existsSync(dir), false);
    assert.equal(existsSync(alias), false);
  } finally {
    rmSync(root, {recursive: true, force: true});
  }
});

test('uninstall is safe for an already uninstalled directory', () => {
  const root = freshRoot('missing');
  try {
    const dir = join(root, 'never-installed');
    assert.doesNotThrow(() => uninstallInstallation(dir));
    assert.doesNotThrow(() => uninstallInstallation(dir, false));
    assert.equal(existsSync(dir), false);
  } finally {
    rmSync(root, {recursive: true, force: true});
  }
});

test('keep-data removes runtime, services and run state but retains saved data', () => {
  const root = freshRoot('keep');
  try {
    const dir = privateDirectory(join(root, 'state'));
    const binHome = join(root, 'bin');
    const alias = withBinHome(binHome, () => installAlias(dir, binHome));
    const {config} = initialize(dir, {});
    installRuntime(dir);
    mkdirSync(join(dir, 'backups'));
    writeFileSync(join(dir, 'backups', 'history'), 'saved');
    writeFileSync(join(dir, 'release-state.json'), '{}\n', {mode: 0o600});
    writeFileSync(join(dir, 'hub', 'telemetry.sqlite'), 'history', {mode: 0o600});
    mkdirSync(join(dir, 'run'), {recursive: true, mode: 0o700});
    writeFileSync(join(dir, 'run', 'ephemeral'), 'runtime state');

    withBinHome(binHome, () => uninstallInstallation(dir, false));

    assert.ok(existsSync(join(dir, 'config.json')), 'configuration is retained');
    assert.deepEqual(JSON.parse(readFileSync(join(dir, 'config.json'), 'utf8')), config);
    assert.ok(existsSync(join(dir, 'backups', 'history')), 'backups are retained');
    assert.ok(existsSync(join(dir, 'hub', 'telemetry.sqlite')), 'history is retained');
    assert.ok(existsSync(join(dir, 'collector', 'state.json')), 'credentials and enrollment are retained');
    assert.ok(existsSync(join(dir, 'release-state.json')), 'runtime backup pairing is retained');
    assert.equal(existsSync(join(dir, 'runtime')), false);
    assert.equal(existsSync(join(dir, 'runtime-previous')), false);
    assert.equal(existsSync(join(dir, 'runtime-next')), false);
    assert.equal(existsSync(join(dir, 'run')), false, 'ephemeral run state is removed');
    assert.equal(existsSync(alias), false, 'the command launcher is removed');
  } finally {
    rmSync(root, {recursive: true, force: true});
  }
});

test('uninstall refuses unrecognized, non-private, symlinked and protected directories', () => {
  const root = freshRoot('safety');
  try {
    const unrelated = privateDirectory(join(root, 'unrelated'));
    writeFileSync(join(unrelated, 'keep'), 'keep');
    assert.throws(() => uninstallInstallation(unrelated), /without a saved Rigscope configuration/);
    assert.throws(() => uninstallInstallation(unrelated, false), /without a saved Rigscope configuration/);
    assert.equal(readFileSync(join(unrelated, 'keep'), 'utf8'), 'keep');

    const loose = privateDirectory(join(root, 'loose'));
    initialize(loose, {});
    chmodSync(loose, 0o755);
    assert.throws(() => uninstallInstallation(loose), /owned private/);
    assert.ok(existsSync(join(loose, 'config.json')));

    const real = privateDirectory(join(root, 'real'));
    initialize(real, {});
    const link = join(root, 'linked');
    symlinkSync(real, link);
    assert.throws(() => uninstallInstallation(link), /symbolic link/);
    assert.ok(existsSync(join(real, 'config.json')));

    assert.throws(() => uninstallInstallation(resolve('/')), /protected system or home path/);
    assert.throws(() => uninstallInstallation(homedir()), /protected system or home path|symbolic link/);
    assert.throws(() => uninstallInstallation(resolve(homedir(), '..')), /protected system or home path|symbolic link/);
  } finally {
    rmSync(root, {recursive: true, force: true});
  }
});

async function waitFor(condition: () => boolean, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error('condition was not reached before the deadline');
    await new Promise(resolveWait => setTimeout(resolveWait, 25));
  }
}

test('uninstall stops a live owned foreground process instead of requiring a manual stop', {timeout: 30000}, async () => {
  const root = freshRoot('foreground');
  let child: ReturnType<typeof spawn> | undefined;
  try {
    const dir = privateDirectory(join(root, 'state'));
    initialize(dir, {});
    privateDirectory(join(dir, 'run'));

    const marker = join(root, 'stopped');
    const lock = join(dir, 'run', 'rigscope.lock');
    const script = join(root, 'hold.ts');
    writeFileSync(script, `
import { writeFileSync } from 'node:fs';
import { acquireProcessLease } from ${JSON.stringify(join(import.meta.dirname, '..', 'src', 'runtime', 'process-lease.ts'))};
acquireProcessLease(process.argv[2]);
process.on('SIGTERM', () => { writeFileSync(process.argv[3], 'stopped'); process.exit(0); });
setInterval(() => {}, 1000);
`);
    child = spawn(process.execPath, [
      '--import', 'tsx', script, lock, marker, '--state-dir', dir,
    ], {stdio: 'ignore', env: {...process.env}});
    await waitFor(() => existsSync(lock), 5000);

    uninstallInstallation(dir);

    assert.equal(existsSync(marker), true, 'the foreground process received the stop request');
    assert.equal(existsSync(dir), false, 'state is removed only after the process stops');
    await new Promise<void>(resolveExit => {
      if (child!.exitCode !== null || child!.signalCode !== null) resolveExit();
      else child!.once('exit', () => resolveExit());
    });
    assert.throws(() => process.kill(child!.pid!, 0), /ESRCH/);
  } finally {
    if (child?.pid) {
      try { process.kill(child.pid, 'SIGKILL'); } catch { /* already stopped */ }
    }
    rmSync(root, {recursive: true, force: true});
  }
});

test('uninstall refuses a live lease it cannot prove belongs to the installation', {timeout: 30000}, async () => {
  const root = freshRoot('foreign');
  let child: ReturnType<typeof spawn> | undefined;
  try {
    const dir = privateDirectory(join(root, 'state'));
    initialize(dir, {});
    privateDirectory(join(dir, 'run'));

    const lock = join(dir, 'run', 'rigscope.lock');
    const script = join(root, 'hold.ts');
    writeFileSync(script, `
import { acquireProcessLease } from ${JSON.stringify(join(import.meta.dirname, '..', 'src', 'runtime', 'process-lease.ts'))};
acquireProcessLease(process.env.UNRELATED_LOCK);
setInterval(() => {}, 1000);
`);
    child = spawn(process.execPath, ['--import', 'tsx', script], {
      stdio: 'ignore',
      env: {...process.env, UNRELATED_LOCK: lock},
    });
    await waitFor(() => existsSync(lock), 5000);

    assert.throws(() => uninstallInstallation(dir), /does not reference|foreign|unidentified/);
    assert.ok(existsSync(join(dir, 'config.json')), 'refusal leaves the installation untouched');
    assert.ok(existsSync(join(dir, 'run', 'rigscope.lock')));
  } finally {
    if (child?.pid) {
      try { process.kill(child.pid, 'SIGKILL'); } catch { /* already stopped */ }
    }
    rmSync(root, {recursive: true, force: true});
  }
});
