package gpu

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A second NVIDIA display address used by the mixed-GPU tests. The laptop
// fixture covers 0000:01:00.0; this one is the awake device.
const awakeAddress = "0000:02:00.0"

const (
	awakeControlPath = "/sys/bus/pci/devices/" + awakeAddress + "/power/control"
	awakeStatusPath  = "/sys/bus/pci/devices/" + awakeAddress + "/power/runtime_status"
)

// mixedDisplaySysfs builds a sysfs view with two NVIDIA display functions and
// the runtime-PM attributes of both. Unless a test overrides them, the second
// device is a known-unmanaged device that is powered on: control "on" makes
// the gate's answer independent of runtime_status.
func mixedDisplaySysfs(control, status map[string]string) *memSysFS {
	files := make(map[string]string, len(control)+len(status)+8)
	for path, value := range control {
		files[path] = value
	}
	for path, value := range status {
		files[path] = value
	}
	if _, ok := files[awakeControlPath]; !ok {
		files[awakeControlPath] = "on\n"
	}
	if _, ok := files[awakeStatusPath]; !ok {
		files[awakeStatusPath] = "active\n"
	}
	return displayDeviceSysfs(files, map[string]string{
		laptopAddress: "0x2204",
		awakeAddress:  "0x2204",
	})
}

// mixedRunner answers the restricted GPU query with the awake device only,
// exactly like nvidia-smi --id does on a mixed machine.
func mixedRunner(t *testing.T, rows string) *fakeRunner {
	t.Helper()
	return &fakeRunner{handler: func(_ context.Context, _ string, args []string, _ time.Duration) (string, string, error) {
		if isGPUQuery(args) {
			return readFixture(t, rows), "", nil
		}
		return "", "", nil
	}}
}

// samplePathsContain reports whether any register batch touched the address.
func samplePathsContain(calls []registerCall, address string) bool {
	for _, call := range calls {
		if strings.Contains(call.path, address) {
			return true
		}
	}
	return false
}

func TestPollerNeverTouchesSuspendedRegistersOrProbesThem(t *testing.T) {
	sysfs := mixedDisplaySysfs(
		map[string]string{laptopControlPath: "auto\n"},
		map[string]string{laptopStatusPath: "suspended\n"},
	)
	// Nothing readable, so the temperature cycle falls through to the
	// permission probe — which must also skip the suspended device.
	source := newFakeRegisters()
	temps := NewTempsPoller(TempsPollerOptions{
		Sysfs:    sysfs,
		Source:   source,
		SysfsDir: DefaultDRMDevicesDir,
	})
	poller := NewPoller(PollerOptions{
		Interval:     2 * time.Second,
		Runner:       mixedRunner(t, "nvidia/two-gpus.csv"),
		SensorSource: temps,
		Power: &PowerSource{
			Gate:    NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText}),
			Devices: func() []string { return []string{laptopAddress, awakeAddress} },
		},
	})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	calls := source.callsSnapshot()
	if len(calls) == 0 {
		t.Fatal("register calls = 0, want the awake device to be sampled or probed")
	}
	if samplePathsContain(calls, laptopAddress) {
		t.Fatalf("register calls touched the suspended device: %+v", calls)
	}

	devices := poller.Latest().Devices
	if len(devices) != 2 {
		t.Fatalf("devices = %+v, want the awake device plus the gated one", devices)
	}
	if devices[0].ID != "gpu:GPU-bbbb" || devices[0].Status != StatusOK {
		t.Fatalf("awake device = %+v", devices[0])
	}
	if devices[1].ID != "pci:"+laptopAddress || devices[1].Status != StatusUnavailable {
		t.Fatalf("suspended device = %+v", devices[1])
	}
	if devices[1].JunctionTemperatureC != nil || devices[1].VRAMTemperatureC != nil {
		t.Fatalf("suspended device kept register temperatures: %+v", devices[1])
	}
}

func TestPollerNeverTouchesRegistersOfAYieldingDevice(t *testing.T) {
	// The awake device is in its quiet window: sampling is paused so the
	// driver can suspend it, and a register read would reset that timer.
	sysfs := mixedDisplaySysfs(
		map[string]string{
			laptopControlPath: "on\n",
			awakeControlPath:  "auto\n",
		},
		map[string]string{
			laptopStatusPath: "active\n",
			awakeStatusPath:  "active\n",
		},
	)
	clock := &clock{}
	gate := NewRuntimePMGate(RuntimePMGateOptions{Read: sysfs.ReadText, Now: clock.Now})
	for sample := 0; sample < 3; sample++ {
		gate.Record(awakeAddress, true)
	}
	if decision := gate.Decide(awakeAddress); decision.Sample || decision.Reason != PausedReason {
		t.Fatalf("gate decision = %+v, want the paused window", decision)
	}

	source := newFakeRegisters()
	temps := NewTempsPoller(TempsPollerOptions{
		Sysfs:    sysfs,
		Source:   source,
		SysfsDir: DefaultDRMDevicesDir,
	})
	poller := NewPoller(PollerOptions{
		Interval:     2 * time.Second,
		Runner:       mixedRunner(t, "nvidia/laptop-4060.csv"),
		SensorSource: temps,
		Power: &PowerSource{
			Gate:    gate,
			Devices: func() []string { return []string{laptopAddress, awakeAddress} },
		},
	})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	calls := source.callsSnapshot()
	if len(calls) == 0 {
		t.Fatal("register calls = 0, want the permitted device to be sampled or probed")
	}
	if samplePathsContain(calls, awakeAddress) {
		t.Fatalf("register calls touched the yielding device: %+v", calls)
	}

	devices := poller.Latest().Devices
	if len(devices) != 2 {
		t.Fatalf("devices = %+v", devices)
	}
	if devices[1].ID != "pci:"+awakeAddress || devices[1].Reason != PausedReason {
		t.Fatalf("yielding device = %+v", devices[1])
	}
}

func TestPollerPassesTheGateDecisionToTheSensorScope(t *testing.T) {
	sysfs := loadFixtureTree(t, "sysfs/runtime-pm/laptop")
	sysfs.set(laptopStatusPath, "suspended\n")
	read := func(name string) (string, bool) {
		if strings.Contains(name, awakeAddress) {
			if strings.HasSuffix(name, "control") {
				return "on\n", true
			}
			return "active\n", true
		}
		return sysfs.ReadText(name)
	}
	sensor := &fakeSensor{sensors: SensorMap{}}
	poller := NewPoller(PollerOptions{
		Interval:     2 * time.Second,
		Runner:       mixedRunner(t, "nvidia/two-gpus.csv"),
		SensorSource: sensor,
		Power: &PowerSource{
			Gate:    NewRuntimePMGate(RuntimePMGateOptions{Read: read}),
			Devices: func() []string { return []string{laptopAddress, awakeAddress} },
		},
	})
	if err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	scope, ok := sensor.scope(0)
	if !ok {
		t.Fatalf("sensor polls = %d, want 1", sensor.pollCount())
	}
	if scope.Allows(laptopAddress) {
		t.Fatal("scope permits the suspended device")
	}
	if !scope.Allows(awakeAddress) {
		t.Fatal("scope excludes the awake device")
	}
	if !scope.Gated() {
		t.Fatal("scope = ungated, want the gate decision")
	}
}

func TestReadGpuRegisterTempsIgnoresExcludedDevices(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{
		laptopAddress: "0x2204",
		awakeAddress:  "0x2204",
	})
	source := newFakeRegisters()
	for _, address := range []string{laptopAddress, awakeAddress} {
		source.set(DefaultDRMDevicesDir+"/"+address+"/resource0", 0x2046c, 55<<8)
	}

	sensors := ReadGpuRegisterTemps(context.Background(), source, sysfs, DefaultDRMDevicesDir,
		PermittedScope([]string{awakeAddress}))
	if len(sensors) != 1 {
		t.Fatalf("sensors = %+v, want only the permitted device", sensors)
	}
	if _, ok := sensors["01:00.0"]; ok {
		t.Fatal("excluded device contributed register readings")
	}
	if _, ok := sensors["02:00.0"]; !ok {
		t.Fatalf("sensors = %+v, want 02:00.0", sensors)
	}
	if calls := source.callsSnapshot(); samplePathsContain(calls, laptopAddress) {
		t.Fatalf("register calls touched the excluded device: %+v", calls)
	}
}

func TestTempsPollerPermissionProbesOnlyPermittedDevices(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{
		laptopAddress: "0x2204",
		awakeAddress:  "0x2204",
	})
	source := newFakeRegisters() // nothing readable: the probe path runs
	poller := NewTempsPoller(TempsPollerOptions{
		Sysfs:    sysfs,
		Source:   source,
		SysfsDir: DefaultDRMDevicesDir,
	})
	if err := poller.PollOnce(context.Background(), PermittedScope([]string{awakeAddress})); err != nil {
		t.Fatalf("poll: %v", err)
	}
	calls := source.callsSnapshot()
	if len(calls) == 0 {
		t.Fatal("register calls = 0, want the permitted device probed")
	}
	if samplePathsContain(calls, laptopAddress) {
		t.Fatalf("probe touched the excluded device: %+v", calls)
	}
}

func TestTempsPollerGatedEmptyScopeTouchesNothingAndStaysQuiet(t *testing.T) {
	var events []string
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{laptopAddress: "0x2204"})
	source := newFakeRegisters()
	poller := NewTempsPoller(TempsPollerOptions{
		Sysfs:    sysfs,
		Source:   source,
		SysfsDir: DefaultDRMDevicesDir,
		OnEvent:  func(line string) { events = append(events, line) },
	})
	if err := poller.PollOnce(context.Background(), PermittedScope(nil)); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if calls := source.callCount(); calls != 0 {
		t.Fatalf("register calls = %d, want 0 for an empty scope", calls)
	}
	if len(events) != 0 {
		t.Fatalf("events = %v, want none (the gate decided, not the hardware)", events)
	}
	if latest := poller.Latest(); len(latest) != 0 {
		t.Fatalf("latest = %+v, want no readings", latest)
	}
}

// TestSampleScopeKeepsPCIDomainsDistinct reproduces the fail-open case where a
// domain-less merge key let a permit for 0000:01:00.0 authorize 0001:01:00.0.
func TestSampleScopeKeepsPCIDomainsDistinct(t *testing.T) {
	scope := PermittedScope([]string{"0000:01:00.0"})
	if !scope.Allows("0000:01:00.0") {
		t.Fatal("scope excludes the permitted domain")
	}
	if scope.Allows("0001:01:00.0") {
		t.Fatal("scope permits the same bus/function in another PCI domain")
	}
	if !scope.Allows("00000000:01:00.0") {
		t.Fatal("scope did not match the nvidia-smi 8-digit-domain spelling")
	}

	// Two domains in the discovery tree: only the permitted one is touched.
	const otherDomain = "0001:01:00.0"
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{
		laptopAddress: "0x2204",
		otherDomain:   "0x2204",
	})
	source := newFakeRegisters()
	for _, address := range []string{laptopAddress, otherDomain} {
		source.set(DefaultDRMDevicesDir+"/"+address+"/resource0", 0x2046c, 60<<8)
	}
	sensors := ReadGpuRegisterTemps(context.Background(), source, sysfs, DefaultDRMDevicesDir,
		PermittedScope([]string{otherDomain}))
	if len(sensors) != 1 {
		t.Fatalf("sensors = %+v, want only the permitted domain", sensors)
	}
	for _, call := range source.callsSnapshot() {
		if strings.Contains(call.path, laptopAddress) {
			t.Fatalf("register call touched the excluded domain: %q", call.path)
		}
		if !strings.Contains(call.path, otherDomain) {
			t.Fatalf("register call path = %q, want the permitted domain", call.path)
		}
	}
}

func TestSampleScopeAllowsUngatedByDefault(t *testing.T) {
	scope := AllDevices()
	if scope.Gated() {
		t.Fatal("zero scope reports gated")
	}
	if !scope.Allows(laptopAddress) || !scope.Allows("not-a-bdf") {
		t.Fatal("ungated scope must allow everything")
	}
	gated := PermittedScope([]string{"0000:02:00.0"})
	if gated.Allows("not-a-bdf") {
		t.Fatal("gated scope allowed an unparseable address")
	}
	if !gated.Allows("00000000:02:00.0") {
		t.Fatal("gated scope did not match the nvidia-smi spelling")
	}
}
