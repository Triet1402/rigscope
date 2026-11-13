// Command e2e runs the Rigscope 1.0 installer lifecycle end to end with real
// release binaries in a disposable environment.
//
// Everything runs against a temporary HOME, XDG directories and a fake
// systemctl/journalctl/loginctl that never reaches the real systemd. The
// release artifacts are built with scripts/release/build.sh and served from a
// local HTTPS server with a dedicated test CA; install.sh and `rigscope
// upgrade` both download from that server through the documented test seams
// (RIGSCOPE_INSTALL_BASE_URL and RIGSCOPE_RELEASE_BASE_URL).
//
// scripts/e2e/lifecycle.sh builds this program and calls it. The program is
// intentionally a single process so HTTP, SSE, history and process assertions
// share one cookie jar and one clock.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Triet1402/rigscope/internal/config"
)

const (
	versionOne = "1.0.0-e2e.1"
	versionTwo = "1.0.0-e2e.2"

	hubUnit       = "rigscope-hub.service"
	collectorUnit = "rigscope-collector.service"
)

func main() {
	root := flag.String("root", ".", "repository root")
	work := flag.String("work", "", "disposable work directory (created by lifecycle.sh)")
	flag.Parse()
	if *work == "" {
		fmt.Fprintln(os.Stderr, "e2e: -work is required")
		os.Exit(2)
	}
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		fail(err)
	}
	absoluteWork, err := filepath.Abs(*work)
	if err != nil {
		fail(err)
	}
	run := &runner{root: absoluteRoot, work: absoluteWork, started: time.Now()}
	if err := run.execute(); err != nil {
		run.dumpDiagnostics()
		fmt.Fprintf(os.Stderr, "\ne2e: FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\ne2e: PASS in %s\n", time.Since(run.started).Round(100*time.Millisecond))
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
	os.Exit(1)
}

type runner struct {
	root    string
	work    string
	started time.Time
	envs    []*testEnv
}

func (r *runner) execute() error {
	if err := os.MkdirAll(r.work, 0o700); err != nil {
		return err
	}
	fakeBin := filepath.Join(r.work, "fake-bin")
	if err := r.step("build fake systemd tools", 3*time.Minute, func(ctx context.Context) error {
		return buildFakeTools(ctx, r.root, fakeBin)
	}); err != nil {
		return err
	}

	// Safety gate: no product command runs before this proof that the PATH
	// resolves every system tool to our double and HOME lives below work.
	if err := r.step("verify disposable environment", 30*time.Second, func(ctx context.Context) error {
		return verifyIsolation(fakeBin, r.work)
	}); err != nil {
		return err
	}

	assets := filepath.Join(r.work, "assets")
	for _, version := range []string{versionOne, versionTwo} {
		version := version
		if err := r.step("build release "+version, 15*time.Minute, func(ctx context.Context) error {
			return buildRelease(ctx, r.root, version, filepath.Join(assets, version))
		}); err != nil {
			return err
		}
	}

	server, err := newReleaseServer(assets, versionTwo)
	if err != nil {
		return err
	}
	defer server.Close()
	caFile := filepath.Join(r.work, "test-ca.pem")
	if err := os.WriteFile(caFile, server.caPEM, 0o600); err != nil {
		return err
	}
	r.stepf("release server %s serves %s and %s", server.URL, versionOne, versionTwo)

	hubPort, err := freePort()
	if err != nil {
		return err
	}
	env1, err := r.newEnv("primary", fakeBin, caFile, server.URL, hubPort)
	if err != nil {
		return err
	}
	env2, err := r.newEnv("collector-two", fakeBin, caFile, server.URL, 0)
	if err != nil {
		return err
	}
	env3, err := r.newEnv("keep-data", fakeBin, caFile, server.URL, 0)
	if err != nil {
		return err
	}
	// Registered before uninstall so a failure still stops every fake service.
	defer r.stopAllFakeServices()

	installScript, err := server.fetch("/download/v" + versionOne + "/install.sh")
	if err != nil {
		return err
	}
	installPath := filepath.Join(r.work, "install-"+versionOne+".sh")
	if err := os.WriteFile(installPath, installScript, 0o755); err != nil {
		return err
	}

	independentBinary := filepath.Join(r.work, "rigscope-"+versionOne)
	installOutput := ""
	if err := r.step("install.sh installs "+versionOne+" (both, --boot)", 5*time.Minute, func(ctx context.Context) error {
		output, err := r.run(ctx, env1, 4*time.Minute, r.root, "sh", installPath,
			"--yes", "--role", "both", "--boot", "--port", strconv.Itoa(hubPort))
		if err != nil {
			return err
		}
		installOutput = output
		for _, want := range []string{"Installed Rigscope " + versionOne, "enabled: hub, collector", "started: hub, collector"} {
			if !strings.Contains(output, want) {
				return fmt.Errorf("install.sh output does not contain %q:\n%s", want, output)
			}
		}
		if err := requireFile(filepath.Join(env1.fakeRoot, "linger")); err != nil {
			return fmt.Errorf("--boot did not call loginctl enable-linger: %w", err)
		}
		// Keep a copy so the --keep-data check can install a second fresh
		// environment after the primary installation has been uninstalled.
		data, err := os.ReadFile(env1.binary())
		if err != nil {
			return err
		}
		return os.WriteFile(independentBinary, data, 0o755)
	}); err != nil {
		return err
	}

	if err := r.step("hub and collector units are active", 1*time.Minute, func(ctx context.Context) error {
		if err := r.checkStatus(ctx, env1, versionOne); err != nil {
			return err
		}
		for _, unit := range []string{hubUnit, collectorUnit} {
			if _, err := readPID(env1, unit); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	api, err := newAPI(server, hubPort)
	if err != nil {
		return err
	}
	password := ""
	if err := r.step("hub healthy; owner password shown by install, never in the journal", 2*time.Minute, func(ctx context.Context) error {
		if err := waitFor(ctx, 60*time.Second, 200*time.Millisecond, func() error {
			return api.healthy(versionOne)
		}); err != nil {
			return err
		}
		got, err := r.ownerPassword(ctx, env1, installOutput)
		if err != nil {
			return err
		}
		password = got
		return nil
	}); err != nil {
		return err
	}

	if err := r.step("owner login and automatic local collector pairing", 3*time.Minute, func(ctx context.Context) error {
		if err := api.login(ctx, password); err != nil {
			return err
		}
		// A default `--role both` install pairs the local collector without an
		// operator-created grant. The hub imported the hashed bootstrap file
		// before listening and deleted it; the collector enrolled on its own.
		if err := requireMissing(filepath.Join(env1.stateDir(), "collector-pairing.json")); err != nil {
			return fmt.Errorf("hub pairing bootstrap file was not consumed: %w", err)
		}
		configData, err := os.ReadFile(filepath.Join(env1.configHome(), "rigscope", "config.json"))
		if err != nil {
			return err
		}
		var installed struct {
			Components struct {
				Collector struct {
					Destinations []struct {
						URL string `json:"url"`
					} `json:"destinations"`
				} `json:"collector"`
			} `json:"components"`
		}
		if err := json.Unmarshal(configData, &installed); err != nil {
			return err
		}
		wantDestination := "ws://127.0.0.1:" + strconv.Itoa(hubPort) + "/api/collector/connect"
		destinations := installed.Components.Collector.Destinations
		if len(destinations) != 1 || destinations[0].URL != wantDestination {
			return fmt.Errorf("local collector destinations = %+v, want %s", destinations, wantDestination)
		}
		snapshot, err := api.waitForMachines(ctx, 90*time.Second, func(machines []liveMachine) bool {
			return len(machines) == 1 && freshMachine(machines[0], 30*time.Second)
		})
		if err != nil {
			return err
		}
		if err := requireFile(filepath.Join(env1.stateDir(), "collector", "credentials.json")); err != nil {
			return fmt.Errorf("local collector has no credential after automatic pairing: %w", err)
		}
		if _, err := os.Stat(filepath.Join(env1.stateDir(), "collector", "enrollment.json")); !os.IsNotExist(err) {
			return fmt.Errorf("collector kept its enrollment grant after pairing: %v", err)
		}
		r.stepf("local machine %s (%s) paired automatically", snapshot.Machines[0].MachineID, snapshot.Machines[0].Hostname)
		return nil
	}); err != nil {
		return err
	}

	liveCtx, cancelLive := context.WithTimeout(context.Background(), 30*time.Second)
	primary, err := api.liveOnce(liveCtx)
	cancelLive()
	if err != nil {
		return err
	}
	localMachine := primary.Machines[0].MachineID

	if err := r.step("fresh install defaults to RAM; opt into persistent history", 2*time.Minute, func(ctx context.Context) error {
		data, status, err := api.get(ctx, "/api/history/status")
		if err != nil {
			return err
		}
		var historyStatus struct {
			Mode              string `json:"mode"`
			PersistentHistory bool   `json:"persistentHistory"`
		}
		if status != http.StatusOK {
			return fmt.Errorf("history status = HTTP %d: %s", status, data)
		}
		if err := json.Unmarshal(data, &historyStatus); err != nil {
			return err
		}
		if historyStatus.Mode != "memory" || historyStatus.PersistentHistory {
			return fmt.Errorf("fresh install did not default to RAM history: %s", data)
		}
		path := filepath.Join(env1.configHome(), "rigscope", "config.json")
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		cfg.Components.Hub.TelemetryMode = "persistent"
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		if _, err := r.run(ctx, env1, time.Minute, r.root, env1.binary(), "restart", "hub"); err != nil {
			return err
		}
		return waitFor(ctx, time.Minute, 300*time.Millisecond, func() error {
			data, status, err := api.get(ctx, "/api/history/status")
			if err != nil {
				return err
			}
			if status != http.StatusOK {
				return fmt.Errorf("history status after restart = HTTP %d", status)
			}
			if err := json.Unmarshal(data, &historyStatus); err != nil {
				return err
			}
			if historyStatus.Mode != "persistent" || !historyStatus.PersistentHistory {
				return fmt.Errorf("persistent opt-in was not applied: %s", data)
			}
			return nil
		})
	}); err != nil {
		return err
	}

	if err := r.step("second collector installs in its own fake HOME and enrolls", 4*time.Minute, func(ctx context.Context) error {
		grant, err := api.createGrant(ctx)
		if err != nil {
			return err
		}
		output, err := r.run(ctx, env2, 3*time.Minute, r.root, env1.binary(), "install", "--yes",
			"--role", "collector", "--boot",
			"--hub", "http://127.0.0.1:"+strconv.Itoa(hubPort), "--token", grant)
		if err != nil {
			return err
		}
		for _, want := range []string{"Installed Rigscope " + versionOne, "enabled: collector", "started: collector"} {
			if !strings.Contains(output, want) {
				return fmt.Errorf("second collector install output does not contain %q:\n%s", want, output)
			}
		}
		if err := requireFile(filepath.Join(env2.fakeRoot, "linger")); err != nil {
			return fmt.Errorf("--boot did not call loginctl enable-linger in the second HOME: %w", err)
		}
		snapshot, err := api.waitForMachines(ctx, 90*time.Second, func(machines []liveMachine) bool {
			if len(machines) != 2 {
				return false
			}
			for _, machine := range machines {
				if !freshMachine(machine, 30*time.Second) {
					return false
				}
			}
			return true
		})
		if err != nil {
			return err
		}
		ids := machineIDs(snapshot.Machines)
		if !ids[localMachine] {
			return fmt.Errorf("local machine %s disappeared from live state; saw %v", localMachine, ids)
		}
		r.stepf("two machines reporting: %v", ids)
		return nil
	}); err != nil {
		return err
	}

	if err := r.step("status --json and doctor (before upgrade)", 2*time.Minute, func(ctx context.Context) error {
		if err := r.checkStatus(ctx, env1, versionOne); err != nil {
			return err
		}
		return r.checkDoctor(ctx, env1)
	}); err != nil {
		return err
	}

	var beforePoints int64
	if err := r.step("history records the local machine", 2*time.Minute, func(ctx context.Context) error {
		from := time.Now().Add(-2 * time.Minute).UnixMilli()
		to := time.Now().UnixMilli()
		result, err := api.waitForHistory(ctx, localMachine, from, to, 1)
		if err != nil {
			return err
		}
		beforePoints = countPoints(result)
		r.stepf("history before upgrade: %d point(s) across %d series", beforePoints, len(result.Series))
		return nil
	}); err != nil {
		return err
	}

	upgradeStart := time.Now()
	var oldHubPID, oldCollectorPID int
	if err := r.step("upgrade to "+versionTwo, 8*time.Minute, func(ctx context.Context) error {
		var err error
		oldHubPID, err = readPID(env1, hubUnit)
		if err != nil {
			return err
		}
		oldCollectorPID, err = readPID(env1, collectorUnit)
		if err != nil {
			return err
		}
		output, err := r.run(ctx, env1, 6*time.Minute, r.root, env1.binary(),
			"upgrade", "--yes", "--version", versionTwo)
		if err != nil {
			return err
		}
		if !strings.Contains(output, "Upgraded to Rigscope "+versionTwo) {
			return fmt.Errorf("upgrade output does not report the new version:\n%s", output)
		}
		if err := waitFor(ctx, 90*time.Second, 200*time.Millisecond, func() error {
			return api.healthy(versionTwo)
		}); err != nil {
			return err
		}
		if err := r.assertBinaryVersion(ctx, env1, versionTwo); err != nil {
			return err
		}
		if err := requireFile(env1.home + "/.local/bin/rigscope.prev"); err != nil {
			return fmt.Errorf("upgrade retained no previous binary: %w", err)
		}
		phase, err := journalPhase(env1)
		if err != nil {
			return err
		}
		if phase != "done" {
			return fmt.Errorf("upgrade journal phase = %q, want done", phase)
		}
		hubPID, err := readPID(env1, hubUnit)
		if err != nil {
			return err
		}
		collectorPID, err := readPID(env1, collectorUnit)
		if err != nil {
			return err
		}
		if hubPID == oldHubPID || collectorPID == oldCollectorPID {
			return fmt.Errorf("services did not restart: hub %d→%d collector %d→%d", oldHubPID, hubPID, oldCollectorPID, collectorPID)
		}
		if processExists(oldHubPID) || processExists(oldCollectorPID) {
			return fmt.Errorf("old service process survived the upgrade: hub %d, collector %d", oldHubPID, oldCollectorPID)
		}
		snapshot, err := api.waitForMachines(ctx, 90*time.Second, func(machines []liveMachine) bool {
			ids := machineIDs(machines)
			return len(machines) == 2 && ids[localMachine] && allFresh(machines, 30*time.Second)
		})
		if err != nil {
			return err
		}
		r.stepf("after upgrade: %d machines, hub pid %d→%d, collector pid %d→%d", len(snapshot.Machines), oldHubPID, hubPID, oldCollectorPID, collectorPID)
		return nil
	}); err != nil {
		return err
	}
	upgradeEnd := time.Now()

	if err := r.step("data and history survive the upgrade", 3*time.Minute, func(ctx context.Context) error {
		// Re-login proves the owner account and its password survived.
		if err := api.login(ctx, password); err != nil {
			return err
		}
		collectors, err := api.collectors(ctx)
		if err != nil {
			return err
		}
		if len(collectors) < 2 {
			return fmt.Errorf("collector registry has %d rows after upgrade, want at least 2", len(collectors))
		}
		result, err := api.waitForHistoryAcross(ctx, localMachine,
			upgradeStart.Add(-2*time.Minute).UnixMilli(),
			upgradeEnd.Add(2*time.Minute).UnixMilli(),
			upgradeStart.UnixMilli(), upgradeEnd.UnixMilli())
		if err != nil {
			return err
		}
		r.stepf("history is continuous across the upgrade: %d points", countPoints(result))
		return nil
	}); err != nil {
		return err
	}

	if err := r.step("status --json and doctor (after upgrade)", 2*time.Minute, func(ctx context.Context) error {
		if err := r.checkStatus(ctx, env1, versionTwo); err != nil {
			return err
		}
		previous, err := r.binaryVersionAt(ctx, env1, env1.binary()+".prev")
		if err != nil {
			return err
		}
		if previous != versionOne {
			return fmt.Errorf("rigscope.prev reports %s, want %s", previous, versionOne)
		}
		return r.checkDoctor(ctx, env1)
	}); err != nil {
		return err
	}

	if err := r.step("rollback to "+versionOne, 6*time.Minute, func(ctx context.Context) error {
		hubPID, err := readPID(env1, hubUnit)
		if err != nil {
			return err
		}
		collectorPID, err := readPID(env1, collectorUnit)
		if err != nil {
			return err
		}
		output, err := r.run(ctx, env1, 5*time.Minute, r.root, env1.binary(), "rollback", "--yes")
		if err != nil {
			return err
		}
		if !strings.Contains(output, "Rolled back to Rigscope "+versionOne) {
			return fmt.Errorf("rollback output does not report %s:\n%s", versionOne, output)
		}
		if err := waitFor(ctx, 90*time.Second, 200*time.Millisecond, func() error {
			return api.healthy(versionOne)
		}); err != nil {
			return err
		}
		if err := r.assertBinaryVersion(ctx, env1, versionOne); err != nil {
			return err
		}
		phase, err := journalPhase(env1)
		if err != nil {
			return err
		}
		if phase != "rolled-back" {
			return fmt.Errorf("upgrade journal phase = %q, want rolled-back", phase)
		}
		newHubPID, err := readPID(env1, hubUnit)
		if err != nil {
			return err
		}
		newCollectorPID, err := readPID(env1, collectorUnit)
		if err != nil {
			return err
		}
		if newHubPID == hubPID || newCollectorPID == collectorPID {
			return fmt.Errorf("rollback did not restart services: hub %d→%d collector %d→%d", hubPID, newHubPID, collectorPID, newCollectorPID)
		}
		if _, err := api.waitForMachines(ctx, 90*time.Second, func(machines []liveMachine) bool {
			ids := machineIDs(machines)
			return len(machines) == 2 && ids[localMachine] && allFresh(machines, 30*time.Second)
		}); err != nil {
			return err
		}
		// History still reaches back before the upgrade after the rollback.
		from := upgradeStart.Add(-2 * time.Minute).UnixMilli()
		to := time.Now().UnixMilli()
		result, err := api.waitForHistory(ctx, localMachine, from, to, 1)
		if err != nil {
			return err
		}
		var before bool
		for _, series := range result.Series {
			for _, point := range series.Points {
				if point.T < upgradeStart.UnixMilli() {
					before = true
				}
			}
		}
		if !before {
			return fmt.Errorf("history no longer reaches back before the upgrade after rollback")
		}
		return nil
	}); err != nil {
		return err
	}

	if err := r.step("status --json and doctor (after rollback)", 2*time.Minute, func(ctx context.Context) error {
		if err := r.checkStatus(ctx, env1, versionOne); err != nil {
			return err
		}
		// After a rollback the previous binary path holds the newer release;
		// it must still be runnable and report that release.
		previous, err := r.binaryVersionAt(ctx, env1, env1.binary()+".prev")
		if err != nil {
			return err
		}
		if previous != versionTwo {
			return fmt.Errorf("rigscope.prev reports %s after rollback, want %s", previous, versionTwo)
		}
		return r.checkDoctor(ctx, env1)
	}); err != nil {
		return err
	}

	if err := r.step("uninstall the second collector", 2*time.Minute, func(ctx context.Context) error {
		if _, err := r.run(ctx, env2, 90*time.Second, r.root, env2.binary(), "uninstall", "--yes"); err != nil {
			return err
		}
		for _, unit := range []string{hubUnit, collectorUnit} {
			if err := requireMissing(filepath.Join(env2.home, ".config", "systemd", "user", unit)); err != nil {
				return err
			}
		}
		if err := requireMissing(env2.binary()); err != nil {
			return err
		}
		return r.assertNoFakeProcesses(env2)
	}); err != nil {
		return err
	}

	foreign := []string{
		filepath.Join(env1.home, ".local", "bin", "keep-me"),
		filepath.Join(env1.home, ".config", "rigscope", "keep-me.txt"),
		filepath.Join(env1.home, ".config", "systemd", "user", "foreign.service"),
		filepath.Join(env1.home, ".local", "share", "keep-me"),
	}
	if err := r.step("uninstall removes only owned files and stops services", 3*time.Minute, func(ctx context.Context) error {
		for _, path := range foreign {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte("keep\n"), 0o600); err != nil {
				return err
			}
		}
		hubPID, _ := readPID(env1, hubUnit)
		collectorPID, _ := readPID(env1, collectorUnit)
		if _, err := r.run(ctx, env1, 2*time.Minute, r.root, env1.binary(), "uninstall", "--yes"); err != nil {
			return err
		}
		for _, path := range []string{
			env1.binary(),
			env1.binary() + ".prev",
			filepath.Join(env1.home, ".config", "rigscope", "config.json"),
			filepath.Join(env1.home, ".local", "state", "rigscope"),
			filepath.Join(env1.home, ".config", "systemd", "user", hubUnit),
			filepath.Join(env1.home, ".config", "systemd", "user", collectorUnit),
		} {
			if err := requireMissing(path); err != nil {
				return err
			}
		}
		for _, path := range foreign {
			if err := requireFile(path); err != nil {
				return err
			}
		}
		for _, unit := range []string{hubUnit, collectorUnit} {
			if err := requireMissing(filepath.Join(env1.fakeRoot, "enabled", unit)); err != nil {
				return err
			}
		}
		if processExists(hubPID) || processExists(collectorPID) {
			return fmt.Errorf("uninstall left a fake service running: hub %d, collector %d", hubPID, collectorPID)
		}
		if err := r.assertNoFakeProcesses(env1); err != nil {
			return err
		}
		commands, err := os.ReadFile(filepath.Join(env1.fakeRoot, "commands.log"))
		if err != nil {
			return err
		}
		for _, verb := range []string{"daemon-reload", "enable", "start", "stop", "restart", "--user show", "disable", "enable-linger"} {
			if !strings.Contains(string(commands), verb) {
				return fmt.Errorf("fake systemctl never saw %q:\n%s", verb, commands)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if err := r.step("uninstall --keep-data keeps config and state", 3*time.Minute, func(ctx context.Context) error {
		port, err := freePort()
		if err != nil {
			return err
		}
		output, err := r.run(ctx, env3, 2*time.Minute, r.root, independentBinary, "install", "--yes",
			"--role", "both", "--no-start", "--port", strconv.Itoa(port))
		if err != nil {
			return err
		}
		if !strings.Contains(output, "services not started (--no-start)") {
			return fmt.Errorf("--no-start install output unexpected:\n%s", output)
		}
		kept := filepath.Join(env3.home, ".local", "state", "rigscope", "keep-data.txt")
		if err := os.WriteFile(kept, []byte("state\n"), 0o600); err != nil {
			return err
		}
		if _, err := r.run(ctx, env3, 90*time.Second, r.root, env3.binary(), "uninstall", "--yes", "--keep-data"); err != nil {
			return err
		}
		for _, path := range []string{
			env3.binary(),
			filepath.Join(env3.home, ".config", "systemd", "user", hubUnit),
			filepath.Join(env3.home, ".config", "systemd", "user", collectorUnit),
		} {
			if err := requireMissing(path); err != nil {
				return err
			}
		}
		config := filepath.Join(env3.home, ".config", "rigscope", "config.json")
		if err := requireFile(config); err != nil {
			return err
		}
		if err := requireFile(kept); err != nil {
			return err
		}
		if err := requireFile(filepath.Join(env3.home, ".local", "state", "rigscope", "install-manifest.json")); err != nil {
			return err
		}
		// The rewritten manifest must let a later install adopt the kept data.
		if _, err := r.run(ctx, env3, 2*time.Minute, r.root, independentBinary, "install", "--yes",
			"--role", "hub", "--no-start", "--port", strconv.Itoa(port)); err != nil {
			return fmt.Errorf("reinstall after --keep-data: %w", err)
		}
		return requireFile(kept)
	}); err != nil {
		return err
	}

	if err := r.step("adding the collector to an already-running hub pairs without a restart", 4*time.Minute, func(ctx context.Context) error {
		if _, err := r.run(ctx, env3, 2*time.Minute, r.root, env3.binary(), "start", "hub"); err != nil {
			return err
		}
		output, err := r.run(ctx, env3, 3*time.Minute, r.root, env3.binary(), "install", "--yes", "--role", "both")
		if err != nil {
			return err
		}
		if !strings.Contains(output, "started: collector") || !strings.Contains(output, "running: hub (unchanged)") {
			return fmt.Errorf("install output does not show the collector started beside the unchanged hub:\n%s", output)
		}
		// The active hub never re-imported its bootstrap at startup, so this
		// only succeeds when the enrollment request imports the pending grant
		// lazily. The collector consumed it and deleted the bootstrap.
		return waitFor(ctx, 90*time.Second, 250*time.Millisecond, func() error {
			if err := requireFile(filepath.Join(env3.stateDir(), "collector", "credentials.json")); err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(env3.stateDir(), "collector-pairing.json")); !os.IsNotExist(err) {
				return fmt.Errorf("hub pairing bootstrap was not consumed: %v", err)
			}
			return nil
		})
	}); err != nil {
		return err
	}

	return nil
}

// ---------------------------------------------------------------------------
// Steps, commands and environment
// ---------------------------------------------------------------------------

func (r *runner) step(name string, timeout time.Duration, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	started := time.Now()
	fmt.Printf("\n== %s\n", name)
	if err := fn(ctx); err != nil {
		fmt.Printf("   FAIL (%s)\n", time.Since(started).Round(100*time.Millisecond))
		return fmt.Errorf("%s: %w", name, err)
	}
	fmt.Printf("   ok (%s)\n", time.Since(started).Round(100*time.Millisecond))
	return nil
}

func (r *runner) stepf(format string, args ...any) {
	fmt.Printf("   · "+format+"\n", args...)
}

func (r *runner) run(ctx context.Context, env *testEnv, timeout time.Duration, dir, name string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, name, args...)
	command.Dir = dir
	command.Env = env.environ()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		return output.String(), fmt.Errorf("%s %s timed out after %s\n%s", filepath.Base(name), strings.Join(args, " "), timeout, output.String())
	}
	if err != nil {
		return output.String(), fmt.Errorf("%s %s failed: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, output.String())
	}
	return output.String(), nil
}

type testEnv struct {
	work       string
	name       string
	home       string
	runtimeDir string
	tmpDir     string
	fakeRoot   string
	fakeBin    string
	caFile     string
	releaseURL string
	hubPort    int
}

func (r *runner) newEnv(name, fakeBin, caFile, releaseURL string, hubPort int) (*testEnv, error) {
	ensemble := filepath.Join(r.work, name)
	env := &testEnv{
		work:       ensemble,
		name:       name,
		home:       filepath.Join(ensemble, "home"),
		runtimeDir: filepath.Join(ensemble, "run"),
		tmpDir:     filepath.Join(ensemble, "tmp"),
		fakeRoot:   filepath.Join(ensemble, "fake-systemd"),
		fakeBin:    fakeBin,
		caFile:     caFile,
		releaseURL: releaseURL,
		hubPort:    hubPort,
	}
	for _, dir := range []string{env.home, env.runtimeDir, env.tmpDir, env.fakeRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	r.envs = append(r.envs, env)
	return env, nil
}

func (e *testEnv) binary() string {
	return filepath.Join(e.home, ".local", "bin", "rigscope")
}

func (e *testEnv) stateDir() string {
	return filepath.Join(e.home, ".local", "state", "rigscope")
}

func (e *testEnv) configHome() string {
	return filepath.Join(e.home, ".config")
}

// environ builds the complete environment for one installation. It starts from
// the allowlisted product environment (productEnviron) and adds every fixture
// setting explicitly, so no ambient RIGSCOPE_*, DBUS_*, SYSTEMD_*, XDG_*, proxy
// or CA override reaches the product process.
func (e *testEnv) environ() []string {
	return productEnviron(map[string]string{
		"HOME":                       e.home,
		"XDG_CONFIG_HOME":            e.configHome(),
		"XDG_STATE_HOME":             filepath.Join(e.home, ".local", "state"),
		"XDG_DATA_HOME":              filepath.Join(e.home, ".local", "share"),
		"XDG_RUNTIME_DIR":            e.runtimeDir,
		"TMPDIR":                     e.tmpDir,
		"PATH":                       sanitizedPath(e.fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")),
		"RIGSCOPE_FAKE_SYSTEMD_ROOT": e.fakeRoot,
		"RIGSCOPE_RELEASE_BASE_URL":  e.releaseURL + "/releases",
		"RIGSCOPE_INSTALL_BASE_URL":  e.releaseURL,
		"RIGSCOPE_VERSION":           versionOne,
		"SSL_CERT_FILE":              e.caFile,
		"CURL_CA_BUNDLE":             e.caFile,
		"RIGSCOPE_E2E":               "1",
	})
}

func verifyIsolation(fakeBin, work string) error {
	env := &testEnv{
		home:       filepath.Join(work, "isolation-home"),
		fakeBin:    fakeBin,
		fakeRoot:   filepath.Join(work, "isolation-fake-systemd"),
		runtimeDir: filepath.Join(work, "isolation-run"),
		tmpDir:     filepath.Join(work, "isolation-tmp"),
		caFile:     filepath.Join(work, "test-ca.pem"),
		releaseURL: "https://127.0.0.1:1",
	}
	for _, dir := range []string{env.home, env.fakeRoot, env.runtimeDir, env.tmpDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	for _, tool := range []string{"systemctl", "journalctl", "loginctl", "systemd-sysusers", "udevadm"} {
		command := exec.Command("sh", "-c", "command -v "+tool)
		command.Env = env.environ()
		output, err := command.Output()
		if err != nil {
			return fmt.Errorf("command -v %s failed: %w", tool, err)
		}
		got := strings.TrimSpace(string(output))
		want := filepath.Join(fakeBin, tool)
		if got != want {
			return fmt.Errorf("SAFETY: %s resolves to %q, want the fake %q; refusing to run", tool, got, want)
		}
	}
	homeCommand := exec.Command("sh", "-c", `printf '%s' "$HOME"`)
	homeCommand.Env = env.environ()
	output, err := homeCommand.Output()
	if err != nil {
		return err
	}
	gotHome := string(output)
	if !strings.HasPrefix(gotHome, work+string(os.PathSeparator)) {
		return fmt.Errorf("SAFETY: HOME %q is not under the disposable directory %q; refusing to run", gotHome, work)
	}
	return nil
}

// buildFakeTools runs the Go toolchain, not a product process, so it keeps the
// build-tool environment instead of the product allowlist.
func buildFakeTools(ctx context.Context, root, dest string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(dest, "systemctl"), "./scripts/e2e/fake-systemd")
	command.Dir = root
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build fake-systemd: %w\n%s", err, output)
	}
	for _, name := range []string{"journalctl", "loginctl", "systemd-sysusers", "udevadm"} {
		path := filepath.Join(dest, name)
		_ = os.Remove(path)
		if err := os.Symlink("systemctl", path); err != nil {
			return err
		}
	}
	return nil
}

// buildRelease runs go, npm and GNU tar through scripts/release/build.sh. It is
// build tooling rather than product execution, so it keeps the ambient
// environment; the installer it produces later runs under productEnviron.
func buildRelease(ctx context.Context, root, version, dest string) error {
	command := exec.CommandContext(ctx, "sh", filepath.Join(root, "scripts", "release", "build.sh"), version, dest)
	command.Dir = root
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("scripts/release/build.sh %s: %w\n%s", version, err, tail(string(output), 80))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Local HTTPS release server
// ---------------------------------------------------------------------------

type releaseServer struct {
	*httptest.Server
	caPEM    []byte
	versions map[string]string
}

func newReleaseServer(assetsDir, latest string) (*releaseServer, error) {
	versions := map[string]string{}
	entries, err := os.ReadDir(assetsDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			versions[entry.Name()] = filepath.Join(assetsDir, entry.Name())
		}
	}
	server := &releaseServer{versions: versions}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, "/download/"):
			server.serveDownload(w, request)
		case request.URL.Path == "/releases/latest":
			server.writeMetadata(w, request, latest)
		case strings.HasPrefix(request.URL.Path, "/releases/tags/"):
			version := strings.TrimPrefix(request.URL.Path, "/releases/tags/v")
			server.writeMetadata(w, request, version)
		default:
			http.NotFound(w, request)
		}
	})
	tlsServer, caPEM, err := newTLSServer(handler)
	if err != nil {
		return nil, err
	}
	server.Server = tlsServer
	server.caPEM = caPEM
	return server, nil
}

func (s *releaseServer) serveDownload(w http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/download/"), "/")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "v") {
		http.NotFound(w, request)
		return
	}
	version := strings.TrimPrefix(parts[0], "v")
	dir, ok := s.versions[version]
	if !ok {
		http.NotFound(w, request)
		return
	}
	path := filepath.Join(dir, parts[1])
	if !strings.HasPrefix(path, dir+string(os.PathSeparator)) {
		http.NotFound(w, request)
		return
	}
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, request)
		return
	}
	http.ServeFile(w, request, path)
}

func (s *releaseServer) writeMetadata(w http.ResponseWriter, request *http.Request, version string) {
	dir, ok := s.versions[version]
	if !ok {
		http.NotFound(w, request)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	base := "https://" + request.Host
	assets := make([]map[string]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		assets = append(assets, map[string]string{
			"name":                 entry.Name(),
			"browser_download_url": base + "/download/v" + version + "/" + entry.Name(),
		})
	}
	payload := map[string]any{"tag_name": "v" + version, "assets": assets}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *releaseServer) fetch(path string) ([]byte, error) {
	response, err := s.Client().Get(s.URL + path)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s = HTTP %d", path, response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

func newTLSServer(handler http.Handler) (*httptest.Server, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Rigscope E2E Release CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{leafDER},
		PrivateKey:  leafKey,
	}}}
	server.StartTLS()
	return server, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}

// ---------------------------------------------------------------------------
// Hub API client
// ---------------------------------------------------------------------------

type apiClient struct {
	http   *http.Client
	stream *http.Client
	jar    http.CookieJar
	base   string
	origin string
	csrf   string
}

func newAPI(server *releaseServer, port int) (*apiClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(server.caPEM) {
		return nil, errors.New("could not add the test CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	return &apiClient{
		http:   &http.Client{Transport: transport, Jar: jar, Timeout: 30 * time.Second},
		stream: &http.Client{Transport: transport, Jar: jar},
		jar:    jar,
		base:   "http://127.0.0.1:" + strconv.Itoa(port),
		origin: "http://127.0.0.1:" + strconv.Itoa(port),
	}, nil
}

func (c *apiClient) healthy(expected string) error {
	response, err := c.http.Get(c.base + "/api/health")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health = HTTP %d", response.StatusCode)
	}
	var payload struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return err
	}
	if payload.Status != "ok" {
		return fmt.Errorf("health status = %q", payload.Status)
	}
	if payload.Version != expected {
		return fmt.Errorf("health reports version %q, want %q", payload.Version, expected)
	}
	return nil
}

func (c *apiClient) login(ctx context.Context, password string) error {
	body, err := json.Marshal(map[string]string{"username": "owner", "password": password})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", c.origin)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("login = HTTP %d: %s", response.StatusCode, data)
	}
	var payload struct {
		Username  string `json:"username"`
		CSRFToken string `json:"csrfToken"`
		Admin     *bool  `json:"admin"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	if payload.Username != "owner" || payload.CSRFToken == "" || payload.Admin == nil || !*payload.Admin {
		return fmt.Errorf("login response is not an admin owner session: %s", data)
	}
	c.csrf = payload.CSRFToken
	return nil
}

func (c *apiClient) createGrant(ctx context.Context) (string, error) {
	data, status, err := c.mutate(ctx, http.MethodPost, "/api/admin/collector-grants", []byte("{}"))
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("create grant = HTTP %d: %s", status, data)
	}
	var payload struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", err
	}
	if payload.Token == "" {
		return "", errors.New("grant response has no token")
	}
	return payload.Token, nil
}

func (c *apiClient) collectors(ctx context.Context) ([]struct {
	CollectorID string `json:"collectorId"`
	MachineID   string `json:"machineId"`
	Revoked     bool   `json:"revoked"`
	UpdatedAt   string `json:"updatedAt"`
}, error) {
	data, status, err := c.get(ctx, "/api/admin/collectors")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("collectors = HTTP %d: %s", status, data)
	}
	var payload struct {
		Collectors []struct {
			CollectorID string `json:"collectorId"`
			MachineID   string `json:"machineId"`
			Revoked     bool   `json:"revoked"`
			UpdatedAt   string `json:"updatedAt"`
		} `json:"collectors"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	return payload.Collectors, nil
}

func (c *apiClient) liveOnce(ctx context.Context) (liveSnapshot, error) {
	snapshot, err := c.watchLive(ctx, func([]liveMachine) bool { return true })
	return snapshot, err
}

func (c *apiClient) waitForMachines(ctx context.Context, timeout time.Duration, predicate func([]liveMachine) bool) (liveSnapshot, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining > 10*time.Second {
			remaining = 10 * time.Second
		}
		watchCtx, cancel := context.WithTimeout(ctx, remaining)
		snapshot, err := c.watchLive(watchCtx, predicate)
		cancel()
		if err == nil {
			return snapshot, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return liveSnapshot{}, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return liveSnapshot{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return liveSnapshot{}, fmt.Errorf("live state did not satisfy the predicate within %s: %v", timeout, lastErr)
}

func (c *apiClient) watchLive(ctx context.Context, predicate func([]liveMachine) bool) (liveSnapshot, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/live", nil)
	if err != nil {
		return liveSnapshot{}, err
	}
	response, err := c.stream.Do(request)
	if err != nil {
		return liveSnapshot{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return liveSnapshot{}, fmt.Errorf("live = HTTP %d: %s", response.StatusCode, data)
	}
	reader := bufio.NewReaderSize(response.Body, 1<<20)
	var event string
	var data []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return liveSnapshot{}, errors.New("live stream ended")
			}
			return liveSnapshot{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if event != "" && len(data) > 0 {
				payload := strings.Join(data, "\n")
				var snapshot liveSnapshot
				if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
					return liveSnapshot{}, fmt.Errorf("decode live %s: %w", event, err)
				}
				if predicate(snapshot.Machines) {
					return snapshot, nil
				}
			}
			event, data = "", nil
			continue
		}
		switch {
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}

type liveSnapshot struct {
	HubRunID string        `json:"hubRunId"`
	Machines []liveMachine `json:"machines"`
}

type liveMachine struct {
	MachineID      string  `json:"machineId"`
	CollectorID    string  `json:"collectorId"`
	Hostname       string  `json:"hostname"`
	Connected      bool    `json:"connected"`
	LastReceivedAt *string `json:"lastReceivedAt"`
	Metrics        *struct {
		ReceivedAt string `json:"receivedAt"`
		CapturedAt string `json:"capturedAt"`
	} `json:"metrics"`
}

func freshMachine(machine liveMachine, window time.Duration) bool {
	if !machine.Connected || machine.Metrics == nil || machine.LastReceivedAt == nil {
		return false
	}
	received, err := parseTimestamp(*machine.LastReceivedAt)
	if err != nil {
		return false
	}
	return time.Since(received) <= window
}

func allFresh(machines []liveMachine, window time.Duration) bool {
	for _, machine := range machines {
		if !freshMachine(machine, window) {
			return false
		}
	}
	return true
}

func machineIDs(machines []liveMachine) map[string]bool {
	out := map[string]bool{}
	for _, machine := range machines {
		out[machine.MachineID] = true
	}
	return out
}

func parseTimestamp(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

type historyResult struct {
	MachineID string `json:"machineId"`
	StepMs    int64  `json:"stepMs"`
	Tier      string `json:"tier"`
	Series    []struct {
		Metric string `json:"metric"`
		Points []struct {
			T   int64    `json:"t"`
			Min *float64 `json:"min"`
			Max *float64 `json:"max"`
			Avg *float64 `json:"avg"`
		} `json:"points"`
	} `json:"series"`
}

func countPoints(result historyResult) int64 {
	var total int64
	for _, series := range result.Series {
		total += int64(len(series.Points))
	}
	return total
}

func (c *apiClient) waitForHistory(ctx context.Context, machineID string, from, to int64, minimum int64) (historyResult, error) {
	var last error
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		result, err := c.history(ctx, machineID, from, to)
		if err == nil && countPoints(result) >= minimum {
			return result, nil
		}
		last = err
		if ctx.Err() != nil {
			return historyResult{}, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return historyResult{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return historyResult{}, fmt.Errorf("history never reached %d points: %v", minimum, last)
}

func (c *apiClient) waitForHistoryAcross(ctx context.Context, machineID string, from, to, before, after int64) (historyResult, error) {
	var last error
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		result, err := c.history(ctx, machineID, from, to)
		if err == nil {
			var haveBefore, haveAfter bool
			for _, series := range result.Series {
				for _, point := range series.Points {
					if point.T < before {
						haveBefore = true
					}
					if point.T > after {
						haveAfter = true
					}
				}
			}
			if haveBefore && haveAfter {
				return result, nil
			}
			last = fmt.Errorf("points before=%v after=%v", haveBefore, haveAfter)
		} else {
			last = err
		}
		if ctx.Err() != nil {
			return historyResult{}, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return historyResult{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return historyResult{}, fmt.Errorf("history is not continuous across the upgrade: %v", last)
}

func (c *apiClient) history(ctx context.Context, machineID string, from, to int64) (historyResult, error) {
	query := url.Values{}
	query.Set("machineId", machineID)
	query.Set("metrics", "cpu.usage,memory.used")
	query.Set("from", strconv.FormatInt(from, 10))
	query.Set("to", strconv.FormatInt(to, 10))
	query.Set("maxPoints", "600")
	data, status, err := c.get(ctx, "/api/history?"+query.Encode())
	if err != nil {
		return historyResult{}, err
	}
	if status == http.StatusTooManyRequests {
		return historyResult{}, fmt.Errorf("history rate limited")
	}
	if status != http.StatusOK {
		return historyResult{}, fmt.Errorf("history = HTTP %d: %s", status, data)
	}
	var result historyResult
	if err := json.Unmarshal(data, &result); err != nil {
		return historyResult{}, err
	}
	return result, nil
}

func (c *apiClient) get(ctx context.Context, path string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Origin", c.origin)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, 0, err
	}
	return data, response.StatusCode, nil
}

func (c *apiClient) mutate(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", c.origin)
	request.Header.Set("X-CSRF-Token", c.csrf)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, 0, err
	}
	return data, response.StatusCode, nil
}

// ---------------------------------------------------------------------------
// CLI probes
// ---------------------------------------------------------------------------

// ownerPassword reads the generated owner password from the install output
// and checks the hub journal never carries it: the installer creates the
// owner account, so the service has no password to log.
func (r *runner) ownerPassword(ctx context.Context, env *testEnv, installOutput string) (string, error) {
	match := regexp.MustCompile(`(?m)sign in: owner / (\S+)$`).FindStringSubmatch(installOutput)
	if len(match) != 2 {
		return "", fmt.Errorf("install output does not show the owner password:\n%s", installOutput)
	}
	output, err := r.run(ctx, env, 20*time.Second, r.root, env.binary(), "logs", "hub")
	if err != nil {
		return "", err
	}
	if strings.Contains(output, match[1]) || strings.Contains(output, "Password: ") {
		return "", fmt.Errorf("the hub journal contains a password:\n%s", tail(output, 20))
	}
	return match[1], nil
}

func (r *runner) assertBinaryVersion(ctx context.Context, env *testEnv, version string) error {
	return r.assertBinaryVersionAt(ctx, env, env.binary(), version)
}

func (r *runner) assertBinaryVersionAt(ctx context.Context, env *testEnv, binary, version string) error {
	output, err := r.run(ctx, env, 30*time.Second, r.root, binary, "version")
	if err != nil {
		return err
	}
	fields := strings.Fields(output)
	if len(fields) == 0 || fields[len(fields)-1] != version {
		return fmt.Errorf("%s version = %q, want %s", binary, strings.TrimSpace(output), version)
	}
	return nil
}

func (r *runner) binaryVersionAt(ctx context.Context, env *testEnv, binary string) (string, error) {
	output, err := r.run(ctx, env, 30*time.Second, r.root, binary, "version")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return "", fmt.Errorf("%s version printed nothing", binary)
	}
	return fields[len(fields)-1], nil
}

func (r *runner) checkStatus(ctx context.Context, env *testEnv, version string) error {
	output, err := r.run(ctx, env, 60*time.Second, r.root, env.binary(), "status", "--json")
	if err != nil {
		return err
	}
	var report struct {
		Scope      string `json:"scope"`
		Version    string `json:"version"`
		Installed  bool   `json:"installed"`
		BinarySet  bool   `json:"binaryPresent"`
		Previous   bool   `json:"previousBinary"`
		Config     string `json:"config"`
		StateDir   string `json:"stateDir"`
		Components map[string]struct {
			Enabled   bool `json:"enabled"`
			Installed bool `json:"installed"`
			Active    bool `json:"active"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		return fmt.Errorf("parse status --json: %w\n%s", err, output)
	}
	if report.Scope != "user" || report.Version != version || !report.Installed || !report.BinarySet {
		return fmt.Errorf("unexpected status report: %+v", report)
	}
	for _, name := range []string{"hub", "collector"} {
		component, ok := report.Components[name]
		if !ok || !component.Enabled || !component.Installed || !component.Active {
			return fmt.Errorf("status reports %s = %+v, want enabled/installed/active", name, component)
		}
	}
	return nil
}

func (r *runner) checkDoctor(ctx context.Context, env *testEnv) error {
	output, err := r.run(ctx, env, 2*time.Minute, r.root, env.binary(), "doctor")
	if err != nil {
		return err
	}
	if !strings.Contains(output, "0 failure(s)") {
		return fmt.Errorf("doctor did not report zero failures:\n%s", output)
	}
	return nil
}

func (r *runner) assertNoFakeProcesses(env *testEnv) error {
	entries, err := os.ReadDir(filepath.Join(env.fakeRoot, "run"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(env.fakeRoot, "run", entry.Name()))
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if processExists(pid) {
			return fmt.Errorf("fake unit %s is still running as pid %d", entry.Name(), pid)
		}
	}
	return nil
}

func (r *runner) stopAllFakeServices() {
	for _, env := range r.envs {
		entries, err := os.ReadDir(filepath.Join(env.fakeRoot, "run"))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(env.fakeRoot, "run", entry.Name()))
			if err != nil {
				continue
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if !processExists(pid) {
				continue
			}
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		remaining := false
		for _, env := range r.envs {
			entries, _ := os.ReadDir(filepath.Join(env.fakeRoot, "run"))
			for _, entry := range entries {
				data, err := os.ReadFile(filepath.Join(env.fakeRoot, "run", entry.Name()))
				if err != nil {
					continue
				}
				pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
				if processExists(pid) {
					remaining = true
				}
			}
		}
		if !remaining {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, env := range r.envs {
		entries, _ := os.ReadDir(filepath.Join(env.fakeRoot, "run"))
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(env.fakeRoot, "run", entry.Name()))
			if err != nil {
				continue
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if processExists(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Diagnostics
// ---------------------------------------------------------------------------

func (r *runner) dumpDiagnostics() {
	fmt.Fprintf(os.Stderr, "\n===== e2e diagnostics (%s) =====\n", r.work)
	for _, env := range r.envs {
		fmt.Fprintf(os.Stderr, "\n--- %s: fake systemctl commands ---\n", env.name)
		printTail(filepath.Join(env.fakeRoot, "commands.log"), 80)
		if entries, err := os.ReadDir(filepath.Join(env.fakeRoot, "journal")); err == nil {
			for _, entry := range entries {
				fmt.Fprintf(os.Stderr, "\n--- %s: journal %s ---\n", env.name, entry.Name())
				printTail(filepath.Join(env.fakeRoot, "journal", entry.Name()), 120)
			}
		}
		fmt.Fprintf(os.Stderr, "\n--- %s: units and state ---\n", env.name)
		for _, path := range []string{
			filepath.Join(env.home, ".config", "systemd", "user"),
			filepath.Join(env.home, ".config", "rigscope"),
			filepath.Join(env.home, ".local", "state", "rigscope"),
			filepath.Join(env.fakeRoot, "run"),
		} {
			printListing(path)
		}
	}
}

func printTail(path string, lines int) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "(no %s: %v)\n", path, err)
		return
	}
	fmt.Fprint(os.Stderr, tail(string(data), lines))
}

func printListing(path string) {
	fmt.Fprintf(os.Stderr, "%s:\n", path)
	entries, err := os.ReadDir(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  (%v)\n", err)
		return
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		fmt.Fprintf(os.Stderr, "  %-10s %8d  %s\n", info.Mode().Perm(), info.Size(), entry.Name())
	}
}

func tail(text string, lines int) string {
	parts := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n") + "\n"
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("unexpected listener address")
	}
	return address.Port, nil
}

func requireFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("expected %s to exist: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("expected %s to be a file, not a directory", path)
	}
	return nil
}

func requireMissing(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("expected %s to be removed, but it still exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readPID(env *testEnv, unit string) (int, error) {
	data, err := os.ReadFile(filepath.Join(env.fakeRoot, "run", unit+".pid"))
	if err != nil {
		return 0, fmt.Errorf("read pid for %s: %w", unit, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid pid for %s: %q", unit, data)
	}
	return pid, nil
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	if data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

func journalPhase(env *testEnv) (string, error) {
	data, err := os.ReadFile(filepath.Join(env.stateDir(), "upgrade-journal.json"))
	if err != nil {
		return "", err
	}
	var journal struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		return "", err
	}
	return journal.Phase, nil
}

func waitFor(ctx context.Context, timeout, interval time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if err := fn(); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("condition not met within %s: %v", timeout, last)
}
