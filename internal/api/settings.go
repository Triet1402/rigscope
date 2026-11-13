package api

import "fmt"

// HubStorageMode is where the hub keeps telemetry history.
type HubStorageMode string

// Hub storage modes. They match components.hub.telemetryMode.
const (
	HubStorageMemory     HubStorageMode = "memory"
	HubStoragePersistent HubStorageMode = "persistent"
)

func (m HubStorageMode) validate() error {
	switch m {
	case HubStorageMemory, HubStoragePersistent:
		return nil
	default:
		return fmt.Errorf("unknown storage mode %q", m)
	}
}

// InstallScope is the kind of installation the hub runs from. It decides the
// terminal command that changes a setting: a system installation needs sudo.
type InstallScope string

// Installation scopes.
const (
	InstallScopeUser   InstallScope = "user"
	InstallScopeSystem InstallScope = "system"
)

func (s InstallScope) validate() error {
	switch s {
	case InstallScopeUser, InstallScopeSystem:
		return nil
	default:
		return fmt.Errorf("unknown install scope %q", s)
	}
}

// ForwardedHeaderFamily is the one proxy header family the hub trusts.
type ForwardedHeaderFamily string

// Forwarded header families.
const (
	ForwardedHeaderXForwarded ForwardedHeaderFamily = "x-forwarded"
	ForwardedHeaderForwarded  ForwardedHeaderFamily = "forwarded"
)

func (f ForwardedHeaderFamily) validate() error {
	switch f {
	case ForwardedHeaderXForwarded, ForwardedHeaderForwarded:
		return nil
	default:
		return fmt.Errorf("unknown forwarded header family %q", f)
	}
}

// HubStorageSettings describes history storage and its current footprint.
// diskBytes is the hub database on disk (accounts, machines and alert state in
// every mode, plus history in persistent mode). memoryBytes is history held in
// RAM: everything in memory mode, only the recent one-second ring in
// persistent mode.
//
//api:response
type HubStorageSettings struct {
	Mode              HubStorageMode `json:"mode"`
	DiskBytes         int64          `json:"diskBytes"`         //api:range=0..Number.MAX_SAFE_INTEGER
	DiskLimitBytes    *int64         `json:"diskLimitBytes"`    //api:range=0..Number.MAX_SAFE_INTEGER
	MemoryBytes       int64          `json:"memoryBytes"`       //api:range=0..Number.MAX_SAFE_INTEGER
	MemoryBudgetBytes int64          `json:"memoryBudgetBytes"` //api:range=0..Number.MAX_SAFE_INTEGER
	OldestHistoryMs   *int64         `json:"oldestHistoryMs"`   //api:range=0..Number.MAX_SAFE_INTEGER
}

// HubNetworkSettings is where and how the dashboard answers. TLS reports only
// whether the hub serves HTTPS itself, never certificate or key paths.
//
//api:response
type HubNetworkSettings struct {
	URL              string                `json:"url"`
	Listen           string                `json:"listen"`
	Port             int64                 `json:"port"` //api:range=1..65535
	Origin           *string               `json:"origin"`
	TLS              bool                  `json:"tls"`
	TrustedProxies   []string              `json:"trustedProxies"`
	ForwardedHeaders ForwardedHeaderFamily `json:"forwardedHeaders"`
}

// LocalCollectorSettings is the collector configured on the hub's own machine.
// Each machine's collector applies its own sharing policy before anything is
// sent; the hub can show this one only because it reads the same file.
//
//api:response
type LocalCollectorSettings struct {
	SamplingIntervalMs int64    `json:"samplingIntervalMs"` //api:range=100..3600000
	Destination        *string  `json:"destination"`
	Shared             []string `json:"shared"`
	Withheld           []string `json:"withheld"`
	GPURegisters       bool     `json:"gpuRegisters"`
}

// HubSettings is GET /api/admin/settings: the settings the running hub
// started with. It is read-only and carries no credentials, key material or
// file paths. Settings change from a terminal on the hub machine with
// `rigscope configure`, which previews the change and restarts what it must.
//
//api:response
type HubSettings struct {
	GeneratedAt    string                  `json:"generatedAt"`
	Scope          InstallScope            `json:"scope"`
	Storage        HubStorageSettings      `json:"storage"`
	Network        HubNetworkSettings      `json:"network"`
	LocalCollector *LocalCollectorSettings `json:"localCollector"`
}

func (s HubSettings) validate() error {
	if err := validateTimestamp(s.GeneratedAt, "generatedAt"); err != nil {
		return err
	}
	if err := s.Scope.validate(); err != nil {
		return err
	}
	storage := s.Storage
	if err := storage.Mode.validate(); err != nil {
		return err
	}
	if storage.DiskBytes < 0 || storage.MemoryBytes < 0 || storage.MemoryBudgetBytes < 0 ||
		(storage.DiskLimitBytes != nil && *storage.DiskLimitBytes < 0) ||
		(storage.OldestHistoryMs != nil && *storage.OldestHistoryMs < 0) {
		return fmt.Errorf("storage sizes must not be negative")
	}
	network := s.Network
	if err := validateText(network.URL, "url", 1, MaxResourceIDLength); err != nil {
		return err
	}
	if err := validateText(network.Listen, "listen", 1, MaxIDLength); err != nil {
		return err
	}
	if network.Port < 1 || network.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if network.Origin != nil {
		if err := validateText(*network.Origin, "origin", 1, MaxResourceIDLength); err != nil {
			return err
		}
	}
	if network.TrustedProxies == nil {
		return fmt.Errorf("trustedProxies must be an array")
	}
	for _, proxy := range network.TrustedProxies {
		if err := validateText(proxy, "trusted proxy", 1, MaxIDLength); err != nil {
			return err
		}
	}
	if err := network.ForwardedHeaders.validate(); err != nil {
		return err
	}
	if collector := s.LocalCollector; collector != nil {
		if collector.SamplingIntervalMs < 100 || collector.SamplingIntervalMs > 3_600_000 {
			return fmt.Errorf("samplingIntervalMs must be between 100 and 3600000")
		}
		if collector.Destination != nil {
			if err := validateText(*collector.Destination, "destination", 1, MaxResourceIDLength); err != nil {
				return err
			}
		}
		if collector.Shared == nil || collector.Withheld == nil {
			return fmt.Errorf("shared and withheld must be arrays")
		}
		for _, family := range append(append([]string{}, collector.Shared...), collector.Withheld...) {
			if err := validateText(family, "sharing family", 1, MaxIDLength); err != nil {
				return err
			}
		}
	}
	return nil
}

// DecodeHubSettings strictly decodes GET /api/admin/settings.
func DecodeHubSettings(data []byte) (HubSettings, error) {
	out, err := decodeResponse[HubSettings](data, "hub settings response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// EncodeHubSettings encodes the admin hub settings summary.
func EncodeHubSettings(v HubSettings) ([]byte, error) {
	return encodeChecked(v, DecodeHubSettings)
}
