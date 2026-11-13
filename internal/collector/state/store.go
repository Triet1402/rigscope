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
	"sync"
)

// File names inside the collector-private directory.
const (
	identityFile    = "identity.json"
	credentialsFile = "credentials.json"
	enrollmentFile  = "enrollment.json"
)

// Bound for every private state file. They hold a handful of short strings.
const maxStateFileBytes = 64 * 1024

// MaxCredentials bounds the stored per-hub credentials. Rigscope 1.0 talks to
// exactly one hub; the list is bounded so a corrupted or hand-edited file
// cannot grow without limit.
const MaxCredentials = 8

// Options injects the environment the store depends on. Zero values use the
// real machine and cryptography/rand.
type Options struct {
	// HostID returns a stable machine fingerprint. Defaults to
	// /etc/machine-id (then /var/lib/dbus/machine-id), falling back to the
	// hostname. An empty string means "unknown": clone detection is skipped.
	HostID func() string
	// Random is the entropy source for new installation ids.
	Random io.Reader
	// Owner, when set, is applied to every file this store writes that has
	// no owner of its own. A root command acting on a system collector (for
	// example "sudo rigscope collector enroll") sets it to the collector
	// account, so it never leaves a root-owned file the service cannot read.
	Owner func(*os.File) error
}

// Store gives access to one collector state directory.
//
// All methods are safe for concurrent use from one process. Cross-process
// serialization is the caller's responsibility: the collector service and the
// enrollment command are never started at the same time.
type Store struct {
	dir    string
	hostID func() string
	random io.Reader
	owner  func(*os.File) error
	mu     sync.Mutex
}

// Open prepares <root>/collector/ and returns a store. The directory is
// created with 0700 when missing; an existing directory that is not private or
// not a directory is refused rather than repaired silently.
func Open(root string, opts Options) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("state: state root must not be empty")
	}
	dir := filepath.Join(root, "collector")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state: create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("state: inspect %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("state: %s must be a directory", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("state: %s must be private (0700)", dir)
	}
	if opts.HostID == nil {
		opts.HostID = MachineFingerprint
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	return &Store{
		dir:    dir,
		hostID: opts.HostID,
		random: opts.Random,
		owner:  opts.Owner,
	}, nil
}

// Dir returns the collector-private directory.
func (s *Store) Dir() string { return s.dir }

// Identity returns the persistent installation identity, creating it when
// missing. A file whose recorded machine fingerprint does not match this
// machine is treated as a clone: a new collector id is generated and the
// credentials and enrollment proof that belonged to the old identity are
// removed.
func (s *Store) Identity() (StoredIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadIdentityLocked()
}

// ValidateIdentity checks the stored installation identity without creating
// it, adopting this machine's fingerprint or regenerating a cloned identity.
// A missing file is valid: the next run creates it. It is the read-only probe
// `rigscope collector --check` uses.
func (s *Store) ValidateIdentity() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok, err := s.readFile(identityFile)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	var id StoredIdentity
	if err := decodeStrict(data, &id); err != nil {
		return fmt.Errorf("state: %s: %w", identityFile, err)
	}
	if err := id.validate(); err != nil {
		return fmt.Errorf("state: %s: %w", identityFile, err)
	}
	return nil
}

func (s *Store) loadIdentityLocked() (StoredIdentity, error) {
	data, ok, err := s.readFile(identityFile)
	if err != nil {
		return StoredIdentity{}, err
	}
	current := s.hostID()
	if !ok {
		id, err := s.newIdentityLocked(current)
		if err != nil {
			return StoredIdentity{}, err
		}
		return id, nil
	}
	var id StoredIdentity
	if err := decodeStrict(data, &id); err != nil {
		return StoredIdentity{}, fmt.Errorf("state: %s: %w", identityFile, err)
	}
	if err := id.validate(); err != nil {
		return StoredIdentity{}, fmt.Errorf("state: %s: %w", identityFile, err)
	}
	switch {
	case id.HostID == "" && current != "":
		// A file from before fingerprinting: adopt this machine without
		// discarding the installation id it already owns.
		id.HostID = current
		if err := s.writeJSONLocked(identityFile, id); err != nil {
			return StoredIdentity{}, err
		}
	case id.HostID != "" && current != "" && id.HostID != current:
		// The state directory was copied from another machine. The old
		// collector id and its credentials belong to that machine.
		regenerated, err := s.newIdentityLocked(current)
		if err != nil {
			return StoredIdentity{}, err
		}
		// The old credential and proof belong to the machine this directory
		// was copied from. Refuse to continue silently if they cannot be
		// cleared: an operator must know before the clone talks to the hub.
		if err := s.removeFileLocked(credentialsFile); err != nil {
			return StoredIdentity{}, err
		}
		if err := s.removeFileLocked(enrollmentFile); err != nil {
			return StoredIdentity{}, err
		}
		return regenerated, nil
	}
	return id, nil
}

func (s *Store) newIdentityLocked(hostID string) (StoredIdentity, error) {
	collectorID, err := newUUID(s.random)
	if err != nil {
		return StoredIdentity{}, err
	}
	id := StoredIdentity{CollectorID: collectorID, HostID: hostID}
	if err := s.writeJSONLocked(identityFile, id); err != nil {
		return StoredIdentity{}, err
	}
	return id, nil
}

// EnsureIdentity returns the persistent installation identity, creating it
// when missing. Creation goes through the descriptor-owned write path so an
// installer running as root can hand the file to the collector account without
// a window where the final path has the wrong owner. An existing identity is
// returned unchanged: an automatic pairing grant must be bound to the id the
// collector will actually present.
func (s *Store) EnsureIdentity(prepare func(*os.File) error) (StoredIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok, err := s.readFile(identityFile)
	if err != nil {
		return StoredIdentity{}, err
	}
	if ok {
		var id StoredIdentity
		if err := decodeStrict(data, &id); err != nil {
			return StoredIdentity{}, fmt.Errorf("state: %s: %w", identityFile, err)
		}
		if err := id.validate(); err != nil {
			return StoredIdentity{}, fmt.Errorf("state: %s: %w", identityFile, err)
		}
		return id, nil
	}
	collectorID, err := newUUID(s.random)
	if err != nil {
		return StoredIdentity{}, err
	}
	id := StoredIdentity{CollectorID: collectorID, HostID: s.hostID()}
	payload, err := json.Marshal(id)
	if err != nil {
		return StoredIdentity{}, fmt.Errorf("state: encode %s: %w", identityFile, err)
	}
	payload = append(payload, '\n')
	if err := s.writeFileOwned(identityFile, payload, nil, nil, prepare); err != nil {
		return StoredIdentity{}, err
	}
	return id, nil
}

// Credentials returns every stored credential.
func (s *Store) Credentials() ([]Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadCredentialsLocked()
}

// Credential returns the stored credential for one hub URL.
func (s *Store) Credential(hubURL string) (Credential, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credentials, err := s.loadCredentialsLocked()
	if err != nil {
		return Credential{}, false, err
	}
	hubURL = canonicalURL(hubURL)
	for _, credential := range credentials {
		if credential.URL == hubURL {
			return credential, true, nil
		}
	}
	return Credential{}, false, nil
}

// SaveCredential durably stores (or replaces) the credential for its hub URL.
func (s *Store) SaveCredential(credential Credential) error {
	if err := credential.validate(); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	credential.URL = canonicalURL(credential.URL)
	s.mu.Lock()
	defer s.mu.Unlock()
	credentials, err := s.loadCredentialsLocked()
	if err != nil {
		return err
	}
	next := make([]Credential, 0, len(credentials)+1)
	replaced := false
	for _, existing := range credentials {
		if existing.URL == credential.URL {
			next = append(next, credential)
			replaced = true
			continue
		}
		next = append(next, existing)
	}
	if !replaced {
		if len(next) >= MaxCredentials {
			return fmt.Errorf("state: credential budget exhausted (%d)", MaxCredentials)
		}
		next = append(next, credential)
	}
	return s.writeCredentialsLocked(next)
}

// DeleteCredential removes the credential for one hub URL. A missing entry is
// not an error.
func (s *Store) DeleteCredential(hubURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	credentials, err := s.loadCredentialsLocked()
	if err != nil {
		return err
	}
	hubURL = canonicalURL(hubURL)
	next := credentials[:0]
	for _, credential := range credentials {
		if credential.URL != hubURL {
			next = append(next, credential)
		}
	}
	return s.writeCredentialsLocked(next)
}

// PendingEnrollment returns the saved enrollment proof for a hub URL.
func (s *Store) PendingEnrollment(hubURL string) (PendingEnrollment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, err := s.loadEnrollmentsLocked()
	if err != nil {
		return PendingEnrollment{}, false, err
	}
	hubURL = canonicalURL(hubURL)
	for _, enrollment := range pending {
		if enrollment.URL == hubURL {
			return enrollment, true, nil
		}
	}
	return PendingEnrollment{}, false, nil
}

// PendingEnrollments returns every saved enrollment proof.
func (s *Store) PendingEnrollments() ([]PendingEnrollment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadEnrollmentsLocked()
}

// SavePendingEnrollment durably stores (or replaces) the proof for its URL.
// It must be called before the enrollment request so a lost response can be
// retried with the same proof.
func (s *Store) SavePendingEnrollment(pending PendingEnrollment) error {
	if err := pending.validate(); err != nil {
		return err
	}
	pending.URL = canonicalURL(pending.URL)
	s.mu.Lock()
	defer s.mu.Unlock()
	enrollments, err := s.loadEnrollmentsLocked()
	if err != nil {
		return err
	}
	next := make([]PendingEnrollment, 0, len(enrollments)+1)
	replaced := false
	for _, existing := range enrollments {
		if existing.URL == pending.URL {
			next = append(next, pending)
			replaced = true
			continue
		}
		next = append(next, existing)
	}
	if !replaced {
		if len(next) >= MaxCredentials {
			return fmt.Errorf("state: enrollment budget exhausted (%d)", MaxCredentials)
		}
		next = append(next, pending)
	}
	return s.writeJSONLocked(enrollmentFile, enrollmentFileValue{Enrollments: next})
}

// DeletePendingEnrollment removes the proof for one URL. A missing entry is
// not an error.
func (s *Store) DeletePendingEnrollment(hubURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	enrollments, err := s.loadEnrollmentsLocked()
	if err != nil {
		return err
	}
	hubURL = canonicalURL(hubURL)
	next := enrollments[:0]
	for _, enrollment := range enrollments {
		if enrollment.URL != hubURL {
			next = append(next, enrollment)
		}
	}
	if len(next) == 0 {
		return s.removeFileLocked(enrollmentFile)
	}
	return s.writeJSONLocked(enrollmentFile, enrollmentFileValue{Enrollments: next})
}

// CompleteEnrollment saves the issued credential first and only then removes
// the enrollment proof. If the process dies between the two writes, the proof
// remains and a retry returns the same credential from the hub (or finds the
// credential already saved); it is never lost in the other order.
func (s *Store) CompleteEnrollment(hubURL string, credential Credential) error {
	if canonicalURL(credential.URL) != canonicalURL(hubURL) {
		return errors.New("state: credential URL does not match the enrollment URL")
	}
	if err := s.SaveCredential(credential); err != nil {
		return err
	}
	if err := s.DeletePendingEnrollment(hubURL); err != nil {
		return fmt.Errorf("state: credential saved but enrollment proof removal failed: %w", err)
	}
	return nil
}

func (s *Store) loadCredentialsLocked() ([]Credential, error) {
	data, ok, err := s.readFile(credentialsFile)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	var value credentialsFileValue
	if err := decodeStrict(data, &value); err != nil {
		return nil, fmt.Errorf("state: %s: %w", credentialsFile, err)
	}
	if len(value.Credentials) > MaxCredentials {
		return nil, fmt.Errorf("state: %s: too many credentials", credentialsFile)
	}
	raw := make(map[string]struct{}, len(value.Credentials))
	for _, credential := range value.Credentials {
		if err := credential.validate(); err != nil {
			return nil, fmt.Errorf("state: %s: %w", credentialsFile, err)
		}
		if _, duplicate := raw[credential.URL]; duplicate {
			return nil, fmt.Errorf("state: %s: duplicate credential URL", credentialsFile)
		}
		raw[credential.URL] = struct{}{}
	}
	return canonicalCredentials(value.Credentials), nil
}

func (s *Store) writeCredentialsLocked(credentials []Credential) error {
	for _, credential := range credentials {
		if err := credential.validate(); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	if len(credentials) == 0 {
		return s.removeFileLocked(credentialsFile)
	}
	return s.writeJSONLocked(credentialsFile, credentialsFileValue{Credentials: credentials})
}

func (s *Store) loadEnrollmentsLocked() ([]PendingEnrollment, error) {
	data, ok, err := s.readFile(enrollmentFile)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	var value enrollmentFileValue
	if err := decodeStrict(data, &value); err != nil {
		// A flat grant from the installer is upgraded here, before the
		// enrollment request, so the collector generates the recovery proof
		// it will reuse after a lost response. The upgrade is atomic: either
		// the old file or the complete new document is visible.
		upgraded, upgradeErr := s.upgradeLegacyEnrollmentLocked(data)
		if upgradeErr != nil {
			return nil, fmt.Errorf("state: %s: %w", enrollmentFile, err)
		}
		return upgraded, nil
	}
	if len(value.Enrollments) > MaxCredentials {
		return nil, fmt.Errorf("state: %s: too many enrollments", enrollmentFile)
	}
	raw := make(map[string]struct{}, len(value.Enrollments))
	for _, enrollment := range value.Enrollments {
		if err := enrollment.validate(); err != nil {
			return nil, fmt.Errorf("state: %s: %w", enrollmentFile, err)
		}
		if _, duplicate := raw[enrollment.URL]; duplicate {
			return nil, fmt.Errorf("state: %s: duplicate enrollment URL", enrollmentFile)
		}
		raw[enrollment.URL] = struct{}{}
	}
	return canonicalEnrollments(value.Enrollments), nil
}

// upgradeLegacyEnrollmentLocked converts the flat installer grant into a
// pending-enrollment document. It returns the upgraded list and rewrites the
// file in place with the collector-owned schema.
func (s *Store) upgradeLegacyEnrollmentLocked(data []byte) ([]PendingEnrollment, error) {
	var legacy legacyEnrollmentFileValue
	if err := decodeStrict(data, &legacy); err != nil {
		return nil, err
	}
	if legacy.Version != 1 {
		return nil, fmt.Errorf("unsupported enrollment file version %d", legacy.Version)
	}
	requestID, secret, err := NewRecoveryProof(s.random)
	if err != nil {
		return nil, err
	}
	pending := PendingEnrollment{
		URL:            legacy.URL,
		Grant:          legacy.Grant,
		RequestID:      requestID,
		RecoverySecret: secret,
	}
	if err := pending.validate(); err != nil {
		return nil, err
	}
	if err := s.writeJSONLocked(enrollmentFile, enrollmentFileValue{Enrollments: []PendingEnrollment{pending}}); err != nil {
		return nil, err
	}
	return []PendingEnrollment{pending}, nil
}

// readFile reads one private state file. A missing file is (nil, false, nil).
func (s *Store) readFile(name string) ([]byte, bool, error) {
	path := filepath.Join(s.dir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("state: inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("state: %s must be a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("state: %s must be private (0600)", path)
	}
	if info.Size() > maxStateFileBytes {
		return nil, false, fmt.Errorf("state: %s exceeds %d bytes", path, maxStateFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("state: read %s: %w", path, err)
	}
	return data, true, nil
}

// writeJSONLocked writes one value atomically with private permissions.
func (s *Store) writeJSONLocked(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("state: encode %s: %w", name, err)
	}
	data = append(data, '\n')
	if len(data) > maxStateFileBytes {
		return fmt.Errorf("state: %s exceeds %d bytes", name, maxStateFileBytes)
	}
	return s.writeFile(name, data)
}

func (s *Store) writeFile(name string, data []byte) error {
	return s.writeFileOwned(name, data, nil, nil, nil)
}

// writeFileOwned writes one file atomically. A non-nil uid or gid is applied to
// the temporary descriptor before the rename, so root can install a file for
// the collector account with no window where the final path has the wrong
// owner.
func (s *Store) writeFileOwned(name string, data []byte, uid, gid *int, prepare func(*os.File) error) error {
	info, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("state: inspect %s: %w", s.dir, err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state: %s must be a private directory (0700)", s.dir)
	}
	target := filepath.Join(s.dir, name)
	if existing, err := os.Lstat(target); err == nil {
		if !existing.Mode().IsRegular() {
			return fmt.Errorf("state: %s must be a regular file", target)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state: inspect %s: %w", target, err)
	}

	temporary := filepath.Join(s.dir, "."+name+"."+mustUUID(s.random)+".tmp")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("state: create %s: %w", temporary, err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(temporary)
	}
	if uid != nil || gid != nil {
		owner, group := -1, -1
		if uid != nil {
			owner = *uid
		}
		if gid != nil {
			group = *gid
		}
		if err := file.Chown(owner, group); err != nil {
			cleanup()
			return fmt.Errorf("state: set owner of %s: %w", temporary, err)
		}
	}
	if prepare == nil && uid == nil && gid == nil {
		prepare = s.owner
	}
	if prepare != nil {
		if err := prepare(file); err != nil {
			cleanup()
			return err
		}
	}
	if _, err := file.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("state: write %s: %w", temporary, err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("state: sync %s: %w", temporary, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("state: close %s: %w", temporary, err)
	}
	if err := os.Rename(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("state: replace %s: %w", target, err)
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("state: open %s: %w", s.dir, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("state: sync %s: %w", s.dir, err)
	}
	return nil
}

func (s *Store) removeFileLocked(name string) error {
	path := filepath.Join(s.dir, name)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state: remove %s: %w", path, err)
	}
	return nil
}

// decodeStrict decodes exactly one JSON value and rejects unknown fields and
// trailing data.
func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

// canonicalCredentials keys every stored credential by its canonical endpoint.
// Two entries can differ only in spelling (ws://localhost:8081 written before
// the shared URL policy, ws://127.0.0.1:8081 after it); they name the same
// hub, and the later entry in the file is the more recently saved one, so it
// wins. The next write persists the canonical form.
func canonicalCredentials(credentials []Credential) []Credential {
	out := make([]Credential, 0, len(credentials))
	index := make(map[string]int, len(credentials))
	for _, credential := range credentials {
		credential.URL = canonicalURL(credential.URL)
		if at, ok := index[credential.URL]; ok {
			out[at] = credential
			continue
		}
		index[credential.URL] = len(out)
		out = append(out, credential)
	}
	return out
}

// canonicalEnrollments applies the same identity rule to enrollment proofs.
func canonicalEnrollments(enrollments []PendingEnrollment) []PendingEnrollment {
	out := make([]PendingEnrollment, 0, len(enrollments))
	index := make(map[string]int, len(enrollments))
	for _, enrollment := range enrollments {
		enrollment.URL = canonicalURL(enrollment.URL)
		if at, ok := index[enrollment.URL]; ok {
			out[at] = enrollment
			continue
		}
		index[enrollment.URL] = len(out)
		out = append(out, enrollment)
	}
	return out
}

// AliasCredential stores the credential issued for fromURL under toURL as
// well. The installer uses it when the colocated hub's own address changes
// (a new port or origin): the hub and its credential are the same, only the
// endpoint moved. It is idempotent and keeps the fromURL entry, so a process
// death before the configuration names toURL still leaves a working
// credential. prepare, when non-nil, is applied to the new file's descriptor
// before it is published, so root can keep the collector account as owner.
func (s *Store) AliasCredential(fromURL, toURL string, prepare func(*os.File) error) error {
	fromURL, toURL = canonicalURL(fromURL), canonicalURL(toURL)
	if err := validateHubURL(toURL); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	credentials, err := s.loadCredentialsLocked()
	if err != nil {
		return err
	}
	var source *Credential
	for i := range credentials {
		if credentials[i].URL == toURL {
			return nil
		}
		if credentials[i].URL == fromURL {
			source = &credentials[i]
		}
	}
	if source == nil {
		return fmt.Errorf("state: no credential for the previous hub address")
	}
	if len(credentials) >= MaxCredentials {
		return fmt.Errorf("state: credential budget exhausted (%d)", MaxCredentials)
	}
	next := append(append([]Credential(nil), credentials...), Credential{URL: toURL, Token: source.Token})
	return s.writeCredentialsOwnedLocked(next, prepare)
}

// DropCredential removes the credential for hubURL with descriptor-applied
// ownership. A missing entry is not an error.
func (s *Store) DropCredential(hubURL string, prepare func(*os.File) error) error {
	hubURL = canonicalURL(hubURL)
	s.mu.Lock()
	defer s.mu.Unlock()
	credentials, err := s.loadCredentialsLocked()
	if err != nil {
		return err
	}
	next := make([]Credential, 0, len(credentials))
	for _, credential := range credentials {
		if credential.URL != hubURL {
			next = append(next, credential)
		}
	}
	if len(next) == len(credentials) {
		return nil
	}
	if len(next) == 0 {
		return s.removeFileLocked(credentialsFile)
	}
	return s.writeCredentialsOwnedLocked(next, prepare)
}

func (s *Store) writeCredentialsOwnedLocked(credentials []Credential, prepare func(*os.File) error) error {
	for _, credential := range credentials {
		if err := credential.validate(); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	data, err := json.Marshal(credentialsFileValue{Credentials: credentials})
	if err != nil {
		return fmt.Errorf("state: encode %s: %w", credentialsFile, err)
	}
	data = append(data, '\n')
	if len(data) > maxStateFileBytes {
		return fmt.Errorf("state: %s exceeds %d bytes", credentialsFile, maxStateFileBytes)
	}
	return s.writeFileOwned(credentialsFile, data, nil, nil, prepare)
}
