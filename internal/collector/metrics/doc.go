// Package metrics assembles the wire contract from the collector's samplers.
//
// It owns three things:
//
//   - explicit adapters from the local procfs, GPU and process types into
//     internal/protocol. The mirrored sampler structs are never serialized
//     directly;
//   - the per-tick metrics record and the machine inventory, including the
//     inventory change signature that decides when an inventory message is
//     due;
//   - a Collector that binds the samplers, one shared clock, one sequence
//     allocator and the local sharing policy together.
//
// It does not own delivery, the queue, enrollment or the runtime loop. Wave 2
// composes this Collector with the delivery session.
package metrics
