package gpu

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A recovered panic must not orphan a command lease. The goroutine that leased
// the devices defers releasing the set, and only an explicit transfer to a
// command's reaper disarms that safety net. These tests panic on the paths
// between "leases acquired" and "reaper owns them", recover, and then require
// that a revocation still drains.

// recoverPanic runs one poll and requires it to panic, then swallows the
// panic like the collector's sampling supervisor does.
func recoverPanic(t *testing.T, poller *Poller) {
	t.Helper()
	recovered := false
	func() {
		defer func() {
			if recover() != nil {
				recovered = true
			}
		}()
		_ = poller.PollOnce(context.Background())
	}()
	if !recovered {
		t.Fatal("the poll did not panic")
	}
}

// gatedLeasePoller builds a poller gated over the laptop fixture's single
// device, and returns the shared arbiter so the test can inspect its leases.
func gatedLeasePoller(t *testing.T, read TextReadFunc, runner Runner, monotonic func() time.Duration) (*Poller, *AccessArbiter) {
	t.Helper()
	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: read})
	arbiter := gate.Arbiter()
	arbiter.SetDrainTimeout(50 * time.Millisecond)
	poller := NewPoller(PollerOptions{
		Interval:  time.Second,
		Runner:    runner,
		Monotonic: monotonic,
		Power: &PowerSource{
			Gate:    gate,
			Devices: func() []string { return []string{laptopAddress} },
		},
	})
	return poller, arbiter
}

// requireRevocationDrains proves no lease survived the recovered panic.
func requireRevocationDrains(t *testing.T, arbiter *AccessArbiter) {
	t.Helper()
	if result := arbiter.Revoke(laptopAddress); !result.Drained || result.Outstanding != 0 {
		t.Fatalf("revocation = %+v, want a drained device (a lease leaked)", result)
	}
	if status := arbiter.Status(laptopAddress); status.Outstanding != 0 {
		t.Fatalf("status = %+v, want no outstanding lease", status)
	}
}

func TestRecoveredPanicStillDrainsLeases(t *testing.T) {
	t.Run("power re-check", func(t *testing.T) {
		sysfs := runtimePMSysfs("auto\n", "active\n")
		var mu sync.Mutex
		statusReads := 0
		read := func(name string) (string, bool) {
			if name == laptopStatusPath {
				mu.Lock()
				statusReads++
				reads := statusReads
				mu.Unlock()
				// The first read is the gate decision; the second is the
				// pre-access Recheck under the lease.
				if reads >= 2 {
					panic("simulated power re-check panic")
				}
			}
			return sysfs.ReadText(name)
		}
		runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
			t.Fatal("a command ran after the power re-check panicked")
			return "", "", nil
		}}
		poller, arbiter := gatedLeasePoller(t, read, runner, nil)
		recoverPanic(t, poller)
		requireRevocationDrains(t, arbiter)
	})

	t.Run("clock", func(t *testing.T) {
		sysfs := runtimePMSysfs("auto\n", "active\n")
		runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
			t.Fatal("a command ran after the clock panicked")
			return "", "", nil
		}}
		poller, arbiter := gatedLeasePoller(t, sysfs.ReadText, runner, func() time.Duration {
			panic("simulated clock panic")
		})
		recoverPanic(t, poller)
		requireRevocationDrains(t, arbiter)
	})

	t.Run("synchronous runner", func(t *testing.T) {
		sysfs := runtimePMSysfs("auto\n", "active\n")
		runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
			panic("simulated runner panic")
		}}
		poller, arbiter := gatedLeasePoller(t, sysfs.ReadText, runner, nil)
		recoverPanic(t, poller)
		requireRevocationDrains(t, arbiter)
	})
}
