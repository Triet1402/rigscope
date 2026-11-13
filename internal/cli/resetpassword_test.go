package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/platform/lock"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

func resetFixture(t *testing.T) (Deps, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "config.json")
	cfg := config.Default()
	cfg.Components.Hub.Listen = "127.0.0.1"
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	deps := Deps{Stdout: &stdout, Stderr: &stderr}
	return deps, configPath, stateDir, filepath.Join(stateDir, hubDatabaseFile)
}

func seedHubDatabase(t *testing.T, databasePath string) *storage.Database {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(context.Background(), databasePath, storage.Options{})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	users := storage.NewUsers(db)
	if err := users.Create(context.Background(), "test", storage.User{
		Username: "owner", PasswordHash: "test:old-password", Scope: storage.AllMachines(), Admin: true,
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := storage.NewSessions(db).Create(context.Background(), storage.Session{
		TokenHash: auth.HashToken("stale-token"),
		Username:  "owner",
		CSRFToken: "csrf",
		Scope:     storage.AllMachines(),
		Admin:     true,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return db
}

func TestResetPasswordPrintsOnceAndRevokesSessions(t *testing.T) {
	deps, configPath, stateDir, databasePath := resetFixture(t)
	db := seedHubDatabase(t, databasePath)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	code := runResetPassword(context.Background(), deps, []string{"--config", configPath, "--state-dir", stateDir})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, deps.Stderr.(*bytes.Buffer).String())
	}
	output := deps.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(output, "Username: owner\nPassword: ") ||
		!strings.Contains(output, "Save this password; it is shown once.") {
		t.Fatalf("output = %q", output)
	}
	password := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Password: ") {
			password = strings.TrimPrefix(line, "Password: ")
		}
	}
	if password == "" {
		t.Fatalf("no password in %q", output)
	}

	reopened, err := storage.Open(context.Background(), databasePath, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	user, err := storage.NewUsers(reopened).Get(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := auth.VerifyPassword(password, user.PasswordHash); err != nil || !ok {
		t.Fatalf("new password does not verify: %v", err)
	}
	if ok, _ := auth.VerifyPassword("old-password", user.PasswordHash); ok {
		t.Fatal("old password still verifies")
	}
	count, err := storage.NewSessions(reopened).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("recovery left %d sessions", count)
	}
}

// lockHoldingHub is a fake systemd whose hub unit holds the hub lease until
// it is stopped, like the real service.
type lockHoldingHub struct {
	*systemd.Fake
	held *lock.Lock
}

func (m *lockHoldingHub) Stop(ctx context.Context, unit string) error {
	if err := m.Fake.Stop(ctx, unit); err != nil {
		return err
	}
	if unit == systemd.Hub.UnitName() && m.held != nil {
		_ = m.held.Release()
		m.held = nil
	}
	return nil
}

func runningHubFixture(t *testing.T) (Deps, *lockHoldingHub, *[]string, []string) {
	t.Helper()
	deps, configPath, stateDir, databasePath := resetFixture(t)
	db := seedHubDatabase(t, databasePath)
	_ = db.Close()
	held, err := lock.Acquire(filepath.Join(stateDir, hubLockFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })
	fake := systemd.NewFake()
	fake.Active[systemd.Hub.UnitName()] = true
	manager := &lockHoldingHub{Fake: fake, held: held}
	deps.NewManager = func(config.Scope) systemd.Manager { return manager }
	prompts := &[]string{}
	deps.Confirm = func(question string) (bool, error) {
		*prompts = append(*prompts, question)
		return true, nil
	}
	return deps, manager, prompts, []string{"--config", configPath, "--state-dir", stateDir}
}

func TestResetPasswordStopsAndRestartsTheHubService(t *testing.T) {
	deps, manager, prompts, args := runningHubFixture(t)
	if code := runResetPassword(context.Background(), deps, args); code != 0 {
		t.Fatalf("exit = %d: %s", code, deps.Stderr.(*bytes.Buffer).String())
	}
	if len(*prompts) != 1 {
		t.Fatalf("prompts = %v", *prompts)
	}
	if !manager.HasCall("stop", systemd.Hub.UnitName()) || !manager.HasCall("start", systemd.Hub.UnitName()) {
		t.Fatalf("calls = %v", manager.Methods())
	}
	if out := deps.Stdout.(*bytes.Buffer).String(); !strings.Contains(out, "Password: ") || !strings.Contains(out, "resumed: hub") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestResetPasswordNoRestartLeavesTheHubStopped(t *testing.T) {
	deps, manager, prompts, args := runningHubFixture(t)
	if code := runResetPassword(context.Background(), deps, append(args, "--yes", "--no-restart")); code != 0 {
		t.Fatalf("exit = %d: %s", code, deps.Stderr.(*bytes.Buffer).String())
	}
	if len(*prompts) != 0 {
		t.Fatalf("--yes still prompted: %v", *prompts)
	}
	if !manager.HasCall("stop", systemd.Hub.UnitName()) || manager.HasCall("start", systemd.Hub.UnitName()) {
		t.Fatalf("calls = %v", manager.Methods())
	}
}

func TestResetPasswordDeclinedLeavesTheHubRunning(t *testing.T) {
	deps, manager, _, args := runningHubFixture(t)
	deps.Confirm = func(string) (bool, error) { return false, nil }
	if code := runResetPassword(context.Background(), deps, args); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if manager.HasCall("stop", systemd.Hub.UnitName()) {
		t.Fatalf("declined prompt still stopped the hub: %v", manager.Methods())
	}
	if !strings.Contains(deps.Stderr.(*bytes.Buffer).String(), "cancelled") {
		t.Fatalf("stderr = %q", deps.Stderr.(*bytes.Buffer).String())
	}
}

func TestResetPasswordWithoutTerminalNeedsYes(t *testing.T) {
	deps, manager, _, args := runningHubFixture(t)
	deps.Confirm = nil
	if code := runResetPassword(context.Background(), deps, args); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if manager.HasCall("stop", systemd.Hub.UnitName()) {
		t.Fatalf("stopped without approval: %v", manager.Methods())
	}
	if !strings.Contains(deps.Stderr.(*bytes.Buffer).String(), "--yes") {
		t.Fatalf("stderr = %q", deps.Stderr.(*bytes.Buffer).String())
	}
}

func TestResetPasswordRefusesAHubOutsideSystemd(t *testing.T) {
	deps, configPath, stateDir, databasePath := resetFixture(t)
	db := seedHubDatabase(t, databasePath)
	_ = db.Close()
	held, err := lock.Acquire(filepath.Join(stateDir, hubLockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	fake := systemd.NewFake()
	deps.NewManager = func(config.Scope) systemd.Manager { return fake }
	code := runResetPassword(context.Background(), deps, []string{"--config", configPath, "--state-dir", stateDir})
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(deps.Stderr.(*bytes.Buffer).String(), "outside the rigscope-hub.service unit") || fake.HasCall("stop", systemd.Hub.UnitName()) {
		t.Fatalf("stderr = %q, calls = %v", deps.Stderr.(*bytes.Buffer).String(), fake.Methods())
	}
}

func TestResetPasswordMissingDatabaseAndUser(t *testing.T) {
	deps, configPath, stateDir, databasePath := resetFixture(t)
	code := runResetPassword(context.Background(), deps, []string{"--config", configPath, "--state-dir", stateDir})
	if code != 1 || !strings.Contains(deps.Stderr.(*bytes.Buffer).String(), "does not exist") {
		t.Fatalf("missing database exit = %d, stderr %q", code, deps.Stderr.(*bytes.Buffer).String())
	}
	seed := seedHubDatabase(t, databasePath)
	_ = seed.Close()
	deps, configPath, stateDir, _ = resetFixture(t)
	db := seedHubDatabase(t, filepath.Join(stateDir, hubDatabaseFile))
	_ = db.Close()
	code = runResetPassword(context.Background(), deps, []string{"--config", configPath, "--state-dir", stateDir, "--user", "ghost"})
	if code != 1 || !strings.Contains(deps.Stderr.(*bytes.Buffer).String(), `user "ghost" does not exist`) {
		t.Fatalf("unknown user exit = %d, stderr %q", code, deps.Stderr.(*bytes.Buffer).String())
	}
}

func TestHubRefusesToStartWhenLockHeld(t *testing.T) {
	deps, configPath, stateDir, _ := resetFixture(t)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := lock.Acquire(filepath.Join(stateDir, hubLockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	code := runHub(context.Background(), deps, []string{"--config", configPath, "--state-dir", stateDir})
	if code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(deps.Stderr.(*bytes.Buffer).String(), "another hub or offline recovery") {
		t.Fatalf("stderr = %q", deps.Stderr.(*bytes.Buffer).String())
	}
}
