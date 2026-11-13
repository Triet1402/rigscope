package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

func runUninstall(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("uninstall", deps.Stderr)
	keepData := flags.Bool("keep-data", false, "keep configuration and state")
	system := flags.Bool("system", false, "remove root-owned system services")
	yes := addYesFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		return deps.errorf("uninstall takes no positional arguments, got %q", flags.Arg(0))
	}
	scope := deps.installedScope(*system)
	if code, done := deps.ensureRoot(scope); done {
		return code
	}
	paths, err := deps.paths(scope)
	if err != nil {
		return deps.errorf("%v", err)
	}

	// Ownership is checked before anything is created, stopped or deleted.
	current, err := loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		if errors.Is(err, errManifestMissing) {
			if found := detectLegacy(paths); len(found) > 0 {
				return deps.errorf("%v", legacyUninstallError(found))
			}
			return deps.errorf("no Rigscope 1.x installation manifest at %s; refusing to remove anything\nIf a Rigscope 0.2 installation is present, remove it with 0.2's own uninstall command first", paths.ManifestFile())
		}
		return deps.errorf("refusing to uninstall: %v", err)
	}
	if found := detectLegacy(paths); len(found) > 0 {
		return deps.errorf("%v", legacyUninstallError(found))
	}

	held, err := acquireLock(paths)
	if err != nil {
		return deps.errorf("%v", err)
	}
	defer held.Release()

	// Re-read under the lock so a concurrent install cannot change ownership
	// between validation and deletion.
	current, err = loadManifest(paths.ManifestFile(), paths)
	if err != nil {
		return deps.errorf("refusing to uninstall: %v", err)
	}
	stateEntry := current.entry(paths.StateDir, kindStateDir)
	if stateEntry == nil {
		return deps.errorf("refusing to uninstall: %s is not recorded in the installation manifest", paths.StateDir)
	}
	if err := verifyStateRoot(paths, stateEntry, *keepData); err != nil {
		return deps.errorf("refusing to uninstall: %v; nothing was deleted", err)
	}

	units, err := verifyOwnedUnits(deps, paths, current)
	if err != nil {
		return deps.errorf("%v", err)
	}
	manager := deps.manager(scope)
	if err := deps.approveServiceStop(activeComponents(ctx, manager, installedComponents(paths)), "stop and be removed", *yes); err != nil {
		return deps.errorf("uninstall: %v", err)
	}
	if err := stopAndVerify(ctx, manager, units); err != nil {
		return deps.errorf("%v; nothing was deleted", err)
	}
	if deps.AfterStop != nil {
		if err := deps.AfterStop(); err != nil {
			return deps.errorf("uninstall interrupted after stop: %v; nothing was deleted", err)
		}
	}

	// Verify every deletion target through a no-follow directory descriptor
	// before removing a single file. A parent swapped for a symlink during the
	// stop phase fails here, so no foreign tree is ever touched.
	plan, err := newUninstallPlan(paths, current, *keepData)
	if err != nil {
		return deps.errorf("refusing to uninstall: %v; nothing was deleted", err)
	}
	defer plan.close()
	if err := plan.verify(); err != nil {
		return deps.errorf("refusing to uninstall: %v; nothing was deleted", err)
	}
	// Finish privileged hardware cleanup before deleting the binary and the
	// ownership manifest. Otherwise a declined sudo prompt loses the evidence
	// needed to safely retry removal of the root-owned rule.
	if err := removeUserGPURule(ctx, deps, paths, current); err != nil {
		return deps.errorf("hardware cleanup incomplete: %v; installation files retained; rerun rigscope uninstall after completing the commands above", err)
	}
	if err := plan.execute(ctx, deps); err != nil {
		return deps.errorf("%v", err)
	}
	if *keepData {
		if err := keepManifest(paths, current, deps.buildVersion()); err != nil {
			return deps.errorf("%v", err)
		}
	}

	fmt.Fprintf(deps.Stdout, "%s (%s scope)\n", deps.out(styleGreen+";"+styleBold, "Removed Rigscope"), scopeLabel(scope))
	if plan.removedUnits > 0 {
		fmt.Fprintf(deps.Stdout, "  units:  %d removed\n", plan.removedUnits)
	} else {
		fmt.Fprintln(deps.Stdout, "  units:  none installed")
	}
	fmt.Fprintf(deps.Stdout, "  binary: %s\n", presentLabel(plan.removedBinary, "removed", "not installed"))
	if *keepData {
		fmt.Fprintf(deps.Stdout, "  kept:   %s and %s\n", paths.ConfigFile, paths.StateDir)
	} else {
		fmt.Fprintf(deps.Stdout, "  data:   deleted (%s)\n", paths.ConfigFile)
	}
	return 0
}

// verifyStateRoot proves the state directory is the one install recorded
// before any unit is stopped or any file is touched. --keep-data keeps the
// directory, so it must still exist; a normal uninstall may proceed when it is
// already gone.
func verifyStateRoot(paths config.Paths, entry *manifestEntry, keepData bool) error {
	dir, err := fsutil.OpenDirNoFollow(paths.StateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !keepData {
			return nil
		}
		return fmt.Errorf("inspect %s: %w", paths.StateDir, err)
	}
	defer dir.Close()
	if entry.State != manifestOwned {
		return nil
	}
	identity, err := dir.Identity()
	if err != nil {
		return err
	}
	if !entry.Identity.matches(identity) {
		return fmt.Errorf("%s changed since install recorded it", paths.StateDir)
	}
	return nil
}

// verifyOwnedUnits refuses to touch a unit that the manifest does not record as
// owned or whose content no longer carries the Rigscope marker. It returns
// every recorded unit, including ones whose file is already gone: systemd can
// still have a deleted unit loaded, and skipping the stop would leave a
// running service behind while its state is deleted.
func verifyOwnedUnits(deps Deps, paths config.Paths, current *manifest) ([]manifestEntry, error) {
	if dir, err := fsutil.OpenDirNoFollow(paths.UnitDir); err == nil {
		_ = dir.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect unit directory %s: %w", paths.UnitDir, err)
	}
	for _, component := range installComponents {
		path := paths.UnitPath(component.UnitName())
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if systemd.IsLegacySystemManaged(string(content)) {
			return nil, legacyUninstallError([]string{path})
		}
		if !systemd.IsManaged(string(content)) {
			return nil, fmt.Errorf("refusing to remove unrecognized unit %s; inspect it first", path)
		}
		entry := current.entry(path, kindUnit)
		if entry == nil || entry.State != manifestOwned {
			return nil, fmt.Errorf("refusing to uninstall: %s exists but is not recorded as owned in the installation manifest; run rigscope install to repair the installation or inspect the unit first", path)
		}
	}
	return current.unitEntries(), nil
}

// stopAndVerify stops and disables every recorded unit, then proves they are
// inactive. A unit whose file was deleted is still stopped when the manager
// knows it; only a unit systemd reports as unknown and inactive is skipped.
func stopAndVerify(ctx context.Context, manager systemd.Manager, units []manifestEntry) error {
	for _, unit := range units {
		name := unitName(unit.Path)
		known, err := manager.UnitKnown(ctx, name)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if !known {
			active, err := manager.IsActive(ctx, name)
			if err != nil {
				return fmt.Errorf("verify %s is stopped: %w", name, err)
			}
			if active {
				return fmt.Errorf("%s is active but unknown to systemd; refusing to delete any data", name)
			}
			continue
		}
		if err := manager.Stop(ctx, name); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}
	}
	for _, unit := range units {
		name := unitName(unit.Path)
		known, err := manager.UnitKnown(ctx, name)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if !known {
			continue
		}
		if err := manager.Disable(ctx, name); err != nil {
			return fmt.Errorf("disable %s: %w", name, err)
		}
		active, err := manager.IsActive(ctx, name)
		if err != nil {
			return fmt.Errorf("verify %s is stopped: %w", name, err)
		}
		if active {
			return fmt.Errorf("%s is still active after stop; refusing to delete any data", name)
		}
	}
	return nil
}

func unitName(path string) string {
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		return path[index+1:]
	}
	return path
}

// uninstallOp is one verified deletion relative to an open parent directory.
type uninstallOp struct {
	path    string
	name    string
	kind    string
	entry   *manifestEntry
	tree    bool
	skip    bool
	parent  *fsutil.Dir
	deleted bool
}

type uninstallPlan struct {
	paths         config.Paths
	keepData      bool
	ops           []*uninstallOp
	removedUnits  int
	removedBinary bool
	removedUdev   bool
}

func newUninstallPlan(paths config.Paths, current *manifest, keepData bool) (*uninstallPlan, error) {
	plan := &uninstallPlan{paths: paths, keepData: keepData}
	// When the state directory is deleted, its children are covered by the
	// recursive removal; adding them again would race with it.
	var removedTrees []string
	if !keepData {
		if entry := current.entry(paths.StateDir, kindStateDir); entry != nil && entry.State == manifestOwned {
			removedTrees = append(removedTrees, paths.StateDir)
		}
	}
	insideRemovedTree := func(path string) bool {
		for _, tree := range removedTrees {
			if path == tree {
				return false
			}
			if strings.HasPrefix(path, tree+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}

	for _, group := range [][]manifestEntry{current.Units, current.IntegrationFiles, current.Paths} {
		for i := range group {
			entry := group[i]
			if insideRemovedTree(entry.Path) {
				continue
			}
			if entry.Kind == kindBinaryTemps {
				// The binary directory is a shared location. Only the
				// transaction temp files recorded individually in kindBinaryTemp
				// entries are removed; a name pattern alone never authorizes
				// deletion of a foreign file.
				continue
			}
			if keepData && keepOnKeepData(entry.Kind) {
				continue
			}
			if paths.Scope == config.ScopeUser && entry.Kind == kindUdevGPU {
				// The root-owned user-scope GPU rule is removed through the
				// sudo flow in removeUserGPURule, before this plan deletes
				// the ownership record under $HOME.
				continue
			}
			if !deletable(entry) {
				continue
			}
			plan.ops = append(plan.ops, &uninstallOp{
				path:  entry.Path,
				name:  filepath.Base(entry.Path),
				kind:  entry.Kind,
				entry: &entry,
				tree:  isDirectoryKind(entry.Kind),
			})
		}
	}
	sort.SliceStable(plan.ops, func(i, j int) bool {
		return pathDepth(plan.ops[i].path) > pathDepth(plan.ops[j].path)
	})
	return plan, nil
}

func pathDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}

func isDirectoryKind(kind string) bool {
	switch kind {
	case kindStateDir, kindComponentState, kindCollectorState, kindStagingDir, kindBackupsDir:
		return true
	default:
		return false
	}
}

func keepOnKeepData(kind string) bool {
	switch kind {
	case kindConfig, kindStateDir, kindComponentState, kindCollectorState,
		kindEnrollmentGrant, kindPairingBootstrap, kindOwnerBootstrap, kindCredentials, kindUpgradeJournal,
		kindStagingDir, kindBackupsDir, kindBinaryTemps:
		return true
	default:
		return false
	}
}

// deletable decides whether an entry may be removed at all. Only an owned
// entry authorizes deletion: a planned entry is a path install intended to
// create, and a reserved pathname (rigscope.prev, the journal, staging or
// backups) is never enough on its own. Activation records every artifact it
// creates, under the same lock, so uninstall can tell its files from a foreign
// file that merely appeared at the expected name.
func deletable(entry manifestEntry) bool {
	return entry.State == manifestOwned && entry.Identity != nil
}

func (p *uninstallPlan) close() {
	for _, op := range p.ops {
		if op.parent != nil {
			_ = op.parent.Close()
			op.parent = nil
		}
	}
}

func (p *uninstallPlan) verify() error {
	for _, op := range p.ops {
		if err := verifyOp(op); err != nil {
			return err
		}
	}
	return nil
}

func verifyOp(op *uninstallOp) error {
	parent, err := fsutil.OpenDirNoFollow(filepath.Dir(op.path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The parent is gone, so the entry cannot exist either.
			op.skip = true
			return nil
		}
		return fmt.Errorf("verify parent of %s: %w", op.path, err)
	}
	op.parent = parent
	if op.entry.Parent != nil {
		identity, err := parent.Identity()
		if err != nil {
			return err
		}
		if !op.entry.Parent.matches(identity) {
			return fmt.Errorf("refusing to delete %s: parent directory %s changed since install recorded it", op.path, filepath.Dir(op.path))
		}
	}
	identity, err := parent.Stat(op.name)
	if errors.Is(err, os.ErrNotExist) {
		op.skip = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", op.path, err)
	}
	if op.entry.State == manifestOwned && !identityDriftAllowed(op.kind) {
		if !op.entry.Identity.matches(identity) {
			return fmt.Errorf("refusing to delete %s: its identity changed since install recorded it; inspect it first", op.path)
		}
	}
	switch {
	case op.tree:
		if identity.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			return fmt.Errorf("refusing to delete %s: it is not a directory", op.path)
		}
	default:
		if identity.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			return fmt.Errorf("refusing to delete %s: it is a directory", op.path)
		}
		if identity.Mode&syscall.S_IFMT == syscall.S_IFLNK {
			return fmt.Errorf("refusing to delete %s: it is a symbolic link", op.path)
		}
	}
	if isMarkerKind(op.kind) {
		content, err := parent.ReadFile(op.name)
		if err != nil {
			return fmt.Errorf("read %s: %w", op.path, err)
		}
		if !systemd.IsManaged(string(content)) {
			return fmt.Errorf("refusing to remove unrecognized file %s; inspect it first", op.path)
		}
	}
	if op.entry.State == manifestOwned && verifiesSHA(op.kind) && op.entry.SHA256 != "" {
		content, err := parent.ReadFile(op.name)
		if err != nil {
			return fmt.Errorf("read %s: %w", op.path, err)
		}
		if fsutil.SHA256Bytes(content) != op.entry.SHA256 {
			return fmt.Errorf("refusing to delete %s: its content changed since install recorded it", op.path)
		}
	}
	return nil
}

// identityDriftAllowed lists paths that a successful upgrade legitimately
// replaces or rewrites outside install. Their parent identity and type are
// still verified, but a changed inode does not block deletion.
func identityDriftAllowed(kind string) bool {
	switch kind {
	case kindBinary, kindPreviousBinary, kindConfig, kindEnrollmentGrant:
		return true
	default:
		return false
	}
}

func isMarkerKind(kind string) bool {
	switch kind {
	case kindUnit, kindSysusers, kindUdevRAPL, kindUdevGPU:
		return true
	default:
		return false
	}
}

func verifiesSHA(kind string) bool {
	switch kind {
	case kindUnit, kindSysusers, kindUdevRAPL, kindUdevGPU, kindPairingBootstrap, kindOwnerBootstrap, kindBinaryTemp:
		return true
	default:
		return false
	}
}

func (p *uninstallPlan) execute(ctx context.Context, deps Deps) error {
	for _, op := range p.ops {
		if op.skip {
			continue
		}
		if op.tree {
			if err := op.parent.RemoveAll(op.name); err != nil {
				return fmt.Errorf("remove %s: %w", op.path, err)
			}
		} else {
			if err := op.parent.Remove(op.name); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf("remove %s: %w", op.path, err)
			}
		}
		op.deleted = true
		switch op.kind {
		case kindUnit:
			p.removedUnits++
		case kindBinary:
			p.removedBinary = true
		case kindUdevRAPL, kindUdevGPU:
			p.removedUdev = true
		}
	}
	if len(p.ops) > 0 {
		_ = deps.manager(p.paths.Scope).DaemonReload(ctx)
	}
	if p.removedUdev {
		// Removing the rule file does not tell the running udev daemon. Reload
		// it through the injected runner and report a failure instead of
		// silently leaving the old group/mode grants active.
		if deps.RunCommand == nil {
			deps.warnf(deps.Stderr, "could not reload udev rules: no command runner is available")
		} else if err := deps.RunCommand(ctx, "udevadm", "control", "--reload"); err != nil {
			deps.warnf(deps.Stderr, "could not reload udev rules: %v", err)
		}
	}
	if !p.keepData {
		// Remove the configuration directory only when it is empty; it is a
		// shared parent for other user files.
		removeIfEmptyNoFollow(p.paths.ConfigDir)
	}
	return nil
}

// isBinaryTemp matches the transaction-unique temporary names
// internal/platform/fsutil writes beside the installed binary.
func isBinaryTemp(name, base string) bool {
	if name == base+".prev.tmp" {
		return true
	}
	if strings.HasPrefix(name, "."+base+".tmp-") {
		return true
	}
	prefix := "." + base + "."
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rest := strings.TrimPrefix(name, prefix)
	for _, suffix := range []string{".new", ".prev", ".restore-new", ".restore-old"} {
		if strings.HasSuffix(rest, suffix) && len(rest) > len(suffix) {
			return true
		}
	}
	return false
}

// keepManifest rewrites the ownership record after --keep-data so a later
// uninstall deletes only configuration and state. Units, the binary and
// integration files are gone and are no longer claimed.
func keepManifest(paths config.Paths, current *manifest, version string) error {
	kept := &manifest{
		Marker:        manifestMarker,
		FormatVersion: manifestFormatVersion,
		Version:       version,
		Scope:         paths.Scope,
	}
	for _, entry := range current.Paths {
		if keepOnKeepData(entry.Kind) {
			kept.Paths = append(kept.Paths, entry)
		}
	}
	return writeManifest(paths.ManifestFile(), kept)
}

func presentLabel(present bool, yes, no string) string {
	if present {
		return yes
	}
	return no
}

// removeIfEmptyNoFollow removes dir when it contains nothing. It never
// recurses and refuses a symlinked parent.
func removeIfEmptyNoFollow(dir string) {
	parent, err := fsutil.OpenDirNoFollow(filepath.Dir(dir))
	if err != nil {
		return
	}
	defer parent.Close()
	_ = parent.RemoveDir(filepath.Base(dir))
}
