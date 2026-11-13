// Package server owns the Rigscope hub's HTTP(S) listener and the security
// boundary applied before every handler.
//
// It is deliberately small in surface area: the composition root (the
// `rigscope hub` command now, storage/auth/API packages later) mounts
// handlers with Mount and MountUpgrade. This package implements only:
//
//   - listener setup from internal/config: plaintext on loopback, or TLS 1.2+
//     with a certificate/key pair, and a refusal to bind a non-loopback
//     address without TLS unless a trusted reverse proxy is configured;
//   - the canonical Host check, exact Origin checks for state-changing
//     requests and WebSocket upgrades, forwarded-header trust limited to
//     trustedProxies, request body limits, timeouts and security headers;
//   - the static React export route table (internal/hub/webui) and the
//     /api/health document;
//   - graceful shutdown that stops accepting, cancels long-lived request
//     contexts and closes hijacked WebSocket connections.
//
// It does not own authentication, sessions, storage, the collector protocol,
// SSE payloads, the browser API or installation decisions. Those are mounted
// by their own packages.
package server
