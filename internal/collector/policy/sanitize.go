package policy

import (
	"net/netip"
	"regexp"
	"strings"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// withheldAddressText replaces a network address found inside a failure reason
// or resource id while addresses are withheld. The surrounding text stays so
// the failure remains diagnosable without naming the host on the network.
const withheldAddressText = "[address withheld]"

// ApplyMetricsPayload replaces withheld metric families with the unavailable
// variant. The input is never mutated; callers get a fresh payload value.
func ApplyMetricsPayload(payload protocol.MetricsPayload, sharing Policy) protocol.MetricsPayload {
	out := payload
	if !sharing.CPU {
		out.CPU = withheld()
	}
	if !sharing.Memory {
		out.Memory = withheld()
	}
	if !sharing.Swap {
		out.Swap = withheld()
	}
	if !sharing.GPUs {
		out.GPUs = withheld()
	}
	if !sharing.Disks {
		out.Disks = withheld()
	}
	if !sharing.Network {
		out.Network = withheld()
	}
	return out
}

// ApplyMetrics applies the policy to one metrics record, returning a fresh
// envelope and payload. The source record is never mutated.
func ApplyMetrics(record protocol.MetricsRecord, sharing Policy) protocol.MetricsRecord {
	return protocol.MetricsRecord{
		RecordEnvelope: record.RecordEnvelope,
		Payload:        ApplyMetricsPayload(record.Payload, sharing),
	}
}

// FilterInventory applies the policy to inventory. Withheld families keep
// their capability entry but report "withheld" and carry no resources.
// Address lists can be withheld independently of the network family.
func FilterInventory(inventory protocol.Inventory, sharing Policy) protocol.Inventory {
	out := inventory
	out.Capabilities = inventory.Capabilities
	if !sharing.CPU {
		out.CPU = protocol.CPUInventory{}
		out.Capabilities.CPU = withheldCapability()
	}
	if !sharing.Memory {
		out.Memory.TotalBytes = nil
		out.Capabilities.Memory = withheldCapability()
	}
	if !sharing.Swap {
		out.Memory.SwapTotalBytes = nil
		out.Capabilities.Swap = withheldCapability()
	}
	if !sharing.GPUs {
		out.GPUs = []protocol.GpuInventory{}
		out.Capabilities.GPUs = withheldCapability()
	} else {
		out.GPUs = cloneGPUs(inventory.GPUs)
	}
	if !sharing.Disks {
		out.Filesystems = []protocol.FilesystemInventory{}
		out.Disks = []protocol.DiskInventory{}
		out.Capabilities.Disks = withheldCapability()
	} else {
		out.Filesystems = cloneFilesystems(inventory.Filesystems)
		out.Disks = cloneDisks(inventory.Disks)
	}
	if !sharing.Network {
		out.Network = []protocol.InterfaceInventory{}
		out.Capabilities.Network = withheldCapability()
	} else {
		out.Network = cloneNetwork(inventory.Network, sharing.NetworkAddresses)
	}
	if !sharing.Processes {
		out.Capabilities.Processes = withheldCapability()
	}
	return out
}

// ApplyDetail applies the policy to one process-detail result. It returns
// false when processes are withheld entirely, telling the caller to drop the
// result instead of transmitting an empty snapshot. When only arguments are
// withheld, the rows are copied with Command removed.
func ApplyDetail(result protocol.DetailResult, sharing Policy) (protocol.DetailResult, bool) {
	if !sharing.Processes {
		return protocol.DetailResult{}, false
	}
	snapshot := result.Snapshot
	rows := make([]protocol.ProcessRow, len(snapshot.Processes))
	for index, row := range snapshot.Processes {
		if !sharing.ProcessArguments {
			row.Command = nil
		}
		rows[index] = row
	}
	snapshot.Processes = rows
	result.Snapshot = snapshot
	return result, true
}

// FilterFailures applies the sharing policy to collection failures before they
// are exposed in a status message. A failure for a withheld family is dropped
// entirely; when addresses are withheld, any address text inside a reason or
// resource id is redacted. The input is never mutated.
func FilterFailures(failures []protocol.CollectionFailure, sharing Policy) []protocol.CollectionFailure {
	out := make([]protocol.CollectionFailure, 0, len(failures))
	for _, failure := range failures {
		if !failureFamilyShared(failure, sharing) {
			continue
		}
		metric := failure.Metric
		reason := failure.Reason
		resourceID := cloneString(failure.ResourceID)
		if !sharing.NetworkAddresses {
			reason = redactNetworkAddresses(reason)
			if resourceID != nil {
				redacted := redactNetworkAddresses(*resourceID)
				resourceID = &redacted
			}
		}
		out = append(out, protocol.CollectionFailure{
			Metric:     metric,
			ResourceID: resourceID,
			Reason:     reason,
		})
	}
	return out
}

// failureFamilyShared reports whether a failure belongs to a shared family. A
// disk resource id is enough to classify a failure even when its metric name is
// unfamiliar, so a withheld disk family cannot leak an fs:/disk: identifier.
func failureFamilyShared(failure protocol.CollectionFailure, sharing Policy) bool {
	if !sharing.Disks && isDiskResource(failure.ResourceID) {
		return false
	}
	family, known := failureFamily(failure.Metric)
	if !known {
		return true
	}
	return sharing.MetricEnabled(family)
}

func failureFamily(metric string) (protocol.MetricFamily, bool) {
	switch protocol.MetricFamily(strings.ToLower(strings.TrimSpace(metric))) {
	case protocol.FamilyCPU:
		return protocol.FamilyCPU, true
	case protocol.FamilyMemory:
		return protocol.FamilyMemory, true
	case protocol.FamilySwap:
		return protocol.FamilySwap, true
	case protocol.FamilyGPUs:
		return protocol.FamilyGPUs, true
	case protocol.FamilyDisks:
		return protocol.FamilyDisks, true
	case protocol.FamilyNetwork:
		return protocol.FamilyNetwork, true
	default:
		return "", false
	}
}

func isDiskResource(resourceID *string) bool {
	if resourceID == nil {
		return false
	}
	id := strings.ToLower(*resourceID)
	return strings.HasPrefix(id, "fs:") || strings.HasPrefix(id, "disk:")
}

// addressTokenPattern matches the characters an IPv4/IPv6 address or CIDR can
// contain, including the colon-only compressed forms. Candidates that do not
// parse as an address are left untouched, so ordinary words survive.
var addressTokenPattern = regexp.MustCompile(`[0-9A-Fa-f:][0-9A-Fa-f:.%]*`)

// redactNetworkAddresses replaces every parseable IP address in text. MAC
// addresses are deliberately kept: the policy withholds interface addresses,
// not hardware identifiers (which the inventory shares independently).
func redactNetworkAddresses(text string) string {
	if text == "" {
		return text
	}
	return addressTokenPattern.ReplaceAllStringFunc(text, func(token string) string {
		if isNetworkAddress(token) {
			return withheldAddressText
		}
		return token
	})
}

func isNetworkAddress(token string) bool {
	candidate := token
	if index := strings.IndexByte(candidate, '/'); index >= 0 {
		candidate = candidate[:index]
	}
	if index := strings.IndexByte(candidate, '%'); index >= 0 {
		candidate = candidate[:index]
	}
	if parsesAsAddress(candidate) {
		return true
	}
	// A trailing sentence period or colon is punctuation, not part of the
	// address.
	trimmed := strings.TrimRight(candidate, ".:")
	return trimmed != candidate && trimmed != "" && parsesAsAddress(trimmed)
}

func parsesAsAddress(candidate string) bool {
	if candidate == "" {
		return false
	}
	if _, err := netip.ParseAddr(candidate); err == nil {
		return true
	}
	if _, err := netip.ParseAddrPort(candidate); err == nil {
		return true
	}
	return false
}

// SanitizeTelemetryRecord applies the policy to a queued metrics or gap
// record. Gap records carry no shareable fields and are normalized to their
// value form. The boolean is false when the record cannot be sanitized (an
// unknown or nil implementation) and the caller must drop it instead of
// transmitting it unchanged. Both value and pointer forms are accepted.
func SanitizeTelemetryRecord(record protocol.TelemetryRecord, sharing Policy) (protocol.TelemetryRecord, bool) {
	switch value := record.(type) {
	case protocol.MetricsRecord:
		return ApplyMetrics(value, sharing), true
	case *protocol.MetricsRecord:
		if value == nil {
			return nil, false
		}
		return ApplyMetrics(*value, sharing), true
	case protocol.GapRecord:
		return value, true
	case *protocol.GapRecord:
		if value == nil {
			return nil, false
		}
		return *value, true
	default:
		return nil, false
	}
}

// SanitizeCollectorMessage applies the policy to a queued collector message.
// The boolean is false when the message must be dropped: a detail result whose
// process visibility was withdrawn, or a message implementation this policy
// does not know how to sanitize. The queue calls this when a policy narrows, so
// already-queued records cannot leak fields the new policy hides. Both value
// and pointer forms are accepted and normalized to values.
func SanitizeCollectorMessage(message protocol.CollectorMessage, sharing Policy) (protocol.CollectorMessage, bool) {
	switch value := message.(type) {
	case protocol.HelloMessage:
		return value, true
	case *protocol.HelloMessage:
		if value == nil {
			return nil, false
		}
		return *value, true
	case protocol.SamplesMessage:
		return sanitizeSamples(value, sharing)
	case *protocol.SamplesMessage:
		if value == nil {
			return nil, false
		}
		return sanitizeSamples(*value, sharing)
	case protocol.InventoryMessage:
		value.Inventory = FilterInventory(value.Inventory, sharing)
		return value, true
	case *protocol.InventoryMessage:
		if value == nil {
			return nil, false
		}
		copied := *value
		copied.Inventory = FilterInventory(value.Inventory, sharing)
		return copied, true
	case protocol.StatusMessage:
		value.Failures = FilterFailures(value.Failures, sharing)
		return value, true
	case *protocol.StatusMessage:
		if value == nil {
			return nil, false
		}
		copied := *value
		copied.Failures = FilterFailures(value.Failures, sharing)
		return copied, true
	case protocol.DetailResult:
		return ApplyDetail(value, sharing)
	case *protocol.DetailResult:
		if value == nil {
			return nil, false
		}
		return ApplyDetail(*value, sharing)
	case protocol.DetailEnded:
		return value, true
	case *protocol.DetailEnded:
		if value == nil {
			return nil, false
		}
		return *value, true
	default:
		return nil, false
	}
}

func sanitizeSamples(message protocol.SamplesMessage, sharing Policy) (protocol.CollectorMessage, bool) {
	records := make([]protocol.TelemetryRecord, len(message.Records))
	for index, record := range message.Records {
		sanitized, ok := SanitizeTelemetryRecord(record, sharing)
		if !ok {
			return nil, false
		}
		records[index] = sanitized
	}
	message.Records = records
	return message, true
}

func cloneGPUs(entries []protocol.GpuInventory) []protocol.GpuInventory {
	out := make([]protocol.GpuInventory, 0, len(entries))
	for _, entry := range entries {
		entry.PciBusID = cloneString(entry.PciBusID)
		entry.MemoryTotalBytes = cloneInt64(entry.MemoryTotalBytes)
		entry.Reason = cloneString(entry.Reason)
		out = append(out, entry)
	}
	return out
}

func cloneFilesystems(entries []protocol.FilesystemInventory) []protocol.FilesystemInventory {
	out := make([]protocol.FilesystemInventory, 0, len(entries))
	for _, entry := range entries {
		entry.TotalBytes = cloneInt64(entry.TotalBytes)
		out = append(out, entry)
	}
	return out
}

func cloneDisks(entries []protocol.DiskInventory) []protocol.DiskInventory {
	out := make([]protocol.DiskInventory, 0, len(entries))
	for _, entry := range entries {
		entry.Model = cloneString(entry.Model)
		entry.Rotational = cloneBool(entry.Rotational)
		entry.SizeBytes = cloneInt64(entry.SizeBytes)
		out = append(out, entry)
	}
	return out
}

func cloneNetwork(entries []protocol.InterfaceInventory, withAddresses bool) []protocol.InterfaceInventory {
	out := make([]protocol.InterfaceInventory, 0, len(entries))
	for _, entry := range entries {
		entry.Mac = cloneString(entry.Mac)
		entry.SpeedMbps = cloneInt64(entry.SpeedMbps)
		addresses := make([]protocol.InterfaceAddress, 0, len(entry.Addresses))
		if withAddresses {
			addresses = append(addresses, entry.Addresses...)
		}
		entry.Addresses = addresses
		out = append(out, entry)
	}
	return out
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
