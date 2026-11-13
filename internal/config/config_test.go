package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default invalid: %v", err)
	}
	if !cfg.Components.Hub.Enabled || !cfg.Components.Collector.Enabled {
		t.Fatal("fresh defaults must enable both components")
	}
	if cfg.Components.Hub.TelemetryMode != "memory" {
		t.Fatal("fresh hubs must default to RAM telemetry")
	}
}

func TestTelemetryModeDefaultAndPersistentOptIn(t *testing.T) {
	for _, mode := range []string{"", "memory", "persistent", "typo"} {
		cfg := Default()
		cfg.Components.Hub.TelemetryMode = mode
		cfg.Normalize()
		err := cfg.Validate()
		if mode == "typo" {
			if err == nil {
				t.Fatal("invalid telemetry mode accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "config.json")
		if err := Save(path, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		want := mode
		if want == "" {
			want = "memory"
		}
		if loaded.Components.Hub.TelemetryMode != want {
			t.Fatalf("mode = %q, want %q", loaded.Components.Hub.TelemetryMode, want)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rigscope", "config.json")
	cfg := Default()
	cfg.Components.Hub.Port = 8090
	cfg.Components.Hub.Origin = "https://hub.example.com"
	cfg.Components.Collector.Destinations = []Destination{{URL: "wss://hub.example.com/api/collector/connect", CAFile: "/etc/rigscope/hub-ca.pem"}}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %04o, want 0600", mode)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Components.Hub.Port != 8090 || loaded.Components.Hub.Origin != "https://hub.example.com" {
		t.Fatalf("round trip mismatch: %+v", loaded.Components.Hub)
	}
	if got := loaded.Components.Collector.SamplingInterval.Value(); got != 2*time.Second {
		t.Fatalf("sampling interval = %s", got)
	}
	if loaded.Components.Collector.Destinations[0].CAFile != "/etc/rigscope/hub-ca.pem" {
		t.Fatal("caFile not preserved")
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), `"token"`) || strings.Contains(string(data), `"grant"`) {
		t.Fatalf("configuration contains enrollment secrets: %s", data)
	}
	if got := loaded.Components.Collector.Sharing; got != DefaultSharing() {
		t.Fatalf("sharing = %+v, want defaults", got)
	}
	if !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("missing version: %s", data)
	}
	if !strings.Contains(string(data), `"networkAddresses": false`) || !strings.Contains(string(data), `"processArguments": false`) {
		t.Fatalf("privacy defaults missing from JSON: %s", data)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, `{"version":1,"components":{"hub":{"enabled":true,"listen":"127.0.0.1","port":8081,"origin":"","tlsCert":"","tlsKey":"","trustedProxies":[],"typo":1},"collector":{"enabled":true,"destinations":[],"samplingInterval":"2s"}}}`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want unknown field", err)
	}
}

func TestLoadRefusesNewerVersion(t *testing.T) {
	path := writeConfig(t, `{"version":2,"components":{}}`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRequiresVersion(t *testing.T) {
	path := writeConfig(t, `{"components":{}}`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `missing "version"`) {
		t.Fatalf("err = %v", err)
	}
}

const oldStyleConfig = `{
  "version": 0,
  "components": {
    "hub": {
      "enabled": true,
      "listen": "127.0.0.1",
      "port": "8081",
      "origin": "",
      "tlsCert": "",
      "tlsKey": "",
      "trustedProxies": []
    },
    "collector": {
      "enabled": true,
      "destinations": [],
      "sampling_interval": "3s",
      "sharing": {
        "cpu": true, "memory": true, "swap": true, "gpus": true, "disks": true,
        "network": true, "networkAddresses": false, "processes": true,
        "processArguments": false
      },
      "hardware": {"gpuRegisters": false}
    }
  }
}`

func TestMigrationRenamesKeyAndChangesType(t *testing.T) {
	path := writeConfig(t, oldStyleConfig)
	testOnly := []Migration{{
		From: 0,
		To:   1,
		Apply: func(raw map[string]any) error {
			components, ok := raw["components"].(map[string]any)
			if !ok {
				return fmt.Errorf("components missing")
			}
			hub, ok := components["hub"].(map[string]any)
			if !ok {
				return fmt.Errorf("hub missing")
			}
			port, ok := hub["port"].(string)
			if !ok {
				return fmt.Errorf("port is %T, want string", hub["port"])
			}
			value, err := strconv.Atoi(port)
			if err != nil {
				return err
			}
			hub["port"] = value
			collector, ok := components["collector"].(map[string]any)
			if !ok {
				return fmt.Errorf("collector missing")
			}
			interval, ok := collector["sampling_interval"]
			if !ok {
				return fmt.Errorf("sampling_interval missing")
			}
			collector["samplingInterval"] = interval
			delete(collector, "sampling_interval")
			return nil
		},
	}}
	cfg, err := LoadWithMigrations(path, testOnly)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Version != CurrentVersion {
		t.Fatalf("version = %d", cfg.Version)
	}
	if cfg.Components.Hub.Port != 8081 {
		t.Fatalf("port was not changed from string to number: %d", cfg.Components.Hub.Port)
	}
	if got := cfg.Components.Collector.SamplingInterval.Value(); got != 3*time.Second {
		t.Fatalf("renamed key was not migrated: %s", got)
	}
}

func TestMigrationLeavesUnknownKeyForStrictDecode(t *testing.T) {
	path := writeConfig(t, oldStyleConfig)
	testOnly := []Migration{{
		From: 0,
		To:   1,
		Apply: func(raw map[string]any) error {
			components := raw["components"].(map[string]any)
			hub := components["hub"].(map[string]any)
			hub["port"] = 8081
			collector := components["collector"].(map[string]any)
			// Rename the interval but leave a foreign key behind.
			collector["samplingInterval"] = collector["sampling_interval"]
			delete(collector, "sampling_interval")
			collector["typo"] = true
			return nil
		},
	}}
	_, err := LoadWithMigrations(path, testOnly)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want strict decode failure", err)
	}
}

func TestMigrationRequiresAPath(t *testing.T) {
	raw := map[string]any{"version": 0}
	err := Migrate(raw, []Migration{{From: 1, To: 2, Apply: func(map[string]any) error { return nil }}})
	if err == nil || !strings.Contains(err.Error(), "no migration from version 0") {
		t.Fatalf("err = %v", err)
	}
}

func TestMigrationRejectsMalformedChain(t *testing.T) {
	raw := map[string]any{"version": 0}
	err := Migrate(raw, []Migration{
		{From: 0, To: 1, Apply: func(map[string]any) error { return nil }},
		{From: 0, To: 1, Apply: func(map[string]any) error { return nil }},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate migration") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingSharingSectionIsRejected(t *testing.T) {
	path := writeConfig(t, `{"version":1,"components":{"hub":{"enabled":false},"collector":{"enabled":false,"destinations":[],"samplingInterval":"2s","hardware":{"gpuRegisters":false}}}}`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "components.collector.sharing") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"bad port", func(c *Config) { c.Components.Hub.Port = 70000 }, "outside 1-65535"},
		{"empty listen", func(c *Config) { c.Components.Hub.Listen = "" }, "listen address is empty"},
		{"origin path", func(c *Config) { c.Components.Hub.Origin = "https://example.com/app" }, "root URL"},
		{"origin scheme", func(c *Config) { c.Components.Hub.Origin = "ftp://example.com" }, "must use http or https"},
		{"cert without key", func(c *Config) { c.Components.Hub.TLSCert = "/tmp/cert" }, "set together"},
		{"bad proxy", func(c *Config) { c.Components.Hub.TrustedProxies = []string{"proxy"} }, "not an IP address"},
		{"bad forwarded family", func(c *Config) { c.Components.Hub.ForwardedHeaders = "both" }, "forwardedHeaders"},
		{"public listen needs origin", func(c *Config) { c.Components.Hub.Listen = "0.0.0.0" }, "origin is required"},
		{"origin not canonical", func(c *Config) { c.Components.Hub.Origin = "https://example.com/" }, "not canonical"},
		{"two destinations", func(c *Config) {
			c.Components.Collector.Destinations = []Destination{{URL: "wss://a/api/collector/connect"}, {URL: "wss://b/api/collector/connect"}}
		}, "exactly one"},
		{"bad destination", func(c *Config) {
			c.Components.Collector.Destinations = []Destination{{URL: "file:///etc/passwd"}}
		}, "must use ws:// or wss://"},
		{"plaintext remote destination", func(c *Config) {
			c.Components.Collector.Destinations = []Destination{{URL: "ws://hub.example.com/api/collector/connect"}}
		}, "plaintext ws:// is restricted"},
		{"destination with the wrong path", func(c *Config) {
			c.Components.Collector.Destinations = []Destination{{URL: "wss://hub.example.com/other"}}
		}, "path must be exactly /api/collector/connect"},
		{"relative ca file", func(c *Config) {
			c.Components.Collector.Destinations = []Destination{{URL: "wss://hub.example.com/api/collector/connect", CAFile: "ca.pem"}}
		}, "absolute path"},
		{"ca file on plaintext", func(c *Config) {
			c.Components.Collector.Destinations = []Destination{{URL: "ws://127.0.0.1:8081/api/collector/connect", CAFile: "/tmp/ca.pem"}}
		}, "requires a wss destination"},
		{"short interval", func(c *Config) { c.Components.Collector.SamplingInterval = Duration(time.Millisecond) }, "shorter than 100ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeRewritesStoredDestinations(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://hub.example.com", "wss://hub.example.com/api/collector/connect"},
		{"wss://hub.example.com", "wss://hub.example.com/api/collector/connect"},
		{"http://localhost:8081", "ws://127.0.0.1:8081/api/collector/connect"},
		{"http://ip6-localhost:8081", "ws://[::1]:8081/api/collector/connect"},
		{"wss://ip6-localhost:8443", "wss://ip6-localhost:8443/api/collector/connect"},
		{"https://localhost6", "wss://localhost6/api/collector/connect"},
		{"ws://127.0.0.1:8081/api/collector/connect", "ws://127.0.0.1:8081/api/collector/connect"},
	}
	for _, c := range cases {
		cfg := Default()
		cfg.Components.Collector.Destinations = []Destination{{URL: c.raw}}
		cfg.Normalize()
		if got := cfg.Components.Collector.Destinations[0].URL; got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("normalized configuration for %q was rejected: %v", c.raw, err)
		}
	}
}

func TestSavePersistsTheNormalizedDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.Components.Collector.Destinations = []Destination{{URL: "http://localhost:8081"}}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"url": "ws://127.0.0.1:8081/api/collector/connect"`) {
		t.Fatalf("saved configuration is not normalized:\n%s", data)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("loading the normalized configuration: %v", err)
	}
	if got := loaded.Components.Collector.Destinations[0].URL; got != "ws://127.0.0.1:8081/api/collector/connect" {
		t.Fatalf("destination = %q", got)
	}
}

func TestLoopbackPlaintextDestinationIsAllowed(t *testing.T) {
	for _, url := range []string{"ws://127.0.0.1:8081/api/collector/connect", "ws://localhost:8081/api/collector/connect", "ws://[::1]:8081/api/collector/connect"} {
		cfg := Default()
		cfg.Components.Collector.Destinations = []Destination{{URL: url}}
		cfg.Normalize()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		normalized := cfg.Components.Collector.Destinations[0].URL
		if strings.Contains(normalized, "localhost") {
			t.Fatalf("%s was stored with a DNS name: %s", url, normalized)
		}
	}
}

func TestPublicOriginIsRequiredAndCanonicalWhenBound(t *testing.T) {
	cfg := Default()
	cfg.Components.Hub.Listen = "0.0.0.0"
	cfg.Components.Hub.Origin = "https://hub.example.com/"
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("canonical origin should be accepted: %v", err)
	}
	cfg.Components.Hub.Origin = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "origin is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestDisabledComponentSkipsNetworkValidation(t *testing.T) {
	cfg := Default()
	cfg.Components.Hub.Enabled = false
	cfg.Components.Hub.Port = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled hub should not be validated: %v", err)
	}
}

func TestNormalizeStripsOriginSlash(t *testing.T) {
	cfg := Default()
	cfg.Components.Hub.Origin = "https://hub.example.com/"
	cfg.Normalize()
	if cfg.Components.Hub.Origin != "https://hub.example.com" {
		t.Fatalf("origin = %q", cfg.Components.Hub.Origin)
	}
}

func TestForwardedHeadersDefaultAndRoundTrip(t *testing.T) {
	cfg := Default()
	if cfg.Components.Hub.ForwardedHeaders != ForwardedHeadersXForwarded {
		t.Fatalf("default forwardedHeaders = %q", cfg.Components.Hub.ForwardedHeaders)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"forwardedHeaders":"x-forwarded"`) {
		t.Fatalf("json = %s", data)
	}

	cfg.Components.Hub.ForwardedHeaders = ""
	cfg.Normalize()
	if cfg.Components.Hub.ForwardedHeaders != ForwardedHeadersXForwarded {
		t.Fatalf("empty value was not normalised: %q", cfg.Components.Hub.ForwardedHeaders)
	}
	cfg.Components.Hub.ForwardedHeaders = ForwardedHeadersForwarded
	if err := cfg.Validate(); err != nil {
		t.Fatalf("forwarded family rejected: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Components.Hub.ForwardedHeaders != ForwardedHeadersForwarded {
		t.Fatalf("round trip = %q", loaded.Components.Hub.ForwardedHeaders)
	}
}

func TestLoadRejectsUnknownForwardedFamily(t *testing.T) {
	path := writeConfig(t, `{"version":1,"components":{"hub":{"enabled":true,"listen":"127.0.0.1","port":8081,"origin":"","tlsCert":"","tlsKey":"","trustedProxies":[],"forwardedHeaders":"x-real-ip"},"collector":{"enabled":true,"destinations":[],"samplingInterval":"2s","sharing":{"cpu":true,"memory":true,"swap":true,"gpus":true,"disks":true,"network":true,"networkAddresses":false,"processes":true,"processArguments":false},"hardware":{"gpuRegisters":false}}}}`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "forwardedHeaders") {
		t.Fatalf("err = %v, want forwardedHeaders", err)
	}
}

func TestDurationEncodesAsString(t *testing.T) {
	data, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"samplingInterval":"2s"`) {
		t.Fatalf("json = %s", data)
	}
}

func TestPathsUser(t *testing.T) {
	paths, err := Resolve(ScopeUser, Env{Home: "/home/alice"})
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigFile != "/home/alice/.config/rigscope/config.json" {
		t.Fatalf("config = %s", paths.ConfigFile)
	}
	if paths.StateDir != "/home/alice/.local/state/rigscope" {
		t.Fatalf("state = %s", paths.StateDir)
	}
	if paths.Binary != "/home/alice/.local/bin/rigscope" {
		t.Fatalf("binary = %s", paths.Binary)
	}
	if paths.PreviousBinary != paths.Binary+".prev" {
		t.Fatalf("previous = %s", paths.PreviousBinary)
	}
	if paths.UnitDir != "/home/alice/.config/systemd/user" {
		t.Fatalf("units = %s", paths.UnitDir)
	}
	if got := paths.ComponentStateDir("hub"); got != paths.StateDir {
		t.Fatalf("user component state = %s", got)
	}
}

func TestPathsUserXDG(t *testing.T) {
	paths, err := Resolve(ScopeUser, Env{
		Home:          "/home/alice",
		XDGConfigHome: "/tmp/config",
		XDGStateHome:  "/tmp/state",
	})
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigFile != "/tmp/config/rigscope/config.json" {
		t.Fatalf("config = %s", paths.ConfigFile)
	}
	if paths.StateDir != "/tmp/state/rigscope" {
		t.Fatalf("state = %s", paths.StateDir)
	}
	if paths.UnitDir != "/tmp/config/systemd/user" {
		t.Fatalf("units = %s", paths.UnitDir)
	}
}

func TestPathsSystem(t *testing.T) {
	paths, err := Resolve(ScopeSystem, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigFile != "/etc/rigscope/config.json" || paths.StateDir != "/var/lib/rigscope" {
		t.Fatalf("system paths = %+v", paths)
	}
	if paths.Binary != "/usr/local/bin/rigscope" || paths.UnitDir != "/etc/systemd/system" {
		t.Fatalf("system paths = %+v", paths)
	}
	if paths.SysusersFile != "/etc/sysusers.d/rigscope.conf" || paths.UdevRAPLFile != "/etc/udev/rules.d/90-rigscope-rapl.rules" {
		t.Fatalf("system integration paths = %+v", paths)
	}
	if got := paths.ComponentStateDir("collector"); got != "/var/lib/rigscope/collector" {
		t.Fatalf("component state = %s", got)
	}
}

func TestPathsSystemRootPrefix(t *testing.T) {
	paths, err := Resolve(ScopeSystem, Env{Root: "/tmp/root"})
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigFile != "/tmp/root/etc/rigscope/config.json" {
		t.Fatalf("config = %s", paths.ConfigFile)
	}
	if paths.Binary != "/tmp/root/usr/local/bin/rigscope" {
		t.Fatalf("binary = %s", paths.Binary)
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCanonicalOrigin(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"https://Hub.Example.com:443/", "https://hub.example.com", true},
		{"https://hub.example.com", "https://hub.example.com", true},
		{"http://hub.example.com:80", "http://hub.example.com", true},
		{"http://[::1]:8081", "http://[::1]:8081", true},
		{"https://:443", "", false},
		{"https://:443/", "", false},
		{"https://hub.example.com:0", "", false},
		{"https://hub.example.com:70000", "", false},
		{"https://user@hub.example.com", "", false},
		{"https://hub.example.com/path", "", false},
		{"https://hub.example.com?x=1", "", false},
		{"https://hub.example.com#frag", "", false},
		{"ftp://hub.example.com", "", false},
		{"", "", false},
	}
	for _, test := range cases {
		got := CanonicalOrigin(test.raw)
		if test.ok && got != test.want {
			t.Errorf("CanonicalOrigin(%q) = %q, want %q", test.raw, got, test.want)
		}
		if !test.ok && got != "" {
			t.Errorf("CanonicalOrigin(%q) = %q, want rejection", test.raw, got)
		}
	}
}

func TestHubOriginValidationRejectsMissingHostname(t *testing.T) {
	hub := Hub{Enabled: true, Listen: "0.0.0.0", Port: 8081, Origin: "https://:443"}
	if err := hub.Validate(); err == nil {
		t.Fatal("https://:443 was accepted")
	}
	hub.Origin = "https://Hub.Example.com"
	if err := hub.Validate(); err == nil {
		t.Fatal("non-canonical origin was accepted by Validate")
	}
	hub.Origin = "https://hub.example.com"
	if err := hub.Validate(); err != nil {
		t.Fatalf("canonical origin rejected: %v", err)
	}
}

func TestNormalizeCanonicalizesOrigin(t *testing.T) {
	cfg := Default()
	cfg.Components.Hub.Origin = "HTTPS://Hub.Example.com:443/"
	cfg.Normalize()
	if cfg.Components.Hub.Origin != "https://hub.example.com" {
		t.Fatalf("origin = %q", cfg.Components.Hub.Origin)
	}
}

func TestNormalizeKeepsInvalidOriginForValidation(t *testing.T) {
	cfg := Default()
	cfg.Components.Hub.Origin = "https://:443"
	cfg.Normalize()
	if cfg.Components.Hub.Origin != "https://:443" {
		t.Fatalf("invalid origin was silently rewritten to %q", cfg.Components.Hub.Origin)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid origin was accepted after Normalize")
	}
}

func TestSyntheticModeIsAValidConfigValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.Components.Collector.Synthetic = true
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.Components.Collector.Synthetic {
		t.Fatal("synthetic mode did not survive the round trip")
	}
	// A normal installation never writes the field, and strict decoding must
	// still accept a file without it.
	plain := Default()
	if err := Save(path, plain); err != nil {
		t.Fatalf("save default: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "synthetic") {
		t.Fatalf("false synthetic mode was written: %s", data)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("load default: %v", err)
	}
}

func TestRuntimeLockPathsStayInsideComponentSubtrees(t *testing.T) {
	for _, test := range []struct {
		name  string
		scope Scope
		env   Env
	}{
		{"user", ScopeUser, Env{Home: "/home/alice"}},
		{"system", ScopeSystem, Env{Root: "/tmp/root"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths, err := Resolve(test.scope, test.env)
			if err != nil {
				t.Fatal(err)
			}
			collectorLock := paths.CollectorRuntimeLock()
			if filepath.Dir(collectorLock) != paths.CollectorDir() {
				t.Fatalf("collector runtime lock %s is not inside %s", collectorLock, paths.CollectorDir())
			}
			hubLock := paths.HubRuntimeLock()
			if filepath.Dir(hubLock) != paths.ComponentStateDir("hub") {
				t.Fatalf("hub runtime lock %s is not inside the hub state directory", hubLock)
			}
			// In user scope the state root is the hub-owned subtree; in system
			// scope the hub has its own directory below the root.
			if test.scope == ScopeSystem && filepath.Dir(hubLock) == paths.StateDir {
				t.Fatal("the system hub runtime lock lives in the shared state root")
			}
			if filepath.Dir(collectorLock) == paths.StateDir {
				t.Fatal("the collector runtime lock lives in the shared state root")
			}
		})
	}
}

func TestNormalizeHubURL(t *testing.T) {
	cases := []struct {
		raw, want string
		ok        bool
	}{
		{"https://hub.example.com", "wss://hub.example.com/api/collector/connect", true},
		{"https://hub.example.com/", "wss://hub.example.com/api/collector/connect", true},
		{"https://hub.example.com:8443", "wss://hub.example.com:8443/api/collector/connect", true},
		{"wss://hub.example.com/api/collector/connect", "wss://hub.example.com/api/collector/connect", true},
		{"http://127.0.0.1:8081", "ws://127.0.0.1:8081/api/collector/connect", true},
		{"http://hub.example.com", "", false},
		{"https://hub.example.com/other", "", false},
		{"https://user@hub.example.com", "", false},
		{"https://hub.example.com/?x=1", "", false},
		{"ftp://hub.example.com", "", false},
	}
	for _, c := range cases {
		got, err := NormalizeHubURL(c.raw)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("NormalizeHubURL(%q) = %q, %v; want %q", c.raw, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("NormalizeHubURL(%q) = %q, want an error", c.raw, got)
		}
	}
}
