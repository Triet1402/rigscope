package gpu

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"
)

var errSensorFailure = errors.New("simulated sysfs failure")

func TestPollerNotFoundDisablesNormalCadence(t *testing.T) {
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		return "", "", NotFoundError("nvidia-smi")
	}}
	poller := NewPoller(PollerOptions{Interval: 100 * time.Millisecond, Runner: runner})
	if poller.PollInterval() != time.Second {
		t.Fatalf("poll interval = %s, want 1s floor", poller.PollInterval())
	}
	if poller.Timeout() != 3*time.Second {
		t.Fatalf("timeout = %s, want 3s default", poller.Timeout())
	}
	if err := poller.PollOnce(context.Background()); err == nil {
		t.Fatal("pollOnce err = nil, want the not-found error")
	}
	if runner.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", runner.callCount())
	}
	if got := runner.observedTimeout(0); got != 3*time.Second {
		t.Fatalf("observed timeout = %s, want 3s", got)
	}
	if got := poller.Available(); got != AvailabilityNo {
		t.Fatalf("availability = %q, want no", got)
	}
	if poller.Latest() != nil {
		t.Fatalf("latest = %+v, want nil", poller.Latest())
	}
	stats := poller.CostStats()
	if stats.Runs != 1 || stats.Failures != 1 {
		t.Fatalf("stats = %+v, want one failed run", stats)
	}
	if stats.AvgDurationMs != stats.MaxDurationMs {
		t.Fatalf("avg %v != max %v for one run", stats.AvgDurationMs, stats.MaxDurationMs)
	}
	if delay := poller.nextDelay(); delay != notFoundRetryDelay {
		t.Fatalf("next delay = %s, want %s", delay, notFoundRetryDelay)
	}

	// ENOENT is also recognized through a wrapped fs.ErrNotExist, which is
	// what ExecRunner reports.
	runner = &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		return "", "", fmt.Errorf("spawn nvidia-smi: %w", fs.ErrNotExist)
	}}
	poller = NewPoller(PollerOptions{Interval: time.Second, Runner: runner})
	_ = poller.PollOnce(context.Background())
	if got := poller.Available(); got != AvailabilityNo {
		t.Fatalf("availability = %q, want no", got)
	}
}

func TestPollerSuccessAndComputeAppsEveryThirdQuery(t *testing.T) {
	gpuCSV := readFixture(t, "nvidia/gpu-3090.csv")
	appsCSV := readFixture(t, "nvidia/compute-apps.csv")
	c := &clock{}
	gpuQueries, appQueries := 0, 0
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args []string, _ time.Duration) (string, string, error) {
		c.Advance(7 * time.Millisecond)
		switch {
		case isGPUQuery(args):
			gpuQueries++
			return gpuCSV, "", nil
		case isAppQuery(args):
			appQueries++
			return appsCSV, "", nil
		default:
			return "", "", errors.New("unexpected arguments")
		}
	}}
	poller := NewPoller(PollerOptions{Interval: 5 * time.Second, Runner: runner, Monotonic: c.Now})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got := poller.Available(); got != AvailabilityYes {
		t.Fatalf("availability = %q, want yes", got)
	}
	if gpuQueries != 1 || appQueries != 0 {
		t.Fatalf("queries = %d/%d, want 1/0", gpuQueries, appQueries)
	}
	snapshot := poller.Latest()
	if snapshot == nil || len(snapshot.Devices) != 2 {
		t.Fatalf("snapshot = %+v, want two devices", snapshot)
	}
	if snapshot.Devices[0].ID != "gpu:GPU-aaaaaaaa-1111" {
		t.Fatalf("device id = %q", snapshot.Devices[0].ID)
	}
	requireBytes(t, "memory total", snapshot.Devices[0].MemoryTotalBytes, 24576*mib)
	if info := poller.DeviceInfo(); len(info) != 2 || info[0].UUID != "GPU-aaaaaaaa-1111" {
		t.Fatalf("device info = %+v", info)
	}

	for run := 0; run < 2; run++ {
		if err := poller.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", run, err)
		}
	}
	if gpuQueries != 3 || appQueries != 1 {
		t.Fatalf("queries = %d/%d, want 3/1", gpuQueries, appQueries)
	}
	apps := poller.ComputeApps()
	if got := apps[2_662_911]; got != (15222+256)*mib {
		t.Fatalf("compute apps pid sum = %d, want %d", got, (15222+256)*mib)
	}
	stats := poller.CostStats()
	if stats.Runs != 4 || stats.Failures != 0 {
		t.Fatalf("stats = %+v, want 4 runs and no failures", stats)
	}
	if stats.LastDurationMs != 7 || stats.MaxDurationMs != 7 {
		t.Fatalf("durations = %+v, want 7ms", stats)
	}
}

func TestPollerConcurrentPollOnceSharesOneQuery(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return readFixture(t, "nvidia/gpu-3090.csv"), "", nil
	}}
	poller := NewPoller(PollerOptions{Interval: 2 * time.Second, Runner: runner})

	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- poller.PollOnce(context.Background()) }()
	<-started
	go func() { second <- poller.PollOnce(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("calls while first query is in flight = %d, want 1", calls)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if snapshot := poller.Latest(); snapshot == nil || len(snapshot.Devices) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestPollerTimeoutFailureThenRecovery(t *testing.T) {
	mode := "timeout"
	runner := &fakeRunner{handler: func(_ context.Context, _ string, _ []string, _ time.Duration) (string, string, error) {
		if mode == "timeout" {
			return "", "", fmt.Errorf("command failed: %w", context.DeadlineExceeded)
		}
		return readFixture(t, "nvidia/gpu-3090.csv"), "", nil
	}}
	poller := NewPoller(PollerOptions{Interval: 2 * time.Second, Timeout: 50 * time.Millisecond, Runner: runner})
	if err := poller.PollOnce(context.Background()); err == nil {
		t.Fatal("pollOnce err = nil, want timeout failure")
	}
	if got := runner.observedTimeout(0); got != 50*time.Millisecond {
		t.Fatalf("observed timeout = %s, want 50ms", got)
	}
	if got := poller.Available(); got != AvailabilityUnknown {
		t.Fatalf("availability = %q, want unknown", got)
	}
	if stats := poller.CostStats(); stats.Failures != 1 {
		t.Fatalf("failures = %d, want 1", stats.Failures)
	}
	if poller.Latest() != nil {
		t.Fatal("latest should stay nil before the first success")
	}

	mode = "ok"
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("recovery poll: %v", err)
	}
	if got := poller.Available(); got != AvailabilityYes {
		t.Fatalf("availability = %q, want yes", got)
	}
	stats := poller.CostStats()
	if stats.Failures != 1 || stats.Runs != 2 {
		t.Fatalf("stats = %+v, want 1 failure in 2 runs", stats)
	}
	if snapshot := poller.Latest(); snapshot == nil || len(snapshot.Devices) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestPollerBackoffCappedAtFiveMinutes(t *testing.T) {
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		return "", "", errors.New("driver returned an error")
	}}
	poller := NewPoller(PollerOptions{Interval: time.Second, Runner: runner})
	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		64 * time.Second,
		128 * time.Second,
		256 * time.Second,
		300 * time.Second,
		300 * time.Second,
		300 * time.Second,
	}
	for index, expected := range want {
		_ = poller.PollOnce(context.Background())
		if delay := poller.nextDelay(); delay != expected {
			t.Fatalf("delay after %d failure(s) = %s, want %s", index+1, delay, expected)
		}
	}
	if stats := poller.CostStats(); stats.Failures != len(want) {
		t.Fatalf("failures = %d, want %d", stats.Failures, len(want))
	}
}

func TestPollerKeepsLastSnapshotOnFailure(t *testing.T) {
	failing := false
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		if failing {
			return "", "", errors.New("transient query failure")
		}
		return readFixture(t, "nvidia/gpu-3090.csv"), "", nil
	}}
	poller := NewPoller(PollerOptions{Interval: time.Second, Runner: runner})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	before := poller.Latest()
	failing = true
	if err := poller.PollOnce(context.Background()); err == nil {
		t.Fatal("pollOnce err = nil, want failure")
	}
	after := poller.Latest()
	if after == nil || len(after.Devices) != len(before.Devices) {
		t.Fatalf("snapshot after failure = %+v, want the previous reading", after)
	}
	if got := poller.Available(); got != AvailabilityYes {
		t.Fatalf("availability = %q, want yes (tooling still works)", got)
	}
}

func TestPollerDiagnosticsOnlyOnStateChange(t *testing.T) {
	var events []string
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		return readFixture(t, "nvidia/gpu-3090.csv"), "", nil
	}}
	poller := NewPoller(PollerOptions{
		Interval: time.Second,
		Runner:   runner,
		OnEvent:  func(line string) { events = append(events, line) },
	})
	for run := 0; run < 3; run++ {
		_ = poller.PollOnce(context.Background())
	}
	count := 0
	for _, line := range events {
		if line == "gpu telemetry: 2 device(s)" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("device-count line emitted %d times, want 1: %v", count, events)
	}
}

func TestPollerStartStopsOnContextCancel(t *testing.T) {
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		return readFixture(t, "nvidia/gpu-3090.csv"), "", nil
	}}
	poller := NewPoller(PollerOptions{Interval: time.Second, Runner: runner})
	ctx, cancel := context.WithCancel(context.Background())
	poller.Start(ctx)
	waitFor(t, 2*time.Second, func() bool { return poller.Latest() != nil })

	cancel()
	waited := make(chan struct{})
	go func() {
		poller.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop after context cancellation")
	}
	poller.Stop()
}

// --- register-temperature merge into nvidia-smi readings ----------------------

func pollerWithSensor(t *testing.T, sensor SensorSource, options PollerOptions) *Poller {
	t.Helper()
	runner := &fakeRunner{handler: func(_ context.Context, _ string, args []string, _ time.Duration) (string, string, error) {
		if isGPUQuery(args) {
			return readFixture(t, "nvidia/gpu-3090.csv"), "", nil
		}
		return "", "", nil
	}}
	options.Runner = runner
	options.SensorSource = sensor
	options.Interval = time.Second
	return NewPoller(options)
}

func TestPollerMergesRegisterTemperaturesByBusID(t *testing.T) {
	sensor := &fakeSensor{sensors: SensorMap{
		"c1:00.0": {JunctionTemperatureC: pointer(71.0), VRAMTemperatureC: pointer(74.5)},
	}}
	poller := pollerWithSensor(t, sensor, PollerOptions{})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	devices := poller.Latest().Devices
	if len(devices) != 2 {
		t.Fatalf("devices = %+v", devices)
	}
	requireReadingValue(t, "junction", devices[0].JunctionTemperatureC, 71)
	requireReadingValue(t, "vram", devices[0].VRAMTemperatureC, 74.5)
	if devices[1].JunctionTemperatureC != nil || devices[1].VRAMTemperatureC != nil {
		t.Fatalf("unmatched device got register values: %+v", devices[1])
	}
	if sensor.pollCount() != 1 {
		t.Fatalf("sensor polls = %d, want 1", sensor.pollCount())
	}
}

func TestPollerPartiallySupportedSensorLeavesAbsentFieldsAbsent(t *testing.T) {
	sensor := &fakeSensor{sensors: SensorMap{
		"c1:00.0": {JunctionTemperatureC: pointer(60.0)},
	}}
	poller := pollerWithSensor(t, sensor, PollerOptions{})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	first := poller.Latest().Devices[0]
	requireReadingValue(t, "junction", first.JunctionTemperatureC, 60)
	if first.VRAMTemperatureC != nil {
		t.Fatalf("vram = %v, want absent", *first.VRAMTemperatureC)
	}
}

func TestPollerStallingSensorCannotHoldUpSnapshot(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sensor := &fakeSensor{
		sensors: SensorMap{"c1:00.0": {JunctionTemperatureC: pointer(71.0), VRAMTemperatureC: pointer(74.5)}},
		poll: func(context.Context, SampleScope) error {
			<-release
			return nil
		},
	}
	var events []string
	poller := pollerWithSensor(t, sensor, PollerOptions{
		Timeout: 5 * time.Millisecond,
		OnEvent: func(line string) { events = append(events, line) },
	})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	first := poller.Latest().Devices[0]
	requireReadingValue(t, "junction", first.JunctionTemperatureC, 71)
	found := false
	for _, line := range events {
		if strings.Contains(line, "did not settle") {
			found = true
		}
	}
	if !found {
		t.Fatalf("events = %v, want a settle-timeout diagnostic", events)
	}
}

func TestPollerSensorFailuresNeverInvalidateNvidiaReadings(t *testing.T) {
	sensor := &fakeSensor{
		sensors: SensorMap{},
		poll: func(context.Context, SampleScope) error {
			return errSensorFailure
		},
	}
	poller := pollerWithSensor(t, sensor, PollerOptions{})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got := poller.Available(); got != AvailabilityYes {
		t.Fatalf("availability = %q, want yes", got)
	}
	if devices := poller.Latest().Devices; len(devices) != 2 {
		t.Fatalf("devices = %+v, want two", devices)
	}
}

// --- runtime-PM integration ---------------------------------------------------

func TestPollerNeverQueriesSuspendedGPU(t *testing.T) {
	sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
	sysfs.set(laptopStatusPath, "suspended\n")
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		return readFixture(t, "nvidia/laptop-4060.csv"), "", nil
	}}
	sensor := &fakeSensor{sensors: SensorMap{}}
	poller := NewPoller(PollerOptions{
		Interval:     2 * time.Second,
		Runner:       runner,
		SensorSource: sensor,
		Power: &PowerSource{
			Gate:    NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText}),
			Devices: func() []string { return []string{laptopAddress} },
		},
	})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if calls := runner.callCount(); calls != 0 {
		t.Fatalf("nvidia-smi calls = %d, want 0 for a suspended GPU", calls)
	}
	if polls := sensor.pollCount(); polls != 0 {
		t.Fatalf("sensor polls = %d, want 0 for a suspended GPU", polls)
	}
	snapshot := poller.Latest()
	if snapshot == nil || len(snapshot.Devices) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	expected := UnavailableReading("pci:"+laptopAddress, PoweredDownReason)
	if snapshot.Devices[0] != expected {
		t.Fatalf("device = %+v, want %+v", snapshot.Devices[0], expected)
	}

	sysfs.set(laptopStatusPath, "active\n")
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("wake poll: %v", err)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("nvidia-smi calls = %d, want 1 after wake", calls)
	}
	// A gated query is always restricted to the permitted addresses, even
	// when nothing is skipped, so an unlisted device can never be touched.
	restricted := false
	for _, arg := range runner.call(0) {
		if arg == "--id=00000000:01:00.0" {
			restricted = true
		}
	}
	if !restricted {
		t.Fatalf("args = %v, want --id=00000000:01:00.0", runner.call(0))
	}
	if devices := poller.Latest().Devices; devices[0].Status != StatusOK {
		t.Fatalf("device after wake = %+v, want ok", devices[0])
	}

	// Once the UUID is known, a sleeping device keeps the nvidia-smi id.
	sysfs.set(laptopStatusPath, "suspended\n")
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("re-suspend poll: %v", err)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("nvidia-smi calls = %d, want still 1", calls)
	}
	devices := poller.Latest().Devices
	if len(devices) != 1 || devices[0].ID != "gpu:GPU-aaaa" || devices[0].Status != StatusUnavailable {
		t.Fatalf("gated devices = %+v, want gpu:GPU-aaaa unavailable", devices)
	}
	if info := poller.DeviceInfo(); len(info) != 1 {
		t.Fatalf("device info = %+v, want the remembered device", info)
	}
}

func TestPollerRestrictsQueryWhenOneGPUIsAsleep(t *testing.T) {
	other := "0000:02:00.0"
	sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
	sysfs.set(laptopStatusPath, "suspended\n")
	read := func(name string) (string, bool) {
		if strings.Contains(name, other) {
			if strings.HasSuffix(name, "control") {
				return "on\n", true
			}
			return "active\n", true
		}
		return sysfs.ReadText(name)
	}
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		return readFixture(t, "nvidia/two-gpus.csv"), "", nil
	}}
	poller := NewPoller(PollerOptions{
		Interval: 2 * time.Second,
		Runner:   runner,
		Power: &PowerSource{
			Gate:    NewRuntimePMGate(RuntimePMGateOptions{Read: read}),
			Devices: func() []string { return []string{laptopAddress, other} },
		},
	})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if calls := runner.callCount(); calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	restricted := false
	for _, arg := range runner.call(0) {
		if arg == "--id=00000000:02:00.0" {
			restricted = true
		}
	}
	if !restricted {
		t.Fatalf("args = %v, want --id=00000000:02:00.0", runner.call(0))
	}
	devices := poller.Latest().Devices
	if len(devices) != 2 {
		t.Fatalf("devices = %+v, want awake device plus gated one", devices)
	}
	if devices[0].ID != "gpu:GPU-bbbb" || devices[0].Status != StatusOK {
		t.Fatalf("awake device = %+v", devices[0])
	}
	if devices[1].ID != "pci:"+laptopAddress || devices[1].Status != StatusUnavailable {
		t.Fatalf("asleep device = %+v", devices[1])
	}
}
