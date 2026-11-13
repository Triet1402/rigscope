// Package enroll exchanges a one-time enrollment grant for a collector
// credential.
//
// The client speaks the hub's HTTPS JSON enrollment endpoint
// (/api/collector/enroll), writes the recovery proof to disk *before* the
// first request, and only deletes it after the issued credential is durably
// saved. A lost or interrupted response is therefore recoverable: retrying
// with the same grant, request id and recovery secret returns the same
// credential instead of burning the grant.
//
// Transport rules match the collector connection: wss:// (HTTPS) everywhere,
// plaintext only for literal loopback addresses, system certificate trust by
// default with an optional pinned CA file.
//
// The package does not own collector runtime state, connection sessions or
// telemetry; it produces one state.Credential.
package enroll
