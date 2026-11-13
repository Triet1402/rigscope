package activation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Triet1402/rigscope/internal/platform/fsutil"
)

// DatabaseBackup copies the hub database to dst. The default implementation is
// a file copy, which is consistent because the hub is stopped (quiesced) before
// the backup starts.
//
// A future hub can provide a SQLite online backup / VACUUM INTO implementation
// behind this interface. modernc.org/sqlite is deliberately not a dependency of
// this build, so the WAL set is copied and restored as files and tested with
// file-level WAL/SHM fixtures.
type DatabaseBackup interface {
	Backup(ctx context.Context, src, dst string) error
}

// DatabaseSetBackup is an optional richer interface for a backend that can copy
// a consistent SQLite set — the database and its -wal and -shm companions —
// into dstDir while the hub is quiesced. When it is implemented, FileBackup
// calls it and expects files named after the source base name.
type DatabaseSetBackup interface {
	BackupSet(ctx context.Context, src, dstDir string) error
}

// FileCopyDatabase copies a stopped SQLite database file and its WAL set.
type FileCopyDatabase struct{}

// Backup implements DatabaseBackup.
func (FileCopyDatabase) Backup(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return copyFile(ctx, src, dst, 0, -1, -1)
}

// BackupSet implements DatabaseSetBackup. Every existing file is copied and
// fsynced; a missing sidecar is not created.
func (FileCopyDatabase) BackupSet(ctx context.Context, src, dstDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureDir(dstDir, 0o700); err != nil {
		return err
	}
	base := filepath.Base(src)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		source := src + suffix
		info, err := os.Stat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", source)
		}
		if err := copyFile(ctx, source, filepath.Join(dstDir, base+suffix), info.Mode().Perm(), -1, -1); err != nil {
			return err
		}
	}
	return syncDir(dstDir)
}

// BackupRequest names the one consistent set an upgrade backs up.
type BackupRequest struct {
	Dir          string
	From         string
	To           string
	HubDBFile    string
	ConfigFile   string
	CollectorDir string
}

// BackupManager creates and restores a backup set.
type BackupManager interface {
	Create(ctx context.Context, req BackupRequest) (BackupResult, error)
	Restore(ctx context.Context, dir string) error
}

// BackupResult reports where the set was written.
type BackupResult struct {
	Dir string
}

// backupItem is one manifest entry. Source is the original absolute path;
// Relative is the path inside the backup directory.
type backupItem struct {
	Source   string `json:"source"`
	Relative string `json:"relative"`
	Kind     string `json:"kind"`
	Existed  bool   `json:"existed"`
	Mode     uint32 `json:"mode"`
	UID      int    `json:"uid"`
	GID      int    `json:"gid"`
}

type backupManifest struct {
	Format    int          `json:"format"`
	CreatedAt time.Time    `json:"createdAt"`
	From      string       `json:"fromVersion"`
	To        string       `json:"toVersion"`
	Items     []backupItem `json:"items"`
}

const (
	backupManifestName = "manifest.json"
	backupFormat       = 2
)

// FileBackup is the default BackupManager. It copies files and directories and
// preserves modes and (when running as root) ownership.
type FileBackup struct {
	DB DatabaseBackup
	FS FS
}

// NewFileBackup returns a FileBackup. A nil database backup selects the
// copy-while-stopped implementation; a nil FS selects the real filesystem.
func NewFileBackup(db DatabaseBackup) *FileBackup {
	if db == nil {
		db = FileCopyDatabase{}
	}
	return &FileBackup{DB: db, FS: OSFS{}}
}

// Create writes the database set, configuration and collector identity into dir
// and records a manifest. Every copied file and created directory is fsynced
// before the manifest is written; the manifest is written last, so a partial
// directory is recognisable and safe to discard.
func (b *FileBackup) Create(ctx context.Context, req BackupRequest) (BackupResult, error) {
	if req.Dir == "" {
		return BackupResult{}, errors.New("backup directory is empty")
	}
	fs := b.fs()
	if err := ensureDir(req.Dir, 0o700); err != nil {
		return BackupResult{}, fmt.Errorf("create backup directory: %w", err)
	}
	if req.CollectorDir != "" && pathWithin(req.Dir, req.CollectorDir) {
		return BackupResult{}, fmt.Errorf("backup directory %s must not be inside the collector identity directory %s", req.Dir, req.CollectorDir)
	}
	manifest := backupManifest{
		Format:    backupFormat,
		CreatedAt: time.Now().UTC(),
		From:      req.From,
		To:        req.To,
	}
	fail := func(err error) (BackupResult, error) {
		_ = fsutil.RemoveAllPath(req.Dir)
		return BackupResult{}, err
	}

	if req.HubDBFile != "" {
		if err := b.backupDatabase(ctx, req.HubDBFile, req.Dir, &manifest); err != nil {
			return fail(err)
		}
	}

	if req.ConfigFile != "" {
		info, err := os.Stat(req.ConfigFile)
		switch {
		case err == nil && info.Mode().IsRegular():
			item := backupItem{
				Source:   req.ConfigFile,
				Relative: "config.json",
				Kind:     "file",
				Existed:  true,
				Mode:     uint32(info.Mode().Perm()),
			}
			item.UID, item.GID = ownership(info)
			if err := copyFile(ctx, req.ConfigFile, filepath.Join(req.Dir, item.Relative), info.Mode().Perm(), item.UID, item.GID); err != nil {
				return fail(fmt.Errorf("back up configuration: %w", err))
			}
			if err := syncDir(req.Dir); err != nil {
				return fail(fmt.Errorf("sync configuration backup: %w", err))
			}
			manifest.Items = append(manifest.Items, item)
		case errors.Is(err, os.ErrNotExist):
			manifest.Items = append(manifest.Items, backupItem{Source: req.ConfigFile, Kind: "file"})
		case err != nil:
			return fail(fmt.Errorf("inspect configuration: %w", err))
		}
	}

	if req.CollectorDir != "" {
		info, err := os.Stat(req.CollectorDir)
		switch {
		case err == nil && info.IsDir():
			item := backupItem{
				Source:   req.CollectorDir,
				Relative: "collector",
				Kind:     "dir",
				Existed:  true,
				Mode:     uint32(info.Mode().Perm()),
			}
			item.UID, item.GID = ownership(info)
			if err := copyTree(ctx, req.CollectorDir, filepath.Join(req.Dir, item.Relative)); err != nil {
				return fail(fmt.Errorf("back up collector identity: %w", err))
			}
			manifest.Items = append(manifest.Items, item)
		case errors.Is(err, os.ErrNotExist):
			manifest.Items = append(manifest.Items, backupItem{Source: req.CollectorDir, Kind: "dir"})
		case err != nil:
			return fail(fmt.Errorf("inspect collector identity: %w", err))
		}
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fail(fmt.Errorf("encode backup manifest: %w", err))
	}
	data = append(data, '\n')
	if err := fs.WriteFileAtomic(filepath.Join(req.Dir, backupManifestName), data, 0o600); err != nil {
		return fail(fmt.Errorf("write backup manifest: %w", err))
	}
	if err := syncDir(req.Dir); err != nil {
		return fail(fmt.Errorf("sync backup directory: %w", err))
	}
	if err := syncDir(filepath.Dir(req.Dir)); err != nil {
		return fail(fmt.Errorf("sync backup parent directory: %w", err))
	}
	return BackupResult{Dir: req.Dir}, nil
}

// backupDatabase copies hub.db, hub.db-wal and hub.db-shm as one set and records
// one manifest item per file, including the ones that did not exist. A custom
// DatabaseSetBackup receives the whole set at once.
func (b *FileBackup) backupDatabase(ctx context.Context, src, dir string, manifest *backupManifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	base := filepath.Base(src)
	mainInfo, err := os.Stat(src)
	mainExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect hub database: %w", err)
	}
	if mainExists && !mainInfo.Mode().IsRegular() {
		return fmt.Errorf("hub database %s is not a regular file", src)
	}
	if !mainExists {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			manifest.Items = append(manifest.Items, backupItem{Source: src + suffix, Relative: base + suffix, Kind: "file"})
		}
		return nil
	}

	if setBackup, ok := b.DB.(DatabaseSetBackup); ok {
		if mainExists {
			if err := setBackup.BackupSet(ctx, src, dir); err != nil {
				return fmt.Errorf("back up hub database set: %w", err)
			}
			if err := syncDir(dir); err != nil {
				return fmt.Errorf("sync hub database backup: %w", err)
			}
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			source := src + suffix
			info, statErr := os.Stat(source)
			existed := statErr == nil
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return fmt.Errorf("inspect hub database companion %s: %w", source, statErr)
			}
			if existed && !info.Mode().IsRegular() {
				return fmt.Errorf("hub database companion %s is not a regular file", source)
			}
			item := backupItem{Source: source, Relative: base + suffix, Kind: "file", Existed: existed}
			if existed {
				item.Mode = uint32(info.Mode().Perm())
				item.UID, item.GID = ownership(info)
				if _, err := os.Stat(filepath.Join(dir, item.Relative)); err != nil {
					return fmt.Errorf("database set backup is missing %s: %w", item.Relative, err)
				}
			}
			manifest.Items = append(manifest.Items, item)
		}
		return nil
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		source := src + suffix
		info, statErr := os.Stat(source)
		existed := statErr == nil
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect hub database companion %s: %w", source, statErr)
		}
		item := backupItem{Source: source, Relative: base + suffix, Kind: "file", Existed: existed}
		if existed {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("hub database companion %s is not a regular file", source)
			}
			item.Mode = uint32(info.Mode().Perm())
			item.UID, item.GID = ownership(info)
			destination := filepath.Join(dir, item.Relative)
			if suffix == "" {
				if err := b.DB.Backup(ctx, source, destination); err != nil {
					return fmt.Errorf("back up hub database: %w", err)
				}
			} else if err := copyFile(ctx, source, destination, info.Mode().Perm(), item.UID, item.GID); err != nil {
				return fmt.Errorf("back up hub database companion %s: %w", suffix, err)
			}
		}
		manifest.Items = append(manifest.Items, item)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync hub database backup: %w", err)
	}
	return nil
}

// Restore puts every manifest item back in place. It attempts every item and
// reports every error instead of stopping at the first one.
func (b *FileBackup) Restore(ctx context.Context, dir string) error {
	data, err := b.fs().ReadFile(filepath.Join(dir, backupManifestName))
	if err != nil {
		return fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest backupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse backup manifest: %w", err)
	}
	if manifest.Format != backupFormat {
		return fmt.Errorf("backup manifest has unsupported format %d", manifest.Format)
	}
	var errs []error
	for _, item := range manifest.Items {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := restoreItem(ctx, dir, item); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", item.Source, err))
		}
	}
	return errors.Join(errs...)
}

func restoreItem(ctx context.Context, dir string, item backupItem) error {
	if !item.Existed {
		if item.Source == "" {
			return errors.New("backup item has no source path")
		}
		if err := fsutil.RemoveAllPath(item.Source); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(item.Source)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	relative, err := safeRelative(item.Relative)
	if err != nil {
		return err
	}
	source := filepath.Join(dir, relative)
	if item.Source != "" && pathWithin(dir, item.Source) {
		return fmt.Errorf("backup directory %s is inside the restore destination %s", dir, item.Source)
	}
	switch item.Kind {
	case "file":
		mode := os.FileMode(item.Mode).Perm()
		return replaceFile(ctx, source, item.Source, mode, item.UID, item.GID)
	case "dir":
		if err := fsutil.RemoveAllPath(item.Source); err != nil {
			return err
		}
		if err := copyTree(ctx, source, item.Source); err != nil {
			return err
		}
		return syncDir(filepath.Dir(item.Source))
	default:
		return fmt.Errorf("unknown backup item kind %q", item.Kind)
	}
}

// pathWithin reports whether child is parent or below it.
func pathWithin(child, parent string) bool {
	childAbs, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	parentAbs, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(parentAbs, childAbs)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func safeRelative(relative string) (string, error) {
	if relative == "" {
		return "", errors.New("empty backup item path")
	}
	clean := filepath.Clean(relative)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("backup item %q escapes the backup directory", relative)
	}
	return clean, nil
}

func (b *FileBackup) fs() FS {
	if b != nil && b.FS != nil {
		return b.FS
	}
	return OSFS{}
}

// replaceFile copies src to dst atomically through a temporary file in dst's
// directory.
func replaceFile(ctx context.Context, src, dst string, mode os.FileMode, uid, gid int) error {
	if err := ensureDir(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".restore-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Close(); err != nil {
		return err
	}
	if err := copyFile(ctx, src, name, mode, uid, gid); err != nil {
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func copyTree(ctx context.Context, src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	if err := ensureDir(dst, info.Mode().Perm()); err != nil {
		return err
	}
	if err := chmodOwn(dst, info); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		entryInfo, err := os.Lstat(srcPath)
		if err != nil {
			return err
		}
		switch {
		case entryInfo.IsDir():
			if err := copyTree(ctx, srcPath, dstPath); err != nil {
				return err
			}
		case entryInfo.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("refusing to back up symlink %s", srcPath)
		case entryInfo.Mode().IsRegular():
			uid, gid := ownership(entryInfo)
			if err := copyFile(ctx, srcPath, dstPath, entryInfo.Mode().Perm(), uid, gid); err != nil {
				return err
			}
		default:
			return fmt.Errorf("refusing to back up special file %s", srcPath)
		}
	}
	if err := syncDir(dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func copyFile(ctx context.Context, src, dst string, mode os.FileMode, uid, gid int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if mode == 0 {
		info, err := in.Stat()
		if err != nil {
			return err
		}
		mode = info.Mode().Perm()
		if uid < 0 || gid < 0 {
			uid, gid = ownership(info)
		}
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chmod(mode); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	if os.Geteuid() == 0 && uid >= 0 && gid >= 0 {
		if err := os.Chown(dst, uid, gid); err != nil {
			return err
		}
	}
	if err := syncFile(dst); err != nil {
		return err
	}
	return nil
}

func chmodOwn(path string, info os.FileInfo) error {
	if err := os.Chmod(path, info.Mode().Perm()); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		uid, gid := ownership(info)
		if uid >= 0 && gid >= 0 {
			if err := os.Chown(path, uid, gid); err != nil {
				return err
			}
		}
	}
	return nil
}

func ownership(info os.FileInfo) (int, int) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid), int(stat.Gid)
	}
	return -1, -1
}

// ensureDir creates path and missing parents, fsyncing each created directory
// and its parent up to an existing ancestor.
func ensureDir(path string, perm os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("directory path is empty")
	}
	var missing []string
	current := path
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", current)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("no existing ancestor for %s", path)
		}
		current = parent
	}
	for index := len(missing) - 1; index >= 0; index-- {
		dir := missing[index]
		if err := os.Mkdir(dir, perm); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := syncDir(dir); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	return nil
}

func syncFile(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
