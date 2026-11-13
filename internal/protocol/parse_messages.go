package protocol

import "fmt"

func (p *parser) parseHello(v any) (HelloMessage, error) {
	o, err := p.object(v)
	if err != nil {
		return HelloMessage{}, err
	}
	if err := p.keys(
		o,
		[]string{"type", "collectorId", "runId", "bootId", "hostname", "capabilities", "expectedIntervalMs"},
		[]string{"protocol"},
	); err != nil {
		return HelloMessage{}, err
	}
	rawCapabilities, err := p.child("capabilities").array(o["capabilities"], 1, len(CapabilityNames))
	if err != nil {
		return HelloMessage{}, err
	}
	capabilities := make([]CapabilityName, len(rawCapabilities))
	seenCapabilities := make(map[CapabilityName]struct{}, len(rawCapabilities))
	hasMetrics := false
	for i, item := range rawCapabilities {
		value, err := p.child("capabilities").at(i).oneOf(
			item,
			string(CapabilityMetrics),
			string(CapabilityInventory),
			string(CapabilityProcesses),
		)
		if err != nil {
			return HelloMessage{}, err
		}
		capability := CapabilityName(value)
		if _, duplicate := seenCapabilities[capability]; duplicate {
			return HelloMessage{}, p.errf("capabilities must be unique and include metrics")
		}
		seenCapabilities[capability] = struct{}{}
		if capability == CapabilityMetrics {
			hasMetrics = true
		}
		capabilities[i] = capability
	}
	if !hasMetrics {
		return HelloMessage{}, p.errf("capabilities must be unique and include metrics")
	}
	var out HelloMessage
	out.Capabilities = capabilities
	if out.CollectorID, err = p.child("collectorId").id(o["collectorId"]); err != nil {
		return HelloMessage{}, err
	}
	if out.RunID, err = p.child("runId").id(o["runId"]); err != nil {
		return HelloMessage{}, err
	}
	if out.BootID, err = p.child("bootId").text(o["bootId"], 0, MaxIDLength); err != nil {
		return HelloMessage{}, err
	}
	if out.Hostname, err = p.child("hostname").text(o["hostname"], 0, MaxHostnameLength); err != nil {
		return HelloMessage{}, err
	}
	if out.ExpectedIntervalMS, err = p.child("expectedIntervalMs").integer(
		o["expectedIntervalMs"],
		MinExpectedIntervalMS,
		MaxExpectedIntervalMS,
	); err != nil {
		return HelloMessage{}, err
	}
	if protocol, ok := o["protocol"]; ok {
		version, err := p.child("protocol").integer(protocol, 1, MaxSafeInteger)
		if err != nil {
			return HelloMessage{}, err
		}
		out.Protocol = &version
	}
	return out, nil
}

func (p *parser) parseSamples(v any) (SamplesMessage, error) {
	o, err := p.object(v)
	if err != nil {
		return SamplesMessage{}, err
	}
	if err := p.keys(o, []string{"type", "mode", "records"}, nil); err != nil {
		return SamplesMessage{}, err
	}
	mode, err := p.child("mode").oneOf(o["mode"], string(SampleModeLive), string(SampleModeReplay))
	if err != nil {
		return SamplesMessage{}, err
	}
	rawRecords, err := p.child("records").array(o["records"], 1, MaxBatchRecords)
	if err != nil {
		return SamplesMessage{}, err
	}
	records := make([]TelemetryRecord, len(rawRecords))
	seen := make(map[recordKey]struct{}, len(rawRecords))
	for i, item := range rawRecords {
		record, err := p.child("records").at(i).parseTelemetryRecord(item)
		if err != nil {
			return SamplesMessage{}, err
		}
		key := recordUniquenessKey(record)
		if _, duplicate := seen[key]; duplicate {
			return SamplesMessage{}, p.errf("duplicate record in batch")
		}
		seen[key] = struct{}{}
		records[i] = record
	}
	return SamplesMessage{Mode: SampleMode(mode), Records: records}, nil
}

// recordKey is the (runId, sequence) uniqueness key within a batch.
type recordKey struct {
	runID    string
	sequence int64
}

func recordUniquenessKey(r TelemetryRecord) recordKey {
	switch v := r.(type) {
	case MetricsRecord:
		return recordKey{v.RunID, v.Sequence}
	case GapRecord:
		return recordKey{v.RunID, v.Sequence}
	default:
		return recordKey{}
	}
}

func (p *parser) parseInventoryMessage(v any) (InventoryMessage, error) {
	o, err := p.object(v)
	if err != nil {
		return InventoryMessage{}, err
	}
	if err := p.keys(o, []string{"type", "capturedAt", "inventory"}, nil); err != nil {
		return InventoryMessage{}, err
	}
	var out InventoryMessage
	if out.CapturedAt, err = p.child("capturedAt").timestamp(o["capturedAt"]); err != nil {
		return InventoryMessage{}, err
	}
	if out.Inventory, err = p.child("inventory").parseInventory(o["inventory"]); err != nil {
		return InventoryMessage{}, err
	}
	return out, nil
}

func (p *parser) parseStatusMessage(v any) (StatusMessage, error) {
	o, err := p.object(v)
	if err != nil {
		return StatusMessage{}, err
	}
	if err := p.keys(o, []string{"type", "capturedAt", "delivery", "failures"}, nil); err != nil {
		return StatusMessage{}, err
	}
	var out StatusMessage
	if out.CapturedAt, err = p.child("capturedAt").timestamp(o["capturedAt"]); err != nil {
		return StatusMessage{}, err
	}
	if out.Delivery, err = p.child("delivery").parseDeliveryStatus(o["delivery"]); err != nil {
		return StatusMessage{}, err
	}
	rawFailures, err := p.child("failures").array(o["failures"], 0, MaxCollectionFailures)
	if err != nil {
		return StatusMessage{}, err
	}
	out.Failures = make([]CollectionFailure, len(rawFailures))
	for i, item := range rawFailures {
		failure, err := p.child("failures").at(i).parseCollectionFailure(item)
		if err != nil {
			return StatusMessage{}, err
		}
		out.Failures[i] = failure
	}
	return out, nil
}

func (p *parser) parseCollectionFailure(v any) (CollectionFailure, error) {
	o, err := p.object(v)
	if err != nil {
		return CollectionFailure{}, err
	}
	if err := p.keys(o, []string{"metric", "resourceId", "reason"}, nil); err != nil {
		return CollectionFailure{}, err
	}
	var out CollectionFailure
	if out.Metric, err = p.child("metric").text(o["metric"], 1, 64); err != nil {
		return CollectionFailure{}, err
	}
	if resourceID := o["resourceId"]; resourceID != nil {
		id, err := p.child("resourceId").resourceID(resourceID)
		if err != nil {
			return CollectionFailure{}, err
		}
		out.ResourceID = &id
	}
	if out.Reason, err = p.child("reason").text(o["reason"], 0, MaxTextLength); err != nil {
		return CollectionFailure{}, err
	}
	return out, nil
}

func (p *parser) parseDetailResult(v any) (DetailResult, error) {
	o, err := p.object(v)
	if err != nil {
		return DetailResult{}, err
	}
	if err := p.keys(o, []string{"type", "subscriptionId", "capturedAt", "snapshot"}, nil); err != nil {
		return DetailResult{}, err
	}
	var out DetailResult
	if out.SubscriptionID, err = p.child("subscriptionId").id(o["subscriptionId"]); err != nil {
		return DetailResult{}, err
	}
	if out.CapturedAt, err = p.child("capturedAt").timestamp(o["capturedAt"]); err != nil {
		return DetailResult{}, err
	}
	if out.Snapshot, err = p.child("snapshot").parseProcessSnapshot(o["snapshot"]); err != nil {
		return DetailResult{}, err
	}
	return out, nil
}

func (p *parser) parseDetailEnded(v any) (DetailEnded, error) {
	o, err := p.object(v)
	if err != nil {
		return DetailEnded{}, err
	}
	if err := p.keys(o, []string{"type", "subscriptionId", "reason"}, nil); err != nil {
		return DetailEnded{}, err
	}
	var out DetailEnded
	if out.SubscriptionID, err = p.child("subscriptionId").id(o["subscriptionId"]); err != nil {
		return DetailEnded{}, err
	}
	reason, err := p.child("reason").oneOf(
		o["reason"],
		string(DetailEndedExpired),
		string(DetailEndedCancelled),
		string(DetailEndedWithheld),
		string(DetailEndedUnsupported),
		string(DetailEndedBusy),
		string(DetailEndedFailed),
	)
	if err != nil {
		return DetailEnded{}, err
	}
	out.Reason = DetailEndedReason(reason)
	return out, nil
}

func (p *parser) parseDeliveryStatus(v any) (DeliveryStatus, error) {
	o, err := p.object(v)
	if err != nil {
		return DeliveryStatus{}, err
	}
	if err := p.keys(
		o,
		[]string{"pendingRecords", "pendingBytes", "oldestPendingCapturedAt", "inFlightRecords", "droppedRecords", "replaying", "lastAckAt"},
		[]string{"queueMode"},
	); err != nil {
		return DeliveryStatus{}, err
	}
	var out DeliveryStatus
	if out.Replaying, err = p.child("replaying").boolean(o["replaying"]); err != nil {
		return DeliveryStatus{}, err
	}
	if queueMode, ok := o["queueMode"]; ok {
		mode, err := p.child("queueMode").oneOf(queueMode, string(QueueModeMemory), string(QueueModeDurable))
		if err != nil {
			return DeliveryStatus{}, err
		}
		typed := QueueMode(mode)
		out.QueueMode = &typed
	}
	if out.PendingRecords, err = p.child("pendingRecords").integer(o["pendingRecords"], 0, MaxSafeInteger); err != nil {
		return DeliveryStatus{}, err
	}
	if out.PendingBytes, err = p.child("pendingBytes").integer(o["pendingBytes"], 0, MaxSafeInteger); err != nil {
		return DeliveryStatus{}, err
	}
	if out.OldestPendingCapturedAt, err = p.child("oldestPendingCapturedAt").nullableTimestamp(o["oldestPendingCapturedAt"]); err != nil {
		return DeliveryStatus{}, err
	}
	if out.InFlightRecords, err = p.child("inFlightRecords").integer(o["inFlightRecords"], 0, MaxSafeInteger); err != nil {
		return DeliveryStatus{}, err
	}
	if out.DroppedRecords, err = p.child("droppedRecords").integer(o["droppedRecords"], 0, MaxSafeInteger); err != nil {
		return DeliveryStatus{}, err
	}
	if out.LastAckAt, err = p.child("lastAckAt").nullableTimestamp(o["lastAckAt"]); err != nil {
		return DeliveryStatus{}, err
	}
	return out, nil
}

func (p *parser) parseProcessSnapshot(v any) (ProcessSnapshot, error) {
	o, err := p.object(v)
	if err != nil {
		return ProcessSnapshot{}, err
	}
	if err := p.keys(o, []string{"totalProcesses", "truncated", "inaccessible", "processes"}, nil); err != nil {
		return ProcessSnapshot{}, err
	}
	var out ProcessSnapshot
	if out.Truncated, err = p.child("truncated").boolean(o["truncated"]); err != nil {
		return ProcessSnapshot{}, err
	}
	if out.TotalProcesses, err = p.child("totalProcesses").integer(o["totalProcesses"], 0, 10_000_000); err != nil {
		return ProcessSnapshot{}, err
	}
	if out.Inaccessible, err = p.child("inaccessible").integer(o["inaccessible"], 0, 10_000_000); err != nil {
		return ProcessSnapshot{}, err
	}
	raw, err := p.child("processes").array(o["processes"], 0, MaxProcessRows)
	if err != nil {
		return ProcessSnapshot{}, err
	}
	out.Processes = make([]ProcessRow, len(raw))
	for i, item := range raw {
		row, err := p.child("processes").at(i).parseProcessRow(item)
		if err != nil {
			return ProcessSnapshot{}, err
		}
		out.Processes[i] = row
	}
	return out, nil
}

func (p *parser) parseProcessRow(v any) (ProcessRow, error) {
	o, err := p.object(v)
	if err != nil {
		return ProcessRow{}, err
	}
	if err := p.keys(
		o,
		[]string{"pid", "ppid", "startTicks", "name", "user", "state", "cpuPercent", "memoryRssBytes", "gpuMemoryBytes", "ioReadBytesPerSec", "ioWriteBytesPerSec", "threads", "unit"},
		[]string{"command"},
	); err != nil {
		return ProcessRow{}, err
	}
	var out ProcessRow
	if out.PID, err = p.child("pid").integer(o["pid"], 0, 1<<31); err != nil {
		return ProcessRow{}, err
	}
	if out.PPID, err = p.child("ppid").integer(o["ppid"], 0, 1<<31); err != nil {
		return ProcessRow{}, err
	}
	if out.StartTicks, err = p.child("startTicks").integer(o["startTicks"], 0, MaxSafeInteger); err != nil {
		return ProcessRow{}, err
	}
	if out.Name, err = p.child("name").text(o["name"], 0, MaxTextLength); err != nil {
		return ProcessRow{}, err
	}
	if out.User, err = p.child("user").nullableText(o["user"], 0, 64); err != nil {
		return ProcessRow{}, err
	}
	if out.State, err = p.child("state").text(o["state"], 0, 16); err != nil {
		return ProcessRow{}, err
	}
	if out.CPUPercent, err = p.child("cpuPercent").nullableNumber(o["cpuPercent"], 0, 100*MaxCores); err != nil {
		return ProcessRow{}, err
	}
	if out.MemoryRSSBytes, err = p.child("memoryRssBytes").nullableInteger(o["memoryRssBytes"], 0, MaxSafeInteger); err != nil {
		return ProcessRow{}, err
	}
	if out.GpuMemoryBytes, err = p.child("gpuMemoryBytes").nullableInteger(o["gpuMemoryBytes"], 0, MaxSafeInteger); err != nil {
		return ProcessRow{}, err
	}
	if out.IOReadBytesPerSec, err = p.child("ioReadBytesPerSec").nullableNumber(o["ioReadBytesPerSec"], 0, 1e13); err != nil {
		return ProcessRow{}, err
	}
	if out.IOWriteBytesPerSec, err = p.child("ioWriteBytesPerSec").nullableNumber(o["ioWriteBytesPerSec"], 0, 1e13); err != nil {
		return ProcessRow{}, err
	}
	if out.Threads, err = p.child("threads").nullableInteger(o["threads"], 0, 10_000_000); err != nil {
		return ProcessRow{}, err
	}
	if out.Unit, err = p.child("unit").nullableText(o["unit"], 1, MaxTextLength); err != nil {
		return ProcessRow{}, err
	}
	if command, ok := o["command"]; ok {
		text, err := p.child("command").text(command, 0, 4096)
		if err != nil {
			return ProcessRow{}, err
		}
		out.Command = &text
	}
	return out, nil
}

func (p *parser) parseWelcome(v any) (WelcomeMessage, error) {
	o, err := p.object(v)
	if err != nil {
		return WelcomeMessage{}, err
	}
	if err := p.keys(
		o,
		[]string{"type", "hubRunId", "machineId", "delivery", "maxInFlightBatches"},
		[]string{"protocol"},
	); err != nil {
		return WelcomeMessage{}, err
	}
	var out WelcomeMessage
	delivery, err := p.child("delivery").oneOf(o["delivery"], string(DeliveryDurable), string(DeliveryMemory))
	if err != nil {
		return WelcomeMessage{}, err
	}
	out.Delivery = DeliveryMode(delivery)
	if out.HubRunID, err = p.child("hubRunId").id(o["hubRunId"]); err != nil {
		return WelcomeMessage{}, err
	}
	if out.MachineID, err = p.child("machineId").id(o["machineId"]); err != nil {
		return WelcomeMessage{}, err
	}
	if out.MaxInFlightBatches, err = p.child("maxInFlightBatches").integer(o["maxInFlightBatches"], 1, MaxInFlightBatches); err != nil {
		return WelcomeMessage{}, err
	}
	if protocol, ok := o["protocol"]; ok {
		version, err := p.child("protocol").integer(protocol, 1, MaxSafeInteger)
		if err != nil {
			return WelcomeMessage{}, err
		}
		out.Protocol = &version
	}
	return out, nil
}

func (p *parser) parseAck(v any) (AckMessage, error) {
	o, err := p.object(v)
	if err != nil {
		return AckMessage{}, err
	}
	if err := p.keys(o, []string{"type", "accepted", "rejected"}, []string{"retryAfterMs"}); err != nil {
		return AckMessage{}, err
	}
	rawAccepted, err := p.child("accepted").array(o["accepted"], 0, MaxAckRecords)
	if err != nil {
		return AckMessage{}, err
	}
	accepted := make([]RecordRef, len(rawAccepted))
	for i, item := range rawAccepted {
		ref, err := p.child("accepted").at(i).parseRecordRef(item, nil)
		if err != nil {
			return AckMessage{}, err
		}
		accepted[i] = ref
	}
	rawRejected, err := p.child("rejected").array(o["rejected"], 0, MaxAckRecords)
	if err != nil {
		return AckMessage{}, err
	}
	rejected := make([]RejectedRecord, len(rawRejected))
	for i, item := range rawRejected {
		base := p.child("rejected").at(i)
		ref, err := base.parseRecordRef(item, []string{"reason"})
		if err != nil {
			return AckMessage{}, err
		}
		fields := item.(map[string]any)
		reason, err := base.child("reason").text(fields["reason"], 0, MaxTextLength)
		if err != nil {
			return AckMessage{}, err
		}
		rejected[i] = RejectedRecord{RecordRef: ref, Reason: reason}
	}
	out := AckMessage{Accepted: accepted, Rejected: rejected}
	if retry, ok := o["retryAfterMs"]; ok {
		value, err := p.child("retryAfterMs").integer(retry, 0, 600_000)
		if err != nil {
			return AckMessage{}, err
		}
		out.RetryAfterMS = &value
	}
	return out, nil
}

// parseRecordRef validates an ack record reference with optional extra
// required keys (used by the rejected list).
func (p *parser) parseRecordRef(v any, extra []string) (RecordRef, error) {
	o, err := p.object(v)
	if err != nil {
		return RecordRef{}, err
	}
	required := append([]string{"runId", "sequence"}, extra...)
	if err := p.keys(o, required, nil); err != nil {
		return RecordRef{}, err
	}
	var out RecordRef
	if out.RunID, err = p.child("runId").id(o["runId"]); err != nil {
		return RecordRef{}, err
	}
	if out.Sequence, err = p.child("sequence").integer(o["sequence"], 0, MaxSafeInteger); err != nil {
		return RecordRef{}, err
	}
	return out, nil
}

// mapValue returns a field from a decoded object; parseRecordRef already
// rejected non-objects.
func mapValue(v any, key string) any {
	if o, ok := v.(map[string]any); ok {
		return o[key]
	}
	return nil
}

func (p *parser) parseDetailSubscribe(v any) (DetailSubscribe, error) {
	o, err := p.object(v)
	if err != nil {
		return DetailSubscribe{}, err
	}
	if err := p.keys(o, []string{"type", "subscriptionId", "kind", "intervalMs", "leaseMs"}, nil); err != nil {
		return DetailSubscribe{}, err
	}
	var out DetailSubscribe
	kind, err := p.child("kind").oneOf(o["kind"], string(DetailKindProcesses))
	if err != nil {
		return DetailSubscribe{}, err
	}
	out.Kind = DetailKind(kind)
	if out.SubscriptionID, err = p.child("subscriptionId").id(o["subscriptionId"]); err != nil {
		return DetailSubscribe{}, err
	}
	if out.IntervalMS, err = p.child("intervalMs").integer(o["intervalMs"], MinDetailIntervalMS, MaxDetailIntervalMS); err != nil {
		return DetailSubscribe{}, err
	}
	if out.LeaseMS, err = p.child("leaseMs").integer(o["leaseMs"], MinDetailLeaseMS, MaxDetailLeaseMS); err != nil {
		return DetailSubscribe{}, err
	}
	return out, nil
}

func (p *parser) parseDetailCancel(v any) (DetailCancel, error) {
	o, err := p.object(v)
	if err != nil {
		return DetailCancel{}, err
	}
	if err := p.keys(o, []string{"type", "subscriptionId"}, nil); err != nil {
		return DetailCancel{}, err
	}
	var out DetailCancel
	if out.SubscriptionID, err = p.child("subscriptionId").id(o["subscriptionId"]); err != nil {
		return DetailCancel{}, err
	}
	return out, nil
}

// parseCollectorMessageValue dispatches on the message type.
func parseCollectorMessageValue(v any) (CollectorMessage, error) {
	o, err := (&parser{}).object(v)
	if err != nil {
		return nil, err
	}
	typ, _ := o["type"].(string)
	switch typ {
	case "hello":
		return newParser().child("hello").parseHello(v)
	case "samples":
		return newParser().child("samples").parseSamples(v)
	case "inventory":
		return newParser().child("inventory").parseInventoryMessage(v)
	case "status":
		return newParser().child("status").parseStatusMessage(v)
	case "detail.result":
		return newParser().child("detail.result").parseDetailResult(v)
	case "detail.ended":
		return newParser().child("detail.ended").parseDetailEnded(v)
	default:
		return nil, fmt.Errorf("invalid collector message: unsupported type %s", describe(o["type"]))
	}
}

// parseHubMessageValue dispatches on the message type.
func parseHubMessageValue(v any) (HubMessage, error) {
	o, err := (&parser{}).object(v)
	if err != nil {
		return nil, err
	}
	typ, _ := o["type"].(string)
	switch typ {
	case "welcome":
		return newParser().child("welcome").parseWelcome(v)
	case "ack":
		return newParser().child("ack").parseAck(v)
	case "detail.subscribe":
		return newParser().child("detail.subscribe").parseDetailSubscribe(v)
	case "detail.cancel":
		return newParser().child("detail.cancel").parseDetailCancel(v)
	default:
		return nil, fmt.Errorf("invalid hub message: unsupported type %s", describe(o["type"]))
	}
}
