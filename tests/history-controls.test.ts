import assert from "node:assert/strict";
import test from "node:test";
import { build } from "esbuild";
import { createRequire } from "node:module";

async function renderControls(props: Record<string, unknown>): Promise<string> {
  const bundle = await build({
    stdin: {
      contents: `import { renderToStaticMarkup } from 'react-dom/server';
        import { createElement } from 'react';
        import { HistoryControls } from './src/components/history/history-controls';
        module.exports = renderToStaticMarkup(createElement(HistoryControls, {
          range: '10m', selectedMetrics: [], catalog: null, catalogError: null,
          gpuCards: [], hiddenCards: new Set(), autoRefresh: true,
          statusText: 'Updated 12:00:00', onRangeChange() {}, onToggleMetric() {},
          onSetMetrics() {}, onToggleCard() {}, onToggleAutoRefresh() {}, onRefresh() {},
          ...${JSON.stringify(props)}
        }));`,
      resolveDir: process.cwd(),
    },
    bundle: true, write: false, platform: "node", format: "cjs", jsx: "automatic", packages: "external",
  });
  const output = { exports: "" };
  new Function("require", "module", "exports", bundle.outputFiles[0]!.text)(createRequire(import.meta.url), output, output.exports);
  return output.exports;
}

function refreshButton(html: string): string {
  return html.match(/<button\b[^>]*>[\s\S]*?<\/button>/g)!.find((button) => button.includes("Refresh</button>"))!;
}

test("automatic history polling leaves Refresh visually unchanged", async () => {
  const idle = refreshButton(await renderControls({ loading: false, manualRefreshing: false }));
  const polling = refreshButton(await renderControls({ loading: true, manualRefreshing: false }));
  assert.equal(polling, idle, "background polling must not dim, disable or spin Refresh");
});

test("manual refresh provides feedback until the request settles", async () => {
  const manual = refreshButton(await renderControls({ loading: true, manualRefreshing: true }));
  assert.match(manual, /disabled=""/);
  assert.match(manual, /animate-spin/);
  const idle = refreshButton(await renderControls({ loading: false, manualRefreshing: false }));
  assert.doesNotMatch(idle, /disabled=""|animate-spin/);
});


test("CPU selection offers all cores without a native select menu", async () => {
  const html = await renderControls({
    loading: false, manualRefreshing: false, selectedMetrics: ["cpu.core"],
    machine: { machineId: "many-cores", inventory: { cpu: { logicalCores: 48 } } },
  });
  assert.match(html, /48 \/ 48/);
  assert.match(html, /Core 47/);
  assert.match(html, /All cores/);
  assert.doesNotMatch(html, /<select|Add core|First 8|Hide a core to add another/);
});
