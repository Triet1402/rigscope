package protocol

import (
	"testing"
	"time"
)

func TestTimestampGrammar(t *testing.T) {
	valid := []string{
		"2026-11-12T10:00:00Z",
		"2026-11-12T10:00:00.5Z",
		"2026-11-12T10:00:00.12Z",
		"2026-11-12T10:00:00.123456789Z",
		"2026-11-12T10:00:00.123456789+05:30",
		"2026-11-12T10:00:00-23:59",
		"2000-01-01T00:00:00Z",
		"2199-12-31T23:59:59.999Z",
	}
	for _, value := range valid {
		if _, ok := parseTimestampValue(value); !ok {
			t.Errorf("%q should be accepted", value)
		}
	}

	invalid := []string{
		"2026-11-12T10:00Z",                  // missing seconds
		"2026-11-12T10:00:00,123Z",           // comma fraction
		"2026-11-12T10:00:00+24:00",          // offset hour out of range
		"2026-11-12T10:00:00+23:60",          // offset minute out of range
		"2026-11-12T10:00:00",                // missing zone
		"2026-11-12 10:00:00Z",               // space instead of T
		"2026-11-12t10:00:00z",               // lowercase separators
		"Thu, 12 Nov 2026 10:00:00 PST",      // RFC 1123 must not be guessed
		"2026-02-30T10:00:00Z",               // invalid calendar day
		"2026-13-01T10:00:00Z",               // invalid month
		"2026-11-12T24:00:00Z",               // invalid hour
		"2026-11-12T10:60:00Z",               // invalid minute
		"2026-11-12T10:00:60Z",               // invalid second
		"2026-11-12T10:00:00.Z",              // empty fraction
		"2026-11-12T10:00:00.1234567890Z",    // ten fraction digits
		"2026-11-12T10:00:00.123456789+05:3", // short offset
		"1999-12-31T23:59:59Z",               // before the window
		"2200-01-01T00:00:01Z",               // after the window
	}
	for _, value := range invalid {
		if parsed, ok := parseTimestampValue(value); ok {
			t.Errorf("%q should be rejected, got %s", value, parsed)
		}
	}
}

func TestTimestampMillisecondTruncation(t *testing.T) {
	want := time.Date(2026, 11, 12, 10, 0, 0, 123_000_000, time.UTC)
	for _, value := range []string{
		"2026-11-12T10:00:00.123Z",
		"2026-11-12T10:00:00.1234Z",
		"2026-11-12T10:00:00.123456789Z",
	} {
		got, ok := parseTimestampValue(value)
		if !ok {
			t.Fatalf("%q should be accepted", value)
		}
		if !got.Equal(want) {
			t.Fatalf("%q parsed to %s, want %s", value, got, want)
		}
	}

	got, ok := parseTimestampValue("2026-11-12T10:00:00.1Z")
	if !ok || !got.Equal(time.Date(2026, 11, 12, 10, 0, 0, 100_000_000, time.UTC)) {
		t.Fatalf("one fraction digit should mean tenths of a second, got %s", got)
	}

	// An offset is applied after truncation.
	got, ok = parseTimestampValue("2026-11-12T10:00:00.1239+01:00")
	want = time.Date(2026, 11, 12, 9, 0, 0, 123_000_000, time.UTC)
	if !ok || !got.Equal(want) {
		t.Fatalf("offset not applied at millisecond precision: got %s, want %s", got, want)
	}
}

func TestGapOrderingUsesMillisecondPrecision(t *testing.T) {
	record := func(from, to string) string {
		return `{"collectorId":"collector-a","runId":"run-1","bootId":"boot-123","sequence":1,` +
			`"capturedAt":"2026-11-12T10:00:00.000Z","kind":"gap","payload":{"fromCapturedAt":"` + from +
			`","toCapturedAt":"` + to + `","droppedRecords":1,"reason":"rejected"}}`
	}

	// Sub-millisecond differences truncate to the same instant and are ordered
	// equal, so this reversed pair is accepted.
	if _, err := ParseTelemetryRecord([]byte(record(
		"2026-11-12T10:00:00.1239Z",
		"2026-11-12T10:00:00.1231Z",
	))); err != nil {
		t.Fatalf("sub-millisecond ordering should compare equal: %v", err)
	}
	// The same reversal across a millisecond boundary is out of order.
	if _, err := ParseTelemetryRecord([]byte(record(
		"2026-11-12T10:00:00.1241Z",
		"2026-11-12T10:00:00.1239Z",
	))); err == nil {
		t.Fatal("a reversed pair spanning a millisecond must be rejected")
	}
}

func TestDecoderRejectsLoneSurrogates(t *testing.T) {
	cases := []string{
		`{"type":"detail.cancel","subscriptionId":"\ud800"}`,
		`{"type":"detail.cancel","subscriptionId":"\udfff"}`,
		`{"type":"detail.cancel","subscriptionId":"\udc00"}`,
	}
	for _, data := range cases {
		if _, err := DecodeHubMessage([]byte(data)); err == nil {
			t.Fatalf("lone surrogate should be rejected: %s", data)
		}
	}

	// A valid surrogate pair is a well-formed scalar value.
	message, err := DecodeHubMessage([]byte(`{"type":"detail.cancel","subscriptionId":"\ud83d\ude00"}`))
	if err != nil {
		t.Fatalf("a surrogate pair should be accepted: %v", err)
	}
	if cancel := message.(DetailCancel); cancel.SubscriptionID != "\U0001F600" {
		t.Fatalf("unexpected decoded id %q", cancel.SubscriptionID)
	}

	// An escaped backslash is literal text, not a unicode escape.
	message, err = DecodeHubMessage([]byte(`{"type":"detail.cancel","subscriptionId":"a\\ud800"}`))
	if err != nil {
		t.Fatalf("an escaped backslash must not be read as a surrogate escape: %v", err)
	}
	if cancel := message.(DetailCancel); cancel.SubscriptionID != `a\ud800` {
		t.Fatalf("unexpected decoded id %q", cancel.SubscriptionID)
	}
}
