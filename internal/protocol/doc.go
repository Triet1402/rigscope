// Package protocol defines the Rigscope collector wire contract, protocol v1.
//
// It owns the message shapes, limits and version constant exchanged between a
// collector and a hub over the collector WebSocket. Decoding is strict: unknown
// fields, missing fields, out-of-range values, non-integer integers and
// unsupported enum values are rejected so contract drift fails fast instead of
// being silently accepted.
//
// It does not own transport (WebSocket framing, authentication, reconnect or
// heartbeat policy), storage, sampling or any browser-facing HTTP/SSE API.
// Those live in internal/collector and internal/hub.
//
// Decode inbound bytes with DecodeCollectorMessage / DecodeHubMessage and
// encode outbound messages with EncodeCollectorMessage / EncodeHubMessage.
// Those encoders re-run the strict decoder over their own output, which rejects
// typed-nil metric families and list members and enforces the wire size limits;
// callers must not rely on json.Marshal directly. Optional wire fields use
// pointers or Optional[T] so an absent reading stays absent and is never
// encoded as a fabricated zero. A present JSON null is preserved: Optional[T]
// with Valid == false.
//
// Timestamps use one strict grammar in Go and TypeScript:
// YYYY-MM-DDTHH:MM:SS(.1-9 fraction digits)?(Z|±HH:MM), inside the 2000..2200
// window and interpreted at millisecond precision. Every protocol string must
// be a well-formed Unicode scalar sequence; lone UTF-16 surrogate escapes are
// rejected rather than silently replaced.
//
//go:generate go run ./gen -src ../../testdata/protocol -dst ../../testdata/protocol/reencoded
package protocol
