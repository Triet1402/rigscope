package gpu

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestNormalizeBusFunctionKey(t *testing.T) {
	cases := []struct {
		input string
		want  string
		ok    bool
	}{
		{"00000000:C1:00.0", "c1:00.0", true},
		{"0000:01:00.0", "01:00.0", true},
		{" 0000:01:00.0 ", "01:00.0", true},
		{"0000:1a:00.1", "1a:00.1", true},
		{"01:00.0", "01:00.0", true},
		{"", "", false},
		{"not-a-bdf", "", false},
		{"0000:01:00.9", "", false},
	}
	for _, testCase := range cases {
		got, ok := NormalizeBusFunctionKey(testCase.input)
		if ok != testCase.ok || got != testCase.want {
			t.Fatalf("NormalizeBusFunctionKey(%q) = (%q, %v), want (%q, %v)",
				testCase.input, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestCanonicalPCIAddress(t *testing.T) {
	cases := []struct {
		input string
		want  string
		ok    bool
	}{
		{"0000:01:00.0", "0000:01:00.0", true},
		{" 00000000:01:00.0 ", "0000:01:00.0", true},
		{"0001:01:00.0", "0001:01:00.0", true},
		{"00000001:1A:00.1", "0001:1a:00.1", true},
		{"01:00.0", "0000:01:00.0", true},
		{"0000:01:00.9", "", false},
		{"0000:100:00.0", "", false},
		{"10000:01:00.0", "", false},
		{"not-a-bdf", "", false},
		{"", "", false},
	}
	for _, testCase := range cases {
		got, ok := CanonicalPCIAddress(testCase.input)
		if ok != testCase.ok || got != testCase.want {
			t.Fatalf("CanonicalPCIAddress(%q) = (%q, %v), want (%q, %v)",
				testCase.input, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestSelectSensorProfile(t *testing.T) {
	if got := SelectSensorProfile(0x2204); got != ProfileGDDR6 {
		t.Fatalf("RTX 3090 profile = %q, want gddr6", got)
	}
	for _, blackwell := range []uint32{0x2b85, 0x2c02, 0x2d19, 0x2d59} {
		if got := SelectSensorProfile(blackwell); got != ProfileGDDR7 {
			t.Fatalf("device 0x%04x profile = %q, want gddr7", blackwell, got)
		}
	}
}

func TestDecodeGddr6Junction(t *testing.T) {
	cases := []struct {
		raw  *uint32
		want *float64
	}{
		{pointer(uint32(0x3400)), pointer(52.0)},
		{pointer(uint32(0x0)), nil},
		{pointer(uint32(0x1)), nil},
		{pointer(uint32(0x7f00)), nil},
		{pointer(uint32(0xffffffff)), nil},
		{nil, nil},
	}
	for _, testCase := range cases {
		requireOptionalFloat(t, "junction", DecodeGDDR6Junction(testCase.raw), testCase.want)
	}
}

func TestDecodeGddr6VRAM(t *testing.T) {
	cases := []struct {
		raw  *uint32
		want *float64
	}{
		{pointer(uint32(0x900)), pointer(72.0)},
		{pointer(uint32(0x170)), pointer(11.5)},
		{pointer(uint32(0x0)), nil},
		{pointer(uint32(0xfff)), nil},
		{pointer(uint32(0xffffffff)), nil},
		{nil, nil},
	}
	for _, testCase := range cases {
		requireOptionalFloat(t, "vram", DecodeGDDR6VRAM(testCase.raw), testCase.want)
	}
}

func TestDecodeBlackwell(t *testing.T) {
	cases := []struct {
		raw  *uint32
		want *float64
	}{
		{pointer(uint32(68 * 256)), pointer(68.0)},
		{pointer(uint32(0x2200)), pointer(34.0)},
		{pointer(uint32(0x1180)), pointer(17.5)},
		{pointer(uint32(0x0)), nil},
		{pointer(uint32(151 * 256)), nil},
		{pointer(uint32(0xffffffff)), nil},
		{nil, nil},
	}
	for _, testCase := range cases {
		requireOptionalFloat(t, "blackwell", DecodeBlackwell(testCase.raw), testCase.want)
	}
}

func TestDecodeGddr7(t *testing.T) {
	cases := []struct {
		raw  *uint32
		want *float64
	}{
		{pointer(uint32(56 << 16)), pointer(72.0)},
		{pointer(uint32(21 << 16)), pointer(2.0)},
		{pointer(uint32(80 << 16)), pointer(120.0)},
		{pointer(uint32(20 << 16)), nil},
		{pointer(uint32(81 << 16)), nil},
		{pointer(uint32(0xbadf0000 | (56 << 16))), nil},
		{pointer(uint32(0xffffffff)), nil},
		{nil, nil},
	}
	for _, testCase := range cases {
		requireOptionalFloat(t, "gddr7", DecodeGDDR7(testCase.raw), testCase.want)
	}
	if !IsGDDR7Poison(0xbadf0001) || IsGDDR7Poison(56<<16) {
		t.Fatal("IsGDDR7Poison misidentifies the poison pattern")
	}
}

func displayDeviceSysfs(files map[string]string, devices map[string]string) *memSysFS {
	// devices maps bdf -> device id; vendor/class are the 3D-controller
	// defaults of the fixture set.
	for bdf, deviceID := range devices {
		files["/sys/bus/pci/devices/"+bdf+"/vendor"] = "0x10de\n"
		files["/sys/bus/pci/devices/"+bdf+"/class"] = "0x030200\n"
		files["/sys/bus/pci/devices/"+bdf+"/device"] = deviceID + "\n"
	}
	return newMemSysFS(files)
}

func TestDiscoverNvidiaDisplays(t *testing.T) {
	devices := DiscoverNvidiaDisplays(
		RootFS{FS: os.DirFS(testdataDir + "/sysfs/nvidia-mixed")},
		DefaultDRMDevicesDir,
	)
	if len(devices) != 1 {
		t.Fatalf("devices = %+v, want only the NVIDIA display function", devices)
	}
	if devices[0].BDF != "0000:03:00.0" || devices[0].DeviceID != 0x2b85 {
		t.Fatalf("device = %+v", devices[0])
	}
}

func TestReadGpuRegisterTempsDecodesGDDR6AndGDDR7(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{
		"0000:c1:00.0": "0x2204",
		"0000:c2:00.0": "0x2b85",
	})
	source := newFakeRegisters()

	r6 := DefaultDRMDevicesDir + "/0000:c1:00.0/resource0"
	source.set(r6, 0x2046c, 52<<8)
	source.set(r6, 0xe2a8, 72*32)

	r7 := DefaultDRMDevicesDir + "/0000:c2:00.0/resource0"
	source.set(r7, 0xad0a90, 61*256)
	source.set(r7, 0xad0a94, 68*256)
	source.set(r7, 0xad0aa0, 65*256)
	source.set(r7, 0x9024d0, 1<<24)
	source.set(r7, 0x9024c0, 56<<16)
	source.set(r7, 0x9064d0, 0xbadf0001)

	sensors := ReadGpuRegisterTemps(context.Background(), source, sysfs, DefaultDRMDevicesDir, AllDevices())
	if len(sensors) != 2 {
		t.Fatalf("sensors = %+v, want two devices", sensors)
	}
	gddr6, ok := sensors["c1:00.0"]
	if !ok {
		t.Fatal("missing c1:00.0")
	}
	requireReadingValue(t, "gddr6 junction", gddr6.JunctionTemperatureC, 52)
	requireReadingValue(t, "gddr6 vram", gddr6.VRAMTemperatureC, 72)

	gddr7, ok := sensors["c2:00.0"]
	if !ok {
		t.Fatal("missing c2:00.0")
	}
	requireReadingValue(t, "gddr7 junction", gddr7.JunctionTemperatureC, 68)
	requireReadingValue(t, "gddr7 vram", gddr7.VRAMTemperatureC, 72)
}

func TestReadGpuRegisterTempsUnreadableStaysAbsent(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{"0000:c1:00.0": "0x2204"})
	source := newFakeRegisters()
	if sensors := ReadGpuRegisterTemps(context.Background(), source, sysfs, DefaultDRMDevicesDir, AllDevices()); len(sensors) != 0 {
		t.Fatalf("sensors = %+v, want none", sensors)
	}
}

func TestReadGpuRegisterTempsAllZeroIsNotZeroDegrees(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{"0000:c1:00.0": "0x2204"})
	source := newFakeRegisters()
	r6 := DefaultDRMDevicesDir + "/0000:c1:00.0/resource0"
	source.set(r6, 0x2046c, 0)
	source.set(r6, 0xe2a8, 0)
	if sensors := ReadGpuRegisterTemps(context.Background(), source, sysfs, DefaultDRMDevicesDir, AllDevices()); len(sensors) != 0 {
		t.Fatalf("sensors = %+v, want none (unsupported card)", sensors)
	}
}

func TestReadGpuRegisterTempsBatchesGDDR7IntoAtMostTwoReads(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{"0000:c2:00.0": "0x2b85"})
	source := newFakeRegisters()
	resource := DefaultDRMDevicesDir + "/0000:c2:00.0/resource0"
	source.set(resource, 0xad0a90, 61*256)
	source.set(resource, 0xad0a94, 68*256)
	source.set(resource, 0xad0a98, 62*256)
	source.set(resource, 0xad0a9c, 63*256)
	source.set(resource, 0xad0aa0, 65*256)
	source.set(resource, 0x9024d0, (1<<24)|(1<<25))
	source.set(resource, 0x9024c0, 56<<16)
	source.set(resource, 0x9024c4, 60<<16)
	source.set(resource, 0x9064d0, 0xbadf0001)

	sensors := ReadGpuRegisterTemps(context.Background(), source, sysfs, DefaultDRMDevicesDir, AllDevices())
	values, ok := sensors["c2:00.0"]
	if !ok {
		t.Fatalf("sensors = %+v", sensors)
	}
	requireReadingValue(t, "junction", values.JunctionTemperatureC, 68)
	requireReadingValue(t, "vram", values.VRAMTemperatureC, 80)

	calls := source.callsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("register calls = %d, want 2", len(calls))
	}
	for _, call := range calls {
		if call.path != resource {
			t.Fatalf("call path = %q, want %q", call.path, resource)
		}
	}
}

func TestTempsPollerReportsStateChangesOnce(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{"0000:c1:00.0": "0x2204"})
	source := newFakeRegisters()
	var events []string
	poller := NewTempsPoller(TempsPollerOptions{
		Sysfs:   sysfs,
		Source:  source,
		OnEvent: func(line string) { events = append(events, line) },
	})

	// No register access: hint appears exactly once across repeated cycles.
	if err := poller.PollOnce(context.Background(), AllDevices()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if err := poller.PollOnce(context.Background(), AllDevices()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(events) != 1 || !strings.Contains(events[0], "mapping failed") {
		t.Fatalf("events = %v, want one BAR0 mapping hint", events)
	}

	// Access granted: readings appear and the healthy line logs exactly once.
	r6 := DefaultDRMDevicesDir + "/0000:c1:00.0/resource0"
	source.set(r6, 0x2046c, 60<<8)
	_ = poller.PollOnce(context.Background(), AllDevices())
	_ = poller.PollOnce(context.Background(), AllDevices())
	latest := poller.Latest()
	values, ok := latest["c1:00.0"]
	if !ok {
		t.Fatalf("latest = %+v", latest)
	}
	requireReadingValue(t, "junction", values.JunctionTemperatureC, 60)
	if values.VRAMTemperatureC != nil {
		t.Fatalf("vram = %v, want absent", *values.VRAMTemperatureC)
	}
	if len(events) != 2 || events[1] != "gpu register temps: 1 device(s)" {
		t.Fatalf("events = %v", events)
	}

	// Losing access re-arms the hint.
	source.clear(r6, 0x2046c)
	_ = poller.PollOnce(context.Background(), AllDevices())
	if len(events) != 3 || !strings.Contains(events[2], "mapping failed") {
		t.Fatalf("events = %v", events)
	}
}

func TestTempsPollerUnsupportedRegistersAreNotAccessFailures(t *testing.T) {
	sysfs := displayDeviceSysfs(map[string]string{}, map[string]string{"0000:c2:00.0": "0x2b85"})
	source := newFakeRegisters()
	source.zeroAll = true
	var events []string
	poller := NewTempsPoller(TempsPollerOptions{
		Sysfs:   sysfs,
		Source:  source,
		OnEvent: func(line string) { events = append(events, line) },
	})
	_ = poller.PollOnce(context.Background(), AllDevices())
	_ = poller.PollOnce(context.Background(), AllDevices())
	if len(events) != 1 || events[0] != "gpu register temps: no supported thermal registers on 1 device(s)" {
		t.Fatalf("events = %v", events)
	}
}

func TestTempsPollerWithoutNvidiaDevices(t *testing.T) {
	var events []string
	poller := NewTempsPoller(TempsPollerOptions{
		Sysfs:   newMemSysFS(map[string]string{}),
		Source:  newFakeRegisters(),
		OnEvent: func(line string) { events = append(events, line) },
	})
	_ = poller.PollOnce(context.Background(), AllDevices())
	if len(events) != 1 || events[0] != "gpu register temps: no NVIDIA display devices" {
		t.Fatalf("events = %v", events)
	}
}

func TestTempsEnabled(t *testing.T) {
	if !TempsEnabled(nil) {
		t.Fatal("nil getenv should leave register temps enabled")
	}
	cases := map[string]bool{
		"":      true,
		" on ":  true,
		"off":   false,
		" OFF ": false,
		"auto":  true,
	}
	for value, want := range cases {
		if got := TempsEnabled(func(string) string { return value }); got != want {
			t.Fatalf("TempsEnabled(%q) = %v, want %v", value, got, want)
		}
	}
}
