package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

func TestSettingsRouteIsAdminOnlyReadOnlyAndScoped(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "scoped", PasswordHash: "test:scoped-password", Scope: storage.MachineScope("m-1"), Admin: true},
		{Username: "viewer", PasswordHash: "test:viewer-password", Scope: storage.AllMachines()},
	}, nil)
	destination := "ws://127.0.0.1:8081/api/collector/connect"
	harness.handler.settings = func(context.Context) (api.HubSettings, error) {
		return api.HubSettings{
			GeneratedAt: "2026-11-12T10:00:00.000Z",
			Scope:       api.InstallScopeSystem,
			Storage:     api.HubStorageSettings{Mode: api.HubStorageMemory, DiskBytes: 4096, MemoryBytes: 1024, MemoryBudgetBytes: 2048},
			Network: api.HubNetworkSettings{
				URL: "http://127.0.0.1:8081", Listen: "127.0.0.1", Port: 8081,
				TrustedProxies: []string{}, ForwardedHeaders: api.ForwardedHeaderXForwarded,
			},
			LocalCollector: &api.LocalCollectorSettings{
				SamplingIntervalMs: 2000, Destination: &destination,
				Shared: []string{"cpu"}, Withheld: []string{"process-arguments"},
			},
		}, nil
	}

	if response, _ := harness.request(t, http.MethodGet, settingsPath, nil, nil); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", response.StatusCode)
	}
	viewerCookie, _, _ := harness.login(t, "viewer", "viewer-password")
	if response, _ := harness.adminRequest(t, http.MethodGet, settingsPath, nil, viewerCookie, ""); response.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer status = %d", response.StatusCode)
	}

	ownerCookie, ownerCSRF, _ := harness.login(t, "owner", "owner-password")
	response, body := harness.adminRequest(t, http.MethodGet, settingsPath, nil, ownerCookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d body = %s", response.StatusCode, body)
	}
	decoded, err := api.DecodeHubSettings(body)
	if err != nil {
		t.Fatalf("contract decode: %v", err)
	}
	if decoded.Scope != api.InstallScopeSystem || decoded.Storage.Mode != api.HubStorageMemory || decoded.LocalCollector == nil {
		t.Fatalf("settings = %+v", decoded)
	}

	// A machine-scoped administrator does not see the local machine's policy.
	scopedCookie, _, _ := harness.login(t, "scoped", "scoped-password")
	_, body = harness.adminRequest(t, http.MethodGet, settingsPath, nil, scopedCookie, "")
	if scoped, err := api.DecodeHubSettings(body); err != nil || scoped.LocalCollector != nil {
		t.Fatalf("scoped settings = %+v, %v", scoped, err)
	}

	// There is no write path: the hub cannot change or restart itself.
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		write, _ := harness.adminRequest(t, method, settingsPath, map[string]any{"mode": "persistent"}, ownerCookie, ownerCSRF)
		if write.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d", method, write.StatusCode)
		}
	}

	harness.handler.settings = func(context.Context) (api.HubSettings, error) {
		return api.HubSettings{GeneratedAt: "bad"}, nil
	}
	if invalid, body := harness.adminRequest(t, http.MethodGet, settingsPath, nil, ownerCookie, ""); invalid.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(body), "bad") {
		t.Fatalf("invalid payload status = %d body = %s", invalid.StatusCode, body)
	}
	harness.handler.settings = nil
	if unavailable, _ := harness.adminRequest(t, http.MethodGet, settingsPath, nil, ownerCookie, ""); unavailable.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d", unavailable.StatusCode)
	}
}
