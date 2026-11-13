package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

const testURL = "ws://127.0.0.1:8081/api/collector/connect"

// deterministicRandom returns a stream where every byte is derived from a
// counter, so generated UUIDs are unique but reproducible.
type deterministicRandom struct{ counter byte }

// testSeed keeps two stores opened in one test from generating identical ids.
var testSeed byte

func (r *deterministicRandom) Read(p []byte) (int, error) {
	for i := range p {
		r.counter++
		p[i] = r.counter
	}
	return len(p), nil
}

func openStore(t *testing.T, root, hostID string) *Store {
	t.Helper()
	testSeed++
	store, err := Open(root, Options{
		HostID: func() string { return hostID },
		Random: &deterministicRandom{counter: testSeed},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func TestIdentityPersistsAndIsPrivate(t *testing.T) {
	root := t.TempDir()
	first := openStore(t, root, "host-a")
	created, err := first.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(created.CollectorID) {
		t.Fatalf("collectorId %q is not a version 4 UUID", created.CollectorID)
	}
	if created.HostID != "host-a" {
		t.Fatalf("hostId = %q, want host-a", created.HostID)
	}
	if info, err := os.Stat(filepath.Join(root, "collector")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode = %v, err %v", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(filepath.Join(root, "collector", identityFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %v, err %v", info.Mode().Perm(), err)
	}

	second := openStore(t, root, "host-a")
	reloaded, err := second.Identity()
	if err != nil {
		t.Fatalf("reload Identity: %v", err)
	}
	if reloaded.CollectorID != created.CollectorID {
		t.Fatalf("identity changed across restarts: %s -> %s", created.CollectorID, reloaded.CollectorID)
	}
}

func TestClonedIdentityIsRegeneratedWithItsCredentialsCleared(t *testing.T) {
	root := t.TempDir()
	original := openStore(t, root, "host-a")
	created, err := original.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if err := original.SaveCredential(Credential{URL: testURL, Token: "old-token"}); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	if _, _, err := saveProof(original, testURL, "grant"); err != nil {
		t.Fatalf("SavePendingEnrollment: %v", err)
	}

	// The state directory was copied to another machine: same file, different
	// fingerprint.
	clone := openStore(t, root, "host-b")
	regenerated, err := clone.Identity()
	if err != nil {
		t.Fatalf("cloned Identity: %v", err)
	}
	if regenerated.CollectorID == created.CollectorID {
		t.Fatal("a cloned installation must generate a new collector id")
	}
	if regenerated.HostID != "host-b" {
		t.Fatalf("hostId = %q, want host-b", regenerated.HostID)
	}
	if credentials, err := clone.Credentials(); err != nil || len(credentials) != 0 {
		t.Fatalf("credentials survived clone recovery: %v, %v", credentials, err)
	}
	if pending, err := clone.PendingEnrollments(); err != nil || len(pending) != 0 {
		t.Fatalf("enrollment proof survived clone recovery: %v, %v", pending, err)
	}
}

func TestIdentityFromBeforeFingerprintingIsAdopted(t *testing.T) {
	root := t.TempDir()
	// A file written by an older build has no hostId. It must keep its id.
	legacy := StoredIdentity{CollectorID: "existing-collector"}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "collector")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, identityFile), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, root, "host-a")
	identity, err := store.Identity()
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if identity.CollectorID != "existing-collector" {
		t.Fatalf("collectorId = %q, want the existing one", identity.CollectorID)
	}
	if identity.HostID != "host-a" {
		t.Fatalf("hostId = %q, want host-a", identity.HostID)
	}
}

func saveProof(store *Store, url, grant string) (PendingEnrollment, bool, error) {
	requestID, secret, err := NewRecoveryProof(&deterministicRandom{})
	if err != nil {
		return PendingEnrollment{}, false, err
	}
	pending := PendingEnrollment{URL: url, Grant: grant, RequestID: requestID, RecoverySecret: secret}
	if err := store.SavePendingEnrollment(pending); err != nil {
		return PendingEnrollment{}, false, err
	}
	stored, ok, err := store.PendingEnrollment(url)
	return stored, ok, err
}

func TestCredentialRoundTripAndReplacement(t *testing.T) {
	store := openStore(t, t.TempDir(), "host-a")
	if _, ok, err := store.Credential(testURL); err != nil || ok {
		t.Fatalf("empty store Credential = ok %v, err %v", ok, err)
	}
	if err := store.SaveCredential(Credential{URL: testURL, Token: "first"}); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	if err := store.SaveCredential(Credential{URL: testURL, Token: "second"}); err != nil {
		t.Fatalf("replace SaveCredential: %v", err)
	}
	credential, ok, err := store.Credential(testURL)
	if err != nil || !ok {
		t.Fatalf("Credential = %v, %v", ok, err)
	}
	if credential.Token != "second" {
		t.Fatalf("token = %q, want second", credential.Token)
	}
	if err := store.SaveCredential(Credential{URL: testURL, Token: "bad token with spaces"}); err == nil {
		t.Fatal("expected a credential with spaces to be rejected")
	}
	if err := store.DeleteCredential(testURL); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if _, ok, err := store.Credential(testURL); err != nil || ok {
		t.Fatalf("deleted credential still present: ok %v, err %v", ok, err)
	}
}

func TestPendingEnrollmentRoundTripAndCompletion(t *testing.T) {
	store := openStore(t, t.TempDir(), "host-a")
	stored, ok, err := saveProof(store, testURL, "grant-1")
	if err != nil || !ok {
		t.Fatalf("saveProof = ok %v, err %v", ok, err)
	}
	if stored.RequestID == "" || len(stored.RecoverySecret) < 32 {
		t.Fatalf("incomplete proof: %+v", stored)
	}
	if err := store.CompleteEnrollment(testURL, Credential{URL: testURL, Token: "issued"}); err != nil {
		t.Fatalf("CompleteEnrollment: %v", err)
	}
	if _, ok, err := store.PendingEnrollment(testURL); err != nil || ok {
		t.Fatalf("enrollment proof survived completion: ok %v, err %v", ok, err)
	}
	if credential, ok, err := store.Credential(testURL); err != nil || !ok || credential.Token != "issued" {
		t.Fatalf("credential after completion = %+v, ok %v, err %v", credential, ok, err)
	}
}

func TestUnsafeFilesAreRefused(t *testing.T) {
	root := t.TempDir()
	store := openStore(t, root, "host-a")
	if _, err := store.Identity(); err != nil {
		t.Fatalf("Identity: %v", err)
	}
	path := filepath.Join(root, "collector", identityFile)

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Identity(); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("world-readable identity was accepted: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(root, "elsewhere.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Identity(); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlinked identity was accepted: %v", err)
	}
}

func TestCorruptStateIsRejectedWithoutRewriting(t *testing.T) {
	root := t.TempDir()
	store := openStore(t, root, "host-a")
	if _, err := store.Identity(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "collector", identityFile)
	broken := []byte("{broken")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Identity(); err == nil {
		t.Fatal("corrupt identity was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, broken) {
		t.Fatalf("corrupt file was rewritten: %q", after)
	}

	unknown := []byte(`{"collectorId":"c","hostId":"","extra":1}`)
	if err := os.WriteFile(path, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Identity(); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field was accepted: %v", err)
	}
}

func TestPrivateDirectoryIsRequired(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "collector"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, Options{HostID: func() string { return "host" }}); err == nil {
		t.Fatal("a world-readable state directory was accepted")
	}
}

func TestNoTemporaryFilesRemainAfterWrites(t *testing.T) {
	root := t.TempDir()
	store := openStore(t, root, "host-a")
	if _, err := store.Identity(); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCredential(Credential{URL: testURL, Token: "token"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "collector"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestRunIdentityIsFreshPerLaunch(t *testing.T) {
	store := openStore(t, t.TempDir(), "host-a")
	first, err := store.NewRunIdentity()
	if err != nil {
		t.Fatalf("NewRunIdentity: %v", err)
	}
	second, err := store.NewRunIdentity()
	if err != nil {
		t.Fatalf("NewRunIdentity: %v", err)
	}
	if first.RunID == second.RunID {
		t.Fatal("runId must change on every process launch")
	}
	if first.CollectorID != second.CollectorID {
		t.Fatal("the installation id must not change between launches")
	}
	if first.BootID == "" || len(first.BootID) > 128 {
		t.Fatalf("bootId = %q", first.BootID)
	}
	if len(first.Hostname) > 255 {
		t.Fatalf("hostname too long: %d", len(first.Hostname))
	}
}

func TestHubURLValidation(t *testing.T) {
	valid := map[string]string{
		"ws://127.0.0.1:8081/api/collector/connect":   "ws://127.0.0.1:8081/api/collector/connect",
		"ws://127.2.3.4/api/collector/connect":        "ws://127.2.3.4/api/collector/connect",
		"ws://[::1]:9000/api/collector/connect":       "ws://[::1]:9000/api/collector/connect",
		"wss://hub.example.com/api/collector/connect": "wss://hub.example.com/api/collector/connect",
		// The shared policy stores loopback names as literal addresses and
		// accepts the friendly http(s) spellings.
		"ws://localhost:8081/api/collector/connect":   "ws://127.0.0.1:8081/api/collector/connect",
		"http://127.0.0.1:8081/api/collector/connect": "ws://127.0.0.1:8081/api/collector/connect",
		"https://hub.example.com":                     "wss://hub.example.com/api/collector/connect",
		// A TLS hostname is the certificate identity and is kept as written.
		"wss://ip6-localhost:8443": "wss://ip6-localhost:8443/api/collector/connect",
		"https://localhost:8443":   "wss://localhost:8443/api/collector/connect",
	}
	for raw, want := range valid {
		got, err := ValidateHubURL(raw)
		if err != nil {
			t.Errorf("ValidateHubURL(%q) = %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("ValidateHubURL(%q) = %q, want %q", raw, got, want)
		}
	}
	invalid := []string{
		"",
		"ws://hub.example.com/api/collector/connect",
		"ws://user:pass@127.0.0.1/api/collector/connect",
		"ws://127.0.0.1/api/collector/connect?token=x",
		"ws://127.0.0.1/api/collector/connect#fragment",
		"ws://127.0.0.1/collect",
		"ftp://127.0.0.1/api/collector/connect",
	}
	for _, url := range invalid {
		if _, err := ValidateHubURL(url); err == nil {
			t.Errorf("ValidateHubURL(%q) accepted an invalid URL", url)
		}
	}
}

func TestWriteInitialGrantProducesAPendingProof(t *testing.T) {
	root := t.TempDir()
	uid, gid := os.Geteuid(), os.Getegid()
	err := WriteInitialGrant(root, testURL, "installer-grant", WriteOptions{
		UID:    &uid,
		GID:    &gid,
		Random: &deterministicRandom{},
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "operation not permitted") {
			t.Skipf("cannot exercise descriptor ownership as an unprivileged user: %v", err)
		}
		t.Fatalf("WriteInitialGrant: %v", err)
	}

	store := openStore(t, root, "host-a")
	pending, ok, err := store.PendingEnrollment(testURL)
	if err != nil || !ok {
		t.Fatalf("PendingEnrollment = ok %v, err %v", ok, err)
	}
	if pending.Grant != "installer-grant" {
		t.Fatalf("grant = %q", pending.Grant)
	}
	if pending.RequestID == "" || len(pending.RecoverySecret) < 32 {
		t.Fatalf("incomplete recovery proof: %+v", pending)
	}

	path := filepath.Join(root, "collector", enrollmentFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("enrollment mode = %v, want 0600", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat sys type = %T", info.Sys())
	}
	if int(stat.Uid) != uid || int(stat.Gid) != gid {
		t.Fatalf("owner = %d:%d, want %d:%d", stat.Uid, stat.Gid, uid, gid)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value enrollmentFileValue
	if err := decodeStrict(data, &value); err != nil {
		t.Fatalf("upgraded file does not use the collector schema: %v", err)
	}
	if len(value.Enrollments) != 1 || value.Enrollments[0].URL != testURL {
		t.Fatalf("enrollments = %+v", value.Enrollments)
	}

	// A second installer write for the same hub replaces rather than duplicates.
	err = WriteInitialGrant(root, testURL, "replacement-grant", WriteOptions{
		UID: &uid, GID: &gid, Random: &deterministicRandom{counter: 11},
	})
	if err != nil {
		t.Fatalf("replace WriteInitialGrant: %v", err)
	}
	pending, ok, err = store.PendingEnrollment(testURL)
	if err != nil || !ok || pending.Grant != "replacement-grant" {
		t.Fatalf("replacement = %+v, ok %v, err %v", pending, ok, err)
	}
}

func TestLegacyFlatGrantIsUpgradedBeforeUse(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "collector")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"version":1,"url":"` + testURL + `","grant":"legacy-grant"}` + "\n")
	path := filepath.Join(dir, enrollmentFile)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	store := openStore(t, root, "host-a")
	pending, ok, err := store.PendingEnrollment(testURL)
	if err != nil || !ok {
		t.Fatalf("legacy PendingEnrollment = ok %v, err %v", ok, err)
	}
	if pending.Grant != "legacy-grant" || pending.RequestID == "" || len(pending.RecoverySecret) < 32 {
		t.Fatalf("upgraded proof = %+v", pending)
	}

	upgraded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value enrollmentFileValue
	if err := decodeStrict(upgraded, &value); err != nil {
		t.Fatalf("legacy file was not upgraded: %v", err)
	}
	if len(value.Enrollments) != 1 || value.Enrollments[0].RequestID != pending.RequestID {
		t.Fatalf("upgraded file = %+v, want the same stable proof", value.Enrollments)
	}

	// Reading again is stable: the recovery proof is not regenerated, so a
	// retried enrollment after a lost response still recovers.
	again, ok, err := store.PendingEnrollment(testURL)
	if err != nil || !ok || again.RequestID != pending.RequestID || again.RecoverySecret != pending.RecoverySecret {
		t.Fatalf("proof changed after upgrade: %+v, ok %v, err %v", again, ok, err)
	}
}

func TestUnknownLegacyEnrollmentVersionIsRejected(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "collector")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, enrollmentFile)
	original := []byte(`{"version":2,"url":"` + testURL + `","grant":"g"}` + "\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	store := openStore(t, root, "host-a")
	if _, _, err := store.PendingEnrollment(testURL); err == nil {
		t.Fatal("an unknown enrollment file version was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("rejected file was rewritten: %q", after)
	}
}

func TestDecodeStrictRejectsTrailingData(t *testing.T) {
	var value StoredIdentity
	if err := decodeStrict([]byte(`{"collectorId":"c","hostId":"h"} {"x":1}`), &value); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
	if err := decodeStrict([]byte(`[]`), &value); err == nil {
		t.Fatal("array was accepted as an object")
	}
}

// writeRawState writes a state document exactly as an older binary could have
// left it, bypassing the canonicalization that saves apply.
func writeRawState(t *testing.T, store *Store, name string, value any) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.writeJSONLocked(name, value); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestStateWrittenBeforeURLNormalizationIsStillFound(t *testing.T) {
	const legacy = "ws://localhost:8081/api/collector/connect"
	const canonical = "ws://127.0.0.1:8081/api/collector/connect"
	store := openStore(t, t.TempDir(), "host-a")
	writeRawState(t, store, credentialsFile, credentialsFileValue{Credentials: []Credential{{URL: legacy, Token: "issued"}}})
	writeRawState(t, store, enrollmentFile, enrollmentFileValue{Enrollments: []PendingEnrollment{{
		URL: legacy, Grant: "grant-1", RequestID: "request-1",
		RecoverySecret: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}}})

	// The configuration file now normalizes the destination to the literal
	// address; the credential and proof saved under the old spelling must
	// still belong to it.
	credential, ok, err := store.Credential(canonical)
	if err != nil || !ok || credential.Token != "issued" {
		t.Fatalf("Credential(%s) = %+v, ok %v, err %v", canonical, credential, ok, err)
	}
	if credential.URL != canonical {
		t.Fatalf("credential URL = %q, want the canonical %q", credential.URL, canonical)
	}
	if pending, ok, err := store.PendingEnrollment(canonical); err != nil || !ok || pending.Grant != "grant-1" {
		t.Fatalf("PendingEnrollment(%s) = %+v, ok %v, err %v", canonical, pending, ok, err)
	}
	// Lookups by the old spelling resolve to the same identity.
	if _, ok, err := store.Credential(legacy); err != nil || !ok {
		t.Fatalf("Credential(%s) = ok %v, err %v", legacy, ok, err)
	}
	if err := store.CompleteEnrollment(legacy, Credential{URL: canonical, Token: "reissued"}); err != nil {
		t.Fatalf("CompleteEnrollment across spellings: %v", err)
	}
	if _, ok, err := store.PendingEnrollment(canonical); err != nil || ok {
		t.Fatalf("proof survived completion: ok %v, err %v", ok, err)
	}
	all, err := store.Credentials()
	if err != nil || len(all) != 1 || all[0].URL != canonical || all[0].Token != "reissued" {
		t.Fatalf("Credentials = %+v, %v; want one canonical reissued entry", all, err)
	}
}

func TestSpellingOnlyDuplicateCredentialsCollapseToTheLaterEntry(t *testing.T) {
	store := openStore(t, t.TempDir(), "host-a")
	writeRawState(t, store, credentialsFile, credentialsFileValue{Credentials: []Credential{
		{URL: "ws://localhost:8081/api/collector/connect", Token: "older"},
		{URL: "ws://127.0.0.1:8081/api/collector/connect", Token: "newer"},
	}})
	all, err := store.Credentials()
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if len(all) != 1 || all[0].Token != "newer" {
		t.Fatalf("Credentials = %+v, want the later entry only", all)
	}
}

func TestAliasAndDropCredentialMoveALocalHubCredential(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	oldURL := "ws://127.0.0.1:8081/api/collector/connect"
	newURL := "ws://127.0.0.1:9191/api/collector/connect"
	if err := store.SaveCredential(Credential{URL: oldURL, Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	prepared := 0
	prepare := func(*os.File) error { prepared++; return nil }
	for i := 0; i < 2; i++ { // idempotent
		if err := store.AliasCredential(oldURL, newURL, prepare); err != nil {
			t.Fatal(err)
		}
	}
	if prepared != 1 {
		t.Fatalf("prepare called %d times, want 1", prepared)
	}
	for _, url := range []string{oldURL, newURL} {
		if credential, found, err := store.Credential(url); err != nil || !found || credential.Token != "secret" {
			t.Fatalf("%s: %+v %v %v", url, credential, found, err)
		}
	}
	if err := store.DropCredential(oldURL, prepare); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.Credential(oldURL); found {
		t.Fatal("old credential kept")
	}
	if _, found, _ := store.Credential(newURL); !found {
		t.Fatal("new credential lost")
	}
	if err := store.AliasCredential("ws://127.0.0.1:1/api/collector/connect", "ws://127.0.0.1:2/api/collector/connect", nil); err == nil {
		t.Fatal("aliasing a missing credential succeeded")
	}
}

// Options.Owner applies to every file the store writes without an explicit
// owner, so a root CLI never leaves a credential the service cannot read.
func TestStoreOwnerAppliesToEveryWrite(t *testing.T) {
	var owned []string
	store, err := Open(t.TempDir(), Options{Owner: func(file *os.File) error {
		owned = append(owned, filepath.Base(file.Name()))
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Identity(); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCredential(Credential{URL: testURL, Token: "token"}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(owned, " ")
	if !strings.Contains(joined, ".identity.json.") || !strings.Contains(joined, ".credentials.json.") {
		t.Fatalf("owner hook ran for %v", owned)
	}
}
