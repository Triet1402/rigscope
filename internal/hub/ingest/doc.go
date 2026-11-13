// Package ingest owns the hub side of the collector wire protocol.
//
// It owns:
//
//   - POST /api/collector/enroll: a one-time grant becomes a long-lived
//     collector credential, with lost-enrollment-response recovery delegated to
//     the storage registry;
//   - GET /api/collector/connect: the authenticated collector WebSocket,
//     including hello/welcome protocol negotiation, bounds, heartbeats,
//     acks that follow the storage commit, live publication that does not wait
//     for that commit, inventory/status acceptance and revocation;
//   - restart recovery that binds persisted latest readings and inventory back
//     into the live state without implying a connected collector.
//
// It does not own:
//
//   - authentication policy for browsers (the live package's Authenticator);
//   - storage. The registry and telemetry repositories are injected; all
//     durable semantics live there;
//   - process-detail leases. The gateway forwards detail.results to the caller,
//     which is the live package's process interests;
//   - browser JSON APIs.
package ingest
