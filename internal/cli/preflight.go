package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// preflightInstall inspects every destination before any mutation. It refuses
// when a Rigscope 0.2 installation is present, when a foreign file occupies a
// path 1.x would write, or when a parent directory has been replaced by a
// symlink. A valid 1.x manifest makes its owned paths overwritable; planned
// paths may be created only when they do not exist yet.
func preflightInstall(paths config.Paths) (*manifest, error) {
	return preflightSetup(paths, false)
}

// Only install may replace unchanged default 0.2 udev rules. Other commands
// retain the normal manifest ownership requirement.
func preflightSetup(paths config.Paths, allowLegacyRules bool) (*manifest, error) {
	if found := detectLegacy(paths); len(found) > 0 {
		return nil, legacyPresentError(found)
	}
	current, err := loadManifest(paths.ManifestFile(), paths)
	switch {
	case errors.Is(err, errManifestMissing):
		current = nil
	case err != nil:
		return nil, err
	}

	for path, kind := range expectedKinds(paths) {
		if paths.Scope == config.ScopeUser && kind == kindUdevGPU {
			// The user-scope GPU rule is root-owned and only ever replaced or
			// removed through the explicit sudo flow, which adopts an existing
			// working rule or refuses a foreign one. Preflight cannot inspect
			// it the way it inspects files under $HOME.
			continue
		}
		info, err := os.Lstat(path)
		exists := true
		switch {
		case errors.Is(err, os.ErrNotExist):
			exists = false
		case err != nil:
			return nil, fmt.Errorf("inspect %s: %w", path, err)
		}
		// Refuse a symlinked parent before install writes anything into it.
		// The binary directory itself is a shared location; only its parents
		// are checked, and the directory is opened no-follow below.
		if kind != kindBinaryTemps {
			parent, err := fsutil.OpenDirNoFollow(filepath.Dir(path))
			if err == nil {
				_ = parent.Close()
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("refusing to use %s: %w", path, err)
			}
		}
		if !exists {
			continue
		}
		entry := current.entry(path, kind)
		staged := stagedMatches(entry, path)
		switch kind {
		case kindEnrollmentGrant, kindCredentials:
			// The collector directory is the ownership boundary for the
			// collector's private files. Runtime enrollment creates and replaces
			// credentials.json and consumes enrollment.json, so their individual
			// inode identities legitimately go stale. An owned collector
			// directory still anchors deletion (no-follow, mount-checked).
			if current.owned(paths.CollectorDir(), kindCollectorState) {
				continue
			}
			if entry == nil || (entry.State != manifestOwned && !staged) {
				return nil, unownedPathError(path)
			}
		case kindUnit, kindSysusers, kindUdevRAPL, kindUdevGPU:
			if allowLegacyRules && (entry == nil || entry.State == manifestPlanned) && isDefaultLegacyUdev(path, kind) {
				continue
			}
			if err := checkMarkerFile(path, info, kind, current, staged); err != nil {
				return nil, err
			}
		case kindStateDir, kindComponentState, kindCollectorState, kindStagingDir, kindBackupsDir:
			if !info.IsDir() {
				return nil, fmt.Errorf("refusing to overwrite %s: it is not a directory", path)
			}
			if entry == nil {
				if kind == kindStateDir {
					if err := ensureUnclaimedDirectory(path); err != nil {
						return nil, err
					}
					continue
				}
				return nil, unownedPathError(path)
			}
			if entry.State == manifestOwned {
				if err := checkOwnedIdentity(path, entry); err != nil {
					return nil, err
				}
			} else if err := ensureUnclaimedDirectory(path); err != nil {
				// An interrupted install may have created an empty directory
				// before promoting it to owned. Anything inside is foreign.
				return nil, err
			}
		case kindBinaryTemps:
			if !info.IsDir() {
				return nil, fmt.Errorf("refusing to use %s: it is not a directory", path)
			}
			if _, err := fsutil.OpenDirNoFollow(path); err != nil {
				return nil, fmt.Errorf("refusing to use %s: %w", path, err)
			}
		case kindPreviousBinary, kindUpgradeJournal, kindBinaryTemp:
			if info.IsDir() {
				return nil, fmt.Errorf("refusing to overwrite %s: it is a directory", path)
			}
			if entry == nil {
				return nil, unownedPathError(path)
			}
			// A reserved name alone never authorizes adoption. Only an entry
			// activation promoted to owned (or the exact file it durably staged
			// before an interrupted publication) may be used.
			if entry.State != manifestOwned && !staged {
				return nil, unownedPathError(path)
			}
			if entry.State == manifestOwned && !staged && !identityDriftAllowed(kind) {
				if err := checkOwnedIdentity(path, entry); err != nil {
					return nil, err
				}
			}
		default:
			if info.IsDir() {
				return nil, fmt.Errorf("refusing to overwrite %s: it is a directory", path)
			}
			if entry == nil {
				return nil, unownedPathError(path)
			}
			if entry.State == manifestPlanned && !staged {
				// Install never created this file, so it is foreign.
				return nil, unownedPathError(path)
			}
			if !identityDriftAllowed(kind) && entry.Identity != nil && !staged {
				if err := checkOwnedIdentity(path, entry); err != nil {
					return nil, err
				}
			}
		}
	}
	return current, nil
}

func checkMarkerFile(path string, info os.FileInfo, kind string, current *manifest, staged bool) error {
	if info.IsDir() {
		return fmt.Errorf("refusing to overwrite %s: it is a directory", path)
	}
	entry := current.entry(path, kind)
	if entry == nil || (entry.State != manifestOwned && !staged) {
		return unownedPathError(path)
	}
	if !staged {
		if err := checkOwnedIdentity(path, entry); err != nil {
			return err
		}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if systemd.IsLegacySystemManaged(string(content)) {
		return legacyPresentError([]string{path})
	}
	if !systemd.IsManaged(string(content)) {
		return fmt.Errorf("refusing to overwrite %s: it no longer carries the Rigscope ownership marker; inspect it first", path)
	}
	return nil
}

// stagedMatches reports whether path is exactly the file an interrupted
// publication recorded before publishing. It is the only way a file without a
// promoted owned entry is accepted as Rigscope's.
func stagedMatches(entry *manifestEntry, path string) bool {
	if entry == nil || entry.StagedSHA256 == "" {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	sum, err := fsutil.SHA256File(path)
	return err == nil && sum == entry.StagedSHA256
}

// checkOwnedIdentity refuses a path whose inode or owner changed since install
// recorded it. The binary is handled by the caller because upgrade replaces it
// with a different inode by design.
func checkOwnedIdentity(path string, entry *manifestEntry) error {
	identity, err := fsutil.IdentityOfPath(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !entry.Identity.matches(identity) {
		return fmt.Errorf("refusing to use %s: its identity changed since install recorded it; inspect it and run rigscope install again", path)
	}
	return nil
}

// ensureUnclaimedDirectory permits a directory only when it is empty. Any real
// content needs a 1.x manifest.
func ensureUnclaimedDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	for _, entry := range entries {
		return fmt.Errorf("refusing to use non-empty state directory %s (found %s); it is not recorded in a Rigscope 1.x installation manifest", path, entry.Name())
	}
	return nil
}

func unownedPathError(path string) error {
	return fmt.Errorf("refusing to overwrite %s: it exists but is not recorded in a Rigscope 1.x installation manifest as owned; inspect it and move it away first", path)
}

// detectLegacy lists 0.2 artifacts that mean an old installation still owns
// this machine. It is read-only and runs before install or uninstall mutates
// anything.
func detectLegacy(paths config.Paths) []string {
	var found []string
	if names, err := systemd.LegacyUnits(paths.UnitDir); err == nil {
		for _, name := range names {
			found = append(found, filepath.Join(paths.UnitDir, name))
		}
	}
	for _, component := range installComponents {
		path := paths.UnitPath(component.UnitName())
		content, err := os.ReadFile(path)
		if err == nil && systemd.IsLegacySystemManaged(string(content)) {
			found = append(found, path)
		}
	}
	if path := filepath.Join(paths.UnitDir, "rigscope.service"); fileExists(path) {
		found = append(found, path)
	}
	if paths.Scope == config.ScopeUser {
		found = append(found, existingPaths(paths.StateDir,
			"config.json", "release-state.json", "reset-pending.json", "runtime", "runtime-previous",
			filepath.Join("hub", "enrollment.json"),
			filepath.Join("collector", "state.json"),
		)...)
	} else {
		found = append(found, existingPaths(paths.StateDir, "system-install.json", "power-access.json")...)
		found = append(found, existingPaths(paths.ConfigDir, "hub.env", "collector.env")...)
		found = append(found, existingPaths(filepath.Dir(paths.SysusersFile), "rigscope-rapl.conf")...)
		if path := filepath.Join(systemRoot(paths), "opt", "rigscope"); fileExists(path) {
			found = append(found, path)
		}
	}
	return found
}

func existingPaths(root string, names ...string) []string {
	var found []string
	for _, name := range names {
		path := filepath.Join(root, name)
		if fileExists(path) {
			found = append(found, path)
		}
	}
	return found
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// systemRoot recovers the optional test/image root prefix from a system path
// such as <root>/usr/local/bin.
func systemRoot(paths config.Paths) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(paths.BinaryDir)))
}

func legacyPresentError(found []string) error {
	return fmt.Errorf(`a Rigscope 0.2 installation is present (%s)

Rigscope 1.0 does not import 0.2 data. Remove the old installation first:
  rigscope uninstall          # part of 0.2, or: npx --yes rigscope@latest uninstall
Then run this installer again`, strings.Join(compact(found), ", "))
}

func legacyUninstallError(found []string) error {
	return fmt.Errorf(`a Rigscope 0.2 installation is present (%s)

Rigscope 1.x refuses to delete 0.2 files or data. Remove the old installation with its own uninstall command:
  rigscope uninstall          # part of 0.2, or: npx --yes rigscope@latest uninstall
Then run rigscope uninstall again for the 1.x installation, if any`, strings.Join(compact(found), ", "))
}

func compact(items []string) []string {
	seen := make(map[string]bool, len(items))
	var result []string
	for _, item := range items {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		result = append(result, item)
	}
	return result
}
