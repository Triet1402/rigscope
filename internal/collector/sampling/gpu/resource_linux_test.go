//go:build linux

package gpu

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHelperExecutableUsesProcSelfExe reproduces the upgrade race: resolving
// the running binary through os.Executable() returns a pathname, so a binary
// replaced by an upgrade makes the helper fail or run the new build. The
// kernel handle is immune, and the test override still wins.
func TestHelperExecutableUsesProcSelfExe(t *testing.T) {
	path, err := (HelperSource{}).helperExecutable()
	if err != nil {
		t.Fatalf("helper executable: %v", err)
	}
	if path != "/proc/self/exe" {
		t.Fatalf("helper executable = %q, want /proc/self/exe", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	override := HelperSource{Executable: func() (string, error) { return "/custom/rigscope", nil }}
	if got, err := override.helperExecutable(); err != nil || got != "/custom/rigscope" {
		t.Fatalf("override executable = %q, %v", got, err)
	}
}

// TestOpenResourceNoFollowRefusesASymlink is the last line of defense after
// resolution: even if the final component is replaced by a symlink before the
// open, it must not be followed.
func TestOpenResourceNoFollowRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "unrelated")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "resource0")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if file, err := openResourceNoFollow(link); err == nil {
		file.Close()
		t.Fatal("opened a symlinked resource0, want O_NOFOLLOW to refuse it")
	}
}
