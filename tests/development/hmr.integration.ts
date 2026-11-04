import assert from "node:assert/strict";
import { cpSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { CONNECT_PATH, ROOT } from "../runtime/support/config";
import { within } from "../runtime/support/deadline";
import { baseEnv, FIXTURE_BROWSER_USERS, LISTENING_LINE, listenMatch } from "../runtime/support/fixtures";
import { loginBrowser } from "../runtime/support/browser";
import { startChild } from "../runtime/support/process";
import { wsTerminate, wsUpgrade, wsUpgradeWithDeadline } from "../runtime/support/ws";

test("development hub preserves HMR file updates, collector auth and clean shutdown", async t => {
  // Run in a throwaway project so the probe never touches the user's pages,
  // tsconfig, active dev server or production build artifacts.
  const project = mkdtempSync(join(tmpdir(), "rigscope-hmr-"));
  for (const name of ["src", "package.json", "tsconfig.json", "next.config.js", "postcss.config.mjs"]) {
    cpSync(join(ROOT, name), join(project, name), { recursive: true });
  }
  symlinkSync(join(ROOT, "node_modules"), join(project, "node_modules"), "dir");
  const child = startChild("hub-dev", [join(ROOT, "dist/hub.cjs")], baseEnv("collector", {
    NODE_ENV: "development", HUB_HOST: "127.0.0.1", HUB_PORT: "0",
    HUB_DATABASE_PATH: ':memory:',
    HUB_BROWSER_USERS: JSON.stringify(FIXTURE_BROWSER_USERS),
  }), { cwd: project });
  t.after(async () => {
    await child.stop();
    rmSync(project, { recursive: true, force: true });
  });
  const line = await child.waitForLine(listenMatch, 30000, "development listening line");
  const port = Number(LISTENING_LINE.exec(line)?.[2]);
  const baseUrl = `http://127.0.0.1:${port}`;
  const browser = await loginBrowser(baseUrl);
  const response = await fetch(baseUrl, { headers: browser.headers, signal: AbortSignal.timeout(60000) });
  assert.equal(response.status, 200, child.dump());
  await response.text();
  const unauthorized = await wsUpgrade(`ws://127.0.0.1:${port}${CONNECT_PATH}`);
  assert.equal(unauthorized.kind, "rejected");

  const hmr = await wsUpgradeWithDeadline(`ws://127.0.0.1:${port}/_next/hmr`, { ...browser.headers, Origin: baseUrl }, undefined, 15000);
  assert.equal(hmr.kind, "open", child.dump());
  if (hmr.kind !== "open") return;
  t.after(() => wsTerminate(hmr.ws));
  let onMessage: ((data: Buffer) => void) | undefined;
  const built = new Promise<void>((resolve, reject) => {
    onMessage = data => {
      const message = JSON.parse(data.toString());
      if (message.type !== "built") return;
      if (message.errors?.length) reject(new Error(JSON.stringify(message.errors)));
      else resolve();
    };
    hmr.ws.on("message", onMessage);
  });
  const page = join(project, "src/app/page.tsx");
  const source = readFileSync(page, "utf8");
  const declaration = /export default function (\w+)\(/.exec(source);
  assert.ok(declaration, "page has a named default component for the isolated HMR probe");
  writeFileSync(page, source.replace("export default function", "function") +
    `\nexport default function HmrProbe() { return <><${declaration[1]} /><span>Rigscope HMR probe</span></>; }\n`);
  try {
    await within(built, 20000, "HMR build after page edit");
  } finally {
    if (onMessage) hmr.ws.off("message", onMessage);
  }
  const updated = await fetch(baseUrl, { headers: browser.headers, signal: AbortSignal.timeout(10000) });
  assert.match(await updated.text(), /Rigscope HMR probe/);
  wsTerminate(hmr.ws);
  assert.deepEqual(await child.stop("SIGTERM", 6000), { code: 0, signal: null }, child.dump());
});
