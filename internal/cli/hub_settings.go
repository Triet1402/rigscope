package cli

import (
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

// Collector settings can change without restarting the hub. Keep the running
// hub snapshot, but read the collector's saved configuration for every request.
// This reads only the shared configuration, never collector state or secrets.
func hubSettingsFromFile(scope config.Scope, path string, running *config.Config, database *storage.Database, telemetry *storage.Telemetry, now time.Time) (api.HubSettings, error) {
	current, err := config.Load(path)
	if err != nil {
		return api.HubSettings{}, err
	}
	snapshot := *running
	snapshot.Components.Collector = current.Components.Collector
	return hubSettingsSummary(scope, &snapshot, database, telemetry, now), nil
}

// hubSettingsSummary builds the Administration settings summary from the
// configuration the hub started with and its live storage counters. It
// leaves out file paths, certificate and key material and every credential.
func hubSettingsSummary(scope config.Scope, cfg *config.Config, database *storage.Database, telemetry *storage.Telemetry, now time.Time) api.HubSettings {
	hub := cfg.Components.Hub
	settings := api.HubSettings{
		GeneratedAt: now.UTC().Format("2006-01-02T15:04:05.000Z"),
		Scope:       api.InstallScopeUser,
		Network: api.HubNetworkSettings{
			URL:              hubURLForDisplay(cfg),
			Listen:           hub.Listen,
			Port:             int64(hub.Port),
			TLS:              hub.TLSCert != "",
			TrustedProxies:   append([]string{}, hub.TrustedProxies...),
			ForwardedHeaders: api.ForwardedHeaderFamily(hub.ForwardedHeaders),
		},
	}
	if scope == config.ScopeSystem {
		settings.Scope = api.InstallScopeSystem
	}
	if hub.Origin != "" {
		origin := hub.Origin
		settings.Network.Origin = &origin
	}

	storageSettings := api.HubStorageSettings{Mode: api.HubStorageMemory}
	if database != nil {
		storageSettings.DiskBytes = database.SizeBytes()
		if limit := database.PhysicalLimit(); limit != nil && limit.LimitBytes > 0 {
			value := limit.LimitBytes
			storageSettings.DiskLimitBytes = &value
		}
	}
	if telemetry != nil {
		stats, ring := telemetry.Stats(), telemetry.RingStats()
		storageSettings.MemoryBytes, storageSettings.MemoryBudgetBytes = ring.UsedBytes, ring.BudgetBytes
		if telemetry.Persistent() {
			storageSettings.Mode = api.HubStoragePersistent
		} else {
			storageSettings.MemoryBytes += stats.DBBytes
			storageSettings.MemoryBudgetBytes += stats.DBLimitBytes
		}
		if oldest, _ := telemetry.HistoryBounds(); oldest != nil && *oldest >= 0 {
			value := *oldest
			storageSettings.OldestHistoryMs = &value
		}
	} else if hub.TelemetryMode == "persistent" {
		storageSettings.Mode = api.HubStoragePersistent
	}
	settings.Storage = storageSettings

	collector := cfg.Components.Collector
	if collector.Enabled {
		local := &api.LocalCollectorSettings{
			SamplingIntervalMs: collector.SamplingInterval.Value().Milliseconds(),
			Shared:             []string{},
			Withheld:           []string{},
			GPURegisters:       collector.Hardware.GPURegisters,
		}
		if len(collector.Destinations) > 0 {
			destination := collector.Destinations[0].URL
			local.Destination = &destination
		}
		for _, family := range shareFamilies {
			if family.effective(collector.Sharing) {
				local.Shared = append(local.Shared, family.name)
			} else {
				local.Withheld = append(local.Withheld, family.name)
			}
		}
		settings.LocalCollector = local
	}
	return settings
}
