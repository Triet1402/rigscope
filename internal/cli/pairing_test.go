package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/enroll"
	"github.com/Triet1402/rigscope/internal/collector/state"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/ingest"
	"github.com/Triet1402/rigscope/internal/hub/server"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

type pairingFileValue struct {
	Grants []pairingBootstrapGrant `json:"grants"`
}

func readPairingFile(t *testing.T, path string) pairingFileValue {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pairing file: %v", err)
	}
	var value pairingFileValue
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("parse pairing file: %v\n%s", err, data)
	}
	return value
}

func readInitialGrant(t *testing.T, path string) state.PendingEnrollment {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read enrollment file: %v", err)
	}
	var value struct {
		Enrollments []state.PendingEnrollment `json:"enrollments"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("parse enrollment file: %v\n%s", err, data)
	}
	if len(value.Enrollments) != 1 {
		t.Fatalf("enrollments = %+v, want exactly one", value.Enrollments)
	}
	return value.Enrollments[0]
}

func TestInstallAutoPairsTheLocalCollector(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)

	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	destinations := cfg.Components.Collector.Destinations
	if len(destinations) != 1 || destinations[0].URL != "ws://127.0.0.1:8081/api/collector/connect" || destinations[0].CAFile != "" {
		t.Fatalf("destinations = %+v", destinations)
	}

	pending := readInitialGrant(t, paths.EnrollmentFile())
	if pending.URL != destinations[0].URL || pending.Grant == "" {
		t.Fatalf("pending enrollment = %+v", pending)
	}
	info, err := os.Stat(paths.EnrollmentFile())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("enrollment file mode = %v, err = %v", info.Mode().Perm(), err)
	}

	pairingPath := filepath.Join(paths.ComponentStateDir(systemd.Hub.String()), pairingBootstrapFileName)
	pairing := readPairingFile(t, pairingPath)
	if len(pairing.Grants) != 1 {
		t.Fatalf("pairing grants = %+v", pairing.Grants)
	}
	entry := pairing.Grants[0]
	if entry.TokenHash != pairingGrantHash(pending.Grant) {
		t.Fatalf("pairing hash does not match the collector grant")
	}
	if entry.Use != storage.BootstrapGrantUseColocatedPairing {
		t.Fatalf("pairing use = %q", entry.Use)
	}
	store, err := state.Open(paths.StateDir, state.Options{})
	if err != nil {
		t.Fatalf("open collector state: %v", err)
	}
	identity, err := store.EnsureIdentity(nil)
	if err != nil {
		t.Fatalf("read collector identity: %v", err)
	}
	if entry.CollectorID == "" || entry.CollectorID != identity.CollectorID {
		t.Fatalf("pairing collector id = %q, installation identity = %q", entry.CollectorID, identity.CollectorID)
	}
	expires, err := time.Parse(time.RFC3339, entry.ExpiresAt)
	if err != nil {
		t.Fatalf("pairing expiry: %v", err)
	}
	remaining := time.Until(expires)
	if remaining <= 0 || remaining > autoPairGrantTTL {
		t.Fatalf("pairing grant lifetime = %s, want (0, %s]", remaining, autoPairGrantTTL)
	}
	raw, err := os.ReadFile(pairingPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), pending.Grant) {
		t.Fatalf("pairing file contains the plaintext grant:\n%s", raw)
	}
	info, err = os.Stat(pairingPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pairing file mode = %v, err = %v", info.Mode().Perm(), err)
	}
}

func TestReinstallDoesNotReplaceAnEnrolledCollectorGrant(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)

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
	pairingPath := filepath.Join(paths.ComponentStateDir(systemd.Hub.String()), pairingBootstrapFileName)
	if err := os.Remove(pairingPath); err != nil {
		t.Fatal(err)
	}
	credentialsBefore, err := os.ReadFile(paths.CredentialsFile())
	if err != nil {
		t.Fatal(err)
	}

	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install"); code != 0 {
		t.Fatalf("second install exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.EnrollmentFile()); !os.IsNotExist(err) {
		t.Fatalf("reinstall recreated enrollment.json: %v", err)
	}
	if _, err := os.Stat(pairingPath); !os.IsNotExist(err) {
		t.Fatalf("reinstall recreated the pairing bootstrap: %v", err)
	}
	credentialsAfter, err := os.ReadFile(paths.CredentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(credentialsBefore) != string(credentialsAfter) {
		t.Fatalf("reinstall changed the issued credential")
	}
}

func TestEnablingTheCollectorLaterRestartsAnActiveCollector(t *testing.T) {
	f := newFixture(t)
	f.install(t, "--role", "hub")
	f.fake.Reset()
	f.out.Reset()
	f.err.Reset()
	f.prompts = nil
	if code := f.run(t, "install", "--role", "both"); code != 0 {
		t.Fatalf("enabling collector exit %d: %s", code, f.err.String())
	}
	// The collector's settings change and it reads its first destination only
	// at startup, so the active collector restarts. The hub's settings and
	// binary do not change: it imports the pairing grant on the collector's
	// first enrollment request, so it keeps running.
	if !f.fake.HasCall("restart", systemd.Collector.UnitName()) {
		t.Fatalf("active collector was not restarted: %v", f.fake.Methods())
	}
	for _, method := range []string{"restart", "start", "stop"} {
		if f.fake.HasCall(method, systemd.Hub.UnitName()) {
			t.Fatalf("unchanged hub got %s: %v", method, f.fake.Methods())
		}
	}
	if len(f.prompts) != 1 {
		t.Fatalf("prompts = %v, want one confirmation", f.prompts)
	}
	if out := f.out.String(); !strings.Contains(out, "restarted: collector") || !strings.Contains(out, "running: hub (unchanged)") {
		t.Fatalf("install output = %s", out)
	}
}

func TestPairingImporterImportsAGrantAddedAfterStartup(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Startup sees no pairing file: this models a hub that was already running
	// when `install --role both` added the collector.
	registry, err := openRegistryWithBootstrap(ctx, db, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	token, hash, err := storage.NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, pairingBootstrapFileName)
	writePairingFile(t, path, 0o600, pairingBootstrapGrant{
		TokenHash:   hash,
		ExpiresAt:   time.Now().Add(autoPairGrantTTL).Format(time.RFC3339),
		Use:         storage.BootstrapGrantUseColocatedPairing,
		CollectorID: "late-collector",
	})
	importer := newPairingImporter(stateDir, registry)
	if err := importer.importPending(ctx); err != nil {
		t.Fatalf("lazy import: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lazily imported bootstrap survived: %v", err)
	}
	// A second request with no file is a no-op.
	if err := importer.importPending(ctx); err != nil {
		t.Fatalf("second lazy import: %v", err)
	}
	if _, err := registry.Enroll(ctx, storage.EnrollRequest{CollectorID: "late-collector", Token: token, LocalPeer: true}); err != nil {
		t.Fatalf("enroll with lazily imported grant: %v", err)
	}
}

func TestInterruptedPairingIsRepairedByRerunningInstall(t *testing.T) {
	f := newFixture(t)
	paths := f.paths(t, config.ScopeUser)
	f.deps.PublishStep = func(path, phase string) error {
		if path == paths.EnrollmentFile() && phase == "staged" {
			return errSimulatedCrash
		}
		return nil
	}
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install", "--no-start"); code == 0 {
		t.Fatal("install succeeded despite the simulated crash between pairing writes")
	}
	if _, err := os.Stat(pairingBootstrapPath(paths)); err != nil {
		t.Fatalf("hub bootstrap was not staged before the crash: %v", err)
	}
	if _, err := os.Stat(paths.EnrollmentFile()); !os.IsNotExist(err) {
		t.Fatalf("collector grant exists after the simulated crash: %v", err)
	}

	f.deps.PublishStep = nil
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install", "--no-start"); code != 0 {
		t.Fatalf("repair install exit %d: %s", code, f.err.String())
	}
	pending := readInitialGrant(t, paths.EnrollmentFile())
	entry := readPairingFile(t, pairingBootstrapPath(paths)).Grants[0]
	if entry.TokenHash != pairingGrantHash(pending.Grant) {
		t.Fatalf("repaired pairing hash does not match the collector grant")
	}
	m, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ path, kind string }{
		{paths.EnrollmentFile(), kindEnrollmentGrant},
		{pairingBootstrapPath(paths), kindPairingBootstrap},
	} {
		entry := m.entry(want.path, want.kind)
		if entry == nil || entry.State != manifestOwned || entry.Identity == nil || entry.SHA256 == "" {
			t.Fatalf("%s manifest entry = %+v, want owned with identity and hash", want.path, entry)
		}
	}
}

func TestReinstallRefreshesAnExpiredLocalPairingGrant(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start")
	before := readInitialGrant(t, paths.EnrollmentFile())
	beforeEntry := readPairingFile(t, pairingBootstrapPath(paths)).Grants[0]

	// Simulate `--no-start` followed by a start more than one grant lifetime
	// later: reinstall must stage a fresh grant instead of leaving the expired
	// one in place.
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install", "--no-start"); code != 0 {
		t.Fatalf("repair install exit %d: %s", code, f.err.String())
	}
	after := readInitialGrant(t, paths.EnrollmentFile())
	if after.Grant == before.Grant {
		t.Fatal("reinstall kept the old pairing grant")
	}
	if after.RequestID != before.RequestID || after.RecoverySecret != before.RecoverySecret {
		t.Fatalf("reinstall replaced the recovery proof: before %+v after %+v", before, after)
	}
	entry := readPairingFile(t, pairingBootstrapPath(paths)).Grants[0]
	if entry.TokenHash != pairingGrantHash(after.Grant) {
		t.Fatal("fresh pairing hash does not match the fresh collector grant")
	}
	if entry.TokenHash == beforeEntry.TokenHash {
		t.Fatal("pairing bootstrap kept the old grant hash")
	}
	expires, err := time.Parse(time.RFC3339, entry.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(expires); remaining <= 0 || remaining > autoPairGrantTTL {
		t.Fatalf("fresh grant lifetime = %s", remaining)
	}
}

func TestInstallBehindAProxyPairsTheLocalCollectorOverLoopback(t *testing.T) {
	// A proxy answers at the origin and forwards to the plaintext hub. The hub
	// accepts the local collector's loopback Host on its collector endpoints,
	// so the collector pairs over loopback instead of being left unpaired.
	f := newFixture(t)
	paths := f.install(t, "--no-start", "--origin", "https://hub.example.com")
	if strings.Contains(f.out.String(), "automatic pairing is unavailable") {
		t.Fatalf("install declined pairing: %s", f.out.String())
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "ws://127.0.0.1:8081/api/collector/connect"
	if len(cfg.Components.Collector.Destinations) != 1 || cfg.Components.Collector.Destinations[0].URL != want {
		t.Fatalf("destinations = %+v, want %s", cfg.Components.Collector.Destinations, want)
	}
	if pending := readInitialGrant(t, paths.EnrollmentFile()); pending.URL != want || pending.Grant == "" {
		t.Fatalf("staged grant = %+v", pending)
	}
}

func TestConfigureOriginKeepsTheLocalCollectorPaired(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start")
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	before := cfg.Components.Collector.Destinations
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "configure", "--origin", "https://hub.example.com", "--yes"); code != 0 {
		t.Fatalf("configure exit %d: %s", code, f.err.String())
	}
	if out := f.out.String(); strings.Contains(out, "automatic pairing is unavailable") || strings.Contains(out, "--role collector") {
		t.Fatalf("configure output = %s", out)
	}
	cfg, err = config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Components.Collector.Destinations, before) {
		t.Fatalf("destinations %+v changed from %+v", cfg.Components.Collector.Destinations, before)
	}
}

func TestDoctorReportsPendingLocalPairing(t *testing.T) {
	f := newFixture(t)
	f.install(t, "--no-start")
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "doctor"); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, f.out.String()+f.err.String())
	}
	// Nothing imported the bootstrap file yet, so the staged grant is unexpired
	// and pending.
	if !strings.Contains(f.out.String(), "pairing pending — the hub has not imported the staged grant yet") {
		t.Fatalf("doctor did not report the pending pairing:\n%s", f.out.String())
	}
}

func TestDoctorReportsAnImportedPairingGrantAsPending(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start")
	// The hub deletes the bootstrap file when it imports the grant.
	if err := os.Remove(pairingBootstrapPath(paths)); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "doctor"); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, f.out.String()+f.err.String())
	}
	if !strings.Contains(f.out.String(), "pairing pending — the hub has imported the staged grant") {
		t.Fatalf("doctor did not report the imported pairing:\n%s", f.out.String())
	}
}

func TestDoctorReportsAnExpiredLocalPairingGrant(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start")
	pending := readInitialGrant(t, paths.EnrollmentFile())
	writePairingFile(t, pairingBootstrapPath(paths), 0o600, pairingBootstrapGrant{
		TokenHash:   pairingGrantHash(pending.Grant),
		ExpiresAt:   time.Now().Add(-time.Minute).Format(time.RFC3339),
		Use:         storage.BootstrapGrantUseColocatedPairing,
		CollectorID: "local-install",
	})
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "doctor"); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, f.out.String()+f.err.String())
	}
	if !strings.Contains(f.out.String(), "pairing expired — rerun rigscope install") {
		t.Fatalf("doctor did not report the expired grant:\n%s", f.out.String())
	}
}

func TestDoctorReportsAStagedGrantMismatch(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start")
	writePairingFile(t, pairingBootstrapPath(paths), 0o600, pairingBootstrapGrant{
		TokenHash:   strings.Repeat("a", 64),
		ExpiresAt:   time.Now().Add(time.Hour).Format(time.RFC3339),
		Use:         storage.BootstrapGrantUseColocatedPairing,
		CollectorID: "some-other-install",
	})
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "doctor"); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, f.out.String()+f.err.String())
	}
	if !strings.Contains(f.out.String(), "the hub's staged pairing grant does not match") {
		t.Fatalf("doctor did not report the mismatch:\n%s", f.out.String())
	}
}

func TestDoctorDoesNotAdviseInstallWhileTheLocalHubIsDisabled(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start")
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Components.Hub.Enabled = false
	if err := config.Save(paths.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "doctor"); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, f.out.String()+f.err.String())
	}
	pairing := doctorLine(f.out.String(), "Collector pairing")
	if !strings.Contains(pairing, "the hub component is disabled") || strings.Contains(pairing, "rerun rigscope install") {
		t.Fatalf("doctor pairing check = %q", pairing)
	}
}

func TestDoctorSuggestsAReplacementGrantForAStaleLocalCredential(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start")
	store, err := state.Open(paths.StateDir, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	stale := "ws://127.0.0.1:9999/api/collector/connect"
	if err := store.SaveCredential(state.Credential{URL: stale, Token: "stale-credential"}); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "doctor"); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, f.out.String()+f.err.String())
	}
	pairing := doctorLine(f.out.String(), "Collector pairing")
	if !strings.Contains(pairing, stale) || !strings.Contains(pairing, "Replacement grant") {
		t.Fatalf("doctor pairing check = %q", pairing)
	}
}

// doctorLine returns the one doctor line for a check label.
func doctorLine(output, label string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, label+":") {
			return line
		}
	}
	return ""
}

func TestHubTrustsLoopbackProxy(t *testing.T) {
	cases := []struct {
		proxies []string
		want    bool
	}{
		{nil, false},
		{[]string{"10.0.0.5"}, false},
		{[]string{"10.0.0.0/8", "192.168.1.0/24"}, false},
		{[]string{"127.0.0.1"}, true},
		{[]string{"127.0.0.0/8"}, true},
		{[]string{"::1"}, true},
		{[]string{"10.0.0.5", "127.0.0.1"}, true},
		{[]string{"0.0.0.0/0"}, true},
	}
	for _, tc := range cases {
		if got := hubTrustsLoopbackProxy(config.Hub{TrustedProxies: tc.proxies}); got != tc.want {
			t.Fatalf("hubTrustsLoopbackProxy(%v) = %v, want %v", tc.proxies, got, tc.want)
		}
	}
}

func TestInstallDeclinesAutoPairingBehindALoopbackTrustedProxy(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "hub", "--no-start")
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Components.Hub.TrustedProxies = []string{"127.0.0.1"}
	if err := config.Save(paths.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install", "--role", "both", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), "automatic pairing is unavailable because the hub trusts a loopback reverse proxy") {
		t.Fatalf("install output = %s", f.out.String())
	}
	cfg, err = config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Components.Collector.Destinations) != 0 {
		t.Fatalf("destinations = %+v, want none behind a loopback proxy", cfg.Components.Collector.Destinations)
	}
	if _, err := os.Stat(paths.EnrollmentFile()); !os.IsNotExist(err) {
		t.Fatalf("install staged an unredeemable grant: %v", err)
	}
}

func TestReinstallRepointsAnUnenrolledLocalDestination(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start", "--port", "8081")
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install", "--no-start", "--port", "9090"); code != 0 {
		t.Fatalf("port-change install exit %d: %s", code, f.err.String())
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "ws://127.0.0.1:9090/api/collector/connect"
	if len(cfg.Components.Collector.Destinations) != 1 || cfg.Components.Collector.Destinations[0].URL != want {
		t.Fatalf("destinations = %+v, want %s", cfg.Components.Collector.Destinations, want)
	}
	pending := readEnrollments(t, paths.EnrollmentFile())
	found := false
	for _, enrollment := range pending {
		if enrollment.URL == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending enrollments = %+v, want one for %s", pending, want)
	}
}

func readEnrollments(t *testing.T, path string) []state.PendingEnrollment {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read enrollment file: %v", err)
	}
	var value struct {
		Enrollments []state.PendingEnrollment `json:"enrollments"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("parse enrollment file: %v\n%s", err, data)
	}
	return value.Enrollments
}

func TestInstallRefusesAnUnownedPairingBootstrap(t *testing.T) {
	f := newFixture(t)
	f.install(t, "--role", "hub", "--no-start")
	paths := f.paths(t, config.ScopeUser)
	if err := os.WriteFile(pairingBootstrapPath(paths), []byte("{\"grants\":[]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install", "--role", "both", "--no-start"); code == 0 {
		t.Fatal("install adopted an unowned pairing bootstrap")
	}
	if !strings.Contains(f.err.String(), "not recorded") {
		t.Fatalf("install error does not explain the unowned file: %s", f.err.String())
	}
}

var errSimulatedCrash = errors.New("simulated process death")

func TestEnablingTheCollectorLaterAutoPairs(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "hub", "--no-start")
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Components.Collector.Destinations) != 0 {
		t.Fatalf("hub-only destinations = %+v", cfg.Components.Collector.Destinations)
	}
	pairingPath := filepath.Join(paths.ComponentStateDir(systemd.Hub.String()), pairingBootstrapFileName)
	if _, err := os.Stat(pairingPath); !os.IsNotExist(err) {
		t.Fatalf("hub-only install wrote a pairing bootstrap: %v", err)
	}

	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install", "--role", "both", "--no-start"); code != 0 {
		t.Fatalf("enable collector exit %d: %s", code, f.err.String())
	}
	cfg, err = config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Components.Collector.Destinations) != 1 {
		t.Fatalf("destinations after enabling the collector = %+v", cfg.Components.Collector.Destinations)
	}
	if _, err := os.Stat(paths.EnrollmentFile()); err != nil {
		t.Fatalf("later collector enable wrote no grant: %v", err)
	}
	if _, err := os.Stat(pairingPath); err != nil {
		t.Fatalf("later collector enable wrote no pairing bootstrap: %v", err)
	}
}

func TestExplicitHubInstallDoesNotAutoPair(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--role", "both", "--hub", "https://hub.example.com", "--token", "grant", "--no-start")
	pairingPath := filepath.Join(paths.ComponentStateDir(systemd.Hub.String()), pairingBootstrapFileName)
	if _, err := os.Stat(pairingPath); !os.IsNotExist(err) {
		t.Fatalf("explicit --hub install wrote an automatic pairing bootstrap: %v", err)
	}
}

func TestSystemScopeAutoPairUsesBothServiceAccounts(t *testing.T) {
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
	var fileChowns []string
	f.deps.ChownFile = func(file *os.File, userName, groupName string) error {
		fileChowns = append(fileChowns, file.Name()+" "+userName+":"+groupName)
		return nil
	}
	if code := f.run(t, "install", "--system", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.paths(t, config.ScopeSystem)
	pairingPath := filepath.Join(paths.ComponentStateDir(systemd.Hub.String()), pairingBootstrapFileName)
	if info, err := os.Stat(pairingPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pairing bootstrap mode = %v, err = %v", info.Mode().Perm(), err)
	}
	joined := strings.Join(fileChowns, "\n")
	if !strings.Contains(joined, "collector-pairing.json") || !strings.Contains(joined, "rigscope-hub:rigscope") {
		t.Fatalf("hub pairing bootstrap descriptor chown missing:\n%s", joined)
	}
	if !strings.Contains(joined, "enrollment.json") || !strings.Contains(joined, "rigscope-collector:rigscope") {
		t.Fatalf("collector enrollment descriptor chown missing:\n%s", joined)
	}
}

func TestHubImportsPairingBootstrapAndDeletesIt(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	token, hash, err := storage.NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, pairingBootstrapFileName)
	writePairingFile(t, path, 0o600, pairingBootstrapGrant{
		TokenHash:   hash,
		ExpiresAt:   time.Now().Add(autoPairGrantTTL).Format(time.RFC3339),
		Use:         storage.BootstrapGrantUseColocatedPairing,
		CollectorID: "auto-paired",
	})
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry, err := openRegistryWithBootstrap(ctx, db, stateDir)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("pairing bootstrap survived import: %v", err)
	}
	if _, err := registry.Enroll(ctx, storage.EnrollRequest{CollectorID: "auto-paired", Token: token, LocalPeer: true}); err != nil {
		t.Fatalf("enroll with imported grant: %v", err)
	}
	// Recreating the same file after the grant was consumed must not resurrect
	// it. This is the exactly-once guarantee across a crash before deletion.
	writePairingFile(t, path, 0o600, pairingBootstrapGrant{
		TokenHash:   hash,
		ExpiresAt:   time.Now().Add(autoPairGrantTTL).Format(time.RFC3339),
		Use:         storage.BootstrapGrantUseColocatedPairing,
		CollectorID: "auto-paired",
	})
	if _, err := openRegistryWithBootstrap(ctx, db, stateDir); err != nil {
		t.Fatalf("reopen registry: %v", err)
	}
	if _, err := registry.Enroll(ctx, storage.EnrollRequest{CollectorID: "second", Token: token, LocalPeer: true}); storage.CodeOf(err) != storage.CodeForbidden {
		t.Fatalf("consumed grant replayed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("recreated pairing bootstrap survived the second import: %v", err)
	}
}

func TestHubIgnoresExpiredPairingBootstrapGrant(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	token, hash, err := storage.NewEnrollmentGrantToken()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, pairingBootstrapFileName)
	writePairingFile(t, path, 0o600, pairingBootstrapGrant{
		TokenHash:   hash,
		ExpiresAt:   time.Now().Add(-time.Minute).Format(time.RFC3339),
		Use:         storage.BootstrapGrantUseColocatedPairing,
		CollectorID: "late",
	})
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry, err := openRegistryWithBootstrap(ctx, db, stateDir)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired pairing bootstrap survived import: %v", err)
	}
	if _, err := registry.Enroll(ctx, storage.EnrollRequest{CollectorID: "late", Token: token, LocalPeer: true}); storage.CodeOf(err) != storage.CodeForbidden {
		t.Fatalf("expired grant error = %v", err)
	}
}

func TestHubRejectsInsecurePairingBootstrap(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, pairingBootstrapFileName)
	writePairingFile(t, path, 0o644, pairingBootstrapGrant{
		TokenHash:   strings.Repeat("a", 64),
		ExpiresAt:   time.Now().Add(time.Minute).Format(time.RFC3339),
		Use:         storage.BootstrapGrantUseColocatedPairing,
		CollectorID: "auto-paired",
	})
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := openRegistryWithBootstrap(ctx, db, stateDir); err == nil || !strings.Contains(err.Error(), "must not be group or world accessible") {
		t.Fatalf("insecure bootstrap error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("refused bootstrap file was removed: %v", err)
	}
}

func writePairingFile(t *testing.T, path string, mode os.FileMode, grants ...pairingBootstrapGrant) {
	t.Helper()
	data, err := json.Marshal(pairingBootstrapFile{Grants: grants})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestLocalHubDestinationChoosesLoopbackAndPinsTLSCertificate(t *testing.T) {
	dir := t.TempDir()
	loopbackCert := writeTestCertificate(t, dir, "loopback.pem", []string{"127.0.0.1"}, nil)
	destination, ok, notice := localHubDestination(config.Hub{
		Listen:  "127.0.0.1",
		Port:    8443,
		TLSCert: loopbackCert,
	})
	if !ok {
		t.Fatalf("loopback TLS destination was refused: %s", notice)
	}
	if destination.URL != "wss://127.0.0.1:8443/api/collector/connect" || destination.CAFile != loopbackCert {
		t.Fatalf("destination = %+v", destination)
	}

	// With an origin configured, the hub accepts only that exact authority, so
	// the destination must be the origin authority rather than loopback.
	originCert := writeTestCertificate(t, dir, "origin.pem", nil, []string{"hub.example.com"})
	destination, ok, notice = localHubDestination(config.Hub{
		Listen:  "0.0.0.0",
		Port:    8443,
		Origin:  "https://hub.example.com",
		TLSCert: originCert,
	})
	if !ok {
		t.Fatalf("origin TLS destination was refused: %s", notice)
	}
	if destination.URL != "wss://hub.example.com/api/collector/connect" || destination.CAFile != originCert {
		t.Fatalf("origin destination = %+v", destination)
	}

	// A plaintext hub behind a TLS-terminating proxy pairs its own collector
	// over loopback: the hub accepts the loopback Host on collector endpoints
	// from a direct, unproxied local peer.
	destination, ok, notice = localHubDestination(config.Hub{
		Listen: "127.0.0.1",
		Port:   8081,
		Origin: "https://hub.example.com",
	})
	if !ok || destination.URL != "ws://127.0.0.1:8081/api/collector/connect" || destination.CAFile != "" || notice != "" {
		t.Fatalf("plaintext origin destination = %+v, ok=%v, notice=%q", destination, ok, notice)
	}
}

func TestLocalHubDestinationMatchesTheCertificateHost(t *testing.T) {
	cases := []struct {
		name   string
		dns    []string
		ips    []string
		listen string
		want   string
	}{
		{name: "localhost only", dns: []string{"localhost"}, listen: "0.0.0.0", want: "wss://localhost:8443/api/collector/connect"},
		{name: "ipv4 only", ips: []string{"127.0.0.1"}, listen: "0.0.0.0", want: "wss://127.0.0.1:8443/api/collector/connect"},
		{name: "ipv6 only", ips: []string{"::1"}, listen: "::", want: "wss://[::1]:8443/api/collector/connect"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cert := writeTestCertificate(t, dir, "hub.pem", tc.ips, tc.dns)
			destination, ok, notice := localHubDestination(config.Hub{
				Listen:  tc.listen,
				Port:    8443,
				TLSCert: cert,
			})
			if !ok {
				t.Fatalf("destination refused: %s", notice)
			}
			if destination.URL != tc.want || destination.CAFile != cert {
				t.Fatalf("destination = %+v, want %s", destination, tc.want)
			}
		})
	}
	// A certificate that covers none of the loopback hosts is refused rather
	// than paired with a host it cannot verify.
	dir := t.TempDir()
	cert := writeTestCertificate(t, dir, "remote.pem", nil, []string{"hub.example.com"})
	if destination, ok, _ := localHubDestination(config.Hub{Listen: "0.0.0.0", Port: 8443, TLSCert: cert}); ok {
		t.Fatalf("uncovered certificate produced %+v", destination)
	}
}

func writeTestCertificate(t *testing.T, dir, name string, ips, dns []string) string {
	t.Helper()
	certPEM, _ := testCertificatePEM(t, ips, dns)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeTestKeyPair writes a self-signed certificate and its private key. The
// certificate is its own issuer, so pinning it as the destination CA verifies the
// hub the way an install does.
func writeTestKeyPair(t *testing.T, dir, name string, ips, dns []string) (certPath, keyPath string) {
	t.Helper()
	certPEM, keyPEM := testCertificatePEM(t, ips, dns)
	certPath = filepath.Join(dir, name)
	keyPath = filepath.Join(dir, strings.TrimSuffix(name, filepath.Ext(name))+".key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func testCertificatePEM(t *testing.T, ips, dns []string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Rigscope test hub"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              dns,
	}
	for _, value := range ips {
		template.IPAddresses = append(template.IPAddresses, net.ParseIP(value))
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// localNonLoopbackIP returns one of this host's non-loopback IPv4 addresses, or
// skips the test when the machine has none.
func localNonLoopbackIP(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("list interface addresses: %v", err)
	}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err != nil || ip.IsLoopback() || ip.To4() == nil {
			continue
		}
		return ip.String()
	}
	t.Skip("this machine has no non-loopback IPv4 address")
	return ""
}

// TestOriginTLSAutoPairingRedeemsOverTheHostsOwnAddress is the end-to-end case
// for an origin plus hub-served TLS: the installer records the origin authority
// as the destination, the collector dials it, and on the hub's own machine that
// connection arrives from one of the host's own addresses rather than loopback.
// The redemption must succeed, not merely produce a URL.
func TestOriginTLSAutoPairingRedeemsOverTheHostsOwnAddress(t *testing.T) {
	ip := localNonLoopbackIP(t)
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", ip, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	certPath, keyPath := writeTestKeyPair(t, t.TempDir(), "hub.pem", []string{ip}, nil)
	origin := "https://" + net.JoinHostPort(ip, strconv.Itoa(port))
	want := "wss://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/api/collector/connect"

	f := newFixture(t)
	paths := f.install(t, "--no-start", "--port", strconv.Itoa(port), "--origin", origin)
	// An origin can only be paired automatically when the hub serves TLS itself
	// under a certificate that covers the origin host.
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Components.Hub.TLSCert = certPath
	cfg.Components.Hub.TLSKey = keyPath
	if err := config.Save(paths.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install", "--no-start"); code != 0 {
		t.Fatalf("install with a hub certificate exit %d: %s", code, f.err.String())
	}
	cfg, err = config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Components.Collector.Destinations) != 1 {
		t.Fatalf("destinations = %+v", cfg.Components.Collector.Destinations)
	}
	destination := cfg.Components.Collector.Destinations[0]
	if destination.URL != want || destination.CAFile != certPath {
		t.Fatalf("destination = %+v, want %s pinned to %s", destination, want, certPath)
	}
	// The first install (plaintext, behind the origin) staged a loopback grant
	// too; the TLS reinstall adds the one for the origin authority.
	pendingStore, err := state.Open(paths.StateDir, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := pendingStore.PendingEnrollment(want)
	if err != nil || !found {
		t.Fatalf("no staged grant for %s: found=%v err=%v", want, found, err)
	}

	ctx := context.Background()
	stateDir := paths.ComponentStateDir(systemd.Hub.String())
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry, err := openRegistryWithBootstrap(ctx, db, stateDir)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	importer := newPairingImporter(stateDir, registry)
	handler := ingest.NewEnrollmentHandler(ingest.EnrollmentOptions{
		Registry:            registry,
		ImportPendingClaims: importer.importPending,
	})
	runtime, err := server.New(server.Options{
		Hub:      cfg.Components.Hub,
		StateDir: stateDir,
		Version:  "test-version",
		HubRunID: "00000000-0000-4000-8000-0000000000aa",
		UI:       pairingTestUI(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("hub server: %v", err)
	}
	if err := runtime.Mount("/api/collector/enroll", handler); err != nil {
		t.Fatalf("mount enrollment handler: %v", err)
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{
		Handler:           runtime.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = httpServer.Serve(tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		}))
	}()
	defer func() { _ = httpServer.Close() }()

	store, err := state.Open(paths.StateDir, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := enroll.Run(ctx, enroll.Options{
		Store:   store,
		HubURL:  destination.URL,
		Grant:   pending.Grant,
		CAFile:  destination.CAFile,
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("origin+TLS auto-pairing redemption failed: %v", err)
	}
	if result.Credential.Token == "" {
		t.Fatal("redemption returned an empty credential")
	}
	if _, found, err := store.Credential(destination.URL); err != nil || !found {
		t.Fatalf("collector credential stored = %v, %v", found, err)
	}
	installation, err := store.Identity()
	if err != nil {
		t.Fatal(err)
	}
	identities, err := registry.Identities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || identities[0].CollectorID != installation.CollectorID {
		t.Fatalf("hub identities = %+v, want one for %s", identities, installation.CollectorID)
	}
	if _, err := os.Stat(pairingBootstrapPath(paths)); !os.IsNotExist(err) {
		t.Fatalf("the hub did not import and delete the bootstrap file: %v", err)
	}
}

// pairingTestUI is the smallest static export a hub server will start with.
func pairingTestUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>Rigscope</title>")},
		"login.html": &fstest.MapFile{Data: []byte("<!doctype html><title>Sign in</title>")},
		"404.html":   &fstest.MapFile{Data: []byte("<!doctype html><title>Not found</title>")},
	}
}

// A running hub may import and delete the bootstrap file between its
// publication and the installer recording it. The install still succeeds,
// stages the collector's grant, and records no owned bootstrap file.
func TestInstallSurvivesTheHubImportingTheBootstrapImmediately(t *testing.T) {
	f := newFixture(t)
	paths := f.paths(t, config.ScopeUser)
	pairingPath := filepath.Join(paths.ComponentStateDir(systemd.Hub.String()), pairingBootstrapFileName)
	f.deps.PublishStep = func(path, phase string) error {
		if path == pairingPath && phase == "published" {
			return os.Remove(path)
		}
		return nil
	}
	if code := f.run(t, "install"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if pending := readInitialGrant(t, paths.EnrollmentFile()); pending.Grant == "" {
		t.Fatalf("collector grant was not staged: %+v", pending)
	}
	if entry := f.manifest(t, paths).entry(pairingPath, kindPairingBootstrap); entry != nil && entry.State == manifestOwned {
		t.Fatalf("consumed bootstrap recorded as owned: %+v", entry)
	}
	f.deps.PublishStep = nil
	f.err.Reset()
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
}

func TestConfigureTrustedProxyAndProxyHint(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start")
	f.out.Reset()
	if code := f.run(t, "configure", "--origin", "https://laptop.example.ts.net:8081", "--yes"); code != 0 {
		t.Fatalf("configure --origin exit %d: %s", code, f.err.String())
	}
	out := f.out.String()
	for _, want := range []string{
		"the hub itself serves only http://127.0.0.1:8081",
		"sudo tailscale serve --bg --https=8081 http://127.0.0.1:8081",
		"rigscope configure --trusted-proxy 127.0.0.1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("configure output lacks %q:\n%s", want, out)
		}
	}

	f.out.Reset()
	if code := f.run(t, "configure", "--trusted-proxy", "127.0.0.1", "--yes"); code != 0 {
		t.Fatalf("configure --trusted-proxy exit %d: %s", code, f.err.String())
	}
	out = f.out.String()
	if !strings.Contains(out, "trusted proxies: none -> 127.0.0.1") {
		t.Fatalf("configure output = %s", out)
	}
	if strings.Contains(out, "reverse proxy in front") || strings.Contains(out, "keep the old address") {
		t.Fatalf("configure repeated a hint that no longer applies:\n%s", out)
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Components.Hub.TrustedProxies, []string{"127.0.0.1"}) {
		t.Fatalf("trustedProxies = %v", cfg.Components.Hub.TrustedProxies)
	}

	f.out.Reset()
	if code := f.run(t, "configure", "--trusted-proxy", "none", "--yes"); code != 0 {
		t.Fatalf("configure --trusted-proxy none exit %d: %s", code, f.err.String())
	}
	if cfg, err = config.Load(paths.ConfigFile); err != nil || len(cfg.Components.Hub.TrustedProxies) != 0 {
		t.Fatalf("trustedProxies after none = %v (%v)", cfg.Components.Hub.TrustedProxies, err)
	}

	f.err.Reset()
	if code := f.run(t, "configure", "--trusted-proxy", "proxy.local"); code == 0 || !strings.Contains(f.err.String(), "not an IP address") {
		t.Fatalf("bad proxy accepted: exit %d, %s", code, f.err.String())
	}
}
