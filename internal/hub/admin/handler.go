package admin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

const (
	usersPath          = "/api/admin/users"
	auditPath          = "/api/admin/audit"
	collectorsPath     = "/api/admin/collectors"
	collectorGrants    = "/api/admin/collector-grants"
	alertDiagnostics   = "/api/admin/diagnostics/alerts"
	settingsPath       = "/api/admin/settings"
	machineMetaPath    = "/api/machines/meta"
	maxAdminBodyBytes  = 8 * 1024
	maxCollectorBody   = 2048
	maxMachineMetaBody = 4096
	maxAuditLimit      = 1000
	defaultAuditLimit  = 100
	maxConcurrentHash  = 2
)

// Options configures the administrative router.
type Options struct {
	Users    *storage.Users
	Audit    *storage.Audit
	Machines *storage.Machines
	// Registry is the collector registry. Nil answers collector routes with
	// 503 and leaves machine existence to MachineExists.
	Registry *storage.Registry
	// Auth supplies the session lookup and revocation broadcaster.
	Auth *auth.Manager
	// MachineExists reports whether a machine is known to the hub. Nil falls
	// back to the collector registry.
	MachineExists func(ctx context.Context, machineID string) (bool, error)
	// OnCollectorRevoked closes the collector's live session and removes it
	// from live state. Optional.
	OnCollectorRevoked func(collectorID string)
	// OnMachineRemoved deletes the machine's stored history (and any other
	// runtime state) before its registry row is removed. Optional.
	OnMachineRemoved func(ctx context.Context, machineID string) error
	// OnMachineMetaChanged publishes a live-state change (SSE update).
	// Optional.
	OnMachineMetaChanged func(machineID string)
	// AlertDiagnostics returns the alert runtime's loss counters. Nil answers
	// the diagnostics route with 503.
	AlertDiagnostics func() api.AlertDiagnostics
	// Settings returns the running hub's settings summary. Nil answers the
	// settings route with 503.
	Settings func(ctx context.Context) (api.HubSettings, error)
	// HashPassword is injected by tests. Production uses auth.HashPassword.
	HashPassword func(password string) (string, error)
	Logger       *slog.Logger
}

// Handler serves the administrative and machine-metadata routes.
type Handler struct {
	users            *storage.Users
	audit            *storage.Audit
	machines         *storage.Machines
	registry         *storage.Registry
	auth             *auth.Manager
	machineExists    func(context.Context, string) (bool, error)
	onCollector      func(string)
	onMachineRemoved func(context.Context, string) error
	onMachineMeta    func(string)
	alertDiagnostics func() api.AlertDiagnostics
	settings         func(context.Context) (api.HubSettings, error)
	hashPassword     func(string) (string, error)
	logger           *slog.Logger
	hashSlots        chan struct{}
}

// New validates the dependencies and returns the router.
func New(opts Options) (*Handler, error) {
	if opts.Users == nil {
		return nil, errors.New("admin: user directory is required")
	}
	if opts.Audit == nil {
		return nil, errors.New("admin: audit log is required")
	}
	if opts.Auth == nil {
		return nil, errors.New("admin: authentication manager is required")
	}
	hash := opts.HashPassword
	if hash == nil {
		hash = auth.HashPassword
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	exists := opts.MachineExists
	if exists == nil {
		exists = registryMachineExists(opts.Registry)
	}
	return &Handler{
		users:            opts.Users,
		audit:            opts.Audit,
		machines:         opts.Machines,
		registry:         opts.Registry,
		auth:             opts.Auth,
		machineExists:    exists,
		onCollector:      opts.OnCollectorRevoked,
		onMachineRemoved: opts.OnMachineRemoved,
		onMachineMeta:    opts.OnMachineMetaChanged,
		alertDiagnostics: opts.AlertDiagnostics,
		settings:         opts.Settings,
		hashPassword:     hash,
		logger:           logger,
		hashSlots:        make(chan struct{}, maxConcurrentHash),
	}, nil
}

// ServeHTTP dispatches one administrative request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case usersPath:
		h.usersRoute(w, r)
	case auditPath:
		h.auditRoute(w, r)
	case collectorsPath, collectorGrants:
		h.collectorsRoute(w, r)
	case alertDiagnostics:
		h.alertDiagnosticsRoute(w, r)
	case settingsPath:
		h.settingsRoute(w, r)
	case machineMetaPath:
		h.machineMetaRoute(w, r)
	default:
		auth.WriteError(w, http.StatusNotFound, "not found", "")
	}
}

// requireAdmin checks configuration, session and the durable admin flag.
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) (*auth.Session, bool) {
	configured, err := h.auth.Configured(r.Context())
	if err != nil || !configured {
		auth.WriteError(w, http.StatusServiceUnavailable, "browser access is not configured on this hub", "")
		return nil, false
	}
	session, err := h.auth.Session(r)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "administration temporarily unavailable", "")
		return nil, false
	}
	if session == nil {
		auth.WriteError(w, http.StatusUnauthorized, "authentication required", "")
		return nil, false
	}
	admin, err := h.auth.IsAdmin(r.Context(), session.Username)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "administration temporarily unavailable", "")
		return nil, false
	}
	if !admin {
		auth.WriteError(w, http.StatusForbidden, "administrative access required", "")
		return nil, false
	}
	return session, true
}

// mutationProof enforces the exact Origin plus a constant-time CSRF token.
// The message differs per route, matching the reference hub.
func (h *Handler) mutationProof(w http.ResponseWriter, r *http.Request, session *auth.Session, message string) bool {
	if !h.auth.Origin().Trusted(r) || !auth.CSRFMatches(r, session.CSRFToken) {
		auth.WriteError(w, http.StatusForbidden, message, "")
		return false
	}
	return true
}

func (h *Handler) readJSONBody(w http.ResponseWriter, r *http.Request, maxBytes int) ([]byte, bool) {
	if !auth.ContentTypeJSON(r) {
		auth.WriteError(w, http.StatusUnsupportedMediaType, "JSON required", "")
		return nil, false
	}
	body, status, message := auth.DecodeJSON(r, maxBytes)
	if status == http.StatusRequestEntityTooLarge {
		message = "request body too large"
	}
	if status == http.StatusBadRequest {
		message = "invalid JSON body"
	}
	if status != 0 {
		auth.WriteError(w, status, message, "")
		return nil, false
	}
	return body, true
}

func (h *Handler) acquireHashSlot() bool {
	select {
	case h.hashSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (h *Handler) releaseHashSlot() { <-h.hashSlots }

func registryMachineExists(registry *storage.Registry) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, machineID string) (bool, error) {
		if registry == nil {
			return false, nil
		}
		collectors, err := registry.List(ctx)
		if err != nil {
			return false, err
		}
		for _, collector := range collectors {
			if collector.MachineID == machineID {
				return true, nil
			}
		}
		return false, nil
	}
}

// scopeFromStorage maps a durable scope to the wire form.
func scopeFromStorage(scope storage.Scope) api.MachineScope {
	if scope.All {
		return api.MachineScope{All: true}
	}
	return api.MachineScope{IDs: append([]string(nil), scope.MachineIDs...)}
}

// scopeToStorage maps a validated wire scope to the durable form.
func scopeToStorage(scope api.MachineScope) storage.Scope {
	if scope.All {
		return storage.AllMachines()
	}
	return storage.MachineScope(scope.IDs...)
}

func includesMachine(session *auth.Session, machineID string) bool {
	return session.Scope.Contains(machineID)
}

func isoTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}
