package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// Scope selects the documented installation layout.
type Scope string

const (
	// ScopeUser is the default per-user installation below $HOME.
	ScopeUser Scope = "user"
	// ScopeSystem is the root-owned installation for a machine-wide service.
	ScopeSystem Scope = "system"
)

// ParseScope validates a scope spelling.
func ParseScope(value string) (Scope, error) {
	switch Scope(value) {
	case ScopeUser, ScopeSystem:
		return Scope(value), nil
	case "":
		return ScopeUser, nil
	default:
		return "", errors.New(`scope must be "user" or "system"`)
	}
}

// Env supplies the environment path resolution depends on. Root is an optional
// prefix used by tests and image builds; it applies to system-scope paths only.
// RuntimeDir is XDG_RUNTIME_DIR for user scope. UID identifies the invoking
// user for the /tmp lock fallback when RuntimeDir is empty.
type Env struct {
	Home          string
	XDGConfigHome string
	XDGStateHome  string
	XDGDataHome   string
	RuntimeDir    string
	UID           int
	Root          string
}

// Paths is the resolved 1.0 installation layout.
type Paths struct {
	Scope          Scope
	ConfigFile     string
	StateDir       string
	BinaryDir      string
	Binary         string
	PreviousBinary string
	ConfigDir      string
	UnitDir        string
	// LockPath is deliberately outside StateDir: uninstall deletes the state
	// directory while holding the flock, and removing a locked file would make
	// a concurrent operation create a second inode with its own lock.
	LockPath string
	// Account and hardware-access integration files. SysusersFile is system
	// scope only. The udev paths also resolve in user scope because a user
	// installation can opt in to GPU register access and doctor reports on an
	// existing RAPL rule; they stay root-owned and are applied through sudo,
	// never written directly by the user-scope installer.
	SysusersFile     string
	UdevRAPLFile     string
	UdevGPUTempsFile string
}

// Resolve computes the paths for a scope. An empty home or XDG value falls
// back to the documented default; a relative value is rejected.
func Resolve(scope Scope, env Env) (Paths, error) {
	switch scope {
	case ScopeSystem:
		return systemPaths(env.Root), nil
	case ScopeUser:
		return userPaths(env)
	default:
		return Paths{}, errors.New(`scope must be "user" or "system"`)
	}
}

func userPaths(env Env) (Paths, error) {
	home := env.Home
	if home == "" {
		return Paths{}, errors.New("home directory is unknown; set HOME")
	}
	if !filepath.IsAbs(home) {
		return Paths{}, errors.New("home directory must be absolute")
	}
	configHome := env.XDGConfigHome
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	stateHome := env.XDGStateHome
	if stateHome == "" {
		stateHome = filepath.Join(home, ".local", "state")
	}
	dataHome := env.XDGDataHome
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	_ = dataHome // reserved for hub UI assets and future caches
	if !filepath.IsAbs(configHome) || !filepath.IsAbs(stateHome) {
		return Paths{}, errors.New("XDG directories must be absolute")
	}
	binaryDir := filepath.Join(home, ".local", "bin")
	configDir := filepath.Join(configHome, "rigscope")
	stateDir := filepath.Join(stateHome, "rigscope")
	binary := filepath.Join(binaryDir, "rigscope")
	return Paths{
		Scope:          ScopeUser,
		ConfigFile:     filepath.Join(configDir, "config.json"),
		StateDir:       stateDir,
		BinaryDir:      binaryDir,
		Binary:         binary,
		PreviousBinary: binary + ".prev",
		ConfigDir:      configDir,
		UnitDir:        filepath.Join(configHome, "systemd", "user"),
		// GPU register access is an optional root-owned integration even in a
		// user installation. Resolving the path here keeps the manifest,
		// uninstall and doctor using one spelling; the installer itself never
		// writes it without sudo.
		UdevRAPLFile:     hostPath(env.Root, "etc", "udev", "rules.d", "90-rigscope-rapl.rules"),
		UdevGPUTempsFile: hostPath(env.Root, "etc", "udev", "rules.d", "90-rigscope-gpu-temps.rules"),
		LockPath:         userLockPath(env),
	}, nil
}

// userLockPath keeps the installation lock out of the state directory.
// XDG_RUNTIME_DIR is the correct place; without it, fall back to a
// per-user directory under /tmp. internal/platform/lock verifies that the
// fallback directory is owned by the invoking user and is mode 0700 before
// trusting it.
func userLockPath(env Env) string {
	if env.RuntimeDir != "" && filepath.IsAbs(env.RuntimeDir) {
		return filepath.Join(env.RuntimeDir, "rigscope", "install.lock")
	}
	uid := env.UID
	if uid == 0 {
		// A zero-value Env (tests, library callers) means "the invoking
		// user", not root.
		uid = os.Getuid()
	}
	return filepath.Join("/tmp", "rigscope-"+strconv.Itoa(uid), "install.lock")
}

// hostPath resolves a host-owned path such as /etc or /sys below an optional
// test/image root prefix. An empty root means the real filesystem.
func hostPath(root string, parts ...string) string {
	if root == "" {
		return filepath.Join(append([]string{string(filepath.Separator)}, parts...)...)
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

func systemPaths(root string) Paths {
	prefix := func(parts ...string) string {
		return hostPath(root, parts...)
	}
	binary := prefix("usr", "local", "bin", "rigscope")
	return Paths{
		Scope:            ScopeSystem,
		ConfigFile:       prefix("etc", "rigscope", "config.json"),
		StateDir:         prefix("var", "lib", "rigscope"),
		BinaryDir:        prefix("usr", "local", "bin"),
		Binary:           binary,
		PreviousBinary:   binary + ".prev",
		ConfigDir:        prefix("etc", "rigscope"),
		UnitDir:          prefix("etc", "systemd", "system"),
		SysusersFile:     prefix("etc", "sysusers.d", "rigscope.conf"),
		UdevRAPLFile:     prefix("etc", "udev", "rules.d", "90-rigscope-rapl.rules"),
		UdevGPUTempsFile: prefix("etc", "udev", "rules.d", "90-rigscope-gpu-temps.rules"),
		LockPath:         prefix("run", "rigscope", "install.lock"),
	}
}

// ComponentStateDir returns the private state directory for one component.
// User installs share the state directory; system installs separate the hub and
// collector accounts.
func (p Paths) ComponentStateDir(component string) string {
	if p.Scope == ScopeSystem {
		return filepath.Join(p.StateDir, component)
	}
	return p.StateDir
}

// UnitPath returns the unit file path for one unit name.
func (p Paths) UnitPath(unit string) string {
	return filepath.Join(p.UnitDir, unit)
}

// ManifestFile is the installation ownership record. Without a valid file at
// this path, install refuses to overwrite and uninstall refuses to delete.
func (p Paths) ManifestFile() string {
	return filepath.Join(p.StateDir, "install-manifest.json")
}

// LockFile serialises install, uninstall and service commands. It lives
// outside StateDir so uninstall can delete the state directory without
// removing the inode it holds locked. A zero-value Paths (tests that construct
// one literally) falls back to the old in-state location.
func (p Paths) LockFile() string {
	if p.LockPath != "" {
		return p.LockPath
	}
	return filepath.Join(p.StateDir, ".install.lock")
}

// UpgradeJournalFile is the durable record of the upgrade transaction.
func (p Paths) UpgradeJournalFile() string {
	return filepath.Join(p.StateDir, "upgrade-journal.json")
}

// StagingDir holds verified release binaries before activation.
func (p Paths) StagingDir() string {
	return filepath.Join(p.StateDir, "staging")
}

// BackupsDir holds upgrade backup sets.
func (p Paths) BackupsDir() string {
	return filepath.Join(p.StateDir, "backups")
}

// CollectorDir holds the collector's private enrollment and credential files.
// It is always <stateRoot>/collector, including user scope, so those files can
// be mode 0700/0600 independently of the shared state root.
func (p Paths) CollectorDir() string {
	return filepath.Join(p.StateDir, "collector")
}

// CollectorRuntimeLock is the collector's process lease. It lives inside the
// collector-owned subtree, so system scope can create it even though the state
// root itself is root-owned (the unit's ReadWritePaths covers only
// ComponentStateDir). The collector runtime computes the same path.
func (p Paths) CollectorRuntimeLock() string {
	return filepath.Join(p.CollectorDir(), "collector-runtime.lock")
}

// HubRuntimeLock is the hub's process lease (also taken by offline
// `reset-password`), inside the hub-owned subtree so system scope can create
// it even though the state root is root-owned.
func (p Paths) HubRuntimeLock() string {
	return filepath.Join(p.ComponentStateDir("hub"), HubLockFile)
}

// HubLockFile is the hub lease file name inside the hub state directory.
const HubLockFile = "hub.lock"

// EnrollmentFile is the transient one-time enrollment grant. Future enrollment
// code consumes and deletes it; it is never part of the configuration file.
func (p Paths) EnrollmentFile() string {
	return filepath.Join(p.CollectorDir(), "enrollment.json")
}

// CredentialsFile is the issued per-collector credential. It is created by
// enrollment, not by install.
func (p Paths) CredentialsFile() string {
	return filepath.Join(p.CollectorDir(), "credentials.json")
}

// EnsureDirs creates the private directories the layout needs. It returns the
// directories it created so callers can report them. The system state root is
// traversable (0755) because the per-component directories are owned by
// different accounts; configuration and state stay private.
func (p Paths) EnsureDirs() ([]string, error) {
	stateMode := os.FileMode(0o700)
	configMode := os.FileMode(0o700)
	if p.Scope == ScopeSystem {
		stateMode = 0o755
		configMode = 0o755
	}
	specs := []struct {
		path string
		mode os.FileMode
	}{
		{p.ConfigDir, configMode},
		{p.StateDir, stateMode},
		{p.BinaryDir, 0o755},
		{p.UnitDir, 0o755},
	}
	var created []string
	for _, spec := range specs {
		if spec.path == "" {
			continue
		}
		if _, err := os.Stat(spec.path); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(spec.path, spec.mode); err != nil {
				return created, err
			}
			created = append(created, spec.path)
			continue
		} else if err != nil {
			return created, err
		}
		// Repair the permissions of the two private roots on reinstall.
		if spec.path == p.ConfigDir || spec.path == p.StateDir {
			if err := os.Chmod(spec.path, spec.mode); err != nil {
				return created, err
			}
		}
	}
	return created, nil
}
