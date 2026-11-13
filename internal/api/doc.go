// Package api defines the Rigscope browser ⇄ hub HTTP and SSE contract.
//
// It owns the request bodies, query shapes, success payloads, error bodies and
// server-sent-event envelopes exchanged between the React UI (and the few
// non-browser clients that use the same endpoints) and the hub. Every type
// here was checked against a response captured from the 0.2 Node hub during
// the port (see testdata/api); the Go hub now implements the contract.
//
// Decoding is strict. Unknown fields are rejected at every level, integer
// fields must be integral and inside their documented bounds, enum fields must
// carry a supported value, and request bodies are size-limited the same way
// the hub's HTTP input helpers limit them. A present JSON
// null is preserved for optional fields: protocol.Optional with a nil pointer
// means "absent", a non-nil but invalid Optional means "explicitly null".
//
// Encoding is validated: every Encode* function re-decodes its own output so a
// typed-nil or out-of-range value cannot silently reach the browser.
//
// It does not own HTTP routing, authentication, sessions, storage or the
// collector WebSocket protocol. Those live in internal/hub and
// internal/protocol; this package is only the cross-language data boundary.
//
// TypeScript is generated from these Go declarations:
//
//	go generate ./internal/api
//
// which writes src/contracts/api.generated.ts (types),
// src/contracts/api.validators.generated.ts (strict runtime validators for
// every response and SSE payload) and testdata/api/reencoded (Go-encoded
// fixture copies). Tests fail when any committed output is stale.
//
//go:generate go run ./gen
//go:generate go run ./genfixtures
package api
