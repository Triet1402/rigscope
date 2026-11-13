import { BrandMark } from '../../src/components/brand-mark';

export type ContentPageProps = {
  title: string;
  html: string;
  section: 'Documentation' | 'Guides';
  pages: {path: string; title: string}[];
  headings: {id: string; text: string}[];
  path: string;
};

export function ContentPage({title, html, section, pages, headings, path}: ContentPageProps) {
  const sectionPath = section === 'Documentation' ? '/docs/' : '/guides/';
  return <div className="landing content-page">
    <a className="skip-link" href="#main-content">Skip to main content</a>
    <div className="landing-shell">
      <header className="site-header">
        <a className="brand" href="/"><BrandMark className="brand-mark" /><span>Rigscope</span></a>
        <nav className="site-nav" aria-label="Primary">
          <a href="/#demo">Demo</a><a href="/docs/">Documentation</a><a href="/guides/">Guides</a>
          <a href="https://github.com/Triet1402/rigscope">GitHub</a>
        </nav>
      </header>
      <div className="reading-layout">
        <aside className="reading-nav">
          <nav aria-label={section}><a href={sectionPath}>{section}</a><ul>
            {pages.map(page => <li key={page.path}><a href={page.path} aria-current={path === page.path ? 'page' : undefined}>{page.title}</a></li>)}
          </ul></nav>
          {headings.length > 0 && <nav aria-label="On this page"><p>On this page</p><ul>
            {headings.map(heading => <li key={heading.id}><a href={'#' + heading.id}>{heading.text}</a></li>)}
          </ul></nav>}
        </aside>
        <main id="main-content" tabIndex={-1} className="reading-main">
          <article className="prose"><h1>{title}</h1><div dangerouslySetInnerHTML={{__html: html}} /></article>
        </main>
      </div>
      <footer className="site-footer"><p>Rigscope — Your machines, in view.</p><nav className="footer-links" aria-label="Footer"><a href="/docs/installation/">Install Rigscope</a><a href="/docs/">Documentation</a><a href="/guides/">Guides</a><a href="https://github.com/Triet1402/rigscope">Source on GitHub</a><a href="https://github.com/Triet1402/rigscope/blob/main/LICENSE">AGPLv3 license</a></nav></footer>
    </div>
  </div>;
}
