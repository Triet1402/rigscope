// Package live owns the hub's in-memory view of the fleet and everything a
// browser reads from it.
//
// It owns:
//
//   - the latest state per machine: connected collector session, hello
//     hostname and advertised cadence, the last live metrics record with the
//     hub's receivedAt, inventory, delivery status and presentation metadata;
//   - freshness and ordering rules: a live record updates a row only while its
//     authenticated session is current, and only when its sequence is newer
//     than the last accepted one, so a retried or replayed batch cannot publish
//     twice;
//   - the Server-Sent Events feed at GET /api/live: full snapshots, cursor
//     event ids, snapshot/reset first frames, coalesced updates and scope
//     filtering before serialization;
//   - ephemeral process-detail viewer interests and the collector lease that
//     backs them (GET/POST/DELETE /api/processes).
//
// It does not own:
//
//   - authentication or CSRF. A caller implements Authenticator (and passes a
//     CheckMutation hook for mutating routes); the auth package supplies the
//     real implementation at composition time;
//   - durable telemetry. Ingest and history own the storage packages; this
//     package is one hub process run and its state disappears on restart;
//   - the collector transport. The ingest gateway implements DetailSender and
//     forwards detail.results here.
package live
