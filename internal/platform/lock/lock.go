//go:build linux

package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ErrBusy is returned when another process holds the installation lock.
var ErrBusy = errors.New("another rigscope operation is running")

// pollInterval is how often a timed acquisition retries the non-blocking flock.
const pollInterval = 50 * time.Millisecond

// Lock is one held installation lock. Release it when the operation finishes.
type Lock struct {
	file *os.File
	path string
	// inherited marks a descriptor received from the parent process. Closing it
	// must not unlock the shared open file description, because the parent
	// still owns that lock.
	inherited bool
}

// InheritedLockFDEnv names the environment variable that carries the number of
// the inherited, already-locked installation descriptor. The parent passes the
// file in cmd.ExtraFiles; the child verifies it before skipping acquisition.
const InheritedLockFDEnv = "RIGSCOPE_INSTALL_LOCK_FD"

// InheritedHubLockFDEnv names the same mechanism for the hub runtime lease an
// upgrade transaction holds across the backup and migration. The staged
// migrate child inherits that descriptor so it does not refuse its own parent.
const InheritedHubLockFDEnv = "RIGSCOPE_HUB_LOCK_FD"

// Acquire takes the flock at path without blocking. A held lock returns an
// error wrapping ErrBusy so callers can report a clear "already running"
// message instead of waiting behind an interactive command.
func Acquire(path string) (*Lock, error) {
	return acquire(path)
}

// AcquireWithTimeout waits up to timeout for the lock, retrying without a
// blocking syscall so the caller can still be cancelled by process exit.
func AcquireWithTimeout(path string, timeout time.Duration) (*Lock, error) {
	if timeout <= 0 {
		return acquire(path)
	}
	deadline := time.Now().Add(timeout)
	for {
		held, err := acquire(path)
		if err == nil {
			return held, nil
		}
		if !errors.Is(err, ErrBusy) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		time.Sleep(pollInterval)
	}
}

func acquire(path string) (*Lock, error) {
	if path == "" {
		return nil, errors.New("lock path is empty")
	}
	dir := filepath.Dir(path)
	if err := ensureLockDir(dir); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open installation lock %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("open installation lock %s: invalid descriptor", path)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w (lock %s)", ErrBusy, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{file: file, path: path}, nil
}

// AcquireInherited takes over an installation lock that the parent process
// passed in cmd.ExtraFiles. It proves the descriptor is the lock inode at path
// and takes (or re-takes, for the same open file description) the exclusive
// lock before the caller may skip its own acquisition. A descriptor for
// another inode, an unlocked file another process holds, or a closed descriptor
// is refused.
func AcquireInherited(path string, fd int) (*Lock, error) {
	if path == "" {
		return nil, errors.New("lock path is empty")
	}
	if fd < 3 {
		return nil, fmt.Errorf("inherited lock descriptor %d is not valid", fd)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, fmt.Errorf("inherited lock descriptor %d is not open", fd)
	}
	var inherited, onDisk syscall.Stat_t
	if err := syscall.Fstat(fd, &inherited); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect inherited lock: %w", err)
	}
	if err := syscall.Stat(path, &onDisk); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect installation lock %s: %w", path, err)
	}
	if inherited.Dev != onDisk.Dev || inherited.Ino != onDisk.Ino {
		_ = file.Close()
		return nil, fmt.Errorf("inherited lock descriptor does not match %s", path)
	}
	// flock locks belong to the open file description. Re-locking an inherited
	// descriptor of the parent's locked description succeeds; a foreign
	// descriptor for the same inode blocks while the parent holds the lock.
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w (lock %s)", ErrBusy, path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{file: file, path: path, inherited: true}, nil
}

// File returns the open descriptor of the held lock. The parent passes it to a
// child through cmd.ExtraFiles so the child can prove it inherits the same
// locked inode instead of trusting a flag.
func (l *Lock) File() *os.File {
	if l == nil {
		return nil
	}
	return l.file
}

// ensureLockDir creates the lock directory and refuses to trust a pre-existing
// directory that another account could have prepared. The /tmp fallback is the
// important case: without these checks a local attacker could pre-create
// /tmp/rigscope-<uid> with mode 0777 (or as a symlink) and win the flock race.
func ensureLockDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create lock directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect lock directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("lock directory %s is a symbolic link", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("lock path parent %s is not a directory", dir)
	}
	// The /tmp fallback is the only directory an unprivileged attacker can
	// pre-create. XDG_RUNTIME_DIR and /run/rigscope are trusted by their
	// location, and tests use their own temporary trees.
	if !isTmpFallback(dir) {
		return nil
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("lock directory %s is mode %04o; must be 0700", dir, info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("lock directory %s has no ownership information", dir)
	}
	if euid := os.Geteuid(); euid != 0 && int(stat.Uid) != euid {
		return fmt.Errorf("lock directory %s is owned by uid %d, not the invoking user %d", dir, stat.Uid, euid)
	}
	return nil
}

func isTmpFallback(dir string) bool {
	return filepath.Dir(dir) == "/tmp" && strings.HasPrefix(filepath.Base(dir), "rigscope-")
}

// Release unlocks and closes the file. It is safe to call once per Lock.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	var unlockErr error
	if !l.inherited {
		unlockErr = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	}
	// An inherited descriptor shares the parent's open file description.
	// Closing our copy leaves the parent's lock in place; an explicit unlock
	// would release it for the parent too.
	closeErr := file.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock %s: %w", l.path, unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock %s: %w", l.path, closeErr)
	}
	return nil
}

// Path returns the lock file path.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}
