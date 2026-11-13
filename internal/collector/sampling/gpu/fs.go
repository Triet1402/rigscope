package gpu

import (
	"io/fs"
	"os"
	"sort"
	"strings"
)

// SysFS is the read-only view of sysfs/procfs the GPU samplers need. Paths
// may be absolute, as they are written in sysfs documentation; an
// implementation rooted somewhere else (a fixture tree) resolves them
// relative to its own root.
type SysFS interface {
	// ReadText returns the file contents untrimmed, and false when the file
	// cannot be read.
	ReadText(name string) (string, bool)
	// ReadDir returns entry names sorted for deterministic ordering.
	ReadDir(name string) ([]string, error)
}

// TextReadFunc reads one attribute. It is the narrow dependency the
// runtime-PM gate needs; [RootFS.ReadText] has this shape.
type TextReadFunc func(name string) (string, bool)

// RootFS adapts an [fs.FS] rooted at "/" (or at a fixture directory) to
// [SysFS]. Use [HostFS] for the real machine.
type RootFS struct {
	FS fs.FS
}

// HostFS is the real machine's sysfs/procfs.
func HostFS() RootFS { return RootFS{FS: os.DirFS("/")} }

// ReadText implements [SysFS].
func (r RootFS) ReadText(name string) (string, bool) {
	if r.FS == nil {
		return "", false
	}
	data, err := fs.ReadFile(r.FS, cleanPath(name))
	if err != nil {
		return "", false
	}
	return string(data), true
}

// ReadDir implements [SysFS]. Names are sorted so discovery is stable across
// kernels that return directory entries in hash order.
func (r RootFS) ReadDir(name string) ([]string, error) {
	if r.FS == nil {
		return nil, fs.ErrNotExist
	}
	entries, err := fs.ReadDir(r.FS, cleanPath(name))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

// cleanPath makes an absolute sysfs path relative to an [fs.FS] root.
func cleanPath(name string) string {
	cleaned := strings.TrimPrefix(name, "/")
	if cleaned == "" {
		return "."
	}
	return cleaned
}

// readTextTrimmed returns the trimmed attribute text, or false when the
// attribute is missing or unreadable.
func readTextTrimmed(sysfs SysFS, name string) (string, bool) {
	raw, ok := sysfs.ReadText(name)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(raw), true
}
