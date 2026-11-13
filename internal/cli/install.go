package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

var installComponents = []systemd.Component{systemd.Hub, systemd.Collector}

// Install phases are named so tests can interrupt each one and prove a retry
// recovers without adopting foreign files.
const (
	installStepManifest   = "manifest"
	installStepDirs       = "dirs"
	installStepSysusers   = "sysusers"
	installStepStateDirs  = "state-dirs"
	installStepConfig     = "config"
	installStepGPU        = "gpu-registers"
	installStepPairing    = "pairing"
	installStepEnrollment = "enrollment"
	installStepUdev       = "udev"
	installStepBinary     = "binary"
	installStepUnits      = "units"
	installStepServices   = "services"
)

var installStepNames = []string{
	installStepManifest,
	installStepDirs,
	installStepSysusers,
	installStepStateDirs,
	installStepConfig,
	installStepGPU,
	installStepPairing,
	installStepEnrollment,
	installStepUdev,
	installStepBinary,
	installStepUnits,
	installStepServices,
}

// setupMode separates a full install from configure, which reuses the same
// manifest-checked pipeline to change settings of an existing installation.
type setupMode int

const (
	modeInstall setupMode = iota
	modeConfigure
)

// setupRequest is one parsed install or configure invocation.
type setupRequest struct {
	mode          setupMode
	selected      role
	system        bool
	boot          bool
	noStart       bool
	noRestart     bool
	yes           bool
	gpuRegisters  bool
	port          int
	origin        string
	clearOrigin   bool
	telemetryMode string
	// trustedProxies replaces the hub's trusted proxy list when
	// setTrustedProxies is true (configure --trusted-proxy).
	trustedProxies    []string
	setTrustedProxies bool
	hubURL            string
	hubCA             string
	token             string
	// passwordStdin reads the owner's first password from standard input.
	passwordStdin bool
	// roleSet is true when --role was given. Re-running install on an
	// existing installation changes components only when asked to.
	roleSet bool
	// edit carries the configure-only settings.
	edit configureEdit
}

func runInstall(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("install", deps.Stderr)
	role := flags.String("role", "both", "initial component preset: hub, collector or both")
	system := flags.Bool("system", false, "install root-owned system services")
	boot := flags.Bool("boot", false, "enable startup at boot (user scope: loginctl linger)")
	noStart := flags.Bool("no-start", false, "write everything but do not start services")
	gpuRegisters := flags.Bool("gpu-registers", false, "opt in to NVIDIA PCI BAR0 register temperature access (collector only)")
	port := flags.Int("port", -1, "hub listen port")
	origin := flags.String("origin", "", "public hub origin such as https://hub.example.com")
	hubURL := flags.String("hub", "", "hub URL recorded as the collector destination")
	hubCA := flags.String("hub-ca", "", "optional pinned CA bundle for the hub destination")
	token := flags.String("token", "", "one-time enrollment grant stored privately for the collector")
	passwordStdin := flags.Bool("password-stdin", false, "read the owner's first password from standard input")
	yes := addYesFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		return deps.errorf("install takes no positional arguments, got %q", flags.Arg(0))
	}
	selected, err := parseRole(*role)
	if err != nil {
		return deps.errorf("%v", err)
	}
	roleSet := false
	flags.Visit(func(f *flag.Flag) { roleSet = roleSet || f.Name == "role" })
	return runSetup(ctx, deps, setupRequest{
		roleSet:       roleSet,
		mode:          modeInstall,
		selected:      selected,
		system:        *system,
		boot:          *boot,
		noStart:       *noStart,
		yes:           *yes,
		gpuRegisters:  *gpuRegisters,
		port:          *port,
		origin:        *origin,
		hubURL:        *hubURL,
		hubCA:         *hubCA,
		token:         *token,
		passwordStdin: *passwordStdin,
	})
}

func runSetup(ctx context.Context, deps Deps, req setupRequest) int {
	command := "install"
	if req.mode == modeConfigure {
		command = "configure"
	}
	system, boot, noStart, gpuRegisters := &req.system, &req.boot, &req.noStart, &req.gpuRegisters
	port, origin, hubURL, hubCA, token := &req.port, &req.origin, &req.hubURL, &req.hubCA, &req.token
	selected := req.selected
	// Re-running install acts on the existing installation: with only a system
	// install present, "rigscope install --gpu-registers" changes that one
	// rather than starting a second, per-user hub beside it.
	*system = deps.installedScope(*system) == config.ScopeSystem
	scope := scopeFrom(*system)
	if code, done := deps.ensureRoot(scope); done {
		return code
	}
	if (*hubURL != "") != (*token != "") {
		return deps.errorf("--hub and --token must be used together")
	}
	if *hubCA != "" && *hubURL == "" {
		return deps.errorf("--hub-ca requires --hub")
	}
	if *hubURL != "" {
		normalized, err := config.NormalizeHubURL(*hubURL)
		if err != nil {
			return deps.errorf("install: %v", err)
		}
		*hubURL = normalized
	}
	if *hubURL != "" && selected != roleBoth && selected != roleCollector {
		return deps.errorf("--hub requires --role collector or --role both")
	}
	if *gpuRegisters && selected == roleHub {
		return deps.errorf("--gpu-registers requires the collector component; use --role collector or --role both")
	}
	paths, err := deps.paths(scope)
	if err != nil {
		return deps.errorf("%v", err)
	}
	// First read-only pass: refuse 0.2 installations and foreign files before
	// creating a lock file or any directory.
	firstLook, err := preflightSetup(paths, req.mode == modeInstall)
	if err != nil {
		return deps.errorf("%v", err)
	}

	cfg, existing, err := loadOrCreate(paths.ConfigFile)
	if err != nil {
		return deps.errorf("%v", err)
	}
	priorCfg := *cfg
	priorCfg.Normalize()
	priorHub := priorCfg.Components.Hub
	var before config.Config
	if req.mode == modeConfigure {
		if !existing || firstLook == nil {
			return deps.errorf("configure: no Rigscope installation in %s scope; run rigscope install first", scopeLabel(scope))
		}
		if firstLook.Version != "" && firstLook.Version != deps.buildVersion() {
			return deps.errorf("configure: this binary is Rigscope %s but the installation is %s; run %s configure instead", deps.buildVersion(), firstLook.Version, paths.Binary)
		}
		cfg.Normalize()
		before = cloneConfig(cfg)
		req.edit.apply(cfg)
	} else if !existing || req.roleSet {
		applyRole(cfg, selected, !existing)
	}
	if *port != -1 {
		if *port < 1 || *port > 65535 {
			return deps.errorf("--port must be between 1 and 65535")
		}
		cfg.Components.Hub.Port = *port
	}
	if *origin != "" {
		cfg.Components.Hub.Origin = *origin
	}
	if req.clearOrigin {
		cfg.Components.Hub.Origin = ""
	}
	if req.telemetryMode != "" {
		cfg.Components.Hub.TelemetryMode = req.telemetryMode
	}
	if req.setTrustedProxies {
		cfg.Components.Hub.TrustedProxies = req.trustedProxies
	}
	if *hubURL != "" {
		cfg.Components.Collector.Destinations = []config.Destination{{URL: *hubURL, CAFile: *hubCA}}
	}
	if *gpuRegisters {
		if !cfg.Components.Collector.Enabled {
			return deps.errorf("--gpu-registers requires the collector component; use --role collector or --role both")
		}
		cfg.Components.Collector.Hardware.GPURegisters = true
	}
	cfg.Normalize()
	var sensitive []shareFamily
	if req.mode == modeConfigure {
		if err := req.edit.checkComponents(cfg); err != nil {
			return deps.errorf("configure: %v", err)
		}
		// The listener check comes first: it explains an exposed hub in
		// configure's own terms before the schema validator would.
		if err := hubListenerProblem(&before, cfg); err != nil {
			return deps.errorf("configure: %v", err)
		}
		if err := cfg.Validate(); err != nil {
			return deps.errorf("configure: %v", err)
		}
		notes, err := deps.checkHubResult(paths, &before, cfg)
		if err != nil {
			return deps.errorf("configure: %v", err)
		}
		if reflect.DeepEqual(before, *cfg) {
			fmt.Fprintln(deps.Stdout, "Nothing to change; this installation already uses these settings")
			return 0
		}
		fmt.Fprintln(deps.Stdout, deps.out(styleBold, "Changes:"))
		for _, change := range settingChanges(paths, &before, cfg) {
			fmt.Fprintf(deps.Stdout, "  %s\n", change)
		}
		for _, note := range append(configureNotes(&before, cfg), notes...) {
			fmt.Fprintf(deps.Stdout, "  %s %s\n", deps.out(styleYellow, "note:"), note)
		}
		sensitive = sensitiveOptIns(before.Components.Collector.Sharing, cfg.Components.Collector.Sharing)
		if !cfg.Components.Collector.Enabled {
			sensitive = nil
		}
	} else if err := cfg.Validate(); err != nil {
		return deps.errorf("%v", err)
	}

	// --hub on an existing collector: decide (and confirm a move) before
	// anything is written. The destination may be rewritten to the hub's
	// local address, which the restart decision below must see.
	var destination destinationPlan
	if req.mode == modeInstall {
		destination, err = deps.planDestination(paths, existing, &priorCfg, cfg, hubURL, hubCA, req.yes)
		if err != nil {
			return deps.errorf("%s: %v", command, err)
		}
	}

	// Services that are already running keep their old binary and settings
	// until they restart: systemctl start does nothing for an active unit.
	// Ask once, before anything is written, then restart them at the end.
	// install --no-start leaves running services untouched; configure
	// --no-restart stops them for the change and leaves them stopped.
	manager := deps.manager(scope)
	active := activeComponents(ctx, manager, installedComponents(paths))
	running := deps.componentsToRestart(paths, active, &priorCfg, cfg, req.mode == modeInstall, plannedHardware(paths, cfg, firstLook))
	var unchanged []systemd.Component
	for _, component := range active {
		if !containsComponent(running, component) {
			unchanged = append(unchanged, component)
		}
	}
	leaveStopped := req.noRestart
	untouched := []systemd.Component(nil)
	if *noStart && !leaveStopped {
		untouched, running = running, nil
	}
	effect := "restart so the changes take effect"
	if leaveStopped {
		effect = "stop and stay stopped"
	}
	var plan configureServicePlan
	if req.mode == modeConfigure {
		plan = planConfigureServices(active, running, &before, cfg, leaveStopped)
		if err := deps.approveConfigure(plan, leaveStopped, sensitive, req.yes); err != nil {
			return deps.errorf("%s: %v", command, err)
		}
	} else {
		replacesLegacy := scope == config.ScopeSystem && previewLegacyRules(deps, paths, buildManifest(paths, deps.buildVersion(), plannedHardware(paths, cfg, firstLook)))
		if replacesLegacy && len(running) == 0 && !req.yes {
			approved, err := deps.confirm("Replace the recognized legacy hardware rules and continue?")
			if err != nil {
				return deps.errorf("confirm legacy rule replacement: %v; run again with --yes to approve", err)
			}
			if !approved {
				return deps.errorf("%v", errCancelled)
			}
		}
		// A confirmed move already covers restarting the collector it moves.
		approved := req.yes || (destination.moveFrom != "" && len(running) == 1 && running[0] == systemd.Collector)
		if err := deps.approveServiceStop(running, effect, approved); err != nil {
			return deps.errorf("%s: %v", command, err)
		}
	}

	// A hub that has never started gets its owner account from the installer,
	// so the first password is shown here (or chosen by the operator) instead
	// of being printed by the service into the journal. It is asked before
	// anything is written, so a cancelled prompt leaves no trace.
	var owner *passwordChoice
	// configure --hub on for a hub that never ran sets it up the same way.
	hubTurnedOn := req.mode == modeConfigure && !before.Components.Hub.Enabled
	if (req.mode == modeInstall || hubTurnedOn) && cfg.Components.Hub.Enabled && hubIsFresh(paths) {
		choice, err := deps.choosePassword(ownerUsername, req.passwordStdin, req.yes)
		if err != nil {
			return deps.errorf("%v", err)
		}
		owner = &choice
	} else if req.passwordStdin {
		return deps.errorf("--password-stdin sets the first owner password, but this hub already has accounts; use rigscope reset-password --password-stdin")
	}

	held, err := acquireLock(paths)
	if err != nil {
		return deps.errorf("%v", err)
	}
	defer held.Release()

	// Re-check under the lock so a concurrent operation cannot turn an
	// unowned path into an overwritten one.
	previous, err := preflightSetup(paths, req.mode == modeInstall)
	if err != nil {
		return deps.errorf("%v", err)
	}
	// An enrolled collector redeems the grant now: a move redeems with the
	// new hub before the configuration changes (a refusal returns here), and
	// a re-enrollment replaces the credential it holds.
	if err := deps.redeemGrant(ctx, paths, &destination, *hubURL, *hubCA, *token); err != nil {
		return deps.errorf("%s: %v", command, err)
	}
	// A running collector reads its credential only when a session starts, so
	// one that would otherwise keep running restarts to use the new one.
	if destination.restartsCollector() && containsComponent(unchanged, systemd.Collector) {
		unchanged = removeComponent(unchanged, systemd.Collector)
		if *noStart {
			untouched = append(untouched, systemd.Collector)
		} else {
			running = append(running, systemd.Collector)
		}
	}

	hardware := plannedHardware(paths, cfg, previous)
	removedIntegration, err := removeStaleIntegration(previous, buildManifest(paths, deps.buildVersion(), hardware))
	if err != nil {
		return deps.errorf("%v", err)
	}

	if err := deps.installStep(installStepManifest); err != nil {
		return deps.errorf("install interrupted: %v", err)
	}
	// The state root must exist before the manifest can be written. Writing
	// the planned manifest immediately afterwards makes a state directory
	// created by an interrupted run recoverable: a retry recognises every
	// intended path and never mistakes a planned file for a foreign one.
	if err := ensureStateRoot(paths); err != nil {
		return deps.errorf("%v", err)
	}
	wanted := buildManifest(paths, deps.buildVersion(), hardware)
	carryOwnership(wanted, previous)
	if err := writeManifest(paths.ManifestFile(), wanted); err != nil {
		return deps.errorf("%v", err)
	}
	// The state directory exists (the manifest is inside it) and install just
	// created or adopted it, so record it as owned before creating anything
	// else. An interruption after this point can be uninstalled cleanly.
	if err := markPathOwned(wanted, paths, paths.StateDir, kindStateDir); err != nil {
		return deps.errorf("%v", err)
	}

	if err := deps.installStep(installStepDirs); err != nil {
		return deps.errorf("install interrupted: %v", err)
	}
	if _, err := paths.EnsureDirs(); err != nil {
		return deps.errorf("create installation directories: %v", err)
	}
	if err := ensureExtraDirs(paths); err != nil {
		return deps.errorf("%v", err)
	}
	for _, spec := range []struct {
		path string
		kind string
	}{
		{paths.StateDir, kindStateDir},
		{paths.BinaryDir, kindBinaryTemps},
		{paths.StagingDir(), kindStagingDir},
		{paths.BackupsDir(), kindBackupsDir},
	} {
		if err := markPathOwned(wanted, paths, spec.path, spec.kind); err != nil {
			return deps.errorf("%v", err)
		}
	}

	// systemd-sysusers must run before any chown: on a fresh system the
	// accounts do not exist yet, so resolving a name for chown would fail.
	if scope == config.ScopeSystem {
		if err := deps.installStep(installStepSysusers); err != nil {
			return deps.errorf("install interrupted: %v", err)
		}
		if err := writeSysusers(deps, paths, wanted, hardware); err != nil {
			return deps.errorf("%v", err)
		}
		if err := applySysusers(ctx, deps, paths); err != nil {
			return deps.errorf("%v", err)
		}
	}

	if err := deps.installStep(installStepStateDirs); err != nil {
		return deps.errorf("install interrupted: %v", err)
	}
	if err := ensureStateDirs(deps, paths, wanted); err != nil {
		return deps.errorf("%v", err)
	}

	// A fresh both-components install, or a later --role both that enables the
	// collector on a hub-only install, pairs the local collector automatically.
	// The destination is part of the configuration, so decide it before the
	// configuration is written; the grant material is staged afterwards. An
	// explicit --hub destination owns pairing.
	var autoPair *autoPairing
	if *hubURL == "" {
		autoPair, err = planAutoPairing(deps, paths, cfg, priorHub)
		if err != nil {
			return deps.errorf("%v", err)
		}
	}

	if err := deps.installStep(installStepGPU); err != nil {
		return deps.errorf("install interrupted: %v", err)
	}
	// User installs decide gpuRegisters from what the current user can read,
	// so this runs before the configuration is written.
	if req.mode == modeInstall && scope == config.ScopeUser && cfg.Components.Collector.Enabled && cfg.Components.Collector.Hardware.GPURegisters {
		if err := setupUserGPURegisters(ctx, deps, paths, wanted, cfg); err != nil {
			return deps.errorf("%v", err)
		}
	}

	if err := deps.installStep(installStepConfig); err != nil {
		return deps.errorf("install interrupted: %v", err)
	}
	if err := autoPair.rebind(deps, paths); err != nil {
		return deps.errorf("%v", err)
	}
	if err := writeConfigFile(deps, paths, wanted, cfg); err != nil {
		return deps.errorf("%v", err)
	}
	if err := autoPair.finishRebind(deps, paths); err != nil {
		return deps.errorf("%v", err)
	}

	if err := deps.installStep(installStepPairing); err != nil {
		return deps.errorf("install interrupted: %v", err)
	}
	if autoPair != nil {
		if err := autoPair.stage(deps, paths, wanted); err != nil {
			return deps.errorf("%v", err)
		}
	}

	if owner != nil {
		if err := writeOwnerBootstrap(deps, paths, wanted, owner.password); err != nil {
			return deps.errorf("%v", err)
		}
	}

	if *hubURL != "" && destination.stagesGrant() {
		if err := deps.installStep(installStepEnrollment); err != nil {
			return deps.errorf("install interrupted: %v", err)
		}
		if err := writeEnrollmentGrant(deps, paths, wanted, *hubURL, *token); err != nil {
			return deps.errorf("%v", err)
		}
	}

	if scope == config.ScopeSystem {
		if err := deps.installStep(installStepUdev); err != nil {
			return deps.errorf("install interrupted: %v", err)
		}
		if err := writeUdevFiles(deps, paths, wanted, hardware); err != nil {
			return deps.errorf("%v", err)
		}
		if err := applyUdev(ctx, deps, paths, hardware, removedIntegration); err != nil {
			return deps.errorf("%v", err)
		}
	}

	if err := deps.installStep(installStepBinary); err != nil {
		return deps.errorf("install interrupted: %v", err)
	}
	// configure keeps the installed binary; install publishes the running one.
	if req.mode == modeConfigure {
		if err := markPathOwned(wanted, paths, paths.Binary, kindBinary); err != nil {
			return deps.errorf("%v", err)
		}
	} else if err := installBinary(deps, paths, wanted); err != nil {
		return deps.errorf("%v", err)
	}

	if err := deps.installStep(installStepUnits); err != nil {
		return deps.errorf("install interrupted: %v", err)
	}
	if err := writeUnits(deps, paths, hardware, wanted); err != nil {
		return deps.errorf("%v", err)
	}

	if err := deps.installStep(installStepServices); err != nil {
		return deps.errorf("install interrupted: %v", err)
	}
	if err := manager.DaemonReload(ctx); err != nil {
		return deps.errorf("reload systemd: %v", err)
	}
	for _, component := range installComponents {
		unit := component.UnitName()
		enabled := componentEnabled(cfg, component)
		if enabled {
			if err := manager.Enable(ctx, unit); err != nil {
				return deps.errorf("enable %s: %v", unit, err)
			}
			continue
		}
		_ = manager.Disable(ctx, unit)
	}
	if *boot && scope == config.ScopeUser {
		if err := manager.EnableLinger(ctx); err != nil {
			return deps.errorf("enable boot startup: %v", err)
		}
	}
	started, restarted, stopped, kept, turnedOff := []string{}, []string{}, []string{}, []string{}, []string{}
	if req.mode == modeConfigure {
		started, restarted, stopped, kept, turnedOff, err = deps.runConfigurePlan(ctx, manager, plan, leaveStopped)
		if err != nil {
			return deps.errorf("%v", err)
		}
	} else if leaveStopped {
		// Stop in quiesce order: the collector before its hub.
		for i := len(installComponents) - 1; i >= 0; i-- {
			component := installComponents[i]
			if !containsComponent(running, component) {
				continue
			}
			if err := manager.Stop(ctx, component.UnitName()); err != nil {
				return deps.errorf("stop %s: %v", component, err)
			}
			stopped = append(stopped, component.String())
		}
	} else if !*noStart {
		// A running service keeps its old binary and settings until it
		// restarts, so each running component this run changes restarts. One
		// with nothing new keeps running (a running hub imports a new
		// collector's pairing grant without a restart); the rest start fresh.
		for _, component := range installComponents {
			if !componentEnabled(cfg, component) {
				continue
			}
			unit := component.UnitName()
			if containsComponent(unchanged, component) {
				kept = append(kept, component.String())
				continue
			}
			if containsComponent(running, component) {
				if err := manager.Restart(ctx, unit); err != nil {
					return deps.errorf("restart %s: %v", component, err)
				}
				restarted = append(restarted, component.String())
				continue
			}
			if err := manager.Start(ctx, unit); err != nil {
				return deps.errorf("start %s: %v", component, err)
			}
			started = append(started, component.String())
		}
	}

	if req.mode == modeConfigure {
		fmt.Fprintf(deps.Stdout, "%s (%s scope)\n", deps.out(styleGreen+";"+styleBold, "Updated Rigscope configuration"), scopeLabel(scope))
		fmt.Fprintf(deps.Stdout, "  config: %s\n", paths.ConfigFile)
		printServiceOutcome(deps, started, restarted, stopped, kept, true)
		for _, name := range turnedOff {
			fmt.Fprintf(deps.Stdout, "  %s %s (turn it on again with rigscope configure --%s on)\n", deps.out(styleYellow, "turned off:"), name, name)
		}
		for _, component := range plan.idle {
			fmt.Fprintf(deps.Stdout, "  %s %s (start it with rigscope start %s)\n", deps.out(styleDim, "not running:"), component, component)
		}
		if autoPair != nil && autoPair.rebindFrom != "" {
			fmt.Fprintln(deps.Stdout, "  collector: moved to the hub's new local address")
		}
		if autoPair != nil && autoPair.notice != "" {
			fmt.Fprintf(deps.Stdout, "  pairing: %s\n", autoPair.notice)
		}
		if cfg.Components.Hub.Enabled {
			fmt.Fprintf(deps.Stdout, "  open:    %s\n", deps.out(styleCyan, hubURLForDisplay(cfg)))
			printProxyHint(deps, cfg)
			if before.Components.Hub.Enabled && (before.Components.Hub.Origin != cfg.Components.Hub.Origin || before.Components.Hub.Port != cfg.Components.Hub.Port ||
				(before.Components.Hub.TLSCert == "") != (cfg.Components.Hub.TLSCert == "")) {
				fmt.Fprintln(deps.Stdout, deps.out(styleDim, "  Collectors on other machines keep the old address; give them a Replacement grant for the new one."))
			}
		}
		printOwnerSignIn(deps, owner)
		return 0
	}
	if firstLook != nil && firstLook.Version != "" && firstLook.Version != deps.buildVersion() {
		fmt.Fprintf(deps.Stdout, "%s from %s to %s (%s scope)\n", deps.out(styleGreen+";"+styleBold, "Upgraded Rigscope"), firstLook.Version, deps.buildVersion(), scopeLabel(scope))
	} else {
		fmt.Fprintf(deps.Stdout, "%s %s (%s scope)\n", deps.out(styleGreen+";"+styleBold, "Installed Rigscope"), deps.buildVersion(), scopeLabel(scope))
	}
	fmt.Fprintf(deps.Stdout, "  binary: %s\n", paths.Binary)
	fmt.Fprintf(deps.Stdout, "  config: %s\n", paths.ConfigFile)
	fmt.Fprintf(deps.Stdout, "  state:  %s\n", paths.StateDir)
	fmt.Fprintf(deps.Stdout, "  units:  %s\n", paths.UnitDir)
	fmt.Fprintf(deps.Stdout, "  enabled: %s\n", strings.Join(enabledComponents(cfg), ", "))
	if cfg.Components.Collector.Hardware.GPURegisters {
		fmt.Fprintln(deps.Stdout, "  gpu registers: enabled (opt-in)")
	}
	if *noStart {
		fmt.Fprintln(deps.Stdout, "  services not started (--no-start)")
		if len(untouched) > 0 {
			deps.warnf(deps.Stdout, "the running %s keep the previous binary and settings until rigscope restart", componentList(untouched))
		}
	} else {
		printServiceOutcome(deps, started, restarted, stopped, kept, leaveStopped)
	}
	if autoPair != nil && autoPair.notice != "" {
		fmt.Fprintf(deps.Stdout, "  pairing: %s\n", autoPair.notice)
	}
	if *hubURL != "" {
		printDestinationPlan(deps, destination, *hubURL)
	}
	if cfg.Components.Hub.Enabled {
		fmt.Fprintf(deps.Stdout, "  open:    %s\n", deps.out(styleCyan, hubURLForDisplay(cfg)))
		printProxyHint(deps, cfg)
	}
	printOwnerSignIn(deps, owner)
	return 0
}

// printOwnerSignIn shows how to sign in for the first time. A generated
// password is printed once; a chosen one never is.
func printOwnerSignIn(deps Deps, owner *passwordChoice) {
	if owner == nil {
		return
	}
	if owner.chosen {
		fmt.Fprintf(deps.Stdout, "  sign in: %s, with the password you chose\n", ownerUsername)
		return
	}
	fmt.Fprintf(deps.Stdout, "  sign in: %s / %s\n", ownerUsername, deps.out(styleBold, owner.password))
	fmt.Fprintf(deps.Stdout, "  %s\n", deps.out(styleYellow, "Save this password; it is shown once. To change it: rigscope reset-password"))
}

// installedComponents returns the components whose unit file exists. Only
// those can be running as Rigscope services.
func installedComponents(paths config.Paths) []systemd.Component {
	var installed []systemd.Component
	for _, component := range installComponents {
		if _, err := os.Stat(paths.UnitPath(component.UnitName())); err == nil {
			installed = append(installed, component)
		}
	}
	return installed
}

// printServiceOutcome reports what install or configure did with each service.
func printServiceOutcome(deps Deps, started, restarted, stopped, kept []string, leaveStopped bool) {
	if len(restarted) > 0 {
		fmt.Fprintf(deps.Stdout, "  %s %s\n", deps.out(styleGreen, "restarted:"), strings.Join(restarted, ", "))
	}
	if len(started) > 0 {
		fmt.Fprintf(deps.Stdout, "  %s %s\n", deps.out(styleGreen, "started:"), strings.Join(started, ", "))
	}
	if len(stopped) > 0 {
		fmt.Fprintf(deps.Stdout, "  %s %s (start them with rigscope start)\n", deps.out(styleYellow, "stopped:"), strings.Join(stopped, ", "))
	}
	if len(kept) > 0 {
		fmt.Fprintf(deps.Stdout, "  %s %s (unchanged)\n", deps.out(styleGreen, "running:"), strings.Join(kept, ", "))
	}
	if len(started)+len(restarted)+len(stopped)+len(kept) == 0 && !leaveStopped {
		fmt.Fprintln(deps.Stdout, "  no components enabled; nothing to start")
	}
}

func (d Deps) installStep(step string) error {
	if d.InstallStep == nil {
		return nil
	}
	return d.InstallStep(step)
}

func (d Deps) publishStep(path, phase string) error {
	if d.PublishStep == nil {
		return nil
	}
	return d.PublishStep(path, phase)
}

type role int

const (
	roleHub role = iota
	roleCollector
	roleBoth
)

func parseRole(value string) (role, error) {
	switch value {
	case "hub":
		return roleHub, nil
	case "collector":
		return roleCollector, nil
	case "both", "":
		return roleBoth, nil
	default:
		return 0, fmt.Errorf(`--role must be "hub", "collector" or "both", got %q`, value)
	}
}

func applyRole(cfg *config.Config, selected role, fresh bool) {
	if fresh {
		cfg.Components.Hub.Enabled = selected == roleHub || selected == roleBoth
		cfg.Components.Collector.Enabled = selected == roleCollector || selected == roleBoth
		return
	}
	// Re-running install repairs an installation. It may add a component but
	// never silently disables one the user already enabled.
	if selected == roleHub || selected == roleBoth {
		cfg.Components.Hub.Enabled = true
	}
	if selected == roleCollector || selected == roleBoth {
		cfg.Components.Collector.Enabled = true
	}
}

func loadOrCreate(path string) (*config.Config, bool, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return config.Default(), false, nil
	}
	return nil, false, err
}

func componentEnabled(cfg *config.Config, component systemd.Component) bool {
	if component == systemd.Hub {
		return cfg.Components.Hub.Enabled
	}
	return cfg.Components.Collector.Enabled
}

func enabledComponents(cfg *config.Config) []string {
	var names []string
	for _, component := range installComponents {
		if componentEnabled(cfg, component) {
			names = append(names, component.String())
		}
	}
	return names
}

func stateRootMode(paths config.Paths) os.FileMode {
	if paths.Scope == config.ScopeSystem {
		return 0o755
	}
	return 0o700
}

// ensureStateRoot creates the state directory before the manifest exists. An
// interrupted run leaves an empty directory, which a retry accepts; a
// non-empty foreign directory is refused by preflight before this runs.
func ensureStateRoot(paths config.Paths) error {
	if err := os.MkdirAll(paths.StateDir, stateRootMode(paths)); err != nil {
		return fmt.Errorf("create state directory %s: %w", paths.StateDir, err)
	}
	if err := os.Chmod(paths.StateDir, stateRootMode(paths)); err != nil {
		return fmt.Errorf("set mode on state directory %s: %w", paths.StateDir, err)
	}
	dir, err := fsutil.OpenDirNoFollow(paths.StateDir)
	if err != nil {
		return fmt.Errorf("verify state directory %s: %w", paths.StateDir, err)
	}
	return dir.Close()
}

func ensureExtraDirs(paths config.Paths) error {
	for _, dir := range []string{paths.StagingDir(), paths.BackupsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("set mode on %s: %w", dir, err)
		}
		if handle, err := fsutil.OpenDirNoFollow(dir); err != nil {
			return fmt.Errorf("verify %s: %w", dir, err)
		} else {
			_ = handle.Close()
		}
	}
	return nil
}

// markPathOwned records the identity of a path install just created and
// rewrites the manifest. The manifest write happens after the file exists, so
// a crash can never claim a path that was never created.
func markPathOwned(m *manifest, paths config.Paths, path, kind string) error {
	entry := m.entry(path, kind)
	if entry == nil {
		return fmt.Errorf("internal error: %s is not part of the installation manifest", path)
	}
	return recordPathOwned(m, paths, entry, path)
}

// recordPathOwned records the current identity and digest of path in entry and
// rewrites the manifest. The staged-publication fields are cleared because the
// file is now durably in place.
func recordPathOwned(m *manifest, paths config.Paths, entry *manifestEntry, path string) error {
	identity, err := fsutil.IdentityOfPath(path)
	if err != nil {
		return fmt.Errorf("record identity of %s: %w", path, err)
	}
	parent, err := fsutil.IdentityOfPath(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("record parent identity of %s: %w", path, err)
	}
	entry.State = manifestOwned
	entry.Identity = identityFromFS(identity)
	entry.Parent = identityFromFS(parent)
	entry.SHA256 = ""
	entry.StagedPath = ""
	entry.StagedSHA256 = ""
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		sum, err := fsutil.SHA256File(path)
		if err != nil {
			return fmt.Errorf("hash %s: %w", path, err)
		}
		entry.SHA256 = sum
	}
	return writeManifest(paths.ManifestFile(), m)
}

// markPathStaged durably records the staged publication before the target is
// touched. A crash after this point leaves enough information for a retry to
// recognise its own file at the target without adopting a foreign one.
func markPathStaged(m *manifest, paths config.Paths, path, kind string, staged fsutil.StagedFile) error {
	entry := m.entry(path, kind)
	if entry == nil {
		return fmt.Errorf("internal error: %s is not part of the installation manifest", path)
	}
	entry.StagedPath = staged.Path
	entry.StagedSHA256 = staged.SHA256
	return writeManifest(paths.ManifestFile(), m)
}

// ensureStateDirs creates the per-component private directories. In system
// scope the hub and collector accounts must not be able to read each other's
// state, so each directory is 0700 and owned by its account. It runs after
// systemd-sysusers so account names resolve.
func ensureStateDirs(deps Deps, paths config.Paths, m *manifest) error {
	if paths.Scope == config.ScopeSystem {
		hubDir := paths.ComponentStateDir(systemd.Hub.String())
		if err := makePrivateDir(hubDir); err != nil {
			return err
		}
		if err := deps.chown(hubDir, systemd.HubUser, systemd.SharedGroup); err != nil {
			return fmt.Errorf("set ownership of %s: %w", hubDir, err)
		}
		if err := markPathOwned(m, paths, hubDir, kindComponentState); err != nil {
			return err
		}
	}
	collectorDir := paths.CollectorDir()
	if err := makePrivateDir(collectorDir); err != nil {
		return err
	}
	if paths.Scope == config.ScopeSystem {
		if err := deps.chown(collectorDir, systemd.CollectorUser, systemd.SharedGroup); err != nil {
			return fmt.Errorf("set ownership of %s: %w", collectorDir, err)
		}
	}
	return markPathOwned(m, paths, collectorDir, kindCollectorState)
}

func makePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("set mode on state directory %s: %w", dir, err)
	}
	return nil
}

// removeStaleIntegration deletes an owned integration file that the new
// configuration no longer wants. It only touches files recorded as owned by
// the previous manifest, still carrying the Rigscope marker, and whose
// identity still matches. Deletion is relative to a verified parent directory
// descriptor.
func removeStaleIntegration(previous, wanted *manifest) (bool, error) {
	if previous == nil {
		return false, nil
	}
	removed := false
	for _, entry := range previous.IntegrationFiles {
		if wanted.owns(entry.Path, entry.Kind) {
			continue
		}
		if wanted.Scope == config.ScopeUser && entry.Kind == kindUdevGPU {
			// A root-owned user-scope rule is never deleted without sudo;
			// uninstall performs that check explicitly.
			continue
		}
		if entry.State != manifestOwned {
			continue
		}
		parent, err := fsutil.OpenDirNoFollow(filepath.Dir(entry.Path))
		if err != nil {
			return false, fmt.Errorf("remove stale %s: %w", entry.Path, err)
		}
		name := filepath.Base(entry.Path)
		identity, err := parent.Stat(name)
		if errors.Is(err, os.ErrNotExist) {
			_ = parent.Close()
			continue
		}
		if err != nil {
			_ = parent.Close()
			return false, fmt.Errorf("inspect stale %s: %w", entry.Path, err)
		}
		if !entry.Identity.matches(identity) {
			_ = parent.Close()
			return false, fmt.Errorf("refusing to remove %s: its identity changed since install recorded it; inspect it first", entry.Path)
		}
		content, err := parent.ReadFile(name)
		if err != nil {
			_ = parent.Close()
			return false, fmt.Errorf("read %s: %w", entry.Path, err)
		}
		if !systemd.IsManaged(string(content)) {
			_ = parent.Close()
			return false, fmt.Errorf("refusing to remove unrecognized file %s; inspect it first", entry.Path)
		}
		if err := parent.Remove(name); err != nil {
			_ = parent.Close()
			return false, fmt.Errorf("remove %s: %w", entry.Path, err)
		}
		_ = parent.Close()
		removed = true
	}
	return removed, nil
}

// writeConfigFile writes the configuration with no-overwrite semantics on a
// fresh install and an atomic replace only when the manifest already owns it.
func writeConfigFile(deps Deps, paths config.Paths, m *manifest, cfg *config.Config) error {
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	data = append(data, '\n')
	mode := os.FileMode(0o600)
	if paths.Scope == config.ScopeSystem {
		mode = 0o640
	}
	if err := publishManifestedFile(deps, paths, m, paths.ConfigFile, kindConfig, data, mode, nil); err != nil {
		return err
	}
	// The configuration directory is root-owned in system scope, so a
	// pathname chown cannot be redirected by the service account. The
	// collector-private enrollment grant uses descriptor ownership instead.
	if paths.Scope == config.ScopeSystem {
		if err := deps.chown(paths.ConfigFile, "root", systemd.SharedGroup); err != nil {
			return fmt.Errorf("set ownership of %s: %w", paths.ConfigFile, err)
		}
	}
	return nil
}

// writeManifestedFile creates or replaces one file according to its manifest
// state, then promotes the entry to owned.
func writeManifestedFile(deps Deps, paths config.Paths, m *manifest, path, kind string, data []byte, perm os.FileMode, prepare func(*os.File) error) error {
	return publishManifestedFile(deps, paths, m, path, kind, data, perm, prepare)
}

// publishManifestedFile stages data beside its target, durably records the
// staged identity in the manifest, publishes it, and only then promotes the
// entry to owned. If a previous attempt died between publication and
// promotion, a retry adopts the file only when its digest matches the staged
// record; a foreign file at the same pathname is never adopted. Unchanged default
// 0.2 udev rules are the explicit exception: install previews their replacement
// and records ownership only after publishing the new 1.x rule.
func publishManifestedFile(deps Deps, paths config.Paths, m *manifest, path, kind string, data []byte, perm os.FileMode, prepare func(*os.File) error) error {
	entry := m.entry(path, kind)
	if entry == nil {
		return fmt.Errorf("internal error: %s is not part of the installation manifest", path)
	}
	exists, err := reconcileStagedPublication(m, paths, entry, path)
	if err != nil {
		return err
	}
	legacy := exists && entry.State == manifestPlanned && isDefaultLegacyUdev(path, kind)
	var legacyIdentity fsutil.Identity
	if legacy {
		if err := discardRecordedStaged(entry); err != nil {
			return err
		}
		legacyIdentity, err = fsutil.IdentityOfPath(path)
		if err != nil {
			return err
		}
	}
	if exists {
		if entry.State != manifestOwned && !legacy {
			return unownedPathError(path)
		}
	} else if err := discardRecordedStaged(entry); err != nil {
		return err
	}
	staged, err := fsutil.StageFile(path, data, perm, prepare)
	if err != nil {
		return err
	}
	if err := markPathStaged(m, paths, path, kind, staged); err != nil {
		_ = fsutil.DiscardStagedFile(staged)
		return err
	}
	if err := deps.publishStep(path, "staged"); err != nil {
		return err
	}
	if legacy {
		identity, err := fsutil.IdentityOfPath(path)
		if err != nil || identity != legacyIdentity || !isDefaultLegacyUdev(path, kind) {
			return fmt.Errorf("legacy rule changed during installation: %s; refusing to replace it", path)
		}
	}
	if err := fsutil.PublishStagedFile(staged, path, exists); err != nil {
		return err
	}
	if err := deps.publishStep(path, "published"); err != nil {
		return err
	}
	err = recordPathOwned(m, paths, entry, path)
	if err != nil && kind == kindPairingBootstrap && errors.Is(err, os.ErrNotExist) {
		// A running hub imports the bootstrap file lazily and deletes it once
		// the grant is in its database. Losing the race to that import means
		// the file did its job, not that publication failed: leave nothing
		// recorded as owned so uninstall has no file to look for.
		entry.State = manifestPlanned
		entry.Identity = nil
		entry.Parent = nil
		entry.SHA256 = ""
		entry.StagedPath = ""
		entry.StagedSHA256 = ""
		return writeManifest(paths.ManifestFile(), m)
	}
	return err
}

// reconcileStagedPublication handles a file left by an interrupted
// publication. It returns whether a target file exists and, when the file is
// this installation's own staged content, records it as owned.
func reconcileStagedPublication(m *manifest, paths config.Paths, entry *manifestEntry, path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.IsDir() {
		return false, fmt.Errorf("refusing to overwrite %s: it is a directory", path)
	}
	if entry.StagedSHA256 != "" && info.Mode().IsRegular() {
		sum, err := fsutil.SHA256File(path)
		if err != nil {
			return false, fmt.Errorf("hash %s: %w", path, err)
		}
		if sum == entry.StagedSHA256 {
			if err := recordPathOwned(m, paths, entry, path); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return true, nil
}

// discardRecordedStaged removes a stale staged temporary recorded by an
// interrupted attempt, but only when its content still matches the record.
func discardRecordedStaged(entry *manifestEntry) error {
	if entry.StagedPath == "" || entry.StagedSHA256 == "" {
		return nil
	}
	sum, err := fsutil.SHA256File(entry.StagedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// A directory or unreadable file at the staged name is not ours to
		// remove; leave it for the operator.
		return nil
	}
	if sum != entry.StagedSHA256 {
		return nil
	}
	if err := os.Remove(entry.StagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale staged file %s: %w", entry.StagedPath, err)
	}
	return nil
}

func installBinary(deps Deps, paths config.Paths, m *manifest) error {
	source, err := deps.executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read running binary %s: %w", source, err)
	}
	entry := m.entry(paths.Binary, kindBinary)
	if entry == nil {
		return fmt.Errorf("internal error: binary is not part of the installation manifest")
	}
	exists, err := reconcileStagedPublication(m, paths, entry, paths.Binary)
	if err != nil {
		return err
	}
	if exists {
		if entry.State != manifestOwned {
			return unownedPathError(paths.Binary)
		}
		current, readErr := os.ReadFile(paths.Binary)
		if readErr == nil && bytes.Equal(current, data) {
			return markPathOwned(m, paths, paths.Binary, kindBinary)
		}
	} else if err := discardRecordedStaged(entry); err != nil {
		return err
	}
	staged, err := fsutil.StageFile(paths.Binary, data, 0o755, nil)
	if err != nil {
		return err
	}
	if err := markPathStaged(m, paths, paths.Binary, kindBinary, staged); err != nil {
		_ = fsutil.DiscardStagedFile(staged)
		return err
	}
	if err := deps.publishStep(paths.Binary, "staged"); err != nil {
		return err
	}
	if exists {
		previousEntry := m.entry(paths.PreviousBinary, kindPreviousBinary)
		if previousEntry == nil {
			return fmt.Errorf("internal error: previous binary is not part of the installation manifest")
		}
		retained, err := fsutil.RetainPrevious(paths.Binary)
		if err != nil {
			return fmt.Errorf("retain previous binary: %w", err)
		}
		if retained.Path != "" {
			if err := recordPathOwned(m, paths, previousEntry, paths.PreviousBinary); err != nil {
				return err
			}
		}
	}
	if err := fsutil.PublishStagedFile(staged, paths.Binary, exists); err != nil {
		return fmt.Errorf("install binary %s: %w", paths.Binary, err)
	}
	if err := deps.publishStep(paths.Binary, "published"); err != nil {
		return err
	}
	if err := recordPathOwned(m, paths, entry, paths.Binary); err != nil {
		return err
	}
	if entry := m.entry(paths.PreviousBinary, kindPreviousBinary); entry != nil && entry.State == manifestOwned {
		// RetainPrevious replaced the inode of .prev; record the new identity.
		if err := markPathOwned(m, paths, paths.PreviousBinary, kindPreviousBinary); err != nil {
			return err
		}
	} else if entry != nil {
		// A previous binary is written by install only when it retains one and
		// by upgrade afterwards. If it is gone, stop claiming it so a later
		// uninstall does not treat a foreign file at that path as owned.
		entry.State = manifestPlanned
		entry.Identity = nil
		entry.Parent = nil
		entry.SHA256 = ""
		entry.StagedPath = ""
		entry.StagedSHA256 = ""
		if err := writeManifest(paths.ManifestFile(), m); err != nil {
			return err
		}
	}
	return nil
}

func writeUnits(deps Deps, paths config.Paths, hardware hardwareSelection, m *manifest) error {
	for _, component := range installComponents {
		options := systemd.Options{
			Component:  component,
			Scope:      systemdScope(paths.Scope),
			Binary:     paths.Binary,
			ConfigFile: paths.ConfigFile,
			StateDir:   paths.ComponentStateDir(component.String()),
		}
		if paths.Scope == config.ScopeSystem && component == systemd.Collector {
			options.SupplementaryGroups = systemd.HardwareGroups(hardware.RAPL, hardware.GPURegisters)
		}
		unit, err := systemd.Render(options)
		if err != nil {
			return fmt.Errorf("render %s unit: %w", component, err)
		}
		path := paths.UnitPath(component.UnitName())
		if err := writeManifestedFile(deps, paths, m, path, kindUnit, []byte(unit), systemd.DefaultUnitMode, nil); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// writeSysusers creates the account file. It is separate from the udev files
// because systemd-sysusers must run before any chown on a fresh system.
func writeSysusers(deps Deps, paths config.Paths, m *manifest, hardware hardwareSelection) error {
	content := systemd.RenderSysusers(systemd.SysusersOptions{
		RAPL:         hardware.RAPL,
		GPURegisters: hardware.GPURegisters,
	})
	if err := os.MkdirAll(filepath.Dir(paths.SysusersFile), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", paths.SysusersFile, err)
	}
	return writeManifestedFile(deps, paths, m, paths.SysusersFile, kindSysusers, []byte(content), 0o644, nil)
}

// writeUdevFiles creates the hardware-access rules. Only the RAPL rule is
// installed for a normal collector; GPU register access needs the explicit
// opt-in.
func writeUdevFiles(deps Deps, paths config.Paths, m *manifest, hardware hardwareSelection) error {
	files := []struct {
		path    string
		kind    string
		content string
	}{}
	if hardware.RAPL {
		files = append(files, struct {
			path    string
			kind    string
			content string
		}{paths.UdevRAPLFile, kindUdevRAPL, systemd.RenderRAPLRule()})
	}
	if hardware.GPURegisters {
		files = append(files, struct {
			path    string
			kind    string
			content string
		}{paths.UdevGPUTempsFile, kindUdevGPU, systemd.RenderGPUTempsRule(systemd.GPUGroup)})
	}
	for _, file := range files {
		if file.path == "" {
			return fmt.Errorf("system installation paths are incomplete")
		}
		if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
			return fmt.Errorf("create directory for %s: %w", file.path, err)
		}
		if err := writeManifestedFile(deps, paths, m, file.path, file.kind, []byte(file.content), 0o644, nil); err != nil {
			return fmt.Errorf("write %s: %w", file.path, err)
		}
	}
	return nil
}

// applySysusers creates the service accounts. Missing accounts make the units
// unusable, so a failure aborts.
func applySysusers(ctx context.Context, deps Deps, paths config.Paths) error {
	if deps.RunCommand == nil {
		return nil
	}
	if err := deps.RunCommand(ctx, "systemd-sysusers", paths.SysusersFile); err != nil {
		return fmt.Errorf("create Rigscope service accounts: %w", err)
	}
	return nil
}

// applyUdev reloads and triggers udev. A failure only delays hardware access
// until the next boot and is reported as a warning.
func applyUdev(ctx context.Context, deps Deps, paths config.Paths, hardware hardwareSelection, removed bool) error {
	if deps.RunCommand == nil {
		return nil
	}
	if !hardware.RAPL && !hardware.GPURegisters && !removed {
		return nil
	}
	if err := deps.RunCommand(ctx, "udevadm", "control", "--reload-rules"); err != nil {
		deps.warnf(deps.Stdout, "could not reload udev rules: %v", err)
		return nil
	}
	if hardware.RAPL {
		if err := deps.RunCommand(ctx, "udevadm", "trigger", "--subsystem-match=powercap", "--action=change"); err != nil {
			deps.warnf(deps.Stdout, "could not apply udev rules to powercap: %v", err)
		}
	}
	if hardware.GPURegisters {
		if err := deps.RunCommand(ctx, "udevadm", "trigger", "--subsystem-match=pci", "--action=change"); err != nil {
			deps.warnf(deps.Stdout, "could not apply udev rules to pci: %v", err)
		}
	}
	return nil
}

func systemdScope(scope config.Scope) systemd.Scope {
	if scope == config.ScopeSystem {
		return systemd.System
	}
	return systemd.User
}

func hubURLForDisplay(cfg *config.Config) string {
	hub := cfg.Components.Hub
	if hub.Origin != "" {
		return hub.Origin
	}
	host := hub.Listen
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		return fmt.Sprintf("http://[%s]:%d", host, hub.Port)
	}
	return fmt.Sprintf("http://%s:%d", host, hub.Port)
}

// componentsToRestart returns the running components this run changes: a new
// binary, a different unit file, or different settings. The local collector
// follows the hub's address, so a hub settings change counts for it too. A
// component whose state cannot be compared counts as changed.
func (d Deps) componentsToRestart(paths config.Paths, running []systemd.Component, prior, next *config.Config, replaceBinary bool, hardware hardwareSelection) []systemd.Component {
	if len(running) == 0 {
		return nil
	}
	binaryChanges := false
	if replaceBinary {
		binaryChanges = true
		if source, err := d.executable(); err == nil {
			wanted, errWanted := os.ReadFile(source)
			current, errCurrent := os.ReadFile(paths.Binary)
			binaryChanges = errWanted != nil || errCurrent != nil || !bytes.Equal(wanted, current)
		}
	}
	versionChanges := prior.Version != next.Version
	hubChanges := versionChanges || !reflect.DeepEqual(prior.Components.Hub, next.Components.Hub)
	// History storage and the forwarded-header family never reach the
	// collector; every other hub change may move the address it delivers to.
	// Automatic pairing only re-points an empty or local destination, so a
	// collector sending to another machine's hub is never affected.
	destinations := next.Components.Collector.Destinations
	followsLocalHub := len(destinations) == 0 || isLocalDestination(destinations[0])
	collectorChanges := versionChanges ||
		!reflect.DeepEqual(prior.Components.Collector, next.Components.Collector) ||
		(next.Components.Hub.Enabled && followsLocalHub && !reflect.DeepEqual(collectorFacingHub(prior.Components.Hub), collectorFacingHub(next.Components.Hub)))
	var changed []systemd.Component
	for _, component := range running {
		settings := hubChanges
		if component == systemd.Collector {
			settings = collectorChanges
		}
		if binaryChanges || settings || unitChanges(paths, hardware, component) {
			changed = append(changed, component)
		}
	}
	return changed
}

// collectorFacingHub keeps the hub settings that decide how a local collector reaches
// it.
func collectorFacingHub(hub config.Hub) config.Hub {
	hub.TelemetryMode = ""
	hub.ForwardedHeaders = ""
	return hub
}

// plannedHardware decides which optional hardware integration this run
// installs. GPU register access, once provisioned, stays provisioned until
// uninstall, even when register sampling or the collector is turned off: a
// later install --gpu-registers turns sampling back on. User scope keeps
// the root-owned rule in the manifest as soon as it is requested, when a
// Rigscope-managed rule already exists, or when the recorded manifest owned
// or adopted one, so uninstall can still find it after a later --role hub.
func plannedHardware(paths config.Paths, cfg *config.Config, recorded *manifest) hardwareSelection {
	collector := cfg.Components.Collector.Enabled
	gpuWanted := collector && cfg.Components.Collector.Hardware.GPURegisters
	entry := recorded.entry(paths.UdevGPUTempsFile, kindUdevGPU)
	provisioned := entry != nil && (entry.State == manifestOwned || entry.State == manifestAdopted)
	switch paths.Scope {
	case config.ScopeUser:
		if (collector && managedGPURuleExists(paths)) || provisioned {
			gpuWanted = true
		}
	case config.ScopeSystem:
		if provisioned {
			gpuWanted = true
		}
	}
	return hardwareSelection{
		RAPL:         paths.Scope == config.ScopeSystem && collector,
		GPURegisters: gpuWanted,
	}
}

// unitChanges reports whether writeUnits would rewrite component's unit.
func unitChanges(paths config.Paths, hardware hardwareSelection, component systemd.Component) bool {
	options := systemd.Options{
		Component:  component,
		Scope:      systemdScope(paths.Scope),
		Binary:     paths.Binary,
		ConfigFile: paths.ConfigFile,
		StateDir:   paths.ComponentStateDir(component.String()),
	}
	if paths.Scope == config.ScopeSystem && component == systemd.Collector {
		options.SupplementaryGroups = systemd.HardwareGroups(hardware.RAPL, hardware.GPURegisters)
	}
	want, err := systemd.Render(options)
	if err != nil {
		return true
	}
	current, err := os.ReadFile(paths.UnitPath(component.UnitName()))
	return err != nil || string(current) != want
}

// parseTrustedProxies reads configure --trusted-proxy: a comma-separated list
// of IP addresses or CIDR ranges, or "none" to trust no proxy.
func parseTrustedProxies(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("give one or more addresses, such as 127.0.0.1, or none")
	}
	if strings.EqualFold(raw, "none") {
		return []string{}, nil
	}
	proxies := []string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if net.ParseIP(entry) == nil {
			if _, _, err := net.ParseCIDR(entry); err != nil {
				return nil, fmt.Errorf("%q is not an IP address or CIDR range", entry)
			}
		}
		if !slices.Contains(proxies, entry) {
			proxies = append(proxies, entry)
		}
	}
	if len(proxies) == 0 {
		return nil, errors.New("give one or more addresses, such as 127.0.0.1, or none")
	}
	return proxies, nil
}

func proxyList(proxies []string) string {
	if len(proxies) == 0 {
		return "none"
	}
	return strings.Join(proxies, ", ")
}

// printProxyHint explains what a public HTTPS origin still needs when the hub
// does not serve TLS itself: a proxy that answers at the origin and forwards
// to the hub's plaintext listener, and, for a proxy on this machine, trust in
// its forwarded headers. Without it the origin is an address nobody answers.
func printProxyHint(deps Deps, cfg *config.Config) {
	hub := cfg.Components.Hub
	if hub.Origin == "" || hub.TLSCert != "" || len(hub.TrustedProxies) > 0 {
		return
	}
	origin, err := url.Parse(hub.Origin)
	if err != nil || origin.Scheme != "https" {
		return
	}
	local := hubListenURL(hub)
	fmt.Fprintf(deps.Stdout, "  %s   the hub itself serves only %s; to reach it at %s,\n", deps.out(styleYellow, "proxy:"), local, hub.Origin)
	fmt.Fprintln(deps.Stdout, "           put an HTTPS reverse proxy in front of it:")
	if strings.HasSuffix(strings.ToLower(origin.Hostname()), ".ts.net") {
		port := origin.Port()
		if port == "" {
			port = "443"
		}
		fmt.Fprintf(deps.Stdout, "             %s\n", deps.out(styleCyan, fmt.Sprintf("sudo tailscale serve --bg --https=%s %s", port, local)))
	} else {
		fmt.Fprintln(deps.Stdout, "             Caddy or nginx; see the reverse-proxy section of docs/configuration.md")
	}
	fmt.Fprintf(deps.Stdout, "           then, for a proxy on this machine: %s\n", deps.out(styleCyan, "rigscope configure --trusted-proxy 127.0.0.1"))
}

// hubListenURL is the plaintext address the hub's own socket serves.
func hubListenURL(hub config.Hub) string {
	host := hub.Listen
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(hub.Port))
}
