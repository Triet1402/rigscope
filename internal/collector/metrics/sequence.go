package metrics

import (
	"errors"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// ErrSequenceExhausted is returned when the run has used every sequence
// number the wire contract can carry.
var ErrSequenceExhausted = errors.New("metrics: sequence exhausted")

// Sequencer allocates record sequence numbers. One allocator is shared by
// metrics records, gap records and every other record kind of one collector
// run, so implementations must be safe to call from the delivery loop's
// record-producing goroutine.
type Sequencer interface {
	// Next allocates and returns the next sequence number.
	Next() (int64, error)
}

// Sequence is the monotonic sequence allocator for one collector run. The
// zero value is not usable; call NewSequence. It is not safe for concurrent
// use, matching the single sampling loop that owns it.
type Sequence struct {
	value int64
}

// NewSequence returns an allocator whose first Next call returns start+1.
func NewSequence(start int64) *Sequence {
	if start < 0 || start > protocol.MaxSafeInteger {
		start = 0
	}
	return &Sequence{value: start}
}

// Next implements Sequencer. It never returns a value the contract cannot
// carry: the last usable number is protocol.MaxSafeInteger.
func (s *Sequence) Next() (int64, error) {
	if s.value >= protocol.MaxSafeInteger {
		return 0, ErrSequenceExhausted
	}
	s.value++
	return s.value, nil
}

// Current reports the last allocated value without consuming one.
func (s *Sequence) Current() int64 {
	return s.value
}
