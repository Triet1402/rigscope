package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/state"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

type checkStatus string

const (
	checkOK   checkStatus = "ok"
	checkWarn checkStatus = "warn"
	checkFail checkStatus = "fail"
	checkSkip checkStatus = "skip"
)

type doctorCheck struct {
	ID     string      `json:"id"`
	Label  string      `json:"label"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail"`
}

func runDoctor(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("doctor", deps.Stderr)
	system := flags.Bool("system", false, "diagnose the root-owned system installation")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		return deps.errorf("doctor takes no positional arguments, got %q", flags.Arg(0))
	}
	scope := deps.installedScope(*system)
	paths, err := deps.paths(scope)
	if err != nil {
		return deps.errorf("%v", err)
	}
	manager := deps.manager(scope)
	checks := []doctorCheck{}

	current, manifestErr := loadManifest(paths.ManifestFile(), paths)
	if manifestErr != nil {
		detail := manifestErr.Error()
		if errors.Is(manifestErr, errManifestMissing) {
			detail = fmt.Sprintf("%s is missing; this is not a Rigscope 1.x installation", paths.ManifestFile())
			if found := detectLegacy(paths); len(found) > 0 {
				detail += " (a Rigscope 0.2 installation was detected: " + strings.Join(compact(found), ", ") + ")"
			}
		}
		checks = append(checks, doctorCheck{ID: "manifest", Label: "Installation manifest", Status: checkFail, Detail: detail})
	} else {
		checks = append(checks, doctorCheck{
			ID:     "manifest",
			Label:  "Installation manifest",
			Status: checkOK,
			Detail: fmt.Sprintf("%s (rigscope %s)", paths.ManifestFile(), current.Version),
		})
	}

	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		checks = append(checks, doctorCheck{
			ID:     "config",
			Label:  "Configuration",
			Status: checkFail,
			Detail: err.Error(),
		})
		return printDoctor(deps, scope, checks)
	}
	checks = append(checks, doctorCheck{
		ID:     "config",
		Label:  "Configuration",
		Status: checkOK,
		Detail: fmt.Sprintf("%s; enabled: %s; hub on %s", paths.ConfigFile, strings.Join(enabledComponents(cfg), ", "), hubURLForDisplay(cfg)),
	})

	checks = append(checks, checkConfigPermissions(deps, paths, scope))
	checks = append(checks, checkStateRoot(deps, paths, scope))
	for _, component := range installComponents {
		if component == systemd.Collector || paths.ComponentStateDir(component.String()) == paths.StateDir {
			continue
		}
		checks = append(checks, checkComponentState(deps, paths, component, scope))
	}
	checks = append(checks, checkCollectorState(deps, paths, scope))

	checks = append(checks, checkBinary(deps, "binary", "Installed binary", paths.Binary, false))
	checks = append(checks, checkBinary(deps, "binary-previous", "Previous binary", paths.PreviousBinary, true))

	for _, component := range installComponents {
		if !componentEnabled(cfg, component) {
			continue
		}
		unit := component.UnitName()
		path := paths.UnitPath(unit)
		content, err := os.ReadFile(path)
		switch {
		case err != nil:
			checks = append(checks, doctorCheck{
				ID:     "unit-" + component.String(),
				Label:  "Unit " + unit,
				Status: checkFail,
				Detail: fmt.Sprintf("missing %s; run rigscope install", path),
			})
			continue
		case !systemd.IsManaged(string(content)):
			checks = append(checks, doctorCheck{
				ID:     "unit-" + component.String(),
				Label:  "Unit " + unit,
				Status: checkFail,
				Detail: fmt.Sprintf("%s is not managed by rigscope; inspect it before replacing", path),
			})
			continue
		}
		checks = append(checks, checkUnitPermissions(deps, path, scope))
		active, err := manager.IsActive(ctx, unit)
		switch {
		case err != nil:
			checks = append(checks, doctorCheck{ID: "active-" + component.String(), Label: "Service " + component.String(), Status: checkWarn, Detail: err.Error()})
		case active:
			checks = append(checks, doctorCheck{ID: "active-" + component.String(), Label: "Service " + component.String(), Status: checkOK, Detail: "active"})
		default:
			checks = append(checks, doctorCheck{ID: "active-" + component.String(), Label: "Service " + component.String(), Status: checkWarn, Detail: "inactive; run rigscope start " + component.String()})
		}
	}

	checks = append(checks, checkCollectorDestination(cfg))
	checks = append(checks, checkCollectorPairing(cfg, paths))

	if scope == config.ScopeSystem {
		checks = append(checks, checkManagedSystemFile(deps, "sysusers", "Service accounts", paths.SysusersFile, scope))
		if cfg.Components.Collector.Enabled {
			checks = append(checks, checkManagedSystemFile(deps, "udev-rapl", "RAPL udev rule", paths.UdevRAPLFile, scope))
		}
		if cfg.Components.Collector.Enabled && cfg.Components.Collector.Hardware.GPURegisters {
			checks = append(checks, checkManagedSystemFile(deps, "udev-gpu", "GPU register udev rule", paths.UdevGPUTempsFile, scope))
		}
	}

	checks = append(checks, checkRAPLAccess(deps, paths, cfg))
	checks = append(checks, checkGPUAccess(deps, paths, cfg))

	return printDoctor(deps, scope, checks)
}

// checkRAPLAccess reports whether package energy counters are readable as the
// current user. It runs for both scopes: a user install never installs the
// RAPL rule itself, but a rule left by 0.2 or a system install can still make
// the counters readable.
func checkRAPLAccess(deps Deps, paths config.Paths, cfg *config.Config) doctorCheck {
	check := doctorCheck{ID: "hardware-rapl", Label: "CPU package power (RAPL)"}
	if !cfg.Components.Collector.Enabled {
		check.Status = checkSkip
		check.Detail = "collector disabled; no RAPL access installed"
		return check
	}
	rulePresent := false
	if paths.UdevRAPLFile != "" {
		if _, err := os.Stat(paths.UdevRAPLFile); err == nil {
			rulePresent = true
		}
	}
	pattern := filepath.Join(deps.hostPath("sys", "class", "powercap"), "intel-rapl:*/energy_uj")
	counters, _ := filepath.Glob(pattern)
	if len(counters) == 0 {
		if !rulePresent {
			check.Status = checkSkip
			check.Detail = "no intel-rapl package counter and no RAPL rule installed"
			return check
		}
		check.Status = checkWarn
		check.Detail = fmt.Sprintf("the RAPL rule exists but no intel-rapl package counter was found under %s", filepath.Dir(pattern))
		return check
	}
	readable := 0
	denied := false
	var firstErr error
	for _, counter := range counters {
		if _, err := deps.readAttribute(counter); err != nil {
			if errors.Is(err, os.ErrPermission) {
				denied = true
			} else if firstErr == nil {
				firstErr = err
			}
			continue
		}
		readable++
	}
	switch {
	case readable > 0:
		detail := fmt.Sprintf("working: %d package counter(s) readable by %s", readable, readerName(deps))
		if paths.Scope == config.ScopeSystem {
			ok, why := systemCollectorHasGroup(deps, paths, systemd.RAPLGroup)
			if !ok {
				check.Status = checkWarn
				check.Detail = fmt.Sprintf("%s, but %s; the collector cannot read them — rerun rigscope install --system", detail, why)
				return check
			}
			detail += fmt.Sprintf("; the collector unit joins group %s", systemd.RAPLGroup)
		}
		check.Status = checkOK
		check.Detail = detail
	case denied:
		check.Status = checkWarn
		check.Detail = fmt.Sprintf("%s is not readable as %s: permission denied; log out fully to pick up the group, or check group membership", counters[0], readerName(deps))
	default:
		check.Status = checkWarn
		check.Detail = fmt.Sprintf("%s is not readable: %v", counters[0], firstErr)
	}
	return check
}

// checkGPUAccess reports the user-scope and system-scope GPU rule state and
// proves that this session can map BAR0. Suspended devices are reported as
// present but not probed, never as failures.
func checkGPUAccess(deps Deps, paths config.Paths, cfg *config.Config) doctorCheck {
	check := doctorCheck{ID: "hardware-gpu", Label: "GPU register temperatures"}
	if !cfg.Components.Collector.Enabled {
		check.Status = checkSkip
		check.Detail = "collector disabled"
		return check
	}
	rulePath := paths.UdevGPUTempsFile
	content, err := os.ReadFile(rulePath)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		check.Status = checkFail
		check.Detail = err.Error()
		return check
	}
	if !exists && !cfg.Components.Collector.Hardware.GPURegisters {
		check.Status = checkSkip
		check.Detail = "disabled; add --gpu-registers to the install command to opt in"
		return check
	}
	if !exists {
		check.Status = checkFail
		check.Detail = fmt.Sprintf("%s is missing; run %s", rulePath, gpuInstallHint(paths.Scope))
		return check
	}
	managed := systemd.IsManaged(string(content)) || systemd.IsLegacyManaged(string(content))
	if !cfg.Components.Collector.Hardware.GPURegisters {
		// Sampling was turned off (configure --gpu-registers off) or the rule
		// outlived the configuration. Report it as off, not as working: the
		// permissions stay installed until uninstall.
		check.Status = checkSkip
		check.Detail = fmt.Sprintf("register sampling is off; the access rule %s stays installed; run `%s` to turn sampling back on", rulePath, gpuInstallHint(paths.Scope))
		if !managed {
			check.Status = checkWarn
			check.Detail += "; the file was not written by Rigscope"
		}
		return check
	}
	probe := probeGPUDisplays(deps)
	switch {
	case probe.devices == 0:
		check.Status = checkWarn
		check.Detail = fmt.Sprintf("rule present but no NVIDIA display device was found under %s", deps.hostPath("sys", "bus", "pci", "devices"))
	case probe.readable > 0:
		detail := fmt.Sprintf("working: %d GPU(s) readable by %s through %s", probe.readable, readerName(deps), rulePath)
		if paths.Scope == config.ScopeUser && ruleGroup(string(content)) == userGPUGroup {
			// A user service inherits its supplementary groups from the
			// systemd --user manager once, at start; a readable session does
			// not prove the collector has the group.
			if missing, known := managerMissingGroup(deps, userGPUGroup); missing {
				check.Status = checkWarn
				check.Detail = fmt.Sprintf("%s, but the collector's session lacks the %s group; log out fully so the user manager starts with it, then rerun `rigscope install --gpu-registers`", detail, userGPUGroup)
				return check
			} else if !known {
				check.Status = checkWarn
				check.Detail = fmt.Sprintf("%s, but the collector's session could not be checked (no systemd --user manager or group record); log out fully once to be sure", detail)
				return check
			}
			detail += fmt.Sprintf("; the collector's user manager carries group %s", userGPUGroup)
		}
		if paths.Scope == config.ScopeSystem {
			group := ruleGroup(string(content))
			if group == "" {
				group = systemd.GPUGroup
			}
			ok, why := systemCollectorHasGroup(deps, paths, group)
			if !ok {
				check.Status = checkWarn
				check.Detail = fmt.Sprintf("%s, but %s; the collector cannot read the registers — rerun rigscope install --system --gpu-registers", detail, why)
				return check
			}
			detail += fmt.Sprintf("; the collector unit joins group %s", group)
		}
		check.Status = checkOK
		check.Detail = detail
	case probe.suspended > 0 && probe.suspended == probe.devices:
		check.Status = checkWarn
		check.Detail = fmt.Sprintf("%d GPU(s) are present but suspended; they were not probed for permission (a probe could wake them)", probe.suspended)
	case probe.denied:
		group := ruleGroup(string(content))
		if group == "" {
			group = "the rule's group"
		}
		check.Status = checkWarn
		check.Detail = fmt.Sprintf("%s is present but resource0 is not readable by %s: permission denied; log out fully to pick up group %s, or check group membership", rulePath, readerName(deps), group)
		if !managed {
			check.Detail += "; the file was not written by Rigscope"
		}
	case !managed:
		check.Status = checkWarn
		check.Detail = fmt.Sprintf("%s is present but is not managed by rigscope; inspect it first", rulePath)
	case probe.err != nil:
		check.Status = checkWarn
		check.Detail = fmt.Sprintf("rule present but resource0 is not readable: %v", probe.err)
	default:
		check.Status = checkWarn
		check.Detail = "rule present but no register could be probed"
	}
	return check
}

// systemCollectorHasGroup reports whether the rendered system collector unit
// joins group. Root being able to read a file says nothing about the collector
// account, so doctor checks the unit's SupplementaryGroups instead.
func systemCollectorHasGroup(deps Deps, paths config.Paths, group string) (bool, string) {
	path := paths.UnitPath(systemd.Collector.UnitName())
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Sprintf("the system collector unit %s cannot be read (%v)", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || key != "SupplementaryGroups" {
			continue
		}
		for _, joined := range strings.Fields(value) {
			if joined == group {
				return true, ""
			}
		}
	}
	return false, fmt.Sprintf("group %s is not in %s's SupplementaryGroups", group, path)
}

func gpuInstallHint(scope config.Scope) string {
	if scope == config.ScopeSystem {
		return "rigscope install --system --gpu-registers"
	}
	return "rigscope install --gpu-registers"
}

func checkConfigPermissions(deps Deps, paths config.Paths, scope config.Scope) doctorCheck {
	check := doctorCheck{ID: "config-permissions", Label: "Configuration permissions"}
	info, err := os.Stat(paths.ConfigFile)
	if err != nil {
		check.Status = checkFail
		check.Detail = err.Error()
		return check
	}
	want := os.FileMode(0o600)
	if scope == config.ScopeSystem {
		want = 0o640
	}
	if info.Mode().Perm() != want {
		check.Status = checkFail
		check.Detail = fmt.Sprintf("%s is %04o; must be %04o", paths.ConfigFile, info.Mode().Perm(), want)
		return check
	}
	if scope == config.ScopeSystem {
		if detail := requireOwner(deps, paths.ConfigFile, "root", systemd.SharedGroup); detail != "" {
			check.Status = checkFail
			check.Detail = detail
			return check
		}
	}
	check.Status = checkOK
	check.Detail = fmt.Sprintf("%04o", info.Mode().Perm())
	return check
}

func checkStateRoot(deps Deps, paths config.Paths, scope config.Scope) doctorCheck {
	check := doctorCheck{ID: "state-dir", Label: "State directory"}
	info, err := os.Stat(paths.StateDir)
	if err != nil {
		check.Status = checkFail
		check.Detail = err.Error()
		return check
	}
	if !info.IsDir() {
		check.Status = checkFail
		check.Detail = paths.StateDir + " is not a directory"
		return check
	}
	want := os.FileMode(0o700)
	if scope == config.ScopeSystem {
		want = 0o755
	}
	if info.Mode().Perm() != want {
		check.Status = checkFail
		check.Detail = fmt.Sprintf("%s is %04o; must be %04o", paths.StateDir, info.Mode().Perm(), want)
		return check
	}
	if scope == config.ScopeSystem {
		if detail := requireOwner(deps, paths.StateDir, "root", "root"); detail != "" {
			check.Status = checkFail
			check.Detail = detail
			return check
		}
	}
	check.Status = checkOK
	check.Detail = fmt.Sprintf("%s (%04o)", paths.StateDir, want)
	return check
}

func checkComponentState(deps Deps, paths config.Paths, component systemd.Component, scope config.Scope) doctorCheck {
	path := paths.ComponentStateDir(component.String())
	check := doctorCheck{ID: "state-" + component.String(), Label: "State directory " + component.String()}
	info, err := os.Stat(path)
	if err != nil {
		check.Status = checkFail
		check.Detail = err.Error()
		return check
	}
	if info.Mode().Perm() != 0o700 {
		check.Status = checkFail
		check.Detail = fmt.Sprintf("%s is %04o; must be 0700", path, info.Mode().Perm())
		return check
	}
	account := systemd.HubUser
	if component == systemd.Collector {
		account = systemd.CollectorUser
	}
	if detail := requireOwner(deps, path, account, systemd.SharedGroup); detail != "" {
		check.Status = checkFail
		check.Detail = detail
		return check
	}
	check.Status = checkOK
	check.Detail = fmt.Sprintf("%s (%04o)", path, info.Mode().Perm())
	return check
}

func checkCollectorState(deps Deps, paths config.Paths, scope config.Scope) doctorCheck {
	path := paths.CollectorDir()
	check := doctorCheck{ID: "collector-state", Label: "Collector private state"}
	info, err := os.Stat(path)
	if err != nil {
		check.Status = checkFail
		check.Detail = err.Error()
		return check
	}
	if info.Mode().Perm() != 0o700 {
		check.Status = checkFail
		check.Detail = fmt.Sprintf("%s is %04o; must be 0700", path, info.Mode().Perm())
		return check
	}
	if scope == config.ScopeSystem {
		if detail := requireOwner(deps, path, systemd.CollectorUser, systemd.SharedGroup); detail != "" {
			check.Status = checkFail
			check.Detail = detail
			return check
		}
	}
	for _, secret := range []struct {
		label, path string
	}{
		{"enrollment grant", paths.EnrollmentFile()},
		{"issued credential", paths.CredentialsFile()},
	} {
		info, err := os.Stat(secret.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			check.Status = checkFail
			check.Detail = err.Error()
			return check
		}
		if info.Mode().Perm() != 0o600 {
			check.Status = checkFail
			check.Detail = fmt.Sprintf("%s %s is %04o; must be 0600", secret.label, secret.path, info.Mode().Perm())
			return check
		}
		if scope == config.ScopeSystem {
			if detail := requireOwner(deps, secret.path, systemd.CollectorUser, systemd.SharedGroup); detail != "" {
				check.Status = checkFail
				check.Detail = detail
				return check
			}
		}
	}
	check.Status = checkOK
	check.Detail = fmt.Sprintf("%s (%04o)", path, info.Mode().Perm())
	return check
}

func checkUnitPermissions(deps Deps, path string, scope config.Scope) doctorCheck {
	check := doctorCheck{ID: "unit-permissions", Label: "Unit " + filepath.Base(path)}
	info, err := os.Stat(path)
	if err != nil {
		check.Status = checkFail
		check.Detail = err.Error()
		return check
	}
	if info.Mode().Perm() != systemd.DefaultUnitMode {
		check.Status = checkFail
		check.Detail = fmt.Sprintf("%s is %04o; must be %04o", path, info.Mode().Perm(), systemd.DefaultUnitMode)
		return check
	}
	if scope == config.ScopeSystem {
		if detail := requireOwner(deps, path, "root", "root"); detail != "" {
			check.Status = checkFail
			check.Detail = detail
			return check
		}
	}
	check.Status = checkOK
	check.Detail = fmt.Sprintf("%s (%04o)", path, info.Mode().Perm())
	return check
}

// checkCollectorPairing reports an unfinished automatic local pairing. It only
// speaks for the installer-generated destination; an operator-supplied hub is
// out of scope for this probe. The hub's database is the authority on what it
// imported and issued, but this probe runs beside a live hub and cannot open it;
// it uses the installer's staged files and the collector's own state instead and
// says so when it cannot tell.
func checkCollectorPairing(cfg *config.Config, paths config.Paths) doctorCheck {
	check := doctorCheck{ID: "collector-pairing", Label: "Collector pairing"}
	if !cfg.Components.Collector.Enabled {
		check.Status = checkSkip
		check.Detail = "collector disabled"
		return check
	}
	destinations := cfg.Components.Collector.Destinations
	if len(destinations) != 1 {
		check.Status = checkSkip
		check.Detail = "no single destination configured"
		return check
	}
	local, ok, _ := localHubDestination(cfg.Components.Hub)
	if !ok || !sameDestination(destinations[0], local) {
		check.Status = checkSkip
		check.Detail = "destination was configured explicitly"
		return check
	}
	if info, err := os.Stat(paths.CollectorDir()); err != nil || !info.IsDir() {
		check.Status = checkSkip
		check.Detail = "collector state directory is missing"
		return check
	}
	store, err := state.Open(paths.StateDir, state.Options{})
	if err != nil {
		check.Status = checkFail
		check.Detail = err.Error()
		return check
	}
	destination := destinations[0]
	if _, found, err := store.Credential(destination.URL); err != nil {
		check.Status = checkFail
		check.Detail = err.Error()
		return check
	} else if found {
		check.Status = checkOK
		check.Detail = "local collector is paired"
		return check
	}
	if hubTrustsLoopbackProxy(cfg.Components.Hub) {
		// The hub records a forwarded header's presence before stripping it and
		// refuses a colocated redemption from a trusted proxy peer. Automatic
		// pairing can never complete while this policy is configured, so no
		// amount of rerunning install helps.
		check.Status = checkWarn
		check.Detail = loopbackProxyNotice(destination.URL)
		return check
	}
	hubEnabled := cfg.Components.Hub.Enabled
	if stale, ok := localCredentialElsewhere(store, destination.URL); ok {
		// A credential for another local hub destination means the hub most
		// likely knows this installation id already. Redeeming a fresh
		// automatic grant would then be refused as a conflict.
		check.Status = checkWarn
		check.Detail = fmt.Sprintf(
			"the collector holds a credential for local hub %s; if that hub already knows this collector, rerunning install is refused as a conflict — create a Replacement grant in Administration → Collectors, otherwise remove that stale destination first",
			stale,
		)
		return check
	}
	pending, hasPending, err := store.PendingEnrollment(destination.URL)
	if err != nil {
		check.Status = checkFail
		check.Detail = err.Error()
		return check
	}
	if !hasPending || pending.Grant == "" {
		check.Status = checkWarn
		switch {
		case !hubEnabled:
			check.Detail = "the hub component is disabled; automatic pairing cannot complete"
		default:
			check.Detail = "no enrollment grant is staged — rerun rigscope install"
		}
		return check
	}
	staged, stage, err := stagedPairingGrant(paths, pending.Grant)
	if err != nil {
		check.Status = checkFail
		check.Detail = err.Error()
		return check
	}
	check.Status = checkWarn
	expired := stage == pairingStageMatched && !staged.ExpiresAt.After(time.Now())
	switch {
	case !hubEnabled:
		// Pairing cannot complete while the local hub is disabled, and rerunning
		// install cannot fix that; the operator is told what stands in the way
		// instead of being sent in a loop.
		check.Detail = "pairing pending and the hub component is disabled"
		if expired {
			check.Detail = "pairing expired and the hub component is disabled"
		}
	case stage == pairingStageMismatch:
		check.Detail = "the hub's staged pairing grant does not match this collector's grant — rerun rigscope install"
	case stage == pairingStageNoFile:
		// The hub deletes the bootstrap file once it imports the grant, so this
		// is the ordinary "waiting for the collector to redeem it" state.
		check.Detail = "pairing pending — the hub has imported the staged grant and the collector has not redeemed it yet; check rigscope logs collector"
	case !expired:
		check.Detail = "pairing pending — the hub has not imported the staged grant yet; start the hub"
	default:
		check.Detail = "pairing expired — rerun rigscope install"
	}
	return check
}

// localCredentialElsewhere returns the first stored credential for a loopback
// hub destination other than the configured one. Such a credential names a hub
// on this machine, which is the only kind this probe can attribute to the local
// hub.
func localCredentialElsewhere(store *state.Store, destination string) (string, bool) {
	credentials, err := store.Credentials()
	if err != nil {
		return "", false
	}
	if normalized, err := state.ValidateHubURL(destination); err == nil {
		destination = normalized
	}
	for _, credential := range credentials {
		if credential.URL == destination {
			continue
		}
		if isLocalDestination(config.Destination{URL: credential.URL}) {
			return credential.URL, true
		}
	}
	return "", false
}

// pairingStage reports what the installer's staged bootstrap file says about
// one collector grant.
type pairingStage int

const (
	// pairingStageNoFile means the hub already imported the file (it deletes the
	// file after importing) or that no grant was ever staged.
	pairingStageNoFile pairingStage = iota
	// pairingStageMatched means the file still holds this exact grant.
	pairingStageMatched
	// pairingStageMismatch means a file exists but stages a different grant.
	pairingStageMismatch
)

// stagedPairingGrant finds the installer's staged bootstrap entry for one
// collector grant, so doctor can tell an unexpired staged grant from an expired
// one.
func stagedPairingGrant(paths config.Paths, grant string) (storage.BootstrapGrant, pairingStage, error) {
	loaded, err := loadPairingBootstrap(paths.ComponentStateDir(systemd.Hub.String()))
	if err != nil {
		return storage.BootstrapGrant{}, pairingStageNoFile, err
	}
	if loaded == nil {
		return storage.BootstrapGrant{}, pairingStageNoFile, nil
	}
	hash := pairingGrantHash(grant)
	for _, entry := range loaded.grants {
		if entry.TokenHash == hash {
			return entry, pairingStageMatched, nil
		}
	}
	return storage.BootstrapGrant{}, pairingStageMismatch, nil
}

func checkCollectorDestination(cfg *config.Config) doctorCheck {
	check := doctorCheck{ID: "collector-destination", Label: "Collector destination"}
	if !cfg.Components.Collector.Enabled {
		check.Status = checkSkip
		check.Detail = "collector disabled"
		return check
	}
	switch len(cfg.Components.Collector.Destinations) {
	case 0:
		check.Status = checkWarn
		check.Detail = "awaiting enrollment: no hub destination is recorded"
	case 1:
		destination := cfg.Components.Collector.Destinations[0]
		check.Status = checkOK
		check.Detail = destination.URL
		if destination.CAFile != "" {
			check.Detail += " (pinned CA: " + destination.CAFile + ")"
		}
	default:
		check.Status = checkFail
		check.Detail = fmt.Sprintf("%d destinations configured; 1.0 supports exactly one", len(cfg.Components.Collector.Destinations))
	}
	return check
}

// requireOwner verifies numeric ownership. A group name of "root" is resolved
// through the injectable lookup; accounts use the same path.
func requireOwner(deps Deps, path string, userName, groupName string) string {
	uid, gid, err := deps.fileOwner(path)
	if err != nil {
		return err.Error()
	}
	wantUID := 0
	if userName != "root" {
		value, err := deps.userID(userName)
		if err != nil {
			return fmt.Sprintf("resolve account %s: %v", userName, err)
		}
		wantUID = value
	}
	wantGID := 0
	if groupName != "root" {
		value, err := deps.groupID(groupName)
		if err != nil {
			return fmt.Sprintf("resolve group %s: %v", groupName, err)
		}
		wantGID = value
	}
	if uid != wantUID || gid != wantGID {
		return fmt.Sprintf("%s is owned by %d:%d; must be %s:%s (%d:%d)", path, uid, gid, userName, groupName, wantUID, wantGID)
	}
	return ""
}

func checkManagedSystemFile(deps Deps, id, label, path string, scope config.Scope) doctorCheck {
	check := doctorCheck{ID: id, Label: label}
	content, err := os.ReadFile(path)
	if err != nil {
		check.Status = checkFail
		check.Detail = fmt.Sprintf("%s; run rigscope install --system", err)
		return check
	}
	if !systemd.IsManaged(string(content)) {
		check.Status = checkFail
		check.Detail = path + " is not managed by rigscope; inspect it first"
		return check
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		check.Status = checkFail
		if err != nil {
			check.Detail = err.Error()
		} else {
			check.Detail = fmt.Sprintf("%s is %04o; must be 0644", path, info.Mode().Perm())
		}
		return check
	}
	if scope == config.ScopeSystem {
		if detail := requireOwner(deps, path, "root", "root"); detail != "" {
			check.Status = checkFail
			check.Detail = detail
			return check
		}
	}
	check.Status = checkOK
	check.Detail = path
	return check
}

func checkBinary(deps Deps, id, label, path string, optional bool) doctorCheck {
	check := doctorCheck{ID: id, Label: label}
	if _, err := os.Stat(path); err != nil {
		check.Status = checkFail
		check.Detail = err.Error()
		if optional {
			check.Status = checkWarn
			check.Detail = "no previous binary retained (rollback unavailable)"
		}
		return check
	}
	version, err := binaryVersion(deps, path)
	if err != nil {
		check.Status = checkWarn
		check.Detail = fmt.Sprintf("%s; could not read version: %v", path, err)
		return check
	}
	check.Status = checkOK
	check.Detail = fmt.Sprintf("%s (%s)", path, version)
	return check
}

func binaryVersion(deps Deps, path string) (string, error) {
	if deps.BinaryVersion != nil {
		return deps.BinaryVersion(path)
	}
	out, err := exec.Command(path, "version").Output()
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return strings.TrimSpace(string(out)), nil
	}
	return fields[len(fields)-1], nil
}

func printDoctor(deps Deps, scope config.Scope, checks []doctorCheck) int {
	failures := 0
	warnings := 0
	fmt.Fprintf(deps.Stdout, "%s (%s scope)\n", deps.out(styleBold, "Rigscope doctor"), scopeLabel(scope))
	for _, check := range checks {
		switch check.Status {
		case checkFail:
			failures++
		case checkWarn:
			warnings++
		}
		fmt.Fprintf(deps.Stdout, "  %s %s: %s\n", deps.out(checkStyle(check.Status), "["+string(check.Status)+"]"), check.Label, check.Detail)
	}
	summary := fmt.Sprintf("%d checks, %d warning(s), %d failure(s)", len(checks), warnings, failures)
	switch {
	case failures > 0:
		summary = deps.out(styleRed+";"+styleBold, summary)
	case warnings > 0:
		summary = deps.out(styleYellow, summary)
	default:
		summary = deps.out(styleGreen, summary)
	}
	fmt.Fprintf(deps.Stdout, "\n%s\n", summary)
	if failures > 0 {
		return 1
	}
	return 0
}

func checkStyle(status checkStatus) string {
	switch status {
	case checkOK:
		return styleGreen
	case checkWarn:
		return styleYellow
	default:
		return styleRed + ";" + styleBold
	}
}
