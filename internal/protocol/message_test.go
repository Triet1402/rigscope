package protocol

import (
	"encoding/json"
	"testing"
)

func TestConcreteUnmarshalValidatesType(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		target  func() any
	}{
		{"hello", "collector-hello-minimal", func() any { return new(HelloMessage) }},
		{"samples", "collector-samples-gap", func() any { return new(SamplesMessage) }},
		{"inventory", "collector-inventory-minimal", func() any { return new(InventoryMessage) }},
		{"status", "collector-status-no-queue-mode", func() any { return new(StatusMessage) }},
		{"detail.result", "collector-detail-result-empty", func() any { return new(DetailResult) }},
		{"detail.ended", "collector-detail-ended-expired", func() any { return new(DetailEnded) }},
		{"welcome", "hub-welcome-durable", func() any { return new(WelcomeMessage) }},
		{"ack", "hub-ack-empty", func() any { return new(AckMessage) }},
		{"detail.subscribe", "hub-detail-subscribe-min", func() any { return new(DetailSubscribe) }},
		{"detail.cancel", "hub-detail-cancel", func() any { return new(DetailCancel) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := readFixture(t, "valid", tc.fixture)
			if err := json.Unmarshal(data, tc.target()); err != nil {
				t.Fatalf("valid fixture: %v", err)
			}
			var object map[string]any
			if err := json.Unmarshal(data, &object); err != nil {
				t.Fatal(err)
			}
			original, _ := object["type"].(string)
			for _, bad := range []any{"not-" + original, nil, 7} {
				object["type"] = bad
				mutated, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(mutated, tc.target()); err == nil {
					t.Fatalf("type %#v was accepted", bad)
				}
			}
		})
	}
}
