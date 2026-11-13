package gpu

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestHelperRejectsArbitraryRootPathsBeforeSpawning reproduces the hole where
// "/tmp/devices/0000:01:00.0/resource0" passed the old shape check. Production
// accepts only the canonical sysfs symlink path.
func TestHelperRejectsArbitraryRootPathsBeforeSpawning(t *testing.T) {
	var launches int
	source := HelperSource{
		Executable: func() (string, error) { return os.Args[0], nil },
		slot:       &reapSlot{},
		launch: func(io.Writer, io.Writer) (childProcess, error) {
			launches++
			return nil, errors.New("must not launch")
		},
	}
	paths := []string{
		"/tmp/devices/0000:01:00.0/resource0",
		"/tmp/sys/bus/pci/devices/0000:01:00.0/resource0",
		"/var/sys/bus/pci/devices/0000:01:00.0/resource0",
		"/sys/devices/pci0000:00/0000:01:00.0/resource0",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			values, err := source.ReadRegisters(context.Background(), path, []int64{0})
			if err == nil {
				t.Fatal("err = nil, want a rejected path")
			}
			if len(values) != 1 || values[0] != nil {
				t.Fatalf("values = %v, want one nil", values)
			}
		})
	}
	if launches != 0 {
		t.Fatalf("launches = %d, want a rejected request to spawn nothing", launches)
	}
}

// TestResolveResourcePathEnforcesTheDeviceDirectory covers resolution itself:
// the real path must end in <bdf>/resource0 and stay inside the configured
// root, so a symlink cannot redirect a read to an unrelated file.
func TestResolveResourcePathEnforcesTheDeviceDirectory(t *testing.T) {
	root := t.TempDir()
	devices := filepath.Join(root, "sys", "bus", "pci", "devices")
	const bdf = "0000:01:00.0"
	deviceDir := filepath.Join(devices, bdf)
	resource := filepath.Join(deviceDir, "resource0")
	if err := os.MkdirAll(deviceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(resource, make([]byte, 8), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	resolved, err := resolveResourcePath(resource, root)
	if err != nil {
		t.Fatalf("resolve fixture: %v", err)
	}
	want, err := filepath.EvalSymlinks(resource)
	if err != nil || resolved != want {
		t.Fatalf("resolved = %q, want %q (%v)", resolved, want, err)
	}

	// The final component is a symlink to an unrelated file.
	const linked = "0000:02:00.0"
	linkedDir := filepath.Join(devices, linked)
	if err := os.MkdirAll(linkedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(linkedDir, "resource0")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := resolveResourcePath(filepath.Join(linkedDir, "resource0"), root); err == nil {
		t.Fatal("resolved a resource0 that points outside the device directory")
	}

	// The device directory itself is a symlink leaving the root.
	const escaped = "0000:03:00.0"
	outside := t.TempDir()
	outsideDevice := filepath.Join(outside, escaped)
	if err := os.MkdirAll(outsideDevice, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outsideDevice, "resource0"), make([]byte, 8), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(outsideDevice, filepath.Join(devices, escaped)); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := resolveResourcePath(filepath.Join(devices, escaped, "resource0"), root); err == nil {
		t.Fatal("resolved a device directory outside the fixture root")
	}

	// Without a fixture root the real path must be under /sys/devices.
	if _, err := resolveResourcePath(resource, ""); err == nil {
		t.Fatal("accepted a path outside the host /sys/devices tree")
	}
}
