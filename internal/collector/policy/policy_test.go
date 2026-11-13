package policy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/protocol"
)

func boolPtr(value bool) *bool { return &value }

func TestDefaultWithholdsAddressesAndArguments(t *testing.T) {
	policy := Default()
	if !policy.CPU || !policy.Memory || !policy.Swap || !policy.GPUs || !policy.Disks || !policy.Network {
		t.Fatalf("metric families must be shared by default: %#v", policy)
	}
	if !policy.Processes {
		t.Fatal("process visibility must be shared by default")
	}
	if policy.NetworkAddresses {
		t.Fatal("network addresses must be withheld by default")
	}
	if policy.ProcessArguments {
		t.Fatal("process arguments must be withheld by default")
	}
}

func TestNormalizeSwapFollowsMemoryAndExplicitValuesWin(t *testing.T) {
	legacy := Normalize(&Input{CPU: boolPtr(true), Memory: boolPtr(false)})
	if legacy.Swap {
		t.Fatal("swap must follow a withheld memory family")
	}
	if legacy.NetworkAddresses || legacy.ProcessArguments {
		t.Fatal("privacy-preserving defaults must survive normalization")
	}

	explicit := Normalize(&Input{
		CPU: boolPtr(false), Memory: boolPtr(true), Swap: boolPtr(false),
		ProcessArguments: boolPtr(true), NetworkAddresses: boolPtr(true),
	})
	if explicit.CPU || explicit.Swap {
		t.Fatalf("explicit false ignored: %#v", explicit)
	}
	if !explicit.ProcessArguments || !explicit.NetworkAddresses {
		t.Fatalf("explicit true ignored: %#v", explicit)
	}

	if Normalize(nil) != Default() {
		t.Fatal("nil input must normalize to the defaults")
	}
}

func TestApplyMetricsReplacesWithheldFamiliesWithoutMutatingTheSource(t *testing.T) {
	record := protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: "c", RunID: "r", BootID: "b", Sequence: 3,
			CapturedAt: "2026-11-12T10:00:00.000Z",
		},
		Payload: protocol.MetricsPayload{
			CPU:     protocol.CpuAvailable{UsagePercent: 10, PerCore: []*float64{}},
			Memory:  protocol.MemoryAvailable{UsedBytes: 1, TotalBytes: 2},
			Swap:    protocol.BytesUsageAvailable{UsedBytes: 0, TotalBytes: 0},
			GPUs:    protocol.GpusAvailable{Devices: []protocol.GpuReading{}},
			Disks:   protocol.DisksAvailable{Filesystems: []protocol.FilesystemReading{}, Devices: []protocol.DiskIoReading{}},
			Network: protocol.NetworkAvailable{Interfaces: []protocol.InterfaceReading{}},
		},
	}
	sharing := Policy{CPU: false, Memory: true, Swap: false, GPUs: true, Disks: false, Network: false}
	out := ApplyMetrics(record, sharing)
	if out.Sequence != 3 || out.CollectorID != "c" {
		t.Fatalf("envelope changed: %#v", out.RecordEnvelope)
	}
	for family, value := range map[protocol.MetricFamily]any{
		protocol.FamilyCPU: out.Payload.CPU, protocol.FamilySwap: out.Payload.Swap,
		protocol.FamilyDisks: out.Payload.Disks, protocol.FamilyNetwork: out.Payload.Network,
	} {
		unavailable, ok := value.(protocol.Unavailable)
		if !ok || unavailable.Reason != WithheldReason {
			t.Fatalf("%s = %#v, want withheld unavailable", family, value)
		}
	}
	if _, ok := out.Payload.Memory.(protocol.MemoryAvailable); !ok {
		t.Fatalf("memory = %#v, want the shared reading", out.Payload.Memory)
	}
	if _, ok := record.Payload.CPU.(protocol.CpuAvailable); !ok {
		t.Fatal("source record was mutated")
	}
}

func sampleInventory() protocol.Inventory {
	model := "Fixture CPU"
	distro := "Fixture Linux"
	total := int64(1000)
	mac := "aa:bb:cc:dd:ee:ff"
	size := int64(4096)
	rotational := true
	speed := int64(1000)
	reason := "no telemetry"
	return protocol.Inventory{
		OS:     protocol.OSInventory{Platform: "linux", Release: "6.0", Distro: &distro, Arch: "amd64"},
		CPU:    protocol.CPUInventory{Model: &model, LogicalCores: 8},
		Memory: protocol.MemoryInventory{TotalBytes: &total, SwapTotalBytes: &total},
		GPUs: []protocol.GpuInventory{{
			ID: "gpu:GPU-0", Name: "GPU", Vendor: protocol.GpuVendorNvidia,
			PciBusID: protocol.Ptr("0000:01:00.0"), Telemetry: protocol.TelemetryUnsupported,
			MemoryTotalBytes: &size, Reason: &reason,
		}},
		Filesystems: []protocol.FilesystemInventory{{
			ID: "fs:/", MountPoint: "/", Device: "/dev/sda1", FsType: "ext4", TotalBytes: &total,
		}},
		Disks: []protocol.DiskInventory{{
			ID: "disk:sda", Name: "sda", Model: protocol.Ptr("Disk"), Rotational: &rotational, SizeBytes: &size,
		}},
		Network: []protocol.InterfaceInventory{{
			ID: "net:eth0", Name: "eth0", Mac: &mac,
			Addresses: []protocol.InterfaceAddress{{Family: protocol.AddressFamilyIPv4, Address: "192.0.2.10", Prefix: 24}},
			State:     protocol.InterfaceStateUp, SpeedMbps: &speed,
		}},
		Capabilities: protocol.Capabilities{
			CPU:       protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Memory:    protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Swap:      protocol.CapabilityState{Status: protocol.CapabilitySupported},
			GPUs:      protocol.CapabilityState{Status: protocol.CapabilityUnsupported, Reason: &reason},
			Disks:     protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Network:   protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Processes: protocol.CapabilityState{Status: protocol.CapabilitySupported},
		},
	}
}

func TestFilterInventoryWithholdsResourcesAndCapabilities(t *testing.T) {
	inventory := sampleInventory()
	original := sampleInventory()
	filtered := FilterInventory(inventory, Policy{
		CPU: false, Memory: true, Swap: false, GPUs: false,
		Disks: false, Network: true, NetworkAddresses: false, Processes: false,
	})
	if filtered.Capabilities.CPU.Status != protocol.CapabilityWithheld ||
		filtered.Capabilities.GPUs.Status != protocol.CapabilityWithheld ||
		filtered.Capabilities.Disks.Status != protocol.CapabilityWithheld ||
		filtered.Capabilities.Swap.Status != protocol.CapabilityWithheld ||
		filtered.Capabilities.Processes.Status != protocol.CapabilityWithheld {
		t.Fatalf("capabilities = %#v", filtered.Capabilities)
	}
	if filtered.Capabilities.Memory.Status != protocol.CapabilitySupported {
		t.Fatalf("memory capability = %#v, want supported", filtered.Capabilities.Memory)
	}
	if filtered.CPU.Model != nil || filtered.CPU.LogicalCores != 0 {
		t.Fatalf("cpu = %#v, want withheld", filtered.CPU)
	}
	if filtered.Memory.TotalBytes == nil || filtered.Memory.SwapTotalBytes != nil {
		t.Fatalf("memory = %#v, want total kept and swap withheld", filtered.Memory)
	}
	if len(filtered.GPUs) != 0 || len(filtered.Filesystems) != 0 || len(filtered.Disks) != 0 {
		t.Fatalf("withheld resources remain: %#v", filtered)
	}
	if len(filtered.Network) != 1 || len(filtered.Network[0].Addresses) != 0 {
		t.Fatalf("network = %#v, want one interface without addresses", filtered.Network)
	}
	if filtered.Network[0].Mac == nil || *filtered.Network[0].Mac != "aa:bb:cc:dd:ee:ff" {
		t.Fatal("address withholding must not remove the MAC")
	}

	// The source value (including its pointers) must be untouched.
	if inventory.CPU.Model == nil || *inventory.CPU.Model != *original.CPU.Model {
		t.Fatal("source inventory was mutated")
	}
	if len(inventory.Network[0].Addresses) != 1 {
		t.Fatal("source addresses were mutated")
	}
	if inventory.Network[0].Mac == original.Network[0].Mac && filtered.Network[0].Mac == inventory.Network[0].Mac {
		t.Fatal("filtered inventory must own independent pointer values")
	}
}

func TestFilterInventorySharesAddressesWhenPolicyAllows(t *testing.T) {
	sharing := Default()
	sharing.NetworkAddresses = true
	filtered := FilterInventory(sampleInventory(), sharing)
	if filtered.Network[0].Addresses[0].Address != "192.0.2.10" {
		t.Fatalf("addresses = %#v, want shared", filtered.Network[0].Addresses)
	}
	if filtered.Capabilities.CPU.Status != protocol.CapabilitySupported {
		t.Fatalf("cpu capability = %#v, want supported", filtered.Capabilities.CPU)
	}
}

func TestApplyDetailStripsArgumentsAndRefusesWithheldProcesses(t *testing.T) {
	command := "worker --token secret"
	result := protocol.DetailResult{
		SubscriptionID: "sub-1",
		CapturedAt:     "2026-11-12T10:00:00.000Z",
		Snapshot: protocol.ProcessSnapshot{
			TotalProcesses: 1,
			Processes:      []protocol.ProcessRow{{PID: 42, Name: "worker", State: "S", Command: &command}},
		},
	}
	shared, ok := ApplyDetail(result, Policy{Processes: true, ProcessArguments: false})
	if !ok {
		t.Fatal("processes shared but the result was dropped")
	}
	if shared.Snapshot.Processes[0].Command != nil {
		t.Fatal("command leaked through a withheld-arguments policy")
	}
	if result.Snapshot.Processes[0].Command == nil {
		t.Fatal("source result was mutated")
	}

	withCommands, ok := ApplyDetail(result, Policy{Processes: true, ProcessArguments: true})
	if !ok || withCommands.Snapshot.Processes[0].Command == nil {
		t.Fatal("allowed arguments were stripped")
	}

	if _, ok := ApplyDetail(result, Policy{Processes: false}); ok {
		t.Fatal("a withheld process policy must drop the result")
	}
}

func TestSanitizeHandlesQueuedRecordsAndMessages(t *testing.T) {
	metricsRecord := protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{CollectorID: "c", RunID: "r", Sequence: 1, CapturedAt: "2026-11-12T10:00:00.000Z"},
		Payload: protocol.MetricsPayload{
			CPU:     protocol.CpuAvailable{UsagePercent: 1, PerCore: []*float64{}},
			Memory:  protocol.MemoryAvailable{UsedBytes: 1, TotalBytes: 2},
			Swap:    protocol.BytesUsageAvailable{UsedBytes: 0, TotalBytes: 0},
			GPUs:    protocol.GpusAvailable{Devices: []protocol.GpuReading{}},
			Disks:   protocol.DisksAvailable{Filesystems: []protocol.FilesystemReading{}, Devices: []protocol.DiskIoReading{}},
			Network: protocol.NetworkAvailable{Interfaces: []protocol.InterfaceReading{}},
		},
	}
	sharing := Policy{Memory: true}
	record, ok := SanitizeTelemetryRecord(metricsRecord, sharing)
	if !ok {
		t.Fatal("metrics record dropped")
	}
	sanitized, ok := record.(protocol.MetricsRecord)
	if !ok {
		t.Fatal("metrics record changed type")
	}
	if _, isUnavailable := sanitized.Payload.CPU.(protocol.Unavailable); !isUnavailable {
		t.Fatal("queued metrics record was not sanitized")
	}
	gap := protocol.GapRecord{
		RecordEnvelope: metricsRecord.RecordEnvelope,
		Payload:        protocol.GapPayload{DroppedRecords: 2, Reason: protocol.GapWriteFailure, FromCapturedAt: metricsRecord.CapturedAt, ToCapturedAt: metricsRecord.CapturedAt},
	}
	if _, ok := SanitizeTelemetryRecord(gap, sharing); !ok {
		t.Fatal("gap record dropped")
	}

	samples := protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{metricsRecord, gap}}
	message, ok := SanitizeCollectorMessage(samples, sharing)
	if !ok {
		t.Fatal("samples message dropped")
	}
	sanitizedSamples := message.(protocol.SamplesMessage)
	if _, isUnavailable := sanitizedSamples.Records[0].(protocol.MetricsRecord).Payload.CPU.(protocol.Unavailable); !isUnavailable {
		t.Fatal("queued batch was not sanitized")
	}

	inventoryMessage := protocol.InventoryMessage{CapturedAt: metricsRecord.CapturedAt, Inventory: sampleInventory()}
	message, ok = SanitizeCollectorMessage(inventoryMessage, Policy{Network: true})
	if !ok {
		t.Fatal("inventory message dropped")
	}
	if message.(protocol.InventoryMessage).Inventory.Capabilities.CPU.Status != protocol.CapabilityWithheld {
		t.Fatal("queued inventory was not sanitized")
	}

	if _, ok := SanitizeCollectorMessage(protocol.DetailResult{SubscriptionID: "sub-1"}, Policy{Processes: false}); ok {
		t.Fatal("a queued detail result must be dropped when processes are withheld")
	}
}

func TestProcessesPolicyExposesSchedulerView(t *testing.T) {
	view := ProcessesPolicy(Policy{Processes: true, ProcessArguments: true})
	if !view.Processes || !view.ProcessArguments {
		t.Fatalf("process policy = %#v", view)
	}
}

func sensitiveMetricsRecord() protocol.MetricsRecord {
	return protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{CollectorID: "c", RunID: "r", Sequence: 1, CapturedAt: "2026-11-12T10:00:00.000Z"},
		Payload: protocol.MetricsPayload{
			CPU:     protocol.CpuAvailable{UsagePercent: 1, PerCore: []*float64{}},
			Memory:  protocol.MemoryAvailable{UsedBytes: 1, TotalBytes: 2},
			Swap:    protocol.BytesUsageAvailable{UsedBytes: 0, TotalBytes: 0},
			GPUs:    protocol.GpusAvailable{Devices: []protocol.GpuReading{}},
			Disks:   protocol.DisksAvailable{Filesystems: []protocol.FilesystemReading{protocol.ResourceUnavailable{ID: "fs:/secret", Reason: "unreadable"}}, Devices: []protocol.DiskIoReading{}},
			Network: protocol.NetworkAvailable{Interfaces: []protocol.InterfaceReading{}},
		},
	}
}

func TestSanitizeTelemetryRecordHandlesPointerForms(t *testing.T) {
	sharing := Policy{Memory: true}
	record := sensitiveMetricsRecord()

	value, ok := SanitizeTelemetryRecord(record, sharing)
	if !ok {
		t.Fatal("value metrics record dropped")
	}
	if _, ok := value.(protocol.MetricsRecord); !ok {
		t.Fatalf("value form became %T", value)
	}

	pointer, ok := SanitizeTelemetryRecord(&record, sharing)
	if !ok {
		t.Fatal("pointer metrics record dropped")
	}
	sanitized, ok := pointer.(protocol.MetricsRecord)
	if !ok {
		t.Fatalf("pointer form was not normalized, got %T", pointer)
	}
	if _, withheldCPU := sanitized.Payload.CPU.(protocol.Unavailable); !withheldCPU {
		t.Fatal("pointer metrics record was not sanitized")
	}
	if _, ok := sanitized.Payload.Disks.(protocol.Unavailable); !ok {
		t.Fatal("pointer disk family was not sanitized")
	}
	if _, ok := record.Payload.CPU.(protocol.CpuAvailable); !ok {
		t.Fatal("pointer input was mutated")
	}

	gap := protocol.GapRecord{RecordEnvelope: record.RecordEnvelope}
	if _, ok := SanitizeTelemetryRecord(&gap, sharing); !ok {
		t.Fatal("pointer gap record dropped")
	}

	var nilMetrics *protocol.MetricsRecord
	if _, ok := SanitizeTelemetryRecord(nilMetrics, sharing); ok {
		t.Fatal("typed-nil metrics record was accepted")
	}
	var nilGap *protocol.GapRecord
	if _, ok := SanitizeTelemetryRecord(nilGap, sharing); ok {
		t.Fatal("typed-nil gap record was accepted")
	}
}

func TestSanitizeCollectorMessageHandlesSensitivePointerPayloads(t *testing.T) {
	sharing := Default()
	sharing.GPUs = false
	sharing.NetworkAddresses = false
	sharing.ProcessArguments = false

	record := sensitiveMetricsRecord()
	samples := protocol.SamplesMessage{Mode: protocol.SampleModeLive, Records: []protocol.TelemetryRecord{&record, protocol.GapRecord{RecordEnvelope: record.RecordEnvelope}}}
	for name, message := range map[string]protocol.CollectorMessage{
		"value samples":   samples,
		"pointer samples": &samples,
	} {
		sanitized, ok := SanitizeCollectorMessage(message, sharing)
		if !ok {
			t.Fatalf("%s dropped", name)
		}
		batch, ok := sanitized.(protocol.SamplesMessage)
		if !ok {
			t.Fatalf("%s normalized to %T, want a value", name, sanitized)
		}
		metrics, ok := batch.Records[0].(protocol.MetricsRecord)
		if !ok {
			t.Fatalf("%s record = %T, want a value", name, batch.Records[0])
		}
		if _, withheld := metrics.Payload.GPUs.(protocol.Unavailable); !withheld {
			t.Fatalf("%s gpUs family leaked", name)
		}
	}

	inventory := protocol.InventoryMessage{CapturedAt: record.CapturedAt, Inventory: sampleInventory()}
	for name, message := range map[string]protocol.CollectorMessage{
		"value inventory":   inventory,
		"pointer inventory": &inventory,
	} {
		sanitized, ok := SanitizeCollectorMessage(message, sharing)
		if !ok {
			t.Fatalf("%s dropped", name)
		}
		filtered, ok := sanitized.(protocol.InventoryMessage)
		if !ok {
			t.Fatalf("%s normalized to %T, want a value", name, sanitized)
		}
		if filtered.Inventory.Capabilities.GPUs.Status != protocol.CapabilityWithheld {
			t.Fatalf("%s gpU capability = %#v", name, filtered.Inventory.Capabilities.GPUs)
		}
		if len(filtered.Inventory.Network) != 1 || len(filtered.Inventory.Network[0].Addresses) != 0 {
			t.Fatalf("%s addresses leaked: %#v", name, filtered.Inventory.Network)
		}
	}
	if inventory.Inventory.Capabilities.GPUs.Status != protocol.CapabilityUnsupported ||
		len(inventory.Inventory.Network[0].Addresses) != 1 {
		t.Fatal("sanitizing a pointer inventory message mutated its source")
	}

	command := "worker --token secret"
	detail := protocol.DetailResult{
		SubscriptionID: "sub-1",
		CapturedAt:     record.CapturedAt,
		Snapshot: protocol.ProcessSnapshot{
			Processes: []protocol.ProcessRow{{PID: 42, Name: "worker", State: "S", Command: &command}},
		},
	}
	for name, message := range map[string]protocol.CollectorMessage{
		"value detail":   detail,
		"pointer detail": &detail,
	} {
		sanitized, ok := SanitizeCollectorMessage(message, sharing)
		if !ok {
			t.Fatalf("%s dropped", name)
		}
		shared, ok := sanitized.(protocol.DetailResult)
		if !ok {
			t.Fatalf("%s normalized to %T, want a value", name, sanitized)
		}
		if shared.Snapshot.Processes[0].Command != nil {
			t.Fatalf("%s command leaked", name)
		}
	}

	withheld := Default()
	withheld.Processes = false
	if _, ok := SanitizeCollectorMessage(&detail, withheld); ok {
		t.Fatal("pointer detail survived a withheld process policy")
	}
}

func TestSanitizeCollectorMessageCoversEveryMessageImplementation(t *testing.T) {
	record := sensitiveMetricsRecord()
	messages := []protocol.CollectorMessage{
		protocol.HelloMessage{},
		&protocol.HelloMessage{},
		protocol.SamplesMessage{Mode: protocol.SampleModeLive},
		&protocol.SamplesMessage{Mode: protocol.SampleModeLive},
		protocol.InventoryMessage{},
		&protocol.InventoryMessage{},
		protocol.StatusMessage{},
		&protocol.StatusMessage{},
		protocol.DetailResult{},
		&protocol.DetailResult{},
		protocol.DetailEnded{},
		&protocol.DetailEnded{},
	}
	for _, message := range messages {
		sanitized, ok := SanitizeCollectorMessage(message, Default())
		if !ok {
			t.Fatalf("%T was dropped by a sharing policy that shares everything", message)
		}
		if reflect.TypeOf(sanitized).Kind() == reflect.Ptr {
			t.Fatalf("%T was not normalized to a value", message)
		}
	}

	for _, record := range []protocol.TelemetryRecord{
		record, &record,
		protocol.GapRecord{}, &protocol.GapRecord{},
	} {
		sanitized, ok := SanitizeTelemetryRecord(record, Default())
		if !ok {
			t.Fatalf("%T was dropped by a sharing policy that shares everything", record)
		}
		switch sanitized.(type) {
		case protocol.MetricsRecord, protocol.GapRecord:
		default:
			t.Fatalf("%T normalized to %T, want a value", record, sanitized)
		}
	}
}

func TestFilterFailuresDropsWithheldFamiliesAndRedactsAddresses(t *testing.T) {
	resource := "fs:/secret/data"
	address := "192.0.2.10"
	failures := []protocol.CollectionFailure{
		{Metric: "disks", ResourceID: &resource, Reason: "statfs failed for fs:/secret/data"},
		{Metric: "disks", ResourceID: protocol.Ptr("disk:sda"), Reason: "counter read failed"},
		{Metric: "network", ResourceID: protocol.Ptr("net:eth0"), Reason: "probe to " + address + " timed out"},
		{Metric: "cpu", Reason: "cpu counters unavailable"},
		{Metric: "future-family", ResourceID: protocol.Ptr("other:1"), Reason: "unknown family"},
	}
	sharing := Default()
	sharing.Disks = false
	sharing.NetworkAddresses = false
	filtered := FilterFailures(failures, sharing)

	if len(filtered) != 3 {
		t.Fatalf("failures = %#v, want disk failures dropped", filtered)
	}
	for _, failure := range filtered {
		if failure.Metric == "disks" {
			t.Fatalf("disk failure survived: %#v", failure)
		}
		if failure.Metric == "network" {
			if failure.Reason == "probe to "+address+" timed out" {
				t.Fatalf("address leaked: %q", failure.Reason)
			}
			if !strings.Contains(failure.Reason, withheldAddressText) {
				t.Fatalf("reason = %q, want a redaction marker", failure.Reason)
			}
		}
	}
	if filtered[len(filtered)-1].Metric != "future-family" {
		t.Fatalf("an unknown family must survive: %#v", filtered)
	}

	// The source values and their pointers stay untouched.
	if *failures[1].ResourceID != "disk:sda" || failures[2].Reason != "probe to "+address+" timed out" {
		t.Fatal("FilterFailures mutated its input")
	}

	networkOff := Default()
	networkOff.Network = false
	for _, failure := range FilterFailures(failures, networkOff) {
		if failure.Metric == "network" {
			t.Fatalf("network failure survived a withheld network family: %#v", failure)
		}
	}
}

func TestSanitizeStatusMessageFiltersQueuedFailures(t *testing.T) {
	networkAddress := "10.0.0.7"
	message := protocol.StatusMessage{
		CapturedAt: "2026-11-12T10:00:00.000Z",
		Failures: []protocol.CollectionFailure{
			{Metric: "disks", ResourceID: protocol.Ptr("fs:/secret"), Reason: "mount reader failed"},
			{Metric: "network", ResourceID: protocol.Ptr("net:eth0"), Reason: "no route to " + networkAddress},
			{Metric: "gpus", Reason: "gpu telemetry unavailable"},
		},
	}
	sharing := Default()
	sharing.Disks = false
	sharing.GPUs = false
	sharing.NetworkAddresses = false

	for name, input := range map[string]protocol.CollectorMessage{
		"value status":   message,
		"pointer status": &message,
	} {
		sanitized, ok := SanitizeCollectorMessage(input, sharing)
		if !ok {
			t.Fatalf("%s dropped", name)
		}
		status, ok := sanitized.(protocol.StatusMessage)
		if !ok {
			t.Fatalf("%s normalized to %T, want a value", name, sanitized)
		}
		if len(status.Failures) != 1 || status.Failures[0].Metric != "network" {
			t.Fatalf("%s failures = %#v", name, status.Failures)
		}
		if strings.Contains(status.Failures[0].Reason, networkAddress) {
			t.Fatalf("%s address leaked: %q", name, status.Failures[0].Reason)
		}
	}
	if len(message.Failures) != 3 {
		t.Fatal("sanitizing a pointer status message mutated its source")
	}
}
