package gpu

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Pre-access runtime-status re-check.
//
// User space cannot atomically prevent the kernel from autosuspending a device
// between the gate's decision and the hardware operation: nvidia-smi, sysfs
// and the register helper offer no non-waking power reference. The best
// available narrowing is to re-read power/runtime_status under the lease
// immediately before the access and refuse it when the kernel no longer
// reports the device usable. These tests cover each access point.

// fakePowerState is a [PowerStateReader] with a fixed answer.
type fakePowerState struct {
	mu     sync.Mutex
	status RuntimePMStatus
}

func newFakePowerState(status RuntimePMStatus) *fakePowerState {
	return &fakePowerState{status: status}
}

func (f *fakePowerState) RuntimeStatus(string) RuntimePMStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakePowerState) set(status RuntimePMStatus) {
	f.mu.Lock()
	f.status = status
	f.mu.Unlock()
}

// readRecorder records every attribute a [SysFS] read touches, so a test can
// prove that a refused access happened before any hardware-touching read.
type readRecorder struct {
	inner SysFS
	mu    sync.Mutex
	paths []string
}

func (r *readRecorder) ReadText(name string) (string, bool) {
	r.mu.Lock()
	r.paths = append(r.paths, name)
	r.mu.Unlock()
	return r.inner.ReadText(name)
}

func (r *readRecorder) ReadDir(name string) ([]string, error) {
	return r.inner.ReadDir(name)
}

func (r *readRecorder) readPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.paths...)
}

// TestPollerRefusesADeviceThatSuspendsAfterTheDecision covers the nvidia-smi
// launch: the gate said "sample", and the kernel suspended the device before
// dispatch. No command may run, and the reading must say why.
func TestPollerRefusesADeviceThatSuspendsAfterTheDecision(t *testing.T) {
	sysfs := runtimePMSysfs("auto\n", "active\n")
	var mu sync.Mutex
	statusReads := 0
	read := func(name string) (string, bool) {
		if name != laptopStatusPath {
			return sysfs.ReadText(name)
		}
		mu.Lock()
		statusReads++
		first := statusReads == 1
		mu.Unlock()
		if first {
			return "active\n", true
		}
		return "suspended\n", true
	}
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		t.Fatal("nvidia-smi ran for a device that suspended after the gate decision")
		return "", "", nil
	}}
	poller := NewPoller(PollerOptions{
		Interval: time.Second,
		Runner:   runner,
		Power: &PowerSource{
			Gate:    NewRuntimePMGate(RuntimePMGateOptions{Read: read}),
			Devices: func() []string { return []string{laptopAddress} },
		},
	})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if calls := runner.callCount(); calls != 0 {
		t.Fatalf("nvidia-smi calls = %d, want 0 for a device that suspended", calls)
	}
	snapshot := poller.Latest()
	if snapshot == nil || len(snapshot.Devices) != 1 {
		t.Fatalf("snapshot = %+v, want the gated device", snapshot)
	}
	expected := UnavailableReading("pci:"+laptopAddress, PoweredDownReason)
	if snapshot.Devices[0] != expected {
		t.Fatalf("device = %+v, want %+v", snapshot.Devices[0], expected)
	}
}

// TestPollerRefusesTheCommandWhenADeviceSuspendsAtTheLastMoment covers the
// check inside the dispatch itself: the device was still active when the cycle
// leased it, and suspended before the launch. The command must not start, and
// the refusal is a power state, not a telemetry failure.
func TestPollerRefusesTheCommandWhenADeviceSuspendsAtTheLastMoment(t *testing.T) {
	sysfs := runtimePMSysfs("auto\n", "active\n")
	var mu sync.Mutex
	statusReads := 0
	read := func(name string) (string, bool) {
		if name != laptopStatusPath {
			return sysfs.ReadText(name)
		}
		mu.Lock()
		statusReads++
		suspended := statusReads > 2
		mu.Unlock()
		if suspended {
			return "suspended\n", true
		}
		return "active\n", true
	}
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		t.Fatal("nvidia-smi ran for a device that suspended at the last moment")
		return "", "", nil
	}}
	poller := NewPoller(PollerOptions{
		Interval: time.Second,
		Runner:   runner,
		Power: &PowerSource{
			Gate:    NewRuntimePMGate(RuntimePMGateOptions{Read: read}),
			Devices: func() []string { return []string{laptopAddress} },
		},
	})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if calls := runner.callCount(); calls != 0 {
		t.Fatalf("nvidia-smi calls = %d, want 0", calls)
	}
	if stats := poller.CostStats(); stats.Runs != 0 {
		t.Fatalf("cost stats = %+v, want no command run", stats)
	}
	snapshot := poller.Latest()
	if snapshot == nil || len(snapshot.Devices) != 1 {
		t.Fatalf("snapshot = %+v, want the refused device", snapshot)
	}
	expected := UnavailableReading("pci:"+laptopAddress, PoweredDownReason)
	if snapshot.Devices[0] != expected {
		t.Fatalf("device = %+v, want %+v", snapshot.Devices[0], expected)
	}
}

// TestHelperRechecksRuntimeStatusBeforeDispatch covers the pci-read helper:
// the lease is granted, then the device is found suspended, so no child is
// launched and the caller learns why.
func TestHelperRechecksRuntimeStatusBeforeDispatch(t *testing.T) {
	cases := map[string]struct {
		status RuntimePMStatus
		reason string
	}{
		"suspended": {RuntimePMSuspended, PoweredDownReason},
		"unknown":   {RuntimePMUnknown, AccessUnknownReason},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			arbiter := NewAccessArbiter()
			arbiter.setPowerStateReader(newFakePowerState(testCase.status))
			var launches int32
			source := HelperSource{
				Access:       arbiter,
				Timeout:      time.Second,
				slot:         &reapSlot{},
				resourceRoot: "/",
				launch: func(io.Writer, io.Writer) (childProcess, error) {
					atomic.AddInt32(&launches, 1)
					return &fakeChildProcess{release: make(chan struct{})}, nil
				},
			}
			path := "/sys/bus/pci/devices/" + laptopAddress + "/resource0"
			_, err := source.ReadRegisters(context.Background(), path, []int64{0})
			if !errors.Is(err, ErrAccessDenied) {
				t.Fatalf("err = %v, want an access refusal", err)
			}
			if reason := accessDeniedReason(err); reason != testCase.reason {
				t.Fatalf("reason = %q, want %q", reason, testCase.reason)
			}
			if got := atomic.LoadInt32(&launches); got != 0 {
				t.Fatalf("helper launches = %d, want none for a device that is not awake", got)
			}
			if status := arbiter.Status(laptopAddress); status.Outstanding != 0 {
				t.Fatalf("status = %+v, want the refusal to release its lease", status)
			}
		})
	}
}

// TestReadSysfsGpuGuardedRechecksRuntimeStatus covers the DRM counter read:
// gpu_busy_percent, mem_info_* and hwmon attributes resume a suspended GPU, so
// no attribute may be read once the re-check refuses.
func TestReadSysfsGpuGuardedRechecksRuntimeStatus(t *testing.T) {
	t.Run("suspended reads nothing", func(t *testing.T) {
		recorder := &readRecorder{inner: loadFixtureTree(t, "sysfs/amd-580")}
		arbiter := NewAccessArbiter()
		arbiter.setPowerStateReader(newFakePowerState(RuntimePMSuspended))
		reading, err := ReadSysfsGpuGuarded(arbiter, recorder, amdAddress)
		if !errors.Is(err, ErrAccessDenied) {
			t.Fatalf("err = %v, want an access refusal", err)
		}
		requireUnavailable(t, reading, PoweredDownReason)
		if paths := recorder.readPaths(); len(paths) != 0 {
			t.Fatalf("attribute reads = %v, want none", paths)
		}
		if status := arbiter.Status(amdAddress); status.Outstanding != 0 {
			t.Fatalf("status = %+v, want the refusal to release its lease", status)
		}
	})

	t.Run("active reads the counters", func(t *testing.T) {
		recorder := &readRecorder{inner: loadFixtureTree(t, "sysfs/amd-580")}
		arbiter := NewAccessArbiter()
		arbiter.setPowerStateReader(newFakePowerState(RuntimePMActive))
		reading, err := ReadSysfsGpuGuarded(arbiter, recorder, amdAddress)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if reading.Status != StatusOK {
			t.Fatalf("status = %q, want ok (%+v)", reading.Status, reading)
		}
		if len(recorder.readPaths()) == 0 {
			t.Fatal("no attribute was read for an awake device")
		}
	})
}

// TestGateBindsItsStatusReaderToTheSharedArbiter keeps the wiring honest: the
// gate is the only source of runtime_status, so the arbiter every access point
// consults must end up with that exact reader.
func TestGateBindsItsStatusReaderToTheSharedArbiter(t *testing.T) {
	sysfs := runtimePMSysfs("auto\n", "active\n")
	arbiter := NewAccessArbiter()
	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText, Access: arbiter})
	lease, err := arbiter.Acquire(context.Background(), laptopAddress)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()
	if err := lease.Recheck(); err != nil {
		t.Fatalf("recheck while active: %v", err)
	}
	sysfs.set(laptopStatusPath, "suspended\n")
	if err := lease.Recheck(); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("recheck while suspended = %v, want a refusal", err)
	}
	if gate.RuntimeStatus(laptopAddress) != RuntimePMSuspended {
		t.Fatal("the gate did not report the suspended status")
	}
}
