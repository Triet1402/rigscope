package activation

// ArtifactKind names one path the activation transaction creates inside an
// existing installation layout. The CLI maps each value onto its manifest kind
// and records the path's identity and, for regular files, its digest. Uninstall
// deletes only paths recorded this way; a reserved filename alone is never
// proof of ownership.
type ArtifactKind string

const (
	// ArtifactPreviousBinary is the retained <binary>.prev file.
	ArtifactPreviousBinary ArtifactKind = "previous-binary"
	// ArtifactUpgradeJournal is the durable transaction record.
	ArtifactUpgradeJournal ArtifactKind = "upgrade-journal"
	// ArtifactStagingDir is the verified-release staging directory.
	ArtifactStagingDir ArtifactKind = "staging-dir"
	// ArtifactBackupsDir is the pre-upgrade backup root.
	ArtifactBackupsDir ArtifactKind = "backups-dir"
	// ArtifactCollectorDir is the collector-private state directory. It is
	// recorded again after a backup restore replaces its inode.
	ArtifactCollectorDir ArtifactKind = "collector-state"
	// ArtifactBinaryTemp is one transaction temp file beside the binary.
	ArtifactBinaryTemp ArtifactKind = "binary-temp"
)

// Ownership records activation-created artifacts in the installation manifest.
// Implementations must run under the shared installation lock and must record
// the current filesystem identity, refusing to claim a path they did not
// create. A nil Ownership disables recording (tests that do not exercise
// uninstall ownership).
type Ownership interface {
	// Record promotes path to owned with its current identity. SHA256 is the
	// expected digest for a regular file; an empty value means "hash it now".
	Record(path string, kind ArtifactKind, sha256 string) error
	// Forget stops claiming path. It is used when a transaction temp file is
	// removed by recovery or cleanup.
	Forget(path string, kind ArtifactKind) error
}
