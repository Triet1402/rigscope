package api

// AuthRequirement is the access check an endpoint performs before handling a
// request. It is informational metadata for the generated client constant;
// enforcement lives in the Go hub.
type AuthRequirement string

// Access requirements used by the endpoint table.
const (
	// AuthNone means the endpoint is reachable without a browser session.
	AuthNone AuthRequirement = "none"
	// AuthSession means a valid browser session cookie is required.
	AuthSession AuthRequirement = "session"
	// AuthAdmin means a browser session plus an active administrator is required.
	AuthAdmin AuthRequirement = "admin"
	// AuthGrant means a collector enrollment grant in an Authorization header.
	AuthGrant AuthRequirement = "grant"
	// AuthCollector means collector bearer credentials on a WebSocket upgrade.
	AuthCollector AuthRequirement = "collector"
)

// Endpoint is one entry of the browser-facing API table. Query parameters and
// JSON bodies are described in docs/rewrite/api-v1.md; this table exists so
// generated TypeScript can list the routes the hub serves.
type Endpoint struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Auth   AuthRequirement `json:"auth"`
}

// endpoints is every HTTP and SSE route that is part of the 1.0 contract, in a
// stable order. Recording routes are intentionally absent: recordings are
// deferred and their endpoints are removed in 1.0.
var endpoints = []Endpoint{
	{Method: "GET", Path: "/api/health", Auth: AuthNone},
	{Method: "HEAD", Path: "/api/health", Auth: AuthNone},
	{Method: "GET", Path: "/api/live", Auth: AuthSession},

	{Method: "POST", Path: "/api/auth/login", Auth: AuthNone},
	{Method: "GET", Path: "/api/auth/session", Auth: AuthNone},
	{Method: "HEAD", Path: "/api/auth/session", Auth: AuthNone},
	// Logout is session-authenticated: a missing session is 401, but the hub
	// still clears the cookie on that failure. An Origin/CSRF rejection is 403
	// and leaves the cookie untouched.
	{Method: "POST", Path: "/api/auth/logout", Auth: AuthSession},

	{Method: "POST", Path: "/api/collector/enroll", Auth: AuthGrant},
	{Method: "GET", Path: "/api/collector/connect", Auth: AuthCollector},

	{Method: "GET", Path: "/api/admin/users", Auth: AuthAdmin},
	{Method: "POST", Path: "/api/admin/users", Auth: AuthAdmin},
	{Method: "DELETE", Path: "/api/admin/users", Auth: AuthAdmin},
	{Method: "GET", Path: "/api/admin/audit", Auth: AuthAdmin},
	{Method: "GET", Path: "/api/admin/collectors", Auth: AuthAdmin},
	{Method: "POST", Path: "/api/admin/collectors", Auth: AuthAdmin},
	{Method: "POST", Path: "/api/admin/collector-grants", Auth: AuthAdmin},
	{Method: "GET", Path: "/api/admin/diagnostics/alerts", Auth: AuthAdmin},
	{Method: "GET", Path: "/api/admin/settings", Auth: AuthAdmin},

	{Method: "GET", Path: "/api/alerts", Auth: AuthSession},
	{Method: "POST", Path: "/api/alerts/rules", Auth: AuthAdmin},
	{Method: "DELETE", Path: "/api/alerts/rules", Auth: AuthAdmin},
	{Method: "POST", Path: "/api/alerts/webhook", Auth: AuthAdmin},

	{Method: "GET", Path: "/api/history/status", Auth: AuthSession},
	{Method: "GET", Path: "/api/history/metrics", Auth: AuthSession},
	{Method: "GET", Path: "/api/history", Auth: AuthSession},

	{Method: "POST", Path: "/api/processes", Auth: AuthSession},
	{Method: "GET", Path: "/api/processes", Auth: AuthSession},
	{Method: "DELETE", Path: "/api/processes", Auth: AuthSession},

	{Method: "POST", Path: "/api/machines/meta", Auth: AuthAdmin},
}

// Endpoints returns a defensive copy of the 1.0 endpoint table.
func Endpoints() []Endpoint {
	out := make([]Endpoint, len(endpoints))
	copy(out, endpoints)
	return out
}
