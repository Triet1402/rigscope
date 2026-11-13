package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/admin"
	"github.com/Triet1402/rigscope/internal/hub/alerts"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/server"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/platform/lock"
)

// hubLockFile serialises the running hub and offline recovery. A second hub
// start, or `rigscope reset-password` while the hub runs, fails instead of
// racing the database.
const hubLockFile = config.HubLockFile

// runHub runs the hub runtime. The long-running form is what systemd starts:
//
//	rigscope hub --config <file> --state-dir <dir>
//
// It is the single composition root for the Go hub: one installation lock, one
// SQLite handle shared by auth, admin, the collector registry, telemetry, live
// state and history, one hub run id and one HTTP server.
func runHub(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("hub", deps.Stderr)
	configFlag := flags.String("config", "", "configuration file")
	stateFlag := flags.String("state-dir", "", "hub state directory")
	system := flags.Bool("system", false, "use system paths")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(deps.Stderr, "rigscope: hub: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	configPath, stateDir, err := hubPaths(deps, *configFlag, *stateFlag, *system)
	if err != nil {
		return deps.errorf("hub: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return deps.errorf("%v", err)
	}
	hubScope := config.ScopeUser
	if systemPaths, err := deps.paths(config.ScopeSystem); *system || (err == nil && systemPaths.ConfigFile == configPath) || filepath.Dir(configPath) == "/etc/rigscope" {
		hubScope = config.ScopeSystem
	}
	// The runtime test harness (and a systemd unit with a dynamic port) may
	// override the persisted listener; 0 selects an ephemeral port.
	if override := os.Getenv("RIGSCOPE_HUB_PORT"); override != "" {
		var port int
		if _, err := fmt.Sscanf(override, "%d", &port); err != nil || port < 0 || port > 65535 {
			return deps.errorf("hub: invalid RIGSCOPE_HUB_PORT %q", override)
		}
		cfg.Components.Hub.Port = port
	}
	if !cfg.Components.Hub.Enabled {
		return deps.errorf("hub: the hub component is disabled in %s", configPath)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return deps.errorf("hub: create state directory %s: %v", stateDir, err)
	}

	held, err := lock.Acquire(hubLeasePath(stateDir))
	if err != nil {
		if errors.Is(err, lock.ErrBusy) {
			return deps.errorf("hub: another hub or offline recovery holds %s", hubLeasePath(stateDir))
		}
		return deps.errorf("hub: %v", err)
	}
	defer func() { _ = held.Release() }()

	// One database handle for every repository in this process.
	database, err := storage.Open(ctx, hubDatabasePath(stateDir), storage.Options{})
	if err != nil {
		return deps.errorf("hub: open database: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(deps.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	hubConfig := cfg.Components.Hub
	hubRunID, err := newHubRunID()
	if err != nil {
		return deps.errorf("hub: generate run id: %v", err)
	}

	// Build the HTTP foundation first: a broken deployment (missing UI export,
	// invalid TLS pair) must fail before an owner account is created and its
	// password printed.
	runtime, err := server.New(server.Options{
		Hub:      hubConfig,
		StateDir: stateDir,
		Version:  deps.buildVersion(),
		Logger:   logger,
		HubRunID: hubRunID,
	})
	if err != nil {
		_ = database.Close()
		return deps.errorf("hub: %v", err)
	}

	users := storage.NewUsers(database)
	password, err := bootstrapBrowserUsers(ctx, users, stateDir)
	if err != nil {
		_ = database.Close()
		return deps.errorf("hub: bootstrap browser accounts: %v", err)
	}
	if password != "" {
		fmt.Fprintf(deps.Stdout, "Username: owner\nPassword: %s\nSave this password; it is shown once.\n", password)
	}

	originRules, err := auth.NewOriginRules(hubConfig.Origin, hubConfig.Listen, hubConfig.Port, hubConfig.TLSCert != "")
	if err != nil {
		_ = database.Close()
		return deps.errorf("hub: %v", err)
	}
	manager, err := auth.New(auth.Options{
		Users:    users,
		Sessions: storage.NewSessions(database),
		Origin:   originRules,
		Logger:   logger,
		// The listener's effective scheme includes a trusted reverse proxy.
		Secure: func(r *http.Request) bool { return server.Scheme(r) == "https" },
	})
	if err != nil {
		_ = database.Close()
		return deps.errorf("hub: %v", err)
	}
	defer func() { _ = manager.Close() }()
	authenticator := auth.NewLiveAuthenticator(manager)
	defer authenticator.Close()

	services, err := newHubServices(ctx, database, stateDir, hubRunID, logger, authenticator, cfg.Components.Hub)
	if err != nil {
		_ = database.Close()
		return deps.errorf("hub: %v", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := services.Close(closeCtx); err != nil {
			logger.Warn("hub services did not close cleanly", "error", err)
		}
	}()

	adminHandler, err := admin.New(admin.Options{
		Users:    users,
		Audit:    storage.NewAudit(database),
		Machines: services.machines,
		Registry: services.registry,
		Auth:     manager,
		MachineExists: func(ctx context.Context, machineID string) (bool, error) {
			if _, ok := services.state.Machine(machineID); ok {
				return true, nil
			}
			collectors, err := services.registry.List(ctx)
			if err != nil {
				return false, err
			}
			for _, collector := range collectors {
				if collector.MachineID == machineID {
					return true, nil
				}
			}
			return false, nil
		},
		OnCollectorRevoked: func(collectorID string) {
			// Revocation removes the live machine row and closes the socket.
			// Alert rules, incidents and the per-machine progress row are
			// durable machine history and stay, matching the Node hub; a later
			// re-enrollment rebinds progress by run/sequence instead of
			// starting over.
			services.gateway.RevokeCollector(collectorID)
		},
		OnMachineRemoved: func(ctx context.Context, machineID string) error {
			// Removal is offered only for revoked collectors, whose live row
			// and socket are already gone; this deletes the stored history.
			if err := services.telemetry.ForgetMachine(ctx, machineID); err != nil {
				return err
			}
			if services.alerts != nil {
				services.alerts.engine.ForgetMachine(machineID)
			}
			return nil
		},
		OnMachineMetaChanged: func(machineID string) {
			meta, err := services.machines.Get(context.Background(), machineID)
			if err != nil {
				logger.Warn("read machine metadata for live update", "machineId", machineID, "error", err)
				return
			}
			services.state.SetMetadata(machineID, meta.DisplayName, meta.Tags)
		},
		AlertDiagnostics: services.alerts.APIDiagnostics,
		Settings: func(context.Context) (api.HubSettings, error) {
			return hubSettingsFromFile(hubScope, configPath, cfg, database, services.telemetry, time.Now())
		},
		Logger: logger,
	})
	if err != nil {
		return deps.errorf("hub: %v", err)
	}

	authHandler := auth.NewHandler(manager)
	if err := runtime.Mount("/api/auth/", authHandler); err != nil {
		return deps.errorf("hub: %v", err)
	}
	if err := runtime.Mount("/api/admin/", adminHandler); err != nil {
		return deps.errorf("hub: %v", err)
	}
	if err := runtime.Mount("/api/machines/meta", adminHandler); err != nil {
		return deps.errorf("hub: %v", err)
	}
	alertsHandler, err := alerts.NewHandler(alerts.HTTPOptions{
		Engine: services.alerts.engine,
		Auth:   manager,
	})
	if err != nil {
		return deps.errorf("hub: %v", err)
	}
	if err := runtime.Mount("/api/alerts", alertsHandler); err != nil {
		return deps.errorf("hub: %v", err)
	}
	if err := services.mount(runtime); err != nil {
		return deps.errorf("hub: %v", err)
	}
	if err := runtime.Listen(); err != nil {
		return deps.errorf("hub: %v", err)
	}
	// Port 0 selects an ephemeral port; same-origin checks must use the port
	// actually bound.
	if address, ok := runtime.Addr().(*net.TCPAddr); ok {
		originRules.SetPort(address.Port)
	}
	if public := runtime.URL(); public != runtime.ListenURL() {
		fmt.Fprintf(deps.Stdout, "hub listening on %s (public address %s)\n", runtime.ListenURL(), public)
	} else {
		fmt.Fprintf(deps.Stdout, "hub listening on %s\n", public)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- runtime.Serve() }()

	select {
	case err := <-serveErr:
		if err != nil {
			return deps.errorf("hub: %v", err)
		}
		return 0
	case <-ctx.Done():
		logger.Info("hub shutting down")
		// Stop accepting, then stop alert evaluation and end SSE/WebSocket
		// streams before draining HTTP. The deferred Close flushes telemetry
		// and closes the one database handle after the engine has drained.
		if err := runtime.StopAccepting(); err != nil {
			logger.Warn("hub stopped accepting with an error", "error", err)
		}
		services.alerts.Stop()
		services.StopStreams()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := runtime.Shutdown(shutdownCtx); err != nil {
			return deps.errorf("hub: shutdown: %v", err)
		}
		// Serve returns after the listener closes; wait briefly so a listener
		// error is still reported, but do not hang a supervised stop.
		select {
		case err := <-serveErr:
			if err != nil {
				return deps.errorf("hub: %v", err)
			}
		case <-time.After(time.Second):
		}
		return 0
	}
}

// hubPaths resolves the hub configuration and state directory. A configuration
// under /etc/rigscope selects the system layout even without --system, so the
// service unit does not need a second flag.
func hubPaths(deps Deps, configFlag, stateFlag string, system bool) (string, string, error) {
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
		stateDir = paths.ComponentStateDir("hub")
	}
	return configPath, stateDir, nil
}
