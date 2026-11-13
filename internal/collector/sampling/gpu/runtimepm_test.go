package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const laptopAddress = "0000:01:00.0"

const (
	laptopControlPath    = "/sys/bus/pci/devices/" + laptopAddress + "/power/control"
	laptopStatusPath     = "/sys/bus/pci/devices/" + laptopAddress + "/power/runtime_status"
	laptopPowerStatePath = "/sys/bus/pci/devices/" + laptopAddress + "/power_state"
	laptopBootVGAPath    = "/sys/bus/pci/devices/" + laptopAddress + "/boot_vga"
	laptopNvidiaPath     = "/proc/driver/nvidia/gpus/" + laptopAddress + "/power"
)

func TestReadRuntimePMMapsKernelStatus(t *testing.T) {
	sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
	read := sysfs.ReadText
	if got := ReadRuntimePM(read, laptopAddress, DefaultPCIRoot); got != RuntimePMActive {
		t.Fatalf("status = %q, want active", got)
	}
	sysfs.set(laptopStatusPath, "suspended\n")
	if got := ReadRuntimePM(read, laptopAddress, DefaultPCIRoot); got != RuntimePMSuspended {
		t.Fatalf("status = %q, want suspended", got)
	}
	sysfs.set(laptopStatusPath, "suspending\n")
	if got := ReadRuntimePM(read, laptopAddress, DefaultPCIRoot); got != RuntimePMSuspended {
		t.Fatalf("status = %q, want suspended for suspending", got)
	}
	sysfs.set(laptopStatusPath, "error\n")
	if got := ReadRuntimePM(read, laptopAddress, DefaultPCIRoot); got != RuntimePMUnknown {
		t.Fatalf("status = %q, want unknown for error", got)
	}
	if got := ReadRuntimePM(read, "0000:02:00.0", DefaultPCIRoot); got != RuntimePMUnknown {
		t.Fatalf("missing device status = %q, want unknown", got)
	}
}

// TestReadRuntimePMUnsupportedRequiresCachedD0 covers the kernel's
// misleading "unsupported" status: it is also reported when runtime PM is
// disabled, which masks a suspended device. Only the cached power_state
// attribute can prove the device is awake, and anything short of D0 must fail
// closed.
func TestReadRuntimePMUnsupportedRequiresCachedD0(t *testing.T) {
	cases := map[string]struct {
		powerState string
		present    bool
		want       RuntimePMStatus
	}{
		"D0":         {powerState: "D0\n", present: true, want: RuntimePMUnmanaged},
		"D1":         {powerState: "D1\n", present: true, want: RuntimePMUnknown},
		"D2":         {powerState: "D2\n", present: true, want: RuntimePMUnknown},
		"D3hot":      {powerState: "D3hot\n", present: true, want: RuntimePMSuspended},
		"D3cold":     {powerState: "D3cold\n", present: true, want: RuntimePMSuspended},
		"unknown":    {powerState: "unknown\n", present: true, want: RuntimePMUnknown},
		"malformed":  {powerState: "D0x\n", present: true, want: RuntimePMUnknown},
		"missing":    {present: false, want: RuntimePMUnknown},
		"unreadable": {present: false, want: RuntimePMUnknown},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
			sysfs.set(laptopStatusPath, "unsupported\n")
			sysfs.delete(laptopPowerStatePath)
			read := sysfs.ReadText
			if testCase.present {
				sysfs.set(laptopPowerStatePath, testCase.powerState)
			} else if name == "unreadable" {
				read = func(path string) (string, bool) {
					if path == laptopPowerStatePath {
						return "", false
					}
					return sysfs.ReadText(path)
				}
			}
			if got := ReadRuntimePM(read, laptopAddress, DefaultPCIRoot); got != testCase.want {
				t.Fatalf("status = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestGateUnsupportedStatusNeedsCachedD0(t *testing.T) {
	cases := map[string]struct {
		powerState string
		sample     bool
		reason     string
	}{
		"D0":      {powerState: "D0\n", sample: true},
		"D1":      {powerState: "D1\n", reason: AccessUnknownReason},
		"D2":      {powerState: "D2\n", reason: AccessUnknownReason},
		"D3hot":   {powerState: "D3hot\n", reason: PoweredDownReason},
		"D3cold":  {powerState: "D3cold\n", reason: PoweredDownReason},
		"unknown": {powerState: "unknown\n", reason: AccessUnknownReason},
		"missing": {reason: AccessUnknownReason},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
			sysfs.set(laptopStatusPath, "unsupported\n")
			sysfs.delete(laptopPowerStatePath)
			if testCase.powerState != "" {
				sysfs.set(laptopPowerStatePath, testCase.powerState)
			}
			gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText})
			decision := gate.Decide(laptopAddress)
			if decision.Sample != testCase.sample || decision.Reason != testCase.reason {
				t.Fatalf("decision = %+v, want sample=%t reason=%q", decision, testCase.sample, testCase.reason)
			}

			// The pre-access re-check must reach the same verdict from the
			// same cached attribute.
			check := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText})
			arbiter := check.Arbiter()
			lease, err := arbiter.Acquire(context.Background(), laptopAddress)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			defer lease.Release()
			err = lease.Recheck()
			if testCase.sample {
				if err != nil {
					t.Fatalf("recheck for D0 = %v, want access", err)
				}
				return
			}
			if !errors.Is(err, ErrAccessDenied) {
				t.Fatalf("recheck = %v, want an access refusal", err)
			}
			if got := accessDeniedReason(err); got != testCase.reason {
				t.Fatalf("recheck reason = %q, want %q", got, testCase.reason)
			}
		})
	}
}

// TestGateStaticExemptionsDoNotBypassASuspendedPowerState keeps boot-VGA,
// control=on and RTD3-disabled exemptions from overriding a cached D3*
// reading: the kernel's "unsupported" status can hide a device that was
// disabled while suspended.
func TestGateStaticExemptionsDoNotBypassASuspendedPowerState(t *testing.T) {
	cases := map[string]struct {
		read func(*memSysFS) TextReadFunc
	}{
		"boot display": {
			read: func(sysfs *memSysFS) TextReadFunc {
				sysfs.set(laptopBootVGAPath, "1\n")
				return sysfs.ReadText
			},
		},
		"control on": {
			read: func(sysfs *memSysFS) TextReadFunc {
				sysfs.set(laptopControlPath, "on\n")
				return sysfs.ReadText
			},
		},
		"runtime D3 disabled": {
			read: func(sysfs *memSysFS) TextReadFunc {
				return func(name string) (string, bool) {
					if strings.HasPrefix(name, "/proc/driver/nvidia") {
						return "Runtime D3 status:          Disabled by default\n", true
					}
					return sysfs.ReadText(name)
				}
			},
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
			sysfs.set(laptopStatusPath, "unsupported\n")
			sysfs.set(laptopPowerStatePath, "D3cold\n")
			gate := NewRuntimePMGate(RuntimePMGateOptions{Read: testCase.read(sysfs)})
			if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != PoweredDownReason {
				t.Fatalf("decision = %+v, want the suspended state before any exemption", decision)
			}
		})
	}
}

func TestGateNeverGatesDesktopsBootDisplayOrDisabledRTD3(t *testing.T) {
	t.Run("control on", func(t *testing.T) {
		sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
		sysfs.set(laptopControlPath, "on\n")
		sysfs.set(laptopStatusPath, "active\n")
		gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText})
		if decision := gate.Decide(laptopAddress); !decision.Sample {
			t.Fatalf("decision = %+v, want sample", decision)
		}
	})
	t.Run("boot display", func(t *testing.T) {
		sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
		sysfs.set(laptopBootVGAPath, "1\n")
		sysfs.set(laptopStatusPath, "active\n")
		gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText})
		if decision := gate.Decide(laptopAddress); !decision.Sample {
			t.Fatalf("decision = %+v, want sample", decision)
		}
	})
	t.Run("runtime D3 disabled", func(t *testing.T) {
		sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
		read := func(name string) (string, bool) {
			if strings.HasPrefix(name, "/proc/driver/nvidia") {
				return "Runtime D3 status:          Disabled by default\n", true
			}
			return sysfs.ReadText(name)
		}
		gate := NewRuntimePMGate(RuntimePMGateOptions{Read: read})
		if decision := gate.Decide(laptopAddress); !decision.Sample {
			t.Fatalf("decision = %+v, want sample", decision)
		}
	})
	t.Run("suspended beats every exemption", func(t *testing.T) {
		sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
		sysfs.set(laptopControlPath, "on\n")
		sysfs.set(laptopBootVGAPath, "1\n")
		sysfs.set(laptopStatusPath, "suspended\n")
		gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText})
		if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != PoweredDownReason {
			t.Fatalf("decision = %+v, want the suspended state before any exemption", decision)
		}
	})
}

// TestQuietWindowStartsWhenTheDrainCompletes reproduces the timing bug where
// the quiet window was measured from the moment the gate asked for it: a slow
// drain then ate into the window, so the driver got less quiet time than the
// window promises and could be woken before it ever autosuspended.
func TestQuietWindowStartsWhenTheDrainCompletes(t *testing.T) {
	sysfs := runtimePMSysfs("auto\n", "active\n")
	testClock := &clock{}
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(5 * time.Second)
	gate := NewRuntimePMGate(RuntimePMGateOptions{
		Read:   sysfs.ReadText,
		Now:    testClock.Now,
		Access: arbiter,
	})
	for sample := 0; sample < 3; sample++ {
		gate.Record(laptopAddress, true)
	}
	lease, err := arbiter.Acquire(context.Background(), laptopAddress)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	decided := make(chan GateDecision, 1)
	go func() { decided <- gate.Decide(laptopAddress) }()
	waitFor(t, 2*time.Second, func() bool {
		return arbiter.Status(laptopAddress).State == AccessRevoking
	})

	// The drain takes 900 ms, and only then does the quiet window begin.
	testClock.Advance(900 * time.Millisecond)
	lease.Release()
	if decision := <-decided; decision.Sample || decision.Reason != PausedReason {
		t.Fatalf("decision after the drain = %+v, want the quiet window", decision)
	}

	// 20.1 s have passed since the gate asked for the window but only 19.2 s
	// since the drain finished. A window measured from the request has already
	// expired here; measured from the drain it has not.
	testClock.Advance(19_200 * time.Millisecond)
	if decision := gate.Decide(laptopAddress); decision.Sample {
		t.Fatal("quiet window expired before a full window since the drain finished")
	}

	// One full window after the drain, sampling resumes.
	testClock.Advance(900 * time.Millisecond)
	if decision := gate.Decide(laptopAddress); !decision.Sample {
		t.Fatalf("decision after the full window = %+v, want sampling", decision)
	}
}

func TestGateRereadsPowerControl(t *testing.T) {
	sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText})
	sysfs.set(laptopControlPath, "on\n")
	sysfs.set(laptopStatusPath, "active\n")
	if decision := gate.Decide(laptopAddress); !decision.Sample {
		t.Fatalf("decision with control=on = %+v, want sample", decision)
	}
	sysfs.set(laptopControlPath, "auto\n")
	sysfs.set(laptopStatusPath, "suspended\n")
	decision := gate.Decide(laptopAddress)
	if decision.Sample || decision.Reason != PoweredDownReason {
		t.Fatalf("decision after control=auto = %+v, want powered down", decision)
	}
}

func TestGateIdleDeviceGetsQuietWindowThenStaysOff(t *testing.T) {
	sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
	c := &clock{}
	var events []string
	gate := NewRuntimePMGate(RuntimePMGateOptions{
		Read:    sysfs.ReadText,
		Now:     c.Now,
		OnEvent: func(line string) { events = append(events, line) },
	})

	for sample := 0; sample < 3; sample++ {
		if decision := gate.Decide(laptopAddress); !decision.Sample {
			t.Fatalf("idle sample %d = %+v, want sample", sample, decision)
		}
		gate.Record(laptopAddress, true)
		c.Advance(2 * time.Second)
	}
	if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != PausedReason {
		t.Fatalf("decision after idle streak = %+v, want paused", decision)
	}
	// Idempotent within a cycle: inventory and metrics both ask.
	if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != PausedReason {
		t.Fatalf("second decision in the same cycle = %+v, want paused", decision)
	}

	sysfs.set(laptopStatusPath, "suspended\n")
	c.Advance(10 * time.Second)
	if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != PoweredDownReason {
		t.Fatalf("suspended decision = %+v, want powered down", decision)
	}
	c.Advance(10 * time.Minute)
	if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != PoweredDownReason {
		t.Fatalf("long-suspended decision = %+v, want powered down", decision)
	}

	// Another client (a game, prime-run) woke it: sample while it is in use.
	sysfs.set(laptopStatusPath, "active\n")
	if decision := gate.Decide(laptopAddress); !decision.Sample {
		t.Fatalf("woken decision = %+v, want sample", decision)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "powered down; sampling suspended") {
		t.Fatalf("missing power-down diagnostic: %v", events)
	}
	if !strings.Contains(joined, "woke up; sampling resumed") {
		t.Fatalf("missing wake diagnostic: %v", events)
	}
}

func TestGateHeldAwakeIsPausedRarelyWithBackoff(t *testing.T) {
	sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
	c := &clock{}
	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText, Now: c.Now})
	idleSamples := func(count int) {
		for sample := 0; sample < count; sample++ {
			if gate.Decide(laptopAddress).Sample {
				gate.Record(laptopAddress, true)
			}
			c.Advance(2 * time.Second)
		}
	}
	idleSamples(3)
	if gate.Decide(laptopAddress).Sample {
		t.Fatal("expected the first pause after three idle samples")
	}
	c.Advance(20 * time.Second)
	// Window over, still awake: resume, and do not pause again for 5 minutes.
	if !gate.Decide(laptopAddress).Sample {
		t.Fatal("expected sampling to resume after the quiet window")
	}
	idleSamples(100)
	if !gate.Decide(laptopAddress).Sample {
		t.Fatal("expected sampling to stay on inside the backoff")
	}
	c.Advance(300 * time.Second)
	idleSamples(1)
	if gate.Decide(laptopAddress).Sample {
		t.Fatal("expected a second pause after the backoff")
	}
	c.Advance(20 * time.Second)
	if !gate.Decide(laptopAddress).Sample {
		t.Fatal("expected sampling to resume after the second window")
	}
	// Real work resets the backoff.
	gate.Record(laptopAddress, false)
	idleSamples(3)
	if gate.Decide(laptopAddress).Sample {
		t.Fatal("expected a pause again after fresh idle samples")
	}
}
