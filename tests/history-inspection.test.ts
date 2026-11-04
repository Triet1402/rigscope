import assert from "node:assert/strict";
import { createRequire } from "node:module";
import test from "node:test";
import { build, type Plugin } from "esbuild";
import { inspectionPoint, inspectionTimeAtPointer, inspectionTimeForKey, inspectionTimes } from "../src/history/inspection";

type RenderExports = Record<string, any>;
const inspectionGlobals = globalThis as typeof globalThis & {
  __rigscopeInspectTime?: number | null;
  __rigscopeFormatDateTimeCalls?: number;
};

async function compile(contents: string, plugins: Plugin[] = []): Promise<RenderExports> {
  const bundle = await build({
    stdin: { contents, resolveDir: process.cwd() },
    bundle: true, write: false, platform: "node", format: "cjs", jsx: "automatic", packages: "external",
    plugins,
  });
  const output = { exports: {} as RenderExports };
  new Function("require", "module", "exports", bundle.outputFiles[0]!.text)(createRequire(import.meta.url), output, output.exports);
  return output.exports;
}

const point = (t: number, avg: number | null) => ({ t, min: avg, max: avg, avg });

test("inspection selects the closest recorded bucket without interpolating", () => {
  const points = [point(1000, 10), point(2000, 90)];
  assert.equal(inspectionPoint(points, 1400, 1000, []), points[0]);
  assert.equal(inspectionPoint(points, 1600, 1000, []), points[1]);
  assert.equal(inspectionPoint(points, 1500, 1000, []), points[0]);
  assert.equal(inspectionPoint(points, 5000, 1000, []), null);
  assert.equal(inspectionPoint([], 1000, 1000, []), null);
});

test("inspection preserves unavailable readings and does not bridge missing or dropped intervals", () => {
  const points = [point(1000, 10), point(2000, null), point(4000, 90)];
  assert.equal(inspectionPoint(points, 2000, 1000, [])?.avg, null);
  assert.equal(inspectionPoint(points, 3000, 1000, []), null);
  for (const kind of ["missing", "dropped"] as const) {
    const gaps = [{ fromMs: 1000, toMs: 3500, kind }];
    assert.equal(inspectionPoint(points, 3400, 1000, gaps), null);
    assert.equal(inspectionPoint(points, 3600, 2000, [{ fromMs: 3700, toMs: 3800, kind }]), null);
    assert.equal(inspectionPoint(points, 4000, 1000, gaps), points[2]);
  }
});

test("inspection maps pointer coordinates to the plotted time range including SVG padding", () => {
  assert.equal(inspectionTimeAtPointer(500, 100, 800, 1000, 11_000, 6 / 800), 6000);
  assert.equal(inspectionTimeAtPointer(106, 100, 800, 1000, 11_000, 6 / 800), 1000);
  assert.equal(inspectionTimeAtPointer(894, 100, 800, 1000, 11_000, 6 / 800), 11_000);
  assert.equal(inspectionTimeAtPointer(-50, 100, 800, 1000, 11_000), 1000);
  assert.equal(inspectionTimeAtPointer(1200, 100, 800, 1000, 11_000), 11_000);
  assert.equal(inspectionTimeAtPointer(100, 100, 0, 1000, 11_000), 1000);
});

test("keyboard inspection navigates the union of series timestamps and stays at its edges", () => {
  const times = inspectionTimes([{ points: [point(2000, 90), point(4000, 10)] }, { points: [point(1000, null), point(2000, 20)] }]);
  assert.deepEqual(times.map((p) => p.t), [1000, 2000, 4000]);
  assert.equal(inspectionTimeForKey(times, null, "ArrowLeft"), 4000);
  assert.equal(inspectionTimeForKey(times, 4000, "ArrowLeft"), 2000);
  assert.equal(inspectionTimeForKey(times, 2000, "ArrowRight"), 4000);
  assert.equal(inspectionTimeForKey(times, 1000, "ArrowLeft"), 1000);
  assert.equal(inspectionTimeForKey(times, 4000, "ArrowRight"), 4000);
  assert.equal(inspectionTimeForKey(times, 2000, "Home"), 1000);
  assert.equal(inspectionTimeForKey(times, 2000, "End"), 4000);
  assert.equal(inspectionTimeForKey([], null, "End"), null);
});

test("core inspection preserves the peak summary and desktop Peak column, formatting the cursor time once", async () => {
  const { readFile } = await import("node:fs/promises");
  const render = await compile(`
    import { renderToStaticMarkup } from 'react-dom/server';
    import { createElement } from 'react';
    import { CoreHistoryChart } from './src/components/history/core-history-chart';
    module.exports.render = () => renderToStaticMarkup(createElement(CoreHistoryChart, {
      title: 'Per-core usage', fromMs: 0, toMs: 2000, stepMs: 1000, gaps: [],
      hiddenKeys: new Set(), onToggleSeries() {},
      series: Array.from({length: 12}, (_, i) => ({
        key: String(i), label: 'Core ' + i, points: [{t: 1000, min: 20, max: 80, avg: 40}],
      })),
    }));`, [{
      name: "inspection-cursor",
      setup(builder) {
        builder.onResolve({ filter: /^\.\/chart-inspection$/ }, () => ({ path: "inspection", namespace: "cursor-fixture" }));
        builder.onLoad({ filter: /.*/, namespace: "cursor-fixture" }, () => ({ contents: `
          export const useChartInspection = () => ({ id: 'cursor', timeMs: globalThis.__rigscopeInspectTime,
            inspect() {}, clear() {}, onKeyDown() {}, onFocus() {} });` }));
        builder.onLoad({ filter: /history\/presentation\.ts$/ }, async ({ path }) => ({
          contents: (await readFile(path, "utf8")).replace(/(export function formatDateTime\([^)]*\): string \{)/,
            "$1 globalThis.__rigscopeFormatDateTimeCalls++ ;"), loader: "ts",
        }));
      },
    }]);
  try {
    inspectionGlobals.__rigscopeInspectTime = null;
    const idle = render.render() as string;
    inspectionGlobals.__rigscopeInspectTime = 1000;
    inspectionGlobals.__rigscopeFormatDateTimeCalls = 0;
    const inspected = render.render() as string;
    const summary = (html: string) => html.match(/Peak · Core 0.*?<\/span><\/span>/)?.[0];
    assert.ok(summary(idle));
    assert.equal(summary(inspected), summary(idle), "inspection must not replace or resize the peak summary");
    assert.match(inspected, /class="hidden sm:inline">Peak<\/span>/, "the desktop peak column still displays peak values");
    assert.match(inspected, /80%/);
    assert.equal(inspectionGlobals.__rigscopeFormatDateTimeCalls, 1, "share one formatted cursor timestamp across all core labels");
  } finally {
    delete inspectionGlobals.__rigscopeInspectTime;
    delete inspectionGlobals.__rigscopeFormatDateTimeCalls;
  }
});
