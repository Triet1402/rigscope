package procfs

import (
	"io/fs"
	"os"
	"strings"
)

// Source is the file-system view the samplers read. Paths are absolute, as the
// kernel exposes them ("/proc/stat"), so the same parser works against a live
// host and against a fixture tree rooted at "/".
type Source interface {
	ReadFile(path string) ([]byte, error)
	ReadDir(path string) ([]string, error)
}

// rootSource reads the live host through the os package.
type rootSource struct{}

// NewRootSource returns a Source backed by the real root file system.
func NewRootSource() Source { return rootSource{} }

func (rootSource) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (rootSource) ReadDir(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

// fsSource adapts an fs.FS rooted where "/" would be to absolute paths.
type fsSource struct {
	fsys fs.FS
}

// NewFSSource returns a Source over an fs.FS whose root corresponds to "/".
// Tests pass os.DirFS("../../../../testdata/proc/<host>") or fstest.MapFS.
func NewFSSource(fsys fs.FS) Source { return fsSource{fsys: fsys} }

func (s fsSource) ReadFile(path string) ([]byte, error) {
	return fs.ReadFile(s.fsys, fsPath(path))
}

func (s fsSource) ReadDir(path string) ([]string, error) {
	entries, err := fs.ReadDir(s.fsys, fsPath(path))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

// fsPath maps an absolute kernel path into an fs.FS path.
func fsPath(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "."
	}
	return trimmed
}

// StatFSSnapshot is the subset of statfs(2) the filesystem readings use.
// Files/Ffree are zero when the platform or filesystem does not report an
// inode table.
type StatFSSnapshot struct {
	Bsize  int64
	Blocks int64
	Bfree  int64
	Files  int64
	Ffree  int64
}

// StatFSFunc reads capacity for one mount point. It is injectable because
// statfs(2) is a syscall and fixture trees are not mounted.
type StatFSFunc func(path string) (StatFSSnapshot, error)

// InterfaceAddressSource enumerates the addresses the kernel reports per
// interface. Prefixes are already resolved to CIDR lengths.
type InterfaceAddressSource func() (map[string][]InterfaceAddress, error)

// readText reads a file as text, returning errors unchanged.
func readText(src Source, path string) (string, error) {
	data, err := src.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// readTextOrZero reads a file, returning "" when it is missing or unreadable.
// Used only for optional sensors and text sources.
func readTextOrZero(src Source, path string) string {
	text, err := readText(src, path)
	if err != nil {
		return ""
	}
	return text
}

// readOptional reads a file and reports whether it was readable. An empty but
// present file is distinct from a missing one (sysfs uses that distinction).
func readOptional(src Source, path string) (string, bool) {
	text, err := readText(src, path)
	if err != nil {
		return "", false
	}
	return text, true
}
