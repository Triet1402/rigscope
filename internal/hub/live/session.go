package live

import (
	"errors"
	"net/http"

	"github.com/Triet1402/rigscope/internal/api"
)

// Session is one authenticated browser session as the live APIs need it.
// User identity is carried for auditing and admin checks; authority for the
// machine data is the scope only. Revoked, when non-nil, is closed when the
// session ends (logout, expiry or user revocation), which ends every SSE
// stream and process-detail interest owned by the session.
type Session struct {
	ID        string
	Username  string
	Admin     bool
	Scope     api.MachineScope
	CSRFToken string
	Revoked   <-chan struct{}
}

// Authenticator resolves a browser request to its session. The auth package
// implements this at composition time; tests use a fake.
type Authenticator interface {
	Authenticate(r *http.Request) (Session, error)
}

// Auth errors with the status the browser contract maps them to.
var (
	// ErrUnauthenticated means the request carries no valid session cookie.
	ErrUnauthenticated = errors.New("authentication required")
	// ErrBrowserDisabled means browser access is not configured on this hub.
	ErrBrowserDisabled = errors.New("browser access is not configured")
	// ErrUnavailable means the session store could not be read; the browser
	// contract answers 503 instead of pretending the cookie is invalid.
	ErrUnavailable = errors.New("authentication temporarily unavailable")
	// ErrMutationRejected means a mutating request failed the Origin or CSRF
	// proof. Handlers map it to 403.
	ErrMutationRejected = errors.New("missing or invalid csrf token")
)

// CheckMutation enforces Origin and CSRF proof for a mutating browser request.
// It returns a non-nil error when the request must be rejected with 403. It is
// optional here because authentication lives in another package; the auth
// package supplies it at composition time.
type CheckMutation func(r *http.Request, session Session) error
