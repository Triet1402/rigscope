import assert from 'node:assert/strict';
import {readFile, readdir, stat} from 'node:fs/promises';
import {resolve} from 'node:path';
import {fileURLToPath} from 'node:url';
import {spawn} from 'node:child_process';
import {once} from 'node:events';

const root = fileURLToPath(new URL('../dist/', import.meta.url));
async function htmlFiles(directory) {
  const files = [];
  for (const entry of await readdir(directory, {withFileTypes:true})) {
    const path = resolve(directory, entry.name);
    if (entry.isDirectory()) files.push(...await htmlFiles(path));
    else if (entry.name.endsWith('.html')) files.push(path);
  }
  return files;
}
const files = await htmlFiles(root);
const indexed = files.filter(file => !file.endsWith('/demo.html'));
const sitemap = await readFile(resolve(root, 'sitemap.xml'), 'utf8');
const locations = [...sitemap.matchAll(/<loc>(.*?)<\/loc>/g)].map(match => match[1]);
assert.equal(locations.length, indexed.length);
assert.equal(new Set(locations).size, locations.length);
const titles = new Set();
const descriptions = new Set();
const decode = value => value.replaceAll('&amp;', '&');
for (const file of indexed) {
  const html = await readFile(file, 'utf8');
  const path = '/' + file.slice(root.length).replace(/index\.html$/, '');
  const canonical = 'https://rigscope.dev' + path;
  assert.ok(locations.includes(canonical), `Missing sitemap URL: ${canonical}`);
  assert.ok(html.includes(`<link rel="canonical" href="${canonical}">`));
  assert.equal([...html.matchAll(/<h1\b/g)].length, 1, `Expected one h1 in ${path}`);
  assert.ok(!html.includes('content="noindex"'));
  const title = html.match(/<title>(.*?)<\/title>/)?.[1];
  const description = html.match(/<meta name="description" content="(.*?)"/)?.[1];
  assert.ok(title && !titles.has(title), `Missing or duplicate title: ${path}`);
  assert.ok(description && !descriptions.has(description), `Missing or duplicate description: ${path}`);
  titles.add(title); descriptions.add(description);
  const ids = [...html.matchAll(/\bid="([^"]+)"/g)].map(match => match[1]);
  assert.equal(new Set(ids).size, ids.length, `Duplicate IDs: ${path}`);
  for (const match of html.matchAll(/\b(?:href|src)="([^"]+)"/g)) {
    const href = decode(match[1]);
    const url = new URL(href, canonical);
    if (url.origin !== 'https://rigscope.dev') continue;
    const target = resolve(root, '.' + url.pathname + (url.pathname.endsWith('/') ? 'index.html' : ''));
    assert.ok((await stat(target)).isFile(), `Broken local link in ${path}: ${href}`);
    if (url.hash && !url.hash.startsWith('#/')) {
      const targetHtml = target === file ? html : await readFile(target, 'utf8');
      assert.ok(targetHtml.includes(`id="${decodeURIComponent(url.hash.slice(1))}"`), `Broken anchor in ${path}: ${href}`);
    }
  }
}
assert.ok((await readFile(resolve(root, 'demo.html'), 'utf8')).includes('content="noindex"'));
assert.ok(!sitemap.includes('demo.html'));
assert.ok((await readFile(resolve(root, 'robots.txt'), 'utf8')).includes('Sitemap: https://rigscope.dev/sitemap.xml'));

const server = spawn(process.execPath, [fileURLToPath(new URL('./serve.mjs', import.meta.url))], {env:{...process.env, PORT:'0', HOST:'127.0.0.1'}, stdio:['ignore','pipe','inherit']});
try {
  const [chunk] = await once(server.stdout, 'data');
  const origin = chunk.toString().match(/http:\/\/127\.0\.0\.1:\d+/)?.[0];
  assert.ok(origin, 'Server did not announce a port');
  for (const url of locations) assert.equal((await fetch(origin + new URL(url).pathname)).status, 200, url);
  for (const path of ['/docs', '/guides', '/docs/installation', '/docs/installation/index.html', '/index.html']) {
    const response = await fetch(origin + path + '?ref=check', {redirect:'manual'});
    assert.equal(response.status, 308, path);
    assert.ok(response.headers.get('location').endsWith('/?ref=check'));
  }
  const xml = await fetch(origin + '/sitemap.xml');
  assert.equal(xml.headers.get('content-type'), 'application/xml; charset=utf-8');
  assert.equal((await fetch(origin + '/missing-page')).status, 404);
  assert.equal((await fetch(origin + '/docs/missing-page/')).status, 404);
  assert.equal((await fetch(origin + '/docs/installation/', {method:'HEAD'})).status, 200);
} finally {
  server.kill('SIGTERM');
  await once(server, 'exit');
}
console.log(`Verified ${indexed.length} indexable pages, metadata, local links and anchors, sitemap, demo exclusion, and HTTP routes.`);
