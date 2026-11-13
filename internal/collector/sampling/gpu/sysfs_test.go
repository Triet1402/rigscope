package gpu

import (
	"os"
	"testing"
)

const amdAddress = "0000:03:00.0"
const amdBase = "/sys/bus/pci/devices/" + amdAddress

func amdFixture(t *testing.T, name string) RootFS {
	t.Helper()
	return RootFS{FS: os.DirFS(testdataDir + "/sysfs/" + name)}
}

func requireUnavailable(t *testing.T, reading GpuReading, reason string) {
	t.Helper()
	if reading.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable (%+v)", reading.Status, reading)
	}
	if reason != "" && reading.Reason != reason {
		t.Fatalf("reason = %q, want %q", reading.Reason, reason)
	}
}

func TestReadSysfsGpuAMDUnitsAndIdentity(t *testing.T) {
	reading := ReadSysfsGpu(amdFixture(t, "amd-580"), amdAddress)
	if reading.Status != StatusOK {
		t.Fatalf("status = %q, want ok (%+v)", reading.Status, reading)
	}
	if reading.ID != "pci:"+amdAddress {
		t.Fatalf("id = %q", reading.ID)
	}
	requireReadingValue(t, "utilization", reading.UtilizationPercent, 37)
	requireBytes(t, "memory total", reading.MemoryTotalBytes, 8_589_934_592)
	requireBytes(t, "memory used", reading.MemoryUsedBytes, 1_073_741_824)
	requireReadingValue(t, "temperature", reading.TemperatureC, 61.5)
	requireReadingValue(t, "power", reading.PowerWatts, 21.5)
}

func TestReadSysfsGpuIntegratedVRAMStaysAbsent(t *testing.T) {
	reading := ReadSysfsGpu(amdFixture(t, "amd-integrated"), amdAddress)
	if reading.Status != StatusOK {
		t.Fatalf("status = %q, want ok (%+v)", reading.Status, reading)
	}
	// A zero busy counter is a real reading, not a missing one.
	requireReadingValue(t, "utilization", reading.UtilizationPercent, 0)
	if reading.MemoryTotalBytes != nil {
		t.Fatalf("memory total = %v, want absent", *reading.MemoryTotalBytes)
	}
	if reading.MemoryUsedBytes != nil {
		t.Fatalf("memory used = %v, want absent", *reading.MemoryUsedBytes)
	}
	requireReadingValue(t, "temperature", reading.TemperatureC, 45)
}

func TestReadSysfsGpuIntelPartialReadingNeverFabricates(t *testing.T) {
	sysfs := loadFixtureTree(t, "sysfs/amd-580")
	sysfs.set(amdBase+"/vendor", "0x8086\n")
	for _, name := range []string{"gpu_busy_percent", "mem_info_vram_total", "mem_info_vram_used"} {
		sysfs.delete(amdBase + "/" + name)
	}
	reading := ReadSysfsGpu(sysfs, amdAddress)
	if reading.Status != StatusOK {
		t.Fatalf("status = %q, want ok (%+v)", reading.Status, reading)
	}
	if reading.UtilizationPercent != nil || reading.MemoryTotalBytes != nil || reading.MemoryUsedBytes != nil {
		t.Fatalf("fabricated missing counters: %+v", reading)
	}
	requireReadingValue(t, "temperature", reading.TemperatureC, 61.5)

	sysfs.delete(amdBase + "/hwmon/hwmon4/temp1_input")
	sysfs.delete(amdBase + "/hwmon/hwmon4/power1_average")
	requireUnavailable(t, ReadSysfsGpu(sysfs, amdAddress),
		"GPU detected, but no readable DRM/sysfs counters; driver support or permissions may limit telemetry")
}

func TestReadSysfsGpuMalformedCountersStayUnavailable(t *testing.T) {
	sysfs := loadFixtureTree(t, "sysfs/amd-580")
	sysfs.set(amdBase+"/gpu_busy_percent", "101\n")
	sysfs.set(amdBase+"/mem_info_vram_total", "-1\n")
	sysfs.set(amdBase+"/hwmon/hwmon4/temp1_input", "NaN\n")
	sysfs.set(amdBase+"/hwmon/hwmon4/power1_average", "\n")
	requireUnavailable(t, ReadSysfsGpu(sysfs, amdAddress), "")

	sysfs.delete(amdBase + "/vendor")
	requireUnavailable(t, ReadSysfsGpu(sysfs, amdAddress), "no supported DRM telemetry")
}

func TestReadSysfsGpuPowerFallsBackToInput(t *testing.T) {
	sysfs := loadFixtureTree(t, "sysfs/amd-580")
	sysfs.delete(amdBase + "/hwmon/hwmon4/power1_average")
	sysfs.set(amdBase+"/hwmon/hwmon4/power1_input", "9000000\n")
	reading := ReadSysfsGpu(sysfs, amdAddress)
	requireReadingValue(t, "power", reading.PowerWatts, 9)
}

func TestReadSysfsGpuUsesFirstHwmonReading(t *testing.T) {
	sysfs := loadFixtureTree(t, "sysfs/amd-580")
	sysfs.set(amdBase+"/hwmon/hwmon0/temp1_input", "70000\n")
	sysfs.set(amdBase+"/hwmon/hwmon0/power1_average", "10000000\n")
	reading := ReadSysfsGpu(sysfs, amdAddress)
	// hwmon0 sorts before hwmon4, so its values win.
	requireReadingValue(t, "temperature", reading.TemperatureC, 70)
	requireReadingValue(t, "power", reading.PowerWatts, 10)
}
