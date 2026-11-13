// Package webui embeds and serves the static React UI export produced by
// `next build` (`output: "export"`).
//
// The export is copied from the repository root's `out/` directory into
// `dist/` before a release build. The package always compiles because a
// `dist/.keep` placeholder is committed; a server started without a real
// export refuses to serve the UI rather than answering with a misleading 404.
//
// Build order for an embedded UI:
//
//	npm run build:ui
//	go generate ./internal/hub/webui
//	go build ./cmd/rigscope
//
// `go generate` runs the small copier in `./cmd/webui-copy`, which replaces
// the contents of `dist/` (except `.keep`) with `out/` and writes the
// per-document inline-script hashes (`.csp.json`) used for the
// Content-Security-Policy. `dist/` is ignored by git so a built export is
// never committed. This package owns the route table, cache policy,
// compression negotiation and the document CSP; it does not authenticate
// requests, apply the hub's other security headers, or know about the hub's
// API routes.
package webui
