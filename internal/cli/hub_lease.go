package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/lock"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// hubLeasePath is the hub runtime lease inside an explicit hub state directory.
// The hub, reset-password and the upgrade transaction all derive the path here,
// so a running hub, an offline recovery command and a migration cannot each
// take a different inode.
func hubLeasePath(hubStateDir string) string {
	return filepath.Join(hubStateDir, config.HubLockFile)
}

// hubLeasePathForStateRoot derives the hub runtime lease from an installation
// state root and scope, exactly as config.Paths.HubRuntimeLock does. It is used
// by migrate, whose --state flag may name the installation root directly.
func hubLeasePathForStateRoot(scope config.Scope, stateRoot string) string {
	return config.Paths{Scope: scope, StateDir: stateRoot}.HubRuntimeLock()
}

// hubLeasePrepare returns the ownership adjustment for a newly acquired hub
// lease, or nil. In system scope the hub runs as its own account inside a
// 0700 directory; if the upgrade created the lease file as root, the hub could
// not open it on the next start. Chowning the held descriptor gives the account
// its lease back without ever touching a pathname.
func hubLeasePrepare(deps Deps, scope config.Scope) func(*os.File) error {
	if scope != config.ScopeSystem {
		return nil
	}
	return func(file *os.File) error {
		return deps.chownFile(file, systemd.HubUser, systemd.SharedGroup)
	}
}

// migrationHubLease acquires the hub runtime lease for a standalone migrate, or
// verifies a descriptor inherited from an upgrade transaction that already
// holds it. A busy lease means the hub (or another offline command) is running,
// so the migration must refuse instead of racing the database.
func migrationHubLease(path, inherited string) (*lock.Lock, error) {
	if inherited != "" {
		fd, err := strconv.Atoi(inherited)
		if err != nil {
			return nil, fmt.Errorf("invalid inherited hub lock descriptor %q", inherited)
		}
		return lock.AcquireInherited(path, fd)
	}
	held, err := lock.Acquire(path)
	if err != nil {
		if errors.Is(err, lock.ErrBusy) {
			return nil, fmt.Errorf("the hub is running; stop it before migrating: %v", err)
		}
		return nil, err
	}
	return held, nil
}

// hubRuntimeLease adapts the hub process lease to activation.HubLease. The
// upgrade transaction takes it after quiescing the hub and holds it across the
// backup and migration; the staged migrate child inherits File().
type hubRuntimeLease struct {
	path string
	// prepare may adjust the open lease file (system scope hands it to the hub
	// account) before the transaction trusts it.
	prepare func(*os.File) error

	mu   sync.Mutex
	held *lock.Lock
}

func newHubRuntimeLease(path string, prepare func(*os.File) error) *hubRuntimeLease {
	return &hubRuntimeLease{path: path, prepare: prepare}
}

// Acquire takes the lease without blocking. A busy lease is reported as "the
// hub is running" so the transaction fails before touching the database.
func (l *hubRuntimeLease) Acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held != nil {
		return nil, errors.New("the hub runtime lease is already held")
	}
	held, err := lock.Acquire(l.path)
	if err != nil {
		if errors.Is(err, lock.ErrBusy) {
			return nil, fmt.Errorf("the hub is running: another process holds %s", l.path)
		}
		return nil, err
	}
	if l.prepare != nil {
		if err := l.prepare(held.File()); err != nil {
			_ = held.Release()
			return nil, fmt.Errorf("prepare the hub runtime lease %s: %w", l.path, err)
		}
	}
	l.held = held
	return l.Release, nil
}

// File exposes the held lease descriptor to the staged migration child.
func (l *hubRuntimeLease) File() *os.File {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		return nil
	}
	return l.held.File()
}

// Release unlocks and closes the lease. It is idempotent.
func (l *hubRuntimeLease) Release() {
	l.mu.Lock()
	held := l.held
	l.held = nil
	l.mu.Unlock()
	if held != nil {
		_ = held.Release()
	}
}
