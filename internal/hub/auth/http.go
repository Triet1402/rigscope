package auth

import (
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/server"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

const (
	loginPath   = "/api/auth/login"
	sessionPath = "/api/auth/session"
	logoutPath  = "/api/auth/logout"
	// MaxLoginBodyBytes is the login/logout body limit from the contract.
	MaxLoginBodyBytes = 4096
	requestDeadline   = 5 * time.Second
)

// Handler serves /api/auth/*.
type Handler struct {
	manager *Manager
	slots   chan struct{}
}

// NewHandler returns the authentication route handler.
func NewHandler(manager *Manager) *Handler {
	return &Handler{manager: manager, slots: make(chan struct{}, maxConcurrentLogins)}
}

// Manager exposes the shared manager to sibling handlers.
func (h *Handler) Manager() *Manager { return h.manager }

// ServeHTTP dispatches one /api/auth/* request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case loginPath:
		h.login(w, r)
	case sessionPath:
		h.session(w, r)
	case logoutPath:
		h.logout(w, r)
	default:
		writeAPIError(w, http.StatusNotFound, "unknown auth endpoint", "")
	}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "POST required", http.MethodPost)
		return
	}
	configured, err := h.manager.Configured(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "browser access is not configured on this hub", "")
		return
	}
	if !configured {
		writeAPIError(w, http.StatusServiceUnavailable, "browser access is not configured on this hub", "")
		return
	}
	if !h.manager.origin.Trusted(r) {
		writeAPIError(w, http.StatusForbidden, "origin not allowed", "")
		return
	}
	if !jsonContentType(r) {
		writeAPIError(w, http.StatusUnsupportedMediaType, "JSON required", "")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		writeAPIError(w, http.StatusTooManyRequests, "too many login attempts; wait briefly and retry", "")
		return
	}
	clearDeadline := setReadDeadline(w, requestDeadline)
	defer clearDeadline()
	body, status, message := readBody(r, MaxLoginBodyBytes)
	if status != 0 {
		writeAPIError(w, status, message, "")
		return
	}
	request, err := api.DecodeLoginRequest(body)
	if err != nil {
		code := http.StatusBadRequest
		message := "invalid login request"
		var input *api.InputError
		if errors.As(err, &input) {
			code = input.Status
			if code == http.StatusRequestEntityTooLarge {
				message = "body too large"
			}
		}
		writeAPIError(w, code, message, "")
		return
	}
	session, token, ok, err := h.manager.Login(r.Context(), server.ClientIP(r), request.Username, request.Password)
	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			writeAPIError(w, http.StatusTooManyRequests, "too many login attempts; wait briefly and retry", "")
			return
		}
		writeAPIError(w, http.StatusServiceUnavailable, "authentication temporarily unavailable", "")
		return
	}
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "invalid username or password", "")
		return
	}
	secure := h.manager.Secure(r)
	setSessionCookie(w, token, int(h.manager.TTL()/time.Second), secure)
	writeSession(w, session, false)
}

func (h *Handler) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeAPIError(w, http.StatusMethodNotAllowed, "GET required", "GET, HEAD")
		return
	}
	configured, err := h.manager.Configured(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "browser access is not configured on this hub", "")
		return
	}
	if !configured {
		writeAPIError(w, http.StatusServiceUnavailable, "browser access is not configured on this hub", "")
		return
	}
	session, err := h.manager.Session(r)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "authentication temporarily unavailable", "")
		return
	}
	if session == nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required", "")
		return
	}
	writeSession(w, session, r.Method == http.MethodHead)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, "POST required", http.MethodPost)
		return
	}
	configured, err := h.manager.Configured(r.Context())
	if err != nil || !configured {
		writeAPIError(w, http.StatusServiceUnavailable, "browser access is not configured on this hub", "")
		return
	}
	if !h.manager.origin.Trusted(r) {
		writeAPIError(w, http.StatusForbidden, "origin not allowed", "")
		return
	}
	secure := h.manager.Secure(r)
	session, err := h.manager.Session(r)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "authentication temporarily unavailable", "")
		return
	}
	if session == nil {
		clearSessionCookie(w, secure)
		writeAPIError(w, http.StatusUnauthorized, "authentication required", "")
		return
	}
	if !csrfMatches(r, session.CSRFToken) {
		writeAPIError(w, http.StatusForbidden, "missing or invalid csrf token", "")
		return
	}
	if err := h.manager.Logout(r.Context(), session); err != nil && storage.CodeOf(err) != storage.CodeNotFound {
		writeAPIError(w, http.StatusServiceUnavailable, "authentication temporarily unavailable", "")
		return
	}
	clearSessionCookie(w, secure)
	writeOK(w)
}

func writeSession(w http.ResponseWriter, session *Session, head bool) {
	payload := api.SessionResponse{Username: session.Username, CSRFToken: session.CSRFToken}
	if session.Admin {
		admin := true
		payload.Admin = &admin
	}
	body, err := api.EncodeSessionResponse(payload)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "session unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, body, head)
}

// ---------------------------------------------------------------------------
// Shared HTTP helpers
// ---------------------------------------------------------------------------

func jsonContentType(r *http.Request) bool {
	return api.RequireJSONContentType(r.Header.Get("Content-Type")) == nil
}

func readBody(r *http.Request, maxBytes int) ([]byte, int, string) {
	if r.Body == nil {
		return nil, http.StatusBadRequest, "invalid JSON body"
	}
	limited := io.LimitReader(r.Body, int64(maxBytes)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, http.StatusRequestEntityTooLarge, "body too large"
		}
		return nil, http.StatusBadRequest, "request failed"
	}
	if len(body) > maxBytes {
		return nil, http.StatusRequestEntityTooLarge, "body too large"
	}
	return body, 0, ""
}

func csrfMatches(r *http.Request, expected string) bool {
	header := r.Header.Get("X-CSRF-Token")
	if header == "" || len(header) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header), []byte(expected)) == 1
}

func writeAPIError(w http.ResponseWriter, status int, message, allow string) {
	body, err := api.EncodeAPIError(api.APIError{Error: message})
	if err != nil {
		body = []byte(`{"error":"internal error"}`)
	}
	if allow != "" {
		w.Header().Set("Allow", allow)
	}
	writeJSON(w, status, body, false)
}

func writeOK(w http.ResponseWriter) {
	body, err := api.EncodeOKResponse(api.OKResponse{OK: true})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error", "")
		return
	}
	writeJSON(w, http.StatusOK, body, false)
}

func writeJSON(w http.ResponseWriter, status int, body []byte, head bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if !head {
		_, _ = w.Write(body)
	}
}

func setReadDeadline(w http.ResponseWriter, deadline time.Duration) func() {
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(deadline)); err != nil {
		return func() {}
	}
	// Clear the deadline so a later keep-alive request is not rejected.
	return func() { _ = controller.SetReadDeadline(time.Time{}) }
}

// DecodeJSON is a small helper for sibling packages that need the same bounded
// body read without re-importing net/http details.
func DecodeJSON(r *http.Request, maxBytes int) ([]byte, int, string) { return readBody(r, maxBytes) }

// ContentTypeJSON reports whether the request declares a JSON body.
func ContentTypeJSON(r *http.Request) bool { return jsonContentType(r) }

// WriteJSON writes one validated body with the contract headers.
func WriteJSON(w http.ResponseWriter, status int, body []byte) { writeJSON(w, status, body, false) }

// WriteError writes the shared JSON error envelope.
func WriteError(w http.ResponseWriter, status int, message, allow string) {
	writeAPIError(w, status, message, allow)
}

// WriteOK writes {"ok":true}.
func WriteOK(w http.ResponseWriter) { writeOK(w) }

// CSRFMatches compares the X-CSRF-Token header with the session value in
// constant time.
func CSRFMatches(r *http.Request, expected string) bool { return csrfMatches(r, expected) }
