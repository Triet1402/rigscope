// Package auth owns browser authentication for the hub: password hashing,
// login rate limiting, session issuance and lookup, cookie attributes, the
// same-origin gate for mutations, and the session-revocation broadcaster that
// long-lived consumers (SSE, process detail) subscribe to.
//
// The durable user directory and session table live in
// internal/hub/storage; this package treats the password hash as an opaque
// string it created itself. Login verifies the password outside the write
// transaction, then re-checks the account version and inserts the session in
// one transaction, so an account change or revocation that commits first makes
// the attempt fail. It does not own authorization decisions for
// administrative routes (internal/hub/admin does), collector enrollment,
// SSE payloads or the HTTP listener.
package auth
