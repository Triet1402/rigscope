package lock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquireIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", ".install.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	second, err := Acquire(path)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire err = %v, want ErrBusy", err)
	}
	if second != nil {
		t.Fatalf("second acquire returned a lock: %v", second)
	}
	if !strings.Contains(err.Error(), "another rigscope operation is running") {
		t.Fatalf("err = %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	third, err := Acquire(path)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestAcquireWithTimeoutReportsBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".install.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	start := time.Now()
	_, err = AcquireWithTimeout(path, 80*time.Millisecond)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("timed acquisition returned too early: %s", elapsed)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	held, err := Acquire(filepath.Join(t.TempDir(), ".install.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestAcquireRejectsSymlinkLockFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "install.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil {
		t.Fatal("symlinked lock file was followed")
	}
}

func TestTmpFallbackDirectoryMustBePrivate(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "rigscope-locktest-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := Acquire(filepath.Join(dir, "install.lock"))
	if err != nil {
		t.Fatalf("private fallback rejected: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(filepath.Join(dir, "install.lock")); err == nil {
		t.Fatal("world-writable fallback directory was trusted")
	}
}

func TestAcquireInheritedValidatesAndSharesTheParentsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.lock")
	parent, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Release()

	command := exec.Command(os.Args[0], "-test.run=TestInheritedLockHelper")
	command.Env = append(os.Environ(),
		"RIGSCOPE_LOCK_HELPER=1",
		"RIGSCOPE_LOCK_HELPER_PATH="+path,
	)
	command.ExtraFiles = []*os.File{parent.File()}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, output)
	}
	// The child exited after taking the inherited description; closing its copy
	// must not have released the parent's lock.
	if _, err := Acquire(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("parent lock was released by the child: %v", err)
	}
}

func TestInheritedLockHelper(t *testing.T) {
	if os.Getenv("RIGSCOPE_LOCK_HELPER") != "1" {
		t.Skip("helper process only")
	}
	path := os.Getenv("RIGSCOPE_LOCK_HELPER_PATH")
	held, err := AcquireInherited(path, 3)
	if err != nil {
		t.Fatalf("acquire inherited: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("release inherited: %v", err)
	}
}

func TestAcquireInheritedRejectsAnotherInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "install.lock")
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(other)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := AcquireInherited(path, int(file.Fd())); err == nil {
		t.Fatal("a descriptor for another inode was accepted as the installation lock")
	}
	if _, err := AcquireInherited(path, 2); err == nil {
		t.Fatal("a reserved descriptor was accepted")
	}
}
