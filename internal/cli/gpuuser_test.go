package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// fakeAccounts is a fixed passwd/group database. currentErr simulates a uid
// that the local database does not list, as cgo-free os/user would report for
// an NSS-only account.
type fakeAccounts struct {
	current    UserAccount
	currentErr error
	passwd     []UserAccount
	groups     []GroupRecord
}

func (f fakeAccounts) Current() (UserAccount, error) {
	if f.currentErr != nil {
		return UserAccount{}, f.currentErr
	}
	return f.current, nil
}
func (f fakeAccounts) Passwd() ([]UserAccount, error) { return f.passwd, nil }
func (f fakeAccounts) Groups() ([]GroupRecord, error) { return f.groups, nil }

// privateAccounts has a private primary group: name equals the user, no
// supplementary members and no other account with the GID as primary.
func privateAccounts() fakeAccounts {
	return fakeAccounts{
		current: UserAccount{Name: "alice", UID: 1000, GID: 1000},
		passwd: []UserAccount{
			{Name: "root", UID: 0, GID: 0},
			{Name: "alice", UID: 1000, GID: 1000},
		},
		groups: []GroupRecord{
			{Name: "root", GID: 0},
			{Name: "alice", GID: 1000},
			{Name: "rigscope-gpu", GID: 987},
		},
	}
}

// sharedAccounts has a shared primary GID and no rigscope-gpu group yet.
func sharedAccounts() fakeAccounts {
	return fakeAccounts{
		current: UserAccount{Name: "alice", UID: 1000, GID: 100},
		passwd: []UserAccount{
			{Name: "root", UID: 0, GID: 0},
			{Name: "alice", UID: 1000, GID: 100},
			{Name: "bob", UID: 1001, GID: 100},
		},
		groups: []GroupRecord{
			{Name: "root", GID: 0},
			{Name: "users", GID: 100, Members: []string{"bob"}},
		},
	}
}

// fakeSudo records every sudo command. It installs the rule file like the real
// install command would, so the manifest identity checks see a real file.
type fakeSudo struct {
	calls [][]string
	fail  bool
	allow func()
}

func (s *fakeSudo) run(_ context.Context, name string, args ...string) error {
	s.calls = append(s.calls, append([]string{name}, args...))
	if s.fail {
		return errors.New("sudo: a password is required")
	}
	switch name {
	case "install":
		// install -m 0644 -o root -g root <temp> <rule>
		if len(args) == 8 {
			data, err := os.ReadFile(args[6])
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(args[7]), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(args[7], data, 0o644); err != nil {
				return err
			}
		}
	case "rm":
		if len(args) == 2 {
			if err := os.Remove(args[1]); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if s.allow != nil {
		s.allow()
	}
	return nil
}

func (s *fakeSudo) joined() string {
	var lines []string
	for _, call := range s.calls {
		lines = append(lines, strings.Join(call, " "))
	}
	return strings.Join(lines, "\n")
}

// gpuFixture is a user-scope fixture with an isolated /etc and /sys tree.
type gpuFixture struct {
	*fixture
	sudo     *fakeSudo
	granted  bool
	readable bool
}

// writeFakeSysfs creates NVIDIA display functions under <root>/sys/bus/pci.
func writeFakeSysfs(t *testing.T, root string, names ...string) {
	t.Helper()
	dir := filepath.Join(root, "sys", "bus", "pci", "devices")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		device := filepath.Join(dir, name)
		if err := os.MkdirAll(device, 0o755); err != nil {
			t.Fatal(err)
		}
		for attribute, value := range map[string]string{
			"vendor": "0x10de\n",
			"class":  "0x030000\n",
			"device": "0x2684\n",
		} {
			if err := os.WriteFile(filepath.Join(device, attribute), []byte(value), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(device, "resource0"), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func newGPUFix(t *testing.T, accounts AccountDatabase, devices ...string) *gpuFixture {
	t.Helper()
	// Keep the rendered rule files inside the test's temporary tree; the
	// no-terminal and declined-sudo paths deliberately leave them behind for
	// the printed command.
	t.Setenv("TMPDIR", t.TempDir())
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Accounts = accounts
	if len(devices) > 0 {
		writeFakeSysfs(t, f.deps.Root, devices...)
	}
	sudo := &fakeSudo{}
	gpu := &gpuFixture{fixture: f, sudo: sudo}
	sudo.allow = func() { gpu.granted = true }
	f.deps.RunSudo = sudo.run
	f.deps.StdinIsTerminal = func() bool { return true }
	f.deps.ProbeRegisterAccess = func(path string) error {
		if !gpu.granted || !gpu.readable {
			return os.ErrPermission
		}
		return nil
	}
	return gpu
}

func (f *gpuFixture) userPaths(t *testing.T) config.Paths {
	t.Helper()
	return f.paths(t, config.ScopeUser)
}

func TestPrivatePrimaryGroupDecision(t *testing.T) {
	base := func() fakeAccounts { return privateAccounts() }
	cases := []struct {
		name string
		edit func(*fakeAccounts)
		want bool
	}{
		{
			name: "private",
			edit: func(*fakeAccounts) {},
			want: true,
		},
		{
			name: "group name differs from user",
			edit: func(f *fakeAccounts) { f.groups[1].Name = "alice-private" },
			want: false,
		},
		{
			name: "group lists another member",
			edit: func(f *fakeAccounts) { f.groups[1].Members = []string{"bob"} },
			want: false,
		},
		{
			name: "another account shares the primary gid",
			edit: func(f *fakeAccounts) {
				f.passwd = append(f.passwd, UserAccount{Name: "bob", UID: 1001, GID: 1000})
			},
			want: false,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			accounts := base()
			test.edit(&accounts)
			group, ok := privatePrimaryGroup(accounts)
			if ok != test.want {
				t.Fatalf("private = %v, want %v (group %q)", ok, test.want, group)
			}
			if ok && group != "alice" {
				t.Fatalf("group = %q, want alice", group)
			}
		})
	}
}

func TestInstallUserScopeGPURegistersPrivateGroup(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.userPaths(t)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Components.Collector.Hardware.GPURegisters {
		t.Fatalf("gpuRegisters not enabled:\n%s", f.out.String())
	}
	content, err := os.ReadFile(paths.UdevGPUTempsFile)
	if err != nil {
		t.Fatalf("rule file: %v", err)
	}
	if !systemd.IsManaged(string(content)) || !strings.Contains(string(content), "# Group: alice\n") ||
		!strings.Contains(string(content), "chgrp alice /sys%p/resource0") {
		t.Fatalf("rule content:\n%s", content)
	}
	commands := f.sudo.joined()
	for _, wanted := range []string{
		"install -m 0644 -o root -g root ",
		"udevadm control --reload",
		"udevadm trigger --subsystem-match=pci --attr-match=vendor=0x10de",
		"udevadm settle",
	} {
		if !strings.Contains(commands, wanted) {
			t.Fatalf("missing command %q:\n%s", wanted, commands)
		}
	}
	if strings.Contains(commands, "usermod") || strings.Contains(commands, "groupadd") {
		t.Fatalf("private group install ran group commands:\n%s", commands)
	}
	if !strings.Contains(f.out.String(), "sudo install -m 0644 -o root -g root") {
		t.Fatalf("install did not print the commands:\n%s", f.out.String())
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU)
	if entry == nil || entry.State != manifestOwned {
		t.Fatalf("manifest entry = %+v", entry)
	}
}

func TestInstallUserScopeGPUAdoptsLegacySharedRuleWithoutRewriting(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	paths := f.userPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.UdevGPUTempsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.UdevGPUTempsFile, []byte(legacyGPURule()), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	// A rule for the shared group belongs to the group, not to this user:
	// never rewrite it to the private group.
	content, err := os.ReadFile(paths.UdevGPUTempsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != legacyGPURule() {
		t.Fatalf("shared rule was rewritten:\n%s", content)
	}
	commands := f.sudo.joined()
	if !strings.Contains(commands, "usermod -aG rigscope-gpu alice") {
		t.Fatalf("shared membership was not ensured:\n%s", commands)
	}
	if strings.Contains(commands, "install -m") || strings.Contains(commands, "udevadm trigger") {
		t.Fatalf("adopted shared rule was reinstalled:\n%s", commands)
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU); entry == nil || entry.State != manifestAdopted {
		t.Fatalf("shared rule manifest entry = %+v", entry)
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Components.Collector.Hardware.GPURegisters {
		t.Fatalf("adopted shared rule did not enable register sampling:\n%s", f.out.String())
	}
	if !strings.Contains(f.out.String(), "using the existing shared-group rule") {
		t.Fatalf("adoption not reported:\n%s", f.out.String())
	}
}

func TestInstallUserScopeGPURefusesAnotherUsersGroup(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	f.granted = true
	paths := f.userPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.UdevGPUTempsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := systemd.RenderGPUTempsRule("bob")
	if err := os.WriteFile(paths.UdevGPUTempsFile, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("refusal must not fail the install: exit %d: %s", code, f.err.String())
	}
	content, err := os.ReadFile(paths.UdevGPUTempsFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != foreign {
		t.Fatalf("foreign rule was rewritten:\n%s", content)
	}
	if len(f.sudo.calls) != 0 {
		t.Fatalf("refusal ran sudo: %v", f.sudo.calls)
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Components.Collector.Hardware.GPURegisters {
		t.Fatal("foreign rule enabled register sampling")
	}
	if !strings.Contains(f.out.String(), "belongs to another account") || !strings.Contains(f.out.String(), "gpu registers: not enabled") {
		t.Fatalf("refusal not explained:\n%s", f.out.String())
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU); entry == nil || entry.State != manifestPlanned {
		t.Fatalf("foreign rule manifest entry = %+v", entry)
	}
}

func TestInstallUserScopeGPUClaimsHandInstalledPrivateRule(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	f.granted = true
	paths := f.userPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.UdevGPUTempsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	rendered := systemd.RenderGPUTempsRule("alice")
	if err := os.WriteFile(paths.UdevGPUTempsFile, []byte(rendered), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if len(f.sudo.calls) != 0 {
		t.Fatalf("identical rule install ran sudo: %v", f.sudo.calls)
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU); entry == nil || entry.State != manifestOwned {
		t.Fatalf("hand-installed private rule entry = %+v", entry)
	}
}

func TestInstallUserScopeGPURegistersSharedGroupFallback(t *testing.T) {
	f := newGPUFix(t, sharedAccounts(), "0000:01:00.0")
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.userPaths(t)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Components.Collector.Hardware.GPURegisters {
		t.Fatal("shared-group fallback enabled register sampling before the log-out")
	}
	content, err := os.ReadFile(paths.UdevGPUTempsFile)
	if err != nil {
		t.Fatalf("rule file: %v", err)
	}
	if !strings.Contains(string(content), "chgrp rigscope-gpu /sys%p/resource0") {
		t.Fatalf("rule content:\n%s", content)
	}
	commands := f.sudo.joined()
	for _, wanted := range []string{
		"groupadd --system rigscope-gpu",
		"usermod -aG rigscope-gpu alice",
		"install -m 0644 -o root -g root ",
	} {
		if !strings.Contains(commands, wanted) {
			t.Fatalf("missing command %q:\n%s", wanted, commands)
		}
	}
	if !strings.Contains(f.out.String(), "Log out fully") || !strings.Contains(f.out.String(), "rigscope install --gpu-registers") {
		t.Fatalf("log-out guidance missing:\n%s", f.out.String())
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	// A shared rule belongs to every account in the group, so even the install
	// that wrote it records it as adopted and uninstall leaves it in place.
	if entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU); entry == nil || entry.State != manifestAdopted {
		t.Fatalf("shared rule not recorded as adopted: %+v", entry)
	}
	f.sudo.calls = nil
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); err != nil {
		t.Fatalf("shared rule written by a user install was deleted: %v", err)
	}
	if len(f.sudo.calls) != 0 {
		t.Fatalf("uninstall ran sudo for a shared rule: %v", f.sudo.calls)
	}
	if !strings.Contains(f.out.String(), "left in place") {
		t.Fatalf("uninstall did not explain the shared rule:\n%s", f.out.String())
	}
}

func TestReinstallDemotesPrivateRuleRewrittenForSharedGroup(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("first install exit %d: %s", code, f.err.String())
	}
	paths := f.userPaths(t)
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU); entry == nil || entry.State != manifestOwned {
		t.Fatalf("private rule not owned after the first install: %+v", entry)
	}

	// A system install rewrites the same path for the shared group.
	if err := os.WriteFile(paths.UdevGPUTempsFile, []byte(systemd.RenderGPUTempsRule(systemd.GPUGroup)), 0o644); err != nil {
		t.Fatal(err)
	}
	f.sudo.calls = nil
	f.out.Reset()
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("reinstall exit %d: %s", code, f.err.String())
	}
	loaded, err = loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU); entry == nil || entry.State != manifestAdopted {
		t.Fatalf("rewritten rule was not demoted to adopted: %+v", entry)
	}

	f.sudo.calls = nil
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); err != nil {
		t.Fatalf("uninstall deleted the shared rule: %v", err)
	}
	if len(f.sudo.calls) != 0 {
		t.Fatalf("uninstall ran sudo for the demoted rule: %v", f.sudo.calls)
	}
	if !strings.Contains(f.out.String(), "left in place") {
		t.Fatalf("uninstall did not explain the demoted rule:\n%s", f.out.String())
	}
}

func TestUninstallLeavesModifiedOwnedGPURule(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.userPaths(t)
	content, err := os.ReadFile(paths.UdevGPUTempsFile)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the managed marker so only the digest differs.
	modified := append(append([]byte{}, content...), []byte("# local edit\n")...)
	if err := os.WriteFile(paths.UdevGPUTempsFile, modified, 0o644); err != nil {
		t.Fatal(err)
	}
	f.sudo.calls = nil
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); err != nil {
		t.Fatalf("uninstall deleted a modified rule: %v", err)
	}
	if len(f.sudo.calls) != 0 {
		t.Fatalf("uninstall ran sudo for a modified rule: %v", f.sudo.calls)
	}
	if !strings.Contains(f.out.String(), "content changed since install recorded it") {
		t.Fatalf("uninstall did not explain the modified rule:\n%s", f.out.String())
	}
}

// sharedGroupAccounts has a shared primary group and an existing rigscope-gpu
// group whose membership the test controls.
func sharedGroupAccounts(members ...string) fakeAccounts {
	accounts := sharedAccounts()
	accounts.groups = append(accounts.groups, GroupRecord{Name: "rigscope-gpu", GID: 987, Members: members})
	return accounts
}

func TestSharedPlanAddsMissingMembershipOnly(t *testing.T) {
	install := func(t *testing.T, accounts fakeAccounts) *gpuFixture {
		t.Helper()
		f := newGPUFix(t, accounts, "0000:01:00.0")
		f.readable = true
		f.granted = true
		paths := f.userPaths(t)
		if err := os.MkdirAll(filepath.Dir(paths.UdevGPUTempsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.UdevGPUTempsFile, []byte(legacyGPURule()), 0o644); err != nil {
			t.Fatal(err)
		}
		f.sudo.calls = nil
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		return f
	}

	t.Run("not a member", func(t *testing.T) {
		f := install(t, sharedGroupAccounts())
		if !strings.Contains(f.sudo.joined(), "usermod -aG rigscope-gpu alice") {
			t.Fatalf("missing membership was not added:\n%s", f.sudo.joined())
		}
		if strings.Contains(f.sudo.joined(), "groupadd") {
			t.Fatalf("groupadd ran although the group exists:\n%s", f.sudo.joined())
		}
	})

	t.Run("already a member", func(t *testing.T) {
		f := install(t, sharedGroupAccounts("alice"))
		if len(f.sudo.calls) != 0 {
			t.Fatalf("reinstall on an existing member ran sudo: %v", f.sudo.calls)
		}
	})
}

func TestInstallUserScopeGPUEnvFallbackUsesSharedGroup(t *testing.T) {
	accounts := sharedAccounts()
	accounts.currentErr = errors.New("uid 1000 is not listed in /etc/passwd")
	f := newGPUFix(t, accounts, "0000:01:00.0")
	f.deps.Getenv = func(key string) string {
		if key == "USER" {
			return "carol"
		}
		return ""
	}
	f.readable = true
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.userPaths(t)
	content, err := os.ReadFile(paths.UdevGPUTempsFile)
	if err != nil {
		t.Fatalf("rule file: %v", err)
	}
	if !strings.Contains(string(content), "chgrp rigscope-gpu /sys%p/resource0") {
		t.Fatalf("env fallback did not use the shared group:\n%s", content)
	}
	commands := f.sudo.joined()
	if !strings.Contains(commands, "usermod -aG rigscope-gpu carol") {
		t.Fatalf("env fallback did not add the account: %v", f.sudo.calls)
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU); entry == nil || entry.State != manifestAdopted {
		t.Fatalf("shared env-fallback rule entry = %+v", entry)
	}
}

func TestInstallUserScopeGPURegistersNoTerminal(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.deps.StdinIsTerminal = func() bool { return false }
	f.readable = true
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.userPaths(t)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Components.Collector.Hardware.GPURegisters {
		t.Fatal("no-terminal install enabled register sampling")
	}
	if len(f.sudo.calls) != 0 {
		t.Fatalf("no-terminal install ran sudo: %v", f.sudo.calls)
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); !os.IsNotExist(err) {
		t.Fatalf("no-terminal install wrote the rule: %v", err)
	}
	kept := filepath.Join(paths.StateDir, userGPURuleDraftName)
	content, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("rendered rule was not kept in the state directory: %v", err)
	}
	if !systemd.IsManaged(string(content)) || !strings.Contains(string(content), "chgrp alice ") {
		t.Fatalf("kept rule:\n%s", content)
	}
	for _, wanted := range []string{
		"sudo install -m 0644 -o root -g root ",
		"sudo udevadm control --reload",
		"sudo udevadm trigger --subsystem-match=pci --attr-match=vendor=0x10de",
		"sudo udevadm settle",
		"run them by hand",
		kept,
	} {
		if !strings.Contains(f.out.String(), wanted) {
			t.Fatalf("output missing %q:\n%s", wanted, f.out.String())
		}
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU); entry == nil || entry.State != manifestPlanned {
		t.Fatalf("manifest entry = %+v", entry)
	}
}

func TestInstallUserScopeGPURegistersSudoDeclined(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.sudo.fail = true
	f.readable = true
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("declined sudo must not fail the install: exit %d: %s", code, f.err.String())
	}
	paths := f.userPaths(t)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Components.Collector.Hardware.GPURegisters {
		t.Fatal("declined sudo enabled register sampling")
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); !os.IsNotExist(err) {
		t.Fatalf("declined sudo wrote the rule: %v", err)
	}
	if !strings.Contains(f.out.String(), "sudo install -m 0644") || !strings.Contains(f.out.String(), "run the commands above by hand") {
		t.Fatalf("declined sudo output:\n%s", f.out.String())
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU); entry == nil || entry.State != manifestPlanned {
		t.Fatalf("manifest entry = %+v", entry)
	}
}

// legacyGPURule is the shape a 0.2 user installation left behind: the older
// managed marker and the shared group.
func legacyGPURule() string {
	return `# Managed by Rigscope: generated by packaging/system/90-rigscope-gpu-temps.rules
ACTION=="add|change", SUBSYSTEM=="pci", ATTR{vendor}=="0x10de", ATTR{class}=="0x03[0-9a-f]*", \
  RUN+="/usr/bin/chgrp rigscope-gpu /sys%p/resource0", \
  RUN+="/usr/bin/chmod 0440 /sys%p/resource0"
`
}

func TestInstallUserScopeGPUAdoptsWorkingRule(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	f.granted = true
	f.sudo.fail = true
	paths := f.userPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.UdevGPUTempsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.UdevGPUTempsFile, []byte(legacyGPURule()), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Components.Collector.Hardware.GPURegisters {
		t.Fatalf("adopted rule did not enable register sampling:\n%s", f.out.String())
	}
	// The shared membership command is attempted even when sudo is declined;
	// the readable rule still proves this session's access.
	if !strings.Contains(f.sudo.joined(), "usermod -aG rigscope-gpu alice") {
		t.Fatalf("shared membership was not attempted: %v", f.sudo.calls)
	}
	if !strings.Contains(f.out.String(), "using the existing rule") {
		t.Fatalf("adoption not reported:\n%s", f.out.String())
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU)
	if entry == nil || entry.State != manifestAdopted {
		t.Fatalf("adopted manifest entry = %+v", entry)
	}
}

func TestInstallUserScopeGPUSharedRuleWaitsForUserManager(t *testing.T) {
	accounts := sharedAccounts()
	accounts.groups = append(accounts.groups, GroupRecord{Name: "rigscope-gpu", GID: 987})

	t.Run("manager without the group", func(t *testing.T) {
		f := newGPUFix(t, accounts, "0000:01:00.0")
		f.readable = true
		f.granted = true
		f.deps.UserManagerGroups = func() ([]int, error) { return []int{100}, nil }
		f.deps.LingerEnabled = func(user string) bool { return true }
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		cfg, err := config.Load(f.userPaths(t).ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Components.Collector.Hardware.GPURegisters {
			t.Fatal("gpuRegisters was enabled although the user manager lacks the group")
		}
		for _, wanted := range []string{"systemd --user manager does not have the rigscope-gpu group", "Log out of all sessions", "loginctl terminate-user alice"} {
			if !strings.Contains(f.out.String(), wanted) {
				t.Fatalf("output missing %q:\n%s", wanted, f.out.String())
			}
		}
	})

	t.Run("manager with the group", func(t *testing.T) {
		f := newGPUFix(t, accounts, "0000:01:00.0")
		f.readable = true
		f.granted = true
		f.deps.UserManagerGroups = func() ([]int, error) { return []int{100, 987}, nil }
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		cfg, err := config.Load(f.userPaths(t).ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Components.Collector.Hardware.GPURegisters {
			t.Fatalf("gpuRegisters stayed off although the user manager has the group:\n%s", f.out.String())
		}
	})

	t.Run("manager unknown warns but enables a successful probe", func(t *testing.T) {
		f := newGPUFix(t, accounts, "0000:01:00.0")
		f.readable = true
		f.granted = true
		// No UserManagerGroups and no /proc below the fixture root: the
		// manager cannot be found, so the session cannot be checked.
		f.deps.UserManagerGroups = nil
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		cfg, err := config.Load(f.userPaths(t).ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Components.Collector.Hardware.GPURegisters {
			t.Fatalf("a successful probe did not enable sampling:\n%s", f.out.String())
		}
		if !strings.Contains(f.out.String(), "could not check whether the collector's systemd --user session carries") {
			t.Fatalf("missing session warning:\n%s", f.out.String())
		}
	})
}

func TestInstallUserScopeGPUNSSFallsBackToSharedGroup(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	nsswitch := filepath.Join(f.deps.Root, "etc", "nsswitch.conf")
	if err := os.MkdirAll(filepath.Dir(nsswitch), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nsswitch, []byte("passwd: files ldap\ngroup: files ldap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.readable = true
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.userPaths(t)
	content, err := os.ReadFile(paths.UdevGPUTempsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "chgrp rigscope-gpu /sys%p/resource0") {
		t.Fatalf("NSS install did not use the shared group:\n%s", content)
	}
	if !strings.Contains(f.sudo.joined(), "usermod -aG rigscope-gpu alice") {
		t.Fatalf("NSS install did not join the shared group:\n%s", f.sudo.joined())
	}
}

func TestDefaultSudoRunnerUsesDoubleDash(t *testing.T) {
	var got *exec.Cmd
	err := defaultRunSudo(context.Background(), func(cmd *exec.Cmd) error {
		got = cmd
		return nil
	}, "install", "-m", "0644", "/tmp/a b/rule", "/etc/udev/rules.d/90-rigscope-gpu-temps.rules")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("the sudo runner did not build a command")
	}
	want := []string{"sudo", "--", "install", "-m", "0644", "/tmp/a b/rule", "/etc/udev/rules.d/90-rigscope-gpu-temps.rules"}
	if len(got.Args) != len(want) {
		t.Fatalf("argv = %v, want %v", got.Args, want)
	}
	for i := range want {
		if got.Args[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got.Args, want)
		}
	}
	if got.Stdin != os.Stdin || got.Stdout != os.Stdout || got.Stderr != os.Stderr {
		t.Fatal("the default sudo runner must attach the caller's terminal")
	}
}

func TestDefaultDepsWiresTheSudoRunner(t *testing.T) {
	deps := DefaultDeps(io.Discard, io.Discard)
	if deps.RunSudo == nil {
		t.Fatal("DefaultDeps did not wire RunSudo")
	}
	if deps.ProbeRegisterAccess == nil || deps.ReadAttribute == nil {
		t.Fatal("DefaultDeps did not wire the hardware probe seams")
	}
}

func TestPrintRootCommandsQuotesPaths(t *testing.T) {
	var out bytes.Buffer
	printRootCommands(Deps{Stdout: &out}, [][]string{
		{"install", "-m", "0644", "/tmp/a b/$rule", "/etc/udev/rules.d/90-rigscope-gpu-temps.rules"},
	})
	want := "  sudo install -m 0644 '/tmp/a b/$rule' /etc/udev/rules.d/90-rigscope-gpu-temps.rules\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestProbeRegisterAccessDefaultMapsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resource0")
	data := make([]byte, 4096)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Deps{}).probeRegisterAccess(path); err != nil {
		t.Fatalf("probe the file: %v", err)
	}
	if err := (Deps{}).probeRegisterAccess(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error = %v, want ENOENT", err)
	}
}

func TestDoctorSystemScopeGPURequiresCollectorGroup(t *testing.T) {
	f := newFixture(t)
	f.deps.Euid = func() int { return 0 }
	f.deps.FileOwner = func(string) (int, int, error) { return 0, 0, nil }
	f.deps.UserID = func(string) (int, error) { return 0, nil }
	f.deps.GroupID = func(string) (int, error) { return 0, nil }
	f.deps.ProbeRegisterAccess = func(string) error { return nil }
	writeFakeSysfs(t, f.deps.Root, "0000:01:00.0")
	if code := f.run(t, "install", "--system", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.paths(t, config.ScopeSystem)
	f.out.Reset()
	if code := f.run(t, "doctor", "--system"); code != 0 {
		t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
	}
	if !strings.Contains(f.out.String(), "readable by") ||
		!strings.Contains(f.out.String(), "the collector unit joins group rigscope-gpu") {
		t.Fatalf("doctor did not attribute system GPU access to the collector:\n%s", f.out.String())
	}

	// Root readability alone is not collector access.
	unitPath := paths.UnitPath(systemd.Collector.UnitName())
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	stripped := strings.ReplaceAll(string(unit), "SupplementaryGroups=rigscope-rapl rigscope-gpu", "")
	if err := os.WriteFile(unitPath, []byte(stripped), 0o644); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if code := f.run(t, "doctor", "--system"); code != 0 {
		t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
	}
	if !strings.Contains(f.out.String(), "[warn] GPU register temperatures:") ||
		!strings.Contains(f.out.String(), "SupplementaryGroups") {
		t.Fatalf("doctor claimed collector access without the group:\n%s", f.out.String())
	}
}

func TestRoleSwitchKeepsAdoptedGPURuleEntry(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.userPaths(t)
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Components.Collector.Enabled = false
	cfg.Components.Collector.Hardware.GPURegisters = false
	if err := config.Save(paths.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "install", "--role", "hub", "--no-start"); code != 0 {
		t.Fatalf("role switch exit %d: %s", code, f.err.String())
	}
	loaded, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		t.Fatal(err)
	}
	entry := loaded.entry(paths.UdevGPUTempsFile, kindUdevGPU)
	if entry == nil || (entry.State != manifestOwned && entry.State != manifestAdopted) {
		t.Fatalf("GPU rule entry was orphaned by the role switch: %+v", entry)
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); err != nil {
		t.Fatalf("role switch removed the GPU rule: %v", err)
	}
}

func TestInstallUserScopeGPURegistersReinstallIsIdempotent(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("first install exit %d: %s", code, f.err.String())
	}
	f.sudo.calls = nil
	f.sudo.fail = true
	f.out.Reset()
	f.err.Reset()
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("reinstall exit %d: %s", code, f.err.String())
	}
	if len(f.sudo.calls) != 0 {
		t.Fatalf("idempotent reinstall ran sudo: %v", f.sudo.calls)
	}
	cfg, err := config.Load(f.userPaths(t).ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Components.Collector.Hardware.GPURegisters {
		t.Fatal("reinstall disabled a working GPU rule")
	}
}

func TestUninstallUserScopeRemovesOwnedGPURule(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.userPaths(t)
	f.sudo.calls = nil
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); !os.IsNotExist(err) {
		t.Fatalf("owned rule still exists: %v", err)
	}
	commands := f.sudo.joined()
	if !strings.Contains(commands, "rm -f "+paths.UdevGPUTempsFile) || !strings.Contains(commands, "udevadm control --reload") {
		t.Fatalf("uninstall commands:\n%s", commands)
	}
	if !strings.Contains(f.out.String(), "gpu rule: removed") {
		t.Fatalf("uninstall output:\n%s", f.out.String())
	}
}

func TestUninstallUserGPURuleCleanupFailureCanBeRetried(t *testing.T) {
	for _, failure := range []string{"no-terminal", "sudo", "reload"} {
		t.Run(failure, func(t *testing.T) {
			f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
			f.readable = true
			if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
				t.Fatalf("install exit %d: %s", code, f.err.String())
			}
			paths := f.userPaths(t)
			f.deps.StdinIsTerminal = func() bool { return failure != "no-terminal" }
			f.deps.RunSudo = func(ctx context.Context, name string, args ...string) error {
				if failure == "sudo" || failure == "reload" && name == "udevadm" {
					return errors.New("hardware cleanup failed")
				}
				return f.sudo.run(ctx, name, args...)
			}
			if code := f.run(t, "uninstall"); code == 0 {
				t.Fatal("uninstall reported success despite incomplete owned GPU rule cleanup")
			}
			for _, path := range []string{paths.Binary, paths.ConfigFile, paths.ManifestFile()} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("retry needs %s: %v", path, err)
				}
			}
			if failure == "reload" {
				if _, err := os.Stat(paths.UdevGPUTempsFile); !os.IsNotExist(err) {
					t.Fatalf("rule should already have been removed before the reload failed: %v", err)
				}
			}
			f.deps.StdinIsTerminal = func() bool { return true }
			f.deps.RunSudo = f.sudo.run
			f.sudo.calls = nil
			if code := f.run(t, "uninstall"); code != 0 {
				t.Fatalf("retry exit %d: %s", code, f.err.String())
			}
			if !strings.Contains(f.sudo.joined(), "udevadm control --reload") {
				t.Fatal("retry did not reload udev, including after an already-removed rule")
			}
			for _, path := range []string{paths.UdevGPUTempsFile, paths.Binary, paths.ManifestFile()} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("retry left %s: %v", path, err)
				}
			}
		})
	}
}

func TestUninstallUserScopeLeavesAdoptedGPURule(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
	f.readable = true
	f.granted = true
	f.sudo.fail = true
	paths := f.userPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.UdevGPUTempsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.UdevGPUTempsFile, []byte(legacyGPURule()), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	f.sudo.calls = nil
	if code := f.run(t, "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d: %s", code, f.err.String())
	}
	if _, err := os.Stat(paths.UdevGPUTempsFile); err != nil {
		t.Fatalf("adopted rule was deleted: %v", err)
	}
	if len(f.sudo.calls) != 0 {
		t.Fatalf("uninstall ran sudo for an adopted rule: %v", f.sudo.calls)
	}
	if !strings.Contains(f.out.String(), "left in place") || !strings.Contains(f.out.String(), "sudo rm -f") {
		t.Fatalf("uninstall output:\n%s", f.out.String())
	}
}

func TestDoctorUserScopeGPUStates(t *testing.T) {
	t.Run("working", func(t *testing.T) {
		f := newGPUFix(t, privateAccounts(), "0000:01:00.0", "0000:02:00.0")
		f.readable = true
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		f.out.Reset()
		if code := f.run(t, "doctor"); code != 0 {
			t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
		}
		if !strings.Contains(f.out.String(), "[ok] GPU register temperatures: working: 2 GPU(s) readable") {
			t.Fatalf("doctor output:\n%s", f.out.String())
		}
	})

	t.Run("adopted legacy rule", func(t *testing.T) {
		f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
		f.readable = true
		f.granted = true
		f.deps.UserManagerGroups = func() ([]int, error) { return []int{1000, 987}, nil }
		paths := f.userPaths(t)
		if err := os.MkdirAll(filepath.Dir(paths.UdevGPUTempsFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths.UdevGPUTempsFile, []byte(legacyGPURule()), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		f.out.Reset()
		if code := f.run(t, "doctor"); code != 0 {
			t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
		}
		if !strings.Contains(f.out.String(), "[ok] GPU register temperatures: working: 1 GPU(s) readable") ||
			!strings.Contains(f.out.String(), "user manager carries group rigscope-gpu") {
			t.Fatalf("doctor output:\n%s", f.out.String())
		}
	})

	t.Run("missing rule", func(t *testing.T) {
		f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
		f.readable = true
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		if err := os.Remove(f.userPaths(t).UdevGPUTempsFile); err != nil {
			t.Fatal(err)
		}
		f.out.Reset()
		if code := f.run(t, "doctor"); code != 1 {
			t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
		}
		if !strings.Contains(f.out.String(), "[fail] GPU register temperatures: ") ||
			!strings.Contains(f.out.String(), "is missing; run rigscope install --gpu-registers") {
			t.Fatalf("doctor output:\n%s", f.out.String())
		}
	})

	t.Run("permission denied", func(t *testing.T) {
		f := newGPUFix(t, sharedAccounts(), "0000:01:00.0")
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		// The install left sampling off; this models a working installation
		// whose access was revoked later.
		paths := f.userPaths(t)
		cfg, err := config.Load(paths.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Components.Collector.Hardware.GPURegisters = true
		if err := config.Save(paths.ConfigFile, cfg); err != nil {
			t.Fatal(err)
		}
		f.out.Reset()
		if code := f.run(t, "doctor"); code != 0 {
			t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
		}
		if !strings.Contains(f.out.String(), "[warn] GPU register temperatures: ") ||
			!strings.Contains(f.out.String(), "log out fully") ||
			!strings.Contains(f.out.String(), "rigscope-gpu") {
			t.Fatalf("doctor output:\n%s", f.out.String())
		}
	})

	t.Run("rule present but sampling disabled", func(t *testing.T) {
		f := newGPUFix(t, sharedAccounts(), "0000:01:00.0")
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		f.out.Reset()
		if code := f.run(t, "doctor"); code != 0 {
			t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
		}
		if !strings.Contains(f.out.String(), "[skip] GPU register temperatures: register sampling is off") ||
			!strings.Contains(f.out.String(), "run `rigscope install --gpu-registers` to turn sampling back on") ||
			strings.Contains(f.out.String(), "working:") {
			t.Fatalf("doctor output:\n%s", f.out.String())
		}
	})

	t.Run("shared manager lacks the group", func(t *testing.T) {
		f := newGPUFix(t, sharedGroupAccounts("alice"), "0000:01:00.0")
		f.readable = true
		f.granted = true
		f.deps.UserManagerGroups = func() ([]int, error) { return []int{100, 987}, nil }
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		f.deps.UserManagerGroups = func() ([]int, error) { return []int{100}, nil }
		f.out.Reset()
		if code := f.run(t, "doctor"); code != 0 {
			t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
		}
		if !strings.Contains(f.out.String(), "[warn] GPU register temperatures: ") ||
			!strings.Contains(f.out.String(), "collector's session lacks the rigscope-gpu group") {
			t.Fatalf("doctor output:\n%s", f.out.String())
		}
	})

	t.Run("manager unknown", func(t *testing.T) {
		f := newGPUFix(t, sharedGroupAccounts("alice"), "0000:01:00.0")
		f.readable = true
		f.granted = true
		f.deps.UserManagerGroups = func() ([]int, error) { return []int{100, 987}, nil }
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		f.deps.UserManagerGroups = func() ([]int, error) { return nil, errors.New("no systemd --user manager found") }
		f.out.Reset()
		if code := f.run(t, "doctor"); code != 0 {
			t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
		}
		if !strings.Contains(f.out.String(), "[warn] GPU register temperatures: ") ||
			!strings.Contains(f.out.String(), "could not be checked") {
			t.Fatalf("doctor output:\n%s", f.out.String())
		}
	})

	t.Run("no nvidia device", func(t *testing.T) {
		f := newGPUFix(t, privateAccounts())
		if err := os.MkdirAll(filepath.Join(f.deps.Root, "sys", "bus", "pci", "devices"), 0o755); err != nil {
			t.Fatal(err)
		}
		if code := f.run(t, "install", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		paths := f.userPaths(t)
		cfg, err := config.Load(paths.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Components.Collector.Hardware.GPURegisters = true
		if err := config.Save(paths.ConfigFile, cfg); err != nil {
			t.Fatal(err)
		}
		f.out.Reset()
		if code := f.run(t, "doctor"); code != 0 {
			t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
		}
		if !strings.Contains(f.out.String(), "[warn] GPU register temperatures: ") ||
			!strings.Contains(f.out.String(), "no NVIDIA display device") {
			t.Fatalf("doctor output:\n%s", f.out.String())
		}
	})

	t.Run("not opted in", func(t *testing.T) {
		f := newGPUFix(t, privateAccounts(), "0000:01:00.0")
		if code := f.run(t, "install", "--role", "collector", "--no-start"); code != 0 {
			t.Fatalf("install exit %d: %s", code, f.err.String())
		}
		f.out.Reset()
		if code := f.run(t, "doctor"); code != 0 {
			t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
		}
		if !strings.Contains(f.out.String(), "[skip] GPU register temperatures: disabled") {
			t.Fatalf("doctor output:\n%s", f.out.String())
		}
	})
}

func TestDoctorUserScopeRAPLReadability(t *testing.T) {
	f := newGPUFix(t, privateAccounts())
	powercap := filepath.Join(f.deps.Root, "sys", "class", "powercap", "intel-rapl:0")
	if err := os.MkdirAll(powercap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(powercap, "energy_uj"), []byte("42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.deps.StdinIsTerminal = func() bool { return false }
	if code := f.run(t, "install", "--role", "collector", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	f.deps.ReadAttribute = func(path string) ([]byte, error) {
		if strings.HasSuffix(path, "energy_uj") {
			return []byte("4"), nil
		}
		return nil, os.ErrPermission
	}
	f.out.Reset()
	if code := f.run(t, "doctor"); code != 0 {
		t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
	}
	if !strings.Contains(f.out.String(), "[ok] CPU package power (RAPL): working: 1 package counter(s) readable") {
		t.Fatalf("doctor output:\n%s", f.out.String())
	}

	f.deps.ReadAttribute = func(string) ([]byte, error) { return nil, os.ErrPermission }
	f.out.Reset()
	if code := f.run(t, "doctor"); code != 0 {
		t.Fatalf("doctor exit %d:\n%s", code, f.out.String())
	}
	if !strings.Contains(f.out.String(), "[warn] CPU package power (RAPL): ") ||
		!strings.Contains(f.out.String(), "log out fully") {
		t.Fatalf("doctor output:\n%s", f.out.String())
	}
}

func TestSystemScopeGPURuleStillGrantsSharedGroup(t *testing.T) {
	f := newFixture(t)
	f.deps.Root = t.TempDir()
	f.deps.Euid = func() int { return 0 }
	f.deps.RunCommand = func(context.Context, string, ...string) error { return nil }
	if code := f.run(t, "install", "--system", "--role", "collector", "--gpu-registers", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	paths := f.paths(t, config.ScopeSystem)
	content, err := os.ReadFile(paths.UdevGPUTempsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !systemd.IsManaged(string(content)) ||
		!strings.Contains(string(content), "# Group: rigscope-gpu\n") ||
		!strings.Contains(string(content), "chgrp rigscope-gpu /sys%p/resource0") {
		t.Fatalf("system rule:\n%s", content)
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Components.Collector.Hardware.GPURegisters {
		t.Fatal("system install did not record gpuRegisters")
	}
}

// writeRuntimeStatus writes the runtime-PM state of one fake display function.
func writeRuntimeStatus(t *testing.T, root, name, status string) {
	t.Helper()
	dir := filepath.Join(root, "sys", "bus", "pci", "devices", name, "power")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime_status"), []byte(status+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProbeGPUDisplaysSkipsSuspendedDevices(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0", "0000:02:00.0")
	probed := 0
	f.deps.ProbeRegisterAccess = func(string) error {
		probed++
		return nil
	}
	writeRuntimeStatus(t, f.deps.Root, "0000:01:00.0", "suspended")
	writeRuntimeStatus(t, f.deps.Root, "0000:02:00.0", "active")

	probe := probeGPUDisplays(f.deps)
	if probe.devices != 2 || probe.suspended != 1 || probe.readable != 1 {
		t.Fatalf("probe = %+v, want 2 devices, 1 suspended, 1 readable", probe)
	}
	if probed != 1 {
		t.Fatalf("probed %d devices, want the suspended one skipped", probed)
	}
	if probe.denied || probe.err != nil {
		t.Fatalf("probe reported a failure: %+v", probe)
	}
}

func TestProbeGPUDisplaysClassifiesErrors(t *testing.T) {
	f := newGPUFix(t, privateAccounts(), "0000:01:00.0", "0000:02:00.0", "0000:03:00.0")
	f.deps.ProbeRegisterAccess = func(path string) error {
		switch filepath.Base(filepath.Dir(path)) {
		case "0000:01:00.0":
			return os.ErrPermission
		case "0000:02:00.0":
			return os.ErrNotExist
		default:
			return errors.New("input/output error")
		}
	}
	probe := probeGPUDisplays(f.deps)
	if !probe.denied || probe.missing != 1 || probe.err == nil || probe.readable != 0 {
		t.Fatalf("probe = %+v, want one denied, one missing and one error", probe)
	}
}

// writeProcEntry writes a minimal /proc/<pid>/{status,cgroup} pair.
func writeProcEntry(t *testing.T, procRoot, pid, cgroup string, groups ...string) {
	t.Helper()
	dir := filepath.Join(procRoot, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	status := "Name:\tsystemd\nUid:\t1000\t1000\t1000\t1000\nGroups:\t" + strings.Join(groups, " ") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
	if cgroup != "" {
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScanUserManagerGroupsPrefersTheUserManager(t *testing.T) {
	procRoot := t.TempDir()
	// A systemd process of the same uid that is not the user manager, listed
	// first so the old first-match scan would pick it.
	writeProcEntry(t, procRoot, "100", "0::/user.slice/user-1000.slice/session-2.scope", "100")
	writeProcEntry(t, procRoot, "200", "0::/user.slice/user-1000.slice/user@1000.service/init.scope", "100", "987")

	groups, err := scanUserManagerGroups(procRoot, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0] != 100 || groups[1] != 987 {
		t.Fatalf("groups = %v, want the user@1000.service manager's [100 987]", groups)
	}

	// Without a cgroup marker the first matching process is the fallback.
	fallbackRoot := t.TempDir()
	writeProcEntry(t, fallbackRoot, "100", "", "100")
	writeProcEntry(t, fallbackRoot, "200", "", "100", "987")
	groups, err = scanUserManagerGroups(fallbackRoot, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0] != 100 {
		t.Fatalf("fallback groups = %v, want [100]", groups)
	}

	if _, err := scanUserManagerGroups(t.TempDir(), 1000); err == nil {
		t.Fatal("an empty /proc did not report a missing manager")
	}
}
