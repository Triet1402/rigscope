package metrics

import (
	"fmt"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/sampling/procfs"
)

// capturedAtLayout is the canonical JS toISOString form the protocol accepts
// with millisecond precision.
const capturedAtLayout = "2006-01-02T15:04:05.000Z"

// protocolMinTime and protocolMaxTime bound the wire timestamp window.
var (
	protocolMinTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	protocolMaxTime = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
)

// Clock is the one clock every sampler shares. Monotonic drives counter
// intervals, rate derivation and GPU staleness; Now drives capturedAt. They
// must come from the same origin, which is why callers inject a Clock instead
// of a wall clock and a monotonic function that could drift apart.
type Clock interface {
	// Now returns the wall clock reading.
	Now() time.Time
	// Monotonic returns a non-decreasing duration since an arbitrary origin.
	Monotonic() time.Duration
}

// SystemClock is the production Clock: wall time from time.Now and a
// monotonic reading since the clock was created.
type SystemClock struct {
	origin time.Time
}

// NewSystemClock creates the production clock. Construct one per collector
// process and pass it to every sampler.
func NewSystemClock() SystemClock {
	return SystemClock{origin: time.Now()}
}

// Now implements Clock.
func (c SystemClock) Now() time.Time { return time.Now() }

// Monotonic implements Clock.
func (c SystemClock) Monotonic() time.Duration { return time.Since(c.origin) }

// procfsEpoch anchors monotonic readings in a time.Time the procfs samplers can
// subtract. It is constant, so intervals are pure monotonic differences.
var procfsEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// ProcFSMonotonic adapts a Clock to the procfs sampler's interval clock. It is
// exported so the runtime builds the procfs sampler with the same origin as
// the GPU poller and the assembler.
func ProcFSMonotonic(clock Clock) func() (time.Time, bool) {
	return func() (time.Time, bool) {
		return procfsEpoch.Add(clock.Monotonic()), true
	}
}

// MonotonicDuration adapts a Clock to the GPU poller and runtime-PM gate.
// Both must use the same origin as the Collector; a snapshot taken on a
// different origin cannot be compared with the assembler's staleness check.
func MonotonicDuration(clock Clock) func() time.Duration {
	return clock.Monotonic
}

// NewProcFSSampler builds a procfs sampler whose interval clock is clock. The
// shared clock is forced; a caller-supplied Monotonic is ignored so rates can
// never be derived from a second origin.
func NewProcFSSampler(clock Clock, options procfs.SamplerOptions) *procfs.Sampler {
	options.Monotonic = ProcFSMonotonic(clock)
	return procfs.NewSampler(options)
}

// FormatCapturedAt renders a capture time in the canonical protocol form. It
// returns an error when the wall clock is outside the contract's sanity window
// instead of emitting a timestamp every hub would reject.
func FormatCapturedAt(t time.Time) (string, error) {
	utc := t.UTC()
	if utc.Before(protocolMinTime) || !utc.Before(protocolMaxTime) {
		return "", fmt.Errorf("metrics: capturedAt %s is outside the protocol window", utc.Format(time.RFC3339Nano))
	}
	return utc.Format(capturedAtLayout), nil
}

// compile-time proof that the production clock satisfies Clock.
var _ Clock = SystemClock{}
