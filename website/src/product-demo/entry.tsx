import {createRoot} from 'react-dom/client';
import {ThemeProvider} from '../../../src/components/theme-provider';
import {FleetView} from '../../../src/components/live/fleet-view';
import {MachineView} from '../../../src/components/live/machine-view';
import {HistoryView} from '../../../src/components/history/history-view';
import {ProcessesView} from '../../../src/components/processes/processes-view';
import {AppShell} from '../../../src/components/shell/app-shell';
import {Panel} from '../../../src/components/panel';
import {HISTORY_METRICS} from '../../../src/history/metrics';
import {DemoLiveProvider} from './live';
import DemoLink, {usePathname} from './navigation';

// The isolated demo document never connects to a hub. This is the only fetch
// used by the four views once their data hooks are adapted for synthetic data.
window.fetch = async (input) => {
  const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, location.href);
  return url.pathname === '/api/history/metrics'
    ? Response.json({metrics:Object.fromEntries(HISTORY_METRICS.map(metric => [metric.metric, {unit:metric.unit}]))})
    : Response.json({error:'This action is unavailable in the synthetic demo.'}, {status:403});
};

// Keep the product's native skip link from becoming a demo hash route.
document.addEventListener('click', event => {
  if (!(event.target instanceof Element)) return;
  if (event.target.closest('a[href="#main-content"]')) {
    event.preventDefault();
    document.getElementById('main-content')?.focus();
  }
});

function ProductDemo() {
  const path = usePathname();
  if (path === '/') return <FleetView />;
  if (path.startsWith('/machines/')) return <MachineView machineId={decodeURIComponent(path.slice('/machines/'.length))} />;
  if (path === '/history') return <HistoryView />;
  if (path === '/processes') return <ProcessesView />;
  const title = path.startsWith('/administration') ? 'Administration' : path === '/alerts' ? 'Alerts' : 'Connections';
  return <AppShell title={title}><Panel title={`${title} in your installation`}>
    <p className="text-sm text-muted-foreground">The public demo includes Fleet, Overview, History, and Processes. Install Rigscope to configure your own machines, alerts, and access.</p>
    <DemoLink href="/" className="mt-4 text-sm text-accent hover:underline">Back to Fleet</DemoLink>
  </Panel></AppShell>;
}

createRoot(document.getElementById('root')!).render(
  <ThemeProvider attribute="class" defaultTheme="dark" enableSystem={false} storageKey="rigscope-public-demo-theme">
    <DemoLiveProvider><ProductDemo /></DemoLiveProvider>
  </ThemeProvider>,
);
