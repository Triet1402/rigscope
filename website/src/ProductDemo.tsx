import {ExternalLink} from 'lucide-react';
import './product-demo.css';

export function Demo() {
  return <section id="demo" className="product-demo" aria-label="Interactive Rigscope product demo">
    <div className="product-demo-caption">
      <p><span className="product-demo-dot" aria-hidden="true" />Interactive demo <span className="product-demo-note">Synthetic data</span></p>
      <a href="/demo.html#/machines/workstation" target="_blank" rel="noreferrer">Open full demo <ExternalLink size={14} aria-hidden="true" /></a>
    </div>
    <iframe src="/demo.html#/machines/workstation" title="Rigscope product UI with synthetic machines — Fleet, Overview, History, and Processes" loading="lazy" />
  </section>;
}
