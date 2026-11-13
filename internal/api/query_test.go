package api

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
)

func TestParseHistoryQuery(t *testing.T) {
	query, err := ParseHistoryQuery(url.Values{
		"machineId": {"machine-1"},
		"from":      {"1000"},
		"to":        {"61000"},
	})
	if err != nil {
		t.Fatalf("parse default query: %v", err)
	}
	if len(query.Metrics) != 2 || query.Metrics[0] != "cpu.usage" || query.MaxPoints != DefaultHistoryPoints {
		t.Fatalf("unexpected defaults: %+v", query)
	}

	query, err = ParseHistoryQuery(url.Values{
		"machineId":   {"machine-1"},
		"metrics":     {"cpu.core,cpu.usage"},
		"resourceIds": {"core:0,core:1"},
		"coreIds":     {"core:0,,core:1"},
		"from":        {"0"},
		"to":          {"86400000"},
		"maxPoints":   {"999999"},
	})
	if err != nil {
		t.Fatalf("parse full query: %v", err)
	}
	if query.MaxPoints != MaxHistoryPoints {
		t.Fatalf("maxPoints must clamp to %d, got %d", MaxHistoryPoints, query.MaxPoints)
	}
	if query.CoreIDs == nil || len(*query.CoreIDs) != 2 {
		t.Fatalf("empty core ids must be dropped, got %v", query.CoreIDs)
	}

	roundTrip, err := ParseHistoryQuery(query.QueryValue())
	if err != nil {
		t.Fatalf("re-parse rendered query: %v", err)
	}
	if roundTrip.MachineID != query.MachineID || roundTrip.FromMs != query.FromMs ||
		roundTrip.ToMs != query.ToMs || roundTrip.MaxPoints != query.MaxPoints {
		t.Fatalf("query did not round-trip: %+v", roundTrip)
	}
}

func TestParseHistoryQueryDistinguishesAbsentAndEmptyCoreIDs(t *testing.T) {
	absent, err := ParseHistoryQuery(url.Values{
		"machineId": {"machine-1"},
		"from":      {"0"},
		"to":        {"1000"},
	})
	if err != nil {
		t.Fatalf("parse absent coreIds: %v", err)
	}
	if absent.CoreIDs != nil {
		t.Fatalf("absent coreIds must stay nil, got %v", *absent.CoreIDs)
	}
	if values := absent.QueryValue(); values.Has("coreIds") {
		t.Fatalf("an absent coreIds must not render a query parameter: %v", values)
	}

	empty, err := ParseHistoryQuery(url.Values{
		"machineId": {"machine-1"},
		"coreIds":   {""},
		"from":      {"0"},
		"to":        {"1000"},
	})
	if err != nil {
		t.Fatalf("parse explicit empty coreIds: %v", err)
	}
	if empty.CoreIDs == nil || len(*empty.CoreIDs) != 0 {
		t.Fatalf("explicit empty coreIds must stay present and empty, got %v", empty.CoreIDs)
	}
	if values := empty.QueryValue(); !values.Has("coreIds") || values.Get("coreIds") != "" {
		t.Fatalf("an explicit empty coreIds must render coreIds=, got %v", values)
	}
	rendered := empty.QueryValue()
	reparsed, err := ParseHistoryQuery(rendered)
	if err != nil {
		t.Fatalf("re-parse explicit empty coreIds: %v", err)
	}
	if reparsed.CoreIDs == nil || len(*reparsed.CoreIDs) != 0 {
		t.Fatalf("explicit empty coreIds did not round-trip: %v", reparsed.CoreIDs)
	}
}

func TestParseHistoryQueryRejectsBadInput(t *testing.T) {
	cases := []url.Values{
		{"from": {"0"}, "to": {"1000"}},
		{"machineId": {"m"}, "metrics": {""}, "from": {"0"}, "to": {"1000"}},
		{"machineId": {"m"}, "metrics": {"cpu.usage,cpu.usage"}, "from": {"0"}, "to": {"1000"}},
		{"machineId": {"m"}, "metrics": {"nope"}, "from": {"0"}, "to": {"1000"}},
		{"machineId": {"m"}, "metrics": {"cpu.usage"}, "from": {"1.5"}, "to": {"1000"}},
		{"machineId": {"m"}, "metrics": {"cpu.usage"}, "from": {"1000"}, "to": {"1000"}},
		{"machineId": {"m"}, "metrics": {"cpu.usage"}, "from": {"0"}, "to": {"999999999999999"}},
		{"machineId": {"m"}, "metrics": {"cpu.usage"}, "from": {"0"}, "to": {"1000"}, "maxPoints": {"0"}},
		{"machineId": {"m"}, "metrics": {"cpu.usage"}, "from": {"0"}, "to": {"1000"}, "coreIds": {"core:1024"}},
		{"machineId": {"m"}, "metrics": {"cpu.usage"}, "from": {"0"}, "to": {"1000"}, "resourceIds": {"a,a"}},
	}
	for _, values := range cases {
		_, err := ParseHistoryQuery(values)
		var input *InputError
		if !errors.As(err, &input) || input.Status != http.StatusBadRequest {
			t.Fatalf("expected a 400 InputError for %v, got %v", values, err)
		}
	}
}
