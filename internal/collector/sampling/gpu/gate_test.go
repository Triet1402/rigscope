package gpu

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// staticGate is a [PowerGate] whose decision is fixed. Tests that need the
// real runtime-PM behaviour use [RuntimePMGate] instead.
type staticGate struct {
	sample bool
	reason string
}

func (g *staticGate) Decide(string) GateDecision {
	return GateDecision{Sample: g.sample, Reason: g.reason}
}

func (g *staticGate) Record(string, bool) {}

// toggleGate lets a test flip the runtime-PM decision between cycles.
type toggleGate struct {
	mu     sync.Mutex
	sample bool
}

func (g *toggleGate) Decide(string) GateDecision {
	g.mu.Lock()
	defer g.mu.Unlock()
	return GateDecision{Sample: g.sample}
}

func (g *toggleGate) Record(string, bool) {}

func (g *toggleGate) set(sample bool) {
	g.mu.Lock()
	g.sample = sample
	g.mu.Unlock()
}

// holdRegisters blocks its first batch like a BAR read that has not settled
// yet. Later batches are answered immediately, so a test can tell whether a
// cancelled cycle kept accessing the device.
type holdRegisters struct {
	mu       sync.Mutex
	calls    []registerCall
	firstCtx context.Context
	once     sync.Once
	started  chan struct{}
	release  chan struct{}
}

func newHoldRegisters() *holdRegisters {
	return &holdRegisters{started: make(chan struct{}), release: make(chan struct{})}
}

func (h *holdRegisters) ReadRegisters(ctx context.Context, resourcePath string, offsets []int64) ([]*uint32, error) {
	h.mu.Lock()
	h.calls = append(h.calls, registerCall{path: resourcePath, offsets: append([]int64{}, offsets...)})
	first := len(h.calls) == 1
	if first {
		h.firstCtx = ctx
	}
	h.mu.Unlock()
	if first {
		h.once.Do(func() { close(h.started) })
		<-h.release
	}
	values := make([]*uint32, len(offsets))
	// A GDDR7 status batch advertises one valid DQR entry, so the decoder asks
	// for a second BAR read unless the authorization stopped it.
	if len(offsets) == len(gddr7StatusOffsets())+len(blackwellThermOffsets()) {
		values[0] = pointer(uint32(1 << 24))
	}
	return values, nil
}

func (h *holdRegisters) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-h.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the register read never started")
	}
}

// waitCanceled asserts the poller cancelled the running cycle's context, not
// just its live scope.
func (h *holdRegisters) waitCanceled(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	ctx := h.firstCtx
	h.mu.Unlock()
	if ctx == nil {
		t.Fatal("the register read never received a context")
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the running cycle was not cancelled when the permitted set narrowed")
	}
}

func (h *holdRegisters) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *holdRegisters) callsSnapshot() []registerCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]registerCall{}, h.calls...)
}

// TestGatedPollerFailsClosedWithoutInventory reproduces the fail-open case:
// an empty inventory used to produce an ungated plan, which ran an
// unrestricted nvidia-smi query and let the sensor walk every device.
func TestGatedPollerFailsClosedWithoutInventory(t *testing.T) {
	cases := map[string]struct {
		devices func() []string
		want    int
	}{
		"nil discovery":    {nil, 0},
		"no addresses":     {func() []string { return nil }, 0},
		"empty list value": {func() []string { return []string{} }, 0},
		"unknown address":  {func() []string { return []string{"not-a-bdf"} }, 1},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
				t.Fatal("nvidia-smi ran without a known permitted device")
				return "", "", nil
			}}
			sensor := &fakeSensor{sensors: SensorMap{"01:00.0": {JunctionTemperatureC: pointer(55.0)}}}
			poller := NewPoller(PollerOptions{
				Interval:     time.Second,
				Runner:       runner,
				SensorSource: sensor,
				Power: &PowerSource{
					Gate:    &staticGate{sample: true},
					Devices: testCase.devices,
				},
			})
			if err := poller.PollOnce(context.Background()); err != nil {
				t.Fatalf("poll: %v", err)
			}
			if calls := runner.callCount(); calls != 0 {
				t.Fatalf("nvidia-smi calls = %d, want 0", calls)
			}
			if polls := sensor.pollCount(); polls != 0 {
				t.Fatalf("sensor polls = %d, want 0", polls)
			}
			snapshot := poller.Latest()
			if snapshot == nil {
				t.Fatal("snapshot = nil, want an empty gated snapshot")
			}
			if len(snapshot.Devices) != testCase.want {
				t.Fatalf("devices = %+v, want %d", snapshot.Devices, testCase.want)
			}
			if apps := poller.ComputeApps(); len(apps) != 0 {
				t.Fatalf("compute apps = %+v, want none", apps)
			}
		})
	}
}

// TestGatedPollerAlwaysRestrictsCommandsToPermittedDevices covers the other
// half of the fail-closed rule: even when every listed device is permitted,
// the command is still limited to the explicit --id list so an unlisted device
// is never touched.
func TestGatedPollerAlwaysRestrictsCommandsToPermittedDevices(t *testing.T) {
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args []string, _ time.Duration) (string, string, error) {
		switch {
		case isGPUQuery(args):
			return readFixture(t, "nvidia/two-gpus.csv"), "", nil
		case isAppQuery(args):
			return readFixture(t, "nvidia/compute-apps.csv"), "", nil
		default:
			return "", "", nil
		}
	}}
	poller := NewPoller(PollerOptions{
		Interval: time.Second,
		Runner:   runner,
		Power: &PowerSource{
			Gate:    &staticGate{sample: true},
			Devices: func() []string { return []string{laptopAddress, awakeAddress} },
		},
	})

	wantID := "--id=00000000:01:00.0,00000000:02:00.0"
	for cycle := 0; cycle < computeAppsEveryNthQ; cycle++ {
		if err := poller.PollOnce(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	wantCalls := computeAppsEveryNthQ + 1 // one compute-app query on the last cycle
	if calls := runner.callCount(); calls != wantCalls {
		t.Fatalf("command calls = %d, want %d", calls, wantCalls)
	}
	for index := 0; index < wantCalls; index++ {
		found := false
		for _, arg := range runner.call(index) {
			if arg == wantID {
				found = true
			}
		}
		if !found {
			t.Fatalf("call %d args = %v, want %s", index, runner.call(index), wantID)
		}
	}
	if !isAppQuery(runner.call(wantCalls - 1)) {
		t.Fatalf("last call args = %v, want the compute-app query", runner.call(wantCalls-1))
	}
	if devices := poller.Latest().Devices; len(devices) != 1 {
		t.Fatalf("devices = %+v, want the one row the fixture reports", devices)
	}
}

// TestRunningSensorCycleLosesAuthorizationWhenTheGateNarrows reproduces the
// leak where a reused sensor cycle kept its old scope: after a settle timeout,
// a later gate decision excluded the only GPU while that cycle still performed
// another BAR read.
func TestRunningSensorCycleLosesAuthorizationWhenTheGateNarrows(t *testing.T) {
	// A Blackwell device splits one sample into two BAR reads, so the second
	// read is the access the narrowed authorization must refuse.
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{laptopAddress: "0x2b85"})
	source := newHoldRegisters()
	temps := NewTempsPoller(TempsPollerOptions{
		Sysfs:    sysfs,
		Source:   source,
		SysfsDir: DefaultDRMDevicesDir,
	})
	gate := &toggleGate{sample: true}
	poller := NewPoller(PollerOptions{
		Interval:     time.Second,
		Timeout:      20 * time.Millisecond,
		Runner:       mixedRunner(t, "nvidia/laptop-4060.csv"),
		SensorSource: temps,
		Power: &PowerSource{
			Gate:    gate,
			Devices: func() []string { return []string{laptopAddress} },
		},
	})

	first := make(chan error, 1)
	go func() { first <- poller.PollOnce(context.Background()) }()
	source.waitStarted(t)
	if err := <-first; err != nil {
		t.Fatalf("first cycle: %v", err)
	}

	// The gate now excludes the device. The cycle that did not settle is
	// still running and must not touch it again.
	gate.set(false)
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("narrowed cycle: %v", err)
	}
	source.waitCanceled(t)
	close(source.release)
	poller.Wait()

	calls := source.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("register calls = %d, want only the read already in flight: %+v", len(calls), calls)
	}
	if !samplePathsContain(calls, laptopAddress) {
		t.Fatalf("register calls = %+v, want the in-flight device", calls)
	}
}

// TestRunningSensorCycleSkipsThePermissionProbeAfterTheGateNarrows covers the
// other device access: when nothing decodes, the cycle probes resource0 for
// diagnostics, and that probe must re-check the live decision too.
func TestRunningSensorCycleSkipsThePermissionProbeAfterTheGateNarrows(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{laptopAddress: "0x2204"})
	source := newHoldRegisters() // every value is nil: the probe path would run
	temps := NewTempsPoller(TempsPollerOptions{
		Sysfs:    sysfs,
		Source:   source,
		SysfsDir: DefaultDRMDevicesDir,
	})
	gate := &toggleGate{sample: true}
	poller := NewPoller(PollerOptions{
		Interval:     time.Second,
		Timeout:      20 * time.Millisecond,
		Runner:       mixedRunner(t, "nvidia/laptop-4060.csv"),
		SensorSource: temps,
		Power: &PowerSource{
			Gate:    gate,
			Devices: func() []string { return []string{laptopAddress} },
		},
	})

	first := make(chan error, 1)
	go func() { first <- poller.PollOnce(context.Background()) }()
	source.waitStarted(t)
	if err := <-first; err != nil {
		t.Fatalf("first cycle: %v", err)
	}

	gate.set(false)
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("narrowed cycle: %v", err)
	}
	source.waitCanceled(t)
	close(source.release)
	poller.Wait()

	if calls := source.callCount(); calls != 1 {
		t.Fatalf("register calls = %d, want only the read already in flight", calls)
	}
}

// TestStopPreventsSensorsFromStartingAfterwards reproduces the shutdown race:
// a sensor launched after Stop used a context nobody would cancel, so Wait
// could block on it forever. The launch now checks the poller root while
// holding the sensor lock.
func TestStopPreventsSensorsFromStartingAfterwards(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	var once sync.Once
	sensor := &fakeSensor{
		sensors: SensorMap{"c1:00.0": {JunctionTemperatureC: pointer(60.0)}},
		poll: func(context.Context, SampleScope) error {
			once.Do(func() { close(started) })
			<-release
			return nil
		},
	}
	poller := pollerWithSensor(t, sensor, PollerOptions{})
	poller.Stop()

	done := make(chan error, 1)
	go func() { done <- poller.PollOnce(context.Background()) }()
	select {
	case <-started:
		t.Fatal("a register-temperature cycle started after Stop")
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PollOnce after Stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PollOnce after Stop did not finish")
	}
}

// TestGateQuietWindowStartsAfterEveryHardwareQuery uses the real
// [RuntimePMGate]: the compute-app query must run before idle state is
// recorded, or the quiet window starts while that query is still touching the
// device.
func TestGateQuietWindowStartsAfterEveryHardwareQuery(t *testing.T) {
	sysfs := mixedDisplaySysfs(
		map[string]string{laptopControlPath: "auto\n"},
		map[string]string{laptopStatusPath: "active\n"},
	)
	testClock := &clock{}
	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText, Now: testClock.Now})
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args []string, _ time.Duration) (string, string, error) {
		switch {
		case isGPUQuery(args):
			return readFixture(t, "nvidia/laptop-4060.csv"), "", nil
		case isAppQuery(args):
			// The query takes longer than the quiet window it must not
			// overlap with.
			testClock.Advance(25 * time.Second)
			return readFixture(t, "nvidia/compute-apps.csv"), "", nil
		default:
			return "", "", nil
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
	for cycle := 0; cycle < computeAppsEveryNthQ; cycle++ {
		if err := poller.PollOnce(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != PausedReason {
		t.Fatalf("gate decision = %+v, want the quiet window to be open", decision)
	}

	before := runner.callCount()
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("quiet cycle: %v", err)
	}
	if calls := runner.callCount(); calls != before {
		t.Fatalf("nvidia-smi calls = %d, want none during the quiet window", calls-before)
	}
}

// TestGatedPollerSkipsUnparseableInventory keeps a malformed inventory entry
// from widening a query: it is reported as unavailable and never queried.
func TestGatedPollerSkipsUnparseableInventory(t *testing.T) {
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		return readFixture(t, "nvidia/two-gpus.csv"), "", nil
	}}
	poller := NewPoller(PollerOptions{
		Interval: time.Second,
		Runner:   runner,
		Power: &PowerSource{
			Gate:    &staticGate{sample: true},
			Devices: func() []string { return []string{laptopAddress, "bogus-device"} },
		},
	})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("calls = %d, want one restricted GPU query", calls)
	}
	for _, arg := range runner.call(0) {
		if strings.Contains(arg, "bogus") {
			t.Fatalf("args = %v, want the malformed address excluded", runner.call(0))
		}
	}
	devices := poller.Latest().Devices
	if len(devices) != 2 {
		t.Fatalf("devices = %+v, want the sampled device plus the rejected one", devices)
	}
	if devices[1].ID != "pci:bogus-device" || devices[1].Status != StatusUnavailable {
		t.Fatalf("rejected device = %+v", devices[1])
	}
}

// deferredReapRunner is a [reapObserver] that can leave one command's cleanup
// pending, exactly like a command child the kernel has not reaped yet: the
// poller reported a timeout, but the child may still be touching the device.
type deferredReapRunner struct {
	mu      sync.Mutex
	gpuCSV  string
	appsCSV string
	stuck   bool
	pending []func()
	calls   int
}

func (r *deferredReapRunner) Run(_ context.Context, _ string, args []string, _ time.Duration) (string, string, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if isAppQuery(args) {
		return r.appsCSV, "", nil
	}
	return r.gpuCSV, "", nil
}

func (r *deferredReapRunner) runReaping(ctx context.Context, command string, args []string, timeout time.Duration, onReap func()) (string, string, error) {
	r.mu.Lock()
	stuck := r.stuck && isAppQuery(args)
	if stuck {
		r.calls++
		r.pending = append(r.pending, onReap)
	}
	r.mu.Unlock()
	if stuck {
		return "", "", fmt.Errorf("gpu: %s timed out after %s", command, timeout)
	}
	stdout, stderr, err := r.Run(ctx, command, args, timeout)
	if onReap != nil {
		onReap()
	}
	return stdout, stderr, err
}

// reap settles every pending command, as the kernel would once it reaps them.
func (r *deferredReapRunner) reap() {
	r.mu.Lock()
	pending := r.pending
	r.pending = nil
	r.mu.Unlock()
	for _, release := range pending {
		release()
	}
}

func (r *deferredReapRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// gateIdleStreak reads the gate's consecutive-idle counter for one device.
func gateIdleStreak(gate *RuntimePMGate, address string) int {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	state, ok := gate.devices[address]
	if !ok {
		return 0
	}
	return state.idleStreak
}

// TestCommandLeaseOutlivesAnUnreapedChild is the nvidia-smi half of finding 1:
// the poller used to release the cycle's leases as soon as its own call
// returned, even when the timed-out child was never reaped and could still be
// touching the GPU. The child's reaper now owns the leases, so the device stays
// non-drainable and the gate reports busy.
func TestCommandLeaseOutlivesAnUnreapedChild(t *testing.T) {
	sysfs := runtimePMSysfs("auto\n", "active\n")
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(20 * time.Millisecond)
	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText, Access: arbiter})
	proc := &fakeChildProcess{release: make(chan struct{})}
	runner := ExecRunner{
		slot: &reapSlot{},
		launch: func(io.Writer, io.Writer) (childProcess, error) {
			return proc, nil
		},
	}
	poller := NewPoller(PollerOptions{
		Interval: time.Second,
		Timeout:  20 * time.Millisecond,
		Runner:   runner,
		Power: &PowerSource{
			Gate:    gate,
			Devices: func() []string { return []string{laptopAddress} },
		},
	})

	if err := poller.PollOnce(context.Background()); err == nil {
		t.Fatal("poll err = nil, want the command timeout")
	}
	if status := arbiter.Status(laptopAddress); status.Outstanding == 0 {
		t.Fatalf("status = %+v, want the lease held while the child is unreaped", status)
	}
	for sample := 0; sample < 3; sample++ {
		gate.Record(laptopAddress, true)
	}
	if decision := gate.Decide(laptopAddress); decision.Sample || decision.Reason != BusyReason {
		t.Fatalf("decision = %+v, want %q while the child is unreaped", decision, BusyReason)
	}

	close(proc.release)
	waitFor(t, 2*time.Second, func() bool {
		return arbiter.Status(laptopAddress).Outstanding == 0
	})
	waitFor(t, 2*time.Second, func() bool {
		decision := gate.Decide(laptopAddress)
		return !decision.Sample && decision.Reason == PausedReason
	})
}

// TestTimedOutComputeAppQueryRecordsNoIdleSample is the idle-state half of
// finding 1: a compute-app query that timed out left an unreaped child behind,
// yet the cycle still counted the GPU reading as idle, so the gate could open
// its quiet window underneath that child. It now stays pending: no idle sample,
// the lease stays held, and the gate reports busy.
func TestTimedOutComputeAppQueryRecordsNoIdleSample(t *testing.T) {
	sysfs := runtimePMSysfs("auto\n", "active\n")
	arbiter := NewAccessArbiter()
	arbiter.SetDrainTimeout(20 * time.Millisecond)
	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText, Access: arbiter})
	runner := &deferredReapRunner{
		gpuCSV:  readFixture(t, "nvidia/laptop-4060.csv"),
		appsCSV: readFixture(t, "nvidia/compute-apps.csv"),
		stuck:   true,
	}
	poller := NewPoller(PollerOptions{
		Interval: time.Second,
		Timeout:  20 * time.Millisecond,
		Runner:   runner,
		Power: &PowerSource{
			Gate:    gate,
			Devices: func() []string { return []string{laptopAddress} },
		},
	})
	for cycle := 0; cycle < computeAppsEveryNthQ; cycle++ {
		if err := poller.PollOnce(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	if calls := runner.callCount(); calls != computeAppsEveryNthQ+1 {
		t.Fatalf("command calls = %d, want the GPU query per cycle plus one compute-app query", calls)
	}

	// The two settled cycles are idle; the third is not, because its
	// compute-app child is still unreaped.
	if streak := gateIdleStreak(gate, laptopAddress); streak != 2 {
		t.Fatalf("idle streak = %d, want 2 (the third sample must not be recorded)", streak)
	}
	if decision := gate.Decide(laptopAddress); !decision.Sample {
		t.Fatalf("decision = %+v, want sampling to continue", decision)
	}
	if status := arbiter.Status(laptopAddress); status.Outstanding == 0 {
		t.Fatalf("status = %+v, want the compute-app lease held", status)
	}

	runner.reap()
	waitFor(t, 2*time.Second, func() bool {
		return arbiter.Status(laptopAddress).Outstanding == 0
	})
}
