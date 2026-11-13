// Package processes owns the Linux process table: reading /proc into a
// bounded, contract-shaped snapshot, and scheduling request-scoped process
// detail scans for the hub.
//
// The package has two independent pieces:
//
//   - Scanner reads /proc through an fs.FS, so tests run against synthetic
//     trees without root or a real host. Rates (CPU, I/O) come from deltas of
//     the same process identity (pid + startTicks) and are never fabricated.
//     A scan takes a context so shutdown can stop reading between processes.
//   - Scheduler owns expiring detail subscriptions from the collector's single
//     hub. Overlapping subscriptions share one scan, each receives results at
//     its own interval, and a lease expiry, cancel, policy withdrawal or
//     repeated scan failure ends the subscription with an explicit reason.
//     Leases keep expiring while a scan is in flight, and Stop cancels a scan
//     and waits for it to return.
//
// It does not do delivery or wire encoding beyond the local mirror types in
// types.go: the delivery layer owns sockets, framing and protocol limits.
// It does not read /etc/passwd implicitly; callers pass a UserNames.
// It never spawns subprocesses (no getconf), signals processes, or writes to
// /proc, and it omits command lines unless the caller asks for them.
package processes
