#!/usr/bin/env node
// Preserve upstream license/notice texts, including Next's vendored components.
// Include build dependencies conservatively: their output can enter browser assets.
const fs = require('node:fs');
const path = require('node:path');

function generate(root = path.join(__dirname, '..')) {
  const lock = JSON.parse(fs.readFileSync(path.join(root, 'package-lock.json'), 'utf8'));
  const sections = [];
  const inventory = [];
  const missing = [];
  for (const [directory, entry] of Object.entries(lock.packages).sort(([a], [b]) => a.localeCompare(b))) {
    if (!directory) continue;
    const absolute = path.join(root, directory);
    if (!fs.existsSync(absolute)) continue; // Platform-specific optional dependencies.
    const manifest = JSON.parse(fs.readFileSync(path.join(absolute, 'package.json'), 'utf8'));
    inventory.push(`| ${manifest.name} | ${manifest.version} | ${entry.license || manifest.license || 'See upstream text'} | ${entry.dev ? 'build' : 'runtime'} |`);
    let found = 0;
    function visit(relative = '') {
      for (const item of fs.readdirSync(path.join(absolute, relative), {withFileTypes:true}).sort((a,b) => a.name.localeCompare(b.name))) {
        if (item.isSymbolicLink() || item.name === 'node_modules' || item.name === '.git') continue;
        const file = path.join(relative, item.name);
        if (item.isDirectory()) { visit(file); continue; }
        if (!/^(licen[cs]e|notice|copying|copyright)([._-]|$)/i.test(item.name)) continue;
        const content = fs.readFileSync(path.join(absolute, file), 'utf8');
        if (!content.trim()) continue;
        sections.push(`\n---\n\n## ${manifest.name}@${manifest.version}: ${file}\n\n${content.trim()}\n`);
        found++;
      }
    }
    visit();
    if (!found) {
      // These split packages inherit the parent project's license text.
      const parentLicense = manifest.name.startsWith('@next/') ? 'node_modules/next/license.md'
        : manifest.name.startsWith('@esbuild/') ? 'node_modules/esbuild/LICENSE.md'
        : manifest.name === 'client-only' ? 'node_modules/react/LICENSE' : undefined;
      if (parentLicense) {
        sections.push(`\n---\n\n## ${manifest.name}@${manifest.version}: ${parentLicense}\n\n${fs.readFileSync(path.join(root, parentLicense), 'utf8').trim()}\n`);
      } else if (manifest.name.startsWith('@img/sharp-libvips-')) {
        sections.push(`\n---\n\n## ${manifest.name}@${manifest.version}: upstream distribution notes\n\n${fs.readFileSync(path.join(absolute, 'README.md'), 'utf8').trim()}\n`);
      } else missing.push(`${manifest.name}@${manifest.version}`);
    }
  }
  if (missing.length) throw new Error(`No upstream license text found: ${missing.join(', ')}`);
  const header = `# Third-party notices\n\nRigscope's MIT license covers its own code. Upstream components retain their own\nlicenses and copyright notices reproduced below. This inventory includes installed\nruntime dependencies, vendored components and build tools conservatively. Optional\nnative dependencies vary by target platform and are installed separately by npm;\nretain the license and notice files in those packages when redistributing them.\nThis file does not relicense those components under MIT.\n\nSharp's optional libvips binaries use LGPL-3.0-or-later and other upstream licenses;\nsee the package's bundled notices and https://sharp.pixelplumbing.com/install/#licensing.\nRigscope's npm archive does not embed those binaries. Independently redistributing\nthem requires satisfying their own license/source obligations.\n\n| Package | Version | Declared license | Use |\n| --- | --- | --- | --- |\n`;
  return header + inventory.join('\n') + '\n' + sections.join('');
}

if (require.main === module) {
  const output = generate();
  fs.writeFileSync(path.join(__dirname, '..', 'THIRD_PARTY_NOTICES.md'), output);
  console.log(`third-party notices: ${Buffer.byteLength(output)} bytes`);
}
module.exports = {generate};
