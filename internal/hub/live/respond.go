package live

import (
	"net/http"

	"github.com/Triet1402/rigscope/internal/api"
)

const jsonContentType = "application/json; charset=utf-8"

// writeAPIBytes writes an already encoded contract response.
func writeAPIBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError writes the shared JSON error body.
func writeError(w http.ResponseWriter, status int, message string) {
	body, err := api.EncodeAPIError(api.APIError{Error: message})
	if err != nil {
		body = []byte(`{"error":"internal error"}`)
	}
	writeAPIBytes(w, status, body)
}

// writeErrorCode writes an error body carrying a programmatic code.
func writeErrorCode(w http.ResponseWriter, status int, message, code string) {
	body, err := api.EncodeAPIError(api.APIError{Error: message, Code: &code})
	if err != nil {
		body = []byte(`{"error":"internal error"}`)
	}
	writeAPIBytes(w, status, body)
}

// writeMethodNotAllowed writes the JSON method error used by monitoring routes.
func writeMethodNotAllowed(w http.ResponseWriter, message string) {
	writeError(w, http.StatusMethodNotAllowed, message)
}

// authenticate resolves the browser session and maps auth errors to the
// documented statuses. It writes the response and returns ok=false on failure.
func authenticate(w http.ResponseWriter, auth Authenticator, r *http.Request) (Session, bool) {
	if auth == nil {
		writeError(w, http.StatusServiceUnavailable, "browser access is not configured")
		return Session{}, false
	}
	session, err := auth.Authenticate(r)
	if err != nil {
		switch err {
		case ErrUnauthenticated:
			writeError(w, http.StatusUnauthorized, "authentication required")
		case ErrBrowserDisabled:
			writeError(w, http.StatusServiceUnavailable, "browser access is not configured")
		case ErrUnavailable:
			writeError(w, http.StatusServiceUnavailable, "authentication temporarily unavailable")
		default:
			writeError(w, http.StatusUnauthorized, "authentication required")
		}
		return Session{}, false
	}
	return session, true
}

// writeValidationError encodes a contract value or falls back to a 500.
func writeValidationError(w http.ResponseWriter, err error) {
	_ = err
	writeError(w, http.StatusInternalServerError, "response unavailable")
}

// writeBody writes raw JSON bytes with the JSON content type.
func writeBody(w http.ResponseWriter, status int, body []byte) {
	writeAPIBytes(w, status, body)
}
