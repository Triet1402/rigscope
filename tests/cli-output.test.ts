import assert from 'node:assert/strict';
import test from 'node:test';
import {
  bold,
  colorize,
  error,
  formatError,
  help,
  printError,
  printStatus,
  status,
  success,
  supportsColor,
  useColor,
  warning,
  writeLine,
} from '../src/cli/output';

const ANSI = /\u001b\[/;
const tty = {isTTY: true, write() {}};
const pipe = {isTTY: false, write() {}};

test('color auto-detects a TTY and honors NO_COLOR, dumb terminals and JSON mode', () => {
  assert.equal(useColor({stream: tty, env: {}}), true);
  assert.equal(supportsColor({stream: pipe, env: {}}), false);
  assert.equal(useColor({stream: tty, env: {NO_COLOR: '1'}}), false);
  assert.equal(useColor({stream: tty, env: {NO_COLOR: ''}}), false, 'presence disables color');
  assert.equal(useColor({stream: tty, env: {TERM: 'dumb'}}), false);
  assert.equal(useColor({stream: tty, env: {}, json: true}), false);
  assert.equal(useColor({stream: tty, env: {}, color: false}), false);
  assert.equal(useColor({stream: pipe, env: {}, color: true}), true);
  assert.equal(useColor({stream: tty, env: {NO_COLOR: '1'}, color: true}), false);
  assert.equal(useColor({stream: tty, env: {}, json: true, color: true}), false);
});

test('terminal messages carry ANSI color while piped and JSON messages do not', () => {
  const colored = success('ready', {stream: tty, env: {}});
  assert.match(colored, ANSI);
  assert.match(colored, /ready/);
  assert.doesNotMatch(success('ready', {stream: pipe, env: {}}), ANSI);
  assert.doesNotMatch(success('ready', {stream: tty, env: {}, json: true}), ANSI);
  assert.match(status('Service', 'active', {stream: tty, env: {}}), ANSI);
  assert.equal(status('Service', 'active', {stream: pipe, env: {}}), 'Service: active');
  assert.equal(warning('slow', {stream: pipe, env: {}}), '! Warning: slow');
  assert.equal(error('boom', {stream: pipe, env: {}}), '✖ Error: boom');
  assert.equal(help('Usage: rigscope', {stream: pipe, env: {}}), 'Usage: rigscope');
  assert.equal(formatError(new Error('boom'), {stream: pipe, env: {}}), '✖ Error: boom');
  assert.equal(bold('label', {stream: tty, env: {}}), '\u001b[1mlabel\u001b[0m');
  assert.equal(colorize('x', 'green', {stream: tty, env: {NO_COLOR: '1'}}), 'x');
});

test('write helpers target the supplied stream and never mutate console', () => {
  const writes: string[] = [];
  const stream = {isTTY: false, write(chunk: string) { writes.push(chunk); return true; }};
  const originalError = console.error;
  const originalLog = console.log;
  try {
    printStatus('Service', 'active', {stream: stream as NodeJS.WriteStream});
    printError('broken', {stream: stream as NodeJS.WriteStream});
    writeLine(stream, 'already\n');
    assert.deepEqual(writes, ['Service: active\n', '✖ Error: broken\n', 'already\n']);
    assert.equal(console.error, originalError);
    assert.equal(console.log, originalLog);
    assert.equal(writes.join('').includes('\u001b['), false, 'piped output is plain');
  } finally {
    console.error = originalError;
    console.log = originalLog;
  }
});
