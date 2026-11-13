package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/platform/lock"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// runResetPassword is the offline recovery command. It takes the same
// exclusive state lock the hub holds for its whole lifetime, so it never races
// a running hub. When the systemd hub holds that lock, the command asks to stop
// it, resets the password and starts the hub again.
func runResetPassword(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("reset-password", deps.Stderr)
	username := flags.String("user", "owner", "account to recover")
	configFlag := flags.String("config", "", "configuration file")
	stateFlag := flags.String("state-dir", "", "hub state directory")
	system := flags.Bool("system", false, "use system paths")
	noRestart := flags.Bool("no-restart", false, "leave the hub stopped after the reset")
	passwordStdin := flags.Bool("password-stdin", false, "read the new password from standard input")
	yes := addYesFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(deps.Stderr, "rigscope: reset-password: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *configFlag == "" && *stateFlag == "" {
		*system = deps.installedScope(*system) == config.ScopeSystem
	}
	if *system {
		if code, done := deps.ensureRoot(config.ScopeSystem); done {
			return code
		}
	}
	_, stateDir, err := hubPaths(deps, *configFlag, *stateFlag, *system)
	if err != nil {
		return deps.errorf("reset-password: %v", err)
	}
	databasePath := filepath.Join(stateDir, hubDatabaseFile)
	if _, err := os.Stat(databasePath); err != nil {
		if os.IsNotExist(err) {
			return deps.errorf("reset-password: hub database does not exist: %s", databasePath)
		}
		return deps.errorf("reset-password: inspect %s: %v", databasePath, err)
	}
	// Ask for the new password before anything stops, so a typo or a
	// cancelled prompt leaves the hub running untouched.
	choice, err := deps.choosePassword(*username, *passwordStdin, *yes)
	if err != nil {
		return deps.errorf("reset-password: %v", err)
	}

	held, err := lock.Acquire(hubLeasePath(stateDir))
	var stopped []systemd.Component
	manager := deps.manager(scopeFrom(*system))
	if errors.Is(err, lock.ErrBusy) {
		// The lease is busy. If the installed hub service is the holder, stop
		// it with the operator's approval; anything else is a hub this command
		// cannot manage.
		running := activeComponents(ctx, manager, []systemd.Component{systemd.Hub})
		if len(running) == 0 {
			return deps.errorf("reset-password: a hub outside the %s unit holds %s (for example rigscope hub in a terminal); stop it, then run this again", systemd.Hub.UnitName(), hubLeasePath(stateDir))
		}
		effect := "stop while the password is reset, then start again"
		if *noRestart {
			effect = "stop and stay stopped"
		}
		if err := deps.approveServiceStop(running, effect, *yes); err != nil {
			return deps.errorf("reset-password: %v", err)
		}
		if err := manager.Stop(ctx, systemd.Hub.UnitName()); err != nil {
			return deps.errorf("reset-password: stop %s: %v", systemd.Hub, err)
		}
		stopped = running
		held, err = lock.Acquire(hubLeasePath(stateDir))
		if errors.Is(err, lock.ErrBusy) {
			code := deps.errorf("reset-password: another process still holds %s after the hub stopped", hubLeasePath(stateDir))
			resumeAfterReset(ctx, deps, manager, stopped, *noRestart)
			return code
		}
	}
	if err != nil {
		code := deps.errorf("reset-password: %v", err)
		resumeAfterReset(ctx, deps, manager, stopped, *noRestart)
		return code
	}

	err = resetPassword(ctx, databasePath, *username, choice.password)
	_ = held.Release()
	if err != nil {
		code := deps.errorf("reset-password: %v", err)
		resumeAfterReset(ctx, deps, manager, stopped, *noRestart)
		return code
	}
	if choice.chosen {
		fmt.Fprintf(deps.Stdout, "%s for %s; every browser session was signed out.\n", deps.out(styleGreen+";"+styleBold, "Password changed"), *username)
	} else {
		fmt.Fprintf(deps.Stdout, "Username: %s\nPassword: %s\n%s\n", *username, deps.out(styleBold, choice.password), deps.out(styleYellow, "Save this password; it is shown once."))
	}
	if !resumeAfterReset(ctx, deps, manager, stopped, *noRestart) {
		return 1
	}
	return 0
}

// resumeAfterReset starts the hub again when this command stopped it. It
// reports false when the hub was meant to start and could not.
func resumeAfterReset(ctx context.Context, deps Deps, manager systemd.Manager, stopped []systemd.Component, noRestart bool) bool {
	if len(stopped) == 0 {
		return true
	}
	if noRestart {
		fmt.Fprintf(deps.Stdout, "  %s hub (start it with rigscope start hub)\n", deps.out(styleYellow, "stopped:"))
		return true
	}
	if err := deps.resumeComponents(ctx, manager, stopped); err != nil {
		deps.errorf("reset-password: %v; start it with rigscope start hub", err)
		return false
	}
	return true
}

// resetPassword replaces one account's password and signs out its sessions.
// The caller holds the hub lease.
func resetPassword(ctx context.Context, databasePath, username, password string) error {
	database, err := storage.Open(ctx, databasePath, storage.Options{})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = database.Close() }()

	users := storage.NewUsers(database)
	if _, err := users.Get(ctx, username); err != nil {
		if storage.CodeOf(err) == storage.CodeNotFound {
			return fmt.Errorf("user %q does not exist", username)
		}
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	// The hash update and the session revocation commit together: a recovery
	// that succeeds can never leave a browser sign-in that survived the stop
	// valid, and a failed revocation never leaves the password changed.
	if _, err := users.ResetPassword(ctx, "local-recovery", username, hash); err != nil {
		return err
	}
	return nil
}
