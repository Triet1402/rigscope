package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// The installation ownership manifest is the only proof that a path belongs to
// Rigscope 1.x. Install writes it before touching anything; uninstall refuses
// without it and deletes only the paths it records.
//
// Format 2 separates planned paths from owned paths. A planned path is one
// install intends to create; it is never deleted. An owned path records the
// identity (device, inode, owner) of the file install created and, for regular
// files, a SHA-256 digest. Uninstall deletes an owned path only while its
// current identity still matches, so an interrupted install can never claim a
// foreign file and a file swapped in behind the manifest is left alone.
const (
	manifestMarker        = "rigscope-installation/1"
	manifestFormatVersion = 2
	manifestMaxSize       = 1 << 20
)

const (
	manifestPlanned = "planned"
	manifestOwned   = "owned"
	// manifestAdopted records a file Rigscope relies on but did not write, such
	// as a GPU udev rule left behind by a 0.2 installation. Uninstall never
	// deletes an adopted path.
	manifestAdopted = "adopted"
)

const (
	kindBinary           = "binary"
	kindPreviousBinary   = "previous-binary"
	kindConfig           = "config"
	kindStateDir         = "state-dir"
	kindComponentState   = "component-state"
	kindCollectorState   = "collector-state"
	kindEnrollmentGrant  = "enrollment-grant"
	kindPairingBootstrap = "pairing-bootstrap"
	kindOwnerBootstrap   = "owner-bootstrap"
	kindCredentials      = "credentials"
	kindUnit             = "unit"
	kindSysusers         = "sysusers"
	kindUdevRAPL         = "udev-rapl"
	kindUdevGPU          = "udev-gpu"
	kindUpgradeJournal   = "upgrade-journal"
	kindStagingDir       = "staging-dir"
	kindBackupsDir       = "backups-dir"
	kindBinaryTemps      = "binary-temps"
	// kindBinaryTemp is one activation-created transaction temp file inside the
	// binary directory. It is a separate entry from the directory itself so a
	// reserved-looking name alone never authorizes deletion.
	kindBinaryTemp = "binary-temp"
)

// errManifestMissing distinguishes "this is not a 1.x installation" from a
// corrupted or unrecognised manifest, which must also stop uninstall.
var errManifestMissing = errors.New("installation manifest is missing")

// manifest is <stateRoot>/install-manifest.json. Paths, Units and
// IntegrationFiles each carry the kind that says how the path may be removed.
type manifest struct {
	Marker           string          `json:"marker"`
	FormatVersion    int             `json:"formatVersion"`
	Version          string          `json:"version"`
	Scope            config.Scope    `json:"scope"`
	Paths            []manifestEntry `json:"paths"`
	Units            []manifestEntry `json:"units"`
	IntegrationFiles []manifestEntry `json:"integrationFiles"`
}

// manifestIdentity is the recorded filesystem identity of an owned path.
type manifestIdentity struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// manifestEntry records one path. State is manifestPlanned or manifestOwned.
// Identity and Parent are required for owned entries; SHA256 is recorded for
// regular files install created.
type manifestEntry struct {
	Path     string            `json:"path"`
	Kind     string            `json:"kind"`
	State    string            `json:"state"`
	Identity *manifestIdentity `json:"identity,omitempty"`
	Parent   *manifestIdentity `json:"parent,omitempty"`
	SHA256   string            `json:"sha256,omitempty"`
	// RuleGroup records the group a user-scope GPU udev rule grants. It is
	// part of what makes the file this installation's own: a rule rewritten
	// for another group, or one written for the shared group, is not.
	RuleGroup string `json:"ruleGroup,omitempty"`
	// StagedPath and StagedSHA256 identify a fully written temporary file that
	// install recorded immediately before publishing it at Path. If the process
	// dies between publication and promoting the entry to owned, a retry may
	// adopt a file at Path only when its digest still matches this record.
	StagedPath   string `json:"stagedPath,omitempty"`
	StagedSHA256 string `json:"stagedSha256,omitempty"`
}

// hardwareSelection records exactly which optional integration files the
// installer installed.
type hardwareSelection struct {
	RAPL         bool
	GPURegisters bool
}

// buildManifest describes every path a 1.x installation owns in this scope.
// Every entry starts planned; install promotes an entry to owned only after it
// has created the file or directory with no-overwrite semantics.
func buildManifest(paths config.Paths, version string, hardware hardwareSelection) *manifest {
	m := &manifest{
		Marker:        manifestMarker,
		FormatVersion: manifestFormatVersion,
		Version:       version,
		Scope:         paths.Scope,
	}
	add := func(path, kind string) {
		m.Paths = append(m.Paths, manifestEntry{Path: path, Kind: kind, State: manifestPlanned})
	}
	add(paths.Binary, kindBinary)
	add(paths.PreviousBinary, kindPreviousBinary)
	add(paths.ConfigFile, kindConfig)
	add(paths.StateDir, kindStateDir)
	add(paths.CollectorDir(), kindCollectorState)
	add(paths.EnrollmentFile(), kindEnrollmentGrant)
	add(pairingBootstrapPath(paths), kindPairingBootstrap)
	add(ownerBootstrapPath(paths), kindOwnerBootstrap)
	add(paths.CredentialsFile(), kindCredentials)
	add(paths.UpgradeJournalFile(), kindUpgradeJournal)
	add(paths.StagingDir(), kindStagingDir)
	add(paths.BackupsDir(), kindBackupsDir)
	add(paths.BinaryDir, kindBinaryTemps)
	m.Units = []manifestEntry{
		{Path: paths.UnitPath(systemd.Hub.UnitName()), Kind: kindUnit, State: manifestPlanned},
		{Path: paths.UnitPath(systemd.Collector.UnitName()), Kind: kindUnit, State: manifestPlanned},
	}
	if paths.Scope == config.ScopeSystem {
		m.Paths = append(m.Paths, manifestEntry{Path: paths.ComponentStateDir(systemd.Hub.String()), Kind: kindComponentState, State: manifestPlanned})
		m.IntegrationFiles = append(m.IntegrationFiles, manifestEntry{Path: paths.SysusersFile, Kind: kindSysusers, State: manifestPlanned})
		if hardware.RAPL {
			m.IntegrationFiles = append(m.IntegrationFiles, manifestEntry{Path: paths.UdevRAPLFile, Kind: kindUdevRAPL, State: manifestPlanned})
		}
		if hardware.GPURegisters {
			m.IntegrationFiles = append(m.IntegrationFiles, manifestEntry{Path: paths.UdevGPUTempsFile, Kind: kindUdevGPU, State: manifestPlanned})
		}
	} else if hardware.GPURegisters {
		// A user installation may own or adopt the root-owned GPU rule so
		// uninstall knows whether it is Rigscope's to remove.
		m.IntegrationFiles = append(m.IntegrationFiles, manifestEntry{Path: paths.UdevGPUTempsFile, Kind: kindUdevGPU, State: manifestPlanned})
	}
	return m
}

// carryOwnership copies recorded ownership from the previous manifest into the
// freshly built one. Install then only has to re-record paths it actually
// rewrites, and an interrupted run keeps every already-owned entry owned.
func carryOwnership(wanted, previous *manifest) {
	if previous == nil {
		return
	}
	groups := [][]manifestEntry{wanted.Paths, wanted.Units, wanted.IntegrationFiles}
	for gi := range groups {
		for i := range groups[gi] {
			entry := &groups[gi][i]
			old := previous.entry(entry.Path, entry.Kind)
			if old == nil {
				continue
			}
			// The staged-publication record survives a crash and must reach
			// the retry's freshly built manifest even when the entry was never
			// promoted to owned.
			entry.StagedPath = old.StagedPath
			entry.StagedSHA256 = old.StagedSHA256
			if old.State == manifestAdopted {
				// An adopted root-owned file stays adopted until an install
				// rewrites it; a plain reinstall must not forget that Rigscope
				// did not create it.
				entry.State = manifestAdopted
			}
			if old.State != manifestOwned {
				continue
			}
			if wanted.Scope == config.ScopeUser && entry.Kind == kindUdevGPU && (old.RuleGroup == "" || old.RuleGroup == systemd.GPUGroup) {
				// In user scope only a rule for the user's own private group may
				// be owned; a system install owns its rigscope-gpu rule.
				// An entry without a recorded group predates the rule-group
				// field; a shared rule belongs to every account in the group.
				// Both are demoted so a reinstall starts from adopted and can
				// only reclaim a byte-identical private rule.
				entry.State = manifestAdopted
				continue
			}
			entry.State = manifestOwned
			entry.Identity = old.Identity
			entry.Parent = old.Parent
			entry.SHA256 = old.SHA256
			entry.RuleGroup = old.RuleGroup
		}
	}
}

// markPathAdopted records that Rigscope uses a file it did not write, such as
// the GPU rule a 0.2 installation left in place. Uninstall sees the state and
// leaves the file alone.
func markPathAdopted(m *manifest, paths config.Paths, path, kind string) error {
	entry := m.entry(path, kind)
	if entry == nil {
		return fmt.Errorf("internal error: %s is not part of the installation manifest", path)
	}
	entry.State = manifestAdopted
	entry.Identity = nil
	entry.Parent = nil
	entry.SHA256 = ""
	entry.RuleGroup = ""
	entry.StagedPath = ""
	entry.StagedSHA256 = ""
	return writeManifest(paths.ManifestFile(), m)
}

// expectedKindFor returns the one valid kind for a recorded path. Fixed layout
// paths come from expectedKinds; activation transaction temp files are allowed
// only directly inside the binary directory and only with a transaction temp
// name built from the installed binary's own base name.
func expectedKindFor(paths config.Paths, path string) (string, bool) {
	if kind, ok := expectedKinds(paths)[path]; ok {
		return kind, true
	}
	if filepath.Dir(path) == paths.BinaryDir && isBinaryTemp(filepath.Base(path), filepath.Base(paths.Binary)) {
		return kindBinaryTemp, true
	}
	return "", false
}

// expectedKinds maps every fixed path this scope may own to its one valid kind.
func expectedKinds(paths config.Paths) map[string]string {
	expected := map[string]string{
		paths.Binary:                                 kindBinary,
		paths.PreviousBinary:                         kindPreviousBinary,
		paths.ConfigFile:                             kindConfig,
		paths.StateDir:                               kindStateDir,
		paths.CollectorDir():                         kindCollectorState,
		paths.EnrollmentFile():                       kindEnrollmentGrant,
		pairingBootstrapPath(paths):                  kindPairingBootstrap,
		ownerBootstrapPath(paths):                    kindOwnerBootstrap,
		paths.CredentialsFile():                      kindCredentials,
		paths.UpgradeJournalFile():                   kindUpgradeJournal,
		paths.StagingDir():                           kindStagingDir,
		paths.BackupsDir():                           kindBackupsDir,
		paths.BinaryDir:                              kindBinaryTemps,
		paths.UnitPath(systemd.Hub.UnitName()):       kindUnit,
		paths.UnitPath(systemd.Collector.UnitName()): kindUnit,
	}
	if paths.Scope == config.ScopeSystem {
		expected[paths.ComponentStateDir(systemd.Hub.String())] = kindComponentState
		expected[paths.SysusersFile] = kindSysusers
		expected[paths.UdevRAPLFile] = kindUdevRAPL
		expected[paths.UdevGPUTempsFile] = kindUdevGPU
	} else if paths.UdevGPUTempsFile != "" {
		// User scope records only the optional root-owned GPU rule. The rule
		// file lives outside $HOME but is still this installation's to remove
		// when the manifest says Rigscope wrote it.
		expected[paths.UdevGPUTempsFile] = kindUdevGPU
	}
	return expected
}

// loadManifest reads and validates the ownership record. A missing file is
// reported as errManifestMissing; anything else that does not validate is a
// hard error so install never overwrites and uninstall never deletes.
func loadManifest(path string, paths config.Paths) (*manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errManifestMissing
		}
		return nil, fmt.Errorf("read installation manifest %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect installation manifest %s: %w", path, err)
	}
	if info.Size() > manifestMaxSize {
		return nil, fmt.Errorf("installation manifest %s is larger than %d bytes", path, manifestMaxSize)
	}
	data, err := io.ReadAll(io.LimitReader(file, manifestMaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("read installation manifest %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var m manifest
	if err := decoder.Decode(&m); err != nil {
		return nil, fmt.Errorf("installation manifest %s is not valid: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("installation manifest %s contains more than one JSON value", path)
	}
	if err := validateManifest(&m, paths); err != nil {
		return nil, fmt.Errorf("installation manifest %s is not valid: %w", path, err)
	}
	return &m, nil
}

func validateManifest(m *manifest, paths config.Paths) error {
	if m.Marker != manifestMarker {
		return fmt.Errorf("unrecognised marker %q", m.Marker)
	}
	if m.FormatVersion != manifestFormatVersion {
		return fmt.Errorf("unsupported format version %d", m.FormatVersion)
	}
	if strings.TrimSpace(m.Version) == "" {
		return errors.New("missing rigscope version")
	}
	if m.Scope != paths.Scope {
		return fmt.Errorf("manifest scope %q does not match the requested %q scope", m.Scope, paths.Scope)
	}
	seen := map[string]string{}
	hasStateDir := false
	for _, group := range [][]manifestEntry{m.Paths, m.Units, m.IntegrationFiles} {
		for _, entry := range group {
			if entry.Path == "" || !filepath.IsAbs(entry.Path) {
				return fmt.Errorf("path %q is not absolute", entry.Path)
			}
			if strings.ContainsAny(entry.Path, "\x00\r\n") {
				return fmt.Errorf("path %q contains control characters", entry.Path)
			}
			want, ok := expectedKindFor(paths, entry.Path)
			if !ok {
				return fmt.Errorf("path %s is not part of the %s layout", entry.Path, paths.Scope)
			}
			if entry.Kind != want {
				return fmt.Errorf("path %s has kind %q, want %q", entry.Path, entry.Kind, want)
			}
			if other, duplicate := seen[entry.Path]; duplicate {
				return fmt.Errorf("path %s is recorded twice (%s and %s)", entry.Path, other, entry.Kind)
			}
			seen[entry.Path] = entry.Kind
			switch entry.State {
			case manifestPlanned:
				if entry.Identity != nil {
					return fmt.Errorf("planned path %s must not record an identity", entry.Path)
				}
			case manifestOwned:
				if entry.Identity == nil {
					return fmt.Errorf("owned path %s does not record its identity", entry.Path)
				}
				if entry.Parent == nil {
					return fmt.Errorf("owned path %s does not record its parent directory", entry.Path)
				}
			case manifestAdopted:
				if entry.Identity != nil {
					return fmt.Errorf("adopted path %s must not record an identity", entry.Path)
				}
			default:
				return fmt.Errorf("path %s has invalid state %q", entry.Path, entry.State)
			}
			if err := validateStagedFields(entry); err != nil {
				return err
			}
			if entry.RuleGroup != "" {
				if entry.Kind != kindUdevGPU {
					return fmt.Errorf("path %s records a rule group but has kind %q", entry.Path, entry.Kind)
				}
				if entry.State != manifestOwned {
					return fmt.Errorf("%s path %s records a rule group", entry.State, entry.Path)
				}
			}
			if entry.Kind == kindStateDir {
				hasStateDir = true
			}
		}
	}
	if !hasStateDir {
		return errors.New("state directory is not recorded")
	}
	return nil
}

// validateStagedFields checks the record of a staged publication. The staged
// file must sit beside its target with the writer's transaction temp prefix,
// and the digest must be a lowercase SHA-256.
func validateStagedFields(entry manifestEntry) error {
	if entry.StagedPath == "" && entry.StagedSHA256 == "" {
		return nil
	}
	if entry.StagedPath == "" || entry.StagedSHA256 == "" {
		return fmt.Errorf("path %s records an incomplete staged publication", entry.Path)
	}
	if !filepath.IsAbs(entry.StagedPath) {
		return fmt.Errorf("staged path %q is not absolute", entry.StagedPath)
	}
	base := filepath.Base(entry.Path)
	if filepath.Dir(entry.StagedPath) != filepath.Dir(entry.Path) ||
		!strings.HasPrefix(filepath.Base(entry.StagedPath), "."+base+".stage-") {
		return fmt.Errorf("staged path %s does not belong beside %s", entry.StagedPath, entry.Path)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(entry.StagedSHA256) {
		return fmt.Errorf("staged path %s has an invalid SHA-256", entry.Path)
	}
	return nil
}

func writeManifest(path string, m *manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode installation manifest: %w", err)
	}
	data = append(data, '\n')
	if err := fsutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write installation manifest %s: %w", path, err)
	}
	return nil
}

// entry returns the recorded entry for path and kind, planned or owned.
func (m *manifest) entry(path, kind string) *manifestEntry {
	if m == nil {
		return nil
	}
	for i := range m.Paths {
		if m.Paths[i].Path == path && m.Paths[i].Kind == kind {
			return &m.Paths[i]
		}
	}
	for i := range m.Units {
		if m.Units[i].Path == path && m.Units[i].Kind == kind {
			return &m.Units[i]
		}
	}
	for i := range m.IntegrationFiles {
		if m.IntegrationFiles[i].Path == path && m.IntegrationFiles[i].Kind == kind {
			return &m.IntegrationFiles[i]
		}
	}
	return nil
}

// owns reports whether path is recorded with kind, regardless of state.
func (m *manifest) owns(path, kind string) bool {
	return m.entry(path, kind) != nil
}

// owned reports whether path was actually created by install and still records
// an identity.
func (m *manifest) owned(path, kind string) bool {
	entry := m.entry(path, kind)
	return entry != nil && entry.State == manifestOwned && entry.Identity != nil
}

func (m *manifest) entries(kind string) []manifestEntry {
	var found []manifestEntry
	for _, group := range [][]manifestEntry{m.Paths, m.Units, m.IntegrationFiles} {
		for _, entry := range group {
			if entry.Kind == kind {
				found = append(found, entry)
			}
		}
	}
	return found
}

func (m *manifest) unitEntries() []manifestEntry {
	return m.entries(kindUnit)
}

// unitRecorded returns the unit path for a component when the manifest
// records it.
func unitRecorded(m *manifest, paths config.Paths, component systemd.Component) bool {
	return m.owns(paths.UnitPath(component.UnitName()), kindUnit)
}

func identityFromFS(identity fsutil.Identity) *manifestIdentity {
	return &manifestIdentity{
		Dev: identity.Dev,
		Ino: identity.Ino,
		UID: identity.UID,
		GID: identity.GID,
	}
}

func (m *manifestIdentity) matches(identity fsutil.Identity) bool {
	return m != nil && m.Dev == identity.Dev && m.Ino == identity.Ino && m.UID == identity.UID && m.GID == identity.GID
}

func (m *manifestIdentity) sameFile(identity fsutil.Identity) bool {
	return m != nil && m.Dev == identity.Dev && m.Ino == identity.Ino
}
