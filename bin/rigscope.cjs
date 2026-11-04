#!/usr/bin/env node
if (Number(process.versions.node.split('.')[0]) < 26) {
  console.error('Rigscope requires Node.js 26 or newer.');
  process.exit(1);
}
require('../dist/cli.cjs');
