// Package state owns the collector's private durable files.
//
// It stores three unrelated kinds of low-frequency data under
// <stateRoot>/collector/:
//
//   - identity.json: the persistent collector installation id. It is created
//     once and survives restarts. A file that was copied from another machine
//     (the recorded machine fingerprint no longer matches) is regenerated, so a
//     cloned installation never impersonates the original.
//   - enrollment.json: a transient enrollment proof (hub URL, one-time grant
//     and the recovery proof). It is written before the enrollment request and
//     deleted only after the issued credential is durably saved, so a lost
//     enrollment response can be retried safely.
//   - credentials.json: the issued per-hub credential(s).
//
// Every file is a private regular file (0600) inside a private directory
// (0700) and every write is atomic (temp file, fsync, rename, directory
// fsync). Writes happen only on enrollment, identity creation and clone
// recovery, never on the sampling path.
//
// # Enrollment file schema
//
// enrollment.json is a single JSON object:
//
//	{
//	  "enrollments": [
//	    {
//	      "url": "wss://hub.example/api/collector/connect",
//	      "grant": "<one-time enrollment grant>",
//	      "requestId": "<UUID>",
//	      "recoverySecret": "<32-byte base64url>"
//	    }
//	  ]
//	}
//
// "url" is the collector WebSocket endpoint and is unique. The array is
// bounded by MaxCredentials. Unknown fields are rejected. The collector owns
// this format: the installer calls WriteInitialGrant with the grant it
// received, and the collector generates requestId and recoverySecret so a
// retried request recovers the same credential instead of burning the grant.
//
// For compatibility, a flat legacy document written by an early installer is
// accepted on read and upgraded atomically before any enrollment request:
//
//	{"version":1,"url":"wss://hub.example/api/collector/connect","grant":"..."}
//
// The package does not perform enrollment, build protocol messages or own
// per-run identities; those belong to internal/collector/enroll and
// internal/collector/delivery.
package state
