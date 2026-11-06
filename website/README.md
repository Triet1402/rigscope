# Rigscope website

The standalone marketing page and browser-only synthetic demonstration for
rigscope.dev. This app has its own build and deployment; it is not part of the
installed hub or npm package.

```sh
cd website
npm ci
npm run typecheck
npm run verify:demo
npm run build
PORT=8087 npm start
```

The server binds to `0.0.0.0` by default; set `HOST` to limit its interface.
Only generated files in `dist/` are served. The output can also be deployed to
any static host. The build prerenders the page for search engines and hydrates
the landing page in the browser. The demo is an isolated document at `/demo.html`.

The demo requires no hub, credentials, accounts, live machine access, or server
API. Its machine names, readings, and process rows are synthetic.

## Keep the demo in sync

The demo directly imports the product's `FleetView`, `MachineView`, `HistoryView`,
`ProcessesView`, `AppShell`, sidebar, charts, tables, and theme. The website build
compiles `src/app/globals.css` and scans the product's sources for Tailwind classes.
Do not copy or recreate these views in the website.

Only integration seams differ: `src/product-demo/` provides browser-local routing,
the live context, and synthetic history/process hooks. Those adapters use the
product's own types. Fixtures use the collector's synthetic sampler and the
product's telemetry-to-history mapping. The iframe isolates product styles and
local fixture responses from the marketing page. It never contacts a real hub.

Rebuild this app whenever product UI changes. Run `npm run verify:demo` to check
fixtures against the actual telemetry, history, and process parsers. Run the
website typecheck and build alongside product checks in CI; exercise Fleet,
Overview, History, and Processes in browser smoke checks when changing their UI.
Alerts, Connections, and Administration keep their navigation positions but lead
to a public-demo explanation rather than editable hub settings.

Update the install command and prerequisites alongside the root README when
installation changes. Documentation, source, and contact links point to the
Rigscope GitHub repository and maintainer profile.

The root TypeScript project excludes this app. Its own TypeScript configuration
resolves React types from its own dependencies, including for the shared mark,
so the two installations do not conflict.

## Hosting on the development machine

`rigscope-website.service` serves this build on port 8087.
`rigscope-website-tunnel.service` connects it to Cloudflare for `rigscope.dev`
and `www.rigscope.dev`; www redirects to the apex domain. Both are persistent
systemd user services, enabled at boot through user lingering.

Service definitions live in `~/.config/systemd/user/`. Cloudflare configuration
and credentials live in `~/.cloudflared/` and must never be committed.

After changing the website or shared product UI:

```sh
npm run build --prefix website
systemctl --user restart rigscope-website.service
```

Check or stop hosting:

```sh
systemctl --user status rigscope-website.service rigscope-website-tunnel.service
journalctl --user -u rigscope-website-tunnel.service -n 30
systemctl --user stop rigscope-website-tunnel.service rigscope-website.service
```
