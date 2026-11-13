package gpu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// --- one register-temperature cycle at a time (lifecycle) ---------------------

func TestPollerReusesOneSensorCycleAcrossSettleTimeouts(t *testing.T) {
	// The fake ignores cancellation on purpose: it stands in for a wedged BAR
	// read. Five consecutive settle timeouts must still leave exactly one
	// operation running, and shutdown must join it.
	release := make(chan struct{})
	sensor := &fakeSensor{
		sensors: SensorMap{"c1:00.0": {JunctionTemperatureC: pointer(71.0)}},
		poll: func(context.Context, SampleScope) error {
			<-release
			return nil
		},
	}
	before := runtime.NumGoroutine()
	poller := pollerWithSensor(t, sensor, PollerOptions{Timeout: 5 * time.Millisecond})
	for cycle := 0; cycle < 5; cycle++ {
		if err := poller.PollOnce(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	if polls := sensor.pollCount(); polls != 1 {
		t.Fatalf("sensor polls = %d, want 1 reused cycle", polls)
	}
	if devices := poller.Latest().Devices; len(devices) == 0 {
		t.Fatal("settle timeouts lost the nvidia-smi readings")
	}

	poller.Stop()
	waited := make(chan struct{})
	go func() {
		poller.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("Wait returned before the register-temperature cycle finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not join the register-temperature cycle")
	}

	// One allowance for runtime bookkeeping; the regression this guards
	// against grew by one goroutine per cycle.
	waitFor(t, 2*time.Second, func() bool { return runtime.NumGoroutine() <= before+1 })
}

// TestPollerWaitDoesNotHangOnAnUnreapedHelper is the shutdown half of the
// bounded-wait rule: a child that SIGKILL cannot reap must not keep
// Poller.Wait blocked after Stop.
func TestPollerWaitDoesNotHangOnAnUnreapedHelper(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	proc := &fakeChildProcess{release: release}
	source := &HelperSource{
		Timeout: 20 * time.Millisecond,
		slot:    &reapSlot{},
		launch: func(io.Writer, io.Writer) (childProcess, error) {
			return proc, nil
		},
	}
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{laptopAddress: "0x2204"})
	temps := NewTempsPoller(TempsPollerOptions{Sysfs: sysfs, Source: source})
	poller := NewPoller(PollerOptions{
		Interval:     time.Second,
		Timeout:      20 * time.Millisecond,
		Runner:       mixedRunner(t, "nvidia/laptop-4060.csv"),
		SensorSource: temps,
	})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	waited := make(chan struct{})
	go func() {
		poller.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait blocked on a helper the kernel never reaped")
	}
	poller.Stop()
}

// --- cancellation -------------------------------------------------------------

func TestJoinedPollOnceHonorsCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	runner := &fakeRunner{handler: func(ctx context.Context, _ string, _ []string, _ time.Duration) (string, string, error) {
		once.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-release:
			return readFixture(t, "nvidia/gpu-3090.csv"), "", nil
		}
	}}
	poller := NewPoller(PollerOptions{Interval: time.Second, Runner: runner})

	owner := make(chan error, 1)
	go func() { owner <- poller.PollOnce(context.Background()) }()
	<-started

	waiterCtx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() { waiter <- poller.PollOnce(waiterCtx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("joined PollOnce err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("joined PollOnce ignored the caller's cancellation")
	}

	close(release)
	if err := <-owner; err != nil {
		t.Fatalf("owning PollOnce: %v", err)
	}
}

func TestStopCancelsAnActiveManualQuery(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	runner := &fakeRunner{handler: func(ctx context.Context, _ string, _ []string, _ time.Duration) (string, string, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return "", "", ctx.Err()
	}}
	poller := NewPoller(PollerOptions{Interval: time.Second, Runner: runner})

	done := make(chan error, 1)
	go func() { done <- poller.PollOnce(context.Background()) }()
	<-started

	// No Start: the query belongs to a manual caller, and Stop must still
	// abort it through the cycle-owned cancel.
	poller.Stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("manual query err = nil, want cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel the active manual query")
	}
}

func TestStopCancelsAnActiveQueryStartedByTheLoop(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	runner := &fakeRunner{handler: func(ctx context.Context, _ string, _ []string, _ time.Duration) (string, string, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return "", "", ctx.Err()
	}}
	poller := NewPoller(PollerOptions{Interval: time.Second, Runner: runner})
	poller.Start(context.Background())
	<-started

	poller.Stop()
	waited := make(chan struct{})
	go func() {
		poller.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop after Stop")
	}
}

// --- snapshots are owned by the caller ---------------------------------------

func TestPollerLatestIsMutationIsolated(t *testing.T) {
	sensor := &fakeSensor{sensors: SensorMap{
		"c1:00.0": {JunctionTemperatureC: pointer(71.0), VRAMTemperatureC: pointer(74.5)},
	}}
	poller := pollerWithSensor(t, sensor, PollerOptions{})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	before := poller.Latest()
	first := before.Devices[0]
	requireReadingValue(t, "junction", first.JunctionTemperatureC, 71)
	requireReadingValue(t, "vram", first.VRAMTemperatureC, 74.5)
	requireReadingValue(t, "utilization", first.UtilizationPercent, 25)
	requireBytes(t, "memory total", first.MemoryTotalBytes, 24576*mib)

	// A consumer may keep and edit its copy.
	*first.JunctionTemperatureC = 99
	*first.VRAMTemperatureC = 99
	*first.UtilizationPercent = 99
	*first.MemoryTotalBytes = 1
	*first.TemperatureC = 99
	*first.PowerWatts = 99
	before.Devices[0].ID = "mutated"

	after := poller.Latest()
	if after.Devices[0].ID != "gpu:GPU-aaaaaaaa-1111" {
		t.Fatalf("id = %q, want the cached id", after.Devices[0].ID)
	}
	requireReadingValue(t, "junction after mutation", after.Devices[0].JunctionTemperatureC, 71)
	requireReadingValue(t, "vram after mutation", after.Devices[0].VRAMTemperatureC, 74.5)
	requireReadingValue(t, "utilization after mutation", after.Devices[0].UtilizationPercent, 25)
	requireBytes(t, "memory total after mutation", after.Devices[0].MemoryTotalBytes, 24576*mib)
	requireReadingValue(t, "temperature after mutation", after.Devices[0].TemperatureC, 30)
	requireReadingValue(t, "power after mutation", after.Devices[0].PowerWatts, 22.35)

	info := poller.DeviceInfo()
	*info[0].PCIBusID = "0000:ff:ff.7"
	*info[0].MemoryTotalBytes = 0
	if again := poller.DeviceInfo(); *again[0].PCIBusID != "00000000:C1:00.0" {
		t.Fatalf("device info bus id = %q, want the cached value", *again[0].PCIBusID)
	}
	if again := poller.DeviceInfo(); *again[0].MemoryTotalBytes != 24576*mib {
		t.Fatalf("device info memory = %d, want the cached value", *again[0].MemoryTotalBytes)
	}
}

func TestTempsPollerLatestIsMutationIsolated(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{laptopAddress: "0x2204"})
	source := newFakeRegisters()
	source.set(DefaultDRMDevicesDir+"/"+laptopAddress+"/resource0", 0x2046c, 60<<8)
	source.set(DefaultDRMDevicesDir+"/"+laptopAddress+"/resource0", 0xe2a8, 70*32)
	poller := NewTempsPoller(TempsPollerOptions{Sysfs: sysfs, Source: source})
	if err := poller.PollOnce(context.Background(), AllDevices()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	latest := poller.Latest()
	values, ok := latest["01:00.0"]
	if !ok {
		t.Fatalf("latest = %+v", latest)
	}
	*values.JunctionTemperatureC = 5
	*values.VRAMTemperatureC = 5
	delete(latest, "01:00.0")

	again := poller.Latest()
	values, ok = again["01:00.0"]
	if !ok {
		t.Fatalf("cache was mutated through the copy: %+v", again)
	}
	requireReadingValue(t, "junction", values.JunctionTemperatureC, 60)
	requireReadingValue(t, "vram", values.VRAMTemperatureC, 70)
}

func TestPollerSnapshotsAreSafeForConcurrentReaders(t *testing.T) {
	sensor := &fakeSensor{sensors: SensorMap{
		"c1:00.0": {JunctionTemperatureC: pointer(71.0), VRAMTemperatureC: pointer(74.5)},
	}}
	poller := pollerWithSensor(t, sensor, PollerOptions{})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snapshot := poller.Latest()
				for _, device := range snapshot.Devices {
					// Every reader mutates its own copy; the race detector
					// fails if any of these aliases the poller's cache.
					if device.JunctionTemperatureC != nil {
						*device.JunctionTemperatureC++
					}
					if device.VRAMTemperatureC != nil {
						*device.VRAMTemperatureC++
					}
					if device.UtilizationPercent != nil {
						*device.UtilizationPercent++
					}
					if device.MemoryTotalBytes != nil {
						*device.MemoryTotalBytes++
					}
				}
				for _, info := range poller.DeviceInfo() {
					if info.PCIBusID != nil {
						*info.PCIBusID += "x"
					}
				}
				_ = poller.ComputeApps()
				_ = poller.CostStats()
			}
		}()
	}

	for cycle := 0; cycle < 10; cycle++ {
		if err := poller.PollOnce(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	close(stop)
	readers.Wait()
}

// --- protocol cardinality bound ----------------------------------------------

func TestPollerCapsSnapshotsAtProtocolMaxGPUs(t *testing.T) {
	var csv strings.Builder
	for index := 0; index < protocol.MaxGPUs+4; index++ {
		fmt.Fprintf(&csv, "GPU-%02d, NVIDIA GPU, 00000000:%02x:00.0, 1, 2, 1024, 40, 10\n",
			index, index+1)
	}
	runner := &fakeRunner{handler: func(context.Context, string, []string, time.Duration) (string, string, error) {
		return csv.String(), "", nil
	}}
	poller := NewPoller(PollerOptions{Interval: time.Second, Runner: runner})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	devices := poller.Latest().Devices
	if len(devices) != protocol.MaxGPUs {
		t.Fatalf("devices = %d, want the protocol cap %d", len(devices), protocol.MaxGPUs)
	}
	// The cap keeps the head of the list deterministically.
	if devices[0].ID != "gpu:GPU-00" {
		t.Fatalf("first device = %q", devices[0].ID)
	}
	if last := devices[len(devices)-1].ID; last != fmt.Sprintf("gpu:GPU-%02d", protocol.MaxGPUs-1) {
		t.Fatalf("last device = %q", last)
	}
}
