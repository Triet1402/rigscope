package runtime

import (
	"sync"

	"github.com/Triet1402/rigscope/internal/collector/metrics"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// Sequence is the one record-sequence allocator of a collector run. Every
// record kind of one run — assembled metrics and the gap records delivery
// creates — must come from this allocator, otherwise two records can share a
// (runId, sequence) and corrupt hub deduplication.
//
// Unlike metrics.Sequence it is safe for concurrent use: the sampling loop
// allocates metric sequences while the delivery queue allocates gap sequences
// from its own goroutine.
type Sequence struct {
	mu    sync.Mutex
	value int64
}

// NewSequence returns an allocator whose first Next returns 1.
func NewSequence() *Sequence { return &Sequence{} }

// Next allocates the next sequence number. It never returns a value the wire
// contract cannot carry.
func (s *Sequence) Next() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value >= protocol.MaxSafeInteger {
		return 0, metrics.ErrSequenceExhausted
	}
	s.value++
	return s.value, nil
}

// Allocate implements delivery's AllocateSequence contract, which cannot
// report an error. Exhaustion is unreachable in practice (2^53 records); the
// documented result is 0 so a caller can recognise it in a diagnostic rather
// than seeing a panic.
func (s *Sequence) Allocate() int64 {
	sequence, err := s.Next()
	if err != nil {
		return 0
	}
	return sequence
}

// Current reports the last allocated value without consuming one.
func (s *Sequence) Current() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value
}
