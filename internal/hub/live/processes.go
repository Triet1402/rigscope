package live

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	// DefaultViewerLease is how long a browser viewer keeps its interest
	// without renewal.
	DefaultViewerLease = 15 * time.Second
	// DefaultCollectorLease is the detail.subscribe lease for one machine.
	DefaultCollectorLease = 20 * time.Second
	// DefaultDetailInterval is the collector's sampling interval.
	DefaultDetailInterval = time.Second
	// DefaultRenewEvery is how often an active interest renews its collector
	// lease. It must be shorter than the collector lease.
	DefaultRenewEvery = 5 * time.Second
	// DefaultProcessSweep is the expiry/renewal sweep cadence.
	DefaultProcessSweep = time.Second
	// MaxViewersPerSession and MaxProcessViewers bound ephemeral detail work.
	MaxViewersPerSession = 8
	MaxProcessViewers    = 256
)

// DetailSender forwards one hub message to the collector serving a machine.
// The ingest gateway implements it.
type DetailSender interface {
	SendDetail(machineID string, message protocol.HubMessage) bool
}

// DetailSenderFunc adapts a function to DetailSender. It lets the composition
// root break the construction cycle between the gateway and the interests.
type DetailSenderFunc func(machineID string, message protocol.HubMessage) bool

// SendDetail calls the underlying function.
func (f DetailSenderFunc) SendDetail(machineID string, message protocol.HubMessage) bool {
	return f(machineID, message)
}

// ProcessOptions configures the /api/processes handler.
type ProcessOptions struct {
	Auth   Authenticator
	State  *State
	Sender DetailSender

	ViewerLease    time.Duration
	CollectorLease time.Duration
	Interval       time.Duration
	RenewEvery     time.Duration
	SweepEvery     time.Duration
	MaxPerSession  int
	MaxViewers     int

	// CheckMutation enforces Origin and CSRF for POST and DELETE. Optional:
	// the auth package supplies it at composition time.
	CheckMutation CheckMutation
	// CheckOrigin, when non-nil, rejects an unexpected Origin before auth.
	CheckOrigin func(*http.Request) bool
	// Now is the clock used for leases. Tests override it.
	Now func() time.Time
}

// Acquisition rejections. Both are decided under the interest mutex so a
// logout cannot race a viewer into existence.
var (
	errViewerBudget   = errors.New("process viewer budget exhausted")
	errSessionRevoked = errors.New("session revoked")
)

type viewerInterest struct {
	sessionID string
	machineID string
	expiresAt time.Time
}

type machineInterest struct {
	subscriptionID string
	lastRenewed    time.Time
	result         *protocol.DetailResult
	// ended is a detail.ended reason, or "disconnected".
	ended string
}

// ProcessInterests owns ephemeral process-detail viewer leases. Equivalent
// viewers on one machine share exactly one collector subscription.
type ProcessInterests struct {
	opts ProcessOptions

	mu       sync.Mutex
	viewers  map[string]*viewerInterest // sessionID + "\x00" + viewerId
	machines map[string]*machineInterest
	watched  map[string]struct{} // sessions whose Revoked channel is watched

	stop    chan struct{}
	stopped sync.Once
}

// NewProcessInterests starts the sweep loop.
func NewProcessInterests(opts ProcessOptions) *ProcessInterests {
	if opts.ViewerLease <= 0 {
		opts.ViewerLease = DefaultViewerLease
	}
	if opts.CollectorLease <= 0 {
		opts.CollectorLease = DefaultCollectorLease
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultDetailInterval
	}
	if opts.RenewEvery <= 0 {
		opts.RenewEvery = DefaultRenewEvery
	}
	if opts.SweepEvery <= 0 {
		opts.SweepEvery = DefaultProcessSweep
	}
	if opts.MaxPerSession <= 0 {
		opts.MaxPerSession = MaxViewersPerSession
	}
	if opts.MaxViewers <= 0 {
		opts.MaxViewers = MaxProcessViewers
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	interests := &ProcessInterests{
		opts:     opts,
		viewers:  make(map[string]*viewerInterest),
		machines: make(map[string]*machineInterest),
		watched:  make(map[string]struct{}),
		stop:     make(chan struct{}),
	}
	go interests.sweepLoop()
	return interests
}

// Close stops the sweep loop and cancels every collector lease.
func (p *ProcessInterests) Close() {
	p.stopped.Do(func() {
		close(p.stop)
		p.mu.Lock()
		defer p.mu.Unlock()
		for machineID, machine := range p.machines {
			if p.opts.Sender != nil {
				p.opts.Sender.SendDetail(machineID, protocol.DetailCancel{SubscriptionID: machine.subscriptionID})
			}
		}
		p.viewers = make(map[string]*viewerInterest)
		p.machines = make(map[string]*machineInterest)
	})
}

// watchRevocation ends a session's interests as soon as its revocation
// channel closes. One watcher per session is enough because every viewer is
// released by EndSession.
func (p *ProcessInterests) watchRevocation(session Session) {
	if session.ID == "" || session.Revoked == nil {
		return
	}
	p.mu.Lock()
	if _, ok := p.watched[session.ID]; ok {
		p.mu.Unlock()
		return
	}
	p.watched[session.ID] = struct{}{}
	p.mu.Unlock()
	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.watched, session.ID)
			p.mu.Unlock()
		}()
		select {
		case <-session.Revoked:
			p.EndSession(session.ID)
		case <-p.stop:
		}
	}()
}

// EndSession releases every viewer owned by a browser session.
func (p *ProcessInterests) EndSession(sessionID string) {
	if sessionID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, interest := range p.viewers {
		if interest.sessionID == sessionID {
			delete(p.viewers, key)
			p.releaseUnusedLocked(interest.machineID)
		}
	}
}

// Disconnect marks a machine's detail state disconnected and drops its result.
func (p *ProcessInterests) Disconnect(machineID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	machine, ok := p.machines[machineID]
	if !ok {
		return
	}
	machine.result = nil
	machine.ended = "disconnected"
	machine.lastRenewed = time.Time{}
}

// Receive applies a detail.result or detail.ended forwarded by the gateway.
// Results for an unknown or cancelled subscription are ignored.
func (p *ProcessInterests) Receive(machineID string, message protocol.CollectorMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	machine, ok := p.machines[machineID]
	if !ok {
		return
	}
	switch typed := message.(type) {
	case protocol.DetailResult:
		if typed.SubscriptionID != machine.subscriptionID {
			return
		}
		result := typed
		machine.result = &result
		machine.ended = ""
	case protocol.DetailEnded:
		if typed.SubscriptionID != machine.subscriptionID {
			return
		}
		machine.result = nil
		machine.ended = string(typed.Reason)
	}
}

// ViewerCount reports the number of live viewer interests.
func (p *ProcessInterests) ViewerCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.viewers)
}

// ServeHTTP implements GET/POST/DELETE /api/processes.
func (p *ProcessInterests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.opts.CheckOrigin != nil && r.Header.Get("Origin") != "" && !p.opts.CheckOrigin(r) {
		writeError(w, http.StatusForbidden, "origin not allowed")
		return
	}
	session, ok := authenticate(w, p.opts.Auth, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete:
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeMethodNotAllowed(w, "method not allowed")
		return
	}
	if r.Method != http.MethodGet && p.opts.CheckMutation != nil {
		if err := p.opts.CheckMutation(r, session); err != nil {
			writeError(w, http.StatusForbidden, "missing or invalid csrf token")
			return
		}
	}
	query, err := api.ParseProcessViewerQuery(queryValues(r.URL))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !session.Scope.Includes(query.MachineID) {
		writeError(w, http.StatusForbidden, "machine access denied")
		return
	}
	machine, exists := p.opts.State.Machine(query.MachineID)
	if !exists {
		writeError(w, http.StatusNotFound, "machine not found")
		return
	}
	switch r.Method {
	case http.MethodDelete:
		p.mu.Lock()
		key := viewerKey(session.ID, query.ViewerID)
		interest, had := p.viewers[key]
		delete(p.viewers, key)
		if had {
			p.releaseUnusedLocked(interest.machineID)
		}
		p.mu.Unlock()
		body, encodeErr := api.EncodeOKResponse(api.OKResponse{OK: true})
		if encodeErr != nil {
			writeValidationError(w, encodeErr)
			return
		}
		writeBody(w, http.StatusOK, body)
	case http.MethodPost:
		if !machine.Connected {
			writeError(w, http.StatusConflict, "collector not connected for Processes")
			return
		}
		if machine.Inventory != nil {
			capability := machine.Inventory.Capabilities.Processes
			if capability.Status != protocol.CapabilitySupported {
				reason := "processes " + string(capability.Status)
				if capability.Reason != nil && *capability.Reason != "" {
					reason = *capability.Reason
				}
				writeError(w, http.StatusForbidden, reason)
				return
			}
		}
		p.watchRevocation(session)
		terms, renewErr := p.renew(session, query.MachineID, query.ViewerID)
		if renewErr != nil {
			if errors.Is(renewErr, errSessionRevoked) {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			writeErrorCode(w, http.StatusTooManyRequests,
				"Process viewer budget exhausted. Close another Processes view and retry.",
				api.ErrorCodeProcessViewerBudget)
			return
		}
		body, encodeErr := api.EncodeProcessAcquireResponse(terms)
		if encodeErr != nil {
			writeValidationError(w, encodeErr)
			return
		}
		writeBody(w, http.StatusOK, body)
	default:
		state, result, liveInterest := p.read(session.ID, query.MachineID, query.ViewerID)
		if !liveInterest {
			writeError(w, http.StatusGone, "process interest expired; renew to continue")
			return
		}
		response := api.ProcessReadResponse{State: state, Result: result}
		body, encodeErr := api.EncodeProcessReadResponse(response)
		if encodeErr != nil {
			writeValidationError(w, encodeErr)
			return
		}
		writeBody(w, http.StatusOK, body)
	}
}

// renew acquires or renews one viewer lease and the shared collector lease.
// The revoked-session and budget checks run under the interest mutex, the same
// mutex EndSession takes, so a viewer can never be inserted for a session whose
// logout already completed.
func (p *ProcessInterests) renew(session Session, machineID, viewerID string) (api.ProcessAcquireResponse, error) {
	sessionID := session.ID
	p.mu.Lock()
	defer p.mu.Unlock()
	if sessionRevoked(session) {
		return api.ProcessAcquireResponse{}, errSessionRevoked
	}
	key := viewerKey(sessionID, viewerID)
	if _, exists := p.viewers[key]; !exists {
		if len(p.viewers) >= p.opts.MaxViewers {
			return api.ProcessAcquireResponse{}, errViewerBudget
		}
		count := 0
		for _, interest := range p.viewers {
			if interest.sessionID == sessionID {
				count++
			}
		}
		if count >= p.opts.MaxPerSession {
			return api.ProcessAcquireResponse{}, errViewerBudget
		}
	}
	if previous, exists := p.viewers[key]; exists && previous.machineID != machineID {
		delete(p.viewers, key)
		p.releaseUnusedLocked(previous.machineID)
	}
	p.viewers[key] = &viewerInterest{
		sessionID: sessionID,
		machineID: machineID,
		expiresAt: p.opts.Now().Add(p.opts.ViewerLease),
	}
	if _, exists := p.machines[machineID]; !exists {
		p.machines[machineID] = &machineInterest{subscriptionID: newUUID()}
	}
	p.renewCollectorLocked(machineID)
	return api.ProcessAcquireResponse{
		LeaseMs:    p.opts.ViewerLease.Milliseconds(),
		IntervalMs: p.opts.Interval.Milliseconds(),
	}, nil
}

// sessionRevoked reports whether the browser session has already ended. It is
// non-blocking and safe with a nil channel.
func sessionRevoked(session Session) bool {
	if session.Revoked == nil {
		return false
	}
	select {
	case <-session.Revoked:
		return true
	default:
		return false
	}
}

// read resolves a viewer's current state. The second result is false when the
// viewer has no live interest (expired or never acquired).
func (p *ProcessInterests) read(sessionID, machineID, viewerID string) (api.ProcessState, *protocol.DetailResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	interest, ok := p.viewers[viewerKey(sessionID, viewerID)]
	if !ok || interest.machineID != machineID {
		return "", nil, false
	}
	machine, ok := p.machines[machineID]
	if !ok {
		return "", nil, false
	}
	state := api.ProcessState(machine.ended)
	if state == "" {
		if machine.result != nil {
			state = api.ProcessStateReady
		} else {
			state = api.ProcessStateLoading
		}
	}
	if machine.result == nil {
		return state, nil, true
	}
	copied := *machine.result
	return state, &copied, true
}

func (p *ProcessInterests) sweepLoop() {
	ticker := time.NewTicker(p.opts.SweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.mu.Lock()
			p.sweepLocked()
			p.mu.Unlock()
		}
	}
}

func (p *ProcessInterests) sweepLocked() {
	now := p.opts.Now()
	for key, interest := range p.viewers {
		if !interest.expiresAt.After(now) {
			delete(p.viewers, key)
			p.releaseUnusedLocked(interest.machineID)
		}
	}
	for machineID := range p.machines {
		p.renewCollectorLocked(machineID)
	}
}

// releaseUnusedLocked cancels a machine's collector lease when its last viewer
// leaves. Process snapshots have no persistence and no idle cache.
func (p *ProcessInterests) releaseUnusedLocked(machineID string) {
	for _, interest := range p.viewers {
		if interest.machineID == machineID {
			return
		}
	}
	machine, ok := p.machines[machineID]
	if !ok {
		return
	}
	if p.opts.Sender != nil {
		p.opts.Sender.SendDetail(machineID, protocol.DetailCancel{SubscriptionID: machine.subscriptionID})
	}
	delete(p.machines, machineID)
}

// renewCollectorLocked re-sends detail.subscribe at most every RenewEvery.
func (p *ProcessInterests) renewCollectorLocked(machineID string) {
	machine, ok := p.machines[machineID]
	if !ok {
		return
	}
	now := p.opts.Now()
	if !machine.lastRenewed.IsZero() && now.Sub(machine.lastRenewed) < p.opts.RenewEvery {
		return
	}
	if p.opts.Sender == nil {
		machine.ended = "disconnected"
		return
	}
	sent := p.opts.Sender.SendDetail(machineID, protocol.DetailSubscribe{
		SubscriptionID: machine.subscriptionID,
		Kind:           protocol.DetailKindProcesses,
		IntervalMS:     p.opts.Interval.Milliseconds(),
		LeaseMS:        p.opts.CollectorLease.Milliseconds(),
	})
	if !sent {
		machine.ended = "disconnected"
		return
	}
	machine.lastRenewed = now
	if machine.ended == "disconnected" {
		machine.ended = ""
	}
}

func viewerKey(sessionID, viewerID string) string { return sessionID + "\x00" + viewerID }

func queryValues(u *url.URL) url.Values {
	return u.Query()
}

func newUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("sub-%d", time.Now().UnixNano())
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
