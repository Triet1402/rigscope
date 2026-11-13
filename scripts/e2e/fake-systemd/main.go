// Command fake-systemd is the test double used by the Rigscope lifecycle
// end-to-end test. It is built once and installed as systemctl, journalctl,
// loginctl, systemd-sysusers and udevadm on the test PATH.
//
// It deliberately implements only the surface internal/platform/systemd and
// internal/cli use: daemon-reload, enable/disable, start/stop/restart,
// is-active, show -p LoadState,ActiveState,SubState --value, loginctl
// enable-linger and journalctl --user -u. Everything it touches lives under
// RIGSCOPE_FAKE_SYSTEMD_ROOT; it never talks to the real systemd, journald or
// logind, and it has no notion of a system manager.
//
// start launches the unit's ExecStart as a background process through a small
// supervisor mode of this same binary. The supervisor owns the child process
// group, forwards SIGTERM, reaps the child, and writes per-unit stdout/stderr
// to <root>/journal/<unit>.log so fake journalctl can print it. A pid file per
// unit lets show/is-active report live state from the filesystem.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "__supervise" {
		os.Exit(supervise(os.Args[2:]))
	}
	root := os.Getenv("RIGSCOPE_FAKE_SYSTEMD_ROOT")
	if root == "" {
		fmt.Fprintln(os.Stderr, "fake-systemd: RIGSCOPE_FAKE_SYSTEMD_ROOT is required")
		os.Exit(1)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		fatal(err)
	}
	name := filepath.Base(os.Args[0])
	logCommand(root, name, os.Args[1:])
	switch name {
	case "systemctl":
		os.Exit(systemctl(root, os.Args[1:]))
	case "journalctl":
		os.Exit(journalctl(root, os.Args[1:]))
	case "loginctl":
		os.Exit(loginctl(root, os.Args[1:]))
	case "systemd-sysusers", "udevadm":
		// The user-scope lifecycle never needs these. Record and succeed so an
		// accidental system-scope path still cannot reach the real tools.
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "fake-systemd: unknown tool name %q\n", name)
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "fake-systemd: %v\n", err)
	os.Exit(1)
}

func logCommand(root, name string, args []string) {
	file, err := os.OpenFile(filepath.Join(root, "commands.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%s %s\n", name, strings.Join(args, " "))
}

// systemctl implements the verbs the manager and the CLI use. --user is
// accepted and ignored: this double only has user-scope state.
func systemctl(root string, args []string) int {
	args = stripUser(args)
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fake-systemctl: missing verb")
		return 1
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "daemon-reload":
		return 0
	case "enable", "disable":
		if len(rest) != 1 {
			fmt.Fprintf(os.Stderr, "fake-systemctl: %s requires one unit\n", verb)
			return 1
		}
		return setEnabled(root, rest[0], verb == "enable")
	case "start":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "fake-systemctl: start requires one unit")
			return 1
		}
		return startUnit(root, rest[0])
	case "stop":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "fake-systemctl: stop requires one unit")
			return 1
		}
		return stopUnit(root, rest[0])
	case "restart":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "fake-systemctl: restart requires one unit")
			return 1
		}
		if code := stopUnit(root, rest[0]); code != 0 {
			return code
		}
		return startUnit(root, rest[0])
	case "is-active":
		if len(rest) != 1 {
			fmt.Fprintln(os.Stderr, "fake-systemctl: is-active requires one unit")
			return 1
		}
		if alive(root, rest[0]) {
			fmt.Println("active")
			return 0
		}
		fmt.Println("inactive")
		return 3
	case "show":
		return show(root, rest)
	default:
		fmt.Fprintf(os.Stderr, "fake-systemctl: unsupported verb %q\n", verb)
		return 1
	}
}

func stripUser(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--user" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func unitDir() (string, error) {
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "systemd", "user"), nil
	}
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("HOME and XDG_CONFIG_HOME are both unset")
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func unitPath(unit string) (string, error) {
	dir, err := unitDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, unit), nil
}

func setEnabled(root, unit string, enabled bool) int {
	dir := filepath.Join(root, "enabled")
	if enabled {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, unit), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
			fatal(err)
		}
		return 0
	}
	if err := os.Remove(filepath.Join(dir, unit)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fatal(err)
	}
	return 0
}

func pidPath(root, unit string) string {
	return filepath.Join(root, "run", unit+".pid")
}

func logPath(root, unit string) string {
	return filepath.Join(root, "journal", unit+".log")
}

func readPID(root, unit string) int {
	data, err := os.ReadFile(pidPath(root, unit))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	// A released orphan can be left as a zombie by a container PID 1 that does
	// not reap. Treat Z as gone: no unit is running behind it.
	if state, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")); err == nil {
		fields := strings.Fields(string(state))
		if len(fields) > 2 && fields[2] == "Z" {
			return false
		}
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

func alive(root, unit string) bool {
	return processExists(readPID(root, unit))
}

// execStart extracts the first ExecStart line and splits it with systemd-like
// quoting. The generated user units quote both the binary and the config path.
func execStart(unitFile string) ([]string, error) {
	file, err := os.Open(unitFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		command, err := splitUnitCommand(strings.TrimPrefix(line, "ExecStart="))
		if err != nil {
			return nil, err
		}
		if len(command) == 0 {
			return nil, errors.New("empty ExecStart")
		}
		return command, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("unit has no ExecStart")
}

func stopTimeout(unitFile string) time.Duration {
	timeout := 15 * time.Second
	file, err := os.Open(unitFile)
	if err != nil {
		return timeout
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "TimeoutStopSec=") {
			continue
		}
		value := strings.TrimSuffix(strings.TrimPrefix(line, "TimeoutStopSec="), "s")
		if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
			timeout = time.Duration(seconds) * time.Second
		}
	}
	return timeout
}

// splitUnitCommand implements the subset of systemd quoting the installer
// emits: double-quoted arguments, backslash escapes, and %% / $$ expansion.
func splitUnitCommand(input string) ([]string, error) {
	var args []string
	var current strings.Builder
	inQuote := false
	started := false
	for index := 0; index < len(input); index++ {
		character := input[index]
		switch {
		case character == '\\' && index+1 < len(input):
			index++
			current.WriteByte(input[index])
			started = true
		case character == '"':
			inQuote = !inQuote
			started = true
		case (character == ' ' || character == '\t') && !inQuote:
			if started {
				args = append(args, current.String())
				current.Reset()
				started = false
			}
		case character == '%' && index+1 < len(input) && input[index+1] == '%':
			index++
			current.WriteByte('%')
			started = true
		case character == '$' && index+1 < len(input) && input[index+1] == '$':
			index++
			current.WriteByte('$')
			started = true
		default:
			current.WriteByte(character)
			started = true
		}
	}
	if inQuote {
		return nil, errors.New("unterminated quote in ExecStart")
	}
	if started {
		args = append(args, current.String())
	}
	return args, nil
}

func startUnit(root, unit string) int {
	if alive(root, unit) {
		return 0
	}
	path, err := unitPath(unit)
	if err != nil {
		fatal(err)
	}
	command, err := execStart(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-systemctl: %s: %v\n", unit, err)
		return 1
	}
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o700); err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "journal"), 0o700); err != nil {
		fatal(err)
	}
	logFile, err := os.OpenFile(logPath(root, unit), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fatal(err)
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "----- fake-systemd start %s %s -----\n", unit, time.Now().UTC().Format(time.RFC3339Nano))

	self, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	args := append([]string{"__supervise", unit, pidPath(root, unit)}, command...)
	shim := exec.Command(self, args...)
	shim.Env = os.Environ()
	shim.Stdout = logFile
	shim.Stderr = logFile
	shim.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := shim.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "fake-systemctl: start %s: %v\n", unit, err)
		return 1
	}
	// The supervisor writes the pid file itself right after it starts the real
	// service. Wait briefly so `systemctl start` returning implies the unit is
	// observable, like real systemd's Type=simple start.
	pid := shim.Process.Pid
	_ = shim.Process.Release()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if readPID(root, unit) != 0 {
			return 0
		}
		if !processExists(pid) {
			fmt.Fprintf(os.Stderr, "fake-systemctl: %s exited before becoming ready\n", unit)
			return 1
		}
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "fake-systemctl: %s did not write a pid file\n", unit)
	return 1
}

// supervise is the long-lived parent of one unit's ExecStart. It stays in its
// own session, starts the service in a child process group, forwards SIGTERM,
// and reaps the child so stop's liveness check cannot get stuck on a zombie.
func supervise(args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "fake-systemd __supervise: usage: <unit> <pid-file> <command...>")
		return 1
	}
	unit, pidFile, command := args[0], args[1], args[2:]
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "fake-systemd __supervise: %v\n", err)
		return 1
	}
	defer os.Remove(pidFile)

	child := exec.Command(command[0], command[1:]...)
	child.Env = os.Environ()
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "fake-systemd __supervise: start %s: %v\n", unit, err)
		return 1
	}

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()

	for {
		select {
		case received := <-signals:
			if received == syscall.SIGHUP {
				continue
			}
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGTERM)
			select {
			case <-done:
				return 0
			case <-time.After(10 * time.Second):
				_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
				<-done
				return 1
			}
		case err := <-done:
			if err == nil {
				return 0
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return exitErr.ExitCode()
			}
			return 1
		}
	}
}

func stopUnit(root, unit string) int {
	pid := readPID(root, unit)
	if pid == 0 {
		_ = os.Remove(pidPath(root, unit))
		return 0
	}
	timeout := 10 * time.Second
	if path, err := unitPath(unit); err == nil {
		timeout = stopTimeout(path)
	}
	// The pid is the supervisor; it owns the service's process group and
	// forwards the signal. SIGTERM to the supervisor group also reaches it.
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			_ = os.Remove(pidPath(root, unit))
			return 0
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for i := 0; i < 200; i++ {
		if !processExists(pid) {
			_ = os.Remove(pidPath(root, unit))
			return 0
		}
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "fake-systemctl: %s did not stop after %s\n", unit, timeout)
	return 1
}

// show answers `show [-p LIST] [--value] UNIT`. The manager always asks for
// LoadState,ActiveState,SubState; the parser also accepts key=value output.
func show(root string, args []string) int {
	properties := "LoadState,ActiveState,SubState"
	valueOnly := false
	var unit string
	for index := 0; index < len(args); index++ {
		switch {
		case args[index] == "-p" || args[index] == "--property":
			if index+1 < len(args) {
				index++
				properties = args[index]
			}
		case strings.HasPrefix(args[index], "--property="):
			properties = strings.TrimPrefix(args[index], "--property=")
		case args[index] == "--value":
			valueOnly = true
		case strings.HasPrefix(args[index], "-"):
			continue
		default:
			unit = args[index]
		}
	}
	if unit == "" {
		fmt.Fprintln(os.Stderr, "fake-systemctl: show requires a unit")
		return 1
	}
	path, err := unitPath(unit)
	if err != nil {
		fatal(err)
	}
	_, statErr := os.Stat(path)
	unitFileExists := statErr == nil
	running := alive(root, unit)
	load := "not-found"
	if unitFileExists || running {
		load = "loaded"
	}
	active := "inactive"
	sub := "dead"
	if running {
		active = "active"
		sub = "running"
	}
	for _, property := range strings.Split(properties, ",") {
		property = strings.TrimSpace(property)
		value := ""
		switch property {
		case "LoadState":
			value = load
		case "ActiveState":
			value = active
		case "SubState":
			value = sub
		case "UnitFileState":
			if _, err := os.Stat(filepath.Join(root, "enabled", unit)); err == nil {
				value = "enabled"
			} else {
				value = "disabled"
			}
		default:
			value = ""
		}
		if valueOnly {
			fmt.Println(value)
		} else {
			fmt.Printf("%s=%s\n", property, value)
		}
	}
	return 0
}

// journalctl prints the per-unit logs created by start. The CLI asks for
// `--user --unit U --lines=100 --no-pager`.
func journalctl(root string, args []string) int {
	var units []string
	for index := 0; index < len(args); index++ {
		switch {
		case args[index] == "--user" || args[index] == "--no-pager":
			continue
		case args[index] == "--unit" || args[index] == "-u":
			if index+1 < len(args) {
				index++
				units = append(units, args[index])
			}
		case strings.HasPrefix(args[index], "--unit="):
			units = append(units, strings.TrimPrefix(args[index], "--unit="))
		case strings.HasPrefix(args[index], "--lines=") || strings.HasPrefix(args[index], "-n"):
			continue
		case args[index] == "--follow" || args[index] == "-f":
			// The lifecycle test never follows; returning immediately is
			// safer than hanging the fake journal forever.
			continue
		}
	}
	if len(units) == 0 {
		fmt.Fprintln(os.Stderr, "fake-journalctl: expected --unit")
		return 1
	}
	for _, unit := range units {
		data, err := os.ReadFile(logPath(root, unit))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			fatal(err)
		}
		_, _ = os.Stdout.Write(data)
	}
	return 0
}

func loginctl(root string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "fake-loginctl: missing verb")
		return 1
	}
	switch args[0] {
	case "enable-linger":
		if err := os.WriteFile(filepath.Join(root, "linger"), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
			fatal(err)
		}
		return 0
	case "disable-linger":
		_ = os.Remove(filepath.Join(root, "linger"))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "fake-loginctl: unsupported verb %q\n", args[0])
		return 1
	}
}
