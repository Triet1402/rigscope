// Package admin owns the browser administration endpoints: the durable user
// directory, the bounded audit log, collector administration and machine
// presentation metadata.
//
// Every route requires a live browser session and re-checks the durable
// administrator flag on each request; a wildcard machine scope is never an
// administrative grant. Every privileged mutation re-checks that same flag
// inside the transaction that writes it, so a demotion or disable that commits
// after the request's first check cannot leave a change behind. Password
// hashing is delegated to internal/hub/auth, durable rows to
// internal/hub/storage. This package does not own sessions, SSE payloads,
// collector enrollment or the HTTP listener.
package admin
