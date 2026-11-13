package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/server"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

const (
	// EnrollmentBurst and enrollmentRefill bound the enrollment endpoint: a
	// burst of 30 requests and one token recovered every 200 ms.
	EnrollmentBurst  = 30.0
	enrollmentRefill = 200 * time.Millisecond
	// enrollmentConcurrency bounds simultaneous body reads.
	enrollmentConcurrency = 16
	// enrollmentReadTimeout bounds one enrollment body read.
	enrollmentReadTimeout = 5 * time.Second
)

// EnrollmentOptions configures POST /api/collector/enroll.
type EnrollmentOptions struct {
	Registry *storage.Registry
	State    *live.State
	Logger   *slog.Logger
	Now      func() time.Time
	// ImportPendingClaims runs before a grant is redeemed. The hub uses it to
	// import a pairing bootstrap file that appeared after startup, so adding a
	// collector to an already-running hub needs no restart.
	ImportPendingClaims func(context.Context) error

	// testBeforeInvalidate runs after the replacement transaction commits and
	// before the old credential is invalidated. Tests use it to recover the new
	// credential and connect first, proving the delayed notification does not
	// fence a legitimate successor.
	testBeforeInvalidate func(identity storage.Identity)
}

// EnrollmentHandler claims one-time grants and returns collector credentials.
type EnrollmentHandler struct {
	opts EnrollmentOptions
	log  *slog.Logger

	mu       sync.Mutex
	tokens   float64
	lastFill time.Time

	activeMu sync.Mutex
	active   int
}

// NewEnrollmentHandler creates the enrollment endpoint.
func NewEnrollmentHandler(opts EnrollmentOptions) *EnrollmentHandler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &EnrollmentHandler{
		opts:     opts,
		log:      logger,
		tokens:   EnrollmentBurst,
		lastFill: opts.Now(),
	}
}

// ServeHTTP implements POST /api/collector/enroll.
func (h *EnrollmentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.takeToken() {
		h.reply(w, http.StatusTooManyRequests, api.APIError{Error: "enrollment request budget exceeded"})
		return
	}
	if r.Method != http.MethodPost {
		h.reply(w, http.StatusMethodNotAllowed, api.APIError{Error: "POST required"})
		return
	}
	if h.opts.Registry == nil {
		h.reply(w, http.StatusNotFound, api.APIError{Error: "enrollment disabled"})
		return
	}
	if originPresent(r) {
		h.reply(w, http.StatusForbidden, api.APIError{Error: "origin not allowed"})
		return
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || len(token) > 256 {
		h.reply(w, http.StatusUnauthorized, api.APIError{Error: "invalid enrollment grant"})
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		h.reply(w, http.StatusUnsupportedMediaType, api.APIError{Error: "JSON required"})
		return
	}
	if !h.acquire() {
		h.reply(w, http.StatusTooManyRequests, api.APIError{Error: "enrollment request budget exceeded"})
		return
	}
	defer h.release()

	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(h.opts.Now().Add(enrollmentReadTimeout))
	body, err := io.ReadAll(io.LimitReader(r.Body, api.MaxEnrollmentBodyBytes+1))
	if err != nil {
		h.reply(w, http.StatusBadRequest, api.APIError{Error: "invalid enrollment request"})
		return
	}
	if len(body) > api.MaxEnrollmentBodyBytes {
		h.reply(w, http.StatusRequestEntityTooLarge, api.APIError{Error: "body too large"})
		return
	}
	request, err := api.DecodeEnrollmentRequest(body)
	if err != nil {
		var input *api.InputError
		if errors.As(err, &input) && input.Status == http.StatusRequestEntityTooLarge {
			h.reply(w, http.StatusRequestEntityTooLarge, api.APIError{Error: "body too large"})
			return
		}
		h.reply(w, http.StatusBadRequest, api.APIError{Error: "invalid enrollment request"})
		return
	}
	enrollRequest := storage.EnrollRequest{CollectorID: request.CollectorID, Token: token, LocalPeer: localPeer(r)}
	if request.RequestID != nil {
		enrollRequest.RequestID = *request.RequestID
	}
	if request.RecoverySecret != nil {
		enrollRequest.RecoverySecret = *request.RecoverySecret
	}
	if h.opts.ImportPendingClaims != nil {
		if err := h.opts.ImportPendingClaims(r.Context()); err != nil {
			h.log.Error("import pending collector pairing grant", "error", err)
			h.reply(w, http.StatusServiceUnavailable, api.APIError{Error: "enrollment storage unavailable"})
			return
		}
	}
	identity, err := h.opts.Registry.Enroll(r.Context(), enrollRequest)
	if err != nil {
		switch storage.CodeOf(err) {
		case storage.CodeInvalid:
			h.reply(w, http.StatusBadRequest, api.APIError{Error: "invalid enrollment request"})
		case storage.CodeUnavailable:
			h.reply(w, http.StatusServiceUnavailable, api.APIError{Error: "enrollment storage unavailable"})
		default:
			// Unknown, expired, reused and conflicting grants share one
			// response so the endpoint is not an oracle for a guessed token.
			h.reply(w, http.StatusForbidden, api.APIError{Error: "enrollment denied"})
		}
		return
	}
	if h.opts.testBeforeInvalidate != nil {
		h.opts.testBeforeInvalidate(identity)
	}
	if h.opts.State != nil {
		// Register the new credential's mapping before publishing the old
		// credential's invalidation. The notification names the exact old
		// credential, so it cannot remove a mapping a successor already owns.
		h.ensureMachine(identity)
		if identity.ReplacedCollectorID != "" {
			h.opts.State.InvalidateCredential(live.CredentialInvalidation{
				CollectorID: identity.ReplacedCollectorID,
				MachineID:   identity.MachineID,
				TokenHash:   identity.ReplacedTokenHash,
			})
		}
	}
	h.log.Info("collector enrolled", "collectorId", identity.CollectorID, "machineId", identity.MachineID)
	h.reply(w, http.StatusCreated, api.EnrollmentIdentity{
		CollectorID: identity.CollectorID,
		MachineID:   identity.MachineID,
		Token:       identity.Token,
	})
}

// localPeer reports whether the redemption request arrived on a socket the hub
// can attribute to its own machine. Two shapes count:
//
//   - a loopback peer, the ordinary local install; and
//   - a peer whose address equals the listening socket's own address. A
//     collector configured with a hub origin dials the origin name, which on the
//     hub's own machine resolves to one of the host's addresses; the resulting
//     connection is a self-connection, so its peer address is the local address.
//
// Everything comes from the socket. A configured trusted proxy is refused even
// when it connects from loopback, and a request that carried any forwarding
// header before the security layer stripped it is refused too: a same-host
// reverse proxy must not launder a remote caller into a local one. A TCP-level
// proxy (a stream proxy or `ssh -L`) is indistinguishable at this layer; that
// limit is documented in docs/security.md.
func localPeer(r *http.Request) bool {
	if server.TrustedProxyPeer(r) || server.ForwardedHeadersPresent(r) {
		return false
	}
	peer := addrIP(r.RemoteAddr)
	if peer == nil {
		return false
	}
	if peer.IsLoopback() {
		return true
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	localIP := addrIP(local.String())
	return localIP != nil && localIP.Equal(peer)
}

// addrIP extracts the IP from a host:port address, tolerating a bare address
// and an IPv6 zone.
func addrIP(address string) net.IP {
	host := address
	if parsed, _, err := net.SplitHostPort(address); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if before, _, found := strings.Cut(host, "%"); found {
		host = before
	}
	return net.ParseIP(host)
}

func (h *EnrollmentHandler) ensureMachine(identity storage.Identity) {
	if err := h.opts.State.RegisterMachineWithCredential(identity.CollectorID, identity.MachineID, identity.TokenHash); err != nil {
		// A replacement grant changes the hub-local machine id for a collector
		// that already had one.
		h.opts.State.ReplaceMachineWithCredential(identity.CollectorID, identity.MachineID, identity.TokenHash)
	}
}

func (h *EnrollmentHandler) reply(w http.ResponseWriter, status int, value any) {
	var body []byte
	var err error
	switch typed := value.(type) {
	case api.APIError:
		body, err = api.EncodeAPIError(typed)
	case api.EnrollmentIdentity:
		body, err = api.EncodeEnrollmentIdentity(typed)
	default:
		body = []byte(`{"error":"internal error"}`)
	}
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *EnrollmentHandler) takeToken() bool {
	now := h.opts.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	elapsed := now.Sub(h.lastFill)
	if elapsed > 0 {
		h.tokens += float64(elapsed) / float64(enrollmentRefill)
		if h.tokens > EnrollmentBurst {
			h.tokens = EnrollmentBurst
		}
		h.lastFill = now
	}
	if h.tokens < 1 {
		return false
	}
	h.tokens--
	return true
}

func (h *EnrollmentHandler) acquire() bool {
	h.activeMu.Lock()
	defer h.activeMu.Unlock()
	if h.active >= enrollmentConcurrency {
		return false
	}
	h.active++
	return true
}

func (h *EnrollmentHandler) release() {
	h.activeMu.Lock()
	h.active--
	h.activeMu.Unlock()
}
