package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

func legacyInstallFixture(t *testing.T) (*fixture, config.Paths, []byte) {
	t.Helper()
	f := newFixture(t)
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
	paths := f.paths(t, config.ScopeSystem)
	data, err := legacyHardwareTemplates.ReadFile("legacy-hardware/90-rigscope-rapl.rules")
	if err != nil {
		t.Fatal(err)
	}
	writeLegacyFixtureFile(t, paths.UdevRAPLFile, data)
	return f, paths, data
}

func TestInstallReplacesDefaultLegacyRuleAndOwnsReplacement(t *testing.T) {
	f, paths, _ := legacyInstallFixture(t)
	gpu, err := legacyHardwareTemplates.ReadFile("legacy-hardware/90-rigscope-gpu-temps.rules")
	if err != nil {
		t.Fatal(err)
	}
	writeLegacyFixtureFile(t, paths.UdevGPUTempsFile, gpu)
	if code := f.run(t, "install", "--system", "--role", "collector", "--no-start"); code != 0 {
		t.Fatalf("install: %s", f.err.String())
	}
	if len(f.prompts) != 1 {
		t.Fatalf("prompts: %v", f.prompts)
	}
	data, err := os.ReadFile(paths.UdevRAPLFile)
	if err != nil || !systemd.IsManaged(string(data)) {
		t.Fatalf("replacement: %s %v", data, err)
	}
	m, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if !m.owned(paths.UdevRAPLFile, kindUdevRAPL) || m.entry(paths.UdevGPUTempsFile, kindUdevGPU) != nil {
		t.Fatalf("unexpected ownership: %+v", m.IntegrationFiles)
	}
	kept, err := os.ReadFile(paths.UdevGPUTempsFile)
	if err != nil || !bytes.Equal(kept, gpu) {
		t.Fatal("unused rule changed")
	}
	if !strings.Contains(f.out.String(), "replace recognized default 0.2 rule") || !strings.Contains(f.out.String(), "keep unused default 0.2 rule") {
		t.Fatalf("preview: %s", f.out.String())
	}
	f.resetCalls()
	if code := f.run(t, "install", "--system", "--role", "collector", "--no-start", "--yes"); code != 0 {
		t.Fatalf("retry: %s", f.err.String())
	}
	if len(f.prompts) != 0 {
		t.Fatalf("--yes prompted: %v", f.prompts)
	}
}

func TestInstallLegacyRuleDeclineAndCustomFileLeaveEverythingUntouched(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "decline", true: "custom"}[custom], func(t *testing.T) {
			f, paths, data := legacyInstallFixture(t)
			f.answer = false
			if custom {
				data = append(data, []byte("# local changes\n")...)
				writeLegacyFixtureFile(t, paths.UdevRAPLFile, data)
			}
			if code := f.run(t, "install", "--system", "--role", "collector", "--no-start"); code == 0 {
				t.Fatal("unexpected success")
			}
			got, err := os.ReadFile(paths.UdevRAPLFile)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("rule changed")
			}
			if _, err := os.Stat(paths.ManifestFile()); !os.IsNotExist(err) {
				t.Fatalf("manifest created: %v", err)
			}
			if len(f.fake.Methods()) != 0 { // status reads are allowed, mutations are not
				for _, method := range []string{"stop", "start", "restart", "enable", "disable"} {
					for _, c := range installComponents {
						if f.fake.HasCall(method, c.UnitName()) {
							t.Fatalf("service changed: %s", method)
						}
					}
				}
			}
		})
	}
}

func TestInstallLegacyRuleRechecksBeforePublishingAndRetries(t *testing.T) {
	f, paths, original := legacyInstallFixture(t)
	// Interrupt after durable staging, before publication, then retry normally.
	f.deps.PublishStep = func(path, phase string) error {
		if path == paths.UdevRAPLFile && phase == "staged" {
			return errors.New("interrupted")
		}
		return nil
	}
	if code := f.run(t, "install", "--system", "--role", "collector", "--no-start", "--yes"); code == 0 {
		t.Fatal("expected interruption")
	}
	data, _ := os.ReadFile(paths.UdevRAPLFile)
	if !bytes.Equal(data, original) {
		t.Fatal("rule published before durable staging")
	}
	f.deps.PublishStep = nil
	f.resetCalls()
	if code := f.run(t, "install", "--system", "--role", "collector", "--no-start", "--yes"); code != 0 {
		t.Fatalf("retry: %s", f.err.String())
	}
}

func TestInstallLegacyRuleRejectsSymlinkAndExistingLegacyInstallation(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "old-install", true: "symlink"}[symlink], func(t *testing.T) {
			f, paths, data := legacyInstallFixture(t)
			if symlink {
				target := filepath.Join(t.TempDir(), "rule")
				writeLegacyFixtureFile(t, target, data)
				if err := os.Remove(paths.UdevRAPLFile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, paths.UdevRAPLFile); err != nil {
					t.Fatal(err)
				}
			} else {
				writeLegacyFixtureFile(t, filepath.Join(paths.UnitDir, "rigscope.service"), []byte("old service"))
			}
			if code := f.run(t, "install", "--system", "--role", "collector", "--no-start", "--yes"); code == 0 {
				t.Fatal("unexpected success")
			}
			if _, err := os.Stat(paths.ManifestFile()); !os.IsNotExist(err) {
				t.Fatalf("manifest created: %v", err)
			}
		})
	}
}

func TestInstallLegacyRuleChangedAfterStagingIsNotOverwritten(t *testing.T) {
	f, paths, original := legacyInstallFixture(t)
	edited := append(append([]byte{}, original...), []byte("# operator edit\n")...)
	f.deps.PublishStep = func(path, phase string) error {
		if path == paths.UdevRAPLFile && phase == "staged" {
			return os.WriteFile(path, edited, 0o644)
		}
		return nil
	}
	if code := f.run(t, "install", "--system", "--role", "collector", "--no-start", "--yes"); code == 0 {
		t.Fatal("edited rule was overwritten")
	}
	got, err := os.ReadFile(paths.UdevRAPLFile)
	if err != nil || !bytes.Equal(got, edited) {
		t.Fatal("operator edit lost")
	}
	if !strings.Contains(f.err.String(), "changed during installation") {
		t.Fatalf("wrong error: %s", f.err.String())
	}
}
