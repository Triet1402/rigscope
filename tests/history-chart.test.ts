import assert from "node:assert/strict";
import { createRequire } from "node:module";
import test from "node:test";
import { build } from "esbuild";
import type { HistorySeries } from "../src/hub/storage/history-query";

test("the history chart preserves CPU values when a 601-bucket window loses its oldest bucket", async () => {
  const bundle = await build({
    stdin: {
      contents: `import { renderToStaticMarkup } from 'react-dom/server';
        import { createElement } from 'react';
        import { HistoryChart } from './src/components/history/history-chart';
        module.exports = (points, fromMs, toMs) => renderToStaticMarkup(createElement(HistoryChart, {
          title: 'CPU usage', unit: 'percent', fromMs, toMs, stepMs: 6000,
          series: [{ key: 'cpu', label: 'CPU', metric: 'cpu.usage', resourceId: '', color: 'red', points }],
          gaps: [], hiddenKeys: new Set(), onToggleSeries() {}, emptyMessage: 'No readings'
        }));`,
      resolveDir: process.cwd(),
    },
    bundle: true, write: false, platform: "node", format: "cjs", jsx: "automatic", packages: "external",
  });
  const output = { exports: {} };
  new Function("require", "module", "exports", bundle.outputFiles[0]!.text)(createRequire(import.meta.url), output, output.exports);
  const render = output.exports as (points: HistorySeries["points"], fromMs: number, toMs: number) => string;
  const points = Array.from({ length: 601 }, (_, index) => {
    const value = index % 2 ? 90 : 10;
    return { t: index * 6000, min: value, max: value, avg: value };
  });
  const lineHeights = (html: string): string[] => {
    const path = html.match(/<path\b[^>]*fill="none"[^>]*>/)?.[0];
    assert.ok(path, "CPU average line is present");
    return Array.from(path.matchAll(/[ML][\d.]+,([\d.]+)/g), (match) => match[1]!);
  };
  const before = lineHeights(render(points, 500, 3_600_500));
  const after = lineHeights(render(points.slice(1), 6500, 3_606_500));
  assert.equal(before.length, 601, "draw each API bucket without merging historical readings");
  assert.deepEqual(after, before.slice(1), "past CPU values keep the same vertical positions");
});
