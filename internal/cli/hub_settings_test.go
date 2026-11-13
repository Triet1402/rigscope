package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

func TestHubSettingsSummaryIsValidAndPathFree(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"memory", "persistent"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			database, err := storage.Open(ctx, filepath.Join(dir, "hub.db"), storage.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			telemetry, err := storage.NewTelemetry(ctx, database, storage.TelemetryOptions{Memory: mode == "memory"})
			if err != nil {
				t.Fatal(err)
			}
			defer telemetry.Close(ctx)

			cfg := config.Default()
			cfg.Components.Hub.TelemetryMode = mode
			cfg.Components.Hub.Listen = "0.0.0.0"
			cfg.Components.Hub.Origin = "https://hub.example.com"
			cfg.Components.Hub.TLSCert = "/etc/ssl/rigscope/hub-secret-name.crt"
			cfg.Components.Hub.TLSKey = "/etc/ssl/rigscope/hub-secret-name.key"
			cfg.Components.Collector.Sharing.Processes = false
			cfg.Components.Collector.Sharing.ProcessArguments = true

			summary := hubSettingsSummary(config.ScopeSystem, cfg, database, telemetry, time.Now())
			body, err := api.EncodeHubSettings(summary)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if strings.Contains(string(body), "hub-secret-name") {
				t.Fatalf("summary exposes TLS file paths: %s", body)
			}
			if string(summary.Storage.Mode) != mode || summary.Scope != api.InstallScopeSystem || !summary.Network.TLS || summary.Storage.DiskBytes <= 0 {
				t.Fatalf("summary = %+v", summary)
			}
			if mode == "memory" && summary.Storage.MemoryBudgetBytes <= summary.Storage.MemoryBytes {
				t.Fatalf("memory budget = %+v", summary.Storage)
			}
			local := summary.LocalCollector
			if local == nil || local.SamplingIntervalMs != 2000 || strings.Join(local.Withheld, ",") != "network-addresses,processes,process-arguments" {
				t.Fatalf("local collector = %+v", local)
			}
		})
	}
	cfg := config.Default()
	cfg.Components.Collector.Enabled = false
	if summary := hubSettingsSummary(config.ScopeUser, cfg, nil, nil, time.Now()); summary.LocalCollector != nil || summary.Scope != api.InstallScopeUser {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestHubSettingsReadsCollectorChangesWithoutChangingHubSnapshot(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	running := loadConfig(t, paths)
	f.resetCalls()
	if code := f.run(t, "configure", "--share", "processes=off", "--sampling-interval", "5s", "--yes"); code != 0 {
		t.Fatalf("configure: %s", f.err.String())
	}
	summary, err := hubSettingsFromFile(config.ScopeUser, paths.ConfigFile, running, nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if summary.LocalCollector == nil || summary.LocalCollector.SamplingIntervalMs != 5000 || !strings.Contains(strings.Join(summary.LocalCollector.Withheld, ","), "processes") {
		t.Fatalf("stale collector summary: %+v", summary.LocalCollector)
	}
	saved := loadConfig(t, paths)
	saved.Components.Hub.Port = 9000
	saved.Components.Collector.Enabled = false
	if err := config.Save(paths.ConfigFile, saved); err != nil {
		t.Fatal(err)
	}
	summary, err = hubSettingsFromFile(config.ScopeUser, paths.ConfigFile, running, nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if summary.LocalCollector != nil || summary.Network.Port != int64(running.Components.Hub.Port) {
		t.Fatalf("summary should use current collector and running hub: %+v", summary)
	}
	if err := os.Remove(paths.ConfigFile); err != nil {
		t.Fatal(err)
	}
	if _, err := hubSettingsFromFile(config.ScopeUser, paths.ConfigFile, running, nil, nil, time.Now()); err == nil {
		t.Fatal("unreadable configuration silently returned stale policy")
	}
}
