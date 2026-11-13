package metrics

import (
	"errors"
	"testing"

	"github.com/Triet1402/rigscope/internal/protocol"
)

func TestSequenceStartsAtOneAndIsMonotonic(t *testing.T) {
	sequence := NewSequence(0)
	if sequence.Current() != 0 {
		t.Fatalf("current before allocation = %d, want 0", sequence.Current())
	}
	first, err := sequence.Next()
	if err != nil || first != 1 {
		t.Fatalf("first = %d/%v, want 1/nil", first, err)
	}
	second, err := sequence.Next()
	if err != nil || second != 2 {
		t.Fatalf("second = %d/%v, want 2/nil", second, err)
	}
	if sequence.Current() != 2 {
		t.Fatalf("current = %d, want 2", sequence.Current())
	}
}

func TestSequenceHonoursStartAndRefusesToExceedTheWireBound(t *testing.T) {
	sequence := NewSequence(41)
	next, err := sequence.Next()
	if err != nil || next != 42 {
		t.Fatalf("next = %d/%v, want 42/nil", next, err)
	}

	exhausted := NewSequence(protocol.MaxSafeInteger)
	next, err = exhausted.Next()
	if !errors.Is(err, ErrSequenceExhausted) || next != 0 {
		t.Fatalf("exhausted = %d/%v, want 0/ErrSequenceExhausted", next, err)
	}

	// Invalid starts fall back to zero rather than producing a negative or
	// unsafe sequence number.
	negative := NewSequence(-5)
	if next, _ := negative.Next(); next != 1 {
		t.Fatalf("negative start produced %d, want 1", next)
	}
}
