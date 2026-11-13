// Package activation coordinates a Rigscope upgrade as a transaction.
//
// It owns the journal, phase ordering, quiescing, backup, running the staged
// migration, publishing the binary, readiness verification, automatic and
// manual rollback, and crash recovery. It does not download releases, know
// systemd's command line, write configuration files or parse the hub API on its
// own: every one of those effects is injected through the interfaces in this
// package (see Options in coordinator.go). A temporary file-lock implementation
// lives here until internal/platform/lock replaces it.
package activation
