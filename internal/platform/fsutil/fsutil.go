package fsutil

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// WriteFileAtomic writes data to path through a temporary file in the same
// directory, fsyncs it and renames it into place. Missing parent directories
// are created with mode 0700. A reader either sees the old file or the new
// file, never a partial write. Every created directory, the renamed file and
// its parent are synced; a sync failure is returned.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := ensureDir(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	tmp, err := stageFile(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return syncDir(dir)
}

// ReplaceKeepingPrevious atomically replaces path with data and keeps the
// previous contents at path+".prev". The new file is fully written and fsynced
// before the current file is touched, so a crash leaves either the old or the
// new executable in place. The retained previous file keeps the executable
// permission bits of the file it replaces.
func ReplaceKeepingPrevious(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := ensureDir(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	tmp, err := stageFile(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := RetainPrevious(path); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return syncDir(dir)
}

// RetainPrevious durably copies or links path to path+".prev" and fsyncs the
// directory. A missing source is not an error: there is then no previous file
// to retain. The retained file is verified against the source digest.
func RetainPrevious(path string) (BinaryFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return BinaryFile{}, nil
		}
		return BinaryFile{}, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return BinaryFile{}, fmt.Errorf("%s is not a regular file", path)
	}
	sum, err := HashFile(path)
	if err != nil {
		return BinaryFile{}, fmt.Errorf("hash %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	previous := path + ".prev"
	previousTmp := previous + ".tmp"
	_ = os.Remove(previousTmp)
	if err := linkOrCopy(path, previousTmp, info.Mode().Perm()); err != nil {
		return BinaryFile{}, fmt.Errorf("keep previous %s: %w", path, err)
	}
	if err := os.Chmod(previousTmp, info.Mode().Perm()); err != nil {
		return BinaryFile{}, fmt.Errorf("preserve previous mode: %w", err)
	}
	retainedSum, err := HashFile(previousTmp)
	if err != nil {
		return BinaryFile{}, fmt.Errorf("hash retained previous: %w", err)
	}
	if retainedSum != sum {
		_ = os.Remove(previousTmp)
		return BinaryFile{}, fmt.Errorf("retained previous %s does not match %s", previousTmp, path)
	}
	if err := syncFile(previousTmp); err != nil {
		return BinaryFile{}, fmt.Errorf("sync previous %s: %w", path, err)
	}
	if err := os.Rename(previousTmp, previous); err != nil {
		return BinaryFile{}, fmt.Errorf("replace %s: %w", previous, err)
	}
	if err := syncDir(dir); err != nil {
		return BinaryFile{}, err
	}
	return BinaryFile{Path: previous, SHA256: retainedSum, Mode: info.Mode().Perm()}, nil
}

// BinaryFile identifies one executable by path and SHA-256 digest. Mode is the
// executable's permission bits when it was observed.
type BinaryFile struct {
	Path   string
	SHA256 string
	Mode   os.FileMode
}

// HashFile returns the lowercase hexadecimal SHA-256 digest of path.
func HashFile(path string) (string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// PrepareBinary copies staged to the transaction-unique
// "<target>.<token>.new" file beside target, fsyncs it and the containing
// directory, and returns its verified identity. It never touches target.
func PrepareBinary(staged, target, token string) (BinaryFile, error) {
	if err := validateToken(token); err != nil {
		return BinaryFile{}, err
	}
	stagedInfo, err := os.Stat(staged)
	if err != nil {
		return BinaryFile{}, fmt.Errorf("staged binary %s: %w", staged, err)
	}
	if !stagedInfo.Mode().IsRegular() {
		return BinaryFile{}, fmt.Errorf("staged binary %s is not a regular file", staged)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return BinaryFile{}, fmt.Errorf("installed binary %s: %w", target, err)
	}
	if !targetInfo.Mode().IsRegular() {
		return BinaryFile{}, fmt.Errorf("installed binary %s is not a regular file", target)
	}
	dir := filepath.Dir(target)
	if err := ensureDir(dir, 0o755); err != nil {
		return BinaryFile{}, err
	}
	prepared := tempName(target, token, "new")
	_ = os.Remove(prepared)
	if err := copyInto(staged, prepared, targetInfo.Mode().Perm()); err != nil {
		return BinaryFile{}, fmt.Errorf("stage new binary: %w", err)
	}
	sum, err := HashFile(prepared)
	if err != nil {
		_ = os.Remove(prepared)
		return BinaryFile{}, fmt.Errorf("hash prepared binary: %w", err)
	}
	if err := syncDir(dir); err != nil {
		_ = os.Remove(prepared)
		return BinaryFile{}, err
	}
	return BinaryFile{Path: prepared, SHA256: sum, Mode: targetInfo.Mode().Perm()}, nil
}

// PublishPrepared installs a prepared binary. It first durably retains the
// current target as previous (hard link or copy, fsync, hash, rename, directory
// fsync) and only then renames prepared over target. The returned identity is
// the retained previous binary. A crash at any point leaves a working target
// and at worst transaction temp files that RecoverPublish can finish.
//
// Callers that must journal the previous publication as its own step should
// call PublishPrevious and SwapPrepared separately; this function is their
// composition.
func PublishPrepared(prepared BinaryFile, target, previous, token string) (BinaryFile, error) {
	retained, err := PublishPrevious(prepared, target, previous, token)
	if err != nil {
		return BinaryFile{}, err
	}
	if err := SwapPrepared(prepared, target, token); err != nil {
		return BinaryFile{}, err
	}
	return retained, nil
}

// PublishPrevious durably retains the current target as previous. It verifies
// both the prepared and the installed binary against their recorded digests,
// writes the retained copy as a transaction temp, fsyncs it, hashes it, and
// renames it into place. The returned identity is the verified previous
// binary; the target is untouched.
//
// Splitting this step from SwapPrepared lets the upgrade transaction record
// ownership of the new .prev inode and journal its verified digest before the
// target swap. A crash in that window then leaves an owned previous binary.
func PublishPrevious(prepared BinaryFile, target, previous, token string) (BinaryFile, error) {
	if err := validateToken(token); err != nil {
		return BinaryFile{}, err
	}
	if prepared.Path != tempName(target, token, "new") {
		return BinaryFile{}, fmt.Errorf("prepared binary %s does not belong to transaction %s", prepared.Path, token)
	}
	preparedInfo, err := os.Stat(prepared.Path)
	if err != nil {
		return BinaryFile{}, fmt.Errorf("prepared binary %s: %w", prepared.Path, err)
	}
	if !preparedInfo.Mode().IsRegular() {
		return BinaryFile{}, fmt.Errorf("prepared binary %s is not a regular file", prepared.Path)
	}
	preparedSum, err := HashFile(prepared.Path)
	if err != nil {
		return BinaryFile{}, fmt.Errorf("hash prepared binary: %w", err)
	}
	if prepared.SHA256 == "" || preparedSum != prepared.SHA256 {
		_ = os.Remove(prepared.Path)
		return BinaryFile{}, fmt.Errorf("prepared binary %s does not match its recorded SHA-256", prepared.Path)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return BinaryFile{}, fmt.Errorf("installed binary %s: %w", target, err)
	}
	if !targetInfo.Mode().IsRegular() {
		return BinaryFile{}, fmt.Errorf("installed binary %s is not a regular file", target)
	}
	previousSum, err := HashFile(target)
	if err != nil {
		return BinaryFile{}, fmt.Errorf("hash installed binary: %w", err)
	}
	dir := filepath.Dir(target)
	prevTemp := tempName(target, token, "prev")
	_ = os.Remove(prevTemp)
	if err := linkOrCopy(target, prevTemp, targetInfo.Mode().Perm()); err != nil {
		return BinaryFile{}, fmt.Errorf("retain previous binary: %w", err)
	}
	if err := os.Chmod(prevTemp, targetInfo.Mode().Perm()); err != nil {
		return BinaryFile{}, fmt.Errorf("preserve previous mode: %w", err)
	}
	prevSum, err := HashFile(prevTemp)
	if err != nil {
		return BinaryFile{}, fmt.Errorf("hash retained previous binary: %w", err)
	}
	if prevSum != previousSum {
		_ = os.Remove(prevTemp)
		return BinaryFile{}, fmt.Errorf("retained previous binary %s does not match %s", prevTemp, target)
	}
	if err := syncFile(prevTemp); err != nil {
		return BinaryFile{}, fmt.Errorf("sync retained previous binary: %w", err)
	}
	if err := os.Rename(prevTemp, previous); err != nil {
		return BinaryFile{}, fmt.Errorf("replace %s: %w", previous, err)
	}
	if err := syncDir(dir); err != nil {
		return BinaryFile{}, err
	}
	return BinaryFile{Path: previous, SHA256: previousSum, Mode: targetInfo.Mode().Perm()}, nil
}

// SwapPrepared atomically renames the verified prepared binary over target and
// fsyncs the directory. It never removes previous and never touches any other
// path; a missing or mismatched prepared file is refused and target stays as it
// was.
func SwapPrepared(prepared BinaryFile, target, token string) error {
	if err := validateToken(token); err != nil {
		return err
	}
	if prepared.Path != tempName(target, token, "new") {
		return fmt.Errorf("prepared binary %s does not belong to transaction %s", prepared.Path, token)
	}
	info, err := os.Stat(prepared.Path)
	if err != nil {
		return fmt.Errorf("prepared binary %s: %w", prepared.Path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("prepared binary %s is not a regular file", prepared.Path)
	}
	sum, err := HashFile(prepared.Path)
	if err != nil {
		return fmt.Errorf("hash prepared binary: %w", err)
	}
	if prepared.SHA256 == "" || sum != prepared.SHA256 {
		_ = os.Remove(prepared.Path)
		return fmt.Errorf("prepared binary %s does not match its recorded SHA-256", prepared.Path)
	}
	dir := filepath.Dir(target)
	if err := os.Rename(prepared.Path, target); err != nil {
		return fmt.Errorf("replace %s: %w", target, err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	installedSum, err := HashFile(target)
	if err != nil {
		return fmt.Errorf("hash installed binary: %w", err)
	}
	if installedSum != prepared.SHA256 {
		return fmt.Errorf("installed binary %s does not match the prepared release", target)
	}
	return nil
}

// PublishBinary installs staged as target without ever leaving target missing.
// It is the one-shot form of PrepareBinary followed by PublishPrepared.
func PublishBinary(staged, target, previous, token string) error {
	prepared, err := PrepareBinary(staged, target, token)
	if err != nil {
		return err
	}
	if _, err := PublishPrepared(prepared, target, previous, token); err != nil {
		return err
	}
	return nil
}

// RecoverPublish finishes or discards an interrupted PublishPrepared. It only
// acts on transaction temp files whose contents match the recorded digests: a
// new binary whose hash differs from newSHA is discarded, and a previous temp
// that does not match previousSHA is discarded. When target is missing it is
// restored from a verified transaction file rather than left absent.
func RecoverPublish(target, previous, token, newSHA, previousSHA string) (bool, error) {
	if err := validateToken(token); err != nil {
		return false, err
	}
	did := false
	dir := filepath.Dir(target)
	previousOK := false
	prevTemp := tempName(target, token, "prev")
	if info, err := os.Lstat(prevTemp); err == nil {
		if !info.Mode().IsRegular() {
			return did, fmt.Errorf("previous temp %s is not a regular file", prevTemp)
		}
		sum, hashErr := HashFile(prevTemp)
		switch {
		case hashErr != nil:
			return did, fmt.Errorf("hash previous temp: %w", hashErr)
		case previousSHA != "" && sum != previousSHA:
			_ = os.Remove(prevTemp)
		default:
			if err := syncFile(prevTemp); err != nil {
				return did, fmt.Errorf("sync previous temp: %w", err)
			}
			if err := os.Rename(prevTemp, previous); err != nil {
				return did, fmt.Errorf("recover previous binary: %w", err)
			}
			if err := syncDir(dir); err != nil {
				return did, err
			}
			previousOK = true
			did = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return did, fmt.Errorf("inspect previous temp: %w", err)
	}

	if previousSHA != "" {
		if sum, err := HashFile(previous); err == nil && sum == previousSHA {
			previousOK = true
		}
	}
	targetSum := ""
	if sum, err := HashFile(target); err == nil {
		targetSum = sum
	} else if !errors.Is(err, os.ErrNotExist) {
		return did, fmt.Errorf("hash installed binary: %w", err)
	}
	if !previousOK && previousSHA != "" {
		// The retained previous was lost before it was renamed. When target is
		// still the old binary it can be retained again safely; otherwise the
		// old release is gone and recovery must report it.
		if targetSum != previousSHA {
			return did, fmt.Errorf("previous binary %s does not match the recorded SHA-256", previous)
		}
		if err := os.Remove(prevTemp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return did, err
		}
		if err := linkOrCopy(target, prevTemp, 0o755); err != nil {
			return did, fmt.Errorf("re-retain previous binary: %w", err)
		}
		if err := syncFile(prevTemp); err != nil {
			return did, err
		}
		if err := os.Rename(prevTemp, previous); err != nil {
			return did, fmt.Errorf("recover previous binary: %w", err)
		}
		if err := syncDir(dir); err != nil {
			return did, err
		}
		previousOK = true
		did = true
	}

	newTemp := tempName(target, token, "new")
	if info, err := os.Lstat(newTemp); err == nil {
		if !info.Mode().IsRegular() {
			return did, fmt.Errorf("prepared temp %s is not a regular file", newTemp)
		}
		sum, hashErr := HashFile(newTemp)
		switch {
		case hashErr != nil:
			return did, fmt.Errorf("hash prepared binary: %w", hashErr)
		case newSHA == "" || sum != newSHA:
			if err := os.Remove(newTemp); err != nil {
				return did, err
			}
			did = true
		case previousSHA != "" && !previousOK:
			return did, errors.New("refusing to publish a prepared binary without a verified previous binary")
		default:
			if targetSum != "" && targetSum == sum {
				_ = os.Remove(newTemp)
			} else if targetSum == "" || targetSum == previousSHA {
				if err := os.Rename(newTemp, target); err != nil {
					return did, fmt.Errorf("recover installed binary: %w", err)
				}
				if err := syncDir(dir); err != nil {
					return did, err
				}
				did = true
				targetSum = sum
			} else {
				_ = os.Remove(newTemp)
				return did, fmt.Errorf("installed binary %s matches neither the previous nor the prepared release", target)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return did, fmt.Errorf("inspect prepared binary: %w", err)
	}
	if targetSum == "" {
		return did, fmt.Errorf("installed binary %s is missing", target)
	}
	return did, nil
}

// RecoverPublishBinary infers the transaction digests from the temp files and
// the current target. Callers that have journal digests must use RecoverPublish
// instead.
func RecoverPublishBinary(target, previous, token string) (bool, error) {
	newSHA, _ := HashFile(tempName(target, token, "new"))
	previousSHA, _ := HashFile(tempName(target, token, "prev"))
	if previousSHA == "" {
		previousSHA, _ = HashFile(target)
	}
	return RecoverPublish(target, previous, token, newSHA, previousSHA)
}

// RestorePrevious atomically replaces target with previous and retains the
// failed target as previous. Two transaction-unique temporary files are created
// first, so target is never renamed away and never missing. The retained files
// keep their executable modes.
func RestorePrevious(target, previous, token string) error {
	if err := validateToken(token); err != nil {
		return err
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("no current binary at %s: %w", target, err)
	}
	previousInfo, err := os.Stat(previous)
	if err != nil {
		return fmt.Errorf("no previous binary at %s: %w", previous, err)
	}
	if !targetInfo.Mode().IsRegular() || !previousInfo.Mode().IsRegular() {
		return errors.New("binary paths must be regular files")
	}
	newTargetTemp := tempName(target, token, "restore-new")
	oldTargetTemp := tempName(target, token, "restore-old")
	_ = os.Remove(newTargetTemp)
	_ = os.Remove(oldTargetTemp)
	failed := true
	defer func() {
		if failed {
			_ = os.Remove(newTargetTemp)
			_ = os.Remove(oldTargetTemp)
		}
	}()

	if err := linkOrCopy(previous, newTargetTemp, previousInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("stage previous binary: %w", err)
	}
	if err := syncFile(newTargetTemp); err != nil {
		return fmt.Errorf("sync previous binary: %w", err)
	}
	if err := linkOrCopy(target, oldTargetTemp, targetInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("retain current binary: %w", err)
	}
	if err := syncFile(oldTargetTemp); err != nil {
		return fmt.Errorf("sync current binary: %w", err)
	}
	dir := filepath.Dir(target)
	if err := os.Rename(newTargetTemp, target); err != nil {
		return fmt.Errorf("restore %s: %w", target, err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	if err := os.Rename(oldTargetTemp, previous); err != nil {
		return fmt.Errorf("retain %s: %w", previous, err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	failed = false
	return nil
}

// RecoverSwap finishes a RestorePrevious interrupted between its two renames.
// It reports whether it completed any pending work.
//
// Both candidates must match the digests the journal recorded before the swap
// started: restore-new must be the previous release (restoredSHA) and
// restore-old must be the failed target (retainedSHA). A mismatched file is
// never renamed into place; it is left exactly where it is so an operator can
// inspect it, and the currently installed binaries stay untouched.
func RecoverSwap(target, previous, token, restoredSHA, retainedSHA string) (bool, error) {
	if err := validateToken(token); err != nil {
		return false, err
	}
	if restoredSHA == "" || retainedSHA == "" {
		return false, errors.New("refusing to recover a binary swap without recorded SHA-256 digests")
	}
	restored, err := verifyPendingSwap(tempName(target, token, "restore-new"), restoredSHA, "restored target")
	if err != nil {
		return false, err
	}
	retained, err := verifyPendingSwap(tempName(target, token, "restore-old"), retainedSHA, "retained previous")
	if err != nil {
		return false, err
	}
	did := false
	dir := filepath.Dir(target)
	if restored {
		if err := os.Rename(tempName(target, token, "restore-new"), target); err != nil {
			return did, fmt.Errorf("recover restored target: %w", err)
		}
		if err := syncDir(dir); err != nil {
			return did, err
		}
		did = true
	}
	if retained {
		if err := os.Rename(tempName(target, token, "restore-old"), previous); err != nil {
			return did, fmt.Errorf("recover retained previous: %w", err)
		}
		if err := syncDir(dir); err != nil {
			return did, err
		}
		did = true
	}
	return did, nil
}

// verifyPendingSwap reports whether path exists and matches want. A missing
// file is fine; a non-regular or mismatched file is an error and is preserved.
func verifyPendingSwap(path, want, description string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect %s temp: %w", description, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s temp %s is not a regular file", description, path)
	}
	sum, err := HashFile(path)
	if err != nil {
		return false, fmt.Errorf("hash %s temp: %w", description, err)
	}
	if sum != want {
		return false, fmt.Errorf("%s temp %s has SHA-256 %s, journal records %s; refusing to publish unverified binaries", description, path, sum, want)
	}
	return true, nil
}

// SwapWithPrevious exchanges path with path+".prev". It is used by rollback and
// by automatic rollback after a failed health check.
func SwapWithPrevious(path string) error {
	return RestorePrevious(path, path+".prev", randomToken())
}

// CleanupTransaction removes this transaction's temporary files beside target.
// It never touches target or previous.
func CleanupTransaction(target, token string) error {
	if err := validateToken(token); err != nil {
		return err
	}
	var errs []error
	for _, suffix := range []string{"new", "prev", "restore-new", "restore-old"} {
		if err := os.Remove(tempName(target, token, suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func validateToken(token string) error {
	if token == "" {
		return errors.New("transaction token is required")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`).MatchString(token) {
		return fmt.Errorf("transaction token %q contains unsafe characters", token)
	}
	return nil
}

func tempName(target, token, suffix string) string {
	return filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+"."+token+"."+suffix)
}

// TransactionTempPath returns the path of one transaction temp file beside
// target. Callers use it to record ownership of a temp file before it exists.
func TransactionTempPath(target, token, suffix string) string {
	return tempName(target, token, suffix)
}

func randomToken() string {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return fmt.Sprintf("t%d", os.Getpid())
	}
	return hex.EncodeToString(buffer[:])
}

func stageFile(path string, perm os.FileMode, write func(io.Writer) error) (string, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	name := tmp.Name()
	failed := true
	defer func() {
		if failed {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		return "", fmt.Errorf("set mode on %s: %w", name, err)
	}
	if err := write(tmp); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", name, err)
	}
	failed = false
	return name, nil
}

// copyInto writes src to dst with perm, fsyncing the result.
func copyInto(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
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
	if err := out.Chmod(perm); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

// linkOrCopy hard-links src to dst so the retained executable is the exact
// inode that was running. Filesystems without hard links fall back to a copy,
// preserving perm.
func linkOrCopy(src, dst string, perm os.FileMode) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chmod(perm); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

// ensureDir creates path and any missing parents with perm, then fsyncs every
// created directory and its parent up to an existing ancestor.
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
