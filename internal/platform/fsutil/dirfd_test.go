package fsutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOpenDirNoFollowRejectsSymlinkComponent(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDirNoFollow(filepath.Join(link, "child")); err == nil {
		t.Fatal("symlinked parent was followed")
	}
	dir, err := OpenDirNoFollow(filepath.Join(real, "child"))
	if err != nil {
		t.Fatalf("real directory rejected: %v", err)
	}
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveAllDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(state, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "nested", "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, "foreign")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	foreignFile := filepath.Join(foreign, "keep")
	if err := os.WriteFile(foreignFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, filepath.Join(state, "link")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAllPath(state); err != nil {
		t.Fatalf("RemoveAllPath: %v", err)
	}
	if _, err := os.Lstat(state); !os.IsNotExist(err) {
		t.Fatalf("state directory survived: %v", err)
	}
	if _, err := os.Stat(foreignFile); err != nil {
		t.Fatalf("foreign file behind a symlink was deleted: %v", err)
	}
}

func TestRemoveAllPathMissingIsNotAnError(t *testing.T) {
	if err := RemoveAllPath(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("missing path: %v", err)
	}
}

func TestMountPointsFromDecodesEscapes(t *testing.T) {
	content := strings.Join([]string{
		"36 25 8:1 / / rw,relatime - ext4 /dev/sda1 rw",
		`37 36 0:22 / /var/lib/rigscope/backups\040one rw - tmpfs tmpfs rw`,
		`38 36 0:23 / /foreign\011tab\012newline\134slash rw - tmpfs tmpfs rw`,
		"malformed line",
	}, "\n")
	mounts, err := MountPointsFrom(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/", "/var/lib/rigscope/backups one", "/foreign\ttab\nnewline\\slash"}
	if strings.Join(mounts, "|") != strings.Join(want, "|") {
		t.Fatalf("mounts = %q, want %q", mounts, want)
	}
	if _, err := MountPointsFrom(strings.NewReader(`1 2 3 4 /bad\09 rw - tmpfs tmpfs rw`)); err == nil {
		t.Fatal("invalid escape accepted")
	}
}

func TestMountPointsFromSplitsOnASCIIWhitespaceOnly(t *testing.T) {
	// The kernel escapes ASCII space but leaves other Unicode spaces verbatim.
	// A parser using strings.Fields would split the path and read the wrong
	// field, hiding the bind mount from RemoveAll's safety check.
	content := strings.Join([]string{
		"36 25 8:1 / / rw,relatime - ext4 /dev/sda1 rw",
		"40 36 0:30 / /home/rig\u00a0scope/state rw,nosuid shared:1 - tmpfs tmpfs rw",
		"41 36 0:31 / /home/rig\u2003scope/state/sub rw shared:2 master:1 - tmpfs tmpfs rw",
		"42 36 0:32 / /home/rig scope/state rw shared:3 - tmpfs tmpfs rw",
		`43 36 0:33 / /home/rig\040scope/state\040two rw shared:4 - tmpfs tmpfs rw`,
	}, "\n")
	mounts, err := MountPointsFrom(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/",
		"/home/rig\u00a0scope/state",
		"/home/rig\u2003scope/state/sub",
		"/home/rig",
		"/home/rig scope/state two",
	}
	if strings.Join(mounts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("mounts = %q, want %q", mounts, want)
	}
}

func TestMountPointsFromIgnoresLinesWithoutSeparator(t *testing.T) {
	content := "36 25 8:1 / / rw,relatime\n" +
		"37 36 0:22 / /state rw - tmpfs tmpfs rw\n"
	mounts, err := MountPointsFrom(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0] != "/state" {
		t.Fatalf("mounts = %v", mounts)
	}
}

func TestMountPointsUnderUsesComponentsAndIncludesTheRoot(t *testing.T) {
	mounts := []string{"/var/lib/rigscope", "/var/lib/rigscope-backup", "/var/lib/rigscope/state", "/var/lib/rigscope/state/foreign"}
	inside := filterMounts(mounts, "/var/lib/rigscope/state")
	if len(inside) != 2 || inside[0] != "/var/lib/rigscope/state" || inside[1] != "/var/lib/rigscope/state/foreign" {
		t.Fatalf("inside = %v", inside)
	}
	if got := filterMounts(mounts, "/var/lib/rigscope/state/foreign"); len(got) != 1 || got[0] != "/var/lib/rigscope/state/foreign" {
		t.Fatalf("a mount at the removal root was excluded: %v", got)
	}
}

// filterMounts mirrors MountPointsUnder's comparison over an in-memory list.
func filterMounts(mounts []string, target string) []string {
	var found []string
	for _, mount := range mounts {
		if mount == target || isPathBelow(mount, target) {
			found = append(found, mount)
		}
	}
	return found
}

func TestRemoveAllRefusesExcessiveDepthBeforeDeleting(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	current := tree
	for index := 0; index <= maxRemovalDepth; index++ {
		current = filepath.Join(current, "d")
	}
	if err := os.MkdirAll(current, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tree, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := RemoveAllPath(tree)
	if err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Fatalf("deep tree err = %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("deep tree was partly deleted: %v", statErr)
	}
	if _, statErr := os.Stat(current); statErr != nil {
		t.Fatalf("deep tree was partly deleted: %v", statErr)
	}
}

func TestRemoveAllRefusesBindMount(t *testing.T) {
	if os.Getenv("RIGSCOPE_FSUTIL_MOUNT_HELPER") == "1" {
		runBindMountHelper(t)
		return
	}
	if !unshareAvailable() {
		t.Skip("unshare with mount namespace support is unavailable")
	}
	command := exec.Command("unshare", "-rm", os.Args[0], "-test.run", "TestRemoveAllRefusesBindMount")
	command.Env = append(os.Environ(), "RIGSCOPE_FSUTIL_MOUNT_HELPER=1")
	outputBytes, err := command.CombinedOutput()
	output := string(outputBytes)
	if err != nil {
		// unshare was available a moment ago, so a failure here is the helper
		// failing (setup, cleanup or a real regression), not a missing host
		// feature. Skipping would hide exactly the bugs this test exists for.
		t.Fatalf("mount namespace helper failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "PASS") {
		t.Fatalf("mount namespace probe did not pass:\n%s", output)
	}
}

func runBindMountHelper(t *testing.T) {
	root, err := os.MkdirTemp("", "rigscope-mount-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	state := filepath.Join(root, "state")
	foreign := filepath.Join(root, "foreign")
	inside := filepath.Join(state, "sub", "foreign")
	for _, dir := range []string{inside, foreign} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	foreignFile := filepath.Join(foreign, "keep")
	if err := os.WriteFile(foreignFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mountBind(foreign, inside); err != nil {
		t.Fatalf("bind mount: %v", err)
	}
	defer func() { _ = syscall.Unmount(inside, syscall.MNT_DETACH) }()
	err = RemoveAllPath(state)
	if err == nil || !strings.Contains(err.Error(), "mount point") {
		t.Fatalf("bind mount inside the tree was not refused: %v", err)
	}
	if _, statErr := os.Stat(foreignFile); statErr != nil {
		t.Fatalf("foreign file under the bind mount was deleted: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(state, "sub")); statErr != nil {
		t.Fatalf("state tree was partly deleted: %v", statErr)
	}
}

func TestRemoveAllRefusesBindMountUnderUnicodeAncestor(t *testing.T) {
	if os.Getenv("RIGSCOPE_FSUTIL_UNICODE_MOUNT_HELPER") == "1" {
		runUnicodeAncestorBindMountHelper(t)
		return
	}
	if !unshareAvailable() {
		t.Skip("unshare with mount namespace support is unavailable")
	}
	command := exec.Command("unshare", "-rm", os.Args[0], "-test.run", "TestRemoveAllRefusesBindMountUnderUnicodeAncestor")
	command.Env = append(os.Environ(), "RIGSCOPE_FSUTIL_UNICODE_MOUNT_HELPER=1")
	outputBytes, err := command.CombinedOutput()
	output := string(outputBytes)
	if err != nil {
		t.Fatalf("mount namespace helper failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "PASS") {
		t.Fatalf("mount namespace probe did not pass:\n%s", output)
	}
}

func runUnicodeAncestorBindMountHelper(t *testing.T) {
	root, err := os.MkdirTemp("", "rigscope-unicode-mount-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	// U+00A0 is unicode.IsSpace, so a strings.Fields parser would split this
	// ancestor into two fields and miss the mount below it.
	state := filepath.Join(root, "state\u00a0dir", "state")
	foreign := filepath.Join(root, "foreign")
	inside := filepath.Join(state, "sub", "foreign")
	for _, dir := range []string{inside, foreign} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	foreignFile := filepath.Join(foreign, "keep")
	if err := os.WriteFile(foreignFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mountBind(foreign, inside); err != nil {
		t.Fatalf("bind mount: %v", err)
	}
	defer func() { _ = syscall.Unmount(inside, syscall.MNT_DETACH) }()
	err = RemoveAllPath(state)
	if err == nil || !strings.Contains(err.Error(), "mount point") {
		t.Fatalf("bind mount under a Unicode ancestor was not refused: %v", err)
	}
	if _, statErr := os.Stat(foreignFile); statErr != nil {
		t.Fatalf("foreign file under the bind mount was deleted: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(state, "sub")); statErr != nil {
		t.Fatalf("state tree was partly deleted: %v", statErr)
	}
}

func unshareAvailable() bool {
	if _, err := exec.LookPath("unshare"); err != nil {
		return false
	}
	probe := exec.Command("unshare", "-rm", "true")
	return probe.Run() == nil
}

func TestRemoveAllRefusesMountAtTheRemovalRoot(t *testing.T) {
	if os.Getenv("RIGSCOPE_FSUTIL_ROOT_MOUNT_HELPER") == "1" {
		runRootMountHelper(t)
		return
	}
	if !unshareAvailable() {
		t.Skip("unshare with mount namespace support is unavailable")
	}
	command := exec.Command("unshare", "-rm", os.Args[0], "-test.run", "TestRemoveAllRefusesMountAtTheRemovalRoot")
	command.Env = append(os.Environ(), "RIGSCOPE_FSUTIL_ROOT_MOUNT_HELPER=1")
	outputBytes, err := command.CombinedOutput()
	output := string(outputBytes)
	if err != nil {
		t.Fatalf("mount namespace helper failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "PASS") {
		t.Fatalf("mount namespace probe did not pass:\n%s", output)
	}
}

func runRootMountHelper(t *testing.T) {
	root, err := os.MkdirTemp("", "rigscope-root-mount-")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(state, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(state, "sub", "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A bind mount of the removal root onto itself is a same-device mount that
	// st_dev cannot detect. Deleting below it would empty the mounted tree.
	if err := mountBind(state, state); err != nil {
		t.Fatalf("bind mount: %v", err)
	}
	defer func() {
		_ = syscall.Unmount(state, syscall.MNT_DETACH)
		_ = os.RemoveAll(root)
	}()
	err = RemoveAllPath(state)
	if err == nil || !strings.Contains(err.Error(), "mount point") {
		t.Fatalf("a mount at the removal root was not refused: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the mounted tree was deleted: %v", statErr)
	}
}

func mountBind(source, target string) error {
	return syscall.Mount(source, target, "", syscall.MS_BIND, "")
}

func TestWriteFileDurablePropagatesDirectorySyncFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	syncErr := errors.New("simulated directory fsync failure")
	err := writeFileDurable(path, []byte("data"), 0o600, true, nil, func(string) error { return syncErr })
	if !errors.Is(err, syncErr) {
		t.Fatalf("err = %v, want the directory sync failure", err)
	}
}

func TestWriteFileAtomicNoReplaceRefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rigscope")
	if err := os.WriteFile(path, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomicNoReplace(path, []byte("ours"), 0o600, nil); err == nil {
		t.Fatal("existing file was overwritten")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "foreign" {
		t.Fatalf("foreign file changed: %q, %v", data, err)
	}
}

func TestWriteFileAtomicPreparedRunsBeforeRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	called := false
	err := WriteFileAtomicPrepared(path, []byte("data"), 0o600, func(file *os.File) error {
		called = true
		if filepath.Dir(file.Name()) != dir {
			t.Fatalf("prepared file %s is not in the target directory", file.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("prepare hook did not run")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "data" {
		t.Fatalf("file = %q, %v", data, err)
	}
}
