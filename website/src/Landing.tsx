import { Fragment, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { Check, Copy } from 'lucide-react';
import { BrandMark } from '../../src/components/brand-mark';
import './landing.css';

const INSTALL_COMMAND = 'curl -fsSL https://rigscope.dev/install.sh | sh';
const SYSTEM_INSTALL_COMMAND = 'curl -fsSL https://rigscope.dev/install.sh | sudo sh -s -- --system';
const INSPECT_COMMAND = 'curl -fsSLO https://rigscope.dev/install.sh && less install.sh && sh install.sh';
const DOCUMENTATION_URL = '/docs/';
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

function InstallCommand({ command, label }: { command: string; label: string }) {
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
          await navigator.clipboard.writeText(command);
          copied = true;
        }
      } catch {
        copied = false;
      }
      if (!copied) {
        try {
          copied = legacyCopy(command);
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
    <div className="install-option">
      <p className="install-label">{label}</p>
      <div className="install">
        <div
          ref={installCommandRef}
          className="install-command"
          role="group"
          aria-label={`${label} command`}
          tabIndex={0}
        >
          <code>{command.split(' ').map((token, index) => (
            <Fragment key={index}>{index > 0 ? ' ' : ''}<span className="install-token">{token}</span></Fragment>
          ))}</code>
        </div>
        <button
          ref={copyButtonRef}
          className="install-copy"
          type="button"
          onClick={copyInstallCommand}
          aria-label={`Copy ${label.toLowerCase()} command`}
          title={copyStatus === 'success' ? 'Copied!' : `Copy ${label.toLowerCase()} command`}
          data-state={copyStatus}
        >
          {copyStatus === 'success' ? (
            <Check size={17} strokeWidth={2.5} aria-hidden="true" />
          ) : (
            <Copy size={17} strokeWidth={2} aria-hidden="true" />
          )}
        </button>
      </div>
      <p
        className="copy-status"
        role="status"
        aria-live="polite"
        aria-atomic="true"
        data-state={copyStatus}
      >
        {STATUS_MESSAGES[copyStatus]}
      </p>
    </div>
  );
}

export function Landing({ children }: { children: ReactNode }) {
  return (
    <div className="landing landing-home" id="top">
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
            <a href="/guides/">Guides</a>
            <a href={SOURCE_URL}>GitHub</a>
          </nav>
        </header>

        <main id="main-content" tabIndex={-1}>
          <section className="hero" aria-labelledby="hero-heading">
            <h1 id="hero-heading">Rigscope — Self-hosted Linux System &amp; Process Monitor</h1>
            <p className="hero-subtitle">
              Your machines, in view. One static Go binary for a workstation, server, or small
              fleet — no Node.js, npm, or database server.
            </p>

            <div className="install-options">
              <InstallCommand command={INSTALL_COMMAND} label="Normal install · your user" />
              <InstallCommand command={SYSTEM_INSTALL_COMMAND} label="System install · all users" />
            </div>

            <p className="requirements">
              <span>Linux</span>
              <span className="requirements-dot" aria-hidden="true">
                ·
              </span>
              <span>systemd</span>
              <span className="requirements-dot" aria-hidden="true">
                ·
              </span>
              <span>amd64 · arm64 · armv7</span>
            </p>

            <p className="install-alternative">
              Prefer to read it first?{' '}
              <code>{INSPECT_COMMAND}</code>
            </p>
          </section>

          <div className="demo-section">{children}</div>

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

          <section className="home-details" aria-labelledby="monitor-heading">
            <h2 id="monitor-heading">Linux monitoring, on your own infrastructure</h2>
            <p>Rigscope brings CPU, memory, GPU, disk, network, and process readings into a browser dashboard. Run a hub for the dashboard and a collector on each Linux machine. The default installation runs both on one computer; collectors on other machines connect outbound to your hub.</p>

            <section className="quickstart" aria-labelledby="quickstart-heading">
              <h2 id="quickstart-heading">Install Rigscope 1.0</h2>
              <p>One static Go binary is the installer, the hub, and the collector. There is no Node.js, npm, Python, Docker, or external database to install or maintain.</p>
              <ol className="quickstart-steps">
                <li>
                  <h3>Install</h3>
                  <pre tabIndex={0}><code>curl -fsSL https://rigscope.dev/install.sh | sh</code></pre>
                  <pre tabIndex={0} aria-label="System install command"><code>{SYSTEM_INSTALL_COMMAND}</code></pre>
                  <p>The first command installs for your user; the second installs system-wide with administrator access. Requires Linux with systemd on amd64, arm64, or armv7. The script verifies the release checksum and starts a hub and collector as systemd services. Re-running it repairs an installation and keeps your credentials.</p>
                </li>
                <li>
                  <h3>Sign in</h3>
                  <p>The installer creates the <code>owner</code> account and asks you to choose a password. Press Enter for a generated password, shown once in the terminal:</p>
                  <pre tabIndex={0}><code>{`Username: owner
Password: <generated>
Save this password; it is shown once.`}</code></pre>
                  <p>Open <code>http://127.0.0.1:8081</code> and sign in as <code>owner</code>.</p>
                </li>
                <li>
                  <h3>Add a machine</h3>
                  <p>Create a single-use grant in <strong>Administration → Add machine</strong>, then run the command it shows on the other Linux machine:</p>
                  <pre tabIndex={0}><code>{`curl -fsSL https://rigscope.dev/install.sh | sh -s -- \\
  --role collector \\
  --hub https://hub.example.com \\
  --token 'PASTE-THE-GRANT'`}</code></pre>
                  <p>The collector connects outbound, so the monitored machine needs no inbound port. One collector talks to one hub in 1.0.</p>
                </li>
                <li>
                  <h3>Upgrade</h3>
                  <pre tabIndex={0}><code>rigscope upgrade</code></pre>
                  <p>An upgrade verifies the release, backs up data, migrates, and verifies health. Use <code>rigscope rollback</code> to return to the previous release when its schema is compatible.</p>
                </li>
              </ol>
            </section>

            <div className="feature-details">
              <section><h3>Inspect resources and running processes</h3><p>Start with machine usage, then inspect individual processes to understand what is using CPU and memory. Process details are fetched while you view them and are never saved to History. Monitoring is read-only: Rigscope does not kill processes or change their priorities.</p><a href="/guides/linux-cpu-memory-processes/">Understand CPU, memory, and process readings</a></section>
              <section><h3>Review history and configure alerts</h3><p>History defaults to bounded RAM storage and is lost on hub restart. Enable persistent SQLite history to retain summaries across restarts. Both modes include a one-second RAM ring for the last 15 minutes. Create threshold, unavailable-reading, and missing-receipt rules, then send notifications to an HTTPS webhook. Collection continues when your browser is closed.</p><a href="/docs/monitoring/">Read about history, retention, and alerts</a></section>
              <section><h3>See the GPU readings your hardware provides</h3><p>Monitor NVIDIA GPUs through nvidia-smi and available AMD counters through Linux driver interfaces. Intel support is partial. Driver support and permissions determine which readings appear; missing readings stay unavailable.</p><a href="/guides/linux-gpu-monitoring/">Check NVIDIA, AMD, and Intel GPU support</a></section>
              <section><h3>Bring multiple machines into one dashboard</h3><p>Enroll collectors with single-use grants and control what each collector shares. Each hub owns its users, permissions, history, and alerts. Network addresses and full command lines are withheld by default.</p><a href="/guides/monitor-multiple-linux-machines/">Set up monitoring for multiple Linux machines</a></section>
            </div>

            <section className="footprint" aria-labelledby="footprint-heading">
              <h2 id="footprint-heading">Light footprint</h2>
              <p>Rigscope 1.0 targets small machines. These are development-host measurements, not capacity guarantees:</p>
              <ul>
                <li><strong>About 13 MB</strong> on disk for the static binary; configuration and state are small.</li>
                <li><strong>About 24 MiB</strong> collector RSS with process detail active.</li>
                <li><strong>About 17 MiB</strong> hub RSS at idle, or about 55 MiB for the hub and collector together with the UI open.</li>
              </ul>
              <p>Memory scales with history retention, process-detail viewers, and the number of connected machines.</p>
            </section>

            <p>Ready to install? Follow the <a href="/docs/installation/">Linux installation guide</a>, or browse the <a href="/docs/">documentation</a> for configuration, backups, and exports.</p>
          </section>
        </main>

        <footer className="site-footer">
          <p>Rigscope — a self-hosted Linux resource and process monitor.</p>
          <nav className="footer-links" aria-label="Footer">
            <a href={DOCUMENTATION_URL}>Documentation</a>
            <a href="/guides/">Guides</a>
            <a href={LICENSE_URL}>AGPLv3 license</a>
            <a href="/NOTICES.txt">Third-Party Notices</a>
            <a href={CONTACT_URL} className="footer-contact">
              Contact on GitHub
            </a>
          </nav>
        </footer>
      </div>
    </div>
  );
}
