package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/activation"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/lock"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

type fixture struct {
	deps       Deps
	home       string
	runtimeDir string
	out        *bytes.Buffer
	err        *bytes.Buffer
	fake       *systemd.Fake
	// prompts records every stop confirmation asked; answer is the reply.
	prompts []string
	answer  bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	runtimeDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "rigscope-source")
	if err := os.WriteFile(source, []byte("binary-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	fake := systemd.NewFake()
	fake.ActiveByDefault = true
	fake.KnownByDefault = true
	// Root keeps every host path (/etc, /sys, /proc) inside the test's own
	// temporary tree. Tests must never inspect or modify the real host.
	root := t.TempDir()
	deps := Deps{
		Stdout: out,
		Stderr: errOut,
		Getenv: func(key string) string {
			switch key {
			case "XDG_RUNTIME_DIR":
				return runtimeDir
			case "USER":
				return "alice"
			case "LOGNAME":
				return "alice"
			}
			return ""
		},
		HomeDir:           func() (string, error) { return home, nil },
		Euid:              func() int { return 1000 },
		Executable:        func() (string, error) { return source, nil },
		RunningExecutable: func() (string, error) { return source, nil },
		BinaryVersion:     func(string) (string, error) { return "1.0.0", nil },
		Arch:              "amd64",
		Version:           "1.0.0",
		ProbeHealth:       func(context.Context, string) error { return nil },
		CollectorReady:    func(context.Context) error { return nil },
		Sleep:             func(time.Duration) {},
		Chown:             func(string, string, string) error { return nil },
		ChownFile:         func(*os.File, string, string) error { return nil },
		NewManager:        func(config.Scope) systemd.Manager { return fake },
		AllowInsecureHTTP: true,
		Root:              root,
		// Tests must never read the host's passwd/group files through os/user.
		Accounts: privateAccounts(),
		// No test may map a real BAR by accident; GPU fixtures replace this.
		ProbeRegisterAccess: func(string) error { return os.ErrPermission },
		// No test may read a real RAPL counter by accident.
		ReadAttribute: func(string) ([]byte, error) { return nil, os.ErrPermission },
	}
	f := &fixture{deps: deps, home: home, runtimeDir: runtimeDir, out: out, err: errOut, fake: fake, answer: true}
	f.deps.Confirm = func(question string) (bool, error) {
		f.prompts = append(f.prompts, question)
		return f.answer, nil
	}
	return f
}

func (f *fixture) paths(t *testing.T, scope config.Scope) config.Paths {
	t.Helper()
	paths, err := config.Resolve(scope, config.Env{
		Home:       f.home,
		RuntimeDir: f.runtimeDir,
		UID:        f.deps.Euid(),
		Root:       f.deps.Root,
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func (f *fixture) run(t *testing.T, args ...string) int {
	t.Helper()
	return Run(context.Background(), args, f.deps)
}

func (f *fixture) install(t *testing.T, args ...string) config.Paths {
	t.Helper()
	args = append([]string{"install"}, args...)
	if code := f.run(t, args...); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	return f.paths(t, config.ScopeUser)
}

func TestBaseFixtureNeverTouchesTheHost(t *testing.T) {
	f := newFixture(t)
	if f.deps.Root == "" {
		t.Fatal("the base fixture must root every host path in a temporary directory")
	}
	if f.deps.ProbeRegisterAccess == nil || f.deps.ReadAttribute == nil {
		t.Fatal("the base fixture must fake the hardware probes")
	}
	if f.deps.Accounts == nil {
		t.Fatal("the base fixture must inject fake accounts, never read the host passwd/group files")
	}
	if f.deps.Getenv("USER") == "" && f.deps.Getenv("LOGNAME") == "" {
		t.Fatal("the base fixture must supply an account name for the cgo-free fallback")
	}
}

func TestInstallUserScope(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	if !strings.Contains(f.out.String(), "Installed Rigscope 1.0.0 (user scope)") {
		t.Fatalf("output = %s", f.out.String())
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if !cfg.Components.Hub.Enabled || !cfg.Components.Collector.Enabled {
		t.Fatalf("default install should enable both: %+v", cfg.Components)
	}
	if cfg.Components.Hub.Port != 8081 || cfg.Components.Hub.Listen != "127.0.0.1" {
		t.Fatalf("hub defaults = %+v", cfg.Components.Hub)
	}
	info, err := os.Stat(paths.ConfigFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, err = %v", info.Mode().Perm(), err)
	}
	data, err := os.ReadFile(paths.Binary)
	if err != nil || string(data) != "binary-v1" {
		t.Fatalf("binary = %q, err = %v", data, err)
	}
	info, err = os.Stat(paths.Binary)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode = %v, err = %v", info.Mode().Perm(), err)
	}
	for _, component := range installComponents {
		unitPath := paths.UnitPath(component.UnitName())
		content, err := os.ReadFile(unitPath)
		if err != nil {
			t.Fatalf("unit %s: %v", component, err)
		}
		if !systemd.IsManaged(string(content)) || !strings.Contains(string(content), "ExecStart=") {
			t.Fatalf("unit %s invalid:\n%s", component, content)
		}
		if !f.fake.HasCall("enable", component.UnitName()) {
			t.Fatalf("%s not enabled: %v", component, f.fake.Methods())
		}
		if !f.fake.HasCall("start", component.UnitName()) {
			t.Fatalf("%s not started: %v", component, f.fake.Methods())
		}
	}
	if !f.fake.HasCall("daemon-reload", "") {
		t.Fatalf("daemon-reload missing: %v", f.fake.Methods())
	}
}

func TestInstallRoleAndOptions(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "hub", "--port", "9000", "--origin", "https://hub.example.com/", "--no-start", "--boot")
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Components.Hub.Enabled || cfg.Components.Collector.Enabled {
		t.Fatalf("role hub = %+v", cfg.Components)
	}
	if cfg.Components.Hub.Port != 9000 || cfg.Components.Hub.Origin != "https://hub.example.com" {
		t.Fatalf("hub = %+v", cfg.Components.Hub)
	}
	if !f.fake.HasCall("disable", systemd.Collector.UnitName()) {
		t.Fatalf("disabled collector was not disabled: %v", f.fake.Methods())
	}
	if f.fake.HasCall("start", systemd.Hub.UnitName()) {
		t.Fatal("--no-start started the hub")
	}
	if f.fake.LingerCalls != 1 {
		t.Fatalf("linger calls = %d", f.fake.LingerCalls)
	}
	if !strings.Contains(f.out.String(), "https://hub.example.com") {
		t.Fatalf("output has no hub URL: %s", f.out.String())
	}
}

func TestInstallCollectorEnrollmentFlags(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "collector", "--hub", "https://hub.example.com", "--token", "grant")
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	destinations := cfg.Components.Collector.Destinations
	if len(destinations) != 1 || destinations[0].URL != "wss://hub.example.com/api/collector/connect" || destinations[0].CAFile != "" {
		t.Fatalf("destinations = %+v", destinations)
	}
	raw, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "grant") {
		t.Fatalf("configuration contains the enrollment grant:\n%s", raw)
	}
	info, err := os.Stat(paths.CollectorDir())
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("collector dir mode = %v, err = %v", info.Mode().Perm(), err)
	}
	grant, err := os.ReadFile(paths.EnrollmentFile())
	if err != nil {
		t.Fatalf("enrollment grant: %v", err)
	}
	if !strings.Contains(string(grant), `"grant": "grant"`) || !strings.Contains(string(grant), `"url": "wss://hub.example.com/api/collector/connect"`) {
		t.Fatalf("grant file = %s", grant)
	}
	info, err = os.Stat(paths.EnrollmentFile())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("grant mode = %v, err = %v", info.Mode().Perm(), err)
	}
}

func TestInstallRejectsPlaintextRemoteHub(t *testing.T) {
	f := newFixture(t)
	if code := f.run(t, "install", "--role", "collector", "--hub", "http://hub.example.com", "--token", "grant"); code == 0 {
		t.Fatal("plaintext non-loopback destination accepted")
	}
	if !strings.Contains(f.err.String(), "plaintext ws:// is restricted to literal loopback") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestInstallNormalizesLocalhostHubName(t *testing.T) {
	// Plaintext loopback names become literal addresses; a TLS hostname is the
	// certificate identity and is kept.
	cases := map[string]string{
		"http://localhost:8081":      "ws://127.0.0.1:8081/api/collector/connect",
		"http://ip6-localhost:8081":  "ws://[::1]:8081/api/collector/connect",
		"https://ip6-localhost:8443": "wss://ip6-localhost:8443/api/collector/connect",
		"https://localhost:8443":     "wss://localhost:8443/api/collector/connect",
	}
	for hub, want := range cases {
		f := newFixture(t)
		paths := f.install(t, "--role", "collector", "--hub", hub, "--token", "grant")
		cfg, err := config.Load(paths.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Components.Collector.Destinations[0].URL; got != want {
			t.Fatalf("%s stored as %q, want %q", hub, got, want)
		}
	}
}

func TestInstallAcceptsLoopbackPlaintextHub(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "collector", "--hub", "http://127.0.0.1:8081", "--token", "grant")
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Components.Collector.Destinations[0].URL; got != "ws://127.0.0.1:8081/api/collector/connect" {
		t.Fatalf("destination = %s", got)
	}
}

func TestInstallRejectsIncompleteEnrollment(t *testing.T) {
	f := newFixture(t)
	if code := f.run(t, "install", "--hub", "https://hub.example.com"); code == 0 {
		t.Fatal("--hub without --token succeeded")
	}
	if !strings.Contains(f.err.String(), "--hub and --token") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestInstallRefusesLegacyInstallation(t *testing.T) {
	f := newFixture(t)
	unitDir := filepath.Join(f.home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(unitDir, "rigscope-0123456789ab-hub.service")
	if err := os.WriteFile(legacy, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "0.2 installation is present") || !strings.Contains(f.err.String(), "rigscope uninstall") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	if _, err := os.Stat(filepath.Join(f.home, ".config", "rigscope", "config.json")); !os.IsNotExist(err) {
		t.Fatal("install wrote a config despite refusing")
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--port", "9001")
	first, _ := os.ReadFile(paths.ConfigFile)
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install"); code != 0 {
		t.Fatalf("second install exit %d: %s", code, f.err.String())
	}
	second, _ := os.ReadFile(paths.ConfigFile)
	if string(first) != string(second) {
		t.Fatalf("config changed on reinstall:\n%s\n%s", first, second)
	}
	if _, err := os.Stat(paths.PreviousBinary); !os.IsNotExist(err) {
		t.Fatalf("identical reinstall created .prev: %v", err)
	}
}

func TestInstallSystemScope(t *testing.T) {
	f := newFixture(t)
	if code := f.run(t, "install", "--system"); code != 1 {
		t.Fatalf("non-root system install exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "requires root") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	f = newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	var commands []string
	f.deps.RunCommand = func(_ context.Context, name string, args ...string) error {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil
	}
	var chowns []string
	f.deps.Chown = func(path, userName, groupName string) error {
		chowns = append(chowns, path+" "+userName+":"+groupName)
		return nil
	}
	if code := f.run(t, "install", "--system", "--role", "both"); code != 0 {
		t.Fatalf("system install exit %d: %s", code, f.err.String())
	}
	paths := f.paths(t, config.ScopeSystem)
	if _, err := os.Stat(paths.ConfigFile); err != nil {
		t.Fatalf("system config: %v", err)
	}
	unit, err := os.ReadFile(paths.UnitPath(systemd.Hub.UnitName()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "User=rigscope-hub") || !strings.Contains(string(unit), "ProtectSystem=strict") {
		t.Fatalf("system unit:\n%s", unit)
	}
	if !strings.Contains(string(unit), "Group=rigscope\n") {
		t.Fatalf("system unit missing shared group:\n%s", unit)
	}
	collectorUnit, err := os.ReadFile(paths.UnitPath(systemd.Collector.UnitName()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(collectorUnit), "SupplementaryGroups=rigscope-rapl") {
		t.Fatalf("collector unit:\n%s", collectorUnit)
	}
	if strings.Contains(string(collectorUnit), "rigscope-gpu") {
		t.Fatalf("GPU register group installed without opt-in:\n%s", collectorUnit)
	}
	for _, file := range []string{paths.SysusersFile, paths.UdevRAPLFile} {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("system file %s: %v", file, err)
		}
		if !systemd.IsManaged(string(content)) {
			t.Fatalf("system file %s is not managed", file)
		}
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); !os.IsNotExist(err) {
		t.Fatalf("GPU udev rule installed without --gpu-registers: %v", err)
	}
	if !strings.Contains(strings.Join(commands, "\n"), "systemd-sysusers "+paths.SysusersFile) {
		t.Fatalf("sysusers not applied: %v", commands)
	}
	if !strings.Contains(strings.Join(commands, "\n"), "udevadm control --reload-rules") {
		t.Fatalf("udev not reloaded: %v", commands)
	}
	info, err := os.Stat(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("system config mode = %04o, want 0640", info.Mode().Perm())
	}
	stateInfo, err := os.Stat(paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if stateInfo.Mode().Perm() != 0o755 {
		t.Fatalf("system state root mode = %04o, want 0755", stateInfo.Mode().Perm())
	}
	joinedChowns := strings.Join(chowns, "\n")
	for _, wanted := range []string{
		paths.ConfigFile + " root:rigscope",
		paths.ComponentStateDir("hub") + " rigscope-hub:rigscope",
		paths.ComponentStateDir("collector") + " rigscope-collector:rigscope",
	} {
		if !strings.Contains(joinedChowns, wanted) {
			t.Fatalf("chown missing %q:\n%s", wanted, joinedChowns)
		}
	}
}

func TestUninstallSystemScopeRemovesIntegrationFiles(t *testing.T) {
	for _, keepData := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep-data=%t", keepData), func(t *testing.T) {
			f := newFixture(t)
			f.deps.Root = t.TempDir()
			f.deps.Euid = func() int { return 0 }
			f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
			if code := f.run(t, "install", "--system", "--gpu-registers", "--no-start"); code != 0 {
				t.Fatalf("install exit %d: %s", code, f.err.String())
			}
			paths := f.paths(t, config.ScopeSystem)
			integration := []string{paths.SysusersFile, paths.UdevRAPLFile, paths.UdevGPUTempsFile}
			for _, path := range integration {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("hardware setup did not create %s: %v", path, err)
				}
			}
			args := []string{"uninstall", "--system"}
			if keepData {
				args = append(args, "--keep-data")
			}
			if code := f.run(t, args...); code != 0 {
				t.Fatalf("uninstall exit %d: %s", code, f.err.String())
			}
			for _, path := range append(integration, paths.UnitPath(systemd.Hub.UnitName())) {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("%s still exists: %v", path, err)
				}
			}
			_, err := os.Stat(paths.ConfigFile)
			if keepData && err != nil || !keepData && !os.IsNotExist(err) {
				t.Fatalf("config preservation for keep-data=%t: %v", keepData, err)
			}
		})
	}
}

func TestUninstallRemovesEverything(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	stateFile := filepath.Join(paths.StateDir, "hub.db")
	if err := os.WriteFile(stateFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.fake.Reset()
	f.out.Reset()
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	for _, path := range []string{
		paths.ConfigFile,
		paths.Binary,
		paths.UnitPath(systemd.Hub.UnitName()),
		paths.UnitPath(systemd.Collector.UnitName()),
		stateFile,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", path, err)
		}
	}
	if !f.fake.HasCall("stop", systemd.Hub.UnitName()) || !f.fake.HasCall("disable", systemd.Collector.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
}

func TestUninstallKeepData(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	if code := f.run(t, "uninstall", "--keep-data"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.ConfigFile); err != nil {
		t.Fatalf("config removed: %v", err)
	}
	if _, err := os.Stat(paths.StateDir); err != nil {
		t.Fatalf("state removed: %v", err)
	}
	if _, err := os.Stat(paths.Binary); !os.IsNotExist(err) {
		t.Fatal("binary not removed")
	}
}

func TestUninstallRefusesForeignUnit(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	unit := paths.UnitPath(systemd.Hub.UnitName())
	if err := os.WriteFile(unit, []byte("[Unit]\nDescription=someone else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "uninstall"); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "refusing to remove unrecognized unit") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	if _, err := os.Stat(unit); err != nil {
		t.Fatalf("foreign unit removed: %v", err)
	}
}

func TestInstallWritesOwnershipManifest(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	info, err := os.Stat(paths.ManifestFile())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %04o, want 0600", info.Mode().Perm())
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if loaded.Marker != manifestMarker || loaded.Scope != config.ScopeUser || loaded.Version != "1.0.0" {
		t.Fatalf("manifest = %+v", loaded)
	}
	if !loaded.owns(paths.Binary, kindBinary) || !loaded.owns(paths.ConfigFile, kindConfig) || !loaded.owns(paths.StateDir, kindStateDir) {
		t.Fatalf("manifest paths = %+v", loaded.Paths)
	}
	if len(loaded.Units) != 2 || !loaded.owns(paths.UnitPath(systemd.Hub.UnitName()), kindUnit) {
		t.Fatalf("manifest units = %+v", loaded.Units)
	}
}

func TestInstallRefusesLiveUserZeroTwoInstallation(t *testing.T) {
	f := newFixture(t)
	unitDir := filepath.Join(f.home, ".config", "systemd", "user")
	stateDir := filepath.Join(f.home, ".local", "state", "rigscope")
	for _, dir := range []string{unitDir, stateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"rigscope-6629b1795b8f-hub.service", "rigscope-6629b1795b8f-collector.service"} {
		if err := os.WriteFile(filepath.Join(unitDir, name), []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.json"), []byte(`{"role":"local"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "0.2 installation is present") || !strings.Contains(f.err.String(), "rigscope-6629b1795b8f-hub.service") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	if _, err := os.Stat(filepath.Join(f.home, ".config", "rigscope", "config.json")); !os.IsNotExist(err) {
		t.Fatal("install wrote a config despite refusing")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "config.json")); err != nil {
		t.Fatalf("0.2 state was touched: %v", err)
	}
	if _, err := os.Stat(pathsManifest(f, t)); !os.IsNotExist(err) {
		t.Fatal("install wrote a manifest despite refusing")
	}
}

func TestInstallRefusesSystemZeroTwoInstallation(t *testing.T) {
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
	paths := f.paths(t, config.ScopeSystem)
	legacyUnit := paths.UnitPath(systemd.Hub.UnitName())
	if err := os.MkdirAll(filepath.Dir(legacyUnit), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyContent := "# Managed by `rigscope system`. Enable explicitly; do not edit in place.\n[Unit]\nDescription=Rigscope hub (system scope)\n"
	if err := os.WriteFile(legacyUnit, []byte(legacyContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.StateDir, "system-install.json"), []byte(`{"version":"0.2.6"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "install", "--system", "--no-start"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "0.2 installation is present") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	content, err := os.ReadFile(legacyUnit)
	if err != nil || string(content) != legacyContent {
		t.Fatalf("0.2 unit was modified: %q, %v", content, err)
	}
	if _, err := os.Stat(paths.ConfigFile); !os.IsNotExist(err) {
		t.Fatalf("system config written despite refusing: %v", err)
	}
}

func TestInstallRefusesForeignFiles(t *testing.T) {
	t.Run("config", func(t *testing.T) {
		f := newFixture(t)
		paths := f.paths(t, config.ScopeUser)
		if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.ConfigFile, []byte(`{"foreign":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := f.run(t, "install"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(f.err.String(), "not recorded in a Rigscope 1.x installation manifest") {
			t.Fatalf("stderr = %s", f.err.String())
		}
		content, _ := os.ReadFile(paths.ConfigFile)
		if string(content) != `{"foreign":true}` {
			t.Fatalf("foreign config was overwritten: %s", content)
		}
	})
	t.Run("state", func(t *testing.T) {
		f := newFixture(t)
		paths := f.paths(t, config.ScopeUser)
		if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(paths.StateDir, "keep.txt"), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := f.run(t, "install"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(f.err.String(), "non-empty state directory") {
			t.Fatalf("stderr = %s", f.err.String())
		}
	})
	t.Run("unit", func(t *testing.T) {
		f := newFixture(t)
		paths := f.paths(t, config.ScopeUser)
		unit := paths.UnitPath(systemd.Hub.UnitName())
		if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(unit, []byte("[Unit]\nDescription=foreign\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := f.run(t, "install"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(f.err.String(), "not recorded in a Rigscope 1.x installation manifest") {
			t.Fatalf("stderr = %s", f.err.String())
		}
	})
}

func TestUninstallWithoutManifestRefusesAndLeavesLiveData(t *testing.T) {
	f := newFixture(t)
	unitDir := filepath.Join(f.home, ".config", "systemd", "user")
	stateDir := filepath.Join(f.home, ".local", "state", "rigscope")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(unitDir, "rigscope-6629b1795b8f-collector.service")
	if err := os.WriteFile(unit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(stateDir, "config.json")
	if err := os.WriteFile(stateFile, []byte(`{"role":"collector"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "uninstall"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "0.2 installation is present") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	for _, path := range []string{unit, stateFile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("uninstall deleted live 0.2 data %s: %v", path, err)
		}
	}
}

func TestUninstallWithoutManifestCreatesNothing(t *testing.T) {
	f := newFixture(t)
	paths := f.paths(t, config.ScopeUser)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "uninstall"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "no Rigscope 1.x installation manifest") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	if _, err := os.Stat(paths.StateDir); !os.IsNotExist(err) {
		t.Fatalf("uninstall created the state directory: %v", err)
	}
	if _, err := os.Stat(paths.ConfigFile); err != nil {
		t.Fatalf("uninstall deleted a foreign config: %v", err)
	}
}

func TestUninstallAbortsWhenStopFails(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	stateFile := filepath.Join(paths.StateDir, "hub.db")
	if err := os.WriteFile(stateFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.fake.Fail["stop "+systemd.Hub.UnitName()] = fmt.Errorf("systemctl stop failed")
	if code := f.run(t, "uninstall"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "nothing was deleted") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	for _, path := range []string{
		paths.ConfigFile,
		paths.Binary,
		stateFile,
		paths.UnitPath(systemd.Hub.UnitName()),
		paths.UnitPath(systemd.Collector.UnitName()),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed stop deleted %s: %v", path, err)
		}
	}
	if f.fake.HasCall("disable", systemd.Hub.UnitName()) {
		t.Fatal("uninstall continued to disable after a failed stop")
	}
}

func TestUninstallDeletesOnlyRecordedPaths(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	extraConfig := filepath.Join(paths.ConfigDir, "notes.txt")
	extraUnit := paths.UnitPath("other.service")
	if err := os.WriteFile(extraConfig, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extraUnit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	for _, path := range []string{extraConfig, extraUnit} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("uninstall deleted unrecorded path %s: %v", path, err)
		}
	}
	if _, err := os.Stat(paths.StateDir); !os.IsNotExist(err) {
		t.Fatalf("recorded state directory not removed: %v", err)
	}
}

func TestUninstallAfterKeepDataOnlyRemovesRecordedData(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	if code := f.run(t, "uninstall", "--keep-data"); code != 0 {
		t.Fatalf("keep-data exit %d: %s", code, f.err.String())
	}
	kept, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatalf("kept manifest: %v", err)
	}
	if len(kept.Units) != 0 || kept.owns(paths.Binary, kindBinary) {
		t.Fatalf("keep-data manifest still claims removed files: %+v", kept)
	}
	if !kept.owns(paths.ConfigFile, kindConfig) || !kept.owns(paths.StateDir, kindStateDir) {
		t.Fatalf("keep-data manifest lost data ownership: %+v", kept.Paths)
	}
	extra := filepath.Join(paths.ConfigDir, "notes.txt")
	if err := os.WriteFile(extra, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("second uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.ConfigFile); !os.IsNotExist(err) {
		t.Fatalf("config not removed: %v", err)
	}
	if _, err := os.Stat(extra); err != nil {
		t.Fatalf("unrecorded file removed: %v", err)
	}
}

func TestInstallSystemGPUOptIn(t *testing.T) {
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	var commands []string
	f.deps.RunCommand = func(_ context.Context, name string, args ...string) error {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil
	}
	if code := f.run(t, "install", "--system", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.paths(t, config.ScopeSystem)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Components.Collector.Hardware.GPURegisters {
		t.Fatal("gpuRegisters not recorded")
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); err != nil {
		t.Fatalf("GPU rule missing: %v", err)
	}
	sysusers, err := os.ReadFile(paths.SysusersFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sysusers), "g rigscope-gpu") || !strings.Contains(string(sysusers), "m rigscope-collector rigscope-gpu") {
		t.Fatalf("GPU group missing:\n%s", sysusers)
	}
	unit, err := os.ReadFile(paths.UnitPath(systemd.Collector.UnitName()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "SupplementaryGroups=rigscope-rapl rigscope-gpu") {
		t.Fatalf("collector unit:\n%s", unit)
	}
	if !strings.Contains(strings.Join(commands, "\n"), "udevadm trigger --subsystem-match=pci") {
		t.Fatalf("PCI udev trigger missing: %v", commands)
	}
}

func TestInstallSystemHubOnlySkipsHardwareRules(t *testing.T) {
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
	if code := f.run(t, "install", "--system", "--role", "hub", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.paths(t, config.ScopeSystem)
	for _, file := range []string{paths.UdevRAPLFile, paths.UdevGPUTempsFile} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("hardware rule %s installed for --role hub", file)
		}
	}
	sysusers, err := os.ReadFile(paths.SysusersFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sysusers), "rigscope-rapl") || strings.Contains(string(sysusers), "rigscope-gpu") {
		t.Fatalf("hardware groups installed for --role hub:\n%s", sysusers)
	}
	unit, err := os.ReadFile(paths.UnitPath(systemd.Collector.UnitName()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "SupplementaryGroups=") {
		t.Fatalf("collector unit references missing groups:\n%s", unit)
	}
}

func TestSystemCollectorGrantIsPrivate(t *testing.T) {
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
	var chowns []string
	f.deps.Chown = func(path, userName, groupName string) error {
		chowns = append(chowns, path+" "+userName+":"+groupName)
		return nil
	}
	var fileChowns []string
	f.deps.ChownFile = func(file *os.File, userName, groupName string) error {
		fileChowns = append(fileChowns, file.Name()+" "+userName+":"+groupName)
		return nil
	}
	if code := f.run(t, "install", "--system", "--role", "collector", "--hub", "https://hub.example.com", "--token", "grant", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.paths(t, config.ScopeSystem)
	configData, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configData), "grant") {
		t.Fatalf("system config contains the grant:\n%s", configData)
	}
	if !strings.Contains(strings.Join(chowns, "\n"), paths.CollectorDir()+" rigscope-collector:rigscope") {
		t.Fatalf("collector directory chown missing:\n%s", strings.Join(chowns, "\n"))
	}
	if strings.Contains(strings.Join(chowns, "\n"), paths.EnrollmentFile()) {
		t.Fatalf("enrollment grant was chowned by pathname instead of through its descriptor:\n%s", strings.Join(chowns, "\n"))
	}
	joinedFiles := strings.Join(fileChowns, "\n")
	if !strings.Contains(joinedFiles, "enrollment.json") || !strings.Contains(joinedFiles, "rigscope-collector:rigscope") {
		t.Fatalf("enrollment grant descriptor chown missing:\n%s", joinedFiles)
	}
}

func TestDoctorSystemScopePermissions(t *testing.T) {
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
	f.deps.FileOwner = func(string) (int, int, error) { return 0, 0, nil }
	f.deps.GroupID = func(string) (int, error) { return 0, nil }
	f.deps.UserID = func(string) (int, error) { return 0, nil }
	if code := f.run(t, "install", "--system", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.paths(t, config.ScopeSystem)
	f.out.Reset()
	if code := f.run(t, "doctor", "--system"); code != 0 {
		t.Fatalf("doctor exit %d: %s\n%s", code, f.err.String(), f.out.String())
	}
	if !strings.Contains(f.out.String(), "Configuration permissions: 0640") {
		t.Fatalf("doctor output:\n%s", f.out.String())
	}
	if err := os.Chmod(paths.ConfigFile, 0o600); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if code := f.run(t, "doctor", "--system"); code != 1 {
		t.Fatalf("world-visible system config accepted: exit %d\n%s", code, f.out.String())
	}
	if !strings.Contains(f.out.String(), "must be 0640") {
		t.Fatalf("doctor output:\n%s", f.out.String())
	}
}

func TestInstallAndServiceCommandsTakeTheLock(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	held, err := lock.Acquire(paths.LockFile())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	for _, args := range [][]string{
		{"install"},
		{"start", "hub"},
		{"stop", "collector"},
		{"restart"},
		{"uninstall", "--keep-data"},
	} {
		f.err.Reset()
		if code := f.run(t, args...); code != 1 {
			t.Fatalf("%v exit = %d, want 1", args, code)
		}
		if !strings.Contains(f.err.String(), "another rigscope operation is running") {
			t.Fatalf("%v stderr = %s", args, f.err.String())
		}
	}
}

func pathsManifest(f *fixture, t *testing.T) string {
	t.Helper()
	return f.paths(t, config.ScopeUser).ManifestFile()
}

func TestStatusJSON(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	f.out.Reset()
	if code := f.run(t, "status", "--json"); code != 0 {
		t.Fatalf("status exit %d: %s", code, f.err.String())
	}
	var report statusReport
	if err := json.Unmarshal(f.out.Bytes(), &report); err != nil {
		t.Fatalf("json: %v\n%s", err, f.out.String())
	}
	if report.Scope != "user" || !report.Installed || !report.ConfigRead || !report.BinarySet {
		t.Fatalf("report = %+v", report)
	}
	hub := report.Components["hub"]
	if !hub.Enabled || !hub.Installed || !hub.Active {
		t.Fatalf("hub status = %+v", hub)
	}
	if report.Components["collector"].Destinations != 1 {
		t.Fatalf("collector status = %+v", report.Components["collector"])
	}
}

func TestStatusTextBeforeInstall(t *testing.T) {
	f := newFixture(t)
	if code := f.run(t, "status"); code != 0 {
		t.Fatalf("status exit %d: %s", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), "not installed") {
		t.Fatalf("output = %s", f.out.String())
	}
}

func TestServiceCommands(t *testing.T) {
	f := newFixture(t)
	f.install(t, "--no-start")
	f.fake.Reset()
	f.fake.ActiveByDefault = false
	if code := f.run(t, "start", "hub"); code != 0 {
		t.Fatalf("start exit %d: %s", code, f.err.String())
	}
	if !f.fake.HasCall("start", systemd.Hub.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
	// start on an active unit is a systemctl no-op; it must not claim a start.
	f.fake.Reset()
	f.out.Reset()
	f.fake.Active[systemd.Hub.UnitName()] = true
	if code := f.run(t, "start", "hub"); code != 0 {
		t.Fatalf("start exit %d: %s", code, f.err.String())
	}
	if f.fake.HasCall("start", systemd.Hub.UnitName()) || !strings.Contains(f.out.String(), "Already running: rigscope-hub.service") {
		t.Fatalf("calls = %v, output = %s", f.fake.Methods(), f.out.String())
	}
	if code := f.run(t, "stop", "collector"); code != 0 {
		t.Fatalf("stop exit %d: %s", code, f.err.String())
	}
	if !f.fake.HasCall("stop", systemd.Collector.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
	if code := f.run(t, "restart"); code != 0 {
		t.Fatalf("restart exit %d: %s", code, f.err.String())
	}
	if !f.fake.HasCall("restart", systemd.Hub.UnitName()) || !f.fake.HasCall("restart", systemd.Collector.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
}

func TestServiceRejectsUnknownComponent(t *testing.T) {
	f := newFixture(t)
	f.install(t, "--no-start")
	if code := f.run(t, "start", "database"); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "unknown component") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestLogsCommand(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	var gotName string
	var gotArgs []string
	f.deps.RunStream = func(_ context.Context, name string, args ...string) error {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return nil
	}
	if code := f.run(t, "logs", "--follow", "hub"); code != 0 {
		t.Fatalf("logs exit %d: %s", code, f.err.String())
	}
	if gotName != "journalctl" {
		t.Fatalf("command = %s", gotName)
	}
	want := "--user --unit rigscope-hub.service --lines=100 --no-pager --follow"
	if strings.Join(gotArgs, " ") != want {
		t.Fatalf("args = %v\nwant %s", gotArgs, want)
	}
}

func TestInstallScriptSyntax(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "install.sh")
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	if output, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("sh -n %s: %v\n%s", path, err, output)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{
		"set -eu", "x86_64", "aarch64", "armv7l", "sha256sum", "shasum", "checksums.txt",
		"--https-only", "209715200", "not a regular file",
		`"$tmp/extract/rigscope" install "$@"`,
	} {
		if !strings.Contains(string(content), wanted) {
			t.Fatalf("install.sh missing %q", wanted)
		}
	}
	if strings.Contains(string(content), `exec "$tmp/rigscope"`) {
		t.Fatal("install.sh execs the installer, so its temp dir leak trap never runs")
	}
}

func TestExtractBinary(t *testing.T) {
	archive := testArchive(t, map[string]string{"README.md": "docs", "rigscope": "BINARY"})
	data, err := extractBinary(archive, 1024)
	if err != nil || string(data) != "BINARY" {
		t.Fatalf("data = %q, err = %v", data, err)
	}
	if _, err := extractBinary(archive, 3); err == nil {
		t.Fatal("size limit not enforced")
	}
}

func TestExtractBinaryRejectsTraversal(t *testing.T) {
	archive := tarOnly(t, "../rigscope", tar.TypeReg, []byte("evil"))
	if _, err := extractBinary(archive, 1024); err == nil || !strings.Contains(err.Error(), "escapes the archive root") {
		t.Fatalf("err = %v", err)
	}
	archive = tarOnly(t, "rigscope", tar.TypeSymlink, nil)
	if _, err := extractBinary(archive, 1024); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
	if _, err := extractBinary(testArchive(t, map[string]string{"other": "x"}), 1024); err == nil {
		t.Fatal("archive without rigscope accepted")
	}
}

func TestChecksumVerification(t *testing.T) {
	sum := sha256.Sum256([]byte("asset"))
	good := []byte(hex.EncodeToString(sum[:]) + "  rigscope_1.0.0_linux_amd64.tar.gz\n")
	if err := verifyChecksum(good, "rigscope_1.0.0_linux_amd64.tar.gz", []byte("asset")); err != nil {
		t.Fatalf("good checksum: %v", err)
	}
	bad := []byte(hex.EncodeToString(sum[:]) + "  other.tar.gz\n")
	if err := verifyChecksum(bad, "rigscope_1.0.0_linux_amd64.tar.gz", []byte("asset")); err == nil {
		t.Fatal("missing entry accepted")
	}
	if err := verifyChecksum(good, "rigscope_1.0.0_linux_amd64.tar.gz", []byte("tampered")); err == nil {
		t.Fatal("mismatch accepted")
	}
}

func TestHubHealthURL(t *testing.T) {
	cfg := config.Default()
	if got := hubHealthURL(cfg); got != "http://127.0.0.1:8081/api/health" {
		t.Fatalf("url = %s", got)
	}
	cfg.Components.Hub.Listen = "0.0.0.0"
	cfg.Components.Hub.Port = 9000
	if got := hubHealthURL(cfg); got != "http://127.0.0.1:9000/api/health" {
		t.Fatalf("url = %s", got)
	}
	cfg.Components.Hub.Listen = "::1"
	if got := hubHealthURL(cfg); got != "http://[::1]:9000/api/health" {
		t.Fatalf("url = %s", got)
	}
}

// releaseFixture serves GitHub-like release metadata and assets.
type releaseFixture struct {
	version string
	binary  []byte
	sums    string
	server  *httptest.Server
}

func newReleaseFixture(t *testing.T, version string, binary []byte, corruptChecksum bool) *releaseFixture {
	t.Helper()
	archive := testArchive(t, map[string]string{"rigscope": string(binary)})
	asset := fmt.Sprintf("rigscope_%s_linux_amd64.tar.gz", version)
	sum := sha256.Sum256(archive)
	line := hex.EncodeToString(sum[:])
	if corruptChecksum {
		line = strings.Repeat("0", 64)
	}
	release := releaseFixture{
		version: version,
		binary:  binary,
		sums:    line + "  " + asset + "\n",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		release.writeMetadata(t, w, asset)
	})
	mux.HandleFunc("/releases/tags/", func(w http.ResponseWriter, r *http.Request) {
		release.writeMetadata(t, w, asset)
	})
	mux.HandleFunc("/dl/"+asset, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("/dl/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(release.sums))
	})
	release.server = httptest.NewServer(mux)
	t.Cleanup(release.server.Close)
	return &release
}

func (r *releaseFixture) writeMetadata(t *testing.T, w http.ResponseWriter, asset string) {
	t.Helper()
	response := map[string]any{
		"tag_name": "v" + r.version,
		"assets": []map[string]string{
			{"name": asset, "browser_download_url": r.server.URL + "/dl/" + asset},
			{"name": "checksums.txt", "browser_download_url": r.server.URL + "/dl/checksums.txt"},
		},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Errorf("encode metadata: %v", err)
	}
}

func (f *fixture) useRelease(release *releaseFixture) {
	f.deps.APIBase = release.server.URL + "/releases"
	f.deps.HTTPClient = release.server.Client()
}

func TestUpgradeDownloadsVerifiesAndRetainsPrevious(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	next := upgradeScript("1.1.0")
	release := newReleaseFixture(t, "1.1.0", next, false)
	f.useRelease(release)
	f.fake.Reset()
	f.out.Reset()
	if code := f.run(t, "upgrade"); code != 0 {
		t.Fatalf("upgrade exit %d: %s", code, f.err.String())
	}
	paths := f.paths(t, config.ScopeUser)
	checkContent(t, paths.Binary, string(next))
	checkContent(t, paths.PreviousBinary, "binary-v1")
	if !f.fake.HasCall("restart", systemd.Hub.UnitName()) || !f.fake.HasCall("restart", systemd.Collector.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
	if !strings.Contains(f.out.String(), "Upgraded to Rigscope 1.1.0") {
		t.Fatalf("output = %s", f.out.String())
	}
}

func TestUpgradePinnedVersion(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	next := upgradeScript("0.9.0")
	release := newReleaseFixture(t, "0.9.0", next, false)
	f.useRelease(release)
	if code := f.run(t, "upgrade", "--version", "0.9.0"); code != 0 {
		t.Fatalf("upgrade exit %d: %s", code, f.err.String())
	}
	checkContent(t, f.paths(t, config.ScopeUser).Binary, string(next))
}

func TestUpgradeRejectsChecksumMismatch(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	release := newReleaseFixture(t, "1.1.0", []byte("binary-v2"), true)
	f.useRelease(release)
	f.fake.Reset()
	if code := f.run(t, "upgrade"); code != 1 {
		t.Fatalf("upgrade exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "checksum mismatch") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	paths := f.paths(t, config.ScopeUser)
	checkContent(t, paths.Binary, "binary-v1")
	if _, err := os.Stat(paths.PreviousBinary); !os.IsNotExist(err) {
		t.Fatalf("failed upgrade created .prev: %v", err)
	}
	if f.fake.HasCall("restart", systemd.Hub.UnitName()) {
		t.Fatal("failed upgrade restarted services")
	}
}

func TestUpgradeRollsBackWhenUnhealthy(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	next := upgradeScript("1.1.0")
	release := newReleaseFixture(t, "1.1.0", next, false)
	f.useRelease(release)
	calls := 0
	f.deps.ProbeHealth = func(context.Context, string) error {
		calls++
		// The new release fails every readiness attempt. The restored release
		// then answers health checks so the rollback can complete.
		if calls <= 20 {
			return fmt.Errorf("hub health failed")
		}
		return nil
	}
	f.fake.Reset()
	if code := f.run(t, "upgrade"); code != 1 {
		t.Fatalf("upgrade exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "rolled back") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	paths := f.paths(t, config.ScopeUser)
	checkContent(t, paths.Binary, "binary-v1")
	checkContent(t, paths.PreviousBinary, string(next))
}

func TestUpgradeAlreadyCurrent(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	release := newReleaseFixture(t, "1.0.0", []byte("binary-v2"), false)
	f.useRelease(release)
	f.out.Reset()
	if code := f.run(t, "upgrade"); code != 0 {
		t.Fatalf("exit = %d: %s", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), "already installed") {
		t.Fatalf("output = %s", f.out.String())
	}
	checkContent(t, f.paths(t, config.ScopeUser).Binary, "binary-v1")
}

func TestRollbackSwapsPreviousBack(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	next := upgradeScript("1.1.0")
	release := newReleaseFixture(t, "1.1.0", next, false)
	f.useRelease(release)
	if code := f.run(t, "upgrade"); code != 0 {
		t.Fatalf("upgrade exit %d: %s", code, f.err.String())
	}
	paths := f.paths(t, config.ScopeUser)
	f.out.Reset()
	if code := f.run(t, "rollback"); code != 0 {
		t.Fatalf("rollback exit %d: %s", code, f.err.String())
	}
	checkContent(t, paths.Binary, "binary-v1")
	checkContent(t, paths.PreviousBinary, string(next))
	if !strings.Contains(f.out.String(), "Rolled back") {
		t.Fatalf("output = %s", f.out.String())
	}
}

func TestRollbackRefusesIncompatibleMigration(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	next := upgradeScriptWithMigration("1.1.0", 2, 2) // breaking migration: floor raised to 2
	release := newReleaseFixture(t, "1.1.0", next, false)
	f.useRelease(release)
	if code := f.run(t, "upgrade"); code != 0 {
		t.Fatalf("upgrade exit %d: %s", code, f.err.String())
	}
	f.err.Reset()
	if code := f.run(t, "rollback"); code != 1 {
		t.Fatalf("rollback exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "not allowed") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	checkContent(t, f.paths(t, config.ScopeUser).Binary, string(next))
}

func TestRollbackWithoutPrevious(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	if code := f.run(t, "rollback"); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "no previous binary") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestMigrateCommandReportsSchema(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	f.out.Reset()
	if code := f.run(t, "migrate", "--config", paths.ConfigFile, "--state", paths.StateDir, "--from", "1.0.0", "--to", "1.1.0"); code != 0 {
		t.Fatalf("migrate exit %d: %s", code, f.err.String())
	}
	var report struct {
		Schema         int `json:"schema"`
		CompatibleDown int `json:"compatibleDown"`
	}
	if err := json.Unmarshal(f.out.Bytes(), &report); err != nil {
		t.Fatalf("json: %v\n%s", err, f.out.String())
	}
	if report.Schema != config.CurrentVersion || report.CompatibleDown != config.CurrentVersion {
		t.Fatalf("report = %+v", report)
	}
}

func TestMigrateAcceptsAnOlderUnnormalizedHubDestination(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	// An earlier installer stored the user-facing hub address without
	// normalizing it: https scheme and no collector path.
	data, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	components := document["components"].(map[string]any)
	collector := components["collector"].(map[string]any)
	collector["destinations"] = []any{map[string]any{"url": "https://hub.example.com"}}
	data, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "migrate", "--config", paths.ConfigFile, "--state", paths.StateDir); code != 0 {
		t.Fatalf("migrate exit %d: %s", code, f.err.String())
	}
	loaded, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := loaded.Components.Collector.Destinations[0].URL; got != "wss://hub.example.com/api/collector/connect" {
		t.Fatalf("destination = %q", got)
	}
}

func TestMigrateTakesTheSharedInstallationLock(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	held, err := lock.Acquire(paths.LockFile())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	f.err.Reset()
	if code := f.run(t, "migrate", "--config", paths.ConfigFile, "--state", paths.StateDir); code != 1 {
		t.Fatalf("migrate exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "another rigscope operation is running") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestMigrateRefusesWhileTheHubIsRunning(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	// A live hub (or offline recovery command) holds this exact lease.
	held, err := lock.Acquire(paths.HubRuntimeLock())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	f.err.Reset()
	if code := f.run(t, "migrate", "--config", paths.ConfigFile, "--state", paths.StateDir); code != 1 {
		t.Fatalf("migrate exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "the hub is running") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestUpgradeRefusesWhileTheHubIsRunning(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	release := newReleaseFixture(t, "1.1.0", upgradeScript("1.1.0"), false)
	f.useRelease(release)
	held, err := lock.Acquire(paths.HubRuntimeLock())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	f.err.Reset()
	if code := f.run(t, "upgrade"); code != 1 {
		t.Fatalf("upgrade exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "the hub is running") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	checkContent(t, paths.Binary, "binary-v1")
}

func TestUpgradeTakesTheSharedInstallationLock(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	held, err := lock.Acquire(paths.LockFile())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	f.err.Reset()
	if code := f.run(t, "upgrade"); code != 1 {
		t.Fatalf("upgrade exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "already running") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestUpgradeRecoversPendingJournalBeforeLoadingConfig(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	journal := activation.Journal{
		ID:    "recovery1",
		Phase: activation.PhaseQuiesced,
		From:  "1.0.0",
		To:    "1.1.0",
		Layout: activation.Layout{
			Scope:             string(config.ScopeUser),
			StateDir:          paths.StateDir,
			Binary:            paths.Binary,
			PreviousBinary:    paths.PreviousBinary,
			ConfigFile:        paths.ConfigFile,
			CollectorStateDir: filepath.Join(paths.StateDir, "collector"),
			HubDBFile:         filepath.Join(paths.StateDir, "hub.db"),
			JournalPath:       filepath.Join(paths.StateDir, "upgrade-journal.json"),
		},
		Settings:        activation.Settings{},
		ServicesStopped: true,
	}
	store := activation.JournalStore{Path: filepath.Join(paths.StateDir, "upgrade-journal.json"), FS: activation.OSFS{}}
	if err := store.Save(&journal); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.err.Reset()
	if code := f.run(t, "upgrade"); code != 1 {
		t.Fatalf("upgrade exit = %d", code)
	}
	recovered, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Phase != activation.PhaseRolledBack {
		t.Fatalf("journal was not recovered before config loading: %s", recovered.Phase)
	}
}

func TestDoctorHealthyInstallation(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	f.out.Reset()
	if code := f.run(t, "doctor"); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, f.err.String())
	}
	output := f.out.String()
	for _, wanted := range []string{"Configuration", "Installed binary", "Unit rigscope-hub.service", "CPU package power (RAPL)", "0 failure(s)"} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("doctor output missing %q:\n%s", wanted, output)
		}
	}
}

func TestDoctorReportsInvalidConfig(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	if err := os.WriteFile(paths.ConfigFile, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "doctor"); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(f.out.String(), "[fail] Configuration") {
		t.Fatalf("output = %s", f.out.String())
	}
}

func TestUsageListsCommands(t *testing.T) {
	var buffer bytes.Buffer
	Usage(&buffer)
	for _, wanted := range []string{"install", "uninstall", "upgrade", "rollback", "doctor", "status", "logs"} {
		if !strings.Contains(buffer.String(), wanted) {
			t.Fatalf("usage missing %q", wanted)
		}
	}
}

func upgradeScript(version string) []byte {
	return upgradeScriptWithMigration(version, 1, 1)
}

func upgradeScriptWithMigration(version string, schema, compatibleDown int) []byte {
	return []byte(fmt.Sprintf(`#!/bin/sh
case "$1" in
version) echo "rigscope %s" ;;
migrate) echo '{"schema":%d,"compatibleDown":%d}' ;;
*) exit 0 ;;
esac
`, version, schema, compatibleDown))
}

func checkContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func testArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	for name, content := range files {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func tarOnly(t *testing.T, name string, kind byte, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gz)
	header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: kind}
	if kind == tar.TypeSymlink {
		header.Linkname = "/etc/passwd"
	}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if len(content) > 0 {
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestCollectorCheckReportsAwaitingEnrollment(t *testing.T) {
	f := newFixture(t)
	// A collector-only install has no local hub to pair with, so it stays
	// awaiting a destination grant.
	paths := f.install(t, "--role", "collector", "--no-start")
	code := f.run(t, "collector", "--check", "--config", paths.ConfigFile, "--state-dir", paths.StateDir)
	if code != 0 {
		t.Fatalf("collector --check exit %d: %s", code, f.err.String())
	}
	if output := f.out.String(); !strings.Contains(output, "collector check: ok") ||
		!strings.Contains(output, "awaiting enrollment") {
		t.Fatalf("output = %s", output)
	}
}

func TestCollectorCheckFailsForInvalidConfigAndState(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	// Write the invalid value directly: Save refuses it, and the readiness
	// check must reject a file that is already on disk.
	data, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	invalid := strings.Replace(string(data), `"samplingInterval": "2s"`, `"samplingInterval": "1ms"`, 1)
	if invalid == string(data) {
		t.Fatalf("sampling interval not found in %s", data)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "collector", "--check", "--config", paths.ConfigFile, "--state-dir", paths.StateDir); code != 1 {
		t.Fatalf("invalid sampling interval: exit %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "samplingInterval") {
		t.Fatalf("stderr = %s", f.err.String())
	}

	f = newFixture(t)
	paths = f.install(t)
	if code := f.run(t, "collector", "--check", "--config", paths.ConfigFile, "--state-dir", filepath.Join(f.home, "missing")); code != 1 {
		t.Fatalf("missing state directory: exit %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "state directory") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestCollectorDisabledExitsWithoutWaiting(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Components.Collector.Enabled = false
	if err := config.Save(paths.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "collector", "--config", paths.ConfigFile, "--state-dir", paths.StateDir); code != 0 {
		t.Fatalf("disabled collector exit %d: %s", code, f.err.String())
	}
	if !strings.Contains(f.err.String(), "disabled") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestCollectorEnrollNeedsAGrant(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "collector", "--hub", "http://127.0.0.1:8081", "--token", "grant")
	// The install staged the grant: enrollment must consume it rather than ask
	// for a second copy on the command line.
	code := f.run(t, "collector", "enroll", "--config", paths.ConfigFile, "--state-dir", paths.StateDir)
	if code == 0 {
		t.Fatalf("enrollment without a reachable hub must fail: %s", f.out.String())
	}
	if strings.Contains(f.err.String(), "no enrollment grant") {
		t.Fatalf("the staged install grant was not used: %s", f.err.String())
	}

	f = newFixture(t)
	paths = f.install(t, "--role", "collector")
	if code := f.run(t, "collector", "enroll", "--config", paths.ConfigFile, "--state-dir", paths.StateDir); code != 1 {
		t.Fatalf("enrollment without a grant or destination: exit %d, want 1", code)
	}
	if !strings.Contains(f.err.String(), "no hub URL") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestCollectorRejectsUnexpectedArguments(t *testing.T) {
	f := newFixture(t)
	if code := f.run(t, "collector", "nonsense"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

// A system install owns its rigscope-gpu rule: reinstalling keeps it owned and
// uninstall removes it.
func TestSystemGPURuleStaysOwnedAcrossReinstall(t *testing.T) {
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
	for run := 0; run < 2; run++ {
		f.err.Reset()
		if code := f.run(t, "install", "--system", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install %d exit %d: %s", run+1, code, f.err.String())
		}
	}
	paths := f.paths(t, config.ScopeSystem)
	if entry := f.manifest(t, paths).entry(paths.UdevGPUTempsFile, kindUdevGPU); entry == nil || entry.State != manifestOwned {
		t.Fatalf("system GPU rule entry = %+v, want owned", entry)
	}
	f.err.Reset()
	if code := f.run(t, "uninstall", "--system"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); !os.IsNotExist(err) {
		t.Fatalf("system uninstall left the GPU rule: %v", err)
	}
}
