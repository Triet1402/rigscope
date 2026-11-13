// Package policy owns the collector's local sharing policy.
//
// One collector talks to one hub in 1.0, so there is one policy and no
// per-destination dimension. The policy decides which metric families, network
// addresses, process visibility and process arguments leave the machine. It is
// applied to metrics, inventory and process detail before enqueueing; the
// delivery queue also calls the sanitize helpers when a narrowing policy must
// scrub records that are already queued.
//
// It does not own configuration parsing or transport. Wave 2's config package
// decodes Input and passes it to Normalize.
package policy
