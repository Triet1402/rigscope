import {build} from 'esbuild';
import {buildContent, documentHtml} from './content.mjs';
import {createHash} from 'node:crypto';
import {mkdir, readFile, writeFile, cp, rm} from 'node:fs/promises';
import postcss from 'postcss';
import tailwind from '@tailwindcss/postcss';
import {fileURLToPath, pathToFileURL} from 'node:url';
import {resolve} from 'node:path';

const root = fileURLToPath(new URL('..', import.meta.url));
const dist = resolve(root, 'dist');
await mkdir(dist, {recursive:true});
const asset = async name => '/' + name + '?v=' + createHash('sha256').update(await readFile(resolve(dist, name))).digest('hex').slice(0, 12);
await mkdir(resolve(root, '.build'), {recursive:true});
const shared = {
  absWorkingDir:root, bundle:true, jsx:'automatic', minify:true, external:['/fonts/*'],
  define:{'process.env.NODE_ENV':'"production"'}, logLevel:'info',
  // TypeScript maps React types into this app for the shared BrandMark. Runtime
  // imports must resolve to actual packages instead of those declaration files.
  tsconfigRaw:{compilerOptions:{jsx:'react-jsx'}},
};
await build({...shared, entryPoints:['src/client.tsx'], outfile:'dist/app.js', platform:'browser', target:['es2022'], format:'esm', alias:{react:resolve(root,'node_modules/react'), 'react-dom':resolve(root,'node_modules/react-dom')}});
await build({...shared, entryPoints:['src/render.tsx'], outfile:'.build/render.mjs', platform:'node', format:'esm', packages:'external', loader:{'.css':'empty'}});
const {render, renderContent} = await import(pathToFileURL(resolve(root, '.build/render.mjs')).href + '?t=' + Date.now());
const body = render();
await cp(resolve(root, 'public'), dist, {recursive:true});
const notices = ['Rigscope website third-party notices\n\nGeist font (SIL Open Font License 1.1)\n' + await readFile(resolve(root, 'public/fonts/Geist-LICENSE.txt'), 'utf8')];
for (const [name, license] of [['react','LICENSE'], ['react-dom','LICENSE'], ['scheduler','LICENSE'], ['next-themes','license.md'], ['lucide-react','LICENSE'], ['tailwindcss','LICENSE']]) {
  const manifest = JSON.parse(await readFile(resolve(root, 'node_modules', name, 'package.json'), 'utf8'));
  notices.push(`${name}@${manifest.version}\n\n${await readFile(resolve(root, 'node_modules', name, license), 'utf8')}`);
}
await writeFile(resolve(dist, 'NOTICES.txt'), notices.join('\n\n---\n\n'));

await writeFile(resolve(dist, 'index.html'), documentHtml({
  title:'Rigscope — Self-hosted Linux System & Process Monitor',
  description:'Monitor Linux CPU, memory, GPUs, disks, networks, and processes with Rigscope. Self-host your dashboard, review history, and set alerts for your machines.',
  path:'/', body:`<div id="root">${body}</div>`, css:await asset('app.css'), script:await asset('app.js'),
}));
await buildContent({root, dist, renderContent, css:await asset('app.css')});
await rm(resolve(root, '.build/render.mjs'));

// Bundle the actual product screens. Only routing and data hooks are replaced;
// UI components, presentation logic, and styles remain the product's own files.
const demoDir = resolve(root, 'src/product-demo');
const productRoot = resolve(root, '../src');
const adapters = {
  '@/live/live-provider':resolve(demoDir, 'live.tsx'),
  'next/link':resolve(demoDir, 'navigation.tsx'),
  'next/navigation':resolve(demoDir, 'navigation.tsx'),
};
await build({...shared, entryPoints:['src/product-demo/entry.tsx'], outfile:'dist/product-demo.js',
  platform:'browser', target:['es2022'], format:'esm',
  alias:{...adapters, '@':productRoot, react:resolve(root,'node_modules/react'), 'react-dom':resolve(root,'node_modules/react-dom'), 'next-themes':resolve(root,'node_modules/next-themes'), 'lucide-react':resolve(root,'node_modules/lucide-react')},
  plugins:[{name:'synthetic-product-data', setup(builder) {
    builder.onResolve({filter:/\/use-stored-history$/}, () => ({path:resolve(demoDir,'history.ts')}));
    builder.onResolve({filter:/\/use-process-detail$/}, () => ({path:resolve(demoDir,'processes.ts')}));
  }}],
});
const productStyles = (await readFile(resolve(productRoot, 'app/globals.css'), 'utf8'))
  .replace('@import "tailwindcss";', `@import "tailwindcss" source(none);\n@source "${productRoot}";`);
const css = await postcss([tailwind()]).process(productStyles, {from:resolve(root,'.build/product.css')});
await writeFile(resolve(dist,'product-demo.css'), css.css);
await writeFile(resolve(dist,'demo.html'), `<!doctype html><html lang="en" class="dark"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex">
<title>Rigscope interactive demo — synthetic data</title><link rel="icon" href="/icon.svg"><link rel="stylesheet" href="${await asset('product-demo.css')}">
</head><body><div style="height:32px;display:flex;align-items:center;justify-content:center;gap:8px;border-bottom:1px solid var(--border);font-size:12px;color:var(--muted-foreground)" role="note">Synthetic demo · All machines and readings are simulated.</div>
<style>@media (min-width: 960px) { html { font-size: 18px; } } #root > .h-dvh { height:calc(100dvh - 32px) }</style>
<div id="root"><p style="padding:24px">Loading Rigscope demo…</p></div><script type="module" src="${await asset('product-demo.js')}"></script></body></html>`);
console.log('Built static Rigscope website in website/dist');
