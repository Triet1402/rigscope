package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// enrollmentGrantFile is the transient one-time grant passed with
// `install --hub URL --token TOKEN`. It is deliberately separate from the
// configuration: future enrollment consumes and deletes it, and the issued
// credential is written to credentials.json instead.
type enrollmentGrantFile struct {
	Version int    `json:"version"`
	URL     string `json:"url"`
	Grant   string `json:"grant"`
}

const enrollmentGrantVersion = 1

// writeEnrollmentGrant stores the grant in the collector-private directory. In
// system scope the collector account owns it, so the hub account (a member of
// the shared group only) cannot read it.
//
// The file's ownership is set on the staged temporary file through its
// descriptor, before the atomic rename. Chowning the final pathname would let
// the collector (which owns the directory) swap enrollment.json for a symlink
// and redirect root's chown to an arbitrary file.
func writeEnrollmentGrant(deps Deps, paths config.Paths, m *manifest, url, grant string) error {
	dir := paths.CollectorDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create collector directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("set mode on collector directory %s: %w", dir, err)
	}
	payload, err := json.MarshalIndent(enrollmentGrantFile{
		Version: enrollmentGrantVersion,
		URL:     url,
		Grant:   grant,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode enrollment grant: %w", err)
	}
	payload = append(payload, '\n')
	var prepare func(*os.File) error
	if paths.Scope == config.ScopeSystem {
		if err := deps.chown(dir, systemd.CollectorUser, systemd.SharedGroup); err != nil {
			return fmt.Errorf("set ownership of %s: %w", dir, err)
		}
		prepare = func(file *os.File) error {
			return deps.chownFile(file, systemd.CollectorUser, systemd.SharedGroup)
		}
	}
	return writeManifestedFile(deps, paths, m, paths.EnrollmentFile(), kindEnrollmentGrant, payload, 0o600, prepare)
}
