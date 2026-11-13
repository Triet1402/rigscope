package processes

// Local mirrors of the protocol types owned by internal/protocol. They are
// duplicated on purpose while that package is written in parallel: the field
// names, units and JSON encoding must stay 1:1 with the wire contract
// (src/contracts/telemetry.ts in the prototype). The delivery layer converts
// these values to protocol types when it sends them. Integer bounds and
// MaxSafeInteger are imported from internal/protocol so they cannot drift.

// Limit copies. They mirror the protocol package constants of the same name;
// keep them in sync until this package imports internal/protocol directly.
const (
	// mirrors protocol.MaxProcessRows
	maxProcessRows = 6_000
	// mirrors protocol.MaxCores
	maxCores = 1_024
	// mirrors protocol.MaxDetailBytes
	maxDetailBytes = 1_048_576
	// mirrors protocol.MinDetailIntervalMS
	minDetailIntervalMS = 1_000
	// mirrors protocol.MaxDetailIntervalMS
	maxDetailIntervalMS = 60_000
	// mirrors protocol.MinDetailLeaseMS
	minDetailLeaseMS = 5_000
	// mirrors protocol.MaxDetailLeaseMS
	maxDetailLeaseMS = 120_000
)

// ProcessRow is one process in a snapshot. Optional values are pointers so an
// unknown value is JSON null or an absent field, never a fabricated zero.
type ProcessRow struct {
	PID        int     `json:"pid"`
	PPID       int     `json:"ppid"`
	StartTicks int64   `json:"startTicks"`
	Name       string  `json:"name"`
	User       *string `json:"user"`
	State      string  `json:"state"`
	// CPUPercent follows the contract convention: 100 is one fully busy core,
	// so a multithreaded process may exceed 100.
	CPUPercent         *float64 `json:"cpuPercent"`
	MemoryRSSBytes     *int64   `json:"memoryRssBytes"`
	GPUMemoryBytes     *int64   `json:"gpuMemoryBytes"`
	IOReadBytesPerSec  *float64 `json:"ioReadBytesPerSec"`
	IOWriteBytesPerSec *float64 `json:"ioWriteBytesPerSec"`
	Threads            *int     `json:"threads"`
	// Unit is the systemd service/scope that owns the process, when known.
	Unit *string `json:"unit"`
	// Command is present only when the destination policy allows arguments.
	Command *string `json:"command,omitempty"`
}

// ProcessSnapshot is one process table scan.
type ProcessSnapshot struct {
	TotalProcesses int  `json:"totalProcesses"`
	Truncated      bool `json:"truncated"`
	// Inaccessible counts processes whose stat/status could not be read for a
	// reason other than the process disappearing.
	Inaccessible int          `json:"inaccessible"`
	Processes    []ProcessRow `json:"processes"`
}

// DetailSubscribe asks the collector for process detail. Create-or-renew:
// re-sending an active subscription id extends its lease.
type DetailSubscribe struct {
	Type           string `json:"type"`
	SubscriptionID string `json:"subscriptionId"`
	Kind           string `json:"kind"`
	IntervalMS     int64  `json:"intervalMs"`
	LeaseMS        int64  `json:"leaseMs"`
}

// DetailCancel stops one subscription.
type DetailCancel struct {
	Type           string `json:"type"`
	SubscriptionID string `json:"subscriptionId"`
}

// DetailResult carries one snapshot for one subscription.
type DetailResult struct {
	Type           string          `json:"type"`
	SubscriptionID string          `json:"subscriptionId"`
	CapturedAt     string          `json:"capturedAt"`
	Snapshot       ProcessSnapshot `json:"snapshot"`
}

// DetailEndedReason is the terminal reason of a subscription.
type DetailEndedReason string

const (
	ReasonExpired     DetailEndedReason = "expired"
	ReasonCancelled   DetailEndedReason = "cancelled"
	ReasonWithheld    DetailEndedReason = "withheld"
	ReasonUnsupported DetailEndedReason = "unsupported"
	ReasonBusy        DetailEndedReason = "busy"
	ReasonFailed      DetailEndedReason = "failed"
)

// DetailEnded is the terminal acknowledgement for a subscription.
type DetailEnded struct {
	Type           string            `json:"type"`
	SubscriptionID string            `json:"subscriptionId"`
	Reason         DetailEndedReason `json:"reason"`
}

// DetailMessage is either a DetailResult or a DetailEnded. The scheduler hands
// it to the delivery callback; the unexported method keeps the set closed.
type DetailMessage interface {
	detailMessage()
}

func (DetailResult) detailMessage() {}
func (DetailEnded) detailMessage()  {}

// capturedAtLayout matches JavaScript Date#toISOString.
const capturedAtLayout = "2006-01-02T15:04:05.000Z"
