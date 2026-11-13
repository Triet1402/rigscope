package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// typedPasswords answers ReadPassword prompts in order.
func typedPasswords(prompts *[]string, answers ...string) func(string) (string, error) {
	return func(prompt string) (string, error) {
		*prompts = append(*prompts, prompt)
		if len(answers) == 0 {
			return "", errNoTerminal
		}
		answer := answers[0]
		answers = answers[1:]
		return answer, nil
	}
}

// ownerHash reads the owner hash the installer left for the hub's first start.
func ownerHash(t *testing.T, paths config.Paths) string {
	t.Helper()
	raw, err := os.ReadFile(ownerBootstrapPath(paths))
	if err != nil {
		t.Fatalf("owner bootstrap: %v", err)
	}
	info, err := os.Stat(ownerBootstrapPath(paths))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("owner bootstrap mode: %v %v", info.Mode(), err)
	}
	users, present, err := loadBrowserBootstrap(paths.ComponentStateDir(systemd.Hub.String()))
	if err != nil || !present || len(users) != 1 {
		t.Fatalf("hub cannot import %s: %v (%d users)\n%s", ownerBootstrapPath(paths), err, len(users), raw)
	}
	if users[0].Username != "owner" || !users[0].Admin || !users[0].Scope.All {
		t.Fatalf("owner account = %+v", users[0])
	}
	return users[0].PasswordHash
}

func TestInstallShowsAGeneratedOwnerPassword(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t)
	out := f.out.String()
	start := strings.Index(out, "sign in: owner / ")
	if start < 0 || !strings.Contains(out, "Save this password; it is shown once.") {
		t.Fatalf("install did not show the owner password:\n%s", out)
	}
	password := strings.Fields(out[start+len("sign in: owner / "):])[0]
	if ok, err := auth.VerifyPassword(password, ownerHash(t, paths)); err != nil || !ok {
		t.Fatalf("printed password does not match the stored hash: %v", err)
	}

	// A reinstall never replaces the owner or prints a password again.
	f.out.Reset()
	before, _ := os.ReadFile(ownerBootstrapPath(paths))
	f.install(t)
	after, _ := os.ReadFile(ownerBootstrapPath(paths))
	if strings.Contains(f.out.String(), "sign in:") || !bytes.Equal(before, after) {
		t.Fatalf("reinstall touched the owner account:\n%s", f.out.String())
	}
}

func TestInstallLetsTheOperatorChooseThePassword(t *testing.T) {
	f := newFixture(t)
	var prompts []string
	f.deps.ReadPassword = typedPasswords(&prompts, "correct horse", "correct horse")
	paths := f.install(t)
	if len(prompts) != 2 {
		t.Fatalf("prompts = %q", prompts)
	}
	if out := f.out.String(); strings.Contains(out, "correct horse") || !strings.Contains(out, "sign in: owner, with the password you chose") {
		t.Fatalf("stdout = %s", out)
	}
	if ok, err := auth.VerifyPassword("correct horse", ownerHash(t, paths)); err != nil || !ok {
		t.Fatalf("chosen password not stored: %v", err)
	}
}

func TestInstallRefusesMismatchedPasswordsBeforeChangingAnything(t *testing.T) {
	f := newFixture(t)
	var prompts []string
	f.deps.ReadPassword = typedPasswords(&prompts, "first", "second")
	if code := f.run(t, "install"); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(f.err.String(), "do not match") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	paths := f.paths(t, config.ScopeUser)
	if fileExists(paths.ConfigFile) || fileExists(ownerBootstrapPath(paths)) || len(f.fake.Methods()) != 0 {
		t.Fatalf("a mismatched password still changed the machine: %v", f.fake.Methods())
	}
}

func TestInstallPasswordFromStdinAndYes(t *testing.T) {
	f := newFixture(t)
	var prompts []string
	f.deps.ReadPassword = typedPasswords(&prompts, "never asked")
	f.deps.Stdin = strings.NewReader("from a pipe\n")
	paths := f.install(t, "--password-stdin")
	if len(prompts) != 0 {
		t.Fatalf("--password-stdin still prompted: %q", prompts)
	}
	if ok, _ := auth.VerifyPassword("from a pipe", ownerHash(t, paths)); !ok {
		t.Fatal("stdin password not stored")
	}

	g := newFixture(t)
	g.deps.ReadPassword = typedPasswords(&prompts, "never asked")
	g.install(t, "--yes")
	if len(prompts) != 0 || !strings.Contains(g.out.String(), "sign in: owner / ") {
		t.Fatalf("--yes must generate without asking: %q\n%s", prompts, g.out.String())
	}
	if code := g.run(t, "install", "--password-stdin"); code != 1 || !strings.Contains(g.err.String(), "already has accounts") {
		t.Fatalf("--password-stdin on an existing hub: exit %d: %s", code, g.err.String())
	}
}

func TestResetPasswordToAChosenPassword(t *testing.T) {
	deps, manager, _, args := runningHubFixture(t)
	var prompts []string
	deps.ReadPassword = typedPasswords(&prompts, "new secret", "new secret")
	if code := runResetPassword(context.Background(), deps, args); code != 0 {
		t.Fatalf("exit = %d: %s", code, deps.Stderr.(*bytes.Buffer).String())
	}
	out := deps.Stdout.(*bytes.Buffer).String()
	if strings.Contains(out, "new secret") || strings.Contains(out, "Password: ") || !strings.Contains(out, "Password changed for owner") {
		t.Fatalf("stdout = %q", out)
	}
	if !manager.HasCall("start", systemd.Hub.UnitName()) {
		t.Fatalf("hub not restarted: %v", manager.Methods())
	}
	stateDir := args[3]
	db, err := storage.Open(context.Background(), filepath.Join(stateDir, hubDatabaseFile), storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	user, err := storage.NewUsers(db).Get(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := auth.VerifyPassword("new secret", user.PasswordHash); !ok {
		t.Fatal("chosen password not stored")
	}
}

func TestResetPasswordMismatchLeavesTheHubRunning(t *testing.T) {
	deps, manager, prompts, args := runningHubFixture(t)
	var typed []string
	deps.ReadPassword = typedPasswords(&typed, "one", "two")
	if code := runResetPassword(context.Background(), deps, args); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if len(*prompts) != 0 || manager.HasCall("stop", systemd.Hub.UnitName()) {
		t.Fatalf("a mismatch still asked to stop or stopped the hub: %v %v", *prompts, manager.Methods())
	}
}
