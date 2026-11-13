package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/collector/state"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// enrollLocalCollector completes the automatic pairing the install staged, as
// the collector would after redeeming its grant.
func enrollLocalCollector(t *testing.T, paths config.Paths) string {
	t.Helper()
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	url := cfg.Components.Collector.Destinations[0].URL
	store, err := state.Open(paths.StateDir, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteEnrollment(url, state.Credential{URL: url, Token: "issued-credential"}); err != nil {
		t.Fatal(err)
	}
	return url
}

func loadConfig(t *testing.T, paths config.Paths) *config.Config {
	t.Helper()
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A reinstall with a new port used to call systemctl start on running units,
// which does nothing, so the hub kept its old port. Running units restart.
func TestReinstallWithNewPortRestartsRunningServices(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	f.fake.Reset()
	f.prompts = nil
	if code := f.run(t, "install", "--port", "9090"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if got := loadConfig(t, paths).Components.Hub.Port; got != 9090 {
		t.Fatalf("port = %d", got)
	}
	for _, component := range installComponents {
		if !f.fake.HasCall("restart", component.UnitName()) {
			t.Fatalf("%s not restarted: %v", component, f.fake.Methods())
		}
	}
	if len(f.prompts) != 1 {
		t.Fatalf("prompts = %v", f.prompts)
	}
	if !strings.Contains(f.err.String(), "hub and collector are running and will restart") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestDeclinedReinstallChangesNothing(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	before, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	f.fake.Reset()
	f.answer = false
	if code := f.run(t, "install", "--port", "9090"); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	after, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("declined install rewrote the configuration")
	}
	for _, method := range []string{"stop", "restart", "start", "daemon-reload"} {
		for _, component := range installComponents {
			if f.fake.HasCall(method, component.UnitName()) {
				t.Fatalf("declined install called %s: %v", method, f.fake.Methods())
			}
		}
	}
	if !strings.Contains(f.err.String(), "cancelled; nothing was stopped or changed") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

func TestStopPromptWithoutTerminalRequiresYes(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	f.deps.Confirm = nil
	f.fake.Reset()
	if code := f.run(t, "install", "--port", "9191"); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "run again with --yes") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	for i, flag := range []string{"--yes", "-y"} {
		f.err.Reset()
		if code := f.run(t, "install", flag, "--port", strconv.Itoa(9192+i)); code != 0 {
			t.Fatalf("install %s exit %d: %s", flag, code, f.err.String())
		}
	}
}

func TestReinstallNoStartLeavesRunningServicesAlone(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	f.fake.Reset()
	f.prompts = nil
	f.out.Reset()
	if code := f.run(t, "install", "--no-start", "--port", "9191"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	for _, method := range []string{"stop", "restart", "start"} {
		for _, component := range installComponents {
			if f.fake.HasCall(method, component.UnitName()) {
				t.Fatalf("--no-start called %s: %v", method, f.fake.Methods())
			}
		}
	}
	if len(f.prompts) != 0 {
		t.Fatalf("prompts = %v", f.prompts)
	}
	if !strings.Contains(f.out.String(), "keep the previous binary and settings until rigscope restart") {
		t.Fatalf("stdout = %s", f.out.String())
	}
}

func TestConfigurePortMovesTheEnrolledLocalCollector(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	oldURL := enrollLocalCollector(t, paths)
	binaryBefore, err := os.ReadFile(paths.Binary)
	if err != nil {
		t.Fatal(err)
	}
	f.fake.Reset()
	f.out.Reset()
	f.prompts = nil
	if code := f.run(t, "configure", "--port", "9191"); code != 0 {
		t.Fatalf("configure exit %d: %s", code, f.err.String())
	}
	cfg := loadConfig(t, paths)
	if cfg.Components.Hub.Port != 9191 {
		t.Fatalf("port = %d", cfg.Components.Hub.Port)
	}
	newURL := cfg.Components.Collector.Destinations[0].URL
	if newURL == oldURL || !strings.Contains(newURL, ":9191/") {
		t.Fatalf("destination = %s (was %s)", newURL, oldURL)
	}
	store, err := state.Open(paths.StateDir, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if credential, found, err := store.Credential(newURL); err != nil || !found || credential.Token != "issued-credential" {
		t.Fatalf("credential at new address = %+v, %v, %v", credential, found, err)
	}
	if _, found, _ := store.Credential(oldURL); found {
		t.Fatal("credential for the old address was kept")
	}
	if _, err := os.Stat(paths.EnrollmentFile()); !os.IsNotExist(err) {
		t.Fatalf("configure staged a new grant for an enrolled collector: %v", err)
	}
	for _, component := range installComponents {
		if !f.fake.HasCall("restart", component.UnitName()) {
			t.Fatalf("%s not restarted: %v", component, f.fake.Methods())
		}
	}
	binaryAfter, err := os.ReadFile(paths.Binary)
	if err != nil || !bytes.Equal(binaryBefore, binaryAfter) {
		t.Fatalf("configure replaced the binary: %v", err)
	}
	out := f.out.String()
	for _, want := range []string{"hub port: 8081 -> 9191", "restarted: hub, collector", "moved to the hub's new local address"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestConfigureWithoutChangesDoesNothing(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	port := loadConfig(t, paths).Components.Hub.Port
	f.fake.Reset()
	f.prompts = nil
	f.out.Reset()
	if code := f.run(t, "configure", "--port", "8081"); code != 0 || port != 8081 {
		t.Fatalf("exit %d (port %d): %s", code, port, f.err.String())
	}
	if len(f.fake.Calls) != 0 || len(f.prompts) != 0 {
		t.Fatalf("calls = %v, prompts = %v", f.fake.Methods(), f.prompts)
	}
	if !strings.Contains(f.out.String(), "Nothing to change") {
		t.Fatalf("stdout = %s", f.out.String())
	}
}

func TestConfigureNoRestartLeavesServicesStopped(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	f.fake.Reset()
	f.out.Reset()
	if code := f.run(t, "configure", "--port", "9292", "--no-restart", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	for _, component := range installComponents {
		if !f.fake.HasCall("stop", component.UnitName()) || f.fake.HasCall("restart", component.UnitName()) || f.fake.HasCall("start", component.UnitName()) {
			t.Fatalf("calls = %v", f.fake.Methods())
		}
	}
	if !strings.Contains(f.out.String(), "stopped: collector, hub") {
		t.Fatalf("stdout = %s", f.out.String())
	}
}

func TestConfigureRequiresAnInstallationAndAChange(t *testing.T) {
	f := newFixture(t)
	if code := f.run(t, "configure", "--port", "9000"); code != 1 || !strings.Contains(f.err.String(), "run rigscope install first") {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	f.err.Reset()
	if code := f.run(t, "configure"); code != 1 || !strings.Contains(f.err.String(), "configure needs a change") {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	f.err.Reset()
	if code := f.run(t, "configure", "--local", "--origin", "https://hub.example.com"); code != 1 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
}

func TestDeclinedUninstallKeepsEverything(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	f.fake.Reset()
	f.answer = false
	if code := f.run(t, "uninstall"); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(paths.Binary); err != nil {
		t.Fatalf("binary removed after a declined uninstall: %v", err)
	}
	if f.fake.HasCall("stop", systemd.Hub.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
}

func TestUninstallYesSkipsThePrompt(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	f.prompts = nil
	f.fake.Reset()
	if code := f.run(t, "uninstall", "-y"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if len(f.prompts) != 0 {
		t.Fatalf("prompts = %v", f.prompts)
	}
	if _, err := os.Stat(paths.Binary); !os.IsNotExist(err) {
		t.Fatalf("binary still present: %v", err)
	}
	if f.fake.HasCall("start", systemd.Hub.UnitName()) || f.fake.HasCall("restart", systemd.Hub.UnitName()) {
		t.Fatalf("uninstall restarted a service: %v", f.fake.Methods())
	}
}

func TestColorIsOnlyAppliedWhenEnabled(t *testing.T) {
	var plain, colored bytes.Buffer
	(Deps{Stderr: &plain}).errorf("boom %d", 1)
	if plain.String() != "rigscope: boom 1\n" {
		t.Fatalf("plain = %q", plain.String())
	}
	(Deps{Stderr: &colored, ColorStderr: true}).errorf("boom %d", 1)
	if !strings.Contains(colored.String(), "\x1b[31;1mrigscope:\x1b[0m boom 1") {
		t.Fatalf("colored = %q", colored.String())
	}
	if colorAllowed(func(key string) string {
		if key == "NO_COLOR" {
			return "1"
		}
		return ""
	}, os.Stdout) {
		t.Fatal("NO_COLOR did not disable color")
	}
	if colorAllowed(func(string) string { return "" }, &plain) {
		t.Fatal("a buffer is not a terminal")
	}
}

func TestDeclinedUpgradeStartsNoTransaction(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	f.useRelease(newReleaseFixture(t, "1.1.0", upgradeScript("1.1.0"), false))
	f.fake.Reset()
	f.answer = false
	if code := f.run(t, "upgrade"); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if _, err := os.Stat(journalPath(paths)); !os.IsNotExist(err) {
		t.Fatalf("declined upgrade wrote a journal: %v", err)
	}
	if f.fake.HasCall("stop", systemd.Hub.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
	if !strings.Contains(f.err.String(), "will stop for the upgrade to 1.1.0") || !strings.Contains(f.err.String(), "cancelled") {
		t.Fatalf("stderr = %s", f.err.String())
	}
}

// Commands that act on an existing installation find a system install without
// --system when there is no per-user one; a per-user install still wins.
func TestCommandsFindSystemInstallationWithoutFlag(t *testing.T) {
	f := newFixture(t)
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
	if code := f.run(t, "install", "--system", "--role", "both", "--no-start"); code != 0 {
		t.Fatalf("system install exit %d: %s", code, f.err.String())
	}
	f.deps.Euid = func() int { return 1000 }

	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "status", "--json"); code != 0 {
		t.Fatalf("status exit %d: %s", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), `"scope": "system"`) || !strings.Contains(f.err.String(), "using the system installation") {
		t.Fatalf("status did not pick the system installation:\nstdout=%s\nstderr=%s", f.out.String(), f.err.String())
	}

	f.err.Reset()
	if code := f.run(t, "configure", "--port", "9191"); code != 1 {
		t.Fatalf("non-root configure exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "the system installation requires root; run again with sudo") {
		t.Fatalf("stderr = %s", f.err.String())
	}

	// Re-running install without --system acts on the system install too;
	// it never starts a second, per-user hub beside it.
	f.err.Reset()
	f.deps.Elevate = func(args []string) (int, error) {
		if fmt.Sprint(args[:2]) != "[install --system]" {
			t.Fatalf("elevated %q", args)
		}
		return 0, nil
	}
	if code := f.run(t, "install", "--gpu-registers"); code != 0 {
		t.Fatalf("reinstall exit %d: %s", code, f.err.String())
	}
	if fileExists(f.paths(t, config.ScopeUser).ManifestFile()) {
		t.Fatal("install created a per-user installation beside the system one")
	}

	// With both present (the per-user one made first), the per-user one wins.
	f = newFixture(t)
	f.install(t, "--no-start")
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
	if code := f.run(t, "install", "--system", "--role", "both", "--no-start"); code != 0 {
		t.Fatalf("system install exit %d: %s", code, f.err.String())
	}
	f.deps.Euid = func() int { return 1000 }
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "status", "--json"); code != 0 {
		t.Fatalf("status exit %d: %s", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), `"scope": "user"`) || strings.Contains(f.err.String(), "using the system installation") {
		t.Fatalf("a per-user installation must win:\nstdout=%s\nstderr=%s", f.out.String(), f.err.String())
	}
}

// A system collector runs under systemd without $HOME when its account's home
// is "/"; system paths must resolve anyway.
func TestSystemCollectorRunsWithoutHome(t *testing.T) {
	f := newFixture(t)
	f.deps.HomeDir = func() (string, error) { return "", errors.New("$HOME is not defined") }
	configPath, stateDir, err := collectorPaths(f.deps, "/etc/rigscope/config.json", "", false)
	if err != nil {
		t.Fatalf("system collector paths without HOME: %v", err)
	}
	want := f.paths(t, config.ScopeSystem).StateDir
	if configPath != "/etc/rigscope/config.json" || stateDir != want {
		t.Fatalf("paths = %s, %s; want state %s", configPath, stateDir, want)
	}
	if _, _, err := hubPaths(f.deps, "/etc/rigscope/config.json", "", false); err != nil {
		t.Fatalf("system hub paths without HOME: %v", err)
	}
	if _, err := f.deps.paths(config.ScopeUser); err == nil {
		t.Fatal("user paths must still require a home directory")
	}
}

// Re-running install with nothing new leaves running services alone: no
// prompt, no stop and no restart.
func TestIdenticalReinstallKeepsRunningServices(t *testing.T) {
	f := newFixture(t)
	f.install(t)
	f.fake.Reset()
	f.prompts = nil
	f.out.Reset()
	f.install(t)
	for _, method := range []string{"stop", "restart", "start"} {
		for _, component := range installComponents {
			if f.fake.HasCall(method, component.UnitName()) {
				t.Fatalf("identical reinstall called %s: %v", method, f.fake.Methods())
			}
		}
	}
	if len(f.prompts) != 0 || !strings.Contains(f.out.String(), "running: hub, collector (unchanged)") {
		t.Fatalf("prompts = %v\nstdout = %s", f.prompts, f.out.String())
	}
}

// A system command run as a regular user re-runs itself through sudo with
// --system after the command words, and reports that run's exit status.
func TestSystemCommandsAskSudo(t *testing.T) {
	f := newFixture(t)
	var elevated [][]string
	f.deps.Elevate = func(args []string) (int, error) {
		elevated = append(elevated, args)
		return 7, nil
	}
	if code := f.run(t, "install", "--system", "--port", "9000"); code != 7 {
		t.Fatalf("exit = %d, want the elevated run's status", code)
	}
	if code := f.run(t, "start", "--system", "hub"); code != 7 {
		t.Fatalf("exit = %d", code)
	}
	want := [][]string{
		{"install", "--system", "--system", "--port", "9000"},
		{"start", "--system", "--system", "hub"},
	}
	if fmt.Sprint(elevated) != fmt.Sprint(want) {
		t.Fatalf("elevated = %q, want %q", elevated, want)
	}
	if !strings.Contains(f.err.String(), "asking sudo") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	if fmt.Sprint(withSystemFlag([]string{"collector", "enroll", "--grant", "x"})) != "[collector enroll --system --grant x]" ||
		fmt.Sprint(withSystemFlag([]string{"cleanup-legacy-hardware", "--apply"})) != "[cleanup-legacy-hardware --apply]" {
		t.Fatal("withSystemFlag placed --system wrongly")
	}

	// Without sudo or a terminal, the command explains instead.
	f.err.Reset()
	f.deps.Elevate = func([]string) (int, error) { return 0, errNoElevation }
	if code := f.run(t, "install", "--system"); code != 1 || !strings.Contains(f.err.String(), "run again with sudo") {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
}

// collector enroll restarts a running collector so it uses the new
// credential; a stopped one is left alone.
func TestEnrollRestartsARunningCollector(t *testing.T) {
	f := newFixture(t)
	f.fake.Active[systemd.Collector.UnitName()] = true
	if code := restartCollectorForCredential(context.Background(), f.deps, false, false); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if !f.fake.HasCall("restart", systemd.Collector.UnitName()) || len(f.prompts) != 1 {
		t.Fatalf("calls = %v, prompts = %v", f.fake.Methods(), f.prompts)
	}
	g := newFixture(t)
	g.fake.ActiveByDefault = false
	if code := restartCollectorForCredential(context.Background(), g.deps, false, false); code != 0 || len(g.fake.Methods()) > 1 || len(g.prompts) != 0 {
		t.Fatalf("stopped collector: calls = %v, prompts = %v", g.fake.Methods(), g.prompts)
	}
}

// collector enroll without --grant and with nothing staged asks for the grant
// on the terminal; without a terminal it explains how to pass one.
func TestCollectorEnrollAsksForTheGrant(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "collector")
	var prompts []string
	f.deps.ReadPassword = typedPasswords(&prompts, "pasted-grant")
	args := []string{"collector", "enroll", "--config", paths.ConfigFile, "--state-dir", paths.StateDir, "--hub-url", "http://127.0.0.1:1"}
	if code := f.run(t, args...); code == 0 {
		t.Fatal("enrollment against an unreachable hub must fail")
	}
	if len(prompts) != 1 || strings.Contains(f.err.String(), "no enrollment grant") {
		t.Fatalf("prompts = %q, stderr = %s", prompts, f.err.String())
	}

	// The failed attempt kept the pasted grant, so a retry needs no paste.
	f.err.Reset()
	prompts = nil
	if code := f.run(t, args...); code == 0 || len(prompts) != 0 {
		t.Fatalf("retry asked again: exit %d, prompts %q", code, prompts)
	}

	g := newFixture(t)
	paths = g.install(t, "--role", "collector")
	args = []string{"collector", "enroll", "--config", paths.ConfigFile, "--state-dir", paths.StateDir, "--hub-url", "http://127.0.0.1:1"}
	if code := g.run(t, args...); code != 1 || !strings.Contains(g.err.String(), "no enrollment grant") {
		t.Fatalf("without a terminal: exit %d: %s", code, g.err.String())
	}
}

// The owner password is asked before anything is written, so cancelling at
// the prompt leaves no partial installation.
func TestCancelledPasswordPromptWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.deps.ReadPassword = func(string) (string, error) { return "", errors.New("interrupted") }
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	paths := f.paths(t, config.ScopeUser)
	for _, path := range []string{paths.ManifestFile(), paths.StateDir, paths.ConfigFile} {
		if fileExists(path) {
			t.Fatalf("cancelled install left %s", path)
		}
	}
}

// Re-running install without --role keeps the installed components.
func TestReinstallWithoutRoleKeepsComponents(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "collector")
	f.install(t)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Components.Hub.Enabled || !cfg.Components.Collector.Enabled {
		t.Fatalf("components = hub %v, collector %v", cfg.Components.Hub.Enabled, cfg.Components.Collector.Enabled)
	}
}
