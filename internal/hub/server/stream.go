package server

import (
	"net/http"
	"time"
)

// StreamWriteTimeout is the deadline StreamWriter renews before each write.
// It is exported so live-stream handlers can reason about the interval between
// heartbeats: a heartbeat must arrive well inside it.
const StreamWriteTimeout = defaultResponseWriteTimeout

// StreamWriter renews the connection write deadline before every write and
// flush. The security middleware sets one deadline per request; a long-lived
// SSE handler wraps its ResponseWriter in a StreamWriter so each event (and
// each heartbeat) buys another StreamWriteTimeout. A client that stops reading
// therefore fails the next write instead of pinning the handler forever.
//
// It implements http.ResponseWriter, http.Flusher (through FlushError), and
// Unwrap, so http.NewResponseController keeps working through it.
type StreamWriter struct {
	writer     http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
}

// StreamWriter is what a handler type-asserts to for flushing, so the shape must
// stay http.Flusher-compatible.
var _ http.Flusher = (*StreamWriter)(nil)

// NewStreamWriter wraps w. The returned writer is not safe for concurrent use;
// the owning handler serializes writes, as net/http requires.
func NewStreamWriter(w http.ResponseWriter) *StreamWriter {
	return &StreamWriter{
		writer:     w,
		controller: http.NewResponseController(w),
		timeout:    StreamWriteTimeout,
	}
}

// Header implements http.ResponseWriter.
func (s *StreamWriter) Header() http.Header { return s.writer.Header() }

// WriteHeader implements http.ResponseWriter. The deadline is renewed so the
// header write itself is bounded.
func (s *StreamWriter) WriteHeader(status int) {
	s.extend()
	s.writer.WriteHeader(status)
}

// Write renews the deadline, then writes.
func (s *StreamWriter) Write(data []byte) (int, error) {
	s.extend()
	return s.writer.Write(data)
}

// Flush implements http.Flusher. It renews the deadline and flushes; a flush
// error is deliberately dropped to satisfy the interface. Use FlushError when
// the caller needs to know that a slow client missed an event.
func (s *StreamWriter) Flush() { _ = s.FlushError() }

// FlushError renews the deadline, then flushes. An unsupported flush is
// reported as an error so a caller cannot believe a slow client received an
// event.
func (s *StreamWriter) FlushError() error {
	s.extend()
	return s.controller.Flush()
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (s *StreamWriter) Unwrap() http.ResponseWriter { return s.writer }

func (s *StreamWriter) extend() {
	// A recorder or other non-network writer reports ErrNotSupported; the
	// surrounding response has no deadline to renew.
	_ = s.controller.SetWriteDeadline(time.Now().Add(s.timeout))
}
