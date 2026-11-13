package activation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Triet1402/rigscope/internal/platform/lock"
)

// DefaultMigrateTimeout bounds one staged migration command.
const DefaultMigrateTimeout = 5 * time.Minute

// maxMigrateOutput bounds captured stdout and stderr; anything beyond it is
// discarded and reported.
const maxMigrateOutput = 64 << 10

// MigrateRequest describes one run of the staged binary's migrate command.
type MigrateRequest struct {
	// Binary is the staged executable, never the installed one.
	Binary     string
	ConfigFile string
	StateDir   string
	From       string
	To         string
	// System selects the root-owned system installation in the child, so it
	// enforces root authorization and resolves the system lock path.
	System bool
	// LockFile is the descriptor of the installation lock the parent holds. It
	// is passed as an inherited file; the child verifies the inode and re-locks
	// the same open file description before skipping its own acquisition.
	LockFile *os.File
	// HubLockFile is the descriptor of the hub runtime lease the parent holds
	// across the backup and migration. It is passed the same way; the child
	// verifies it and skips its own acquisition instead of refusing its parent.
	HubLockFile *os.File
}

// MigrateResult is the JSON contract printed by "rigscope migrate".
type MigrateResult struct {
	// Schema is the schema version after migration.
	Schema int `json:"schema"`
	// CompatibleDown is the floor: the lowest schema version whose binaries
	// can safely open and write the migrated data. An additive migration keeps
	// the floor; a breaking migration to version N raises it to N. Rollback to
	// a binary with schema P is allowed iff P >= CompatibleDown.
	CompatibleDown int `json:"compatibleDown"`
}

// Migrator runs migrations with the staged binary.
type Migrator interface {
	Migrate(ctx context.Context, req MigrateRequest) (MigrateResult, error)
}

// ExecMigrator runs `<staged> migrate --from ... --to ...`.
type ExecMigrator struct {
	// Run executes the prepared command and returns its standard output. A nil
	// Run uses os/exec with bounded output and a process-group deadline.
	Run func(ctx context.Context, command *exec.Cmd) ([]byte, error)
	// Timeout bounds one migration. Zero selects DefaultMigrateTimeout.
	Timeout time.Duration
}

// NewExecMigrator returns an ExecMigrator using the real process runner.
func NewExecMigrator() *ExecMigrator { return &ExecMigrator{} }

// Migrate implements Migrator.
func (m *ExecMigrator) Migrate(ctx context.Context, req MigrateRequest) (MigrateResult, error) {
	if req.Binary == "" {
		return MigrateResult{}, errors.New("migrate: staged binary is required")
	}
	args := []string{"migrate"}
	if req.From != "" {
		args = append(args, "--from", req.From)
	}
	if req.To != "" {
		args = append(args, "--to", req.To)
	}
	if req.ConfigFile != "" {
		args = append(args, "--config", req.ConfigFile)
	}
	if req.StateDir != "" {
		args = append(args, "--state", req.StateDir)
	}
	if req.System {
		args = append(args, "--system")
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = DefaultMigrateTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, req.Binary, args...)
	// cmd.ExtraFiles puts the locked descriptors at fd 3 onwards; the
	// environment names each one so the child can verify it before skipping an
	// acquisition of its own.
	nextFD := 3
	var inherited []string
	if req.LockFile != nil {
		command.ExtraFiles = append(command.ExtraFiles, req.LockFile)
		inherited = append(inherited, fmt.Sprintf("%s=%d", lock.InheritedLockFDEnv, nextFD))
		nextFD++
	}
	if req.HubLockFile != nil {
		command.ExtraFiles = append(command.ExtraFiles, req.HubLockFile)
		inherited = append(inherited, fmt.Sprintf("%s=%d", lock.InheritedHubLockFDEnv, nextFD))
	}
	if len(inherited) > 0 {
		command.Env = append(os.Environ(), inherited...)
	}
	run := m.Run
	if run == nil {
		run = func(_ context.Context, prepared *exec.Cmd) ([]byte, error) {
			return runBounded(prepared)
		}
	}
	output, err := run(commandCtx, command)
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		return MigrateResult{}, fmt.Errorf("staged migrate timed out after %s", timeout)
	}
	if err != nil {
		return MigrateResult{}, fmt.Errorf("run staged migrate: %w", err)
	}
	var result MigrateResult
	decoder := json.NewDecoder(bytes.NewReader(output))
	if err := decoder.Decode(&result); err != nil {
		return MigrateResult{}, fmt.Errorf("parse migrate output: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return MigrateResult{}, fmt.Errorf("parse migrate output: %w", err)
	}
	if result.Schema < 0 || result.CompatibleDown < 0 {
		return MigrateResult{}, fmt.Errorf("migrate reported invalid schema %d / compatibleDown %d", result.Schema, result.CompatibleDown)
	}
	return result, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("migrate wrote more than one JSON value")
	}
	return err
}

func runBounded(command *exec.Cmd) ([]byte, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	stdout := &boundedOutput{limit: maxMigrateOutput}
	stderr := &boundedOutput{limit: maxMigrateOutput}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		if stdout.overflow || stderr.overflow {
			message += fmt.Sprintf(" (output exceeded %d bytes and was truncated)", maxMigrateOutput)
		}
		return nil, errors.New(message)
	}
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("migrate output exceeded %d bytes and was truncated", maxMigrateOutput)
	}
	return stdout.Bytes(), nil
}

// boundedOutput keeps at most limit bytes and records that more arrived.
type boundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (w *boundedOutput) Write(data []byte) (int, error) {
	written := len(data)
	if w.buffer.Len() < w.limit {
		remaining := w.limit - w.buffer.Len()
		if len(data) > remaining {
			w.buffer.Write(data[:remaining])
			w.overflow = true
		} else {
			w.buffer.Write(data)
		}
	} else if len(data) > 0 {
		w.overflow = true
	}
	return written, nil
}

func (w *boundedOutput) String() string { return w.buffer.String() }

func (w *boundedOutput) Bytes() []byte { return w.buffer.Bytes() }
