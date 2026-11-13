// Package alerts owns alert-rule evaluation, incident lifecycle and webhook
// notification delivery for the hub.
//
// The engine is driven by two inputs:
//
//   - Observe, for newly committed LIVE metrics records only. A replay or
//     historical record never changes rule state, so a replay can never reopen
//     an old condition. ObserveAt takes the hub's exact receipt time; Observe
//     uses the engine clock. The hub's ingestion package adapts its live commit
//     to this call.
//   - Tick, for machine liveness. It evaluates stale rules from the machine's
//     last receipt time and unavailable rules from the last live payload. The
//     hub's live-state package adapts its machine snapshot to this call.
//
// Rule state, machine progress, observed resources, incidents and delivery
// jobs are durable. A hub restart therefore continues a pending duration,
// keeps an open incident, and never creates a second job for the same
// (incident, event) pair. Delivery is at-least-once: one durable job per event
// with a deterministic idempotency key, bounded attempts and persisted retry
// state.
//
// Webhook endpoints are private state. The engine never logs or returns the
// configured URL, headers or HMAC secret; HTTP responses use the secret-free
// AlertWebhookStatus from internal/api.
//
// This package deliberately issues the SQL for the alert tables through
// storage.Database.Write: one observation must commit its progress row, rule
// transitions, incident and delivery job atomically. The storage package's
// alert repositories cover plain rule/incident/webhook CRUD, whose per-call
// transactions cannot compose into that evaluation cycle.
//
// The HTTP surface is NewHandler, mounted by the composition root at
// /api/alerts; it dispatches the exact list, rules and webhook paths and never
// exposes webhook private state.
//
// It does not own the HTTP server, session cookies, origin checks, collector
// ingestion, live machine state, history storage or the React UI.
package alerts
