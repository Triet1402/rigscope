package protocol

import "encoding/json"

// RecordEnvelope is the identity every telemetry record carries.
// (collectorId, runId, sequence) is unique per collector; sequence increases
// across record kinds within a run and may skip values.
type RecordEnvelope struct {
	CollectorID string `json:"collectorId"`
	RunID       string `json:"runId"`
	BootID      string `json:"bootId"`
	Sequence    int64  `json:"sequence"`
	CapturedAt  string `json:"capturedAt"`
}

// TelemetryRecord is a metrics or gap record.
type TelemetryRecord interface {
	isTelemetryRecord()
	// Kind reports the record discriminator.
	Kind() RecordKind
}

// MetricsRecord is one periodic metrics sample.
type MetricsRecord struct {
	RecordEnvelope
	Payload MetricsPayload `json:"payload"`
}

func (MetricsRecord) isTelemetryRecord() {}
func (MetricsRecord) Kind() RecordKind   { return RecordKindMetrics }

// MarshalJSON emits kind "metrics".
func (r MetricsRecord) MarshalJSON() ([]byte, error) {
	type plain struct {
		RecordEnvelope
		Kind    RecordKind     `json:"kind"`
		Payload MetricsPayload `json:"payload"`
	}
	return json.Marshal(plain{r.RecordEnvelope, RecordKindMetrics, r.Payload})
}

// GapPayload reports an interval whose records were dropped.
type GapPayload struct {
	// FromCapturedAt is the capture time of the first dropped record (inclusive).
	FromCapturedAt string `json:"fromCapturedAt"`
	// ToCapturedAt is the capture time of the last dropped record (inclusive).
	ToCapturedAt   string    `json:"toCapturedAt"`
	DroppedRecords int64     `json:"droppedRecords"`
	Reason         GapReason `json:"reason"`
}

// GapRecord is one gap notice. It is acknowledged like a sample.
type GapRecord struct {
	RecordEnvelope
	Payload GapPayload `json:"payload"`
}

func (GapRecord) isTelemetryRecord() {}
func (GapRecord) Kind() RecordKind   { return RecordKindGap }

// MarshalJSON emits kind "gap".
func (r GapRecord) MarshalJSON() ([]byte, error) {
	type plain struct {
		RecordEnvelope
		Kind    RecordKind `json:"kind"`
		Payload GapPayload `json:"payload"`
	}
	return json.Marshal(plain{r.RecordEnvelope, RecordKindGap, r.Payload})
}

// RecordRef identifies one record in an ack.
type RecordRef struct {
	RunID    string `json:"runId"`
	Sequence int64  `json:"sequence"`
}

// RejectedRecord is an ack rejection with a reason.
type RejectedRecord struct {
	RecordRef
	Reason string `json:"reason"`
}

// CollectionFailure is one metric collection failure reported in status.
type CollectionFailure struct {
	Metric     string  `json:"metric"`
	ResourceID *string `json:"resourceId"`
	Reason     string  `json:"reason"`
}

// DeliveryStatus is the collector's delivery queue state.
type DeliveryStatus struct {
	// QueueMode is optional so a collector predating the field still parses.
	QueueMode               *QueueMode `json:"queueMode,omitempty"`
	PendingRecords          int64      `json:"pendingRecords"`
	PendingBytes            int64      `json:"pendingBytes"`
	OldestPendingCapturedAt *string    `json:"oldestPendingCapturedAt"`
	InFlightRecords         int64      `json:"inFlightRecords"`
	DroppedRecords          int64      `json:"droppedRecords"`
	Replaying               bool       `json:"replaying"`
	LastAckAt               *string    `json:"lastAckAt"`
}

// ProcessRow is one process in a detail snapshot.
type ProcessRow struct {
	PID  int64 `json:"pid"`
	PPID int64 `json:"ppid"`
	// StartTicks is the kernel start time in clock ticks since boot; with PID it
	// identifies a process.
	StartTicks int64   `json:"startTicks"`
	Name       string  `json:"name"`
	User       *string `json:"user"`
	State      string  `json:"state"`
	// CPUPercent may exceed 100 on multi-core hosts (100 = one full core).
	CPUPercent         *float64 `json:"cpuPercent"`
	MemoryRSSBytes     *int64   `json:"memoryRssBytes"`
	GpuMemoryBytes     *int64   `json:"gpuMemoryBytes"`
	IOReadBytesPerSec  *float64 `json:"ioReadBytesPerSec"`
	IOWriteBytesPerSec *float64 `json:"ioWriteBytesPerSec"`
	Threads            *int64   `json:"threads"`
	// Unit is the systemd unit/scope from the process cgroup, when known.
	Unit *string `json:"unit"`
	// Command is present only when the destination's policy allows arguments.
	Command *string `json:"command,omitempty"`
}

// ProcessSnapshot is one process table snapshot.
type ProcessSnapshot struct {
	TotalProcesses int64        `json:"totalProcesses"`
	Truncated      bool         `json:"truncated"`
	Inaccessible   int64        `json:"inaccessible"`
	Processes      []ProcessRow `json:"processes"`
}

// MarshalJSON keeps processes an array instead of null.
func (s ProcessSnapshot) MarshalJSON() ([]byte, error) {
	if s.Processes == nil {
		s.Processes = []ProcessRow{}
	}
	type plain ProcessSnapshot
	return json.Marshal(plain(s))
}
