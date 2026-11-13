package state

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// WriteOptions controls who owns a file written by the installer on behalf of
// the collector account.
//
// UID and GID are applied to the temporary file through its descriptor before
// the atomic rename. A nil field leaves the owner unchanged; the installer
// running as root sets both so the collector service can read the file. The
// rename preserves ownership, and no window exists where the final path has
// the wrong owner.
type WriteOptions struct {
	UID *int
	GID *int
	// Prepare runs on the temporary file's descriptor before the write. It is
	// the caller's seam for ownership changes that must be applied through the
	// descriptor rather than the pathname.
	Prepare func(*os.File) error
	// Random is the entropy source for the generated recovery proof. Defaults
	// to crypto/rand.
	Random io.Reader
}

// WriteInitialGrant writes the enrollment proof an installer received as a
// one-time grant.
//
// The collector owns this file's format: it is the same
// {"enrollments":[{"url","grant","requestId","recoverySecret"}]} document the
// enrollment command writes before its first request. Generating the recovery
// proof here means a lost enrollment response is retried with the same proof,
// so the grant is not burned. An existing entry for the same hub URL is
// replaced; entries for other hubs are kept.
//
// It is safe to call before the collector account or service exists. The
// directory is created private (0700) when missing and must already be private
// when present.
func WriteInitialGrant(root, hubURL, grant string, opts WriteOptions) error {
	next, _, err := prepareEnrollmentUpdate(root, hubURL, grant, opts.Random)
	if err != nil {
		return err
	}
	store := &Store{dir: filepath.Join(root, "collector"), random: opts.Random}
	if store.random == nil {
		store.random = rand.Reader
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.writeEnrollmentFileOwned(next, opts.UID, opts.GID, opts.Prepare)
}

// EnrollmentDocument returns the collector's pending-enrollment document after
// staging grant for hubURL. It exists so the installer can publish the file
// through the installation manifest's owned, staged-publication flow instead of
// writing it directly.
//
// An existing entry for the same hub keeps its request id and recovery secret.
// A reinstall may rotate the grant (its predecessor can be expired or already
// consumed) while the hub still holds a recovery proof for the old request, so
// a lost-response retry remains possible.
func EnrollmentDocument(root, hubURL, grant string, opts WriteOptions) ([]byte, error) {
	next, _, err := prepareEnrollmentUpdate(root, hubURL, grant, opts.Random)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(enrollmentFileValue{Enrollments: next})
	if err != nil {
		return nil, fmt.Errorf("state: encode %s: %w", enrollmentFile, err)
	}
	data = append(data, '\n')
	if len(data) > maxStateFileBytes {
		return nil, fmt.Errorf("state: %s exceeds %d bytes", enrollmentFile, maxStateFileBytes)
	}
	return data, nil
}

// prepareEnrollmentUpdate loads the existing proofs and returns the document's
// enrollment list with grant staged for hubURL. The proof for the same hub is
// preserved when present; only a missing or invalid proof is generated anew.
func prepareEnrollmentUpdate(root, hubURL, grant string, random io.Reader) ([]PendingEnrollment, string, error) {
	if strings.TrimSpace(root) == "" {
		return nil, "", errors.New("state: state root must not be empty")
	}
	normalized, err := ValidateHubURL(hubURL)
	if err != nil {
		return nil, "", fmt.Errorf("state: %w", err)
	}
	if random == nil {
		random = rand.Reader
	}
	dir := filepath.Join(root, "collector")
	if err := ensurePrivateDir(dir); err != nil {
		return nil, "", err
	}
	store := &Store{dir: dir, random: random}
	store.mu.Lock()
	defer store.mu.Unlock()

	existing, err := store.loadEnrollmentsLocked()
	if err != nil {
		return nil, "", err
	}
	pending := PendingEnrollment{URL: normalized, Grant: grant}
	for _, enrollment := range existing {
		if enrollment.URL == normalized {
			pending.RequestID = enrollment.RequestID
			pending.RecoverySecret = enrollment.RecoverySecret
			break
		}
	}
	if pending.RequestID == "" || !isRecoverySecret(pending.RecoverySecret) {
		pending.RequestID, pending.RecoverySecret, err = NewRecoveryProof(random)
		if err != nil {
			return nil, "", err
		}
	}
	if err := pending.validate(); err != nil {
		return nil, "", fmt.Errorf("state: %w", err)
	}

	next := make([]PendingEnrollment, 0, len(existing)+1)
	replaced := false
	for _, enrollment := range existing {
		if enrollment.URL == normalized {
			next = append(next, pending)
			replaced = true
			continue
		}
		next = append(next, enrollment)
	}
	if !replaced {
		if len(next) >= MaxCredentials {
			return nil, "", fmt.Errorf("state: enrollment budget exhausted (%d)", MaxCredentials)
		}
		next = append(next, pending)
	}
	return next, normalized, nil
}

func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("state: inspect %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("state: %s must be a directory", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state: %s must be private (0700)", dir)
	}
	return nil
}

// writeEnrollmentFileOwned writes the enrollment document with an optional
// owner. It is the owned counterpart of writeJSONLocked.
func (s *Store) writeEnrollmentFileOwned(enrollments []PendingEnrollment, uid, gid *int, prepare func(*os.File) error) error {
	for _, enrollment := range enrollments {
		if err := enrollment.validate(); err != nil {
			return err
		}
	}
	data, err := json.Marshal(enrollmentFileValue{Enrollments: enrollments})
	if err != nil {
		return fmt.Errorf("state: encode %s: %w", enrollmentFile, err)
	}
	data = append(data, '\n')
	if len(data) > maxStateFileBytes {
		return fmt.Errorf("state: %s exceeds %d bytes", enrollmentFile, maxStateFileBytes)
	}
	return s.writeFileOwned(enrollmentFile, data, uid, gid, prepare)
}
