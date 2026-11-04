/**
 * Isolated development probe of the remote HTTPS boundary:
 * Next.js HMR upgrades arrive over the private Unix socket with the canonical
 * public Origin, are authenticated, and are terminated when the browser
 * session logs out. The probe runs in a throwaway project and never touches
 * the live dev server, pages, or production build.
 */

import assert from "node:assert/strict";
import {
  cpSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { ROOT } from "../runtime/support/config";
import { within } from "../runtime/support/deadline";
import { baseEnv, FIXTURE_BROWSER_USERS } from "../runtime/support/fixtures";
import { loginRemote, REMOTE_LISTENING_LINE, remoteListenMatch } from "../runtime/support/remote";
import { startChild } from "../runtime/support/process";
import {
  unixPathStats,
  unixRequest,
  wsUpgradeUnix,
} from "../runtime/support/unix";
import { expectClose, wsTerminate } from "../runtime/support/ws";

test("remote development HTTPS boundary preserves authenticated HMR and ends it on logout", async (t) => {
  const project = mkdtempSync(join(tmpdir(), "rigscope-https-hmr-"));
  const runtimeDir = mkdtempSync(join(tmpdir(), "rigscope-https-hmr-run-"));
  const socketPath = join(runtimeDir, "run", "hub.sock");
  const publicOrigin = "https://dev.hub.example.test:8081";
  const hostHeader = new URL(publicOrigin).host;

  for (const name of ["src", "package.json", "tsconfig.json", "next.config.js", "postcss.config.mjs"]) {
    cpSync(join(ROOT, name), join(project, name), { recursive: true });
  }
  symlinkSync(join(ROOT, "node_modules"), join(project, "node_modules"), "dir");

  const child = startChild("hub-remote-dev", [join(ROOT, "dist/hub.cjs")], baseEnv("collector", {
    NODE_ENV: "development",
    HUB_DATABASE_PATH: ':memory:',
    HUB_PUBLIC_ORIGIN: publicOrigin,
    HUB_SOCKET_PATH: socketPath,
    HUB_BROWSER_USERS: JSON.stringify(FIXTURE_BROWSER_USERS),
  }), { cwd: project });
  t.after(async () => {
    await child.stop();
    rmSync(project, { recursive: true, force: true });
    rmSync(runtimeDir, { recursive: true, force: true });
  });

  const line = await child.waitForLine(remoteListenMatch, 90_000, "remote development listening line");
  const match = REMOTE_LISTENING_LINE.exec(line);
  assert.ok(match);
  assert.equal(match![1], socketPath);
  assert.equal(match![2], publicOrigin);

  const login = await loginRemote({ socketPath, publicOrigin, hostHeader });
  const page = await unixRequest(socketPath, {
    path: "/",
    headers: { host: hostHeader, cookie: login.headers.cookie },
    timeoutMs: 60_000,
  });
  assert.equal(page.status, 200, child.dump());
  assert.match(page.headers["content-type"] ?? "", /text\/html/);
  assert.match(page.body, /<!DOCTYPE|<html/i);

  // Cookie-less HMR probes and foreign origins are destroyed before Next.js.
  const anonymous = await wsUpgradeUnix(socketPath, "/_next/hmr", hostHeader, {}, 15_000);
  assert.equal(anonymous.kind, "rejected");
  if (anonymous.kind === "rejected") assert.equal(anonymous.status, 403);
  const foreign = await wsUpgradeUnix(socketPath, "/_next/hmr", hostHeader, {
    cookie: login.headers.cookie,
    origin: "https://evil.example.test",
  }, 15_000);
  assert.equal(foreign.kind, "rejected");
  if (foreign.kind === "rejected") assert.equal(foreign.status, 403);

  const hmr = await wsUpgradeUnix(socketPath, "/_next/hmr", hostHeader, {
    cookie: login.headers.cookie,
    origin: publicOrigin,
  }, 20_000);
  assert.equal(hmr.kind, "open", child.dump());
  if (hmr.kind !== "open") return;
  t.after(() => wsTerminate(hmr.ws));

  let onMessage: ((data: Buffer) => void) | undefined;
  const built = new Promise<void>((resolve, reject) => {
    onMessage = (data) => {
      const message = JSON.parse(data.toString()) as { type?: string; errors?: unknown[] };
      if (message.type !== "built") return;
      if (message.errors?.length) reject(new Error(JSON.stringify(message.errors)));
      else resolve();
    };
    hmr.ws.on("message", onMessage);
  });
  const pagePath = join(project, "src/app/page.tsx");
  const source = readFileSync(pagePath, "utf8");
  const declaration = /export default function (\w+)\(/.exec(source);
  assert.ok(declaration, "page has a named default component for the isolated HMR probe");
  writeFileSync(
    pagePath,
    source.replace("export default function", "function") +
      `\nexport default function RemoteHmrProbe() { return <><${declaration[1]} /><span>Rigscope remote HMR probe</span></>; }\n`,
  );
  try {
    await within(built, 30_000, "remote HMR build after page edit");
  } finally {
    if (onMessage) hmr.ws.off("message", onMessage);
  }
  const updated = await unixRequest(socketPath, {
    path: "/",
    headers: { host: hostHeader, cookie: login.headers.cookie },
    timeoutMs: 20_000,
  });
  assert.match(updated.body, /Rigscope remote HMR probe/);

  // The authenticated HMR socket does not outlive its browser session.
  const hmrClosed = expectClose(hmr.ws, "logout closes remote HMR", 4_000);
  const logout = await unixRequest(socketPath, {
    method: "POST",
    path: "/api/auth/logout",
    headers: {
      host: hostHeader,
      origin: publicOrigin,
      cookie: login.headers.cookie,
      "x-csrf-token": login.csrfToken,
    },
  });
  assert.equal(logout.status, 200);
  await hmrClosed;
  const afterLogout = await wsUpgradeUnix(socketPath, "/_next/hmr", hostHeader, {
    cookie: login.headers.cookie,
    origin: publicOrigin,
  }, 15_000);
  assert.equal(afterLogout.kind, "rejected");
  if (afterLogout.kind === "rejected") assert.equal(afterLogout.status, 403);

  // A fresh authorized HMR socket is also ended by final shutdown.
  const secondLogin = await loginRemote({ socketPath, publicOrigin, hostHeader });
  const hmr2 = await wsUpgradeUnix(socketPath, "/_next/hmr", hostHeader, {
    cookie: secondLogin.headers.cookie,
    origin: publicOrigin,
  }, 20_000);
  assert.equal(hmr2.kind, "open", child.dump());
  if (hmr2.kind !== "open") return;
  t.after(() => wsTerminate(hmr2.ws));

  // A browser session may have multiple tabs, but cannot accumulate an
  // unbounded number of framework upgrades. A different session remains
  // independent, and logout releases capacity for subsequent sign-ins.
  for (let index = 0; index < 7; index += 1) {
    const extra = await wsUpgradeUnix(socketPath, "/_next/hmr", hostHeader, {
      cookie: secondLogin.headers.cookie, origin: publicOrigin,
    }, 20_000);
    assert.equal(extra.kind, "open", child.dump());
    if (extra.kind !== "open") throw new Error("expected authorized HMR socket");
    t.after(() => wsTerminate(extra.ws));
  }
  const overBudget = await wsUpgradeUnix(socketPath, "/_next/hmr", hostHeader, {
    cookie: secondLogin.headers.cookie, origin: publicOrigin,
  }, 20_000);
  assert.equal(overBudget.kind, "rejected");
  if (overBudget.kind === "rejected") assert.equal(overBudget.status, 429);

  const independent = await loginRemote({ socketPath, publicOrigin, hostHeader });
  const otherSession = await wsUpgradeUnix(socketPath, "/_next/hmr", hostHeader, {
    cookie: independent.headers.cookie, origin: publicOrigin,
  }, 20_000);
  assert.equal(otherSession.kind, "open");
  if (otherSession.kind !== "open") throw new Error("second session retains HMR capacity");
  t.after(() => wsTerminate(otherSession.ws));

  const shutdownClosed = expectClose(hmr2.ws, "shutdown closes remote HMR", 8_000);
  assert.deepEqual(await child.stop("SIGTERM", 8_000), { code: 0, signal: null }, child.dump());
  await shutdownClosed;
  assert.equal(unixPathStats(socketPath), null, "clean development shutdown removes its socket");
});
