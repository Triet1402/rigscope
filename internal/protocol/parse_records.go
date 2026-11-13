package protocol

// parseRecordEnvelope checks the shared record identity fields.
func (p *parser) parseRecordEnvelope(o map[string]any) (RecordEnvelope, error) {
	var env RecordEnvelope
	var err error
	if env.CollectorID, err = p.child("collectorId").id(o["collectorId"]); err != nil {
		return RecordEnvelope{}, err
	}
	if env.RunID, err = p.child("runId").id(o["runId"]); err != nil {
		return RecordEnvelope{}, err
	}
	if env.BootID, err = p.child("bootId").text(o["bootId"], 0, MaxIDLength); err != nil {
		return RecordEnvelope{}, err
	}
	if env.Sequence, err = p.child("sequence").integer(o["sequence"], 0, MaxSafeInteger); err != nil {
		return RecordEnvelope{}, err
	}
	if env.CapturedAt, err = p.child("capturedAt").timestamp(o["capturedAt"]); err != nil {
		return RecordEnvelope{}, err
	}
	return env, nil
}

// parseTelemetryRecord strictly validates one metrics or gap record.
func (p *parser) parseTelemetryRecord(v any) (TelemetryRecord, error) {
	o, err := p.object(v)
	if err != nil {
		return nil, err
	}
	if err := p.keys(
		o,
		[]string{"collectorId", "runId", "bootId", "sequence", "capturedAt", "kind", "payload"},
		nil,
	); err != nil {
		return nil, err
	}
	env, err := p.parseRecordEnvelope(o)
	if err != nil {
		return nil, err
	}
	kind, err := p.child("kind").oneOf(o["kind"], string(RecordKindMetrics), string(RecordKindGap))
	if err != nil {
		return nil, err
	}
	if kind == string(RecordKindMetrics) {
		payload, err := p.child("payload").parseMetricsPayload(o["payload"])
		if err != nil {
			return nil, err
		}
		return MetricsRecord{RecordEnvelope: env, Payload: payload}, nil
	}
	payload, err := p.child("payload").parseGapPayload(o["payload"])
	if err != nil {
		return nil, err
	}
	return GapRecord{RecordEnvelope: env, Payload: payload}, nil
}

func (p *parser) parseGapPayload(v any) (GapPayload, error) {
	o, err := p.object(v)
	if err != nil {
		return GapPayload{}, err
	}
	if err := p.keys(o, []string{"fromCapturedAt", "toCapturedAt", "droppedRecords", "reason"}, nil); err != nil {
		return GapPayload{}, err
	}
	var out GapPayload
	if out.FromCapturedAt, err = p.child("fromCapturedAt").timestamp(o["fromCapturedAt"]); err != nil {
		return GapPayload{}, err
	}
	if out.ToCapturedAt, err = p.child("toCapturedAt").timestamp(o["toCapturedAt"]); err != nil {
		return GapPayload{}, err
	}
	from, _ := parseTimestampValue(out.FromCapturedAt)
	to, _ := parseTimestampValue(out.ToCapturedAt)
	if from.After(to) {
		return GapPayload{}, p.errf("fromCapturedAt after toCapturedAt")
	}
	if out.DroppedRecords, err = p.child("droppedRecords").integer(o["droppedRecords"], 1, MaxSafeInteger); err != nil {
		return GapPayload{}, err
	}
	reason, err := p.child("reason").oneOf(
		o["reason"],
		string(GapOutboxAge),
		string(GapOutboxBytes),
		string(GapDiskBudget),
		string(GapWriteFailure),
		string(GapRejected),
	)
	if err != nil {
		return GapPayload{}, err
	}
	out.Reason = GapReason(reason)
	return out, nil
}
