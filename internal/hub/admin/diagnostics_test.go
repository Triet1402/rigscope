package admin

import (
	"net/http"
	"testing"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

func TestAlertDiagnosticsRouteRequiresAdminAndReturnsShape(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "scoped", PasswordHash: "test:scoped-password", Scope: storage.MachineScope("m-1"), Admin: true},
		{Username: "viewer", PasswordHash: "test:viewer-password", Scope: storage.AllMachines()},
	}, nil)
	harness.mu.Lock()
	harness.alertDiagnostics = api.AlertDiagnostics{
		GeneratedAt:             "2026-11-12T10:00:00.000Z",
		DroppedObservations:     3,
		ObservationsAbandoned:   2,
		ContinuityResets:        4,
		ContinuityResetFailures: 1,
		UnresolvedBreaks:        []string{"m-1", "outside-secret"},
		QueueDepth:              5,
		QueueCapacity:           256,
		LastTickAt:              stringPointer("2026-11-12T09:59:59.000Z"),
	}
	harness.mu.Unlock()

	if response, _ := harness.request(t, http.MethodGet, alertDiagnostics, nil, nil); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", response.StatusCode)
	}
	viewerCookie, _, _ := harness.login(t, "viewer", "viewer-password")
	if response, _ := harness.adminRequest(t, http.MethodGet, alertDiagnostics, nil, viewerCookie, ""); response.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer status = %d", response.StatusCode)
	}

	// A scoped administrator must not learn machine ids outside its scope.
	scopedCookie, _, _ := harness.login(t, "scoped", "scoped-password")
	response, body := harness.adminRequest(t, http.MethodGet, alertDiagnostics, nil, scopedCookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("scoped admin status = %d body = %s", response.StatusCode, body)
	}
	scoped, err := api.DecodeAlertDiagnostics(body)
	if err != nil {
		t.Fatalf("contract decode: %v", err)
	}
	if len(scoped.UnresolvedBreaks) != 1 || scoped.UnresolvedBreaks[0] != "m-1" {
		t.Fatalf("scoped unresolved breaks = %v, want [m-1]", scoped.UnresolvedBreaks)
	}
	if scoped.DroppedObservations != 3 || scoped.ContinuityResetFailures != 1 {
		t.Fatalf("scoped global counters = %+v", scoped)
	}

	ownerCookie, ownerCSRF, _ := harness.login(t, "owner", "owner-password")
	response, body = harness.adminRequest(t, http.MethodGet, alertDiagnostics, nil, ownerCookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d body = %s", response.StatusCode, body)
	}
	decoded, err := api.DecodeAlertDiagnostics(body)
	if err != nil {
		t.Fatalf("contract decode: %v", err)
	}
	if decoded.DroppedObservations != 3 || decoded.ObservationsAbandoned != 2 ||
		decoded.ContinuityResets != 4 || decoded.ContinuityResetFailures != 1 {
		t.Fatalf("diagnostics = %+v", decoded)
	}
	if len(decoded.UnresolvedBreaks) != 2 || decoded.UnresolvedBreaks[0] != "m-1" ||
		decoded.UnresolvedBreaks[1] != "outside-secret" {
		t.Fatalf("wildcard unresolved breaks = %v", decoded.UnresolvedBreaks)
	}
	if decoded.QueueDepth != 5 || decoded.QueueCapacity != 256 ||
		decoded.LastTickAt == nil || *decoded.LastTickAt != "2026-11-12T09:59:59.000Z" {
		t.Fatalf("queue/tick diagnostics = %+v", decoded)
	}

	post, _ := harness.adminRequest(t, http.MethodPost, alertDiagnostics, map[string]any{}, ownerCookie, ownerCSRF)
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", post.StatusCode)
	}
	if allow := post.Header.Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q", allow)
	}

	harness.handler.alertDiagnostics = nil
	unavailable, _ := harness.adminRequest(t, http.MethodGet, alertDiagnostics, nil, ownerCookie, "")
	if unavailable.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d", unavailable.StatusCode)
	}
}

func stringPointer(value string) *string { return &value }
