package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicCreatesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	if err := WriteFileAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "first" {
		t.Fatalf("read = %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %04o, want 0600", mode)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Fatalf("dir mode = %04o, want 0700", mode)
	}
	if err := WriteFileAtomic(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "second" {
		t.Fatalf("rewritten = %q", data)
	}
}

func TestReplaceKeepingPrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin", "rigscope")
	if err := ReplaceKeepingPrevious(path, []byte("v1"), 0o755); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if _, err := os.Stat(path + ".prev"); !os.IsNotExist(err) {
		t.Fatalf("prev exists after first install: %v", err)
	}
	if err := ReplaceKeepingPrevious(path, []byte("v2"), 0o755); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	checkFile(t, path, "v2")
	checkFile(t, path+".prev", "v1")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %04o, want 0755", info.Mode().Perm())
	}
}

func TestSwapWithPrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rigscope")
	if err := SwapWithPrevious(path); err == nil {
		t.Fatal("swap without files succeeded")
	}
	if err := ReplaceKeepingPrevious(path, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SwapWithPrevious(path); err == nil {
		t.Fatal("swap without previous succeeded")
	}
	if err := ReplaceKeepingPrevious(path, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SwapWithPrevious(path); err != nil {
		t.Fatalf("swap: %v", err)
	}
	checkFile(t, path, "v1")
	checkFile(t, path+".prev", "v2")
	if err := SwapWithPrevious(path); err != nil {
		t.Fatalf("swap back: %v", err)
	}
	checkFile(t, path, "v2")
	checkFile(t, path+".prev", "v1")
}

func TestPublishBinaryRetainsPreviousAndMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rigscope")
	previous := target + ".prev"
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(target, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PublishBinary(staged, target, previous, "tx1"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	checkFile(t, target, "v2")
	checkFile(t, previous, "v1")
	for _, path := range []string{target, previous} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %04o, want 0755", path, info.Mode().Perm())
		}
	}
}

func TestPublishBinaryRecoversInterruptedRename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rigscope")
	previous := target + ".prev"
	if err := os.WriteFile(target, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("v0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempName(target, "tx2", "prev"), []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempName(target, "tx2", "new"), []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	did, err := RecoverPublishBinary(target, previous, "tx2")
	if err != nil || !did {
		t.Fatalf("recover = %v, %v", did, err)
	}
	checkFile(t, target, "v2")
	checkFile(t, previous, "v1")
}

func TestRestorePreviousNeverLeavesTargetMissing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rigscope")
	previous := target + ".prev"
	if err := os.WriteFile(target, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("v1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := RestorePrevious(target, previous, "tx3"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	checkFile(t, target, "v1")
	checkFile(t, previous, "v2")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("restored mode = %04o, want 0750", info.Mode().Perm())
	}
}

func TestRestorePreviousRecoversInterruptedSwap(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rigscope")
	previous := target + ".prev"
	if err := os.WriteFile(target, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	// First rename already happened: target is v1, and the failed target is
	// still staged as the retained-previous temporary.
	if err := os.WriteFile(target, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempName(target, "tx4", "restore-old"), []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	restoredSum, _ := HashFile(target)
	retainedSum, _ := HashFile(tempName(target, "tx4", "restore-old"))
	did, err := RecoverSwap(target, previous, "tx4", restoredSum, retainedSum)
	if err != nil || !did {
		t.Fatalf("recover = %v, %v", did, err)
	}
	checkFile(t, target, "v1")
	checkFile(t, previous, "v2")
}

func TestRecoverSwapRefusesUnverifiedCandidates(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rigscope")
	previous := target + ".prev"
	if err := os.WriteFile(target, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("v0"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The restore-new temp does not match the journal's previous digest: it
	// must not be renamed over the running binary, and every file is kept.
	bogus := tempName(target, "txbad", "restore-new")
	if err := os.WriteFile(bogus, []byte("foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	restoredSum, _ := HashFile(target)
	retainedSum, _ := HashFile(previous)
	if _, err := RecoverSwap(target, previous, "txbad", restoredSum, retainedSum); err == nil {
		t.Fatal("unverified restore candidate was published")
	}
	checkFile(t, target, "v1")
	checkFile(t, previous, "v0")
	checkFile(t, bogus, "foreign")

	// An unverified retained-previous candidate is refused for the same
	// reason, even when the restore candidate is valid.
	validRestore := tempName(target, "txbad2", "restore-new")
	if err := os.WriteFile(validRestore, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldTemp := tempName(target, "txbad2", "restore-old")
	if err := os.WriteFile(oldTemp, []byte("foreign"), 0o755); err != nil {
		t.Fatal(err)
	}
	restoredSum, _ = HashFile(validRestore)
	retainedSum, _ = HashFile(target)
	if _, err := RecoverSwap(target, previous, "txbad2", restoredSum, retainedSum); err == nil {
		t.Fatal("unverified retained-previous candidate was published")
	}
	checkFile(t, target, "v1")
	checkFile(t, previous, "v0")
	checkFile(t, oldTemp, "foreign")
	checkFile(t, validRestore, "v1")
}

func TestPublishBinaryRejectsUnsafeToken(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rigscope")
	previous := target + ".prev"
	staged := filepath.Join(dir, "staged")
	for _, path := range []string{target, staged} {
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := PublishBinary(staged, target, previous, "../evil"); err == nil {
		t.Fatal("unsafe token accepted")
	}
}

func TestPrepareAndPublishRecordHashes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rigscope")
	previous := target + ".prev"
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(target, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareBinary(staged, target, "txhash")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Path != tempName(target, "txhash", "new") {
		t.Fatalf("prepared path = %s", prepared.Path)
	}
	want, _ := HashFile(staged)
	if prepared.SHA256 != want {
		t.Fatalf("prepared hash = %s, want %s", prepared.SHA256, want)
	}
	if checkFileBytes(t, target) != "v1" {
		t.Fatal("prepare touched the target")
	}
	old, err := PublishPrepared(prepared, target, previous, "txhash")
	if err != nil {
		t.Fatal(err)
	}
	checkFile(t, target, "v2")
	checkFile(t, previous, "v1")
	if old.Path != previous || old.SHA256 == "" || old.SHA256 == prepared.SHA256 {
		t.Fatalf("previous identity = %+v", old)
	}
	if _, err := os.Stat(prepared.Path); !os.IsNotExist(err) {
		t.Fatalf("prepared temp survived publish: %v", err)
	}
}

func TestRecoverPublishDiscardsUnverifiedPreparedBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rigscope")
	previous := target + ".prev"
	if err := os.WriteFile(target, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("v0"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stale .prev from an older transaction and a truncated .new copy must
	// never be published: only the journal digest is authoritative.
	if err := os.WriteFile(tempName(target, "txbad", "new"), []byte("v2-truncated"), 0o755); err != nil {
		t.Fatal(err)
	}
	previousSum, _ := HashFile(target)
	preparedSum, _ := HashFile(target)
	_, err := RecoverPublish(target, previous, "txbad", preparedSum, previousSum)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	checkFile(t, target, "v1")
	checkFile(t, previous, "v1")
	if _, err := os.Stat(tempName(target, "txbad", "new")); !os.IsNotExist(err) {
		t.Fatalf("unverified prepared binary survived: %v", err)
	}
}

func TestRecoverPublishUsesVerifiedPreparedBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rigscope")
	previous := target + ".prev"
	if err := os.WriteFile(target, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("v0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempName(target, "txok", "new"), []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	newSum, _ := HashFile(tempName(target, "txok", "new"))
	previousSum, _ := HashFile(target)
	did, err := RecoverPublish(target, previous, "txok", newSum, previousSum)
	if err != nil || !did {
		t.Fatalf("recover = %v, %v", did, err)
	}
	checkFile(t, target, "v2")
	checkFile(t, previous, "v1")
}

func TestCleanupTransactionRemovesOnlyTemps(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rigscope")
	previous := target + ".prev"
	if err := os.WriteFile(target, []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"new", "prev", "restore-new", "restore-old"} {
		if err := os.WriteFile(tempName(target, "txtemp", suffix), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := CleanupTransaction(target, "txtemp"); err != nil {
		t.Fatal(err)
	}
	checkFile(t, target, "v2")
	checkFile(t, previous, "v1")
	for _, suffix := range []string{"new", "prev", "restore-new", "restore-old"} {
		if _, err := os.Stat(tempName(target, "txtemp", suffix)); !os.IsNotExist(err) {
			t.Fatalf("%s survived cleanup", suffix)
		}
	}
}

func checkFileBytes(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func checkFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
