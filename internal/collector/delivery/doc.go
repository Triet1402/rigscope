// Package delivery owns one collector's connection to one hub: the bounded RAM
// queue, the WebSocket session, acknowledgements, gap records, reconnect
// policy, inventory/status and detail routing.
//
// It deliberately contains no sampling, process scanning, sharing policy or
// enrollment. Callers enqueue already-filtered telemetry records, hand over an
// inventory snapshot, forward detail subscription requests and send
// request-scoped detail results. Everything sent is encoded with
// protocol.EncodeCollectorMessage and everything received is decoded with
// protocol.DecodeHubMessage.
//
// Delivery is at-least-once within one run: unacknowledged records stay queued
// and are retried (the hub deduplicates by collector/run/sequence). A crash or
// restart loses the RAM queue; that unknown interval is represented by the new
// run id, never fabricated as a gap record.
package delivery
