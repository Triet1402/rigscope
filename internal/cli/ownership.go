package cli

import (
	"fmt"

	"github.com/Triet1402/rigscope/internal/activation"
	"github.com/Triet1402/rigscope/internal/config"
)

// manifestOwnership is the activation transaction's narrow view of the
// installation manifest. The transaction holds the shared installation lock
// while it records and switches phases, so every rewrite is serialised with
// install and uninstall.
type manifestOwnership struct{ paths config.Paths }

// Record implements activation.Ownership.
func (o manifestOwnership) Record(path string, kind activation.ArtifactKind, sha256 string) error {
	manifestKind, err := artifactManifestKind(kind)
	if err != nil {
		return err
	}
	current, err := loadManifest(o.paths.ManifestFile(), o.paths)
	if err != nil {
		return fmt.Errorf("record %s in the installation manifest: %w", path, err)
	}
	entry := current.entry(path, manifestKind)
	if entry == nil {
		if manifestKind != kindBinaryTemp {
			return fmt.Errorf("%s is not part of the %s installation layout", path, o.paths.Scope)
		}
		if expected, ok := expectedKindFor(o.paths, path); !ok || expected != manifestKind {
			return fmt.Errorf("%s is not a valid transaction temp path for %s", path, o.paths.Binary)
		}
		current.Paths = append(current.Paths, manifestEntry{Path: path, Kind: manifestKind, State: manifestPlanned})
		entry = &current.Paths[len(current.Paths)-1]
	}
	if err := recordPathOwned(current, o.paths, entry, path); err != nil {
		return err
	}
	if sha256 != "" && entry.SHA256 != sha256 {
		return fmt.Errorf("record %s: content changed while it was being recorded", path)
	}
	return nil
}

// Forget implements activation.Ownership.
func (o manifestOwnership) Forget(path string, kind activation.ArtifactKind) error {
	manifestKind, err := artifactManifestKind(kind)
	if err != nil {
		return err
	}
	current, err := loadManifest(o.paths.ManifestFile(), o.paths)
	if err != nil {
		return fmt.Errorf("forget %s in the installation manifest: %w", path, err)
	}
	entry := current.entry(path, manifestKind)
	if entry == nil {
		return nil
	}
	entry.State = manifestPlanned
	entry.Identity = nil
	entry.Parent = nil
	entry.SHA256 = ""
	entry.StagedPath = ""
	entry.StagedSHA256 = ""
	if err := writeManifest(o.paths.ManifestFile(), current); err != nil {
		return err
	}
	return nil
}

func artifactManifestKind(kind activation.ArtifactKind) (string, error) {
	switch kind {
	case activation.ArtifactPreviousBinary:
		return kindPreviousBinary, nil
	case activation.ArtifactUpgradeJournal:
		return kindUpgradeJournal, nil
	case activation.ArtifactStagingDir:
		return kindStagingDir, nil
	case activation.ArtifactBackupsDir:
		return kindBackupsDir, nil
	case activation.ArtifactCollectorDir:
		return kindCollectorState, nil
	case activation.ArtifactBinaryTemp:
		return kindBinaryTemp, nil
	default:
		return "", fmt.Errorf("unknown activation artifact kind %q", kind)
	}
}
