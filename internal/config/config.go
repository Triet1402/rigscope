package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Triet1402/rigscope/internal/platform/huburl"
)

// CurrentVersion is the configuration schema version written by this binary.
const CurrentVersion = 1

// Forwarded-header families. Exactly one family is trusted from a configured
// proxy; the other is ignored and stripped, so a proxy cannot smuggle facts
// through a header family the operator did not choose.
const (
	ForwardedHeadersXForwarded = "x-forwarded"
	ForwardedHeadersForwarded  = "forwarded"
)

// DefaultForwardedHeaders is the family used when the configuration omits the
// setting. It is the form most reverse proxies emit by default.
const DefaultForwardedHeaders = ForwardedHeadersXForwarded

// Config is the on-disk configuration. It is shared by the hub and collector;
// each process reads the components it cares about.
type Config struct {
	Version    int        `json:"version"`
	Components Components `json:"components"`
}

// Components groups the independently enabled hub and collector settings.
type Components struct {
	Hub       Hub       `json:"hub"`
	Collector Collector `json:"collector"`
}

// Hub holds the dashboard's network and TLS settings.
type Hub struct {
	// TelemetryMode selects volatile RAM history (default) or durable SQLite history.
	TelemetryMode string `json:"telemetryMode,omitempty"`
	Enabled       bool   `json:"enabled"`
	Listen        string `json:"listen"`
	Port          int    `json:"port"`
	Origin        string `json:"origin"`
	TLSCert       string `json:"tlsCert"`
	TLSKey        string `json:"tlsKey"`
	// ForwardedHeaders selects the single header family trusted from
	// TrustedProxies: "x-forwarded" (X-Forwarded-For/-Proto/-Host) or
	// "forwarded" (RFC 7239 Forwarded). The other family is ignored and
	// stripped. Empty means DefaultForwardedHeaders for configurations written
	// before the key existed; Normalize fills it in.
	ForwardedHeaders string   `json:"forwardedHeaders"`
	TrustedProxies   []string `json:"trustedProxies"`
}

// Collector holds delivery, sharing and sampling settings. Destinations stays
// a list so multi-hub delivery can return later; 1.0 accepts exactly one entry.
// The configuration never stores an enrollment grant or issued credential; the
// collector keeps those in its private state directory.
type Collector struct {
	Enabled          bool          `json:"enabled"`
	Destinations     []Destination `json:"destinations"`
	SamplingInterval Duration      `json:"samplingInterval"`
	// Synthetic selects the explicit mock machine used by the website demo and
	// by tests. Every value is clearly fake and no host resource is read, so it
	// can never be mistaken for real telemetry. Default false.
	Synthetic bool     `json:"synthetic,omitempty"`
	Sharing   Sharing  `json:"sharing"`
	Hardware  Hardware `json:"hardware"`
}

// Destination is one hub a collector delivers to. CAFile optionally pins a
// private CA for that destination; an empty value uses the system trust store.
type Destination struct {
	URL    string `json:"url"`
	CAFile string `json:"caFile,omitempty"`
}

// Sharing is the collector-local privacy policy. Family flags are on by
// default; network addresses and process arguments are off because they carry
// identifying or secret material. The names match the 0.2 prototype policy.
type Sharing struct {
	CPU              bool `json:"cpu"`
	Memory           bool `json:"memory"`
	Swap             bool `json:"swap"`
	GPUs             bool `json:"gpus"`
	Disks            bool `json:"disks"`
	Network          bool `json:"network"`
	NetworkAddresses bool `json:"networkAddresses"`
	Processes        bool `json:"processes"`
	ProcessArguments bool `json:"processArguments"`
}

// Hardware holds opt-in hardware access. GPU register reads need a udev rule
// and group membership, so they are never enabled implicitly.
type Hardware struct {
	GPURegisters bool `json:"gpuRegisters"`
}

// DefaultSharing returns the privacy-preserving local policy.
func DefaultSharing() Sharing {
	return Sharing{
		CPU:              true,
		Memory:           true,
		Swap:             true,
		GPUs:             true,
		Disks:            true,
		Network:          true,
		NetworkAddresses: false,
		Processes:        true,
		ProcessArguments: false,
	}
}

// Duration is a time.Duration encoded as a human-readable string such as "2s".
type Duration time.Duration

// MarshalJSON encodes the duration as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON decodes a Go duration string.
func (d *Duration) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*d = 0
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("duration must be a string such as \"2s\": %w", err)
	}
	value, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(value)
	return nil
}

// Value returns the duration as a time.Duration.
func (d Duration) Value() time.Duration { return time.Duration(d) }

// Default returns the configuration a fresh installation starts from. Both
// components are enabled; install presets narrow that.
func Default() *Config {
	return &Config{
		Version: CurrentVersion,
		Components: Components{
			Hub: Hub{
				TelemetryMode:    "memory",
				Enabled:          true,
				Listen:           "127.0.0.1",
				Port:             8081,
				ForwardedHeaders: DefaultForwardedHeaders,
				TrustedProxies:   []string{},
			},
			Collector: Collector{
				Enabled:          true,
				Destinations:     []Destination{},
				SamplingInterval: Duration(2 * time.Second),
				Synthetic:        false,
				Sharing:          DefaultSharing(),
				Hardware:         Hardware{GPURegisters: false},
			},
		},
	}
}

// Validate checks the values this binary understands. It does not rewrite them;
// use Normalize for canonical forms.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("configuration is empty")
	}
	if c.Version != CurrentVersion {
		if c.Version > CurrentVersion {
			return fmt.Errorf("configuration version %d is newer than this binary supports (%d); upgrade rigscope", c.Version, CurrentVersion)
		}
		return fmt.Errorf("configuration version %d was not migrated to %d", c.Version, CurrentVersion)
	}
	if err := c.Components.Hub.Validate(); err != nil {
		return fmt.Errorf("components.hub: %w", err)
	}
	if err := c.Components.Collector.Validate(); err != nil {
		return fmt.Errorf("components.collector: %w", err)
	}
	return nil
}

// Normalize canonicalises values that have more than one spelling. An invalid
// origin is left untouched so Validate rejects it instead of silently
// clearing it.
func (c *Config) Normalize() {
	if c.Components.Hub.TelemetryMode == "" {
		c.Components.Hub.TelemetryMode = "memory"
	}
	if c.Components.Hub.Origin != "" {
		if canonical, err := parseOrigin(c.Components.Hub.Origin); err == nil {
			c.Components.Hub.Origin = canonical
		}
	}
	if c.Components.Collector.Destinations == nil {
		c.Components.Collector.Destinations = []Destination{}
	}
	// Stored destinations are canonicalized before validation so a
	// configuration written by an older installer (https://host, or a URL
	// without the connect path) keeps loading instead of failing strict
	// validation. The normalized form is persisted by the next save.
	for index := range c.Components.Collector.Destinations {
		destination := &c.Components.Collector.Destinations[index]
		if normalized, err := NormalizeHubURL(destination.URL); err == nil {
			destination.URL = normalized
		}
	}
	if c.Components.Hub.TrustedProxies == nil {
		c.Components.Hub.TrustedProxies = []string{}
	}
	if c.Components.Hub.ForwardedHeaders == "" {
		c.Components.Hub.ForwardedHeaders = DefaultForwardedHeaders
	}
}

// Validate checks hub settings independently of the version.
func (h *Hub) Validate() error {
	switch h.TelemetryMode {
	case "", "memory", "persistent":
	default:
		return fmt.Errorf("telemetryMode %q must be memory or persistent", h.TelemetryMode)
	}
	if !h.Enabled {
		return nil
	}
	if strings.TrimSpace(h.Listen) == "" {
		return errors.New("listen address is empty")
	}
	if strings.ContainsAny(h.Listen, " \t\r\n") {
		return fmt.Errorf("listen address %q contains whitespace", h.Listen)
	}
	if h.Port < 1 || h.Port > 65535 {
		return fmt.Errorf("port %d is outside 1-65535", h.Port)
	}
	if !isLoopbackHost(h.Listen) && h.Origin == "" {
		return fmt.Errorf("origin is required when listen %q is not a loopback address", h.Listen)
	}
	if h.Origin != "" {
		canonical, err := parseOrigin(h.Origin)
		if err != nil {
			return err
		}
		if h.Origin != canonical {
			return fmt.Errorf("origin %q is not canonical; use %q", h.Origin, canonical)
		}
	}
	if (h.TLSCert == "") != (h.TLSKey == "") {
		return errors.New("tlsCert and tlsKey must be set together")
	}
	switch h.ForwardedHeaders {
	case "", ForwardedHeadersXForwarded, ForwardedHeadersForwarded:
	default:
		return fmt.Errorf("forwardedHeaders %q must be %q or %q", h.ForwardedHeaders, ForwardedHeadersXForwarded, ForwardedHeadersForwarded)
	}
	for _, proxy := range h.TrustedProxies {
		if net.ParseIP(proxy) != nil {
			continue
		}
		if _, _, err := net.ParseCIDR(proxy); err == nil {
			continue
		}
		return fmt.Errorf("trusted proxy %q is not an IP address or CIDR range", proxy)
	}
	return nil
}

// CanonicalOrigin returns the canonical serialization of a public origin, or
// an empty string when the value is not a valid absolute http(s) root URL.
// Validate uses parseOrigin directly so it can report the specific problem;
// Normalize leaves an invalid value untouched for Validate to reject.
func CanonicalOrigin(raw string) string {
	canonical, err := parseOrigin(raw)
	if err != nil {
		return ""
	}
	return canonical
}

// parseOrigin validates a public origin and returns its canonical form:
// lowercase host, the scheme's default port removed, and no path, query,
// fragment or credentials. A hostname is mandatory, so https://:443 is
// rejected rather than treated as an empty host with a default port.
func parseOrigin(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", fmt.Errorf("origin is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("origin %q: %w", raw, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("origin %q must use http or https", raw)
	}
	if parsed.Opaque != "" || parsed.Host == "" {
		return "", fmt.Errorf("origin %q has no host", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("origin %q must not contain credentials", raw)
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", fmt.Errorf("origin %q must be a root URL without path, query or fragment", raw)
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("origin %q has no hostname", raw)
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("origin %q has an invalid port %q", raw, port)
		}
		if (scheme == "http" && number == 80) || (scheme == "https" && number == 443) {
			port = ""
		}
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

// Validate checks collector settings independently of the version.
func (c *Collector) Validate() error {
	if len(c.Destinations) > 1 {
		return fmt.Errorf("%d destinations configured; 1.0 supports exactly one", len(c.Destinations))
	}
	for i, destination := range c.Destinations {
		if err := destination.Validate(); err != nil {
			return fmt.Errorf("destination %d: %w", i, err)
		}
	}
	if c.SamplingInterval < Duration(100*time.Millisecond) {
		return fmt.Errorf("samplingInterval %s is shorter than 100ms", c.SamplingInterval.Value())
	}
	if c.SamplingInterval > Duration(time.Hour) {
		return fmt.Errorf("samplingInterval %s is longer than 1h", c.SamplingInterval.Value())
	}
	return nil
}

// CollectorConnectPath is the hub's collector WebSocket endpoint.
const CollectorConnectPath = huburl.ConnectPath

// NormalizeHubURL turns the hub address a user types into the collector
// destination URL: https → wss and http → ws, with an empty or "/" path
// becoming CollectorConnectPath, and the loopback names localhost,
// ip6-localhost and localhost6 becoming literal loopback addresses. An
// explicit ws/wss URL must already use that path. The result is validated by
// the same policy the collector applies to its stored state.
func NormalizeHubURL(raw string) (string, error) {
	return huburl.Normalize(raw)
}

// Validate checks one destination URL and its pinned CA. The URL is checked by
// the shared huburl policy (only a canonical ws/wss endpoint is accepted); a
// stored destination is normalized first by Normalize, so a configuration
// written with https://host or without the connect path still loads.
func (d Destination) Validate() error {
	normalized, err := huburl.Validate(d.URL)
	if err != nil {
		return fmt.Errorf("url %q: %w", d.URL, err)
	}
	if d.CAFile != "" {
		if !filepath.IsAbs(d.CAFile) {
			return fmt.Errorf("caFile %q must be an absolute path", d.CAFile)
		}
		if strings.ContainsAny(d.CAFile, "\x00\r\n") {
			return fmt.Errorf("caFile contains control characters")
		}
		if !strings.HasPrefix(normalized, "wss://") {
			return fmt.Errorf("caFile %q requires a wss destination", d.CAFile)
		}
	}
	return nil
}

// isLoopbackHost reports whether a listen address or URL host stays local.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
