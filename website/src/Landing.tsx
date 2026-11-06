import { useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { Check, Copy } from 'lucide-react';
import { BrandMark } from '../../src/components/brand-mark';
import './landing.css';

const INSTALL_COMMAND = 'npx --yes rigscope@latest install --boot';
const DOCUMENTATION_URL = 'https://github.com/Triet1402/rigscope#documentation';
const SOURCE_URL = 'https://github.com/Triet1402/rigscope';
const LICENSE_URL = 'https://github.com/Triet1402/rigscope/blob/main/LICENSE';
const CONTACT_URL = 'https://github.com/Triet1402';
const COPY_RESET_MS = 5000;

type CopyStatus = 'idle' | 'success' | 'error';

const STATUS_MESSAGES: Record<CopyStatus, string> = {
  idle: '',
  success: 'Install command copied to the clipboard.',
  error: 'Copy failed. The command is selected — press Ctrl+C (⌘C on Mac) to copy.',
};

/**
 * Executes a legacy copy through a temporary textarea. This is the fallback for
 * plain-HTTP origins, including a tailnet address, where the Clipboard API is
 * unavailable or rejects the write.
 */
function legacyCopy(text: string): boolean {
  const textarea = document.createElement('textarea');
  textarea.value = text;
  textarea.readOnly = true;
  textarea.tabIndex = -1;
  textarea.setAttribute('aria-hidden', 'true');
  textarea.style.position = 'fixed';
  textarea.style.top = '0';
  textarea.style.left = '0';
  textarea.style.width = '1px';
  textarea.style.height = '1px';
  textarea.style.padding = '0';
  textarea.style.border = '0';
  textarea.style.opacity = '0';
  document.body.appendChild(textarea);

  try {
    textarea.focus({ preventScroll: true });
    textarea.select();
    textarea.setSelectionRange(0, text.length);
    return document.execCommand('copy');
  } catch {
    return false;
  } finally {
    textarea.remove();
  }
}

export function Landing({ children }: { children: ReactNode }) {
  const [copyStatus, setCopyStatus] = useState<CopyStatus>('idle');
  const copyButtonRef = useRef<HTMLButtonElement>(null);
  const installCommandRef = useRef<HTMLDivElement>(null);
  const resetTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const copyingRef = useRef(false);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (resetTimerRef.current !== null) {
        clearTimeout(resetTimerRef.current);
      }
    };
  }, []);

  function selectInstallCommand() {
    const field = installCommandRef.current;
    const command = field?.querySelector('code');
    if (!field || !command) return;

    field.focus({ preventScroll: true });
    const range = document.createRange();
    range.selectNodeContents(command);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
  }

  async function copyInstallCommand() {
    if (copyingRef.current) return;
    copyingRef.current = true;

    if (resetTimerRef.current !== null) {
      clearTimeout(resetTimerRef.current);
      resetTimerRef.current = null;
    }

    let copied = false;
    try {
      try {
        if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
          await navigator.clipboard.writeText(INSTALL_COMMAND);
          copied = true;
        }
      } catch {
        copied = false;
      }
      if (!copied) {
        try {
          copied = legacyCopy(INSTALL_COMMAND);
        } catch {
          copied = false;
        }
      }
    } finally {
      copyingRef.current = false;
    }

    if (!mountedRef.current) return;

    if (copied) {
      setCopyStatus('success');
      copyButtonRef.current?.focus({ preventScroll: true });
    } else {
      setCopyStatus('error');
      selectInstallCommand();
    }

    resetTimerRef.current = setTimeout(() => {
      setCopyStatus('idle');
      resetTimerRef.current = null;
    }, COPY_RESET_MS);
  }

  return (
    <div className="landing" id="top">
      <a className="skip-link" href="#main-content">
        Skip to main content
      </a>

      <div className="landing-shell">
        <header className="site-header">
          <a className="brand" href="#top">
            <BrandMark className="brand-mark" />
            <span>Rigscope</span>
          </a>
          <nav className="site-nav" aria-label="Primary">
            <a href="#demo">Demo</a>
            <a href={DOCUMENTATION_URL}>Documentation</a>
            <a href={SOURCE_URL}>GitHub</a>
          </nav>
        </header>

        <main id="main-content" tabIndex={-1}>
          <section className="hero" aria-labelledby="hero-heading">
            <h1 id="hero-heading">Your machines, in view.</h1>
            <p className="hero-subtitle">
              A self-hosted Linux system and process monitor.
            </p>

            <div className="install">
              <div
                ref={installCommandRef}
                className="install-command"
                role="group"
                aria-label="Install command"
                tabIndex={0}
              >
                <code>{INSTALL_COMMAND}</code>
              </div>
              <button
                ref={copyButtonRef}
                className="install-copy"
                type="button"
                onClick={copyInstallCommand}
                aria-label="Copy install command"
              >
                {copyStatus === 'success' ? (
                  <Check size={17} strokeWidth={2.5} aria-hidden="true" />
                ) : (
                  <Copy size={17} strokeWidth={2} aria-hidden="true" />
                )}
                <span>Copy</span>
              </button>
            </div>

            <p className="requirements">
              <span>Requires Linux</span>
              <span className="requirements-dot" aria-hidden="true">
                ·
              </span>
              <span>Node.js 26+</span>
              <span className="requirements-dot" aria-hidden="true">
                ·
              </span>
              <span>systemd</span>
            </p>

            <p
              className="copy-status"
              role="status"
              aria-live="polite"
              aria-atomic="true"
              data-state={copyStatus}
            >
              {STATUS_MESSAGES[copyStatus]}
            </p>
          </section>

          <ul className="benefits" aria-label="Why Rigscope">
            <li className="benefit">
              <h2>Self-hosted &amp; private</h2>
              <p>Run it on your own infrastructure.</p>
            </li>
            <li className="benefit">
              <h2>Systems and processes</h2>
              <p>CPU, memory, GPU and process visibility.</p>
            </li>
            <li className="benefit">
              <h2>From one machine to a small fleet</h2>
              <p>Monitor a single Linux host or a few of them.</p>
            </li>
          </ul>

          <div className="demo-section">{children}</div>
        </main>

        <footer className="site-footer">
          <p>Rigscope — a self-hosted Linux resource and process monitor.</p>
          <nav className="footer-links" aria-label="Footer">
            <a href={DOCUMENTATION_URL}>Documentation</a>
            <a href={LICENSE_URL}>MIT license</a>
            <a href={CONTACT_URL} className="footer-contact">
              Contact on GitHub
            </a>
          </nav>
        </footer>
      </div>
    </div>
  );
}
