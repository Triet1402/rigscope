package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomicPrepared is WriteFileAtomic with a hook that runs on the
// open, fsynced temporary file before it is renamed into place. Ownership and
// mode changes therefore happen through the descriptor, so a directory owner
// cannot swap the target pathname for a symlink between the write and the
// chown.
func WriteFileAtomicPrepared(path string, data []byte, perm os.FileMode, prepare func(*os.File) error) error {
	return writeFileDurable(path, data, perm, true, prepare, syncDir)
}

// WriteFileAtomicNoReplace writes data to path through a temporary file and
// publishes it with link(2), which fails if path already exists. It is used
// when install creates a file for the first time: a foreign file that appeared
// at the path is never overwritten and never adopted into the manifest.
func WriteFileAtomicNoReplace(path string, data []byte, perm os.FileMode, prepare func(*os.File) error) error {
	return writeFileDurable(path, data, perm, false, prepare, syncDir)
}

// writeFileDurable is the shared implementation. syncDirFn is injected so a
// test can prove that a directory fsync failure is returned instead of being
// discarded: without it a crash could lose the rename even though the command
// reported success.
func writeFileDurable(path string, data []byte, perm os.FileMode, replace bool, prepare func(*os.File) error, syncDirFn func(string) error) error {
	dir := filepath.Dir(path)
	if err := ensureDir(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	name := tmp.Name()
	failed := true
	defer func() {
		if failed {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("set mode on %s: %w", name, err)
	}
	if prepare != nil {
		if err := prepare(tmp); err != nil {
			return err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if replace {
		if err := os.Rename(name, path); err != nil {
			return fmt.Errorf("replace %s: %w", path, err)
		}
	} else {
		if err := os.Link(name, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists: %w", path, os.ErrExist)
			}
			return fmt.Errorf("create %s: %w", path, err)
		}
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove temporary file %s: %w", name, err)
		}
	}
	if err := syncDirFn(dir); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	failed = false
	return nil
}
