# Development

Use Node.js 26+ and Linux for real collection. Keep private configuration outside Git, with directories 0700 and files 0600. Public fixture credentials are only for isolated tests.

```sh
npm ci
npm run dev
```

Development uses `.next-dev`; production uses `.next`. Configure users and collector credentials privately before starting a hub. With no browser users it fails closed. Do not run two hubs against the same database or build output.

```sh
npm run build
npm start
# After enrollment, in a separate terminal or service:
npm run collector:start
```

`npm run collector:enroll` uses `COLLECTOR_ENROLL_URL` and a private `COLLECTOR_ENROLL_TOKEN`. `npm run collector:mock` emits synthetic readings. Real collection continues without a browser. See the README for configuration and destination sharing policy.

## HTTPS boundary

Local HTTP is literal loopback only. Remote access requires `HUB_PUBLIC_ORIGIN` and `HUB_SOCKET_PATH` together: an exact HTTPS origin and an absolute Unix socket inside a private directory. The TLS reverse proxy must support WebSocket upgrades, unbuffered SSE, the configured Host and long-lived connections. The remote-mode hub has no TCP listener. The CLI does not configure DNS, certificates, Tailscale or a proxy.

Cookies are Secure and HttpOnly remotely; same-origin validation and machine scope apply to queries, streams and mutations. A hub restart invalidates in-memory sessions. Development HMR upgrades must reach Next.js while collector upgrades reach `/api/collector/connect`. Browser sign-in during development reconnects to the authenticated socket.

```sh
curl --fail --silent --unix-socket /path/private/hub.sock \
  -H 'Host: monitor.example.invalid' http://localhost/api/health
npm run validate:https
```

The validation command checks an already configured endpoint. Supply its `RIGSCOPE_VALIDATE_*` settings privately; inspect `scripts/validate-https.cjs` for the exact inputs. It does not deploy or change credentials. Health checks and anonymous rejection alone do not prove an authenticated owner stream advances or reconnects.

## Targeted verification

| Command | Scope / default workers |
| --- | --- |
| `npm test` | Source tests; 4 concurrent files, `RIGSCOPE_TEST_CONCURRENCY` |
| `npm run test:runtime` | Packaged runtime topology/auth/durability; 4 concurrent files, `RIGSCOPE_RUNTIME_TEST_CONCURRENCY` |
| `npm run test:dev` | Development/HMR integration; 1 file at a time, `RIGSCOPE_DEV_TEST_CONCURRENCY` |
| `npm run build:ui` | Production UI and TypeScript; `RIGSCOPE_BUILD_WORKERS`, default `min(4, available CPUs)` |
| `npm run build:runtime` | Hub, collector, CLI and operations bundles |
| `npm run typecheck` | Repository TypeScript check |

Run the narrow checks relevant to the change. Build the UI once before runtime integration tests that require it; avoid duplicate unchanged builds. Tests use isolated state and never authorize touching host services or private installed data. `RIGSCOPE_REAL_HW=1 npx tsx --test tests/sampling-real.test.ts` explicitly opts into host collection. See [releasing](releasing.md) for package validation and evidence boundaries.
