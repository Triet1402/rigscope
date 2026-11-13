package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func legacyHardwareFixture(t *testing.T) (*fixture, []string, *[]string) {
	t.Helper()
	f := newFixture(t)
	f.deps.Euid = func() int { return 0 }
	if err := os.MkdirAll(f.deps.hostPath("proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, parts := range [][]string{
		{"etc", "udev", "rules.d", "90-rigscope-rapl.rules"},
		{"etc", "udev", "rules.d", "90-rigscope-gpu-temps.rules"},
		{"etc", "sysusers.d", "rigscope-rapl.conf"},
	} {
		path := f.deps.hostPath(parts...)
		data, err := legacyHardwareTemplates.ReadFile("legacy-hardware/" + filepath.Base(path))
		if err != nil {
			t.Fatal(err)
		}
		writeLegacyFixtureFile(t, path, data)
		paths = append(paths, path)
	}
	commands := new([]string)
	f.deps.RunCommand = func(_ context.Context, name string, args ...string) error {
		*commands = append(*commands, name+" "+strings.Join(args, " "))
		return nil
	}
	return f, paths, commands
}

func writeLegacyFixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupLegacyHardwarePreviewApplyAndRetry(t *testing.T) {
	f, paths, commands := legacyHardwareFixture(t)
	f.deps.Euid = func() int { return 1000 }
	if code := f.run(t, "cleanup-legacy-hardware"); code != 0 {
		t.Fatalf("preview: %s", f.err.String())
	}
	if len(*commands) != 0 || !strings.Contains(f.out.String(), "Preview only") {
		t.Fatal("preview ran a command or did not identify itself")
	}
	if code := f.run(t, "cleanup-legacy-hardware", "--apply"); code == 0 {
		t.Fatal("non-root apply succeeded")
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preview/non-root apply removed %s", path)
		}
	}
	f.deps.Euid = func() int { return 0 }
	foreign := filepath.Join(filepath.Dir(paths[0]), "91-other.rules")
	writeLegacyFixtureFile(t, foreign, []byte("unrelated configuration\n"))
	f.deps.RunCommand = func(context.Context, string, ...string) error { return errors.New("reload failed") }
	if code := f.run(t, "cleanup-legacy-hardware", "--apply"); code == 0 {
		t.Fatal("reload failure reported success")
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("legacy file survived apply: %s: %v", path, err)
		}
	}
	f.deps.RunCommand = func(_ context.Context, name string, args ...string) error {
		*commands = append(*commands, name+" "+strings.Join(args, " "))
		return nil
	}
	if code := f.run(t, "cleanup-legacy-hardware", "--apply"); code != 0 {
		t.Fatalf("retry without remaining files: %s", f.err.String())
	}
	if len(*commands) != 1 || (*commands)[0] != "udevadm control --reload" {
		t.Fatalf("retry did not reload udev: %v", *commands)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "unrelated configuration\n" {
		t.Fatal("cleanup changed unrelated configuration")
	}
}

func TestCleanupLegacyHardwareRefusesUnsafeOrUsedFiles(t *testing.T) {
	for _, scenario := range []string{"modified", "symlink", "fifo", "symlink-parent", "changed-during-check", "parent-changed-during-check", "system-unit", "user-unit", "go-manifest", "go-process", "node-process", "node-bundle", "proc-unavailable", "runner-unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			f, paths, commands := legacyHardwareFixture(t)
			switch scenario {
			case "modified":
				writeLegacyFixtureFile(t, paths[2], []byte("# Managed by Rigscope.\ncustom configuration\n"))
			case "symlink":
				target := filepath.Join(f.deps.Root, "foreign")
				data, _ := os.ReadFile(paths[2])
				writeLegacyFixtureFile(t, target, data)
				if err := os.Remove(paths[2]); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, paths[2]); err != nil {
					t.Fatal(err)
				}
			case "symlink-parent":
				parent := filepath.Dir(paths[2])
				if err := os.Rename(parent, parent+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+"-real", parent); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(paths[2]); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(paths[2], 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed-during-check", "parent-changed-during-check":
				getenv := f.deps.Getenv
				changed := false
				f.deps.Getenv = func(key string) string {
					if !changed {
						changed = true
						if scenario == "parent-changed-during-check" {
							parent := filepath.Dir(paths[2])
							if err := os.Rename(parent, parent+"-old"); err != nil {
								t.Fatal(err)
							}
						}
						writeLegacyFixtureFile(t, paths[2], []byte("replacement\n"))
					}
					return getenv(key)
				}
			case "system-unit":
				writeLegacyFixtureFile(t, f.deps.hostPath("etc", "systemd", "system", "rigscope-collector.service"), []byte("unit\n"))
			case "user-unit":
				writeLegacyFixtureFile(t, filepath.Join(f.home, ".config", "systemd", "user", "rigscope-6629b1795b8f-collector.service"), []byte("unit\n"))
			case "go-manifest":
				writeLegacyFixtureFile(t, f.deps.hostPath("var", "lib", "rigscope", "install-manifest.json"), []byte("{}\n"))
			case "go-process", "node-process", "node-bundle":
				cmd := "/opt/rigscope/rigscope\x00collector\x00"
				if scenario == "node-process" {
					cmd = "node\x00/opt/rigscope/current/dist/hub/worker.js\x00"
				}
				if scenario == "node-bundle" {
					cmd = "node\x00/opt/rigscope/current/dist/collector.cjs\x00"
				}
				writeLegacyFixtureFile(t, f.deps.hostPath("proc", "999999", "cmdline"), []byte(cmd))
			case "proc-unavailable":
				if err := os.Remove(f.deps.hostPath("proc")); err != nil {
					t.Fatal(err)
				}
			case "runner-unavailable":
				f.deps.RunCommand = nil
			}
			if code := f.run(t, "cleanup-legacy-hardware", "--apply"); code == 0 {
				t.Fatalf("unsafe apply succeeded: %s", f.out.String())
			}
			// Even when the last candidate or process is unsafe, earlier files
			// must survive: validation finishes before the first deletion.
			for _, path := range paths {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("removed %s: %v", path, err)
				}
			}
			if len(*commands) != 0 {
				t.Fatalf("ran commands despite refusal: %v", *commands)
			}
		})
	}
}

// A rule written by Rigscope 1.x ("Managed by rigscope.") belongs to the
// current installation. Cleanup keeps it and still removes the 0.2 files; with
// nothing left from 0.2 the preview says so instead of refusing.
func TestCleanupLegacyHardwareKeepsCurrentFiles(t *testing.T) {
	f, paths, _ := legacyHardwareFixture(t)
	current := "# Managed by rigscope. Do not edit; run \"rigscope install --system\" to rewrite.\n"
	writeLegacyFixtureFile(t, paths[0], []byte(current))
	if code := f.run(t, "cleanup-legacy-hardware", "--apply"); code != 0 {
		t.Fatalf("apply: %s", f.err.String())
	}
	if !strings.Contains(f.out.String(), "kept: "+paths[0]) {
		t.Fatalf("stdout = %s", f.out.String())
	}
	if data, err := os.ReadFile(paths[0]); err != nil || string(data) != current {
		t.Fatalf("cleanup changed the 1.x rule: %v", err)
	}
	for _, path := range paths[1:] {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("legacy file survived apply: %s: %v", path, err)
		}
	}
	f.out.Reset()
	if code := f.run(t, "cleanup-legacy-hardware"); code != 0 {
		t.Fatalf("preview: %s", f.err.String())
	}
	if !strings.Contains(f.out.String(), "nothing to clean up") {
		t.Fatalf("stdout = %s", f.out.String())
	}
}
