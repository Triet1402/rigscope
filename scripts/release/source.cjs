#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-only
// Populate an exact committed source tree with the dependency sources used by
// the binary. The shell builder owns deterministic tar/gzip packaging.
const fs = require('node:fs');
const path = require('node:path');
const {execFileSync} = require('node:child_process');
const {collect} = require('../third-party-notices.cjs');
const target = path.resolve(process.argv[2] || '');
if (!process.argv[2] || target === process.cwd()) throw new Error('provide a source staging directory');
execFileSync('git', ['diff', '--quiet', 'HEAD'], {stdio:'inherit'});
fs.mkdirSync(target, {recursive:true});
const archive = execFileSync('git', ['archive', 'HEAD'], {maxBuffer:128*1024*1024});
execFileSync('tar', ['-xf', '-', '-C', target], {input:archive});
execFileSync('go', ['mod', 'vendor', '-o', path.join(target, 'vendor')], {stdio:'inherit'});
const inventory=[];
for (const {directory,manifest} of collect()) {
  const destination=path.join(target,'third-party-ui',manifest.name);
  fs.cpSync(directory,destination,{recursive:true,dereference:true});
  inventory.push({name:manifest.name,version:manifest.version});
}
fs.writeFileSync(path.join(target,'third-party-ui','inventory.json'),JSON.stringify(inventory,null,2)+'\n');
fs.writeFileSync(path.join(target,'SOURCE-BUILD.md'),`# Rigscope corresponding source

This archive contains the committed project tree, vendored Go module sources,
and the installed browser dependency packages in third-party-ui/, including
their license files. These dependencies retain their original licenses.

See docs/development.md for the Go and Node toolchain versions. Install the
locked UI build tools with npm ci, then run npm run build:ui and
go generate ./internal/hub/webui. Compile with go build -mod=vendor ./cmd/rigscope.
For a release UI set NEXT_PUBLIC_RIGSCOPE_VERSION to the release version.
For a modified network deployment also set NEXT_PUBLIC_RIGSCOPE_SOURCE_URL to
your own corresponding source location, as described in LICENSING.md.

General-purpose build tools are not included. package-lock.json records the
exact build tool packages and their registry integrity hashes.
`);
