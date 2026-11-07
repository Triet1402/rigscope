import {readFile, mkdir, writeFile} from 'node:fs/promises';
import {resolve, posix} from 'node:path';
import {Marked, Renderer} from 'marked';
import GithubSlugger from 'github-slugger';

export const escapeHtml = value => String(value).replace(/[&<>"']/g, c => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[c]));
export function documentHtml({title, description, path, body, css, script = ''}) {
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="preload" href="/fonts/geist-latin-variable.woff2" as="font" type="font/woff2" crossorigin>
<title>${escapeHtml(title)}</title><meta name="description" content="${escapeHtml(description)}">
<link rel="canonical" href="https://rigscope.dev${escapeHtml(path)}">
<meta property="og:url" content="https://rigscope.dev${escapeHtml(path)}"><meta property="og:title" content="${escapeHtml(title)}"><meta property="og:description" content="${escapeHtml(description)}"><meta property="og:type" content="website">
<meta name="theme-color" content="#181a1e"><link rel="icon" href="/favicon.ico" sizes="any"><link rel="icon" type="image/png" href="/favicon.png" sizes="512x512"><link rel="icon" href="/icon.svg" type="image/svg+xml"><link rel="apple-touch-icon" href="/apple-touch-icon.png"><link rel="stylesheet" href="${escapeHtml(css)}">
</head><body>${body}${script ? `<script type="module" src="${escapeHtml(script)}"></script>` : ''}</body></html>`;
}

const docs = [
  ['installation', 'Installation', 'Install Rigscope on Linux with npm, configure HTTPS access, add machines, and check supported GPU hardware.'],
  ['monitoring', 'Monitoring and alerts', 'Understand Rigscope collection, SQLite history retention, process details, user access, and webhook alerts.'],
  ['configuration', 'Configuration reference', 'Reference for Rigscope hub and collector settings, sharing policies, and source enrollment.'],
  ['operations', 'Operations', 'Diagnose Rigscope services, back up monitoring data, recover access, and manage upgrades and storage pressure.'],
  ['system-installation', 'System installation', 'Run Rigscope with dedicated service accounts and system units, including optional CPU package-power access.'],
  ['bottlenecks', 'Sensor readings', 'Understand Linux CPU pressure, temperature, package power, memory, disk latency, and other Rigscope sensor readings.'],
  ['exports', 'Export monitoring data', 'Export Rigscope historical monitoring readings and live process details as CSV and JSON.'],
].map(([slug, title, description]) => ({path:`/docs/${slug}/`, title, description, source:`docs/${slug}.md`, section:'Documentation'}));
const guides = [
  ['monitor-multiple-linux-machines', 'Monitor multiple Linux machines from one dashboard', 'Set up a Rigscope hub and enroll Linux collectors with HTTPS, private connection grants, and per-hub sharing settings.'],
  ['linux-cpu-memory-processes', 'Understand Linux CPU, memory, and process readings', 'Interpret Linux machine and process CPU, memory pressure, historical readings, and alerts with Rigscope.'],
  ['linux-gpu-monitoring', 'Monitor NVIDIA and AMD GPUs on Linux with Rigscope', 'Check Linux GPU monitoring support for NVIDIA, AMD, and Intel, then inspect available utilization, VRAM, temperature, and power readings.'],
].map(([slug, title, description]) => ({path:`/guides/${slug}/`, title, description, source:`website/content/guides/${slug}.md`, section:'Guides'}));

export async function buildContent({root, dist, renderContent, css}) {
  const repo = resolve(root, '..');
  const pages = [...docs, ...guides];
  const localSources = new Map(pages.map(page => [page.source, page.path]));
  const indexes = [
    {path:'/docs/', title:'Rigscope documentation', description:'Install, configure, and operate your self-hosted Linux system and process monitor.', section:'Documentation', entries:docs},
    {path:'/guides/', title:'Linux monitoring guides', description:'Practical guides to Linux fleet monitoring, CPU and memory readings, and GPU telemetry with Rigscope.', section:'Guides', entries:guides},
  ];
  for (const page of [...pages, ...indexes]) {
    const headings = [];
    let html;
    if (page.entries) {
      html = `<p>${escapeHtml(page.description)}</p>` + page.entries.map(entry => `<section><h2><a href="${entry.path}">${escapeHtml(entry.title)}</a></h2><p>${escapeHtml(entry.description)}</p></section>`).join('');
      html += `<p>Explore the <a href="${page.section === 'Guides' ? '/docs/' : '/guides/'}">${page.section === 'Guides' ? 'documentation' : 'practical guides'}</a> or <a href="/#demo">try the interactive demo</a>.</p>`;
    } else {
      const slugger = new GithubSlugger();
      const markdown = new Marked({gfm:true});
      markdown.use({renderer:{
        heading({tokens, depth, text}) {
          const id = slugger.slug(text.replace(/[`*_]/g, ''));
          if (depth === 1) return '';
          if (depth === 2) headings.push({id, text:text.replace(/[`*_]/g, '')});
          return `<h${depth} id="${escapeHtml(id)}">${this.parser.parseInline(tokens)}</h${depth}>`;
        },
        table(token) {
          // Markdown tables are horizontally scrollable without widening the page.
          return `<div class="table-scroll" role="region" aria-label="Reference table" tabindex="0">${Renderer.prototype.table.call(this, token)}</div>`;
        },
        code({text, lang}) {
          return `<pre tabindex="0"><code${lang ? ` class="language-${escapeHtml(lang)}"` : ''}>${escapeHtml(text)}</code></pre>`;
        },
      }, walkTokens(token) {
        if (token.type !== 'link' && token.type !== 'image') return;
        if (/^(?:[a-z]+:|\/|#)/i.test(token.href)) return;
        const [file, fragment] = token.href.split('#');
        const resolved = posix.normalize(posix.join(posix.dirname(page.source), file));
        token.href = (localSources.get(resolved) ?? `https://github.com/Triet1402/rigscope/blob/main/${resolved}`) + (fragment ? '#' + fragment : '');
      }});
      // Only repository-owned Markdown is compiled; no user-supplied HTML is accepted.
      html = markdown.parse(await readFile(resolve(repo, page.source), 'utf8'));
    }
    const body = renderContent({...page, html, headings, pages:page.section === 'Guides' ? guides : docs});
    const directory = resolve(dist, '.' + page.path);
    await mkdir(directory, {recursive:true});
    await writeFile(resolve(directory, 'index.html'), documentHtml({title:page.title + ' | Rigscope', description:page.description, path:page.path, body, css}));
  }
  const urls = ['/', ...indexes.map(page => page.path), ...pages.map(page => page.path)];
  await writeFile(resolve(dist, 'sitemap.xml'), `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n${urls.map(path => `  <url><loc>https://rigscope.dev${path}</loc></url>`).join('\n')}\n</urlset>\n`);
  await writeFile(resolve(dist, 'robots.txt'), 'User-agent: *\nAllow: /\n\nSitemap: https://rigscope.dev/sitemap.xml\n');
}
