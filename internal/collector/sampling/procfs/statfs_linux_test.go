//go:build linux

package procfs

import (
	"math"
	"testing"
)

func TestInodePairRequiresBothConversions(t *testing.T) {
	files, ffree, ok := inodePair(1000, 250)
	if !ok || files != 1000 || ffree != 250 {
		t.Fatalf("inodePair = %d/%d/%v, want 1000/250/true", files, ffree, ok)
	}

	// Either overflowing counter makes the pair unavailable; leaving a
	// converted total next to a zeroed free count would fake 100% inode use.
	if _, _, ok := inodePair(math.MaxUint64, 250); ok {
		t.Fatal("inodePair accepted an overflowing total")
	}
	if _, _, ok := inodePair(1000, math.MaxUint64); ok {
		t.Fatal("inodePair accepted an overflowing free count")
	}
	if _, _, ok := inodePair(math.MaxInt64, math.MaxInt64); !ok {
		t.Fatal("inodePair rejected int64-representable counters")
	}
}
