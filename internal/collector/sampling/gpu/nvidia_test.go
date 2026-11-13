package gpu

import "testing"

func TestParseGPUCSVNullTokensAndUnits(t *testing.T) {
	rows := ParseGPUCSV(readFixture(t, "nvidia/gpu-3090.csv"))
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	first := rows[0]
	if first.UUID != "GPU-aaaaaaaa-1111" || first.Name != "NVIDIA GeForce RTX 3090" {
		t.Fatalf("unexpected first row: %+v", first)
	}
	if first.PCIBusID == nil || *first.PCIBusID != "00000000:C1:00.0" {
		t.Fatalf("pci bus id = %v", first.PCIBusID)
	}
	requireReadingValue(t, "utilization", first.UtilizationPercent, 25)
	requireBytes(t, "memory used", first.MemoryUsedBytes, 15232*mib)
	requireBytes(t, "memory total", first.MemoryTotalBytes, 24576*mib)
	requireReadingValue(t, "temperature", first.TemperatureC, 30)
	requireReadingValue(t, "power", first.PowerWatts, 22.35)

	second := rows[1]
	if second.UtilizationPercent != nil {
		t.Fatalf("utilization = %v, want absent", *second.UtilizationPercent)
	}
	if second.MemoryUsedBytes != nil || second.MemoryTotalBytes != nil {
		t.Fatalf("memory = %v/%v, want absent", second.MemoryUsedBytes, second.MemoryTotalBytes)
	}
	requireReadingValue(t, "temperature", second.TemperatureC, 32)
	if second.PowerWatts != nil {
		t.Fatalf("power = %v, want absent", *second.PowerWatts)
	}

	if rows := ParseGPUCSV(""); len(rows) != 0 {
		t.Fatalf("empty output parsed %d rows", len(rows))
	}
	if rows := ParseGPUCSV("not,csv"); len(rows) != 0 {
		t.Fatalf("short output parsed %d rows", len(rows))
	}
}

func TestParseGPUCSVEdgeCases(t *testing.T) {
	rows := ParseGPUCSV(readFixture(t, "nvidia/gpu-edge.csv"))
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (duplicate UUID and junk lines dropped): %+v", len(rows), rows)
	}
	trimmed := rows[1]
	if trimmed.UUID != "GPU-aaaaaaaa-1111" {
		t.Fatalf("trimmed row uuid = %q", trimmed.UUID)
	}
	if trimmed.Name != "NVIDIA GeForce RTX 3090" {
		t.Fatalf("trimmed name = %q", trimmed.Name)
	}
	requireReadingValue(t, "trimmed utilization", trimmed.UtilizationPercent, 99)
	requireBytes(t, "trimmed memory used", trimmed.MemoryUsedBytes, mib)
	requireBytes(t, "trimmed memory total", trimmed.MemoryTotalBytes, 2*mib)
	requireReadingValue(t, "trimmed temperature", trimmed.TemperatureC, 70)
	requireReadingValue(t, "trimmed power", trimmed.PowerWatts, 350.5)

	defaulted := rows[2]
	if defaulted.UUID != "GPU-dddddddd-4444" || defaulted.Name != "NVIDIA GPU" {
		t.Fatalf("defaulted row = %+v", defaulted)
	}
	if defaulted.PCIBusID != nil || defaulted.UtilizationPercent != nil ||
		defaulted.MemoryUsedBytes != nil || defaulted.MemoryTotalBytes != nil ||
		defaulted.TemperatureC != nil || defaulted.PowerWatts != nil {
		t.Fatalf("all-absent row produced values: %+v", defaulted)
	}

	unknown := rows[3]
	if unknown.UtilizationPercent != nil || unknown.MemoryTotalBytes != nil || unknown.PowerWatts != nil {
		t.Fatalf("unknown-error row produced values: %+v", unknown)
	}
	requireReadingValue(t, "unknown temperature", unknown.TemperatureC, 61.0)
}

func TestParseComputeAppsCSV(t *testing.T) {
	apps := ParseComputeAppsCSV(readFixture(t, "nvidia/compute-apps.csv"))
	if len(apps) != 3 {
		t.Fatalf("apps = %d, want 3: %+v", len(apps), apps)
	}
	want := []ParsedComputeApp{
		{PID: 2_662_911, GPUUUID: "GPU-aaaaaaaa-1111", UsedBytes: 15222 * mib},
		{PID: 2_662_908, GPUUUID: "GPU-bbbbbbbb-2222", UsedBytes: 15126 * mib},
		{PID: 2_662_911, GPUUUID: "GPU-bbbbbbbb-2222", UsedBytes: 256 * mib},
	}
	for index, expected := range want {
		if apps[index] != expected {
			t.Fatalf("app[%d] = %+v, want %+v", index, apps[index], expected)
		}
	}
}
