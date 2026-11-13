package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, kind, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureRoot, kind, name+".json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func TestVersion(t *testing.T) {
	if Version != 1 {
		t.Fatalf("Version = %d, want 1", Version)
	}
}

func TestHelloProtocolIsOptional(t *testing.T) {
	minimal, err := DecodeCollectorMessage(readFixture(t, "valid", "collector-hello-minimal"))
	if err != nil {
		t.Fatal(err)
	}
	hello, ok := minimal.(HelloMessage)
	if !ok {
		t.Fatalf("expected HelloMessage, got %T", minimal)
	}
	if hello.Protocol != nil {
		t.Fatalf("protocol should be absent, got %d", *hello.Protocol)
	}
	encoded, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("protocol")) {
		t.Fatalf("absent protocol must not be encoded: %s", encoded)
	}

	versioned, err := DecodeCollectorMessage(readFixture(t, "valid", "collector-hello-protocol"))
	if err != nil {
		t.Fatal(err)
	}
	hello, ok = versioned.(HelloMessage)
	if !ok || hello.Protocol == nil || *hello.Protocol != 1 {
		t.Fatalf("expected protocol 1, got %#v", versioned)
	}
}

func TestWelcomeProtocolIsOptional(t *testing.T) {
	plain, err := DecodeHubMessage(readFixture(t, "valid", "hub-welcome-durable"))
	if err != nil {
		t.Fatal(err)
	}
	if welcome := plain.(WelcomeMessage); welcome.Protocol != nil {
		t.Fatal("welcome protocol should be absent in the unversioned fixture")
	}
	versioned, err := DecodeHubMessage(readFixture(t, "valid", "hub-welcome-memory-protocol"))
	if err != nil {
		t.Fatal(err)
	}
	if welcome := versioned.(WelcomeMessage); welcome.Protocol == nil || *welcome.Protocol != 1 {
		t.Fatalf("expected welcome protocol 1, got %#v", versioned)
	}
}

func TestUnknownFieldErrorNamesPath(t *testing.T) {
	_, err := DecodeCollectorMessage(readFixture(t, "invalid", "collector-samples-cpu-unknown-field"))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"samples.records[0].payload.cpu", `unexpected field "clockMhz"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestMissingFieldErrorNamesPath(t *testing.T) {
	_, err := DecodeCollectorMessage(readFixture(t, "invalid", "collector-samples-cpu-missing-per-core"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), `samples.records[0].payload.cpu: missing field "perCore"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAbsentOptionalStaysAbsent(t *testing.T) {
	message, err := DecodeCollectorMessage(readFixture(t, "valid", "collector-samples-unavailable"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	// Every family is unavailable, so no reading fields may be invented.
	for _, field := range []string{"usagePercent", "usedBytes", "temperatureC", "devices", "interfaces"} {
		if bytes.Contains(encoded, []byte(`"`+field+`"`)) {
			t.Fatalf("unavailable payload must not contain %q: %s", field, encoded)
		}
	}
}

func TestExplicitNullOptionalPreserved(t *testing.T) {
	message, err := DecodeCollectorMessage(readFixture(t, "valid", "collector-samples-explicit-null"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"temperatureC":null`,
		`"pressureSomePercent":null`,
		`"inodesUsed":null`,
		`"readLatencyMs":null`,
	} {
		if !bytes.Contains(encoded, []byte(field)) {
			t.Fatalf("explicit null %s was dropped: %s", field, encoded)
		}
	}
}

func padded(data []byte, size int) []byte {
	if len(data) >= size {
		return data
	}
	return append(append([]byte{}, data...), bytes.Repeat([]byte(" "), size-len(data))...)
}

func TestMessageSizeLimits(t *testing.T) {
	hello := readFixture(t, "valid", "collector-hello-minimal")

	tooLarge := padded(hello, MaxMessageBytes+1)
	if _, err := DecodeCollectorMessage(tooLarge); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("a hello over MaxMessageBytes must be rejected, got %v", err)
	}

	detail := readFixture(t, "valid", "collector-detail-result")
	paddedDetail := padded(detail, MaxMessageBytes+1)
	if len(paddedDetail) > MaxMessageBytes {
		if _, err := DecodeCollectorMessage(paddedDetail); err != nil {
			t.Fatalf("a detail.result may exceed MaxMessageBytes up to MaxDetailBytes: %v", err)
		}
	}

	if _, err := DecodeCollectorMessage(padded(detail, MaxDetailBytes+1)); err == nil ||
		!strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("a detail.result over MaxDetailBytes must be rejected, got %v", err)
	}

	welcome := readFixture(t, "valid", "hub-welcome-durable")
	if _, err := DecodeHubMessage(padded(welcome, MaxMessageBytes+1)); err == nil ||
		!strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("a hub message over MaxMessageBytes must be rejected, got %v", err)
	}
}

func TestTrailingDataRejected(t *testing.T) {
	hello := readFixture(t, "valid", "collector-hello-minimal")
	data := append(append([]byte{}, hello...), []byte(`{"type":"hello"}`)...)
	if _, err := DecodeCollectorMessage(data); err == nil {
		t.Fatal("expected trailing data to be rejected")
	}
}

func TestIntegralNumbers(t *testing.T) {
	record, err := ParseTelemetryRecord([]byte(`{
		"collectorId":"collector-a","runId":"run-1","bootId":"boot-123",
		"sequence":1.0,"capturedAt":"2026-11-12T10:00:00.000Z","kind":"gap",
		"payload":{"fromCapturedAt":"2026-11-12T09:59:00.000Z","toCapturedAt":"2026-11-12T10:00:00.000Z","droppedRecords":2e3,"reason":"rejected"}
	}`))
	if err != nil {
		t.Fatalf("integral float notation should be accepted like Number.isSafeInteger: %v", err)
	}
	gap := record.(GapRecord)
	if gap.Sequence != 1 || gap.Payload.DroppedRecords != 2000 {
		t.Fatalf("unexpected decoded values: %#v", gap)
	}

	if _, err := ParseTelemetryRecord([]byte(`{
		"collectorId":"collector-a","runId":"run-1","bootId":"boot-123",
		"sequence":1.5,"capturedAt":"2026-11-12T10:00:00.000Z","kind":"gap",
		"payload":{"fromCapturedAt":"2026-11-12T09:59:00.000Z","toCapturedAt":"2026-11-12T10:00:00.000Z","droppedRecords":1,"reason":"rejected"}
	}`)); err == nil {
		t.Fatal("a fractional sequence must be rejected")
	}
}

func TestMarshalForcesDiscriminatorAndArrays(t *testing.T) {
	encoded, err := json.Marshal(CpuAvailable{UsagePercent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"status":"ok"`)) {
		t.Fatalf("cpu status not forced: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte(`"perCore":[]`)) {
		t.Fatalf("nil perCore must encode as an empty array: %s", encoded)
	}

	encoded, err = json.Marshal(GpusAvailable{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"devices":[]`)) || !bytes.Contains(encoded, []byte(`"status":"ok"`)) {
		t.Fatalf("unexpected gpus encoding: %s", encoded)
	}
}

func TestUnavailableVariants(t *testing.T) {
	payload := MetricsPayload{
		CPU:     Unavailable{Reason: "a"},
		Memory:  Unavailable{Reason: "b"},
		Swap:    Unavailable{Reason: "c"},
		GPUs:    Unavailable{Reason: "d"},
		Disks:   Unavailable{Reason: "e"},
		Network: Unavailable{Reason: "f"},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"status":"ok"`)) {
		t.Fatalf("unavailable families must not emit status ok: %s", encoded)
	}
	roundTripped, err := ParseMetricsPayload(encoded)
	if err != nil {
		t.Fatalf("re-parse unavailable payload: %v", err)
	}
	for family, metric := range map[string]MetricFamily{
		"cpu": FamilyCPU, "memory": FamilyMemory, "swap": FamilySwap,
		"gpus": FamilyGPUs, "disks": FamilyDisks, "network": FamilyNetwork,
	} {
		var got any
		switch metric {
		case FamilyCPU:
			got = roundTripped.CPU
		case FamilyMemory:
			got = roundTripped.Memory
		case FamilySwap:
			got = roundTripped.Swap
		case FamilyGPUs:
			got = roundTripped.GPUs
		case FamilyDisks:
			got = roundTripped.Disks
		case FamilyNetwork:
			got = roundTripped.Network
		}
		if _, ok := got.(Unavailable); !ok {
			t.Fatalf("%s should decode as Unavailable, got %T", family, got)
		}
	}
}

func TestMetricsPayloadRejectsMissingFamily(t *testing.T) {
	payload := MetricsPayload{
		CPU:     Unavailable{Reason: "a"},
		Memory:  Unavailable{Reason: "b"},
		Swap:    Unavailable{Reason: "c"},
		GPUs:    Unavailable{Reason: "d"},
		Disks:   Unavailable{Reason: "e"},
		Network: nil,
	}
	if _, err := json.Marshal(payload); err == nil {
		t.Fatal("a nil family must fail encoding instead of emitting null")
	}
}

func TestUnmarshalJSONIsStrict(t *testing.T) {
	data := readFixture(t, "invalid", "collector-hello-unknown-field")
	var hello HelloMessage
	if err := json.Unmarshal(data, &hello); err == nil {
		t.Fatal("expected strict UnmarshalJSON to reject an unknown field")
	}
	if err := json.Unmarshal(readFixture(t, "valid", "collector-hello-protocol"), &hello); err != nil {
		t.Fatalf("valid hello: %v", err)
	}
	if hello.Protocol == nil || *hello.Protocol != 1 {
		t.Fatalf("protocol not decoded: %#v", hello)
	}
}

func TestNullableFieldsEncodeAsNull(t *testing.T) {
	encoded, err := json.Marshal(GpuAvailable{ID: "gpu:GPU-0001"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"utilizationPercent":null`,
		`"memoryUsedBytes":null`,
		`"memoryTotalBytes":null`,
		`"temperatureC":null`,
		`"powerWatts":null`,
	} {
		if !bytes.Contains(encoded, []byte(field)) {
			t.Fatalf("required nullable %s missing from %s", field, encoded)
		}
	}
	if bytes.Contains(encoded, []byte("junctionTemperatureC")) {
		t.Fatalf("absent optional field was encoded: %s", encoded)
	}
}

func TestResourceUnavailableWireForms(t *testing.T) {
	reading := ResourceUnavailable{ID: "disk:sdb", Reason: "removed"}
	encoded, err := json.Marshal(reading)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"disk:sdb","status":"unavailable","reason":"removed"}`
	if string(encoded) != want {
		t.Fatalf("resource unavailable encoding = %s, want %s", encoded, want)
	}
	var decoded ResourceUnavailable
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != reading {
		t.Fatalf("resource unavailable decode = %#v", decoded)
	}
}

func TestDeliveryQueueModeAbsent(t *testing.T) {
	message, err := DecodeCollectorMessage(readFixture(t, "valid", "collector-status-no-queue-mode"))
	if err != nil {
		t.Fatal(err)
	}
	status := message.(StatusMessage)
	if status.Delivery.QueueMode != nil {
		t.Fatalf("queueMode should be absent, got %q", *status.Delivery.QueueMode)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("queueMode")) {
		t.Fatalf("absent queueMode must not be encoded: %s", encoded)
	}
}

func TestRecordKinds(t *testing.T) {
	var record TelemetryRecord = MetricsRecord{}
	if record.Kind() != RecordKindMetrics {
		t.Fatalf("metrics record kind = %q", record.Kind())
	}
	record = GapRecord{}
	if record.Kind() != RecordKindGap {
		t.Fatalf("gap record kind = %q", record.Kind())
	}
}

func TestOptionalWireForms(t *testing.T) {
	type holder struct {
		Absent *Optional[float64] `json:"absent,omitempty"`
		Null   *Optional[float64] `json:"null,omitempty"`
		Value  *Optional[float64] `json:"value,omitempty"`
	}
	encoded, err := json.Marshal(holder{Null: Null[float64](), Value: Some(1.5)})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"null":null,"value":1.5}`
	if string(encoded) != want {
		t.Fatalf("optional encoding = %s, want %s", encoded, want)
	}
	// encoding/json represents a present null pointer field as nil, so the
	// decoder path (DecodeCollectorMessage) fills Optional explicitly. Direct
	// Optional decoding is available for standalone use.
	var nullOptional Optional[float64]
	if err := json.Unmarshal([]byte("null"), &nullOptional); err != nil {
		t.Fatal(err)
	}
	if nullOptional.Valid {
		t.Fatal("explicit null should decode as Valid == false")
	}
	var valueOptional Optional[float64]
	if err := json.Unmarshal([]byte("1.5"), &valueOptional); err != nil {
		t.Fatal(err)
	}
	if !valueOptional.Valid || valueOptional.Value != 1.5 {
		t.Fatalf("unexpected optional decode: %#v", valueOptional)
	}
}

func TestHubMessageRejectsCollectorType(t *testing.T) {
	data := readFixture(t, "invalid", "hub-unknown-type")
	if _, err := DecodeHubMessage(data); err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDecodeRejectsNonObject(t *testing.T) {
	if _, err := DecodeCollectorMessage([]byte(`"hello"`)); err == nil ||
		!strings.Contains(err.Error(), "expected object") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := DecodeHubMessage([]byte(`[]`)); err == nil ||
		!strings.Contains(err.Error(), "expected object") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTimestampWindow(t *testing.T) {
	base := `{"collectorId":"collector-a","runId":"run-1","bootId":"boot-123","sequence":1,"capturedAt":%q,"kind":"gap","payload":{"fromCapturedAt":"2026-11-12T09:59:00.000Z","toCapturedAt":"2026-11-12T10:00:00.000Z","droppedRecords":1,"reason":"rejected"}}`
	for _, captured := range []string{"2000-01-01T00:00:00Z", "2199-12-31T23:59:59Z"} {
		if _, err := ParseTelemetryRecord([]byte(strings.Replace(base, "%q", `"`+captured+`"`, 1))); err != nil {
			t.Fatalf("%s should be inside the timestamp window: %v", captured, err)
		}
	}
	for _, captured := range []string{"1999-12-31T23:59:59Z", "2200-01-01T00:00:01Z", "not-a-date"} {
		if _, err := ParseTelemetryRecord([]byte(strings.Replace(base, "%q", `"`+captured+`"`, 1))); err == nil {
			t.Fatalf("%s should be rejected", captured)
		}
	}
}
