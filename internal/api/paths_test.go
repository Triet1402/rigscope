package api

import (
	"strings"
	"testing"
)

func TestEndpointsAreWellFormed(t *testing.T) {
	endpoints := Endpoints()
	seen := make(map[string]struct{}, len(endpoints))
	validAuth := map[AuthRequirement]bool{
		AuthNone: true, AuthSession: true, AuthAdmin: true, AuthGrant: true, AuthCollector: true,
	}
	for _, endpoint := range endpoints {
		if !strings.HasPrefix(endpoint.Path, "/api/") {
			t.Errorf("%s %s: path must start with /api/", endpoint.Method, endpoint.Path)
		}
		if !validAuth[endpoint.Auth] {
			t.Errorf("%s %s: unknown auth requirement %q", endpoint.Method, endpoint.Path, endpoint.Auth)
		}
		key := endpoint.Method + " " + endpoint.Path
		if _, duplicate := seen[key]; duplicate {
			t.Errorf("duplicate endpoint %s", key)
		}
		seen[key] = struct{}{}
	}
}

func TestEndpointsAndMetricsAreDefensiveCopies(t *testing.T) {
	endpoints := Endpoints()
	endpoints[0].Path = "/mutated"
	if Endpoints()[0].Path == "/mutated" {
		t.Fatal("Endpoints must return a defensive copy")
	}
	metrics := KnownMetrics()
	metrics["mutated.metric"] = MetricUnitPercent
	delete(metrics, "cpu.usage")
	if KnownMetric("mutated.metric") || !KnownMetric("cpu.usage") {
		t.Fatal("KnownMetrics must return a defensive copy")
	}
}

func TestKnownMetricsMatchesAlertMetrics(t *testing.T) {
	metrics := KnownMetrics()
	if len(metrics) != MaxHistoryMetrics {
		t.Fatalf("KnownMetrics has %d entries, MaxHistoryMetrics is %d",
			len(metrics), MaxHistoryMetrics)
	}
	for name := range metrics {
		if err := AlertMetric(name).validate(); err != nil {
			t.Errorf("alert metric %q: %v", name, err)
		}
	}
	for _, metric := range []AlertMetric{
		AlertMetricCPUUsage, AlertMetricMemoryUsed, AlertMetricGPUUtilization,
		AlertMetricFSUsage, AlertMetricDiskRead, AlertMetricNetRx,
	} {
		if !KnownMetric(string(metric)) {
			t.Errorf("alert metric %q is not in KnownMetrics", metric)
		}
	}
}
