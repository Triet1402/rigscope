import assert from "node:assert/strict";
import test from "node:test";
import { CONNECT_PATH, credential, CredentialConfig, demoToken, HUB_BOOT_MS } from "./support/config";
import { within } from "./support/deadline";
import { listenMatch, spawnStartupCandidate, startHub } from "./support/fixtures";
import { expectHealth } from "./support/http";
import { SseClient } from "./support/sse";
import { parseSnapshot, rawCollector } from "./support/wire";
import { bindCollectorSession, wsTerminate, wsUpgrade } from "./support/ws";

test("packaged hub: exact health shape, UI route, Next delegation, and collector endpoint HTTP access", async (t) => {
  const hub = await startHub(t, { collectors: [credential("c-primary", "m-primary", demoToken("primary"))] });

  // Health: exact endpoint contract.
  await expectHealth(hub);

  // HTTP access to the collector endpoint is upgrade-only: 426.
  const collectorHttp = await fetch(hub.collectorHttpUrl, { signal: AbortSignal.timeout(5_000) });
  const collectorText = await collectorHttp.text();
  assert.equal(
    collectorHttp.status,
    426,
    `GET ${CONNECT_PATH} must be 426 (got ${collectorHttp.status}: ${collectorText.slice(0, 80)})`,
  );

  // Packaging: the production Next.js UI responds with HTML.
  const ui = await fetch(hub.baseUrl, { headers: hub.browserHeaders, signal: AbortSignal.timeout(15_000) });
  const uiText = await ui.text();
  assert.equal(ui.status, 200, `GET / must render the packaged UI (got ${ui.status})`);
  assert.match(ui.headers.get("content-type") ?? "", /text\/html/, "UI must be served as HTML");
  assert.match(uiText, /<!DOCTYPE|<html/i, "UI route returns an HTML document");

  // Unknown HTTP requests delegate to Next.js (Next's own 404).
  const missing = await fetch(`${hub.baseUrl}/__no_such_route_integration__`, { headers: hub.browserHeaders, signal: AbortSignal.timeout(10_000) });
  assert.equal(missing.status, 404, `unknown route must 404 via Next delegation (got ${missing.status})`);

  // Health stays consistent after UI traffic.
  await expectHealth(hub);
});

test("collector endpoint auth: bearer header required, browser Origin upgrades rejected, empty-credential hub still serves health/SSE", async (t) => {
  const goodToken = demoToken("auth");
  const cred = credential("c-auth", "m-auth", goodToken);
  const hub = await startHub(t, { collectors: [cred] });

  const noAuth = await wsUpgrade(hub.collectorWsUrl);
  assert.equal(noAuth.kind, "rejected", "upgrade without bearer credentials must be rejected at the handshake");
  if (noAuth.kind === "rejected") {
    assert.ok((noAuth.status ?? 400) >= 400, `handshake rejection must be an HTTP error (status ${noAuth.status})`);
  }

  const wrongAuth = await wsUpgrade(hub.collectorWsUrl, { authorization: "Bearer not-the-token" });
  assert.equal(wrongAuth.kind, "rejected", "upgrade with an unknown token must be rejected");
  if (wrongAuth.kind === "rejected") assert.ok((wrongAuth.status ?? 400) >= 400);

  // Credentials in the URL are not credentials: header-only auth is required.
  const urlCreds = await wsUpgrade(`${hub.collectorWsUrl}?token=${encodeURIComponent(goodToken)}`);
  assert.equal(urlCreds.kind, "rejected", "credentials in the URL must not authenticate an upgrade");

  const originAttempt = await wsUpgrade(hub.collectorWsUrl, {
    authorization: `Bearer ${goodToken}`,
    origin: "http://localhost:3000",
  });
  assert.equal(originAttempt.kind, "rejected", "browser Origin headers must never upgrade through the collector endpoint");

  // The valid token upgrades; hello/welcome completes the binding.
  const conn = await wsUpgrade(hub.collectorWsUrl, { authorization: `Bearer ${goodToken}` });
  assert.equal(conn.kind, "open", "the configured token must upgrade");
  if (conn.kind !== "open") return;
  const authCollector = rawCollector("c-auth", "demo-auth-node");
  const { welcome } = await bindCollectorSession(conn.ws, authCollector, "auth handshake");
  assert.ok(welcome !== undefined, "welcome message received in the handshake");
  if (welcome !== undefined) {
    const hubHealth = await expectHealth(hub);
    assert.equal(welcome.hubRunId, hubHealth.hubRunId, "welcome hubRunId matches health");
    assert.equal(welcome.machineId, cred.machineId, "welcome maps the configured machineId");
  }
  wsTerminate(conn.ws);

  // A hub started without HUB_COLLECTORS keeps UI/health/SSE but rejects every collector.
  const openHub = await startHub(t); // no credentials configured
  const openHealth = await expectHealth(openHub);
  assert.equal(openHealth.status, "ok");
  const openLive = new SseClient(openHub.liveUrl, openHub.browserHeaders);
  const openInitial = await openLive.next();
  assert.equal(openInitial.event, "snapshot", `initial SSE event must be snapshot (got ${openInitial.event})`);
  const openSnapshot = parseSnapshot(openInitial.data, "snapshot on empty-credential hub");
  assert.equal(openSnapshot.hubRunId, openHealth.hubRunId, "snapshot hubRunId matches health");
  assert.equal(openSnapshot.machines.length, 0, "no credentials -> no machines");
  const strayCollector = await wsUpgrade(openHub.collectorWsUrl, { authorization: `Bearer ${demoToken("stray")}` });
  assert.equal(strayCollector.kind, "rejected", "hub without credentials must reject every collector");
  openLive.close();
});

test("hub config validation: duplicate identities/tokens, missing fields, non-loopback host all fail startup nonzero", async (t) => {
  const base = (c: string, m: string, k: string): CredentialConfig => credential(c, m, k);
  const cases: { name: string; env: Record<string, string | undefined> }[] = [
    {
      name: "development session lifetime override in production",
      env: { HUB_DEV_SESSION_TTL_MS: "3000" },
    },
    {
      name: "malformed browser user configuration",
      env: { HUB_BROWSER_USERS: "not-json" },
    },
    {
      name: "plaintext browser password is not a password hash",
      env: { HUB_BROWSER_USERS: JSON.stringify([{ username: "demo", passwordHash: "demo-password-local", machineIds: "*" }]) },
    },
    {
      name: "duplicate token across identities",
      env: { HUB_COLLECTORS: JSON.stringify([base("c-1", "m-1", "demo-token-dup"), base("c-2", "m-2", "demo-token-dup")]) },
    },
    {
      name: "duplicate collectorId",
      env: { HUB_COLLECTORS: JSON.stringify([base("c-1", "m-1", "t-1"), base("c-1", "m-2", "t-2")]) },
    },
    {
      name: "duplicate machineId",
      env: { HUB_COLLECTORS: JSON.stringify([base("c-1", "m-1", "t-1"), base("c-2", "m-1", "t-2")]) },
    },
    {
      name: "credential missing token field",
      env: { HUB_COLLECTORS: JSON.stringify([{ collectorId: "c-1", machineId: "m-1" }]) },
    },
    {
      name: "non-loopback HUB_HOST",
      env: { HUB_HOST: "0.0.0.0", HUB_COLLECTORS: JSON.stringify([base("c-nonloop", "m-nonloop", "t-nonloop")]) },
    },
  ];

  for (const { name, env } of cases) {
    const child = spawnStartupCandidate(t, env);
    const info = await within(child.exit, 5000, `${name}: invalid configuration startup`);
    assert.ok(
      info.code !== null || info.signal !== null,
      `${name}: hub must terminate (got ${JSON.stringify(info)})`,
    );
    assert.notEqual(info.code, 0, `${name}: startup with invalid config must exit nonzero (got code ${info.code}, signal ${info.signal})`);
    const printed = [...child.stdoutLines(), ...child.stderrLines()].some(listenMatch);
    assert.equal(printed, false, `${name}: no listening line may be printed on invalid config (the hub must not bind)`);
    await child.stop("SIGKILL", 500); // cleanup even on failure paths
  }

  // More than fifty configured machines still boots; the cap is a byte/work
  // budget, not a product machine count.
  const many = spawnStartupCandidate(t, {
    HUB_COLLECTORS: JSON.stringify(Array.from({ length: 75 }, (_, i) => base(`c-many-${i}`, `m-many-${i}`, `t-many-${i}`))),
  });
  await many.waitForLine(listenMatch, HUB_BOOT_MS, "listening line with more than 50 configured machines");
  await many.stop();
});
