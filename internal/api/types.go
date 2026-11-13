package api

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
	"unicode/utf16"
)

// MachineScope is a browser account's machine allowlist. On the wire it is
// either the JSON string "*" (every machine) or a JSON array of machine IDs
// (the empty array grants no machine access).
type MachineScope struct {
	// All is true for "*".
	All bool
	// IDs holds the explicit allowlist when All is false.
	IDs []string
}

// MarshalJSON emits "*" or an array of machine IDs.
func (s MachineScope) MarshalJSON() ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if s.All {
		return []byte(`"*"`), nil
	}
	ids := s.IDs
	if ids == nil {
		ids = []string{}
	}
	return json.Marshal(ids)
}

// UnmarshalJSON accepts only "*" or an array of bounded machine IDs.
func (s *MachineScope) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("machine scope: %w", err)
	}
	var out MachineScope
	switch value := v.(type) {
	case string:
		if value != "*" {
			return fmt.Errorf("machine scope: expected %q or an array", "*")
		}
		out.All = true
	case []any:
		seen := make(map[string]struct{}, len(value))
		out.IDs = make([]string, 0, len(value))
		for _, item := range value {
			id, ok := item.(string)
			if !ok {
				return fmt.Errorf("machine scope: machine IDs must be strings")
			}
			if err := validateID(id, "machine scope ID"); err != nil {
				return err
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("machine scope: duplicate machine ID %q", id)
			}
			seen[id] = struct{}{}
			out.IDs = append(out.IDs, id)
		}
	default:
		return fmt.Errorf("machine scope: expected %q or an array", "*")
	}
	*s = out
	return nil
}

// Includes reports whether the scope grants access to machineID.
func (s MachineScope) Includes(machineID string) bool {
	if s.All {
		return true
	}
	for _, id := range s.IDs {
		if id == machineID {
			return true
		}
	}
	return false
}

// Empty reports whether the scope grants no machine access.
func (s MachineScope) Empty() bool { return !s.All && len(s.IDs) == 0 }

// Normalized returns a copy with sorted IDs so comparisons are stable.
func (s MachineScope) Normalized() MachineScope {
	if s.All {
		return MachineScope{All: true}
	}
	ids := append([]string(nil), s.IDs...)
	sort.Strings(ids)
	if ids == nil {
		ids = []string{}
	}
	return MachineScope{IDs: ids}
}

func (s MachineScope) validate() error {
	if s.All {
		if len(s.IDs) != 0 {
			return fmt.Errorf("machine scope: %q cannot carry IDs", "*")
		}
		return nil
	}
	seen := make(map[string]struct{}, len(s.IDs))
	for _, id := range s.IDs {
		if err := validateID(id, "machine scope ID"); err != nil {
			return err
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("machine scope: duplicate machine ID %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bounds shared by request and response validation
// ---------------------------------------------------------------------------

const (
	// MaxIDLength bounds every collector, machine, rule and user identifier.
	MaxIDLength = 128
	// MaxHostnameLength bounds a machine hostname.
	MaxHostnameLength = 255
	// MaxUsernameLength bounds a browser username.
	MaxUsernameLength = 128
	// MaxPasswordLength mirrors MAX_BROWSER_PASSWORD_CHARS.
	MaxPasswordLength = 1024
	// MaxDisplayNameLength bounds a machine display name.
	MaxDisplayNameLength = 128
	// MaxTags is the largest machine tag list.
	MaxTags = 32
	// MaxTagLength bounds one machine tag.
	MaxTagLength = 64
	// MaxResourceIDLength bounds an alert resource identifier.
	MaxResourceIDLength = 256
	// MaxTimestampLength bounds an ISO-8601 timestamp string.
	MaxTimestampLength = 64
	// MaxMachineIDQueryLength bounds the machineId query parameter.
	MaxMachineIDQueryLength = 128
	// MaxViewerIDLength bounds the process viewer identifier.
	MaxViewerIDLength = 128
)

// Request body limits, mirroring the Node handlers.
const (
	MaxLoginBodyBytes          = 4096
	MaxAdminUserBodyBytes      = 8 * 1024
	MaxEnrollmentBodyBytes     = 1024
	MaxCollectorAdminBodyBytes = 2048
	MaxMachineMetaBodyBytes    = 4096
	MaxAlertRuleBodyBytes      = 16 * 1024
	MaxAlertWebhookBodyBytes   = 8 * 1024
)

// DefaultHistoryPoints is the /api/history maxPoints default.
const DefaultHistoryPoints = 600

// MaxHistoryPoints is the /api/history maxPoints clamp (a larger value is
// accepted and clamped, not rejected).
const MaxHistoryPoints = 2000

// MaxHistoryMetrics is the largest metric list /api/history accepts: the size
// of this build's storage metric catalog.
const MaxHistoryMetrics = 33

// MaxHistoryResourceIDs is the largest resourceIds list /api/history accepts.
const MaxHistoryResourceIDs = 64

// MaxHistoryMachineIDLength bounds the machineId query parameter on every
// endpoint. (The storage query layer alone accepts 256; the HTTP route applies
// this tighter bound before calling it.)
const MaxHistoryMachineIDLength = 128

// MaxAlertIncidentLimit is the largest /api/alerts limit value.
const MaxAlertIncidentLimit = 500

// DefaultAlertIncidentLimit is the /api/alerts limit default.
const DefaultAlertIncidentLimit = 200

// MaxAuditLimit is the largest /api/admin/audit limit value.
const MaxAuditLimit = 1000

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

// utf16Len reports the JavaScript string length (UTF-16 code units). The Node
// hub validates lengths with String.prototype.length, so Go must count the same
// way to accept and reject exactly the same strings.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xffff {
			n += len(utf16.Encode([]rune{r}))
			continue
		}
		n++
	}
	return n
}

func validateID(value, what string) error {
	if value == "" || utf16Len(value) > MaxIDLength {
		return fmt.Errorf("%s must be 1..%d characters", what, MaxIDLength)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return fmt.Errorf("%s must be printable ASCII without spaces", what)
		}
	}
	return nil
}

// validateText bounds a text field and rejects C0/C1 control characters, the
// same check the hub applies to display names and tags.
func validateText(value, what string, minLen, maxLen int) error {
	n := utf16Len(value)
	if n < minLen || n > maxLen {
		return fmt.Errorf("%s length %d outside %d..%d", what, n, minLen, maxLen)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains a control character", what)
		}
	}
	return nil
}

func validateTimestamp(value, what string) error {
	if utf16Len(value) < 1 || utf16Len(value) > MaxTimestampLength {
		return fmt.Errorf("%s must be 1..%d characters", what, MaxTimestampLength)
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fmt.Errorf("%s is not an RFC 3339 timestamp", what)
	}
	year := t.UTC().Year()
	if year < 2000 || year > 2200 {
		return fmt.Errorf("%s is outside the supported 2000..2200 window", what)
	}
	return nil
}

// isSafeInt64 reports whether v fits the protocol's safe-integer bound.
func isSafeInt64(v int64) bool {
	return v >= -9007199254740991 && v <= 9007199254740991
}
