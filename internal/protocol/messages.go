package protocol

import "encoding/json"

// CollectorMessage is a message sent collector -> hub.
type CollectorMessage interface{ isCollectorMessage() }

// HelloMessage opens a collector session.
type HelloMessage struct {
	CollectorID        string           `json:"collectorId"`
	RunID              string           `json:"runId"`
	BootID             string           `json:"bootId"`
	Hostname           string           `json:"hostname"`
	Capabilities       []CapabilityName `json:"capabilities"`
	ExpectedIntervalMS int64            `json:"expectedIntervalMs"`
	// Protocol is the optional wire protocol version. Absent for collectors
	// that predate versioning.
	Protocol *int64 `json:"protocol,omitempty"`
}

func (HelloMessage) isCollectorMessage() {}

// MarshalJSON emits type "hello".
func (m HelloMessage) MarshalJSON() ([]byte, error) {
	caps := m.Capabilities
	if caps == nil {
		caps = []CapabilityName{}
	}
	type plain struct {
		Type               string           `json:"type"`
		CollectorID        string           `json:"collectorId"`
		RunID              string           `json:"runId"`
		BootID             string           `json:"bootId"`
		Hostname           string           `json:"hostname"`
		Capabilities       []CapabilityName `json:"capabilities"`
		ExpectedIntervalMS int64            `json:"expectedIntervalMs"`
		Protocol           *int64           `json:"protocol,omitempty"`
	}
	return json.Marshal(plain{"hello", m.CollectorID, m.RunID, m.BootID, m.Hostname, caps, m.ExpectedIntervalMS, m.Protocol})
}

// UnmarshalJSON strictly decodes and validates a hello.
func (m *HelloMessage) UnmarshalJSON(data []byte) error {
	out, err := decodeStrict(data, "hello", (*parser).parseHello)
	if err != nil {
		return err
	}
	*m = out
	return nil
}

// SamplesMessage carries a bounded batch of telemetry records.
type SamplesMessage struct {
	Mode    SampleMode        `json:"mode"`
	Records []TelemetryRecord `json:"records"`
}

func (SamplesMessage) isCollectorMessage() {}

// MarshalJSON emits type "samples". It rejects nil and typed-nil records
// instead of emitting JSON null.
func (m SamplesMessage) MarshalJSON() ([]byte, error) {
	records := m.Records
	if records == nil {
		records = []TelemetryRecord{}
	}
	if err := checkNoNilElements("records", records); err != nil {
		return nil, err
	}
	type plain struct {
		Type    string            `json:"type"`
		Mode    SampleMode        `json:"mode"`
		Records []TelemetryRecord `json:"records"`
	}
	return json.Marshal(plain{"samples", m.Mode, records})
}

// UnmarshalJSON strictly decodes and validates a samples batch.
func (m *SamplesMessage) UnmarshalJSON(data []byte) error {
	out, err := decodeStrict(data, "samples", (*parser).parseSamples)
	if err != nil {
		return err
	}
	*m = out
	return nil
}

// InventoryMessage carries the machine inventory.
type InventoryMessage struct {
	CapturedAt string    `json:"capturedAt"`
	Inventory  Inventory `json:"inventory"`
}

func (InventoryMessage) isCollectorMessage() {}

// MarshalJSON emits type "inventory".
func (m InventoryMessage) MarshalJSON() ([]byte, error) {
	type plain struct {
		Type       string    `json:"type"`
		CapturedAt string    `json:"capturedAt"`
		Inventory  Inventory `json:"inventory"`
	}
	return json.Marshal(plain{"inventory", m.CapturedAt, m.Inventory})
}

// UnmarshalJSON strictly decodes and validates an inventory message.
func (m *InventoryMessage) UnmarshalJSON(data []byte) error {
	out, err := decodeStrict(data, "inventory", (*parser).parseInventoryMessage)
	if err != nil {
		return err
	}
	*m = out
	return nil
}

// StatusMessage reports delivery state and collection failures.
type StatusMessage struct {
	CapturedAt string              `json:"capturedAt"`
	Delivery   DeliveryStatus      `json:"delivery"`
	Failures   []CollectionFailure `json:"failures"`
}

func (StatusMessage) isCollectorMessage() {}

// MarshalJSON emits type "status".
func (m StatusMessage) MarshalJSON() ([]byte, error) {
	failures := m.Failures
	if failures == nil {
		failures = []CollectionFailure{}
	}
	type plain struct {
		Type       string              `json:"type"`
		CapturedAt string              `json:"capturedAt"`
		Delivery   DeliveryStatus      `json:"delivery"`
		Failures   []CollectionFailure `json:"failures"`
	}
	return json.Marshal(plain{"status", m.CapturedAt, m.Delivery, failures})
}

// UnmarshalJSON strictly decodes and validates a status message.
func (m *StatusMessage) UnmarshalJSON(data []byte) error {
	out, err := decodeStrict(data, "status", (*parser).parseStatusMessage)
	if err != nil {
		return err
	}
	*m = out
	return nil
}

// DetailResult is one subscription's process snapshot.
type DetailResult struct {
	SubscriptionID string          `json:"subscriptionId"`
	CapturedAt     string          `json:"capturedAt"`
	Snapshot       ProcessSnapshot `json:"snapshot"`
}

func (DetailResult) isCollectorMessage() {}

// MarshalJSON emits type "detail.result".
func (m DetailResult) MarshalJSON() ([]byte, error) {
	type plain struct {
		Type           string          `json:"type"`
		SubscriptionID string          `json:"subscriptionId"`
		CapturedAt     string          `json:"capturedAt"`
		Snapshot       ProcessSnapshot `json:"snapshot"`
	}
	return json.Marshal(plain{"detail.result", m.SubscriptionID, m.CapturedAt, m.Snapshot})
}

// UnmarshalJSON strictly decodes and validates a detail result.
func (m *DetailResult) UnmarshalJSON(data []byte) error {
	out, err := decodeStrict(data, "detail.result", (*parser).parseDetailResult)
	if err != nil {
		return err
	}
	*m = out
	return nil
}

// DetailEnded closes a detail subscription.
type DetailEnded struct {
	SubscriptionID string            `json:"subscriptionId"`
	Reason         DetailEndedReason `json:"reason"`
}

func (DetailEnded) isCollectorMessage() {}

// MarshalJSON emits type "detail.ended".
func (m DetailEnded) MarshalJSON() ([]byte, error) {
	type plain struct {
		Type           string            `json:"type"`
		SubscriptionID string            `json:"subscriptionId"`
		Reason         DetailEndedReason `json:"reason"`
	}
	return json.Marshal(plain{"detail.ended", m.SubscriptionID, m.Reason})
}

// UnmarshalJSON strictly decodes and validates a detail-ended message.
func (m *DetailEnded) UnmarshalJSON(data []byte) error {
	out, err := decodeStrict(data, "detail.ended", (*parser).parseDetailEnded)
	if err != nil {
		return err
	}
	*m = out
	return nil
}

// HubMessage is a message sent hub -> collector.
type HubMessage interface{ isHubMessage() }

// WelcomeMessage answers a hello and fixes the session delivery contract.
type WelcomeMessage struct {
	HubRunID           string       `json:"hubRunId"`
	MachineID          string       `json:"machineId"`
	Delivery           DeliveryMode `json:"delivery"`
	MaxInFlightBatches int64        `json:"maxInFlightBatches"`
	// Protocol is the optional wire protocol version the hub accepted.
	Protocol *int64 `json:"protocol,omitempty"`
}

func (WelcomeMessage) isHubMessage() {}

// MarshalJSON emits type "welcome".
func (m WelcomeMessage) MarshalJSON() ([]byte, error) {
	type plain struct {
		Type               string       `json:"type"`
		HubRunID           string       `json:"hubRunId"`
		MachineID          string       `json:"machineId"`
		Delivery           DeliveryMode `json:"delivery"`
		MaxInFlightBatches int64        `json:"maxInFlightBatches"`
		Protocol           *int64       `json:"protocol,omitempty"`
	}
	return json.Marshal(plain{"welcome", m.HubRunID, m.MachineID, m.Delivery, m.MaxInFlightBatches, m.Protocol})
}

// UnmarshalJSON strictly decodes and validates a welcome.
func (m *WelcomeMessage) UnmarshalJSON(data []byte) error {
	out, err := decodeStrict(data, "welcome", (*parser).parseWelcome)
	if err != nil {
		return err
	}
	*m = out
	return nil
}

// AckMessage acknowledges accepted and permanently rejected records.
type AckMessage struct {
	Accepted []RecordRef      `json:"accepted"`
	Rejected []RejectedRecord `json:"rejected"`
	// RetryAfterMS is set when the hub could not accept the batch now.
	RetryAfterMS *int64 `json:"retryAfterMs,omitempty"`
}

func (AckMessage) isHubMessage() {}

// MarshalJSON emits type "ack".
func (m AckMessage) MarshalJSON() ([]byte, error) {
	accepted := m.Accepted
	if accepted == nil {
		accepted = []RecordRef{}
	}
	rejected := m.Rejected
	if rejected == nil {
		rejected = []RejectedRecord{}
	}
	type plain struct {
		Type         string           `json:"type"`
		Accepted     []RecordRef      `json:"accepted"`
		Rejected     []RejectedRecord `json:"rejected"`
		RetryAfterMS *int64           `json:"retryAfterMs,omitempty"`
	}
	return json.Marshal(plain{"ack", accepted, rejected, m.RetryAfterMS})
}

// UnmarshalJSON strictly decodes and validates an ack.
func (m *AckMessage) UnmarshalJSON(data []byte) error {
	out, err := decodeStrict(data, "ack", (*parser).parseAck)
	if err != nil {
		return err
	}
	*m = out
	return nil
}

// DetailSubscribe asks for or renews a detail subscription. Re-sending an
// active id renews its lease.
type DetailSubscribe struct {
	SubscriptionID string     `json:"subscriptionId"`
	Kind           DetailKind `json:"kind"`
	IntervalMS     int64      `json:"intervalMs"`
	LeaseMS        int64      `json:"leaseMs"`
}

func (DetailSubscribe) isHubMessage() {}

// MarshalJSON emits type "detail.subscribe".
func (m DetailSubscribe) MarshalJSON() ([]byte, error) {
	kind := m.Kind
	if kind == "" {
		kind = DetailKindProcesses
	}
	type plain struct {
		Type           string     `json:"type"`
		SubscriptionID string     `json:"subscriptionId"`
		Kind           DetailKind `json:"kind"`
		IntervalMS     int64      `json:"intervalMs"`
		LeaseMS        int64      `json:"leaseMs"`
	}
	return json.Marshal(plain{"detail.subscribe", m.SubscriptionID, kind, m.IntervalMS, m.LeaseMS})
}

// UnmarshalJSON strictly decodes and validates a detail subscription.
func (m *DetailSubscribe) UnmarshalJSON(data []byte) error {
	out, err := decodeStrict(data, "detail.subscribe", (*parser).parseDetailSubscribe)
	if err != nil {
		return err
	}
	*m = out
	return nil
}

// DetailCancel cancels a detail subscription.
type DetailCancel struct {
	SubscriptionID string `json:"subscriptionId"`
}

func (DetailCancel) isHubMessage() {}

// MarshalJSON emits type "detail.cancel".
func (m DetailCancel) MarshalJSON() ([]byte, error) {
	type plain struct {
		Type           string `json:"type"`
		SubscriptionID string `json:"subscriptionId"`
	}
	return json.Marshal(plain{"detail.cancel", m.SubscriptionID})
}

// UnmarshalJSON strictly decodes and validates a detail cancel.
func (m *DetailCancel) UnmarshalJSON(data []byte) error {
	out, err := decodeStrict(data, "detail.cancel", (*parser).parseDetailCancel)
	if err != nil {
		return err
	}
	*m = out
	return nil
}

// decodeStrict decodes one JSON value and runs a strict message parser whose
// path is rooted at what. The concrete message's discriminator is checked here
// so unmarshalling into the wrong Go type cannot succeed.
func decodeStrict[T any](data []byte, what string, parse func(*parser, any) (T, error)) (T, error) {
	v, err := decodeJSONValue(data)
	if err != nil {
		var zero T
		return zero, err
	}
	if object, ok := v.(map[string]any); ok {
		if _, err := newParser().child(what).child("type").oneOf(object["type"], what); err != nil {
			var zero T
			return zero, err
		}
	}
	return parse(newParser().child(what), v)
}
