/**
 * Machine selection interface tests (src/live/machine-selection).
 *
 * The module is the one ownership point for route/Fleet/remembered
 * resolution, view fallback and selection navigation+persistence. These
 * tests exercise outcomes and coordination through that interface:
 *
 * - an explicit unknown/unauthorized machine id resolves to "missing" and
 *   never silently selects another machine;
 * - the remembered choice is honored while authorized and degrades to the
 *   view fallback when it is not (Processes preferring a process-capable
 *   machine);
 * - delayed Fleet arrival does not overwrite an explicit machine and does
 *   not lose the remembered choice;
 * - history keeps its query scope empty for a missing machine while
 *   Processes keeps the requested id on the shell;
 * - the URL helpers keep History linkable without rewriting an explicit
 *   parameter.
 *
 * The module runs in the browser, so hook scenarios render through the
 * esbuild bundle pattern used by the other view tests, with fixture
 * modules for next/navigation, useLive and browser storage.
 */

import assert from "node:assert/strict";
import { before, describe, it } from "node:test";
import { build, type Plugin } from "esbuild";
import { createRequire } from "node:module";
import type { Inventory, MachineState } from "../src/contracts";
import type {
  MachineResolution,
  MachineSelection,
  MachineViewKind,
} from "../src/live/machine-selection";

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type TestFixture = {
  session?: unknown;
  machines?: MachineState[];
  search?: string;
  pathname?: string;
  remembered?: string;
  pushes?: string[];
  replacements?: string[];
};

declare global {
  // eslint-disable-next-line no-var
  var __machineSelectionTest: TestFixture | undefined;
}

function fixture(scope: Partial<TestFixture> = {}): void {
  globalThis.__machineSelectionTest = { ...scope };
}

function machine(machineId: string, extra: Partial<MachineState> = {}): MachineState {
  return {
    machineId,
    collectorId: `collector-${machineId}`,
    hostname: `host-${machineId}`,
    connected: true,
    lastReceivedAt: null,
    ...extra,
  };
}

function inventoryWithProcesses(status: "supported" | "unsupported"): Inventory {
  return {
    os: { platform: "linux", release: "1", distro: null, arch: "x86_64" },
    cpu: { model: null, logicalCores: 4 },
    memory: { totalBytes: null, swapTotalBytes: null },
    gpus: [],
    filesystems: [],
    disks: [],
    network: [],
    capabilities: {
      cpu: { status: "supported" },
      memory: { status: "supported" },
      swap: { status: "supported" },
      gpus: { status: "supported" },
      disks: { status: "supported" },
      network: { status: "supported" },
      processes:
        status === "supported"
          ? { status: "supported" }
          : { status: "unsupported", reason: "no permission" },
    },
  };
}

// Fixture modules read their scenario at render time so a single build can
// exercise delayed Fleet arrival by mutating the fixture between renders.
const FIXTURE_MODULES: Record<string, string> = {
  react: `
    let effects = [];
    export const useMemo = fn => fn();
    export const useCallback = fn => fn;
    export const useEffect = fn => effects.push(fn);
    export function flushEffects() {
      const pending = effects;
      effects = [];
      pending.forEach(fn => fn());
    }
  `,
  navigation: `
    const fixture = () => globalThis.__machineSelectionTest ?? {};
    export function useRouter() {
      const navigate = (kind, href) => {
        const f = fixture();
        (f[kind] ??= []).push(href);
        const url = new URL(href, "https://fixture.invalid");
        f.pathname = url.pathname;
        f.search = url.search;
      };
      return { push: href => navigate("pushes", href), replace: href => navigate("replacements", href) };
    }
    export function usePathname() {
      return fixture().pathname ?? "/history";
    }
    export function useSearchParams() {
      return new URLSearchParams(fixture().search ?? "");
    }
  `,
  live: `
    const fixture = () => globalThis.__machineSelectionTest ?? {};
    export function useLive() {
      const f = fixture();
      return {
        session: f.session ?? { kind: "signed-in", user: { username: "viewer", csrfToken: "token" } },
        phase: "live",
        machines: f.machines ?? [],
      };
    }
  `,
  prefs: `
    const fixture = () => globalThis.__machineSelectionTest ?? {};
    export function usePersistentState(key, fallback, parse) {
      const f = fixture();
      const stored = parse(f.remembered) ?? fallback;
      return [stored, (next) => {
        const resolved = typeof next === "function" ? next(stored) : next;
        if (!Object.is(resolved, stored)) f.remembered = resolved;
      }];
    }
  `,
};

const PROBE = `
import { flushEffects } from "react";
import {
  useMachineSelection,
  resolveMachineSelection,
  machineViewHref,
  machineUrlWithId,
  useMachineNavigation,
  useSelectMachine,
} from "./src/live/machine-selection";

function capture(view, routeMachineId) {
  const captured = useMachineSelection(view, routeMachineId);
  flushEffects();
  return captured;
}
module.exports = { capture, navigation: useMachineNavigation, select: useSelectMachine,
  resolveMachineSelection, machineViewHref, machineUrlWithId };
`;

const fixturePlugin: Plugin = {
  name: "machine-selection-fixtures",
  setup(build) {
    build.onResolve({ filter: /^react$/ }, () => ({ path: "react", namespace: "fixture" }));
    build.onResolve({ filter: /^next\/navigation$/ }, () => ({ path: "navigation", namespace: "fixture" }));
    build.onResolve({ filter: /live\/live-provider$/ }, () => ({ path: "live", namespace: "fixture" }));
    build.onResolve({ filter: /lib\/persistent-state$/ }, () => ({ path: "prefs", namespace: "fixture" }));
    build.onLoad({ filter: /.*/, namespace: "fixture" }, ({ path }) => ({ contents: FIXTURE_MODULES[path]! }));
  },
};

type ResolutionInput = {
  view: MachineViewKind;
  requestedMachineId: string | null;
  rememberedMachineId: string;
  machines: readonly MachineState[];
};

type SelectionApi = {
  navigation: (machineId?: string, view?: MachineViewKind) => { machine: MachineState | undefined; selectMachine: (id: string) => void };
  select: () => (id: string, view: MachineViewKind) => void;
  capture: (view: MachineViewKind, routeMachineId?: string) => MachineSelection;
  resolveMachineSelection: (input: ResolutionInput) => MachineResolution;
  machineViewHref: (machineId: string, view: MachineViewKind) => string;
  machineUrlWithId: (pathname: string, searchParams: URLSearchParams, machineId: string) => string | null;
};

// One bundle per test process; the exported capture renders one scenario.
let selectionApi: SelectionApi;
before(async () => {
  const bundle = await build({
    stdin: { contents: PROBE, resolveDir: process.cwd() },
    bundle: true,
    write: false,
    platform: "node",
    format: "cjs",
    packages: "external",
    plugins: [fixturePlugin],
  });
  const output = { exports: null as null | SelectionApi };
  new Function("require", "module", "exports", bundle.outputFiles[0]!.text)(
    createRequire(import.meta.url),
    output,
    output.exports,
  );
  assert.ok(output.exports, "the machine selection bundle must export its interface");
  selectionApi = output.exports;
});

// ---------------------------------------------------------------------------
// Resolution rules (pure interface)
// ---------------------------------------------------------------------------

describe("resolveMachineSelection precedence", () => {
  const fleet = [machine("a"), machine("b")];
  const resolve = (input: ResolutionInput) => selectionApi.resolveMachineSelection(input);

  it("selects an explicit authorized machine over the remembered one", () => {
    assert.deepEqual(
      resolve({ view: "history", requestedMachineId: "a", rememberedMachineId: "b", machines: fleet }),
      { kind: "resolved", machine: fleet[0], via: "requested" },
    );
  });

  it("keeps the missing outcome for an explicit unknown machine", () => {
    // The core rule: an explicit id never silently selects another machine.
    assert.deepEqual(
      resolve({ view: "history", requestedMachineId: "outside", rememberedMachineId: "a", machines: fleet }),
      { kind: "missing", requestedMachineId: "outside" },
    );
  });

  it("keeps the remembered machine while it is authorized", () => {
    assert.deepEqual(
      resolve({ view: "history", requestedMachineId: null, rememberedMachineId: "b", machines: fleet }),
      { kind: "resolved", machine: fleet[1], via: "remembered" },
    );
  });

  it("falls back to the first machine on history/overview when the remembered one is unavailable", () => {
    for (const view of ["history", "overview"] as const) {
      assert.deepEqual(
        resolve({ view, requestedMachineId: null, rememberedMachineId: "gone", machines: fleet }),
        { kind: "resolved", machine: fleet[0], via: "fallback" },
        view,
      );
    }
  });

  it("prefers a process-capable machine for Processes, then the first machine", () => {
    const incapable = machine("a", { inventory: inventoryWithProcesses("unsupported") });
    const capable = machine("b");
    const capableLater = machine("c");
    assert.deepEqual(
      resolve({ view: "processes", requestedMachineId: null, rememberedMachineId: "gone", machines: [incapable, capable, capableLater] }),
      { kind: "resolved", machine: capable, via: "fallback" },
      "first process-capable machine",
    );
    assert.deepEqual(
      resolve({ view: "processes", requestedMachineId: null, rememberedMachineId: "gone", machines: [incapable] }),
      { kind: "resolved", machine: incapable, via: "fallback" },
      "an unusable capability still falls back to the first machine",
    );
  });

  it("treats an empty fleet honestly, with or without an explicit machine", () => {
    assert.deepEqual(
      resolve({ view: "history", requestedMachineId: "a", rememberedMachineId: "a", machines: [] }),
      { kind: "fleet-empty" },
    );
    assert.deepEqual(
      resolve({ view: "processes", requestedMachineId: null, rememberedMachineId: "", machines: [] }),
      { kind: "fleet-empty" },
    );
  });
});

describe("route shapes", () => {
  it("builds machine hrefs per view", () => {
    assert.equal(selectionApi.machineViewHref("a/b", "overview"), "/machines/a%2Fb");
    assert.equal(selectionApi.machineViewHref("a", "history"), "/history?machineId=a");
    assert.equal(selectionApi.machineViewHref("a", "processes"), "/processes?machineId=a");
  });

  it("keeps the URL linkable without rewriting an explicit parameter", () => {
    assert.equal(
      selectionApi.machineUrlWithId("/history", new URLSearchParams("range=1h"), "a"),
      "/history?range=1h&machineId=a",
    );
    assert.equal(
      selectionApi.machineUrlWithId("/history", new URLSearchParams("machineId=unknown"), "a"),
      null,
    );
  });
});

// ---------------------------------------------------------------------------
// View coordination (hook interface, rendered per scenario)
// ---------------------------------------------------------------------------

describe("useMachineSelection coordination", () => {
  const fleet = [machine("a"), machine("b", { inventory: inventoryWithProcesses("supported") })];
  const capture = (view: MachineViewKind, id?: string) => selectionApi.capture(view, id);

  it("history keeps an explicit unknown machine missing", () => {
    fixture({ search: "?machineId=outside", machines: fleet, remembered: "b" });
    const out = capture("history");
    assert.deepEqual(out.resolution, { kind: "missing", requestedMachineId: "outside" });
    assert.equal(out.machine, undefined);
    assert.equal(out.machineId, "");
    assert.equal(out.navMachineId, "", "history keeps no machine scope when the machine is missing");
    assert.equal(out.requestedMachineId, "outside");
  });

  it("the remembered selection follows the viewer into history", () => {
    fixture({ machines: fleet, remembered: "b" });
    const out = capture("history");
    assert.deepEqual(out.resolution, { kind: "resolved", machine: fleet[1], via: "remembered" });
    assert.equal(out.machineId, "b");
    assert.equal(out.navMachineId, "b");
    assert.equal(out.requestedMachineId, null);
  });

  it("an empty ?machineId= acts as no explicit machine and resolves the remembered one", () => {
    fixture({ search: "?machineId=", machines: fleet, remembered: "b" });
    const out = capture("history");
    assert.deepEqual(out.resolution, { kind: "resolved", machine: fleet[1], via: "remembered" });
    assert.equal(out.requestedMachineId, null);
  });

  it("delayed Fleet arrival resolves the remembered machine once it exists", () => {
    fixture({ machines: [], remembered: "b" });
    const waiting = capture("history");
    assert.deepEqual(waiting.resolution, { kind: "fleet-empty" });
    assert.equal(waiting.machineId, "");
    assert.equal(waiting.requestedMachineId, null);

    // The fleet arrives later; the remembered choice is honored from then
    // on and no earlier fallback has overwritten it.
    fixture({ machines: fleet, remembered: "b" });
    const loaded = capture("history");
    assert.deepEqual(loaded.resolution, { kind: "resolved", machine: fleet[1], via: "remembered" });
    assert.equal(loaded.machineId, "b");
  });

  it("does not silently reselect while an explicit machine is still unresolvable", () => {
    fixture({ search: "?machineId=outside", machines: [], remembered: "b" });
    const waiting = capture("history");
    assert.deepEqual(waiting.resolution, { kind: "fleet-empty" });
    assert.equal(waiting.requestedMachineId, "outside");

    // Even after the fleet loads, the explicit unknown machine stays missing.
    fixture({ search: "?machineId=outside", machines: fleet, remembered: "b" });
    const missing = capture("history");
    assert.deepEqual(missing.resolution, { kind: "missing", requestedMachineId: "outside" });
    assert.equal(missing.machineId, "");
  });

  it("a remembered machine that disappears resolves honestly to the view fallback", () => {
    const capable = machine("s", { inventory: inventoryWithProcesses("supported") });
    fixture({ machines: [machine("a")], remembered: "gone" });
    assert.deepEqual(capture("history").resolution, { kind: "resolved", machine: machine("a"), via: "fallback" });

    fixture({ machines: [machine("x", { inventory: inventoryWithProcesses("unsupported") }), capable], remembered: "gone" });
    const processes = capture("processes");
    assert.deepEqual(processes.resolution, { kind: "resolved", machine: capable, via: "fallback" });
    assert.equal(processes.navMachineId, "s");
  });

  it("processes keeps the requested identifier on the shell while the machine is missing", () => {
    fixture({ search: "?machineId=outside", machines: fleet });
    const out = capture("processes");
    assert.deepEqual(out.resolution, { kind: "missing", requestedMachineId: "outside" });
    assert.equal(out.machine, undefined);
    assert.equal(out.navMachineId, "outside");
    assert.equal(out.requestedMachineId, "outside");
  });

  it("resolves the overview route machine and keeps its path id on the shell", () => {
    fixture({ machines: fleet, remembered: "b" });
    const out = capture("overview", "a");
    assert.deepEqual(out.resolution, { kind: "resolved", machine: fleet[0], via: "requested" });
    assert.equal(out.machineId, "a");
    assert.equal(out.navMachineId, "a");

    fixture({ machines: fleet, remembered: "b" });
    const missing = capture("overview", "outside");
    assert.deepEqual(missing.resolution, { kind: "missing", requestedMachineId: "outside" });
    assert.equal(missing.machine, undefined);
    assert.equal(missing.navMachineId, "outside");
  });

  it("delays nothing on the overview path id before the fleet arrives", () => {
    fixture({ machines: [] });
    const waiting = capture("overview", "a");
    assert.deepEqual(waiting.resolution, { kind: "fleet-empty" });
    assert.equal(waiting.navMachineId, "a", "the shell keeps the route machine");
  });

  it("preserves the remembered choice across missing overview and Processes routes", () => {
    for (const view of ["overview", "processes"] as const) {
      fixture({ machines: fleet, remembered: "b", search: "?machineId=outside" });
      capture(view, view === "overview" ? "outside" : undefined);
      assert.equal(globalThis.__machineSelectionTest!.remembered, "b");
    }
  });

  it("waits for Fleet before synchronizing History, then replaces once with other parameters intact", () => {
    fixture({ pathname: "/history", search: "?range=1h&machineId=", machines: [], remembered: "b" });
    capture("history");
    const state = globalThis.__machineSelectionTest!;
    assert.equal(state.replacements, undefined);
    state.machines = fleet;
    capture("history");
    assert.deepEqual(state.replacements, ["/history?range=1h&machineId=b"]);
    capture("history");
    assert.deepEqual(state.replacements, ["/history?range=1h&machineId=b"]);
  });

  it("follows back/forward explicit routes without replacing the URL", () => {
    fixture({ pathname: "/history", machines: fleet, remembered: "b", search: "?machineId=a" });
    const state = globalThis.__machineSelectionTest!;
    for (const id of ["a", "b", "a"]) {
      state.search = `?machineId=${id}`;
      assert.equal(capture("history").machineId, id);
      assert.equal(state.remembered, id);
      assert.equal(state.replacements, undefined);
    }
  });

  it("shell selection uses the current route and Processes fallback before persistence effects", () => {
    fixture({ pathname: "/history", machines: fleet, remembered: "b", search: "?machineId=a" });
    assert.equal(selectionApi.navigation("a", "history").machine?.machineId, "a");
    globalThis.__machineSelectionTest!.search = "?machineId=outside";
    assert.equal(selectionApi.navigation("outside", "history").machine?.machineId, "b", "missing content retains authorized navigation");
    fixture({ pathname: "/processes", machines: [machine("x", { inventory: inventoryWithProcesses("unsupported") }), fleet[1]!], remembered: "" });
    assert.equal(selectionApi.navigation().machine?.machineId, "b");
    selectionApi.navigation().selectMachine("x");
    assert.equal(globalThis.__machineSelectionTest!.remembered, "x");
    assert.deepEqual(globalThis.__machineSelectionTest!.pushes, ["/processes?machineId=x"]);
    fixture({ pathname: "/machines/a%2Fb", machines: [machine("a/b")], remembered: "b" });
    assert.equal(selectionApi.navigation("a/b", "overview").machine?.machineId, "a/b");
  });
});
