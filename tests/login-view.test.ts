import assert from "node:assert/strict";
import test from "node:test";
import { build } from "esbuild";
import { createRequire } from "node:module";

test("a successful sign-in never renders an intermediate account screen", async () => {
  const bundle = await build({
    stdin: {
      contents: `import { renderToStaticMarkup } from 'react-dom/server';
        import { createElement } from 'react';
        import { LoginView } from './src/components/live/login-view';
        module.exports = renderToStaticMarkup(createElement(LoginView));`,
      resolveDir: process.cwd(),
    },
    bundle: true, write: false, platform: "node", format: "cjs", jsx: "automatic",
    packages: "external",
    plugins: [{ name: "login-state", setup(build) {
      build.onResolve({ filter: /live\/live-provider$/ }, () => ({ path: "session", namespace: "fixture" }));
      build.onResolve({ filter: /^next\/navigation$/ }, () => ({ path: "router", namespace: "fixture" }));
      build.onLoad({ filter: /.*/, namespace: "fixture" }, ({ path }) => ({ contents: path === "router"
        ? `export const useRouter = () => ({ replace() {} });`
        : `export const useLive = () => ({session: {kind: 'signed-in', user: {username: 'viewer'}}, login() {}, signOut() {}});` }));
    } }],
  });
  const output = { exports: "" };
  new Function("require", "module", "exports", bundle.outputFiles[0]!.text)(createRequire(import.meta.url), output, output.exports);
  assert.doesNotMatch(output.exports, /You are signed in|Go to the fleet|read-only for now|administrator configures accounts/);
  assert.match(output.exports, /Signing in/);
});
