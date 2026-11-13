package gpu

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runtimePMSysfs builds one NVIDIA display function with runtime-PM
// attributes. boot_vga 0 and no NVIDIA runtime-D3 description leave the
// device runtime-managed by power/control.
func runtimePMSysfs(control, status string) *memSysFS {
	return displayDeviceSysfs(map[string]string{
		laptopControlPath:    control,
		laptopStatusPath:     status,
		laptopPowerStatePath: "D0\n",
		laptopBootVGAPath:    "0\n",
	}, map[string]string{laptopAddress: "0x2204"})
}

func testScopedAccess(arbiter *AccessArbiter, addresses ...string) SampleScope {
	scope := PermittedScope(addresses)
	scope.access = arbiter
	return scope
}

// --- arbiter state machine ---------------------------------------------------

func TestAccessArbiterRevocationDrainsEveryLease(t *testing.T) {
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(10 * time.Millisecond)
	first, err := arbiter.Acquire(context.Background(), "0000:01:00.0")
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	if _, err := arbiter.Acquire(context.Background(), "0000:02:00.0"); err != nil {
		t.Fatalf("acquire second: %v", err)
	}
	second, err := arbiter.Acquire(context.Background(), "0000:01:00.0")
	if err != nil {
		t.Fatalf("acquire second lease: %v", err)
	}

	if result := arbiter.Revoke("0000:01:00.0"); result.Drained {
		t.Fatal("revocation drained while a lease was outstanding")
	}
	first.Release()
	if result := arbiter.Revoke("0000:01:00.0"); result.Drained {
		t.Fatal("revocation drained while the last lease was outstanding")
	}
	second.Release()
	if result := arbiter.Revoke("0000:01:00.0"); !result.Drained {
		t.Fatalf("revocation after the last release = %+v, want drained", result)
	}
	if status := arbiter.Status("0000:01:00.0"); status.State != AccessDenied || status.Outstanding != 0 {
		t.Fatalf("status = %+v, want denied with no leases", status)
	}
}

func TestAccessArbiterDeniesNewLeasesWhileRevoking(t *testing.T) {
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(10 * time.Millisecond)
	lease, err := arbiter.Acquire(context.Background(), laptopAddress)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if result := arbiter.Revoke(laptopAddress); result.Drained {
		t.Fatal("revocation reported drained with an outstanding lease")
	}
	if _, err := arbiter.Acquire(context.Background(), laptopAddress); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("acquire while revoking = %v, want ErrAccessDenied", err)
	}
	lease.Release()
	if _, err := arbiter.Acquire(context.Background(), laptopAddress); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("acquire after denial = %v, want ErrAccessDenied", err)
	}
	arbiter.Permit(laptopAddress)
	if _, err := arbiter.Acquire(context.Background(), laptopAddress); err != nil {
		t.Fatalf("acquire after permit: %v", err)
	}
}

func TestAccessArbiterRevokeWithoutLeasesIsImmediate(t *testing.T) {
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(time.Second)
	started := time.Now()
	if result := arbiter.Revoke(laptopAddress); !result.Drained {
		t.Fatalf("revoke = %+v, want drained", result)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("idle revocation took %s, want immediate", elapsed)
	}
}

// TestPermitDuringRevocationSupersedesIt reproduces the interleaving where a
// Permit re-authorized a device while Revoke was draining it. Revoke used to
// publish its Drained:true snapshot — and the "denied" transition it applied
// afterwards — over the newer transition, which could leave a device marked
// denied with a live lease. The generation makes the Permit win: Revoke
// reports itself superseded and the granted lease stays valid.
func TestPermitDuringRevocationSupersedesIt(t *testing.T) {
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(5 * time.Second)
	lease, err := arbiter.Acquire(context.Background(), laptopAddress)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	revoked := make(chan RevokeResult, 1)
	go func() { revoked <- arbiter.Revoke(laptopAddress) }()
	waitFor(t, 2*time.Second, func() bool {
		return arbiter.Status(laptopAddress).State == AccessRevoking
	})

	// An external consumer re-authorizes the device mid-drain and takes a
	// lease. The waiver must survive the revocation.
	arbiter.Permit(laptopAddress)
	live, err := arbiter.Acquire(context.Background(), laptopAddress)
	if err != nil {
		t.Fatalf("acquire after permit: %v", err)
	}

	result := <-revoked
	if result.Drained || !result.Superseded {
		t.Fatalf("revoke = %+v, want a superseded, non-drained verdict", result)
	}
	status := arbiter.Status(laptopAddress)
	if status.State != AccessPermitted || status.Outstanding < 1 {
		t.Fatalf("status = %+v, want a permitted device with the live lease", status)
	}

	// The lease granted by the winning transition is still usable and still
	// accounted for: deny it only after it is released.
	lease.Release()
	live.Release()
	waitFor(t, 2*time.Second, func() bool {
		return arbiter.Status(laptopAddress).Outstanding == 0
	})
	if result := arbiter.Revoke(laptopAddress); !result.Drained {
		t.Fatalf("revoke after every lease settled = %+v, want drained", result)
	}
}

// --- finding 1: dispatch happens under a lease -------------------------------

// TestHelperLeaseOutlivesCancellationUntilReap reproduces the authorization
// race: the caller gives up on a helper whose child is still unreaped. The
// lease must stay held, so revocation reports busy and no replacement helper
// may start until the child is actually reaped.
func TestHelperLeaseOutlivesCancellationUntilReap(t *testing.T) {
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(20 * time.Millisecond)
	proc := &fakeChildProcess{release: make(chan struct{})}
	var launches int32
	source := HelperSource{
		Access:       arbiter,
		Timeout:      20 * time.Millisecond,
		slot:         &reapSlot{},
		resourceRoot: "/",
		launch: func(io.Writer, io.Writer) (childProcess, error) {
			atomic.AddInt32(&launches, 1)
			return proc, nil
		},
	}
	path := "/sys/bus/pci/devices/" + laptopAddress + "/resource0"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := source.ReadRegisters(ctx, path, []int64{0})
		done <- err
	}()
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&launches) == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled helper read did not return")
	}

	if result := arbiter.Revoke(laptopAddress); result.Drained {
		t.Fatal("revocation drained while the helper child was still unreaped")
	}
	if _, err := source.ReadRegisters(context.Background(), path, []int64{0}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("read while revoked = %v, want ErrAccessDenied", err)
	}
	if got := atomic.LoadInt32(&launches); got != 1 {
		t.Fatalf("launches = %d, want no replacement after revocation", got)
	}

	close(proc.release)
	waitFor(t, 2*time.Second, func() bool {
		return arbiter.Revoke(laptopAddress).Drained
	})
}

// --- finding 2: quiet window only after every lease drains -------------------

func TestGateDoesNotStartQuietWindowBeforeLeasesDrain(t *testing.T) {
	sysfs := runtimePMSysfs("auto\n", "active\n")
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(20 * time.Millisecond)
	gate := NewRuntimePMGate(RuntimePMGateOptions{
		Read:   sysfs.ReadText,
		Access: arbiter,
	})
	for sample := 0; sample < 3; sample++ {
		gate.Record(laptopAddress, true)
	}
	lease, err := arbiter.Acquire(context.Background(), laptopAddress)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	decision := gate.Decide(laptopAddress)
	if decision.Sample || decision.Reason != BusyReason {
		t.Fatalf("decision while a lease was outstanding = %+v, want %q", decision, BusyReason)
	}
	if status := arbiter.Status(laptopAddress); status.State == AccessPermitted || status.Outstanding == 0 {
		t.Fatalf("status while busy = %+v, want a revoked device with an outstanding lease", status)
	}

	lease.Release()
	decision = gate.Decide(laptopAddress)
	if decision.Sample || decision.Reason != PausedReason {
		t.Fatalf("decision after drain = %+v, want the quiet window", decision)
	}
	if status := arbiter.Status(laptopAddress); status.State != AccessDenied || status.Outstanding != 0 {
		t.Fatalf("status while quiet = %+v, want denied and drained", status)
	}
}

// TestQuietWindowWaitsForAnOutstandingSensorBatch is the same guarantee at the
// real dispatch point: the helper blocks until the test releases its child, so
// the quiet window must not start while the batch is in flight.
func TestQuietWindowWaitsForAnOutstandingSensorBatch(t *testing.T) {
	sysfs := runtimePMSysfs("auto\n", "active\n")
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(20 * time.Millisecond)
	started := make(chan struct{})
	var once sync.Once
	proc := &fakeChildProcess{release: make(chan struct{})}
	source := HelperSource{
		Access:       arbiter,
		Timeout:      10 * time.Second,
		slot:         &reapSlot{},
		resourceRoot: "/",
		launch: func(io.Writer, io.Writer) (childProcess, error) {
			once.Do(func() { close(started) })
			return proc, nil
		},
	}
	temps := NewTempsPoller(TempsPollerOptions{Sysfs: sysfs, Source: source, Access: arbiter})
	scope := testScopedAccess(arbiter, laptopAddress)
	done := make(chan struct{})
	go func() {
		_ = temps.PollOnce(context.Background(), scope)
		close(done)
	}()
	<-started

	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText, Access: arbiter})
	for sample := 0; sample < 3; sample++ {
		gate.Record(laptopAddress, true)
	}
	if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != BusyReason {
		t.Fatalf("decision during the sensor batch = %+v, want %q", decision, BusyReason)
	}

	close(proc.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sensor batch did not finish")
	}
	waitFor(t, 2*time.Second, func() bool {
		status := arbiter.Status(laptopAddress)
		return status.State == AccessDenied && status.Outstanding == 0
	})
	if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != PausedReason {
		t.Fatalf("decision after the batch drained = %+v, want the quiet window", decision)
	}
}

// TestNvidiaSMINarrowingCannotStartQuietWindowEarly blocks nvidia-smi while a
// revocation is requested. The command owns the lease, so the gate must report
// busy instead of opening the quiet window underneath it; the next cycle then
// runs no command at all.
func TestNvidiaSMINarrowingCannotStartQuietWindowEarly(t *testing.T) {
	sysfs := runtimePMSysfs("auto\n", "active\n")
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(20 * time.Millisecond)
	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText, Access: arbiter})

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	runner := &fakeRunner{handler: func(ctx context.Context, _ string, args []string, _ time.Duration) (string, string, error) {
		if !isGPUQuery(args) {
			return "", "", nil
		}
		once.Do(func() { close(started) })
		select {
		case <-release:
			return readFixture(t, "nvidia/laptop-4060.csv"), "", nil
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}}
	poller := NewPoller(PollerOptions{
		Interval: time.Second,
		Runner:   runner,
		Power: &PowerSource{
			Gate:    gate,
			Devices: func() []string { return []string{laptopAddress} },
		},
	})

	done := make(chan error, 1)
	go func() { done <- poller.PollOnce(context.Background()) }()
	<-started
	for sample := 0; sample < 3; sample++ {
		gate.Record(laptopAddress, true)
	}
	if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != BusyReason {
		t.Fatalf("decision during nvidia-smi = %+v, want %q", decision, BusyReason)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("poll: %v", err)
	}
	if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != PausedReason {
		t.Fatalf("decision after nvidia-smi = %+v, want the quiet window", decision)
	}

	calls := runner.callCount()
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("quiet cycle: %v", err)
	}
	if got := runner.callCount(); got != calls {
		t.Fatalf("nvidia-smi calls in the quiet window = %d, want %d", got-calls, 0)
	}
	devices := poller.Latest().Devices
	if len(devices) != 1 || devices[0].Reason != PausedReason {
		t.Fatalf("quiet snapshot = %+v, want the gated device", devices)
	}
	for _, arg := range runner.call(0) {
		if arg == "--id=00000000:01:00.0" {
			return
		}
	}
	t.Fatalf("first command = %v, want --id=00000000:01:00.0", runner.call(0))
}

// --- finding 4: runtime-PM uncertainty denies sampling -----------------------

func TestGateDeniesSamplingWhenRuntimePMIsUnknown(t *testing.T) {
	cases := map[string]struct {
		mutate func(*memSysFS)
	}{
		"control missing":   {func(fs *memSysFS) { fs.delete(laptopControlPath) }},
		"control malformed": {func(fs *memSysFS) { fs.set(laptopControlPath, "sometimes\n") }},
		"status missing":    {func(fs *memSysFS) { fs.delete(laptopStatusPath) }},
		"status error":      {func(fs *memSysFS) { fs.set(laptopStatusPath, "error\n") }},
		"status empty":      {func(fs *memSysFS) { fs.set(laptopStatusPath, "\n") }},
		"unmanaged control, unknown status": {
			func(fs *memSysFS) {
				fs.set(laptopControlPath, "on\n")
				fs.delete(laptopStatusPath)
			},
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			sysfs := runtimePMSysfs("auto\n", "active\n")
			testCase.mutate(sysfs)
			gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText})
			decision := gate.Decide(laptopAddress)
			if decision.Sample || decision.Reason != AccessUnknownReason {
				t.Fatalf("decision = %+v, want %q", decision, AccessUnknownReason)
			}
			if status := gate.Arbiter().Status(laptopAddress); status.State != AccessDenied {
				t.Fatalf("arbiter status = %+v, want denied", status)
			}
		})
	}
}

func TestGateKnownUnmanagedStillRequiresAReadableStatus(t *testing.T) {
	sysfs := runtimePMSysfs("on\n", "unsupported\n")
	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText})
	if decision := gate.Decide(laptopAddress); !decision.Sample {
		t.Fatalf("decision = %+v, want sampling for a known-unmanaged, unsupported device", decision)
	}
}

// --- finding 5: no production fixture activation -----------------------------

func TestProductionHelperNeverReadsAFixtureEnvironment(t *testing.T) {
	for _, name := range []string{"pci_helper.go", "pci_linux.go", "pci_common.go", "pci_other.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		source := string(data)
		if strings.Contains(source, helperRootEnv) {
			t.Fatalf("%s reads the test-only helper root environment variable", name)
		}
		if strings.Contains(source, "os.Getenv") {
			t.Fatalf("%s reads the environment at all; fixture activation must live in _test.go", name)
		}
	}
}

// --- finding 6: a stuck helper is not a permissions failure ------------------

func TestTempsPollerReportsStuckHelperDistinctly(t *testing.T) {
	sysfs := runtimePMSysfs("auto\n", "active\n")
	arbiter := NewAccessArbiter()
	slot := &reapSlot{}
	if !slot.tryAcquire() {
		t.Fatal("could not hold the helper slot")
	}
	defer slot.release()
	source := HelperSource{Access: arbiter, slot: slot, Timeout: 20 * time.Millisecond}

	var events []string
	temps := NewTempsPoller(TempsPollerOptions{
		Sysfs:   sysfs,
		Source:  source,
		Access:  arbiter,
		OnEvent: func(line string) { events = append(events, line) },
	})
	if err := temps.PollOnce(context.Background(), testScopedAccess(arbiter, laptopAddress)); err != nil {
		t.Fatalf("poll: %v", err)
	}

	diagnostics := temps.Diagnostics()
	if !diagnostics.Stuck || diagnostics.Reason != HelperStuckReason {
		t.Fatalf("diagnostics = %+v, want the stuck-helper state", diagnostics)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "helper stuck") {
		t.Fatalf("events = %v, want a stuck-helper diagnostic", events)
	}
	if strings.Contains(joined, "mapping failed") {
		t.Fatalf("events = %v, stuck helper was reported as a permissions failure", events)
	}
	if len(temps.Latest()) != 0 {
		t.Fatalf("latest = %+v, want no readings", temps.Latest())
	}
}

// --- finding 7: denial cannot panic the GDDR7 decoder ------------------------

func TestDeniedGDDR7BatchDoesNotPanicOrRead(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{laptopAddress: "0x2b85"})
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(10 * time.Millisecond)
	lease, err := arbiter.Acquire(context.Background(), laptopAddress)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	arbiter.Revoke(laptopAddress)
	lease.Release()
	arbiter.Revoke(laptopAddress)

	source := newFakeRegisters()
	temps := NewTempsPoller(TempsPollerOptions{Sysfs: sysfs, Source: source, Access: arbiter})
	if err := temps.PollOnce(context.Background(), testScopedAccess(arbiter, laptopAddress)); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if calls := source.callCount(); calls != 0 {
		t.Fatalf("register calls = %d, want none after denial", calls)
	}
	if len(temps.Latest()) != 0 {
		t.Fatalf("latest = %+v, want no readings", temps.Latest())
	}
	diagnostics := temps.Diagnostics()
	if !diagnostics.Denied {
		t.Fatalf("diagnostics = %+v, want the denied state", diagnostics)
	}
}

func TestRecoveredPanicDropsStaleRegisterReadings(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{laptopAddress: "0x2204"})
	source := newFakeRegisters()
	source.set(DefaultDRMDevicesDir+"/"+laptopAddress+"/resource0", 0x2046c, 60<<8)
	temps := NewTempsPoller(TempsPollerOptions{Sysfs: sysfs, Source: source})
	if err := temps.PollOnce(context.Background(), AllDevices()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if len(temps.Latest()) != 1 {
		t.Fatalf("latest = %+v, want the first reading", temps.Latest())
	}

	source.mu.Lock()
	source.panics = true
	source.mu.Unlock()
	if err := temps.PollOnce(context.Background(), AllDevices()); err != nil {
		t.Fatalf("panicking poll: %v", err)
	}
	if latest := temps.Latest(); len(latest) != 0 {
		t.Fatalf("latest after a panic = %+v, want the stale reading dropped", latest)
	}
	if diagnostics := temps.Diagnostics(); !diagnostics.Faulted {
		t.Fatalf("diagnostics = %+v, want the fault state", diagnostics)
	}
}

// --- finding 8: the overflow flag is synchronized ----------------------------

func TestCappedBufferOverflowIsSynchronized(t *testing.T) {
	buffer := &cappedBuffer{limit: 8}
	// Establish the overflow first; the loop below then races the flag read
	// against continuing writes.
	if _, err := buffer.Write([]byte("0123456789")); err != nil {
		t.Fatalf("write: %v", err)
	}
	done := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for {
			select {
			case <-done:
				return
			default:
				_, _ = buffer.Write([]byte("0123456789"))
			}
		}
	}()
	for index := 0; index < 1000; index++ {
		if !buffer.Overflowed() {
			t.Fatalf("overflow = false after an oversized write")
		}
	}
	close(done)
	writers.Wait()
}

// lateWriter writes through the launched child's stdout after the caller's
// deadline, which is exactly the moment the runner inspects the overflow flag.
func lateWriter(stdout io.Writer, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
			_, _ = stdout.Write([]byte(strings.Repeat("x", 4096)))
		}
	}
}

func TestExecRunnerLateOutputAfterDeadlineIsRaceFree(t *testing.T) {
	proc := &fakeChildProcess{release: make(chan struct{})}
	late := make(chan struct{})
	runner := ExecRunner{
		slot: &reapSlot{},
		launch: func(stdout, stderr io.Writer) (childProcess, error) {
			go lateWriter(stdout, late)
			return proc, nil
		},
	}
	_, _, _ = runner.Run(context.Background(), "nvidia-smi", nil, 5*time.Millisecond)
	close(late)
	close(proc.release)
}

func TestHelperLateOutputAfterDeadlineIsRaceFree(t *testing.T) {
	proc := &fakeChildProcess{release: make(chan struct{})}
	late := make(chan struct{})
	source := HelperSource{
		Timeout:      5 * time.Millisecond,
		slot:         &reapSlot{},
		resourceRoot: "/",
		launch: func(stdout, stderr io.Writer) (childProcess, error) {
			go lateWriter(stdout, late)
			return proc, nil
		},
	}
	_, _ = source.ReadRegisters(context.Background(),
		"/sys/bus/pci/devices/"+laptopAddress+"/resource0", []int64{0})
	close(late)
	close(proc.release)
}
