package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

func unavailablePayload() MetricsPayload {
	return MetricsPayload{
		CPU:     Unavailable{Reason: "cpu"},
		Memory:  Unavailable{Reason: "memory"},
		Swap:    Unavailable{Reason: "swap"},
		GPUs:    Unavailable{Reason: "gpus"},
		Disks:   Unavailable{Reason: "disks"},
		Network: Unavailable{Reason: "network"},
	}
}

func samplesWith(payload MetricsPayload) SamplesMessage {
	return SamplesMessage{
		Mode: SampleModeLive,
		Records: []TelemetryRecord{
			MetricsRecord{
				RecordEnvelope: RecordEnvelope{
					CollectorID: "collector-a",
					RunID:       "run-1",
					BootID:      "boot-1",
					Sequence:    1,
					CapturedAt:  "2026-11-12T10:00:00.000Z",
				},
				Payload: payload,
			},
		},
	}
}

func TestEncoderRejectsTypedNilFamily(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MetricsPayload)
	}{
		{"cpu", func(p *MetricsPayload) { var cpu *CpuAvailable; p.CPU = cpu }},
		{"memory", func(p *MetricsPayload) { var memory *MemoryAvailable; p.Memory = memory }},
		{"swap", func(p *MetricsPayload) { var swap *BytesUsageAvailable; p.Swap = swap }},
		{"gpus", func(p *MetricsPayload) { var gpus *GpusAvailable; p.GPUs = gpus }},
		{"disks", func(p *MetricsPayload) { var disks *DisksAvailable; p.Disks = disks }},
		{"network", func(p *MetricsPayload) { var network *NetworkAvailable; p.Network = network }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := unavailablePayload()
			tc.mutate(&payload)
			if _, err := json.Marshal(payload); err == nil {
				t.Fatal("a typed-nil family must fail encoding instead of emitting null")
			}
			if _, err := EncodeCollectorMessage(samplesWith(payload)); err == nil {
				t.Fatal("the validated encoder must reject a typed-nil family")
			}
		})
	}
}

func TestEncoderRejectsNilListMembers(t *testing.T) {
	values := map[string]any{
		"gpus":        GpusAvailable{Devices: []GpuReading{nil}},
		"typed gpus":  GpusAvailable{Devices: []GpuReading{(*GpuAvailable)(nil)}},
		"filesystems": DisksAvailable{Filesystems: []FilesystemReading{nil}},
		"disks":       DisksAvailable{Devices: []DiskIoReading{nil}},
		"interfaces":  NetworkAvailable{Interfaces: []InterfaceReading{nil}},
		"records": SamplesMessage{
			Mode:    SampleModeLive,
			Records: []TelemetryRecord{nil},
		},
	}
	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			if _, err := json.Marshal(value); err == nil {
				t.Fatal("a nil list member must fail encoding instead of emitting null")
			}
		})
	}

	if _, err := EncodeCollectorMessage(samplesWith(unavailablePayload())); err != nil {
		t.Fatalf("a valid constructed message must encode: %v", err)
	}
}

func TestConstructedMessagesRoundTrip(t *testing.T) {
	messages := []CollectorMessage{
		constructedSamplesForTest(),
		constructedHelloForTest(),
	}
	for _, message := range messages {
		encoded, err := EncodeCollectorMessage(message)
		if err != nil {
			t.Fatalf("%T: %v", message, err)
		}
		decoded, err := DecodeCollectorMessage(encoded)
		if err != nil {
			t.Fatalf("%T: decode: %v", message, err)
		}
		if reflect.TypeOf(decoded) != reflect.TypeOf(message) {
			t.Fatalf("decoded %T, want %T", decoded, message)
		}
		reencoded, err := EncodeCollectorMessage(decoded)
		if err != nil {
			t.Fatalf("%T: re-encode: %v", message, err)
		}
		if string(encoded) != string(reencoded) {
			t.Fatalf("%T: encoding is not stable\nfirst:  %s\nsecond: %s", message, encoded, reencoded)
		}
	}

	ack := constructedAckForTest()
	encoded, err := EncodeHubMessage(ack)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeHubMessage(encoded); err != nil {
		t.Fatalf("ack encode/decode: %v", err)
	}
}

func constructedSamplesForTest() SamplesMessage {
	return SamplesMessage{
		Mode: SampleModeLive,
		Records: []TelemetryRecord{
			MetricsRecord{
				RecordEnvelope: RecordEnvelope{
					CollectorID: "collector-constructed",
					RunID:       "run-constructed",
					BootID:      "boot-constructed",
					Sequence:    1,
					CapturedAt:  "2026-11-12T10:00:00.500Z",
				},
				Payload: MetricsPayload{
					CPU: CpuAvailable{
						UsagePercent: 12.5,
						PerCore:      []*float64{float64Pointer(10), nil},
					},
					Memory: Unavailable{Reason: "not supported"},
					Swap:   BytesUsageAvailable{UsedBytes: 0, TotalBytes: 0},
					GPUs: GpusAvailable{Devices: []GpuReading{
						GpuAvailable{ID: "gpu:GPU-constructed", UtilizationPercent: float64Pointer(4)},
					}},
					Disks: DisksAvailable{
						Filesystems: []FilesystemReading{
							FilesystemAvailable{ID: "fs:/", UsedBytes: 1, TotalBytes: 2},
						},
						Devices: []DiskIoReading{},
					},
					Network: NetworkAvailable{Interfaces: []InterfaceReading{}},
				},
			},
		},
	}
}

func constructedHelloForTest() HelloMessage {
	return HelloMessage{
		CollectorID:        "collector-constructed-unicode",
		RunID:              "run-constructed-unicode",
		BootID:             "boot-constructed-unicode",
		Hostname:           "rigscope-\U0001F600",
		Capabilities:       []CapabilityName{CapabilityMetrics},
		ExpectedIntervalMS: 5_000,
	}
}

func constructedAckForTest() AckMessage {
	return AckMessage{
		Accepted:     []RecordRef{{RunID: "run-constructed", Sequence: 1}},
		Rejected:     []RejectedRecord{},
		RetryAfterMS: int64Pointer(100),
	}
}

func float64Pointer(v float64) *float64 { return &v }

func int64Pointer(v int64) *int64 { return &v }
