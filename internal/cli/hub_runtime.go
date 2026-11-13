package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/alerts"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/history"
	"github.com/Triet1402/rigscope/internal/hub/ingest"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/server"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// bootstrapFileName is the private file an installation writes before its
// first hub start. The storage registry imports it exactly once, so a later
// start never resurrects a revoked or deleted collector.
const bootstrapFileName = "collectors.json"

// browserBootstrapFileName is the private file that seeds the browser account
// directory on first start. It carries password hashes, never plaintext, and is
// imported exactly once like the collector directory. Production installs the
// owner account through auth.Bootstrap; the file exists so an installation (or
// the runtime harness) can supply a prepared directory.
const browserBootstrapFileName = "users.json"

// pairingBootstrapFileName is the hub-owned file the installer writes for an
// automatic colocated pairing. It contains only a SHA-256 grant hash, its
// expiry and the intended use. The hub imports it before listening and deletes
// it, so the plaintext token is never stored outside the collector's private
// enrollment file.
const pairingBootstrapFileName = "collector-pairing.json"

// maxPairingBootstrapBytes bounds the tiny one-grant file.
const maxPairingBootstrapBytes = 16 * 1024

// hubDatabaseFile is the SQLite database inside the hub state directory. Auth,
// admin, the collector registry, telemetry, live and history share this one
// handle in the running hub; reset-password opens the same file offline.
const hubDatabaseFile = "hub.db"

// bootstrapFile is the on-disk shape of the collector directory and pending
// enrollment grants. Tokens are secrets: the file is written 0600.
type bootstrapFile struct {
	Collectors []struct {
		CollectorID string `json:"collectorId"`
		MachineID   string `json:"machineId"`
		Token       string `json:"token"`
	} `json:"collectors"`
	Grants []struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	} `json:"grants"`
}

// pairingBootstrapFile is the on-disk shape of the automatic colocated
// pairing grant. It deliberately carries no plaintext token.
type pairingBootstrapFile struct {
	Grants []pairingBootstrapGrant `json:"grants"`
}

type pairingBootstrapGrant struct {
	TokenHash   string `json:"tokenHash"`
	ExpiresAt   string `json:"expiresAt"`
	Use         string `json:"use"`
	CollectorID string `json:"collectorId"`
}

// browserBootstrapFile is the on-disk shape of the one-time browser account
// import. Hashes are opaque to storage but must be auth's argon2id format.
type browserBootstrapFile struct {
	Users []struct {
		Username     string           `json:"username"`
		PasswordHash string           `json:"passwordHash"`
		MachineIDs   api.MachineScope `json:"machineIds"`
		Admin        bool             `json:"admin"`
	} `json:"users"`
}

// loadBootstrap reads the private bootstrap file, if present.
func loadBootstrap(stateDir string) (storage.RegistryOptions, error) {
	var options storage.RegistryOptions
	path := filepath.Join(stateDir, bootstrapFileName)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return options, nil
	}
	if err != nil {
		return options, fmt.Errorf("read collector bootstrap file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return options, fmt.Errorf("collector bootstrap file %s must not be group or world accessible", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return options, fmt.Errorf("read collector bootstrap file: %w", err)
	}
	var parsed bootstrapFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return options, fmt.Errorf("parse collector bootstrap file: %w", err)
	}
	for _, entry := range parsed.Collectors {
		options.BootstrapIdentities = append(options.BootstrapIdentities, storage.Identity{
			CollectorID: entry.CollectorID,
			MachineID:   entry.MachineID,
			Token:       entry.Token,
		})
	}
	for _, entry := range parsed.Grants {
		expiresAt, err := time.Parse(time.RFC3339, entry.ExpiresAt)
		if err != nil {
			return options, fmt.Errorf("collector bootstrap grant expiry: %w", err)
		}
		options.Grants = append(options.Grants, storage.EnrollmentGrant{
			Token:     entry.Token,
			ExpiresAt: expiresAt,
		})
	}
	return options, nil
}

// pairingBootstrap is one loaded automatic-colocated-pairing file. It keeps the
// identity of the exact file that was read so the importer only deletes the file
// it imported, even if the pathname was replaced meanwhile.
type pairingBootstrap struct {
	grants   []storage.BootstrapGrant
	path     string
	identity fsutil.Identity
}

// loadPairingBootstrap reads the automatic colocated pairing file, if present.
// The read is anchored to a no-follow directory descriptor: the file must be a
// regular, private file and is read through the same descriptor that was
// validated, with a hard size bound. A file that is group- or world-accessible,
// not a regular file, too large or not strictly shaped is refused; it is never
// silently ignored.
func loadPairingBootstrap(stateDir string) (*pairingBootstrap, error) {
	dir, err := fsutil.OpenDirNoFollow(stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open hub state directory %s: %w", stateDir, err)
	}
	defer dir.Close()
	raw, identity, err := dir.ReadRegular(pairingBootstrapFileName, maxPairingBootstrapBytes, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read collector pairing bootstrap file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var parsed pairingBootstrapFile
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse collector pairing bootstrap file: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("collector pairing bootstrap file %s contains more than one JSON value", filepath.Join(stateDir, pairingBootstrapFileName))
	}
	grants := make([]storage.BootstrapGrant, 0, len(parsed.Grants))
	for _, entry := range parsed.Grants {
		expiresAt, err := time.Parse(time.RFC3339, entry.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("collector pairing bootstrap grant expiry: %w", err)
		}
		grants = append(grants, storage.BootstrapGrant{
			TokenHash:   entry.TokenHash,
			ExpiresAt:   expiresAt,
			Use:         entry.Use,
			CollectorID: entry.CollectorID,
		})
	}
	return &pairingBootstrap{
		grants:   grants,
		path:     filepath.Join(stateDir, pairingBootstrapFileName),
		identity: identity,
	}, nil
}

// remove deletes the imported file relative to a verified parent descriptor. A
// file that has been replaced since it was read is left alone: it is a
// different file, and the next enrollment request imports it under the same
// bounded, validated path.
func (p *pairingBootstrap) remove() error {
	if p == nil {
		return nil
	}
	dir, err := fsutil.OpenDirNoFollow(filepath.Dir(p.path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove collector pairing bootstrap file: %w", err)
	}
	defer dir.Close()
	name := filepath.Base(p.path)
	current, err := dir.Stat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove collector pairing bootstrap file: %w", err)
	}
	if !current.SameFile(p.identity) {
		return nil
	}
	if err := dir.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove collector pairing bootstrap file: %w", err)
	}
	return nil
}

// pairingImporter imports a pairing bootstrap file that appears while the hub
// is already running. The installer may add the collector to an active hub, in
// which case the hub never restarted and its startup import could not see the
// new grant. Serialized so two enrollment requests cannot race the deletion.
type pairingImporter struct {
	mu       sync.Mutex
	stateDir string
	registry *storage.Registry
}

func newPairingImporter(stateDir string, registry *storage.Registry) *pairingImporter {
	return &pairingImporter{stateDir: stateDir, registry: registry}
}

func (p *pairingImporter) importPending(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	loaded, err := loadPairingBootstrap(p.stateDir)
	if err != nil {
		return err
	}
	if loaded == nil {
		return nil
	}
	if err := p.registry.ImportBootstrapGrants(ctx, loaded.grants); err != nil {
		return err
	}
	return loaded.remove()
}

// openRegistryWithBootstrap imports every bootstrap source before the hub
// listens. The pairing file is removed only after the registry transaction
// succeeds; an import crash leaves it in place for the next start, and the
// registry's INSERT OR IGNORE makes that retry harmless.
func openRegistryWithBootstrap(ctx context.Context, db *storage.Database, stateDir string) (*storage.Registry, error) {
	bootstrap, err := loadBootstrap(stateDir)
	if err != nil {
		return nil, err
	}
	pairing, err := loadPairingBootstrap(stateDir)
	if err != nil {
		return nil, err
	}
	if pairing != nil {
		bootstrap.BootstrapGrants = pairing.grants
	}
	registry, err := storage.NewRegistry(ctx, db, bootstrap)
	if err != nil {
		return nil, err
	}
	if err := pairing.remove(); err != nil {
		return nil, err
	}
	return registry, nil
}

// loadBrowserBootstrap reads the one-time browser account import, if present.
func loadBrowserBootstrap(stateDir string) ([]storage.BootstrapUser, bool, error) {
	path := filepath.Join(stateDir, browserBootstrapFileName)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read browser bootstrap file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, false, fmt.Errorf("browser bootstrap file %s must not be group or world accessible", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read browser bootstrap file: %w", err)
	}
	var parsed browserBootstrapFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, false, fmt.Errorf("parse browser bootstrap file: %w", err)
	}
	users := make([]storage.BootstrapUser, 0, len(parsed.Users))
	for _, entry := range parsed.Users {
		if err := auth.ValidatePasswordHash(entry.PasswordHash); err != nil {
			return nil, false, fmt.Errorf("browser bootstrap user %q: %w", entry.Username, err)
		}
		scope := storage.MachineScope(entry.MachineIDs.IDs...)
		if entry.MachineIDs.All {
			scope = storage.AllMachines()
		}
		users = append(users, storage.BootstrapUser{
			Username:     entry.Username,
			PasswordHash: entry.PasswordHash,
			Scope:        scope,
			Admin:        entry.Admin,
		})
	}
	return users, true, nil
}

// bootstrapBrowserUsers seeds the browser directory once. The owner password is
// returned only when this call created it and only after the HTTP server has
// already been built, so a broken deployment never prints a credential.
func bootstrapBrowserUsers(ctx context.Context, users *storage.Users, stateDir string) (string, error) {
	configured, present, err := loadBrowserBootstrap(stateDir)
	if err != nil {
		return "", err
	}
	if present {
		if len(configured) == 0 {
			return "", nil
		}
		if _, err := users.BootstrapUsers(ctx, configured, nil); err != nil {
			return "", fmt.Errorf("import browser accounts: %w", err)
		}
		return "", nil
	}
	// An explicit fail-closed start (the runtime harness's "no browser users"
	// fixture) must not create an owner account.
	if os.Getenv("RIGSCOPE_HUB_DISABLE_BOOTSTRAP") != "" {
		return "", nil
	}
	return auth.Bootstrap(ctx, users, auth.BootstrapOptions{})
}

// newHubRunID returns the per-process identifier shared by health, welcome and
// the live snapshots.
func newHubRunID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// hubDatabasePath resolves the telemetry database location. The environment
// override exists for the runtime test harness, which keeps every hub's state
// in a disposable directory.
func hubDatabasePath(stateDir string) string {
	if value := os.Getenv("RIGSCOPE_HUB_DATABASE_PATH"); value != "" {
		return value
	}
	return filepath.Join(stateDir, hubDatabaseFile)
}

// hubFlushInterval resolves the telemetry flush interval override.
func hubFlushInterval() time.Duration {
	value := os.Getenv("RIGSCOPE_HUB_FLUSH_MS")
	if value == "" {
		return 0
	}
	var parsed int64
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed <= 0 {
		return 0
	}
	if parsed > 5000 {
		parsed = 5000
	}
	return time.Duration(parsed) * time.Millisecond
}

// hubServices owns the hub's telemetry-backed repositories and API handlers and
// their life cycle. The composition root opens one database and passes the
// same handle to auth, admin, registry, telemetry, live and history.
type hubServices struct {
	db        *storage.Database
	telemetry *storage.Telemetry
	registry  *storage.Registry
	machines  *storage.Machines
	state     *live.State
	gateway   *ingest.Gateway
	feed      *live.Feed
	processes *live.ProcessInterests
	history   *history.Handler
	enroll    *ingest.EnrollmentHandler
	alerts    *alertRuntime

	stopOnce sync.Once
	mu       sync.Mutex
	closed   bool
}

// newHubServices restores live state and builds the API handlers over the
// composition root's database. The caller mounts them before Listen and closes
// them on shutdown.
func newHubServices(
	ctx context.Context,
	db *storage.Database,
	stateDir string,
	hubRunID string,
	logger *slog.Logger,
	authenticator *auth.LiveAuthenticator,
	hubConfig config.Hub,
) (*hubServices, error) {
	registry, err := openRegistryWithBootstrap(ctx, db, stateDir)
	if err != nil {
		return nil, fmt.Errorf("open collector registry: %w", err)
	}
	flush := hubFlushInterval()
	if hubConfig.TelemetryMode != "persistent" {
		// RAM telemetry has its own budget; the durable control database must
		// retain the same disk cap it has when telemetry shares that database.
		if _, err := db.SetPhysicalLimit(storage.DefaultMaxDatabaseBytes); err != nil {
			return nil, fmt.Errorf("configure control database limit: %w", err)
		}
	}
	telemetry, err := storage.NewTelemetry(ctx, db, storage.TelemetryOptions{
		FlushInterval: flush,
		Memory:        hubConfig.TelemetryMode != "persistent",
	})
	if err != nil {
		return nil, fmt.Errorf("open telemetry store: %w", err)
	}
	machines := storage.NewMachines(db)
	state := live.NewState(hubRunID, nil)
	services := &hubServices{
		db:        db,
		telemetry: telemetry,
		registry:  registry,
		machines:  machines,
		state:     state,
	}
	if err := ingest.Restore(ctx, registry, telemetry, machines, state); err != nil {
		_ = telemetry.Close(ctx)
		return nil, fmt.Errorf("restore live state: %w", err)
	}
	alertEngine, err := alerts.New(alerts.Options{Database: db, Logger: logger})
	if err != nil {
		_ = telemetry.Close(ctx)
		return nil, fmt.Errorf("open alert engine: %w", err)
	}
	alerting := newAlertRuntime(alertEngine, state, logger)
	services.alerts = alerting

	var gateway *ingest.Gateway
	processes := live.NewProcessInterests(live.ProcessOptions{
		Auth:  authenticator,
		State: state,
		Sender: live.DetailSenderFunc(func(machineID string, message protocol.HubMessage) bool {
			if gateway == nil {
				return false
			}
			return gateway.SendDetail(machineID, message)
		}),
		CheckMutation: authenticator.CheckMutation,
		CheckOrigin:   authenticator.CheckOrigin,
	})
	gateway, err = ingest.NewGateway(ingest.GatewayOptions{
		Registry:     registry,
		Telemetry:    telemetry,
		State:        state,
		Logger:       logger,
		OnDetail:     processes.Receive,
		OnDisconnect: processes.Disconnect,
		OnLiveSample: alerting.Observe,
	})
	if err != nil {
		alerting.Stop()
		processes.Close()
		_ = telemetry.Close(ctx)
		return nil, err
	}
	services.gateway = gateway
	services.processes = processes

	services.feed = live.NewFeed(live.FeedOptions{
		Auth:        authenticator,
		State:       state,
		CheckOrigin: authenticator.CheckOrigin,
	})
	services.history = history.New(history.Options{
		Auth:            authenticator,
		State:           state,
		Telemetry:       telemetry,
		CheckOrigin:     authenticator.CheckOrigin,
		FlushIntervalMS: flush.Milliseconds(),
	})
	importer := newPairingImporter(stateDir, registry)
	services.enroll = ingest.NewEnrollmentHandler(ingest.EnrollmentOptions{
		Registry:            registry,
		State:               state,
		Logger:              logger,
		ImportPendingClaims: importer.importPending,
	})
	return services, nil
}

// mount registers every telemetry-backed handler on the hub HTTP server.
func (s *hubServices) mount(runtime *server.Server) error {
	mounts := []struct {
		pattern string
		handler http.Handler
	}{
		{"/api/collector/enroll", s.enroll},
		{"/api/history", s.history},
		{"/api/live", s.feed},
		{"/api/processes", s.processes},
	}
	for _, entry := range mounts {
		if err := runtime.Mount(entry.pattern, entry.handler); err != nil {
			return err
		}
	}
	return runtime.MountUpgrade("/api/collector/connect", s.gateway)
}

// StopStreams ends alert evaluation, the live feed, process interests and
// collector sessions. The listener is already closed when this runs, so no new
// stream can start between stopping acceptance and ending the existing ones.
// Alerts stop first: the ticker is ended and the engine drained before SSE and
// collector sockets are torn down.
func (s *hubServices) StopStreams() {
	s.stopOnce.Do(func() {
		if s.alerts != nil {
			s.alerts.Stop()
		}
		if s.feed != nil {
			s.feed.Stop()
		}
		if s.processes != nil {
			s.processes.Close()
		}
		if s.gateway != nil {
			s.gateway.Close()
		}
	})
}

// Close stops long-lived handlers, flushes telemetry and closes the database.
// Each step is best-effort so one failure does not leak the others.
func (s *hubServices) Close(ctx context.Context) error {
	s.StopStreams()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	var closeErr error
	if s.telemetry != nil {
		if err := s.telemetry.Close(ctx); err != nil {
			closeErr = err
		}
	}
	if s.db != nil {
		if err := s.db.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}
