package activation

import (
	"os"
	"path/filepath"

	"github.com/Triet1402/rigscope/internal/platform/fsutil"
)

// OSFS is the real filesystem implementation of FS.
type OSFS struct{}

// ReadFile implements FS.
func (OSFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// WriteFileAtomic implements FS.
func (OSFS) WriteFileAtomic(name string, data []byte, perm os.FileMode) error {
	return fsutil.WriteFileAtomic(name, data, perm)
}

// MkdirAll implements FS.
func (OSFS) MkdirAll(name string, perm os.FileMode) error { return os.MkdirAll(name, perm) }

// Remove implements FS.
func (OSFS) Remove(name string) error { return os.Remove(name) }

// RemoveAll implements FS.
func (OSFS) RemoveAll(name string) error { return fsutil.RemoveAllPath(name) }

// Stat implements FS.
func (OSFS) Stat(name string) (os.FileInfo, error) { return os.Stat(name) }

func dirOf(path string) string { return filepath.Dir(path) }
