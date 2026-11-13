package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// StagedFile is a fully written, fsynced temporary file waiting to be
// published at a target path. Its identity is recorded in the installation
// manifest before publication, so an interrupted install can prove that a
// file already at the target really is the file it was about to publish.
type StagedFile struct {
	Path   string
	SHA256 string
}

// StageFile writes data to a transaction-unique temporary file beside target
// and fsyncs it and its directory. It does not touch target. The caller owns
// the staged file and must publish or discard it.
func StageFile(target string, data []byte, perm os.FileMode, prepare func(*os.File) error) (StagedFile, error) {
	dir := filepath.Dir(target)
	if err := ensureDir(dir, 0o700); err != nil {
		return StagedFile{}, fmt.Errorf("create directory %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".stage-*")
	if err != nil {
		return StagedFile{}, fmt.Errorf("create staged file for %s: %w", target, err)
	}
	name := temp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = temp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := temp.Chmod(perm); err != nil {
		return StagedFile{}, fmt.Errorf("set mode on %s: %w", name, err)
	}
	if prepare != nil {
		if err := prepare(temp); err != nil {
			return StagedFile{}, err
		}
	}
	if _, err := temp.Write(data); err != nil {
		return StagedFile{}, fmt.Errorf("write %s: %w", name, err)
	}
	if err := temp.Sync(); err != nil {
		return StagedFile{}, fmt.Errorf("sync %s: %w", name, err)
	}
	if err := temp.Close(); err != nil {
		return StagedFile{}, fmt.Errorf("close %s: %w", name, err)
	}
	sum, err := HashFile(name)
	if err != nil {
		return StagedFile{}, fmt.Errorf("hash staged file %s: %w", name, err)
	}
	if err := syncDir(dir); err != nil {
		return StagedFile{}, fmt.Errorf("sync directory %s: %w", dir, err)
	}
	ok = true
	return StagedFile{Path: name, SHA256: sum}, nil
}

// PublishStagedFile verifies the staged file still matches its recorded digest
// and moves it to target. With replace=false publication uses link(2), so an
// existing target is never overwritten.
func PublishStagedFile(staged StagedFile, target string, replace bool) error {
	if staged.Path == "" || staged.SHA256 == "" {
		return errors.New("staged file has no path or digest")
	}
	info, err := os.Lstat(staged.Path)
	if err != nil {
		return fmt.Errorf("inspect staged file %s: %w", staged.Path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("staged file %s is not a regular file", staged.Path)
	}
	sum, err := HashFile(staged.Path)
	if err != nil {
		return fmt.Errorf("hash staged file %s: %w", staged.Path, err)
	}
	if sum != staged.SHA256 {
		return fmt.Errorf("staged file %s does not match its recorded SHA-256", staged.Path)
	}
	if replace {
		if err := os.Rename(staged.Path, target); err != nil {
			return fmt.Errorf("replace %s: %w", target, err)
		}
	} else {
		if err := os.Link(staged.Path, target); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists: %w", target, os.ErrExist)
			}
			return fmt.Errorf("create %s: %w", target, err)
		}
		if err := os.Remove(staged.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove staged file %s: %w", staged.Path, err)
		}
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return fmt.Errorf("sync directory %s: %w", filepath.Dir(target), err)
	}
	return nil
}

// DiscardStagedFile removes a staged file that will not be published. A
// missing file is not an error.
func DiscardStagedFile(staged StagedFile) error {
	if staged.Path == "" {
		return nil
	}
	if err := os.Remove(staged.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
