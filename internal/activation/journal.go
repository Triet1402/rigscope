package activation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Phase is one state of the upgrade journal. The ordered success path is
// locked -> staged -> quiesced -> backed-up -> migrated -> prepared ->
// previous-published -> activated -> verified -> done. Interrupted and failed
// transactions use rolling-back, the rollback-* substates, rolled-back and
// failed-needs-attention.
type Phase string

const (
	PhaseLocked   Phase = "locked"
	PhaseStaged   Phase = "staged"
	PhaseQuiesced Phase = "quiesced"
	PhaseBackedUp Phase = "backed-up"
	PhaseMigrated Phase = "migrated"
	PhasePrepared Phase = "prepared"
	// PhasePreviousPublished records that the installed binary's retained
	// copy (.prev) was durably published and verified, before the target swap.
	// A crash here leaves the old release installed and an owned .prev.
	PhasePreviousPublished Phase = "previous-published"
	PhaseActivated         Phase = "activated"
	PhaseVerified          Phase = "verified"
	PhaseDone              Phase = "done"
	PhaseRollingBack       Phase = "rolling-back"
	// Rollback substates are written in order. Recovery resumes from the
	// recorded substate and never replays the data snapshot once writers have
	// been allowed to run.
	PhaseRollbackQuiesced       Phase = "rollback-quiesced"
	PhaseRollbackDataRestored   Phase = "rollback-data-restored"
	PhaseRollbackBinaryRestored Phase = "rollback-binary-restored"
	PhaseRollbackWritersStarted Phase = "rollback-writers-started"
	PhaseRolledBack             Phase = "rolled-back"
	PhaseFailedNeedsAttention   Phase = "failed-needs-attention"
)

// journalFormat is the schema version of upgrade-journal.json. Format 2 adds
// the persisted layout/settings and binary identities recovery needs.
const journalFormat = 2

// Event records one phase transition for diagnostics.
type Event struct {
	At    time.Time `json:"at"`
	Phase Phase     `json:"phase"`
	Note  string    `json:"note,omitempty"`
}

// Journal is the durable record of one upgrade transaction. It is written
// atomically after every phase so a crash leaves an accurate, actionable
// description of what happened. It carries everything recovery needs — paths,
// service settings and binary identities — so a migrated or invalid
// configuration file cannot block recovery.
type Journal struct {
	Format       int       `json:"format"`
	ID           string    `json:"id"`
	Phase        Phase     `json:"phase"`
	From         string    `json:"fromVersion"`
	To           string    `json:"toVersion"`
	StartedAt    time.Time `json:"startedAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	StagedBinary string    `json:"stagedBinary,omitempty"`
	StagedDir    string    `json:"stagedDir,omitempty"`
	BackupDir    string    `json:"backupDir,omitempty"`

	// Layout and Settings are the exact paths and service contract this
	// transaction used. Recovery reads them from the journal instead of
	// decoding configuration.
	Layout   Layout   `json:"layout"`
	Settings Settings `json:"settings"`
	// Units is the enabled unit start order recorded when the transaction
	// began. Recovery uses it instead of re-deriving names from configuration.
	Units []string `json:"units,omitempty"`

	// PreviousSchema is the configuration/data schema version read before the
	// staged binary migrated it. Schema is what the staged binary wrote.
	PreviousSchema int `json:"previousSchema,omitempty"`
	Schema         int `json:"schema,omitempty"`
	// CompatibleDown is the floor reported by the staged release: the lowest
	// schema whose binaries can use the migrated data. Rollback after writes
	// is allowed only when PreviousSchema >= CompatibleDown.
	CompatibleDown int `json:"compatibleDown,omitempty"`

	// PreparedBinary is <target>.<id>.new. NewBinarySHA256 and
	// PreviousBinarySHA256 identify the prepared release and the binary it
	// replaced, independently of the staging directory.
	PreparedBinary       string `json:"preparedBinary,omitempty"`
	NewBinarySHA256      string `json:"newBinarySha256,omitempty"`
	PreviousBinaryPath   string `json:"previousBinaryPath,omitempty"`
	PreviousBinarySHA256 string `json:"previousBinarySha256,omitempty"`

	// ServicesStopped records that quiescing began, even if it did not finish.
	ServicesStopped bool `json:"servicesStopped,omitempty"`
	// BackupComplete records that the backup manifest was durably written.
	BackupComplete bool `json:"backupComplete,omitempty"`
	// PublishStarted records that binary publication began, even if it did not
	// finish. Recovery uses the recorded digests, never the staging directory,
	// to finish or undo it.
	PublishStarted bool `json:"publishStarted,omitempty"`
	// PreviousPublished records that .prev was durably published and verified
	// by this transaction before the target swap. A crash between that
	// publication and the swap leaves an owned .prev; recovery claims it from
	// PreviousBinarySHA256 even when the phase was not saved.
	PreviousPublished bool `json:"previousPublished,omitempty"`
	// AcceptedWrites is journaled before the first start of the new version. A
	// crash after this point never restores the pre-upgrade data snapshot.
	AcceptedWrites bool `json:"acceptedWrites,omitempty"`

	Error   string  `json:"error,omitempty"`
	History []Event `json:"history,omitempty"`
}

// rollbackRank orders the rollback substates.
func rollbackRank(p Phase) int {
	switch p {
	case PhaseRollingBack:
		return 1
	case PhaseRollbackQuiesced:
		return 2
	case PhaseRollbackDataRestored:
		return 3
	case PhaseRollbackBinaryRestored:
		return 4
	case PhaseRollbackWritersStarted:
		return 5
	case PhaseRolledBack:
		return 6
	default:
		return 0
	}
}

func rollbackAtLeast(p, target Phase) bool { return rollbackRank(p) >= rollbackRank(target) }

// startOrder returns the enabled units in activation order, preferring the
// names persisted by the transaction.
func (j *Journal) startOrder() []string {
	if len(j.Units) > 0 {
		return j.Units
	}
	return j.Settings.Components()
}

// stopOrder returns the persisted units in reverse activation order.
func (j *Journal) stopOrder() []string {
	if len(j.Units) == 0 {
		return j.Settings.StopOrder()
	}
	units := make([]string, len(j.Units))
	for index, unit := range j.Units {
		units[len(j.Units)-1-index] = unit
	}
	return units
}

// ErrNoJournal is returned when no upgrade journal exists.
var ErrNoJournal = errors.New("no upgrade journal")

// JournalStore loads and atomically saves the journal.
type JournalStore struct {
	Path string
	FS   FS
}

// Load reads the journal. A missing file returns ErrNoJournal.
func (s JournalStore) Load() (*Journal, error) {
	data, err := s.FS.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoJournal
		}
		return nil, fmt.Errorf("read upgrade journal: %w", err)
	}
	var journal Journal
	if err := json.Unmarshal(data, &journal); err != nil {
		return nil, fmt.Errorf("parse upgrade journal %s: %w", s.Path, err)
	}
	if journal.Format != journalFormat {
		return nil, fmt.Errorf("upgrade journal %s has unsupported format %d", s.Path, journal.Format)
	}
	if journal.Phase == "" {
		return nil, fmt.Errorf("upgrade journal %s has no phase", s.Path)
	}
	return &journal, nil
}

// Save writes the journal atomically with mode 0600.
func (s JournalStore) Save(journal *Journal) error {
	journal.Format = journalFormat
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode upgrade journal: %w", err)
	}
	data = append(data, '\n')
	if err := s.FS.MkdirAll(dirOf(s.Path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := s.FS.WriteFileAtomic(s.Path, data, 0o600); err != nil {
		return fmt.Errorf("write upgrade journal: %w", err)
	}
	return nil
}

// Remove deletes the journal. A missing journal is not an error.
func (s JournalStore) Remove() error {
	if err := s.FS.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
