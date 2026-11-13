// Package cli implements the installation and lifecycle commands of the single
// rigscope binary: install, uninstall, status, start/stop/restart, logs,
// upgrade, rollback, doctor and legacy hardware-access cleanup.
//
// Every system interaction goes through Deps: paths come from internal/config,
// services through internal/platform/systemd and file writes through
// internal/platform/fsutil. Tests inject temporary directories, a fake systemd
// manager and a fake HTTP client; nothing here contacts the real systemctl,
// journalctl or loginctl unless the default Deps are used.
package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/Triet1402/rigscope/internal/collector/enroll"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/lock"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
	"github.com/Triet1402/rigscope/internal/version"
)

// HTTPDoer is the subset of *http.Client the upgrade path needs.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Deps is the injectable environment for all commands.
type Deps struct {
	Stdout io.Writer
	Stderr io.Writer

	// ColorStdout and ColorStderr enable ANSI styling on each stream. The
	// defaults turn them on only for a terminal with NO_COLOR unset; tests
	// leave them false so output stays plain.
	ColorStdout bool
	ColorStderr bool
	// Confirm asks the operator a yes/no question before running services are
	// stopped. The default asks on /dev/tty; a nil seam means there is no
	// terminal, so commands require --yes.
	Confirm func(question string) (bool, error)

	Getenv     func(string) string
	HomeDir    func() (string, error)
	Euid       func() int
	Executable func() (string, error)
	// RunningExecutable names the file backing the running process image.
	// The default is /proc/self/exe, which still refers to the executing inode
	// after the path on disk has been replaced; hashing the pathname would
	// compare the restored file with itself.
	RunningExecutable func() (string, error)
	// BinaryVersion reports the version of an installed binary. The default
	// runs "<path> version" with a bounded deadline.
	BinaryVersion func(string) (string, error)
	// ExecInstalled replaces this process with the installed binary. The default
	// uses syscall.Exec; tests inject a recorder.
	ExecInstalled func(path string, args []string) error
	// ExecInstalledEnv is ExecInstalled with an explicit environment. The
	// upgrade re-exec uses it to add only the loop guard to os.Environ().
	ExecInstalledEnv func(path string, args, env []string) error

	// Chown sets ownership for system-install files and state directories.
	// Tests inject a no-op so nothing outside their temp tree is touched.
	Chown func(path, userName, groupName string) error
	// ChownFile sets ownership through an open descriptor, so a directory
	// owner cannot swap the pathname for a symlink between the write and the
	// chown. Tests inject a no-op.
	ChownFile func(file *os.File, userName, groupName string) error
	// InstallStep is a test-only interruption seam. It is called before each
	// named install phase; a non-nil error aborts the install.
	InstallStep func(step string) error
	// PublishStep is a test-only interruption seam around file publication. It
	// is called with "staged" after the staged identity is recorded and
	// "published" after the new file is in place but before it is promoted to
	// owned. A non-nil error stops the install like a process death.
	PublishStep func(path, phase string) error
	// AfterStop is a test-only seam between stopping services and deleting
	// files during uninstall.
	AfterStop func() error
	// FileOwner reports a file's numeric owner and group. Doctor uses it for
	// scope-specific ownership checks; tests inject a fixed owner.
	FileOwner func(path string) (uid, gid int, err error)
	// GroupID and UserID resolve account names for doctor checks.
	GroupID func(name string) (int, error)
	UserID  func(name string) (int, error)

	// Arch is a Go architecture name such as "amd64" or "arm64".
	Arch    string
	Version string

	HTTPClient  HTTPDoer
	ProbeHealth func(ctx context.Context, url string) error
	// CollectorReady overrides the installed-binary "collector --check"
	// readiness probe. Tests inject a stub because their installed binary is a
	// fixture, not an executable.
	CollectorReady func(ctx context.Context) error
	Sleep          func(time.Duration)

	NewManager func(config.Scope) systemd.Manager
	RunStream  func(ctx context.Context, name string, args ...string) error
	// RunCommand runs a system integration command such as systemd-sysusers
	// or udevadm and includes its output in errors. It is only used for
	// --system installs.
	RunCommand func(ctx context.Context, name string, args ...string) error
	// RunSudo runs one root-owned command through "sudo -- name args..." with
	// the caller's terminal attached, so sudo can prompt. A user-scope GPU
	// register install never re-executes the rigscope binary under sudo. Tests
	// inject a recorder and must never let this reach the real sudo.
	RunSudo func(ctx context.Context, name string, args ...string) error
	// StdinIsTerminal reports whether standard input is a terminal. The
	// user-scope GPU register install only prompts through sudo when it is;
	// otherwise it prints the commands. Tests inject false to prove that path.
	StdinIsTerminal func() bool
	// Elevate re-runs the command with these arguments as root (sudo) and
	// returns its exit status. Nil, or errNoElevation, means it cannot.
	Elevate func(args []string) (int, error)
	// args is the command line Run received, for Elevate.
	args []string
	// Enroll redeems an enrollment grant. Nil uses enroll.Run; tests inject a
	// fake hub answer.
	Enroll func(ctx context.Context, opts enroll.Options) (enroll.Result, error)
	// Stdin is read by --password-stdin. Nil means os.Stdin.
	Stdin io.Reader
	// ReadPassword asks for a secret with terminal echo off. It returns
	// errNoTerminal when there is no terminal; nil behaves the same.
	ReadPassword func(prompt string) (string, error)
	// ProbeRegisterAccess proves that a PCI resource file can be opened and
	// mapped read-only without reading a register word. The default maps one
	// page and unmaps it, so a suspended GPU is never asked for data. Install
	// and doctor use it to prove register permission; tests inject a fake so
	// they never map a real BAR.
	ProbeRegisterAccess func(path string) error
	// ReadAttribute reads a small sysfs attribute such as energy_uj as the
	// invoking user. Those are ordinary read(2) files, not mappable memory.
	ReadAttribute func(path string) ([]byte, error)
	// Accounts supplies the local passwd/group records used to choose the
	// user-scope GPU register group. Tests inject fixed records.
	Accounts AccountDatabase
	// UserManagerGroups returns the supplementary GIDs of the invoking user's
	// systemd --user manager. A user service inherits that manager's groups,
	// so a fresh login shell is not proof the collector has them. Tests inject
	// a fixed list; a not-found error means "cannot tell".
	UserManagerGroups func() ([]int, error)
	// LingerEnabled reports whether the invoking user's manager runs at boot.
	// Linger keeps a stale manager (and its groups) alive after log-out, so
	// the guidance then needs a reboot or loginctl terminate-user.
	LingerEnabled func(user string) bool

	// APIBase is the GitHub Releases API root used by upgrade. Overridden by
	// tests.
	APIBase string
	// AllowInsecureHTTP is a test-only exception for release downloads. It must
	// stay false outside tests so the initial URL and every redirect are HTTPS.
	AllowInsecureHTTP bool
	// Root prefixes host paths for tests and image builds: system-scope paths,
	// the root-owned udev rules and the sysfs tree the GPU check reads. Empty
	// means the real filesystem.
	Root string
}

// DefaultDeps wires the real system. Only production paths use it.
func DefaultDeps(stdout, stderr io.Writer) Deps {
	colorStderr := colorAllowed(os.Getenv, stderr)
	return Deps{
		Stdout:       stdout,
		Stderr:       stderr,
		ColorStdout:  colorAllowed(os.Getenv, stdout),
		ColorStderr:  colorStderr,
		Confirm:      defaultConfirm(stderr, colorStderr),
		ReadPassword: defaultReadPassword(colorStderr),
		Elevate:      defaultElevate,
		Getenv:       os.Getenv,
		HomeDir:      os.UserHomeDir,
		Euid:         os.Geteuid,
		Executable:   os.Executable,
		RunningExecutable: func() (string, error) {
			return "/proc/self/exe", nil
		},
		BinaryVersion: func(path string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, path, "version")
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			command.Cancel = func() error {
				if command.Process == nil {
					return nil
				}
				return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			}
			command.WaitDelay = 2 * time.Second
			out, err := command.Output()
			if err != nil {
				return "", err
			}
			fields := strings.Fields(string(out))
			if len(fields) < 2 {
				return "", fmt.Errorf("unexpected version output %q", strings.TrimSpace(string(out)))
			}
			return fields[len(fields)-1], nil
		},
		Arch:       runtime.GOARCH,
		Version:    version.Version,
		HTTPClient: &http.Client{Timeout: 2 * time.Minute},
		Sleep:      time.Sleep,
		NewManager: func(scope config.Scope) systemd.Manager {
			if scope == config.ScopeSystem {
				return systemd.NewExec(systemd.System)
			}
			return systemd.NewExec(systemd.User)
		},
		RunStream: func(ctx context.Context, name string, args ...string) error {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			return cmd.Run()
		},
		RunCommand: func(ctx context.Context, name string, args ...string) error {
			output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(output)))
			}
			return nil
		},
		RunSudo: func(ctx context.Context, name string, args ...string) error {
			return defaultRunSudo(ctx, (*exec.Cmd).Run, name, args...)
		},
		StdinIsTerminal: func() bool {
			info, err := os.Stdin.Stat()
			return err == nil && info.Mode()&os.ModeCharDevice != 0
		},
		ProbeRegisterAccess: func(path string) error {
			return gpu.ProbeRegisterAccess(path)
		},
		ReadAttribute: os.ReadFile,
		FileOwner: func(path string) (int, int, error) {
			info, err := os.Stat(path)
			if err != nil {
				return 0, 0, err
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, 0, fmt.Errorf("%s has no ownership information", path)
			}
			return int(stat.Uid), int(stat.Gid), nil
		},
		GroupID: func(name string) (int, error) {
			group, err := user.LookupGroup(name)
			if err != nil {
				return 0, err
			}
			return strconv.Atoi(group.Gid)
		},
		UserID: func(name string) (int, error) {
			account, err := user.Lookup(name)
			if err != nil {
				return 0, err
			}
			return strconv.Atoi(account.Uid)
		},
		Chown: func(path, userName, groupName string) error {
			uid, gid, err := lookupIDs(userName, groupName)
			if err != nil {
				return err
			}
			return os.Chown(path, uid, gid)
		},
		ChownFile: func(file *os.File, userName, groupName string) error {
			uid, gid, err := lookupIDs(userName, groupName)
			if err != nil {
				return err
			}
			return file.Chown(uid, gid)
		},
		APIBase: "https://api.github.com/repos/Triet1402/rigscope/releases",
	}
}

// Run dispatches one non-version command and returns a process exit code.
func Run(ctx context.Context, args []string, deps Deps) int {
	if deps.Stdout == nil {
		deps.Stdout = io.Discard
	}
	if deps.Stderr == nil {
		deps.Stderr = io.Discard
	}
	if len(args) == 0 {
		Usage(deps.Stderr)
		return 2
	}
	deps.args = args
	switch args[0] {
	case "install":
		return runInstall(ctx, deps, args[1:])
	case "configure":
		return runConfigure(ctx, deps, args[1:])
	case "uninstall":
		return runUninstall(ctx, deps, args[1:])
	case "cleanup-legacy-hardware":
		return runCleanupLegacyHardware(ctx, deps, args[1:])
	case "status":
		return runStatus(ctx, deps, args[1:])
	case "start":
		return runService(ctx, deps, "start", args[1:])
	case "stop":
		return runService(ctx, deps, "stop", args[1:])
	case "restart":
		return runService(ctx, deps, "restart", args[1:])
	case "logs":
		return runLogs(ctx, deps, args[1:])
	case "upgrade":
		return runUpgrade(ctx, deps, args[1:])
	case "rollback":
		return runRollback(ctx, deps, args[1:])
	case "migrate":
		return runMigrate(ctx, deps, args[1:])
	case "doctor":
		return runDoctor(ctx, deps, args[1:])
	case "hub":
		return runHub(ctx, deps, args[1:])
	case "reset-password":
		return runResetPassword(ctx, deps, args[1:])
	case "collector":
		return runCollector(ctx, deps, args[1:])
	default:
		fmt.Fprintf(deps.Stderr, "rigscope: unknown command %q\n\n", args[0])
		Usage(deps.Stderr)
		return 2
	}
}

// Usage prints the command list shared by the binary entry point. On a
// terminal (and without NO_COLOR) headings are bold and command names cyan.
func Usage(w io.Writer) {
	color := colorAllowed(os.Getenv, w)
	for _, line := range strings.SplitAfter(usageText, "\n") {
		fmt.Fprint(w, styleUsageLine(color, line))
	}
}

// styleUsageLine colors one help line: "Section:" headings, the command words
// at the start of an entry ("collector enroll", "start|stop|restart") and the
// one-line descriptions under Other. Continuation lines stay plain.
func styleUsageLine(color bool, line string) string {
	if !color {
		return line
	}
	body := strings.TrimRight(line, "\n")
	newline := line[len(body):]
	switch {
	case strings.HasPrefix(body, "Usage: "):
		return paint(color, styleBold, "Usage:") + strings.TrimPrefix(body, "Usage:") + newline
	case body != "" && !strings.HasPrefix(body, " ") && strings.HasSuffix(body, ":"):
		return paint(color, styleBold, body) + newline
	case strings.HasPrefix(body, "  ") && !strings.HasPrefix(body, "   "):
		rest := body[2:]
		end := 0
		for _, word := range strings.SplitAfter(rest, " ") {
			trimmed := strings.TrimSpace(word)
			if trimmed == "" || strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "-") || trimmed != strings.ToLower(trimmed) {
				break
			}
			end += len(word)
		}
		name := strings.TrimRight(rest[:end], " ")
		return "  " + paint(color, styleCyan+";"+styleBold, name) + rest[len(name):] + newline
	}
	return line
}

const usageText = `Usage: rigscope <command> [options]

Installation:
  install [--role hub|collector|both] [--system] [--boot] [--no-start]
          [--gpu-registers] [--port PORT] [--origin URL]
          [--hub URL --token TOKEN [--hub-ca FILE]] [--password-stdin]
          [--yes]
  configure --show [--json] [--system]
  configure [--hub on|off] [--collector on|off] [--system] [--yes] [--no-restart]
            hub:       [--port PORT] [--origin URL | --local] [--listen ADDRESS]
                       [--tls-cert FILE --tls-key FILE | --no-tls]
                       [--trusted-proxy LIST|none] [--forwarded-headers x-forwarded|forwarded]
                       [--telemetry-mode memory|persistent] [--password-stdin]
            collector: [--share FAMILY=on|off]... [--sampling-interval 2s]
                       [--gpu-registers off]
  uninstall [--keep-data] [--system] [--yes]
  cleanup-legacy-hardware [--apply]
  upgrade [--version VERSION] [--system] [--yes]
  rollback [--system] [--yes]
  doctor [--system]

Services:
  status [--json] [--system]
  start|stop|restart [hub|collector] [--system]
  logs [--follow] [hub|collector] [--system]

Hub:
  hub [--config FILE] [--state-dir DIR] [--system]
  reset-password [--user NAME] [--config FILE] [--state-dir DIR] [--system]
                 [--password-stdin] [--yes] [--no-restart]

Collector:
  collector [--config FILE] [--state-dir DIR] [--hostname NAME] [--check]
  collector enroll [--config FILE] [--state-dir DIR] [--grant TOKEN]
                   [--hub-url URL] [--ca-file FILE] [--system] [--yes]

Other:
  license    Print the AGPLv3 license
  version    Print the version
  help       Show this help

Commands that must stop running services say which ones and ask first, then
start them again afterwards. --yes (-y) approves without asking; --no-restart
leaves them stopped. Without --system, commands use the per-user installation,
or the system one when there is no per-user installation; commands that need
root ask sudo for your password. Set NO_COLOR=1 to turn off colored output.
`

func (d Deps) env(key string) string {
	if d.Getenv == nil {
		return ""
	}
	return d.Getenv(key)
}

func (d Deps) home() (string, error) {
	if d.HomeDir != nil {
		return d.HomeDir()
	}
	return os.UserHomeDir()
}

func (d Deps) enroll(ctx context.Context, opts enroll.Options) (enroll.Result, error) {
	if d.Enroll != nil {
		return d.Enroll(ctx, opts)
	}
	return enroll.Run(ctx, opts)
}

func (d Deps) euid() int {
	if d.Euid != nil {
		return d.Euid()
	}
	return os.Geteuid()
}

func (d Deps) executable() (string, error) {
	if d.Executable != nil {
		return d.Executable()
	}
	return os.Executable()
}

// runningExecutable returns the path whose open file is the running image.
// /proc/self/exe follows the executing inode, not a pathname that a binary
// swap may already have replaced.
func (d Deps) runningExecutable() (string, error) {
	if d.RunningExecutable != nil {
		return d.RunningExecutable()
	}
	return "/proc/self/exe", nil
}

func (d Deps) execInstalled(path string, args []string) error {
	return d.execInstalledWithEnv(path, args, os.Environ())
}

func (d Deps) execInstalledWithEnv(path string, args, env []string) error {
	if d.ExecInstalledEnv != nil {
		return d.ExecInstalledEnv(path, args, env)
	}
	if d.ExecInstalled != nil {
		return d.ExecInstalled(path, args)
	}
	argv := append([]string{path}, args...)
	return syscall.Exec(path, argv, env)
}

func (d Deps) chown(path, userName, groupName string) error {
	if d.Chown == nil {
		return nil
	}
	return d.Chown(path, userName, groupName)
}

func (d Deps) chownFile(file *os.File, userName, groupName string) error {
	if d.ChownFile == nil {
		return nil
	}
	return d.ChownFile(file, userName, groupName)
}

// runSudo runs a root-owned command through the injected seam. A nil seam is
// not a reason to fall back to the real sudo: tests must never prompt. The
// caller prints the command and continues without hardware access instead.
func (d Deps) runSudo(ctx context.Context, name string, args ...string) error {
	if d.RunSudo == nil {
		return errors.New("no sudo runner is available")
	}
	return d.RunSudo(ctx, name, args...)
}

// sudoCommand builds the sudo invocation with the caller's terminal attached.
// The "--" separates sudo options from the command so a name or argument that
// starts with "-" is never parsed as a sudo flag.
func sudoCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	argv := append([]string{"--", name}, args...)
	cmd := exec.CommandContext(ctx, "sudo", argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// defaultRunSudo is the real sudo runner. The run seam lets a test inspect the
// exact command without executing sudo.
func defaultRunSudo(ctx context.Context, run func(*exec.Cmd) error, name string, args ...string) error {
	return run(sudoCommand(ctx, name, args...))
}

func (d Deps) stdinIsTerminal() bool {
	if d.StdinIsTerminal != nil {
		return d.StdinIsTerminal()
	}
	return false
}

func (d Deps) probeRegisterAccess(path string) error {
	if d.ProbeRegisterAccess != nil {
		return d.ProbeRegisterAccess(path)
	}
	return gpu.ProbeRegisterAccess(path)
}

func (d Deps) readAttribute(path string) ([]byte, error) {
	if d.ReadAttribute != nil {
		return d.ReadAttribute(path)
	}
	return os.ReadFile(path)
}

// userManagerGroups returns the GIDs the invoking user's systemd --user
// manager carries. A user unit inherits them once, at manager start.
func (d Deps) userManagerGroups() ([]int, error) {
	if d.UserManagerGroups != nil {
		return d.UserManagerGroups()
	}
	return scanUserManagerGroups(d.hostPath("proc"), d.euid())
}

func (d Deps) lingerEnabled(user string) bool {
	if d.LingerEnabled != nil {
		return d.LingerEnabled(user)
	}
	if user == "" {
		return false
	}
	_, err := os.Stat(d.hostPath("var", "lib", "systemd", "linger", user))
	return err == nil
}

func (d Deps) accounts() AccountDatabase {
	if d.Accounts != nil {
		return d.Accounts
	}
	return hostAccounts{Root: d.Root, Euid: d.euid()}
}

// hostPath resolves a host-owned path below the optional test/image root.
func (d Deps) hostPath(parts ...string) string {
	if d.Root == "" {
		return filepath.Join(append([]string{string(filepath.Separator)}, parts...)...)
	}
	return filepath.Join(append([]string{d.Root}, parts...)...)
}

func lookupIDs(userName, groupName string) (int, int, error) {
	uid, gid := -1, -1
	if userName != "" {
		account, err := user.Lookup(userName)
		if err != nil {
			return 0, 0, err
		}
		value, err := strconv.Atoi(account.Uid)
		if err != nil {
			return 0, 0, err
		}
		uid = value
	}
	if groupName != "" {
		group, err := user.LookupGroup(groupName)
		if err != nil {
			return 0, 0, err
		}
		value, err := strconv.Atoi(group.Gid)
		if err != nil {
			return 0, 0, err
		}
		gid = value
	}
	return uid, gid, nil
}

func (d Deps) sleep(duration time.Duration) {
	if d.Sleep != nil {
		d.Sleep(duration)
		return
	}
	time.Sleep(duration)
}

func (d Deps) architecture() string {
	if d.Arch != "" {
		return d.Arch
	}
	return runtime.GOARCH
}

func (d Deps) buildVersion() string {
	if d.Version != "" {
		return d.Version
	}
	return version.Version
}

func (d Deps) apiBase() string {
	if d.APIBase != "" {
		return strings.TrimRight(d.APIBase, "/")
	}
	return "https://api.github.com/repos/Triet1402/rigscope/releases"
}

func (d Deps) manager(scope config.Scope) systemd.Manager {
	if d.NewManager != nil {
		return d.NewManager(scope)
	}
	if scope == config.ScopeSystem {
		return systemd.NewExec(systemd.System)
	}
	return systemd.NewExec(systemd.User)
}

func (d Deps) probeHealth(ctx context.Context, url string) error {
	if d.ProbeHealth != nil {
		return d.ProbeHealth(ctx, url)
	}
	client := d.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", url, response.StatusCode)
	}
	return nil
}

// paths resolves the layout for a scope. System paths never depend on a home
// directory, and a system service may run without $HOME: systemd leaves it
// unset for an account whose home is "/" (as 0.2 created rigscope-collector).
func (d Deps) paths(scope config.Scope) (config.Paths, error) {
	home, err := d.home()
	if err != nil && scope != config.ScopeSystem {
		return config.Paths{}, err
	}
	return config.Resolve(scope, config.Env{
		Home:          home,
		XDGConfigHome: d.env("XDG_CONFIG_HOME"),
		XDGStateHome:  d.env("XDG_STATE_HOME"),
		XDGDataHome:   d.env("XDG_DATA_HOME"),
		RuntimeDir:    d.env("XDG_RUNTIME_DIR"),
		UID:           d.euid(),
		Root:          d.Root,
	})
}

func (d Deps) fileOwner(path string) (int, int, error) {
	if d.FileOwner != nil {
		return d.FileOwner(path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%s has no ownership information", path)
	}
	return int(stat.Uid), int(stat.Gid), nil
}

func (d Deps) userID(name string) (int, error) {
	if d.UserID != nil {
		return d.UserID(name)
	}
	account, err := user.Lookup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(account.Uid)
}

func (d Deps) groupID(name string) (int, error) {
	if d.GroupID != nil {
		return d.GroupID(name)
	}
	group, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(group.Gid)
}

// acquireLock serialises install, uninstall and service commands. It is
// exported to the activation work through internal/platform/lock, not from
// this package.
func acquireLock(paths config.Paths) (*lock.Lock, error) {
	held, err := lock.Acquire(paths.LockFile())
	if err != nil {
		return nil, err
	}
	return held, nil
}

func (d Deps) errorf(format string, args ...any) int {
	fmt.Fprintf(d.Stderr, "%s %s\n", d.errStyle(styleRed+";"+styleBold, "rigscope:"), fmt.Sprintf(format, args...))
	return 1
}

// ensureRoot makes sure a system-scope command runs as root. As a regular
// user it re-runs the same command through sudo, which asks for the password,
// and reports that run's exit status; done is then true and the caller returns
// code without doing anything itself. Without sudo or a terminal to ask on it
// prints how to run the command and returns 1.
func (d Deps) ensureRoot(scope config.Scope) (code int, done bool) {
	if scope != config.ScopeSystem || d.euid() == 0 {
		return 0, false
	}
	if d.Elevate != nil && len(d.args) > 0 {
		fmt.Fprintf(d.Stderr, "%s The system installation needs root; asking sudo.\n", d.errStyle(styleCyan+";"+styleBold, "::"))
		status, err := d.Elevate(withSystemFlag(d.args))
		if err == nil {
			return status, true
		}
		if !errors.Is(err, errNoElevation) {
			return d.errorf("run with sudo: %v", err), true
		}
	}
	return d.errorf("the system installation requires root; run again with sudo"), true
}

// errNoElevation means sudo is missing or there is no terminal to ask on.
var errNoElevation = errors.New("cannot ask for root")

// withSystemFlag adds --system right after the command words, so the elevated
// run acts on the system installation even if root has a per-user one.
func withSystemFlag(args []string) []string {
	at := 1
	switch {
	case args[0] == "cleanup-legacy-hardware":
		return args
	case args[0] == "collector" && len(args) > 1 && args[1] == "enroll":
		at = 2
	}
	out := append([]string{}, args[:at]...)
	out = append(out, "--system")
	return append(out, args[at:]...)
}

// defaultElevate re-runs this binary under sudo with the terminal attached.
// sudo reads the password from the terminal itself, which also works under
// "curl ... | sh".
func defaultElevate(args []string) (int, error) {
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return 0, errNoElevation
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return 0, errNoElevation
	}
	_ = tty.Close()
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	command := exec.Command(sudo, append([]string{"--", self}, args...)...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = command.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// installedScope picks the scope for a command that acts on an existing
// installation. --system always wins. Otherwise the per-user installation is
// used, unless there is none and a system installation exists: then the
// command uses the system one and says so, so "rigscope status" or
// "sudo rigscope reset-password" finds a system install without --system.
func (d Deps) installedScope(system bool) config.Scope {
	if system {
		return config.ScopeSystem
	}
	if d.hasInstallation(config.ScopeUser) || !d.hasInstallation(config.ScopeSystem) {
		return config.ScopeUser
	}
	fmt.Fprintf(d.Stderr, "%s\n", d.errStyle(styleDim, "rigscope: no per-user installation; using the system installation"))
	return config.ScopeSystem
}

// hasInstallation reports whether scope has an installation record or a
// configuration file. It only stats paths, so it works without root for the
// root-owned system state directory.
func (d Deps) hasInstallation(scope config.Scope) bool {
	paths, err := d.paths(scope)
	if err != nil {
		return false
	}
	return fileExists(paths.ManifestFile()) || fileExists(paths.ConfigFile)
}

func scopeFrom(system bool) config.Scope {
	if system {
		return config.ScopeSystem
	}
	return config.ScopeUser
}

func scopeLabel(scope config.Scope) string {
	if scope == config.ScopeSystem {
		return "system"
	}
	return "user"
}
