// Package runtime composes one collector process: it owns the collector
// lease, the installation identity and credential, one sequence allocator, the
// sampling source, the process-detail scheduler and one delivery client bound
// to the single hub configured for this installation.
//
// It owns process lifetime and shutdown: a bounded drain on SIGTERM/SIGINT, a
// "awaiting enrollment" dormant state when no credential exists yet, and the
// consumption of an installer-written enrollment grant. It does not own the
// wire contract (internal/protocol), sampling (sampling/…), delivery
// (collector/delivery), process detail policy (collector/processes) or the
// configuration schema (internal/config). Configuration and state paths come
// from the caller, which is what makes the same composition usable from the
// CLI and from tests.
package runtime
