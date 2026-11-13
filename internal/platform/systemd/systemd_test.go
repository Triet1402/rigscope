package systemd

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden unit files")

func goldenOptions() map[string]Options {
	return map[string]Options{
		"hub-user": {
			Component:  Hub,
			Scope:      User,
			Binary:     "/home/alice/.local/bin/rigscope",
			ConfigFile: "/home/alice/.config/rigscope/config.json",
			StateDir:   "/home/alice/.local/state/rigscope",
		},
		"collector-user": {
			Component:  Collector,
			Scope:      User,
			Binary:     "/home/alice/.local/bin/rigscope",
			ConfigFile: "/home/alice/.config/rigscope/config.json",
			StateDir:   "/home/alice/.local/state/rigscope",
		},
		"hub-system": {
			Component:  Hub,
			Scope:      System,
			Binary:     "/usr/local/bin/rigscope",
			ConfigFile: "/etc/rigscope/config.json",
			StateDir:   "/var/lib/rigscope/hub",
		},
		"collector-system": {
			Component:           Collector,
			Scope:               System,
			Binary:              "/usr/local/bin/rigscope",
			ConfigFile:          "/etc/rigscope/config.json",
			StateDir:            "/var/lib/rigscope/collector",
			SupplementaryGroups: []string{RAPLGroup, GPUGroup},
		},
	}
}

func TestRenderGolden(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "testdata", "units")
	for name, options := range goldenOptions() {
		t.Run(name, func(t *testing.T) {
			unit, err := Render(options)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			path := filepath.Join(dir, name+".service")
			if *update {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s (run go test -update): %v", path, err)
			}
			if string(want) != unit {
				t.Fatalf("unit differs from %s:\n--- want\n%s\n--- got\n%s", path, want, unit)
			}
			if !IsManaged(unit) {
				t.Fatal("rendered unit is not marked as managed")
			}
		})
	}
}

func TestRenderRejectsRelativePaths(t *testing.T) {
	_, err := Render(Options{Component: Hub, Scope: User, Binary: "rigscope", ConfigFile: "/etc/rigscope/config.json"})
	if err == nil || !strings.Contains(err.Error(), "binary path must be absolute") {
		t.Fatalf("err = %v", err)
	}
	_, err = Render(Options{Component: Hub, Scope: User, Binary: "/usr/local/bin/rigscope", ConfigFile: "config.json"})
	if err == nil || !strings.Contains(err.Error(), "config path must be absolute") {
		t.Fatalf("err = %v", err)
	}
}

func TestUserUnitsDoNotUseSystemAccounts(t *testing.T) {
	unit, err := Render(goldenOptions()["hub-user"])
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"User=", "ProtectSystem=strict", "SupplementaryGroups=", "WantedBy=multi-user.target"} {
		if strings.Contains(unit, unwanted) {
			t.Fatalf("user unit contains %q:\n%s", unwanted, unit)
		}
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Fatal("user unit must start at login")
	}
}

func TestSystemCollectorGetsHardwareGroups(t *testing.T) {
	unit, err := Render(goldenOptions()["collector-system"])
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{"User=rigscope-collector", "SupplementaryGroups=rigscope-rapl rigscope-gpu", "ProtectSystem=strict", "CapabilityBoundingSet="} {
		if !strings.Contains(unit, wanted) {
			t.Fatalf("system collector missing %q:\n%s", wanted, unit)
		}
	}
	if strings.Contains(unit, "PrivateDevices=yes") {
		t.Fatal("collector needs real device nodes")
	}
	hub, err := Render(goldenOptions()["hub-system"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hub, "PrivateDevices=yes") {
		t.Fatal("system hub should not need device nodes")
	}
}

func TestCollectorWithoutHardwareGroups(t *testing.T) {
	options := goldenOptions()["collector-system"]
	options.SupplementaryGroups = nil
	unit, err := Render(options)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unit, "SupplementaryGroups=") {
		t.Fatalf("collector without hardware access has groups:\n%s", unit)
	}
}

func TestRenderSysusersHardwareIsOptIn(t *testing.T) {
	plain := RenderSysusers(SysusersOptions{})
	for _, unwanted := range []string{"rigscope-rapl", "rigscope-gpu"} {
		if strings.Contains(plain, unwanted) {
			t.Fatalf("default sysusers mentions %q:\n%s", unwanted, plain)
		}
	}
	if !IsManaged(plain) || !strings.Contains(plain, "rigscope-hub") || !strings.Contains(plain, "rigscope-collector") {
		t.Fatalf("service accounts missing:\n%s", plain)
	}
	raplOnly := RenderSysusers(SysusersOptions{RAPL: true})
	if !strings.Contains(raplOnly, "g rigscope-rapl") || strings.Contains(raplOnly, "rigscope-gpu") {
		t.Fatalf("RAPL-only sysusers:\n%s", raplOnly)
	}
	both := RenderSysusers(SysusersOptions{RAPL: true, GPURegisters: true})
	for _, wanted := range []string{"g rigscope-rapl", "g rigscope-gpu", "m rigscope-collector rigscope-rapl", "m rigscope-collector rigscope-gpu"} {
		if !strings.Contains(both, wanted) {
			t.Fatalf("sysusers missing %q:\n%s", wanted, both)
		}
	}
	if groups := HardwareGroups(true, false); len(groups) != 1 || groups[0] != RAPLGroup {
		t.Fatalf("groups = %v", groups)
	}
	if groups := HardwareGroups(false, false); len(groups) != 0 {
		t.Fatalf("groups = %v", groups)
	}
}

func TestLegacySystemMarker(t *testing.T) {
	for _, content := range []string{
		"# Managed by `rigscope system`. Enable explicitly; do not edit in place.\n[Unit]\n",
		"# Managed by Rigscope system installation.\n",
		`[Service]` + "\n" + `ExecStart="/opt/rigscope/node/node" "/opt/rigscope/current/dist/hub.cjs"` + "\n",
	} {
		if !IsLegacySystemManaged(content) {
			t.Fatalf("0.2 system unit not recognised:\n%s", content)
		}
	}
	modern, err := Render(goldenOptions()["hub-system"])
	if err != nil {
		t.Fatal(err)
	}
	if IsLegacySystemManaged(modern) {
		t.Fatal("1.x unit matched the 0.2 system marker")
	}
}

func TestLegacyManagedMarker(t *testing.T) {
	if !IsLegacyManaged("# Managed by Rigscope: generated by packaging/system/90-rigscope-gpu-temps.rules\n") {
		t.Fatal("0.2 file marker not recognised")
	}
	modern := RenderGPUTempsRule(GPUGroup)
	if IsLegacyManaged(modern) || !IsManaged(modern) {
		t.Fatal("1.x GPU rule marker is wrong")
	}
	if !strings.Contains(modern, "# Group: "+GPUGroup+"\n") ||
		!strings.Contains(modern, "chgrp "+GPUGroup+" /sys%p/resource0") {
		t.Fatalf("GPU rule does not record its group:\n%s", modern)
	}
}

func TestLegacyUnits(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"rigscope-0123456789ab-hub.service",
		"rigscope-0123456789ab-collector.service",
		"rigscope-0123456789ab.service",
		"rigscope-hub.service",
		"rigscope-collector.service",
		"other.service",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	legacy, err := LegacyUnits(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 3 {
		t.Fatalf("legacy = %v, want the three 0.2 names", legacy)
	}
	for _, name := range legacy {
		if name == "rigscope-hub.service" || name == "rigscope-collector.service" {
			t.Fatalf("1.0 name %s matched the 0.2 pattern", name)
		}
	}
}

func TestLegacyUnitsMissingDirectory(t *testing.T) {
	legacy, err := LegacyUnits(filepath.Join(t.TempDir(), "absent"))
	if err != nil || len(legacy) != 0 {
		t.Fatalf("legacy = %v, err = %v", legacy, err)
	}
}

func TestExecManagerCommands(t *testing.T) {
	var calls []string
	runner := func(_ context.Context, name string, args ...string) (Result, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if strings.Contains(strings.Join(args, " "), "show") {
			return Result{Stdout: "loaded\nactive\nrunning\n"}, nil
		}
		return Result{}, nil
	}
	manager := NewExecWithRunner(User, runner)
	ctx := context.Background()
	if err := manager.DaemonReload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(ctx, "rigscope-hub.service"); err != nil {
		t.Fatal(err)
	}
	active, err := manager.IsActive(ctx, "rigscope-hub.service")
	if err != nil || !active {
		t.Fatalf("active = %v, err = %v", active, err)
	}
	if err := manager.EnableLinger(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user start rigscope-hub.service",
		"systemctl --user show -p LoadState,ActiveState,SubState --value rigscope-hub.service",
		"loginctl enable-linger",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestExecManagerSystemScope(t *testing.T) {
	var calls []string
	runner := func(_ context.Context, name string, args ...string) (Result, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return Result{}, nil
	}
	manager := NewExecWithRunner(System, runner)
	if err := manager.Restart(context.Background(), "rigscope-collector.service"); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnableLinger(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "systemctl restart rigscope-collector.service" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestExecManagerReportsFailure(t *testing.T) {
	runner := func(context.Context, string, ...string) (Result, error) {
		return Result{Stderr: "Unit not found", ExitCode: 5}, nil
	}
	manager := NewExecWithRunner(User, runner)
	err := manager.Start(context.Background(), "rigscope-hub.service")
	if err == nil || !strings.Contains(err.Error(), "Unit not found") {
		t.Fatalf("err = %v", err)
	}
	// A failed `show` is a communication failure, never "inactive".
	if _, err := manager.IsActive(context.Background(), "rigscope-hub.service"); err == nil || !strings.Contains(err.Error(), "Unit not found") {
		t.Fatalf("IsActive err = %v", err)
	}
	if _, err := manager.UnitKnown(context.Background(), "rigscope-hub.service"); err == nil {
		t.Fatal("UnitKnown read a systemctl failure as an unknown unit")
	}
}

func TestExecManagerBusFailureIsNotStopped(t *testing.T) {
	runner := func(context.Context, string, ...string) (Result, error) {
		return Result{Stderr: "Failed to connect to bus: No such file or directory", ExitCode: 1}, nil
	}
	manager := NewExecWithRunner(User, runner)
	ctx := context.Background()
	if known, err := manager.UnitKnown(ctx, "rigscope-hub.service"); err == nil {
		t.Fatalf("bus failure read as known=%v", known)
	} else if !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("err = %v", err)
	}
	if active, err := manager.IsActive(ctx, "rigscope-hub.service"); err == nil {
		t.Fatalf("bus failure read as active=%v", active)
	} else if !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecManagerParsesUnitState(t *testing.T) {
	states := map[string]string{
		"not-found\ninactive\ndead\n":                              "unknown",
		"loaded\ninactive\ndead\n":                                 "inactive",
		"loaded\nfailed\nfailed\n":                                 "inactive",
		"loaded\nactive\nrunning\n":                                "active",
		"loaded\nactivating\nstart\n":                              "active",
		"LoadState=loaded\nActiveState=active\nSubState=running\n": "active",
	}
	for output, want := range states {
		output := output
		want := want
		t.Run(strings.ReplaceAll(strings.TrimSpace(output), "\n", ","), func(t *testing.T) {
			runner := func(context.Context, string, ...string) (Result, error) {
				return Result{Stdout: output}, nil
			}
			manager := NewExecWithRunner(User, runner)
			known, err := manager.UnitKnown(context.Background(), "rigscope-hub.service")
			if err != nil {
				t.Fatal(err)
			}
			if want == "unknown" {
				if known {
					t.Fatal("not-found unit reported as known")
				}
				return
			}
			if !known {
				t.Fatal("unit reported as unknown")
			}
			active, err := manager.IsActive(context.Background(), "rigscope-hub.service")
			if err != nil {
				t.Fatal(err)
			}
			if active != (want == "active") {
				t.Fatalf("active = %v, want %s", active, want)
			}
		})
	}
}

func TestExecManagerUnitRunningRejectsTransitionStates(t *testing.T) {
	states := map[string]bool{
		"loaded\nactive\nrunning\n":    true,
		"loaded\nactive\nexited\n":     false, // a daemon that exited is not ready
		"loaded\nactivating\nstart\n":  false,
		"loaded\ndeactivating\nstop\n": false,
		"loaded\nactive\nstart-pre\n":  false,
		"loaded\nreloading\nreload\n":  false,
		"loaded\ninactive\ndead\n":     false,
		"loaded\nfailed\nfailed\n":     false,
	}
	for output, want := range states {
		output, want := output, want
		t.Run(strings.ReplaceAll(strings.TrimSpace(output), "\n", ","), func(t *testing.T) {
			runner := func(context.Context, string, ...string) (Result, error) {
				return Result{Stdout: output}, nil
			}
			manager := NewExecWithRunner(User, runner)
			running, err := manager.UnitRunning(context.Background(), "rigscope-hub.service")
			if err != nil {
				t.Fatal(err)
			}
			if running != want {
				t.Fatalf("running = %v, want %v", running, want)
			}
		})
	}
}

func TestExecManagerRejectsUnexpectedUnitState(t *testing.T) {
	runner := func(context.Context, string, ...string) (Result, error) {
		return Result{Stdout: "\nactive\n"}, nil
	}
	manager := NewExecWithRunner(User, runner)
	if _, err := manager.UnitKnown(context.Background(), "rigscope-hub.service"); err == nil {
		t.Fatal("empty LoadState accepted")
	}
	runner = func(context.Context, string, ...string) (Result, error) {
		return Result{Stdout: "loaded\nflying\nrunning\n"}, nil
	}
	manager = NewExecWithRunner(User, runner)
	if active, err := manager.IsActive(context.Background(), "rigscope-hub.service"); err == nil {
		t.Fatalf("unexpected ActiveState read as active=%v", active)
	}
}

func TestExecManagerRunnerError(t *testing.T) {
	runner := func(context.Context, string, ...string) (Result, error) {
		return Result{}, errors.New("systemctl missing")
	}
	manager := NewExecWithRunner(User, runner)
	if err := manager.DaemonReload(context.Background()); err == nil || !strings.Contains(err.Error(), "systemctl missing") {
		t.Fatalf("err = %v", err)
	}
}

func TestFakeManager(t *testing.T) {
	fake := NewFake()
	ctx := context.Background()
	if err := fake.Start(ctx, "rigscope-hub.service"); err != nil {
		t.Fatal(err)
	}
	if active, _ := fake.IsActive(ctx, "rigscope-hub.service"); !active {
		t.Fatal("start did not activate the unit")
	}
	if err := fake.Stop(ctx, "rigscope-hub.service"); err != nil {
		t.Fatal(err)
	}
	if active, _ := fake.IsActive(ctx, "rigscope-hub.service"); active {
		t.Fatal("stop did not deactivate the unit")
	}
	if err := fake.EnableLinger(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.LingerCalls != 1 {
		t.Fatalf("linger calls = %d", fake.LingerCalls)
	}
	if !fake.HasCall("stop", "rigscope-hub.service") {
		t.Fatalf("calls = %v", fake.Methods())
	}
}

// WorkingDirectory= takes the rest of the line as the path, without quote
// processing; systemd rejects a quoted value as "path is not absolute".
func TestSystemWorkingDirectoryIsUnquoted(t *testing.T) {
	unit, err := Render(Options{Component: Hub, Scope: System, Binary: "/usr/local/bin/rigscope", ConfigFile: "/etc/rigscope/config.json", StateDir: "/srv/100%/hub"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, "\nWorkingDirectory=/srv/100%%/hub\n") {
		t.Fatalf("WorkingDirectory must be unquoted with %% escaped:\n%s", unit)
	}
	for _, bad := range []string{"/var/lib/rig\nscope", " /var/lib/rigscope", "/var/lib/rigscope "} {
		if _, err := Render(Options{Component: Hub, Scope: System, Binary: "/usr/local/bin/rigscope", ConfigFile: "/etc/rigscope/config.json", StateDir: bad}); err == nil {
			t.Fatalf("state directory %q was accepted", bad)
		}
	}
}

// TestUnitsPassSystemdVerify loads every golden unit into the host's real
// systemd parser. The fake manager used elsewhere never parses unit files, so
// this is the only check that systemd accepts what Render writes.
func TestUnitsPassSystemdVerify(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not available")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, options := range goldenOptions() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			options.Binary = executable
			options.ConfigFile = filepath.Join(dir, "config.json")
			options.StateDir = filepath.Join(dir, "state dir")
			if err := os.Mkdir(options.StateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			unit, err := Render(options)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, options.Component.UnitName())
			if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
				t.Fatal(err)
			}
			args := []string{"verify", "--man=no"}
			if options.Scope == User {
				args = append(args, "--user")
			}
			out, err := exec.Command(analyze, append(args, path)...).CombinedOutput()
			if err != nil || strings.Contains(string(out), filepath.Base(path)+":") {
				t.Fatalf("systemd-analyze verify rejected %s: %v\n%s\n%s", name, err, out, unit)
			}
		})
	}
}
