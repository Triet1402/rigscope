package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// LiveEvent is the SSE event name.
type LiveEvent string

// Live feed events. "snapshot" is the first frame of a fresh connection;
// "reset" is the first frame when the request carried any Last-Event-ID (the
// hub never replays); "update" carries a full replacement snapshot.
const (
	LiveEventSnapshot LiveEvent = "snapshot"
	LiveEventReset    LiveEvent = "reset"
	LiveEventUpdate   LiveEvent = "update"
)

// ReceivedMetrics is a metrics record plus the hub's receipt timestamp. It is
// the exact record the collector sent, with receivedAt added by the hub.
//
//api:response
type ReceivedMetrics struct {
	CollectorID string                  `json:"collectorId"`
	RunID       string                  `json:"runId"`
	BootID      string                  `json:"bootId"`
	Sequence    int64                   `json:"sequence"`
	CapturedAt  string                  `json:"capturedAt"`
	Kind        string                  `json:"kind"`
	Payload     protocol.MetricsPayload `json:"payload"`
	ReceivedAt  string                  `json:"receivedAt"`
}

// UnmarshalJSON strictly decodes a received metrics record. The metrics
// payload is validated by the protocol parser, which owns the field-level
// rules for every metric family.
func (m *ReceivedMetrics) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "received metrics",
		[]string{"collectorId", "runId", "bootId", "sequence", "capturedAt", "kind", "payload", "receivedAt"}, nil)
	if err != nil {
		return err
	}
	var out ReceivedMetrics
	if out.CollectorID, err = rawString(fields["collectorId"], "collectorId"); err != nil {
		return err
	}
	if out.RunID, err = rawString(fields["runId"], "runId"); err != nil {
		return err
	}
	if out.BootID, err = rawString(fields["bootId"], "bootId"); err != nil {
		return err
	}
	if out.Sequence, err = rawInt64(fields["sequence"], "sequence"); err != nil {
		return err
	}
	if out.CapturedAt, err = rawString(fields["capturedAt"], "capturedAt"); err != nil {
		return err
	}
	if out.Kind, err = rawString(fields["kind"], "kind"); err != nil {
		return err
	}
	if out.ReceivedAt, err = rawString(fields["receivedAt"], "receivedAt"); err != nil {
		return err
	}
	if out.Payload, err = protocol.ParseMetricsPayload(fields["payload"]); err != nil {
		return fmt.Errorf("received metrics payload: %w", err)
	}
	if err := out.validate(); err != nil {
		return err
	}
	*m = out
	return nil
}

// MarshalJSON emits the record with receivedAt and kind "metrics".
func (m ReceivedMetrics) MarshalJSON() ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	type plain struct {
		CollectorID string                  `json:"collectorId"`
		RunID       string                  `json:"runId"`
		BootID      string                  `json:"bootId"`
		Sequence    int64                   `json:"sequence"`
		CapturedAt  string                  `json:"capturedAt"`
		Kind        string                  `json:"kind"`
		Payload     protocol.MetricsPayload `json:"payload"`
		ReceivedAt  string                  `json:"receivedAt"`
	}
	return json.Marshal(plain{m.CollectorID, m.RunID, m.BootID, m.Sequence, m.CapturedAt, m.Kind, m.Payload, m.ReceivedAt})
}

func (m ReceivedMetrics) validate() error {
	if err := validateID(m.CollectorID, "collectorId"); err != nil {
		return err
	}
	if err := validateID(m.RunID, "runId"); err != nil {
		return err
	}
	if err := validateText(m.BootID, "bootId", 0, MaxIDLength); err != nil {
		return err
	}
	if m.Sequence < 0 || !isSafeInt64(m.Sequence) {
		return fmt.Errorf("sequence must be a non-negative safe integer")
	}
	if m.Kind != "metrics" {
		return fmt.Errorf("kind must be %q", "metrics")
	}
	if err := validateTimestamp(m.CapturedAt, "capturedAt"); err != nil {
		return err
	}
	return validateTimestamp(m.ReceivedAt, "receivedAt")
}

// MachineState is one row of a live snapshot. Optional keys may be absent in
// hand-built fixtures; the hub always emits expectedIntervalMs, as null before
// a hello. displayName may be present as null when explicitly cleared, and
// metrics, inventory and delivery are present once the collector has sent
// them and retained across disconnects.
//
//api:response
type MachineState struct {
	MachineID          string                     `json:"machineId"`   //api:length=1..128
	CollectorID        string                     `json:"collectorId"` //api:length=1..128
	Hostname           string                     `json:"hostname"`    //api:length=0..255
	Connected          bool                       `json:"connected"`
	LastReceivedAt     *string                    `json:"lastReceivedAt"`
	ExpectedIntervalMs *protocol.Optional[int64]  `json:"expectedIntervalMs,omitempty"`
	Metrics            *ReceivedMetrics           `json:"metrics,omitempty"`
	Inventory          *protocol.Inventory        `json:"inventory,omitempty"`
	Delivery           *protocol.StatusMessage    `json:"delivery,omitempty"`
	DisplayName        *protocol.Optional[string] `json:"displayName,omitempty"`
	Tags               *[]string                  `json:"tags,omitempty"` //api:items=0..32
}

// UnmarshalJSON strictly decodes one machine row, validating the embedded
// protocol values with the protocol package's parsers.
func (m *MachineState) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "machine",
		[]string{"machineId", "collectorId", "hostname", "connected", "lastReceivedAt"},
		[]string{"expectedIntervalMs", "metrics", "inventory", "delivery", "displayName", "tags"})
	if err != nil {
		return err
	}
	var out MachineState
	if out.MachineID, err = rawString(fields["machineId"], "machineId"); err != nil {
		return err
	}
	if out.CollectorID, err = rawString(fields["collectorId"], "collectorId"); err != nil {
		return err
	}
	if out.Hostname, err = rawString(fields["hostname"], "hostname"); err != nil {
		return err
	}
	if out.Connected, err = rawBool(fields["connected"], "connected"); err != nil {
		return err
	}
	if out.LastReceivedAt, err = nullableString(fields["lastReceivedAt"], "lastReceivedAt"); err != nil {
		return err
	}
	if raw, ok := fields["expectedIntervalMs"]; ok {
		var value protocol.Optional[int64]
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("expectedIntervalMs: %w", err)
		}
		out.ExpectedIntervalMs = &value
		if value.Valid {
			if value.Value < protocol.MinExpectedIntervalMS || value.Value > protocol.MaxExpectedIntervalMS {
				return fmt.Errorf("expectedIntervalMs %d outside %d..%d", value.Value,
					protocol.MinExpectedIntervalMS, protocol.MaxExpectedIntervalMS)
			}
		}
	}
	if raw, ok := fields["metrics"]; ok {
		if isNull(raw) {
			return fmt.Errorf("metrics must be an object, not null")
		}
		var metrics ReceivedMetrics
		if err := json.Unmarshal(raw, &metrics); err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		if metrics.CollectorID != out.CollectorID {
			return fmt.Errorf("metrics collectorId conflicts with the machine row")
		}
		out.Metrics = &metrics
	}
	if raw, ok := fields["inventory"]; ok {
		if isNull(raw) {
			return fmt.Errorf("inventory must be an object, not null")
		}
		inventory, err := protocol.ParseInventory(raw)
		if err != nil {
			return fmt.Errorf("inventory: %w", err)
		}
		out.Inventory = &inventory
	}
	if raw, ok := fields["delivery"]; ok {
		if isNull(raw) {
			return fmt.Errorf("delivery must be an object, not null")
		}
		var status protocol.StatusMessage
		if err := json.Unmarshal(raw, &status); err != nil {
			return fmt.Errorf("delivery: %w", err)
		}
		out.Delivery = &status
	}
	if raw, ok := fields["displayName"]; ok {
		var value protocol.Optional[string]
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("displayName: %w", err)
		}
		out.DisplayName = &value
	}
	if raw, ok := fields["tags"]; ok {
		if isNull(raw) {
			return fmt.Errorf("tags must be an array, not null")
		}
		var tags []string
		if err := json.Unmarshal(raw, &tags); err != nil {
			return fmt.Errorf("tags: %w", err)
		}
		out.Tags = &tags
	}
	if err := out.validate(); err != nil {
		return err
	}
	*m = out
	return nil
}

func (m MachineState) validate() error {
	if err := validateID(m.MachineID, "machineId"); err != nil {
		return err
	}
	if err := validateID(m.CollectorID, "collectorId"); err != nil {
		return err
	}
	if err := validateText(m.Hostname, "hostname", 0, MaxHostnameLength); err != nil {
		return err
	}
	if m.LastReceivedAt != nil {
		if err := validateTimestamp(*m.LastReceivedAt, "lastReceivedAt"); err != nil {
			return err
		}
	}
	if m.ExpectedIntervalMs != nil && m.ExpectedIntervalMs.Valid {
		value := m.ExpectedIntervalMs.Value
		if value < protocol.MinExpectedIntervalMS || value > protocol.MaxExpectedIntervalMS {
			return fmt.Errorf("expectedIntervalMs %d outside %d..%d", value,
				protocol.MinExpectedIntervalMS, protocol.MaxExpectedIntervalMS)
		}
	}
	if m.Metrics != nil {
		if err := m.Metrics.validate(); err != nil {
			return err
		}
		if m.Metrics.CollectorID != m.CollectorID {
			return fmt.Errorf("metrics collectorId conflicts with the machine row")
		}
		// The browser rejects a snapshot where receivedAt is not exactly the
		// row's lastReceivedAt, including the null case.
		if m.LastReceivedAt == nil || m.Metrics.ReceivedAt != *m.LastReceivedAt {
			return fmt.Errorf("metrics receivedAt conflicts with lastReceivedAt")
		}
	}
	if m.DisplayName != nil && m.DisplayName.Valid {
		if err := validateText(m.DisplayName.Value, "displayName", 0, MaxDisplayNameLength); err != nil {
			return err
		}
	}
	if m.Tags != nil {
		if err := validateTags(*m.Tags); err != nil {
			return err
		}
	}
	return nil
}

// HubSnapshot is the SSE data payload: the hub process run ID and every
// machine the viewer may see, sorted by machineId. A scoped viewer receives
// only its machines; excluded rows (including collector IDs and hostnames)
// never enter the serialized bytes.
//
//api:response
type HubSnapshot struct {
	HubRunID string         `json:"hubRunId"`
	Machines []MachineState `json:"machines"`
}

func (s HubSnapshot) validate() error {
	if err := validateID(s.HubRunID, "hubRunId"); err != nil {
		return err
	}
	if s.Machines == nil {
		return fmt.Errorf("machines must be an array")
	}
	seen := make(map[string]struct{}, len(s.Machines))
	for _, machine := range s.Machines {
		if err := machine.validate(); err != nil {
			return err
		}
		if _, duplicate := seen[machine.MachineID]; duplicate {
			return fmt.Errorf("duplicate snapshot machine %q", machine.MachineID)
		}
		seen[machine.MachineID] = struct{}{}
	}
	return nil
}

// LiveEventID is the SSE id field: "<hubRunId>:<cursor>". The cursor is an
// opaque decimal per viewer; it is not an ordering promise across streams.
type LiveEventID struct {
	HubRunID string
	Cursor   string
}

// ParseLiveEventID splits and validates an SSE event id.
func ParseLiveEventID(value string) (LiveEventID, error) {
	index := strings.IndexByte(value, ':')
	if index <= 0 || index == len(value)-1 {
		return LiveEventID{}, fmt.Errorf("live event id: expected <hubRunId>:<cursor>")
	}
	id := LiveEventID{HubRunID: value[:index], Cursor: value[index+1:]}
	if err := validateID(id.HubRunID, "live event id hubRunId"); err != nil {
		return LiveEventID{}, err
	}
	if len(id.Cursor) > 40 {
		return LiveEventID{}, fmt.Errorf("live event id: cursor is too long")
	}
	for i := 0; i < len(id.Cursor); i++ {
		if id.Cursor[i] < '0' || id.Cursor[i] > '9' {
			return LiveEventID{}, fmt.Errorf("live event id: cursor must be decimal digits")
		}
	}
	return id, nil
}

// String renders the id in wire form.
func (id LiveEventID) String() string { return id.HubRunID + ":" + id.Cursor }

// LiveFrame is one decoded SSE event.
type LiveFrame struct {
	ID    LiveEventID
	Event LiveEvent
	Data  HubSnapshot
}

// EncodeLiveFrame serializes one SSE event block, including the trailing blank
// line. The data line is the checked, compact JSON snapshot encoding; the
// envelope id hubRunId must match the snapshot's hubRunId.
func EncodeLiveFrame(frame LiveFrame) ([]byte, error) {
	if err := validateLiveEvent(frame.Event); err != nil {
		return nil, err
	}
	if _, err := ParseLiveEventID(frame.ID.String()); err != nil {
		return nil, err
	}
	if frame.Data.HubRunID != frame.ID.HubRunID {
		return nil, fmt.Errorf("live frame: id hubRunId conflicts with the snapshot")
	}
	data, err := EncodeHubSnapshot(frame.Data)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	fmt.Fprintf(&buffer, "id: %s\n", frame.ID.String())
	fmt.Fprintf(&buffer, "event: %s\n", frame.Event)
	fmt.Fprintf(&buffer, "data: %s\n\n", data)
	return buffer.Bytes(), nil
}

// DecodeLiveFrame parses one SSE event block. It returns ok=false for a block
// that contains only comments (an SSE keepalive). Unknown fields and missing
// id/event/data lines are errors.
func DecodeLiveFrame(block []byte) (frame LiveFrame, ok bool, err error) {
	text := strings.ReplaceAll(string(block), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	var idLine, eventLine, dataLine string
	var haveID, haveEvent, haveData bool
	for _, line := range lines {
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			return frame, false, fmt.Errorf("live frame: malformed line %q", line)
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			if haveID {
				return frame, false, fmt.Errorf("live frame: duplicate id line")
			}
			idLine, haveID = value, true
		case "event":
			if haveEvent {
				return frame, false, fmt.Errorf("live frame: duplicate event line")
			}
			eventLine, haveEvent = value, true
		case "data":
			if haveData {
				dataLine += "\n" + value
				continue
			}
			dataLine, haveData = value, true
		default:
			return frame, false, fmt.Errorf("live frame: unknown field %q", field)
		}
	}
	if !haveID && !haveEvent && !haveData {
		return frame, false, nil
	}
	if !haveID || !haveEvent || !haveData {
		return frame, false, fmt.Errorf("live frame: id, event and data are all required")
	}
	id, err := ParseLiveEventID(idLine)
	if err != nil {
		return frame, false, err
	}
	event := LiveEvent(eventLine)
	if err := validateLiveEvent(event); err != nil {
		return frame, false, err
	}
	snapshot, err := decodeStrict[HubSnapshot]([]byte(dataLine), "live frame data")
	if err != nil {
		return frame, false, err
	}
	if err := snapshot.validate(); err != nil {
		return frame, false, fmt.Errorf("live frame data: %w", err)
	}
	if snapshot.HubRunID != id.HubRunID {
		return frame, false, fmt.Errorf("live frame: id hubRunId conflicts with the snapshot")
	}
	return LiveFrame{ID: id, Event: event, Data: snapshot}, true, nil
}

func validateLiveEvent(event LiveEvent) error {
	switch event {
	case LiveEventSnapshot, LiveEventReset, LiveEventUpdate:
		return nil
	default:
		return fmt.Errorf("unknown live event %q", event)
	}
}
