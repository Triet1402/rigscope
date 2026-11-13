package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/enroll"
	"github.com/Triet1402/rigscope/internal/collector/runtime"
	"github.com/Triet1402/rigscope/internal/collector/state"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// runCollector runs the collector runtime, or one of its subcommands. The
// long-running form is what systemd starts:
//
//	rigscope collector --config <file> --state-dir <dir>
//
// `enroll` performs the explicit enrollment operation and exits; `--check` is
// the readiness probe used by the upgrade transaction.
func runCollector(ctx context.Context, deps Deps, args []string) int {
	if len(args) > 0 && args[0] == "enroll" {
		return runCollectorEnroll(ctx, deps, args[1:])
	}
	flags := newFlagSet("collector", deps.Stderr)
	configFlag := flags.String("config", "", "configuration file")
	stateFlag := flags.String("state-dir", "", "collector state directory")
	hostname := flags.String("hostname", "", "hostname advertised to the hub (default: the machine hostname)")
	check := flags.Bool("check", false, "check readiness and exit")
	system := flags.Bool("system", false, "use system paths")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(deps.Stderr, "rigscope: collector: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	configPath, stateDir, err := collectorPaths(deps, *configFlag, *stateFlag, *system)
	if err != nil {
		return deps.errorf("collector: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return deps.errorf("%v", err)
	}
	if *check {
		return runCollectorCheck(deps, cfg, stateDir)
	}
	store, err := openCollectorStore(deps, stateDir)
	if err != nil {
		return deps.errorf("collector: %v", err)
	}
	collector, err := runtime.New(runtime.Options{
		Config:   cfg,
		StateDir: stateDir,
		Store:    store,
		Hostname: *hostname,
		Logf:     jsonLogger(deps.Stderr),
		OnReady: func(hubURL string) {
			// One plain readiness line on stdout: structured logs stay on
			// stderr for journald, and a supervisor can still observe the
			// first successful session without parsing JSON.
			fmt.Fprintf(deps.Stdout, "collector ready: welcomed by hub %s\n", hubURL)
		},
	})
	if err != nil {
		return deps.errorf("collector: %v", err)
	}
	if err := collector.Run(ctx); err != nil {
		return deps.errorf("collector: %v", err)
	}
	return 0
}

// runCollectorCheck implements `rigscope collector --check`: the upgrade
// transaction's readiness probe. Exit 0 means the configuration loaded
// strictly and the collector state is readable.
func runCollectorCheck(deps Deps, cfg *config.Config, stateDir string) int {
	result, err := runtime.Check(cfg, stateDir)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "rigscope: collector is not ready: %v\n", err)
		return 1
	}
	status := "awaiting enrollment"
	switch {
	case !result.Enabled:
		status = "disabled"
	case result.Enrolled:
		status = "enrolled"
	case result.PendingGrant:
		status = "enrollment grant staged"
	}
	if result.Destination == "" {
		result.Destination = "(none)"
	}
	fmt.Fprintf(deps.Stdout, "collector check: ok (%s, destination %s, state %s)\n", status, result.Destination, stateDir)
	return 0
}

// runCollectorEnroll performs the explicit enrollment operation. The grant is
// either passed with --grant or read from the installer's pending grant file,
// so `install --hub URL --token TOKEN` followed by this command needs no
// secret on the command line.
func runCollectorEnroll(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("collector enroll", deps.Stderr)
	configFlag := flags.String("config", "", "configuration file")
	stateFlag := flags.String("state-dir", "", "collector state directory")
	hubURL := flags.String("hub-url", "", "collector WebSocket URL of the hub")
	grant := flags.String("grant", "", "one-time enrollment grant")
	caFile := flags.String("ca-file", "", "PEM CA bundle that pins the hub certificate")
	system := flags.Bool("system", false, "use system paths")
	yes := addYesFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(deps.Stderr, "rigscope: collector enroll: unexpected argument %q\n", flags.Arg(0))
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
	configPath, stateDir, err := collectorPaths(deps, *configFlag, *stateFlag, *system)
	if err != nil {
		return deps.errorf("collector enroll: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return deps.errorf("%v", err)
	}
	destinationURL, destinationCA := "", ""
	if destinations := cfg.Components.Collector.Destinations; len(destinations) == 1 {
		destinationURL = destinations[0].URL
		destinationCA = destinations[0].CAFile
	}
	url := *hubURL
	if url != "" {
		// Accept the hub address the operator types (`https://host`) as well
		// as the stored WebSocket endpoint.
		normalized, err := config.NormalizeHubURL(url)
		if err != nil {
			return deps.errorf("collector enroll: %v", err)
		}
		url = normalized
	}
	if url == "" {
		url = destinationURL
	}
	if url == "" {
		return deps.errorf("collector enroll: no hub URL: pass --hub-url, or record a destination with rigscope install --hub URL --token TOKEN")
	}
	pin := *caFile
	if pin == "" && url == destinationURL {
		pin = destinationCA
	}
	store, err := openCollectorStore(deps, stateDir)
	if err != nil {
		return deps.errorf("collector enroll: %v", err)
	}
	token := *grant
	if token == "" {
		pending, ok, err := store.PendingEnrollment(url)
		if err != nil {
			return deps.errorf("collector enroll: %v", err)
		}
		if ok && pending.Grant != "" {
			token = pending.Grant
		} else {
			// Nothing staged: ask for the grant from the hub's Add machine
			// panel. The input is hidden like a password; it is a secret.
			pasted, err := askForGrant(deps)
			if err != nil {
				return deps.errorf("collector enroll: no enrollment grant for %s: pass --grant, or stage one with rigscope install --hub URL --token TOKEN", url)
			}
			token = pasted
		}
	}
	result, err := deps.enroll(ctx, enroll.Options{
		Store:  store,
		HubURL: url,
		Grant:  token,
		CAFile: pin,
		Logf:   printfLogger(deps.Stderr, "enroll"),
	})
	if err != nil {
		return deps.errorf("collector enroll: %v", err)
	}
	// The credential itself is never printed: it lives only in the private
	// state file and the hub's authorization header.
	identity, err := store.Identity()
	if err != nil {
		return deps.errorf("collector enroll: credential stored but the local identity is unreadable: %v", err)
	}
	fmt.Fprintf(deps.Stdout, "collector enrollment saved: collectorId=%s machineId=%s hub=%s\n",
		identity.CollectorID, result.MachineID, url)
	return restartCollectorForCredential(ctx, deps, *system || filepath.Dir(configPath) == "/etc/rigscope", *yes)
}

// restartCollectorForCredential restarts a running collector service so it
// uses the credential just saved: the service reads it only when a session
// starts, and after a revocation it waits minutes before trying again.
func restartCollectorForCredential(ctx context.Context, deps Deps, system bool, assumeYes bool) int {
	manager := deps.manager(scopeFrom(system))
	running := activeComponents(ctx, manager, []systemd.Component{systemd.Collector})
	if len(running) == 0 {
		return 0
	}
	if err := deps.approveServiceStop(running, "restart to use the new credential", assumeYes); err != nil {
		deps.warnf(deps.Stderr, "the running collector keeps its old credential until rigscope restart collector (%v)", err)
		return 0
	}
	if err := manager.Restart(ctx, systemd.Collector.UnitName()); err != nil {
		return deps.errorf("collector enroll: credential saved, but restart failed: %v; run rigscope restart collector", err)
	}
	fmt.Fprintf(deps.Stdout, "  %s collector\n", deps.out(styleGreen, "restarted:"))
	return 0
}

// collectorPaths resolves the configuration and state paths. When the
// configuration lives under /etc/rigscope the installation is a system one
// even though the unit runs as the unprivileged collector account, so state
// resolves under /var/lib/rigscope.
func collectorPaths(deps Deps, configFlag, stateFlag string, system bool) (string, string, error) {
	scope := scopeFrom(system)
	configPath := configFlag
	if configPath == "" {
		paths, err := deps.paths(scope)
		if err != nil {
			return "", "", err
		}
		configPath = paths.ConfigFile
	} else if !system && filepath.Dir(configPath) == "/etc/rigscope" {
		scope = config.ScopeSystem
	}
	stateDir := stateFlag
	if stateDir == "" {
		paths, err := deps.paths(scope)
		if err != nil {
			return "", "", err
		}
		stateDir = paths.StateDir
	}
	return configPath, stateDir, nil
}

// jsonLogger encodes structured events as one JSON object per line. journald
// keeps the fields; a human can still read level and message.
func jsonLogger(output io.Writer) runtime.LogFunc {
	var mu sync.Mutex
	return func(event runtime.Event) {
		record := make(map[string]any, len(event.Fields)+3)
		record["time"] = time.Now().UTC().Format(time.RFC3339Nano)
		record["level"] = event.Level
		record["msg"] = event.Message
		for _, field := range event.Fields {
			record[field.Key] = field.Value
		}
		data, err := json.Marshal(record)
		if err != nil {
			fmt.Fprintf(output, "%s %s\n", event.Level, event.Message)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(output, "%s\n", data)
	}
}

// printfLogger adapts a printf-style callback to a JSON log line.
func printfLogger(output io.Writer, component string) func(string, ...any) {
	logf := jsonLogger(output)
	return func(format string, args ...any) {
		logf(runtime.Event{Level: "info", Message: fmt.Sprintf(format, args...), Fields: []runtime.Field{runtime.F("component", component)}})
	}
}

// openCollectorStore opens collector state for a CLI command. Run as root on
// a directory the collector account owns (a system install), every file the
// command writes gets the directory's owner, so the service can still read
// its own credential afterwards.
func openCollectorStore(deps Deps, stateDir string) (*state.Store, error) {
	options := state.Options{}
	if deps.euid() == 0 {
		info, err := os.Stat(filepath.Join(stateDir, "collector"))
		if err == nil {
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != 0 {
				uid, gid := int(stat.Uid), int(stat.Gid)
				options.Owner = func(file *os.File) error {
					if err := file.Chown(uid, gid); err != nil {
						return fmt.Errorf("set owner of %s: %w", file.Name(), err)
					}
					return nil
				}
			}
		}
	}
	return state.Open(stateDir, options)
}

// askForGrant reads a pasted enrollment grant from the terminal. It fails when
// there is no terminal or nothing was pasted.
func askForGrant(deps Deps) (string, error) {
	if deps.ReadPassword == nil {
		return "", errNoTerminal
	}
	fmt.Fprintln(deps.Stderr, "Create a grant in the hub under Administration > Collectors, then paste it here.")
	grant, err := deps.ReadPassword("Grant (input hidden): ")
	if err != nil {
		return "", err
	}
	grant = strings.TrimSpace(grant)
	if grant == "" {
		return "", errors.New("no grant was pasted")
	}
	return grant, nil
}
