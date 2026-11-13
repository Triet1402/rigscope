package state

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// StoredIdentity is the persistent installation identity on disk.
type StoredIdentity struct {
	CollectorID string `json:"collectorId"`
	// HostID is the machine fingerprint recorded when the file was written.
	// A different fingerprint means the file was copied from another machine.
	HostID string `json:"hostId"`
}

func (i StoredIdentity) validate() error {
	if !isPrintableASCII(i.CollectorID, 1, 128) {
		return errors.New("collectorId must be 1..128 printable ASCII characters")
	}
	if !isPrintableASCII(i.HostID, 0, 128) {
		return errors.New("hostId must be 0..128 printable ASCII characters")
	}
	return nil
}

// Credential is one hub's issued bearer credential.
type Credential struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func (c Credential) validate() error {
	if err := validateHubURL(c.URL); err != nil {
		return err
	}
	if !isPrintableASCII(c.Token, 1, 256) {
		return errors.New("credential token must be 1..256 printable ASCII characters")
	}
	return nil
}

// PendingEnrollment is the proof written before an enrollment request.
//
// Grant is the one-time enrollment grant. RequestID and RecoverySecret are the
// values the hub binds to the issued credential: retrying with the same three
// values within the hub's recovery window returns the same credential, so a
// lost response never burns the grant.
type PendingEnrollment struct {
	URL            string `json:"url"`
	Grant          string `json:"grant"`
	RequestID      string `json:"requestId"`
	RecoverySecret string `json:"recoverySecret"`
}

func (p PendingEnrollment) validate() error {
	if err := validateHubURL(p.URL); err != nil {
		return err
	}
	if !isPrintableASCII(p.Grant, 1, 256) {
		return errors.New("enrollment grant must be 1..256 printable ASCII characters")
	}
	if !isPrintableASCII(p.RequestID, 1, 128) {
		return errors.New("enrollment requestId must be 1..128 printable ASCII characters")
	}
	if !isRecoverySecret(p.RecoverySecret) {
		return errors.New("enrollment recoverySecret must be 32..128 base64url characters")
	}
	return nil
}

type credentialsFileValue struct {
	Credentials []Credential `json:"credentials"`
}

type enrollmentFileValue struct {
	Enrollments []PendingEnrollment `json:"enrollments"`
}

// legacyEnrollmentFileValue is the flat grant an early installer wrote:
// {"version":1,"url":"...","grant":"..."}. It is accepted only on read and is
// upgraded atomically to the collector-owned format before any request.
type legacyEnrollmentFileValue struct {
	Version int    `json:"version"`
	URL     string `json:"url"`
	Grant   string `json:"grant"`
}

// Identity is the identity of one collector run.
type Identity struct {
	CollectorID string
	// RunID is new on every process launch.
	RunID string
	// BootID identifies the OS boot, or is BootIDUnavailable.
	BootID   string
	Hostname string
}

// BootIDUnavailable is the honest sentinel used when the kernel does not
// expose a boot id. It is non-empty because the wire contract requires a
// string, and it can never be mistaken for a real UUID.
const BootIDUnavailable = "unavailable"

// NewRunIdentity creates the identity for one collector process: the stored
// installation id, a fresh run id, the current boot id and the hostname.
func (s *Store) NewRunIdentity() (Identity, error) {
	stored, err := s.Identity()
	if err != nil {
		return Identity{}, err
	}
	runID, err := newUUID(s.random)
	if err != nil {
		return Identity{}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		// Hostname is metadata; an unreadable one is reported honestly as
		// empty rather than substituting a plausible-looking value.
		hostname = ""
	}
	if len(hostname) > 255 {
		hostname = hostname[:255]
	}
	return Identity{
		CollectorID: stored.CollectorID,
		RunID:       runID,
		BootID:      ReadBootID(),
		Hostname:    hostname,
	}, nil
}

// ReadBootID reads the Linux boot identity. Unlike the boot-id parser in the
// sampling packages this is intentionally tiny and has no fs.FS seam: it is
// called once per process launch.
func ReadBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err == nil {
		value := strings.ToLower(strings.TrimSpace(string(data)))
		if isUUID(value) {
			return value
		}
	}
	return BootIDUnavailable
}

// MachineFingerprint returns a stable machine identity for clone detection:
// the systemd/D-Bus machine id when available, otherwise the hostname.
func MachineFingerprint() string {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		value := strings.ToLower(strings.TrimSpace(string(data)))
		if value != "" && isPrintableASCII(value, 1, 128) {
			return value
		}
	}
	hostname, err := os.Hostname()
	if err != nil {
		return ""
	}
	return hostname
}

// NewRecoveryProof returns a printable request id and a 32-byte base64url
// recovery secret, matching the hub's accepted proof grammar.
func NewRecoveryProof(random io.Reader) (requestID, recoverySecret string, err error) {
	if random == nil {
		random = rand.Reader
	}
	requestID, err = newUUID(random)
	if err != nil {
		return "", "", err
	}
	secret := make([]byte, 32)
	if _, err := io.ReadFull(random, secret); err != nil {
		return "", "", fmt.Errorf("state: read random: %w", err)
	}
	return requestID, base64.RawURLEncoding.EncodeToString(secret), nil
}

func isRecoverySecret(value string) bool {
	if len(value) < 32 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'A' && character <= 'Z',
			character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

func isPrintableASCII(value string, min, max int) bool {
	if !utf8.ValidString(value) || len(value) < min || len(value) > max {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if !isHex(character) {
				return false
			}
		}
	}
	return true
}

func isHex(character rune) bool {
	return character >= '0' && character <= '9' ||
		character >= 'a' && character <= 'f' ||
		character >= 'A' && character <= 'F'
}

// newUUID returns a random RFC 4122 version 4 UUID.
func newUUID(random io.Reader) (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(random, raw[:]); err != nil {
		return "", fmt.Errorf("state: read random: %w", err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf(
		"%x-%x-%x-%x-%x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16],
	), nil
}

// mustUUID is used only for a temporary file name. A random failure is
// surfaced as the subsequent O_EXCL write error rather than a panic.
func mustUUID(random io.Reader) string {
	value, err := newUUID(random)
	if err != nil {
		return "tmp"
	}
	return value
}
