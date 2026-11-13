package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

type componentStatus struct {
	Enabled      bool   `json:"enabled"`
	Unit         string `json:"unit"`
	Installed    bool   `json:"installed"`
	Active       bool   `json:"active"`
	Destinations int    `json:"destinations,omitempty"`
}

type statusReport struct {
	Scope      string                     `json:"scope"`
	Version    string                     `json:"version"`
	Installed  bool                       `json:"installed"`
	Config     string                     `json:"config"`
	ConfigRead bool                       `json:"configRead"`
	StateDir   string                     `json:"stateDir"`
	Binary     string                     `json:"binary"`
	BinarySet  bool                       `json:"binaryPresent"`
	Previous   bool                       `json:"previousBinary"`
	Components map[string]componentStatus `json:"components"`
}

func runStatus(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("status", deps.Stderr)
	asJSON := flags.Bool("json", false, "print machine-readable JSON")
	system := flags.Bool("system", false, "inspect root-owned system services")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		return deps.errorf("status takes no positional arguments, got %q", flags.Arg(0))
	}
	scope := deps.installedScope(*system)
	paths, err := deps.paths(scope)
	if err != nil {
		return deps.errorf("%v", err)
	}
	cfg, configErr := config.Load(paths.ConfigFile)
	report := statusReport{
		Scope:      scopeLabel(scope),
		Version:    deps.buildVersion(),
		Config:     paths.ConfigFile,
		ConfigRead: configErr == nil,
		StateDir:   paths.StateDir,
		Binary:     paths.Binary,
		Components: map[string]componentStatus{},
	}
	if _, err := os.Stat(paths.Binary); err == nil {
		report.BinarySet = true
	}
	if _, err := os.Stat(paths.PreviousBinary); err == nil {
		report.Previous = true
	}
	manager := deps.manager(scope)
	for _, component := range installComponents {
		unit := component.UnitName()
		_, unitErr := os.Stat(paths.UnitPath(unit))
		installed := unitErr == nil
		status := componentStatus{
			Unit:      unit,
			Installed: installed,
		}
		if cfg != nil {
			status.Enabled = componentEnabled(cfg, component)
		}
		if installed {
			active, err := manager.IsActive(ctx, unit)
			if err == nil {
				status.Active = active
			}
		}
		if component == systemd.Collector && cfg != nil {
			status.Destinations = len(cfg.Components.Collector.Destinations)
		}
		report.Components[component.String()] = status
		if installed {
			report.Installed = true
		}
	}
	if cfg != nil {
		report.Installed = true
	}

	if *asJSON {
		encoder := json.NewEncoder(deps.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return deps.errorf("encode status: %v", err)
		}
		return 0
	}

	fmt.Fprintf(deps.Stdout, "%s %s (%s scope)\n", deps.out(styleBold, "Rigscope"), report.Version, report.Scope)
	fmt.Fprintf(deps.Stdout, "  config: %s%s\n", report.Config, missingSuffix(report.ConfigRead, configErr))
	fmt.Fprintf(deps.Stdout, "  state:  %s\n", report.StateDir)
	binary := "missing"
	if report.BinarySet {
		binary = "installed"
		if report.Previous {
			binary += ", previous retained"
		}
	}
	fmt.Fprintf(deps.Stdout, "  binary: %s (%s)\n", report.Binary, binary)
	fmt.Fprintln(deps.Stdout, "Components:")
	for _, component := range installComponents {
		status := report.Components[component.String()]
		name := deps.out(styleBold, fmt.Sprintf("%-10s", component))
		if !status.Installed && !status.Enabled {
			fmt.Fprintf(deps.Stdout, "  %s %s\n", name, deps.out(styleDim, "not installed"))
			continue
		}
		state := deps.out(styleDim, "disabled")
		if status.Enabled {
			state = "enabled"
		}
		if status.Active {
			state += ", " + deps.out(styleGreen, "active")
		} else if status.Enabled {
			state += ", " + deps.out(styleYellow, "inactive")
		} else {
			state += ", inactive"
		}
		if component == systemd.Collector && status.Destinations > 0 {
			state += fmt.Sprintf(", %d destination(s)", status.Destinations)
		}
		fmt.Fprintf(deps.Stdout, "  %s %s\n", name, state)
	}
	return 0
}

func missingSuffix(ok bool, err error) string {
	if ok {
		return ""
	}
	if err == nil {
		return " (not installed)"
	}
	return " (" + err.Error() + ")"
}

func runService(ctx context.Context, deps Deps, verb string, args []string) int {
	flags := newFlagSet(verb, deps.Stderr)
	system := flags.Bool("system", false, "control root-owned system services")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 {
		return deps.errorf("%s takes at most one component", verb)
	}
	scope := deps.installedScope(*system)
	if code, done := deps.ensureRoot(scope); done {
		return code
	}
	paths, err := deps.paths(scope)
	if err != nil {
		return deps.errorf("%v", err)
	}
	targets, err := selectedComponents(deps, paths, flags.Args())
	if err != nil {
		return deps.errorf("%v", err)
	}
	if verb != "stop" {
		for _, component := range targets {
			if _, err := os.Stat(paths.UnitPath(component.UnitName())); err != nil {
				return deps.errorf("%s is not installed; run rigscope install first", component.UnitName())
			}
		}
	}
	held, err := acquireLock(paths)
	if err != nil {
		return deps.errorf("%v", err)
	}
	defer held.Release()
	manager := deps.manager(scope)
	for _, component := range targets {
		unit := component.UnitName()
		var err error
		switch verb {
		case "start":
			// systemctl start does nothing for an active unit; say so rather
			// than report a start that did not happen.
			if active, activeErr := manager.IsActive(ctx, unit); activeErr == nil && active {
				fmt.Fprintf(deps.Stdout, "%s %s (rigscope restart %s applies new settings or credentials)\n", deps.out(styleDim, "Already running:"), unit, component)
				continue
			}
			err = manager.Start(ctx, unit)
		case "stop":
			err = manager.Stop(ctx, unit)
		case "restart":
			err = manager.Restart(ctx, unit)
		default:
			return deps.errorf("unknown service action %q", verb)
		}
		if err != nil {
			return deps.errorf("%s %s: %v", verb, unit, err)
		}
		fmt.Fprintf(deps.Stdout, "%s %s\n", deps.out(styleGreen, pastTense(verb)), unit)
	}
	return 0
}

func pastTense(verb string) string {
	switch verb {
	case "start":
		return "Started"
	case "stop":
		return "Stopped"
	default:
		return "Restarted"
	}
}

// selectedComponents returns an explicitly named component or every enabled
// one when no component is named.
func selectedComponents(deps Deps, paths config.Paths, args []string) ([]systemd.Component, error) {
	if len(args) == 1 {
		component, ok := systemd.ParseComponent(args[0])
		if !ok {
			return nil, fmt.Errorf("unknown component %q; use hub or collector", args[0])
		}
		return []systemd.Component{component}, nil
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return nil, fmt.Errorf("load configuration: %w", err)
	}
	var targets []systemd.Component
	for _, component := range installComponents {
		if componentEnabled(cfg, component) {
			targets = append(targets, component)
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("no components are enabled")
	}
	return targets, nil
}

func runLogs(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("logs", deps.Stderr)
	follow := flags.Bool("follow", false, "follow new log lines")
	system := flags.Bool("system", false, "read root-owned system services")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 {
		return deps.errorf("logs takes at most one component")
	}
	scope := deps.installedScope(*system)
	paths, err := deps.paths(scope)
	if err != nil {
		return deps.errorf("%v", err)
	}
	targets, err := selectedComponents(deps, paths, flags.Args())
	if err != nil {
		return deps.errorf("%v", err)
	}
	journalArgs := make([]string, 0, 8)
	if scope == config.ScopeUser {
		journalArgs = append(journalArgs, "--user")
	}
	for _, component := range targets {
		journalArgs = append(journalArgs, "--unit", component.UnitName())
	}
	journalArgs = append(journalArgs, "--lines=100", "--no-pager")
	if *follow {
		journalArgs = append(journalArgs, "--follow")
	}
	runStream := deps.RunStream
	if runStream == nil {
		return deps.errorf("log streaming is unavailable in this build")
	}
	if err := runStream(ctx, "journalctl", journalArgs...); err != nil {
		if ctx.Err() != nil {
			return 0
		}
		return deps.errorf("journalctl: %v", err)
	}
	return 0
}
