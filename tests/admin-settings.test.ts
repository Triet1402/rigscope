import { strict as assert } from "node:assert";
import { describe, it } from "node:test";
import { AdminApiError, createAdminApi } from "../src/admin/api";
import {
  STORAGE_CHOICES,
  configurePrefix,
  formatInterval,
  storageChangeWarning,
  storageCommand,
} from "../src/admin/settings";

const settings = {
  generatedAt: "2026-11-12T10:00:00.000Z",
  scope: "user",
  storage: {
    mode: "memory",
    diskBytes: 4096,
    diskLimitBytes: null,
    memoryBytes: 1024,
    memoryBudgetBytes: 2048,
    oldestHistoryMs: null,
  },
  network: {
    url: "http://127.0.0.1:8081",
    listen: "127.0.0.1",
    port: 8081,
    origin: null,
    tls: false,
    trustedProxies: [],
    forwardedHeaders: "x-forwarded",
  },
  localCollector: null,
};

function apiReturning(body: unknown, status = 200) {
  const urls: string[] = [];
  const fetchImpl = (async (input: string | URL | Request) => {
    urls.push(String(input));
    return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  return { api: createAdminApi({ fetchImpl }), urls };
}

describe("hub settings handoff", () => {
  it("builds the scope-correct configure command", () => {
    assert.equal(configurePrefix("user"), "rigscope configure");
    assert.equal(configurePrefix("system"), "sudo rigscope configure --system");
    assert.equal(storageCommand("user", "persistent"), "rigscope configure --telemetry-mode persistent");
    assert.equal(storageCommand("system", "memory"), "sudo rigscope configure --system --telemetry-mode memory");
  });

  it("warns about history loss only for a real change", () => {
    assert.equal(storageChangeWarning("memory", "memory"), null);
    assert.match(storageChangeWarning("memory", "persistent") ?? "", /not copied/);
    assert.match(storageChangeWarning("persistent", "memory") ?? "", /lost at every hub restart/);
    assert.deepEqual(
      STORAGE_CHOICES.map((choice) => choice.mode),
      ["memory", "persistent"],
    );
  });

  it("formats sampling intervals", () => {
    assert.equal(formatInterval(500), "500 ms");
    assert.equal(formatInterval(2000), "2 s");
    assert.equal(formatInterval(90_000), "1.5 min");
    assert.equal(formatInterval(0), "—");
  });
});

describe("admin settings client", () => {
  it("reads and strictly validates the summary", async () => {
    const { api, urls } = apiReturning(settings);
    const result = await api.getSettings();
    assert.equal(result.storage.mode, "memory");
    assert.deepEqual(urls, ["/api/admin/settings"]);
  });

  it("rejects an unexpected shape instead of rendering it", async () => {
    const { api } = apiReturning({ ...settings, storage: { ...settings.storage, mode: "disk" } });
    await assert.rejects(api.getSettings(), (error: unknown) => error instanceof AdminApiError && error.status === 502);
    const extra = apiReturning({ ...settings, tlsKey: "/etc/ssl/private/hub.key" });
    await assert.rejects(extra.api.getSettings(), (error: unknown) => error instanceof AdminApiError && error.status === 502);
  });

  it("surfaces authorization failures", async () => {
    const { api } = apiReturning({ error: "administrative access required" }, 403);
    await assert.rejects(api.getSettings(), (error: unknown) => error instanceof AdminApiError && error.status === 403);
  });
});
