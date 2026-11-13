package systemd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Result is one completed command invocation. ExitCode is meaningful even when
// Err is nil: a failing systemctl predicate such as is-active exits nonzero
// without being a harness failure.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes a command. Err is reserved for failures to run the command at
// all; a nonzero exit is reported through Result.ExitCode.
type Runner func(ctx context.Context, name string, args ...string) (Result, error)

// Manager controls systemd units for one scope.
type Manager interface {
	DaemonReload(ctx context.Context) error
	Enable(ctx context.Context, unit string) error
	Disable(ctx context.Context, unit string) error
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
	IsActive(ctx context.Context, unit string) (bool, error)
	// UnitRunning reports whether the unit has finished starting and is
	// actually running: ActiveState=active with SubState=running. Readiness
	// uses it; deletion paths keep the conservative IsActive, which counts
	// transition states as active. None of Rigscope's units is a oneshot, so a
	// completed-then-exited unit is a failure for readiness, not success.
	UnitRunning(ctx context.Context, unit string) (bool, error)
	// UnitKnown reports whether the manager knows the unit at all. Uninstall
	// uses it to decide whether a recorded unit with a missing file still
	// needs stopping: systemd keeps a loaded unit until it is stopped, even
	// when its file was deleted.
	UnitKnown(ctx context.Context, unit string) (bool, error)
	EnableLinger(ctx context.Context) error
}

// Exec is the systemctl/loginctl-backed Manager.
type Exec struct {
	scope Scope
	run   Runner
}

// NewExec returns a Manager that invokes the real systemctl and loginctl.
func NewExec(scope Scope) *Exec {
	return &Exec{scope: scope, run: execRunner}
}

// NewExecWithRunner returns an Exec Manager with an injected runner.
func NewExecWithRunner(scope Scope, run Runner) *Exec {
	if run == nil {
		run = execRunner
	}
	return &Exec{scope: scope, run: run}
}

func (e *Exec) DaemonReload(ctx context.Context) error {
	_, err := e.systemctl(ctx, "daemon-reload", "")
	return err
}

func (e *Exec) Enable(ctx context.Context, unit string) error {
	_, err := e.systemctl(ctx, "enable", unit)
	return err
}

func (e *Exec) Disable(ctx context.Context, unit string) error {
	_, err := e.systemctl(ctx, "disable", unit)
	return err
}

func (e *Exec) Start(ctx context.Context, unit string) error {
	_, err := e.systemctl(ctx, "start", unit)
	return err
}

func (e *Exec) Stop(ctx context.Context, unit string) error {
	_, err := e.systemctl(ctx, "stop", unit)
	return err
}

func (e *Exec) Restart(ctx context.Context, unit string) error {
	_, err := e.systemctl(ctx, "restart", unit)
	return err
}

// IsActive reports whether start is not needed. It never treats a failed
// systemctl invocation as "inactive": a unit is only inactive when systemd
// says so explicitly (ActiveState=inactive or failed). A bus, permission or
// protocol failure is returned as an error so callers such as uninstall abort
// instead of deleting data behind a service that may still be running.
func (e *Exec) IsActive(ctx context.Context, unit string) (bool, error) {
	state, err := e.unitState(ctx, unit)
	if err != nil {
		return false, err
	}
	switch state.active {
	case "active", "reloading", "activating", "deactivating":
		return true, nil
	case "inactive", "failed":
		return false, nil
	default:
		return false, fmt.Errorf("systemctl reported unexpected ActiveState %q for %s", state.active, unit)
	}
}

// UnitRunning reports whether the unit finished starting and is running.
// Readiness must not accept activating, deactivating, reloading or exited: a
// unit in any of those states has not proved that the new binary serves
// requests. Every Rigscope daemon unit stays active/running for its lifetime;
// a service that exits is a failure even though systemd calls it active.
func (e *Exec) UnitRunning(ctx context.Context, unit string) (bool, error) {
	state, err := e.unitState(ctx, unit)
	if err != nil {
		return false, err
	}
	switch state.active {
	case "active":
		return state.sub == "running", nil
	case "inactive", "failed", "activating", "deactivating", "reloading":
		return false, nil
	default:
		return false, fmt.Errorf("systemctl reported unexpected ActiveState %q for %s", state.active, unit)
	}
}

// UnitKnown reports whether systemd has a load state for unit. A deleted unit
// file remains loaded until it is stopped, so uninstall must ask the manager
// instead of deciding from the filesystem. Only an explicit LoadState=not-found
// is unknown; communication or permission failures are errors.
func (e *Exec) UnitKnown(ctx context.Context, unit string) (bool, error) {
	state, err := e.unitState(ctx, unit)
	if err != nil {
		return false, err
	}
	return state.load != "not-found", nil
}

// unitState is the precise answer to "does systemd know this unit and is it
// running". It comes from one systemctl show invocation so the two answers are
// consistent with each other.
type unitState struct {
	load   string
	active string
	sub    string
}

// unitState parses `systemctl show -p LoadState,ActiveState,SubState --value`.
// A nonzero exit is never read as a state: it means systemctl could not talk to
// the manager (no bus, no permission, unknown option) and must stop a
// destructive operation.
func (e *Exec) unitState(ctx context.Context, unit string) (unitState, error) {
	args := make([]string, 0, 6)
	if e.scope == User {
		args = append(args, "--user")
	}
	args = append(args, "show", "-p", "LoadState,ActiveState,SubState", "--value", unit)
	result, err := e.run(ctx, "systemctl", args...)
	if err != nil {
		return unitState{}, fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	if result.ExitCode != 0 {
		return unitState{}, fmt.Errorf("systemctl %s failed: %s", strings.Join(args, " "), commandError(result))
	}
	state, err := parseUnitState(result.Stdout)
	if err != nil {
		return unitState{}, fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return state, nil
}

// parseUnitState accepts both the --value form (three bare lines) and the
// key=value form, so a systemd that ignores --value still parses correctly.
func parseUnitState(output string) (unitState, error) {
	values := map[string]string{}
	var positional []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
			continue
		}
		positional = append(positional, line)
	}
	if len(values) == 0 {
		for index, key := range []string{"LoadState", "ActiveState", "SubState"} {
			if index < len(positional) {
				values[key] = positional[index]
			}
		}
	}
	state := unitState{load: values["LoadState"], active: values["ActiveState"], sub: values["SubState"]}
	if state.load == "" || state.active == "" {
		return unitState{}, fmt.Errorf("unexpected systemctl show output %q", strings.TrimSpace(output))
	}
	return state, nil
}

// EnableLinger enables boot startup for the invoking user. It is only
// meaningful for user-scope installs.
func (e *Exec) EnableLinger(ctx context.Context) error {
	if e.scope == System {
		return nil
	}
	result, err := e.run(ctx, "loginctl", "enable-linger")
	if err != nil {
		return fmt.Errorf("loginctl enable-linger: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("loginctl enable-linger failed: %s", commandError(result))
	}
	return nil
}

func (e *Exec) systemctl(ctx context.Context, verb, unit string) (Result, error) {
	args := make([]string, 0, 3)
	if e.scope == User {
		args = append(args, "--user")
	}
	args = append(args, verb)
	if unit != "" {
		args = append(args, unit)
	}
	result, err := e.run(ctx, "systemctl", args...)
	if err != nil {
		return result, fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("systemctl %s failed: %s", strings.Join(args, " "), commandError(result))
	}
	return result, nil
}

func commandError(result Result) string {
	text := strings.TrimSpace(result.Stderr)
	if text == "" {
		text = strings.TrimSpace(result.Stdout)
	}
	if text == "" {
		text = fmt.Sprintf("exit %d", result.ExitCode)
	}
	return text
}

func execRunner(ctx context.Context, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}
