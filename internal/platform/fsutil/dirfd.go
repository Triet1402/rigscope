//go:build linux

package fsutil

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

// oPath is the Linux O_PATH flag. The syscall package does not export it, but
// the value is part of the kernel ABI on every architecture.
const oPath = 0x200000

// atRemovedir makes unlinkat remove a directory entry instead of a file.
const atRemovedir = 0x200

// maxRemovalDepth bounds how deeply RemoveAll descends. One descriptor is held
// per level, so an unbounded tree could exhaust the process descriptor table.
// The depth is measured before anything is deleted; a deeper tree is refused
// intact rather than half-removed.
const maxRemovalDepth = 128

// mountInfoPath is the kernel record of every mount in this namespace. Tests
// parse MountPointsFrom directly and exercise the real kernel file inside a
// disposable mount namespace.
const mountInfoPath = "/proc/self/mountinfo"

// Identity is the filesystem identity of a file or directory. Uninstall
// records it at install time and refuses to delete a path whose identity has
// changed, so a file swapped in behind the manifest cannot be removed.
type Identity struct {
	Dev  uint64
	Ino  uint64
	UID  uint32
	GID  uint32
	Mode uint32
}

// SameFile reports whether two identities name the same inode.
func (i Identity) SameFile(other Identity) bool {
	return i.Dev == other.Dev && i.Ino == other.Ino
}

// SameOwner reports whether two identities have the same owner.
func (i Identity) SameOwner(other Identity) bool {
	return i.UID == other.UID && i.GID == other.GID
}

// IdentityOfPath returns the identity of path without following a final
// symlink.
func IdentityOfPath(path string) (Identity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Identity{}, err
	}
	identity, ok := IdentityOf(info)
	if !ok {
		return Identity{}, fmt.Errorf("%s has no ownership information", path)
	}
	return identity, nil
}

// IdentityOf extracts an Identity from an os.FileInfo.
func IdentityOf(info os.FileInfo) (Identity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, false
	}
	return identityFromStat(stat), true
}

func identityFromStat(stat *syscall.Stat_t) Identity {
	return Identity{
		Dev:  uint64(stat.Dev),
		Ino:  uint64(stat.Ino),
		UID:  uint32(stat.Uid),
		GID:  uint32(stat.Gid),
		Mode: uint32(stat.Mode),
	}
}

// SHA256File hashes a file. It is used to record the content of files install
// created so uninstall can notice in-place edits.
func SHA256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// SHA256Bytes hashes an in-memory file.
func SHA256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Dir is a directory opened component by component with O_DIRECTORY|O_NOFOLLOW
// from a trusted root. Every operation is relative to its descriptor, so a
// pathname swapped for a symlink after the directory was opened cannot
// redirect a deletion outside the tree.
type Dir struct {
	file *os.File
	path string
}

// OpenDirNoFollow opens path without following any symbolic link in any
// component. A symlinked parent is refused instead of silently traversed.
func OpenDirNoFollow(path string) (*Dir, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("%s is not an absolute path", path)
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	for _, part := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			syscall.Close(fd)
			return nil, fmt.Errorf("%s contains a parent component", path)
		}
		next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if err != nil {
			if errors.Is(err, syscall.ELOOP) {
				return nil, fmt.Errorf("refusing to follow symbolic link in %s", path)
			}
			return nil, fmt.Errorf("open directory %s: %w", path, err)
		}
		fd = next
	}
	file := os.NewFile(uintptr(fd), clean)
	if file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("open directory %s: invalid descriptor", path)
	}
	return &Dir{file: file, path: clean}, nil
}

// Path returns the path the directory was opened from.
func (d *Dir) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// Close releases the directory descriptor.
func (d *Dir) Close() error {
	if d == nil || d.file == nil {
		return nil
	}
	file := d.file
	d.file = nil
	return file.Close()
}

// Identity returns the identity of the open directory.
func (d *Dir) Identity() (Identity, error) {
	if d == nil || d.file == nil {
		return Identity{}, errors.New("directory is closed")
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(d.file.Fd()), &stat); err != nil {
		return Identity{}, fmt.Errorf("stat %s: %w", d.path, err)
	}
	return identityFromStat(&stat), nil
}

// Stat returns the identity of name relative to the directory, without
// following a symlink.
func (d *Dir) Stat(name string) (Identity, error) {
	if err := validName(name); err != nil {
		return Identity{}, err
	}
	fd, err := syscall.Openat(int(d.file.Fd()), name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return Identity{}, &os.PathError{Op: "stat", Path: filepath.Join(d.path, name), Err: err}
	}
	defer syscall.Close(fd)
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return Identity{}, &os.PathError{Op: "fstat", Path: filepath.Join(d.path, name), Err: err}
	}
	return identityFromStat(&stat), nil
}

// ReadFile reads a regular file relative to the directory, refusing a symlink
// in the final component.
func (d *Dir) ReadFile(name string) ([]byte, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	fd, err := syscall.Openat(int(d.file.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: filepath.Join(d.path, name), Err: err}
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
	if file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("open %s: invalid descriptor", name)
	}
	defer file.Close()
	return io.ReadAll(file)
}

// ReadPrefix reads at most limit bytes from a regular file relative to the
// directory, refusing a symlink in the final component.
func (d *Dir) ReadPrefix(name string, limit int) ([]byte, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if limit < 0 {
		limit = 0
	}
	fd, err := syscall.Openat(int(d.file.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: filepath.Join(d.path, name), Err: err}
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
	if file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("open %s: invalid descriptor", name)
	}
	defer file.Close()
	buffer := make([]byte, limit)
	n, err := io.ReadFull(file, buffer)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buffer[:n], nil
}

// ReadRegular reads at most maxBytes from a regular file relative to the
// directory. The file is opened once with O_NOFOLLOW; the type, size and
// (optionally) privacy mode are checked on the same descriptor that is then
// read, so a pathname swap cannot redirect the validation or the read.
func (d *Dir) ReadRegular(name string, maxBytes int64, private bool) ([]byte, Identity, error) {
	if err := validName(name); err != nil {
		return nil, Identity{}, err
	}
	if maxBytes < 0 {
		return nil, Identity{}, fmt.Errorf("read limit for %s is negative", name)
	}
	fd, err := syscall.Openat(int(d.file.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, Identity{}, &os.PathError{Op: "open", Path: filepath.Join(d.path, name), Err: err}
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
	if file == nil {
		syscall.Close(fd)
		return nil, Identity{}, fmt.Errorf("open %s: invalid descriptor", name)
	}
	defer file.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return nil, Identity{}, &os.PathError{Op: "fstat", Path: filepath.Join(d.path, name), Err: err}
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil, Identity{}, fmt.Errorf("%s must be a regular file", filepath.Join(d.path, name))
	}
	if private && stat.Mode&0o077 != 0 {
		return nil, Identity{}, fmt.Errorf("%s must not be group or world accessible", filepath.Join(d.path, name))
	}
	if stat.Size > maxBytes {
		return nil, Identity{}, fmt.Errorf("%s is larger than %d bytes", filepath.Join(d.path, name), maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, Identity{}, err
	}
	if int64(len(data)) > maxBytes {
		return nil, Identity{}, fmt.Errorf("%s is larger than %d bytes", filepath.Join(d.path, name), maxBytes)
	}
	return data, identityFromStat(&stat), nil
}

// Names lists the entries in the directory. It uses the descriptor's own
// readdir, so it does not re-resolve the path.
func (d *Dir) Names() ([]string, error) {
	if d == nil || d.file == nil {
		return nil, errors.New("directory is closed")
	}
	// A directory descriptor keeps its read position. Rewind so a second
	// listing (the depth pre-pass followed by the deletion pass) sees every
	// entry instead of the exhausted tail.
	if _, err := d.file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind directory %s: %w", d.path, err)
	}
	entries, err := d.file.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read directory %s: %w", d.path, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

// OpenChild opens one directory entry as a directory, refusing a symlink.
func (d *Dir) OpenChild(name string) (*Dir, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	fd, err := syscall.Openat(int(d.file.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: filepath.Join(d.path, name), Err: err}
	}
	file := os.NewFile(uintptr(fd), filepath.Join(d.path, name))
	if file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("open directory %s: invalid descriptor", name)
	}
	return &Dir{file: file, path: filepath.Join(d.path, name)}, nil
}

// Remove unlinks one entry relative to the directory. It removes symlinks
// themselves, never their targets.
func (d *Dir) Remove(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := syscall.Unlinkat(int(d.file.Fd()), name); err != nil {
		return &os.PathError{Op: "unlinkat", Path: filepath.Join(d.path, name), Err: err}
	}
	return nil
}

// RemoveDir removes an empty child directory. It fails with ENOTEMPTY when
// anything is left inside, so callers never recurse by accident.
func (d *Dir) RemoveDir(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	return d.removeDirectory(name)
}

// RemoveAll removes name and, when it is a directory, everything below it.
// Directories are opened with O_NOFOLLOW, so a symlink is unlinked rather than
// followed and a foreign tree behind it is never touched.
//
// Before anything is deleted it refuses a tree that contains a mount point at
// or below it (including a same-device bind mount, which st_dev cannot detect,
// and the removal root itself) and a tree deeper than maxRemovalDepth. During
// traversal every directory must stay on the root device, so a mount attached
// after the pre-pass is not crossed.
func (d *Dir) RemoveAll(name string) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := refuseMountsUnder(filepath.Join(d.path, name)); err != nil {
		return err
	}
	child, err := d.OpenChild(name)
	if err == nil {
		defer child.Close()
		depth, err := child.measureDepth(1)
		if err != nil {
			return err
		}
		if depth > maxRemovalDepth {
			return fmt.Errorf("refusing to remove %s: the directory tree is deeper than %d levels, which would exhaust open descriptors", filepath.Join(d.path, name), maxRemovalDepth)
		}
		rootDevice, err := child.device()
		if err != nil {
			return err
		}
		if err := child.removeContents(1, rootDevice); err != nil {
			return err
		}
		if err := child.Close(); err != nil {
			return err
		}
		return d.removeDirectory(name)
	}
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EINVAL) {
		if removeErr := d.Remove(name); removeErr != nil && !errors.Is(removeErr, syscall.ENOENT) {
			return removeErr
		}
		return nil
	}
	return err
}

// measureDepth returns the number of directory levels below d (d itself counts
// as depth). It stops at maxRemovalDepth+1 so the measurement itself is
// bounded. Nothing is modified.
func (d *Dir) measureDepth(depth int) (int, error) {
	if depth > maxRemovalDepth {
		return depth, nil
	}
	names, err := d.Names()
	if err != nil {
		return 0, err
	}
	deepest := depth
	for _, entry := range names {
		if entry == "." || entry == ".." {
			continue
		}
		child, err := d.OpenChild(entry)
		if err != nil {
			// A symlink, a regular file, or an entry that vanished between
			// readdir and open: not a directory to descend into.
			continue
		}
		sub, err := child.measureDepth(depth + 1)
		closeErr := child.Close()
		if err != nil {
			return 0, err
		}
		if closeErr != nil {
			return 0, closeErr
		}
		if sub > deepest {
			deepest = sub
		}
	}
	return deepest, nil
}

// removeContents deletes every entry below d. rootDevice is the device of the
// tree root; a child directory on another device is refused before it is
// opened, so a mount cannot be crossed in either direction.
func (d *Dir) removeContents(depth int, rootDevice uint64) error {
	if depth > maxRemovalDepth {
		return fmt.Errorf("refusing to remove %s: the directory tree exceeds %d levels", d.path, maxRemovalDepth)
	}
	names, err := d.Names()
	if err != nil {
		return err
	}
	for _, entry := range names {
		if entry == "." || entry == ".." {
			continue
		}
		identity, err := d.Stat(entry)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		if identity.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			if err := d.Remove(entry); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		child, err := d.OpenChild(entry)
		if err != nil {
			if errors.Is(err, syscall.ENOENT) {
				continue
			}
			if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EINVAL) {
				// The entry changed type after Stat. Unlink the new entry; it is
				// not a directory this traversal can follow.
				if removeErr := d.Remove(entry); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return removeErr
				}
				continue
			}
			return err
		}
		childIdentity, err := child.Identity()
		if err != nil {
			_ = child.Close()
			return err
		}
		if !childIdentity.SameFile(identity) {
			_ = child.Close()
			return fmt.Errorf("refusing to remove %s: it changed while the tree was being deleted", filepath.Join(d.path, entry))
		}
		if childIdentity.Dev != rootDevice {
			_ = child.Close()
			return fmt.Errorf("refusing to remove %s: it is a mount boundary inside the tree", filepath.Join(d.path, entry))
		}
		if err := child.removeContents(depth+1, rootDevice); err != nil {
			_ = child.Close()
			return err
		}
		if err := child.Close(); err != nil {
			return err
		}
		if err := d.removeDirectory(entry); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// device returns the device of the open directory.
func (d *Dir) device() (uint64, error) {
	identity, err := d.Identity()
	if err != nil {
		return 0, err
	}
	return identity.Dev, nil
}

// refuseMountsUnder refuses a tree that contains any mount point, including
// the removal root itself. Removing a mount point's contents would delete a
// foreign filesystem that merely appears below the owned directory; st_dev
// alone misses same-device bind mounts, which mountinfo records. A root that
// is itself a mount is refused too: deleting inside it would empty the mounted
// filesystem and then fail at rmdir, leaving a half-removed tree.
func refuseMountsUnder(path string) error {
	mounts, err := MountPointsUnder(path)
	if err != nil {
		return err
	}
	if len(mounts) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to remove %s: it is or contains mount point(s) %s", path, strings.Join(mounts, ", "))
}

// MountPointsUnder returns the mount points at or below path, including path
// itself when it is a mount point. The path is cleaned and compared
// component-wise, never by string prefix.
func MountPointsUnder(path string) ([]string, error) {
	file, err := os.Open(mountInfoPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	defer file.Close()
	mounts, err := MountPointsFrom(file)
	if err != nil {
		return nil, err
	}
	target := filepath.Clean(path)
	var inside []string
	for _, mount := range mounts {
		if mount == target || isPathBelow(mount, target) {
			inside = append(inside, mount)
		}
	}
	sort.Strings(inside)
	return inside, nil
}

// MountPointsFrom parses /proc/self/mountinfo content. The mount point is the
// fifth field. Fields are separated by ASCII spaces only: the kernel escapes a
// literal space, tab, newline and backslash in a path as three octal digits,
// but it leaves other Unicode whitespace (U+00A0, U+2003, ...) untouched.
// strings.Fields would treat those characters as separators and shift the
// mount point, defeating mount detection for an ancestor with such a name.
//
// A mountinfo line is
//
//	ID parent major:minor root mount-point mount-options [optional ...] - fstype source super-options
//
// so the mount point is field 4 and the separator must appear after at least
// the mount-options field. Optional fields before "-" do not change the index.
func MountPointsFrom(reader io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var mounts []string
	for scanner.Scan() {
		fields := splitMountInfoFields(scanner.Text())
		// Six mandatory fields plus the "-" separator.
		if len(fields) < 7 || !hasMountInfoSeparator(fields) {
			continue
		}
		mount, err := unescapeMountPath(fields[4])
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	return mounts, nil
}

// splitMountInfoFields splits on ASCII spaces only, discarding empty tokens
// from repeated spaces. It deliberately does not use strings.Fields.
func splitMountInfoFields(line string) []string {
	raw := strings.Split(line, " ")
	fields := raw[:0]
	for _, field := range raw {
		if field != "" {
			fields = append(fields, field)
		}
	}
	return fields
}

// hasMountInfoSeparator reports whether the mandatory " - " separator appears
// after the mount-options field (index 5).
func hasMountInfoSeparator(fields []string) bool {
	for index := 6; index < len(fields); index++ {
		if fields[index] == "-" {
			return true
		}
	}
	return false
}

// unescapeMountPath decodes the octal escapes the kernel writes in mountinfo.
func unescapeMountPath(value string) (string, error) {
	if !strings.Contains(value, `\`) {
		return value, nil
	}
	var out strings.Builder
	out.Grow(len(value))
	for index := 0; index < len(value); index++ {
		ch := value[index]
		if ch != '\\' {
			out.WriteByte(ch)
			continue
		}
		if index+3 >= len(value) {
			return "", fmt.Errorf("mount table path %q has an incomplete escape", value)
		}
		var decoded byte
		for offset := 1; offset <= 3; offset++ {
			digit := value[index+offset]
			if digit < '0' || digit > '7' {
				return "", fmt.Errorf("mount table path %q has an invalid escape", value)
			}
			decoded = decoded<<3 + (digit - '0')
		}
		out.WriteByte(decoded)
		index += 3
	}
	return out.String(), nil
}

// isPathBelow reports whether child is a strict descendant of parent.
func isPathBelow(child, parent string) bool {
	if parent == string(filepath.Separator) {
		return child != parent
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

func (d *Dir) removeDirectory(name string) error {
	pointer, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_UNLINKAT, d.file.Fd(), uintptr(unsafe.Pointer(pointer)), atRemovedir)
	if errno != 0 {
		return &os.PathError{Op: "unlinkat", Path: filepath.Join(d.path, name), Err: errno}
	}
	return nil
}

// RemoveAllPath removes path and everything below it without following any
// symlink component. A missing path is not an error.
func RemoveAllPath(path string) error {
	parent, err := OpenDirNoFollow(filepath.Dir(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer parent.Close()
	return parent.RemoveAll(filepath.Base(path))
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("unsafe path component %q", name)
	}
	if strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("unsafe path component %q", name)
	}
	return nil
}
