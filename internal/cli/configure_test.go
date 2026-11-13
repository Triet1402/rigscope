package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// resetCalls clears recorded systemd calls, prompts and output between steps.
func (f *fixture) resetCalls() {
	f.fake.Reset()
	f.prompts = nil
	f.out.Reset()
	f.err.Reset()
}

// assertNoMutation proves a refused or declined configure left the file and
// every service alone.
func assertNoMutation(t *testing.T, f *fixture, paths config.Paths, before []byte) {
	t.Helper()
	after, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("configuration changed:\n%s", after)
	}
	for _, component := range installComponents {
		for _, method := range []string{"stop", "start", "restart", "enable", "disable"} {
			if f.fake.HasCall(method, component.UnitName()) {
				t.Fatalf("unexpected %s %s: %v", method, component, f.fake.Methods())
			}
		}
	}
}

func readConfigBytes(t *testing.T, paths config.Paths) []byte {
	t.Helper()
	data, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestConfigureShareSensitiveFamilyAsksOnceAndRestartsOnlyTheCollector(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	before := loadConfig(t, paths)
	f.resetCalls()
	if code := f.run(t, "configure", "--share", "process-arguments=on", "--share", "disks=off"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if len(f.prompts) != 1 {
		t.Fatalf("prompts = %v, want exactly one", f.prompts)
	}
	if !strings.Contains(f.err.String(), "command lines can contain passwords") {
		t.Fatalf("missing privacy warning: %s", f.err.String())
	}
	after := loadConfig(t, paths)
	want := before.Components.Collector.Sharing
	want.ProcessArguments, want.Disks = true, false
	if after.Components.Collector.Sharing != want {
		t.Fatalf("sharing = %+v, want %+v", after.Components.Collector.Sharing, want)
	}
	if !reflect.DeepEqual(after.Components.Hub, before.Components.Hub) {
		t.Fatalf("hub settings changed: %+v", after.Components.Hub)
	}
	if !f.fake.HasCall("restart", systemd.Collector.UnitName()) {
		t.Fatalf("collector not restarted: %v", f.fake.Methods())
	}
	for _, method := range []string{"restart", "stop", "start"} {
		if f.fake.HasCall(method, systemd.Hub.UnitName()) {
			t.Fatalf("hub was interrupted for a collector setting: %v", f.fake.Methods())
		}
	}
	for _, want := range []string{"share process-arguments (full process command lines): off -> on", "share disks", "running: hub (unchanged)"} {
		if !strings.Contains(f.out.String(), want) {
			t.Fatalf("stdout lacks %q:\n%s", want, f.out.String())
		}
	}
}

func TestConfigureSensitiveOptInNeedsConsentEvenWithoutARestart(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	f.fake.Active[systemd.Collector.UnitName()] = false
	original := readConfigBytes(t, paths)

	f.resetCalls()
	f.answer = false
	if code := f.run(t, "configure", "--share", "network-addresses=on"); code != 1 {
		t.Fatalf("declined exit %d", code)
	}
	if len(f.prompts) != 1 {
		t.Fatalf("prompts = %v", f.prompts)
	}
	assertNoMutation(t, f, paths, original)

	f.resetCalls()
	f.deps.Confirm = func(string) (bool, error) { return false, errNoTerminal }
	if code := f.run(t, "configure", "--share", "network-addresses=on"); code != 1 || !strings.Contains(f.err.String(), "--yes") {
		t.Fatalf("no-terminal exit %d: %s", code, f.err.String())
	}
	assertNoMutation(t, f, paths, original)

	// Turning processes back on while process-arguments is already on starts
	// sending command lines, so it is a sensitive opt-in too.
	effective := config.DefaultSharing()
	effective.Processes, effective.ProcessArguments = false, true
	resumed := effective
	resumed.Processes = true
	if got := sensitiveOptIns(effective, resumed); len(got) != 1 || got[0].name != "process-arguments" {
		t.Fatalf("effective opt-ins = %+v", got)
	}
	if got := sensitiveOptIns(config.DefaultSharing(), effective); len(got) != 0 {
		t.Fatalf("an ineffective switch counted as sharing: %+v", got)
	}

	// A non-sensitive change to a stopped collector interrupts nothing and
	// needs no answer; the stopped collector stays stopped.
	f.resetCalls()
	if code := f.run(t, "configure", "--share", "swap=off"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if loadConfig(t, paths).Components.Collector.Sharing.Swap {
		t.Fatal("swap still shared")
	}
	if f.fake.HasCall("start", systemd.Collector.UnitName()) || f.fake.HasCall("restart", systemd.Collector.UnitName()) {
		t.Fatalf("stopped collector was started: %v", f.fake.Methods())
	}
	if !strings.Contains(f.out.String(), "not running: collector") {
		t.Fatalf("stdout = %s", f.out.String())
	}
}

func TestConfigureShareRejectsBadInputAndIdenticalValues(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	original := readConfigBytes(t, paths)
	for _, args := range [][]string{
		{"--share", "secrets=on"},
		{"--share", "cpu"},
		{"--share", "cpu=maybe"},
		{"--share", "cpu=on", "--share", "cpu=off"},
	} {
		f.resetCalls()
		if code := f.run(t, append([]string{"configure"}, args...)...); code == 0 {
			t.Fatalf("%v accepted", args)
		}
		assertNoMutation(t, f, paths, original)
	}
	f.resetCalls()
	if code := f.run(t, "configure", "--share", "cpu=on,process-arguments=off"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if len(f.fake.Calls) != 0 || len(f.prompts) != 0 || !strings.Contains(f.out.String(), "Nothing to change") {
		t.Fatalf("identical values interrupted services: %v %v %s", f.fake.Methods(), f.prompts, f.out.String())
	}
}

func TestConfigureSamplingIntervalNoRestartAndValidation(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	original := readConfigBytes(t, paths)
	for _, value := range []string{"50ms", "2h", "fast"} {
		f.resetCalls()
		if code := f.run(t, "configure", "--sampling-interval", value); code != 1 {
			t.Fatalf("%s accepted", value)
		}
		assertNoMutation(t, f, paths, original)
	}
	f.resetCalls()
	if code := f.run(t, "configure", "--sampling-interval", "500ms", "--no-restart", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if got := loadConfig(t, paths).Components.Collector.SamplingInterval.Value(); got != 500*time.Millisecond {
		t.Fatalf("interval = %s", got)
	}
	if !f.fake.HasCall("stop", systemd.Collector.UnitName()) || f.fake.HasCall("restart", systemd.Collector.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
	if f.fake.HasCall("stop", systemd.Hub.UnitName()) {
		t.Fatalf("hub stopped for a collector setting: %v", f.fake.Methods())
	}
	if !strings.Contains(f.out.String(), "more CPU") {
		t.Fatalf("missing cost note: %s", f.out.String())
	}
}

func TestConfigureCollectorOnlyInstallation(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "collector")
	original := readConfigBytes(t, paths)
	f.resetCalls()
	if code := f.run(t, "configure", "--port", "9000"); code != 1 || !strings.Contains(f.err.String(), "--hub on") {
		t.Fatalf("hub setting on a collector-only install: exit %d: %s", code, f.err.String())
	}
	assertNoMutation(t, f, paths, original)

	f.resetCalls()
	if code := f.run(t, "configure", "--sampling-interval", "5s", "--share", "processes=off", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	cfg := loadConfig(t, paths)
	if cfg.Components.Collector.SamplingInterval.Value() != 5*time.Second || cfg.Components.Collector.Sharing.Processes || cfg.Components.Hub.Enabled {
		t.Fatalf("config = %+v", cfg.Components)
	}
	if !f.fake.HasCall("restart", systemd.Collector.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
}

func TestConfigureTurnsComponentsOffAndOnKeepingData(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)

	f.resetCalls()
	if code := f.run(t, "configure", "--collector", "off", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if loadConfig(t, paths).Components.Collector.Enabled {
		t.Fatal("collector still enabled")
	}
	for _, method := range []string{"stop", "disable"} {
		if !f.fake.HasCall(method, systemd.Collector.UnitName()) {
			t.Fatalf("collector not %sd: %v", method, f.fake.Methods())
		}
	}
	if f.fake.HasCall("restart", systemd.Hub.UnitName()) || f.fake.HasCall("stop", systemd.Hub.UnitName()) {
		t.Fatalf("hub interrupted: %v", f.fake.Methods())
	}
	if _, err := os.Stat(paths.CollectorDir()); err != nil {
		t.Fatalf("collector state removed: %v", err)
	}
	if !strings.Contains(f.out.String(), "turned off: collector") {
		t.Fatalf("stdout = %s", f.out.String())
	}

	// Both components off is refused.
	original := readConfigBytes(t, paths)
	f.resetCalls()
	if code := f.run(t, "configure", "--hub", "off", "--yes"); code != 1 || !strings.Contains(f.err.String(), "uninstall") {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	assertNoMutation(t, f, paths, original)

	// Collector settings while it is off are refused with the fix.
	if code := f.run(t, "configure", "--sampling-interval", "5s"); code != 1 || !strings.Contains(f.err.String(), "--collector on") {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}

	f.fake.Active[systemd.Collector.UnitName()] = false
	f.resetCalls()
	if code := f.run(t, "configure", "--collector", "on", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if !loadConfig(t, paths).Components.Collector.Enabled || !f.fake.HasCall("enable", systemd.Collector.UnitName()) || !f.fake.HasCall("start", systemd.Collector.UnitName()) {
		t.Fatalf("collector not turned on: %v", f.fake.Methods())
	}
}

func TestConfigureHubOffLeavesTheCollectorRunning(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	f.resetCalls()
	if code := f.run(t, "configure", "--hub", "off", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if loadConfig(t, paths).Components.Hub.Enabled {
		t.Fatal("hub still enabled")
	}
	if !f.fake.HasCall("stop", systemd.Hub.UnitName()) || !f.fake.HasCall("disable", systemd.Hub.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
	if f.fake.HasCall("restart", systemd.Collector.UnitName()) || f.fake.HasCall("stop", systemd.Collector.UnitName()) {
		t.Fatalf("collector interrupted: %v", f.fake.Methods())
	}
	if !strings.Contains(f.out.String(), "keeps retrying") {
		t.Fatalf("missing local collector note: %s", f.out.String())
	}
	if _, err := os.Stat(paths.ComponentStateDir(systemd.Hub.String())); err != nil {
		t.Fatalf("hub data removed: %v", err)
	}
}

func TestConfigureHubOnCreatesTheOwnerAndPairs(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "collector")
	f.fake.Active[systemd.Hub.UnitName()] = false
	f.resetCalls()
	if code := f.run(t, "configure", "--hub", "on", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	cfg := loadConfig(t, paths)
	if !cfg.Components.Hub.Enabled || len(cfg.Components.Collector.Destinations) != 1 {
		t.Fatalf("config = %+v", cfg.Components)
	}
	if !f.fake.HasCall("start", systemd.Hub.UnitName()) || !f.fake.HasCall("restart", systemd.Collector.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
	if !strings.Contains(f.out.String(), "sign in: owner / ") {
		t.Fatalf("no owner sign-in: %s", f.out.String())
	}
	if _, err := os.Stat(ownerBootstrapPath(paths)); err != nil {
		t.Fatalf("owner bootstrap missing: %v", err)
	}
}

func TestConfigureGPURegistersOffKeepsProvisioning(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	if code := f.run(t, "configure", "--gpu-registers", "on"); code != 1 || !strings.Contains(f.err.String(), "install --gpu-registers") {
		t.Fatalf("on: exit %d: %s", code, f.err.String())
	}
	cfg := loadConfig(t, paths)
	cfg.Components.Collector.Hardware.GPURegisters = true
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(paths.ConfigFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f.resetCalls()
	if code := f.run(t, "configure", "--gpu-registers", "off", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if loadConfig(t, paths).Components.Collector.Hardware.GPURegisters {
		t.Fatal("still on")
	}
	if !strings.Contains(f.out.String(), "stay installed") {
		t.Fatalf("stdout = %s", f.out.String())
	}

	// A provisioned system rule stays in the plan while the collector is on.
	system := f.paths(t, config.ScopeSystem)
	recorded := buildManifest(system, "1.0.0", hardwareSelection{RAPL: true, GPURegisters: true})
	recorded.entry(system.UdevGPUTempsFile, kindUdevGPU).State = manifestOwned
	off := config.Default()
	if got := plannedHardware(system, off, recorded); !got.GPURegisters || !got.RAPL {
		t.Fatalf("system plan with sampling off = %+v", got)
	}
	if got := plannedHardware(system, off, nil); got.GPURegisters {
		t.Fatalf("unprovisioned plan = %+v", got)
	}
	// Turning the collector off keeps provisioned GPU access until uninstall.
	off.Components.Collector.Enabled = false
	if got := plannedHardware(system, off, recorded); !got.GPURegisters || got.RAPL {
		t.Fatalf("system plan with the collector off = %+v", got)
	}
}

func TestConfigureHubOnLeavesARemoteCollectorRunning(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "collector", "--hub", "https://hub.example.com", "--token", "grant-token")
	f.fake.Active[systemd.Hub.UnitName()] = false
	f.resetCalls()
	if code := f.run(t, "configure", "--hub", "on", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	cfg := loadConfig(t, paths)
	if got := cfg.Components.Collector.Destinations[0].URL; !strings.Contains(got, "hub.example.com") {
		t.Fatalf("remote destination rewritten: %s", got)
	}
	if !f.fake.HasCall("start", systemd.Hub.UnitName()) {
		t.Fatalf("hub not started: %v", f.fake.Methods())
	}
	if f.fake.HasCall("restart", systemd.Collector.UnitName()) || f.fake.HasCall("stop", systemd.Collector.UnitName()) {
		t.Fatalf("remote collector interrupted: %v", f.fake.Methods())
	}
}

func TestConfigureExpiredExistingCertificateDoesNotBlockUnrelatedChanges(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "hub.crt"), filepath.Join(dir, "hub.key")
	certPEM, keyPEM := testCertificatePEM(t, []string{"127.0.0.1"}, []string{"localhost"})
	for path, data := range map[string][]byte{cert: certPEM, key: keyPEM} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code := f.run(t, "configure", "--tls-cert", cert, "--tls-key", key, "--yes"); code != 0 {
		t.Fatalf("enable TLS: exit %d: %s", code, f.err.String())
	}
	oldPEM, oldKeyPEM := expiredCertificatePEM(t)
	for path, data := range map[string][]byte{cert: oldPEM, key: oldKeyPEM} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.resetCalls()
	if code := f.run(t, "configure", "--telemetry-mode", "persistent", "--yes"); code != 0 {
		t.Fatalf("unrelated change refused: %s", f.err.String())
	}
	if !strings.Contains(f.out.String(), "expired on") {
		t.Fatalf("missing expiry note: %s", f.out.String())
	}
	// A broken pair still stops the change: the hub could not restart.
	if err := os.WriteFile(key, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	original := readConfigBytes(t, paths)
	f.resetCalls()
	if code := f.run(t, "configure", "--telemetry-mode", "memory", "--yes"); code != 1 || !strings.Contains(f.err.String(), "do not load as a pair") {
		t.Fatalf("broken pair: exit %d: %s", code, f.err.String())
	}
	assertNoMutation(t, f, paths, original)
}

func TestConfigureListenAndTLSValidation(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	original := readConfigBytes(t, paths)
	dir := t.TempDir()
	certPEM, keyPEM := testCertificatePEM(t, nil, []string{"hub.example.com"})
	cert, key := filepath.Join(dir, "hub.crt"), filepath.Join(dir, "hub.key")
	otherCert := filepath.Join(dir, "other.crt")
	expiredCert, expiredKey := filepath.Join(dir, "expired.crt"), filepath.Join(dir, "expired.key")
	otherPEM, _ := testCertificatePEM(t, nil, []string{"hub.example.com"})
	oldPEM, oldKeyPEM := expiredCertificatePEM(t)
	futureCert, futureKey := filepath.Join(dir, "future.crt"), filepath.Join(dir, "future.key")
	futurePEM, futureKeyPEM := datedCertificatePEM(t, time.Now().Add(time.Hour), time.Now().Add(48*time.Hour))
	for path, data := range map[string][]byte{futureCert: futurePEM, futureKey: futureKeyPEM, cert: certPEM, key: keyPEM, otherCert: otherPEM, expiredCert: oldPEM, expiredKey: oldKeyPEM} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	refused := []struct {
		args []string
		want string
	}{
		{[]string{"--listen", "hub.lan"}, "not an IP address"},
		{[]string{"--listen", "0.0.0.0"}, "--origin"},
		{[]string{"--listen", "0.0.0.0", "--origin", "https://hub.example.com"}, "refuses to serve plain HTTP"},
		{[]string{"--tls-cert", cert}, "together"},
		{[]string{"--tls-cert", cert, "--tls-key", key, "--no-tls"}, "not both"},
		{[]string{"--tls-cert", otherCert, "--tls-key", key}, "do not load as a pair"},
		{[]string{"--tls-cert", expiredCert, "--tls-key", expiredKey}, "expired"},
		{[]string{"--tls-cert", futureCert, "--tls-key", futureKey}, "not valid until"},
		{[]string{"--tls-cert", cert, "--tls-key", key, "--origin", "https://wrong.example.com"}, "does not cover wrong.example.com"},
		{[]string{"--tls-cert", filepath.Join(dir, "missing.crt"), "--tls-key", key}, "missing.crt"},
		{[]string{"--tls-cert", cert, "--tls-key", key, "--origin", "http://hub.example.com"}, "https://"},
		{[]string{"--forwarded-headers", "both"}, "x-forwarded"},
	}
	for _, test := range refused {
		f.resetCalls()
		if code := f.run(t, append([]string{"configure", "--yes"}, test.args...)...); code != 1 || !strings.Contains(f.err.String(), test.want) {
			t.Fatalf("%v: exit %d, stderr %s", test.args, code, f.err.String())
		}
		assertNoMutation(t, f, paths, original)
	}

	f.resetCalls()
	// A relative path is stored as an absolute one.
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "configure", "--listen", "0.0.0.0", "--origin", "https://hub.example.com", "--tls-cert", "hub.crt", "--tls-key", key, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	hub := loadConfig(t, paths).Components.Hub
	if hub.Listen != "0.0.0.0" || hub.TLSCert != cert || hub.TLSKey != key || hub.Origin != "https://hub.example.com" {
		t.Fatalf("hub = %+v", hub)
	}
	for _, want := range []string{"hub HTTPS: off -> certificate " + cert, "after each renewal run rigscope restart hub", "firewall"} {
		if !strings.Contains(f.out.String(), want) {
			t.Fatalf("stdout lacks %q:\n%s", want, f.out.String())
		}
	}

	// Removing TLS from an exposed hub is refused; going local again works.
	exposed := readConfigBytes(t, paths)
	f.resetCalls()
	if code := f.run(t, "configure", "--no-tls", "--yes"); code != 1 || !strings.Contains(f.err.String(), "plain HTTP") {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	assertNoMutation(t, f, paths, exposed)
	f.resetCalls()
	if code := f.run(t, "configure", "--no-tls", "--listen", "127.0.0.1", "--local", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	hub = loadConfig(t, paths).Components.Hub
	if hub.TLSCert != "" || hub.TLSKey != "" || hub.Listen != "127.0.0.1" || hub.Origin != "" {
		t.Fatalf("hub = %+v", hub)
	}
}

func TestConfigureForwardedHeadersRestartsOnlyTheHub(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	f.resetCalls()
	if code := f.run(t, "configure", "--forwarded-headers", "forwarded", "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if loadConfig(t, paths).Components.Hub.ForwardedHeaders != "forwarded" {
		t.Fatal("not saved")
	}
	if !f.fake.HasCall("restart", systemd.Hub.UnitName()) || f.fake.HasCall("restart", systemd.Collector.UnitName()) {
		t.Fatalf("calls = %v", f.fake.Methods())
	}
}

func TestConfigureShowIsReadableAndSecretFree(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	// Plant recognisable secrets where the collector keeps them.
	const secret = "grant-SECRET-token-value"
	if err := os.WriteFile(paths.EnrollmentFile(), []byte(`{"token":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readConfigBytes(t, paths)
	f.resetCalls()
	if code := f.run(t, "configure", "--show"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	for _, want := range []string{"Components", "history storage", "memory (history is lost", "sampling interval", "keeps private", "network-addresses, process-arguments", "--share FAMILY=on|off"} {
		if !strings.Contains(f.out.String(), want) {
			t.Fatalf("show lacks %q:\n%s", want, f.out.String())
		}
	}
	f.resetCalls()
	if code := f.run(t, "configure", "--show", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if strings.Contains(f.out.String(), secret) || strings.Contains(strings.ToLower(f.out.String()), "token") {
		t.Fatalf("show leaked secret material: %s", f.out.String())
	}
	var view settingsView
	if err := json.Unmarshal(f.out.Bytes(), &view); err != nil {
		t.Fatalf("json: %v\n%s", err, f.out.String())
	}
	if view.Scope != "user" || !view.Components.Hub.Enabled || view.Components.Hub.Service != "active" ||
		view.Hub.TelemetryMode != "memory" || view.Collector.SamplingInterval != "2s" ||
		view.Collector.Sharing["process-arguments"] || !view.Collector.Sharing["cpu"] || len(view.Collector.Sharing) != len(shareFamilies) {
		t.Fatalf("view = %+v", view)
	}
	assertNoMutation(t, f, paths, before)

	for _, args := range [][]string{{"--json"}, {"--show", "--port", "9000"}} {
		f.resetCalls()
		if code := f.run(t, append([]string{"configure"}, args...)...); code != 1 {
			t.Fatalf("%v accepted", args)
		}
	}
}

func TestServiceCanRead(t *testing.T) {
	root := t.TempDir()
	// The test's temporary tree is private by default; open it like a
	// system directory so only the cases below decide access.
	for _, dir := range []string{filepath.Dir(root), root} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "tls")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "hub.key")
	if err := os.WriteFile(file, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	accounts := fakeAccounts{
		passwd: []UserAccount{{Name: systemd.HubUser, UID: 64001, GID: 64001}},
		groups: []GroupRecord{{Name: systemd.SharedGroup, GID: os.Getgid()}},
	}
	deps := Deps{Accounts: accounts}
	if _, err := deps.serviceCanRead(file, systemd.HubUser, nil); err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("0600 key readable by another account: %v", err)
	}
	// Group-readable through the shared group set by the unit.
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	if note, err := deps.serviceCanRead(file, systemd.HubUser, nil); err != nil || note != "" {
		t.Fatalf("group-readable key: %q, %v", note, err)
	}
	// A directory the account cannot enter blocks the file.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.serviceCanRead(file, systemd.HubUser, nil); err == nil || !strings.Contains(err.Error(), "cannot enter") {
		t.Fatalf("blocked directory: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Trees the unit hides are refused, through a symlink too.
	link := filepath.Join(t.TempDir(), "link.key")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.serviceCanRead(link, systemd.HubUser, []string{root}); err == nil || !strings.Contains(err.Error(), "cannot see") {
		t.Fatalf("hidden tree: %v", err)
	}
	// A missing account is a repair hint, not a pass.
	if _, err := (Deps{Accounts: fakeAccounts{}}).serviceCanRead(file, systemd.HubUser, nil); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing account: %v", err)
	}
}

func expiredCertificatePEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	return datedCertificatePEM(t, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
}

func datedCertificatePEM(t *testing.T, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "expired"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     []string{"hub.example.com"},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}
