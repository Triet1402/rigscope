// Package release fetches, verifies and stages a Rigscope release binary.
//
// It owns GitHub release resolution, HTTPS-only downloads (including every
// redirect), checksum verification, bounded and traversal-safe tar extraction,
// and staging the verified binary under <stateRoot>/staging/<version>/rigscope
// before executing it once to confirm the version it reports.
//
// It does not install the binary, talk to systemd, move data or decide whether
// an upgrade may proceed; internal/activation owns those transactions.
package release
