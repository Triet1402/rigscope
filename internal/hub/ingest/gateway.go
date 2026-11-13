package ingest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
	"github.com/coder/websocket"
)

// WebSocket close codes used by the collector gateway. The 4000-4999 range is
// application-private and mirrors docs/rewrite/protocol-v1.md.
const (
	CloseShutdown         websocket.StatusCode = 1001
	CloseMessageTooLarge  websocket.StatusCode = 1009
	ClosePolicy           websocket.StatusCode = 1008
	CloseHelloTimeout     websocket.StatusCode = 4001
	CloseIdentityMismatch websocket.StatusCode = 4002
	CloseInvalidMessage   websocket.StatusCode = 4003
	CloseSessionConflict  websocket.StatusCode = 4005
	CloseProtocolStep     websocket.StatusCode = 4006
	CloseRateLimit        websocket.StatusCode = 4007
)

const (
	// DefaultHelloTimeout bounds the wait for the first hello.
	DefaultHelloTimeout = 5 * time.Second
	// DefaultPingInterval and DefaultIdleTimeout bound collector liveness.
	DefaultPingInterval = 15 * time.Second
	DefaultIdleTimeout  = 45 * time.Second
	// DefaultMaxConnections bounds concurrent collector sessions.
	DefaultMaxConnections = 100
	// DefaultMessagesPerSecond and DefaultMessageBurst bound the message rate.
	DefaultMessagesPerSecond = 20.0
	DefaultMessageBurst      = 30.0
	// defaultSendQueueDepth bounds outbound hub messages per connection.
	defaultSendQueueDepth = 64
	// defaultWriteTimeout bounds one outbound frame.
	defaultWriteTimeout = 10 * time.Second
	// retryAfterOverloadMS is the immediate retry answer when the in-flight
	// batch window or storage is saturated.
	retryAfterOverloadMS = 1000
	// inventoryTimeout bounds the inventory persist before live publication.
	inventoryTimeout = 5 * time.Second
)

// GatewayOptions configures the collector WebSocket gateway.
type GatewayOptions struct {
	Registry  *storage.Registry
	Telemetry *storage.Telemetry
	State     *live.State
	Logger    *slog.Logger

	MaxConnections    int
	HelloTimeout      time.Duration
	PingInterval      time.Duration
	IdleTimeout       time.Duration
	MessagesPerSecond float64
	MessageBurst      float64
	SendQueueDepth    int
	// Now is the clock used to timestamp an ingested batch. One receipt time is
	// captured per batch and shared by live eligibility and persistence.
	// Defaults to time.Now.
	Now func() time.Time

	// OnDetail receives detail.result and detail.ended messages from the
	// collector; the machine id is the hub-local machine of that session.
	OnDetail func(machineID string, message protocol.CollectorMessage)
	// OnLiveSample receives each LIVE metrics record after it has been applied
	// to live state. It runs on the collector's read goroutine, so it must not
	// block: the alert composition root enqueues the observation instead of
	// evaluating synchronously. Replay and rejected records never reach it.
	OnLiveSample func(machineID string, record protocol.MetricsRecord, receivedAt time.Time)
	// OnDisconnect is called after an active session ends.
	OnDisconnect func(machineID string)
	// Revocations, when non-nil, delivers collector ids whose credential was
	// revoked or replaced; their live socket is closed immediately.
	Revocations <-chan string

	// testAfterReserve runs after a connection reserved its slot and before the
	// WebSocket upgrade. Tests pause here to revoke a credential mid-upgrade.
	// It is unexported so only in-package tests can install it.
	testAfterReserve func()
	// testBeforeActivate runs immediately before the hello state transition.
	// Tests use it to interleave concurrent hellos deterministically.
	testBeforeActivate func()
	// testAfterInventoryPersist runs after the inventory commit and before the
	// live state application. Tests use it to replace a session mid-write.
	testAfterInventoryPersist func()
	// testAfterInventoryApply receives the outcome of the session-aware live
	// inventory application. Tests assert that a replaced session was refused.
	testAfterInventoryApply func(applied bool)
	// testBeforeCommit runs at the start of the asynchronous batch commit,
	// after live publication and before the credential is re-checked. Tests
	// pause here to replace the credential mid-commit.
	testBeforeCommit func()
	// testFencedCommit runs when the commit path refuses a batch because the
	// session's credential is no longer current.
	testFencedCommit func()
}

// Gateway serves the authenticated collector WebSocket.
type Gateway struct {
	opts GatewayOptions
	log  *slog.Logger
	now  func() time.Time

	mu      sync.Mutex
	conns   map[*collectorConn]struct{}
	byID    map[string]*collectorConn
	closed  bool
	stop    chan struct{}
	stopOne sync.Once

	// generations is the per-collector credential generation. Explicit
	// revocation bumps it under mu; admission snapshots it and hello activation
	// re-checks it, so neither an in-progress upgrade nor a stale credential can
	// open a session.
	generations map[string]uint64
	// credentialGenerations is the per-credential-hash generation used by a
	// replacement enrollment. Collector ids can be reused by a successor, so
	// keying the fence by hash closes only the credential that was actually
	// superseded.
	credentialGenerations map[string]uint64
	// revocationEpoch counts completed revocations. Authentication reads it
	// before the registry lookup and admission re-checks it afterwards, so a
	// credential validated across a revocation boundary is looked up again.
	revocationEpoch uint64
	// credentialUnsub releases the live-state credential-invalidation
	// subscription.
	credentialUnsub func()
}

// NewGateway creates the gateway. It starts the revocation watcher when one is
// configured and nothing before ServeHTTP otherwise.
func NewGateway(opts GatewayOptions) (*Gateway, error) {
	if opts.Registry == nil || opts.Telemetry == nil || opts.State == nil {
		return nil, errors.New("ingest: registry, telemetry and live state are required")
	}
	if opts.MaxConnections <= 0 {
		opts.MaxConnections = DefaultMaxConnections
	}
	if opts.HelloTimeout <= 0 {
		opts.HelloTimeout = DefaultHelloTimeout
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = DefaultPingInterval
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.MessagesPerSecond <= 0 {
		opts.MessagesPerSecond = DefaultMessagesPerSecond
	}
	if opts.MessageBurst <= 0 {
		opts.MessageBurst = DefaultMessageBurst
	}
	if opts.SendQueueDepth <= 0 {
		opts.SendQueueDepth = defaultSendQueueDepth
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	gateway := &Gateway{
		opts:                  opts,
		log:                   logger,
		now:                   now,
		conns:                 make(map[*collectorConn]struct{}),
		byID:                  make(map[string]*collectorConn),
		generations:           make(map[string]uint64),
		credentialGenerations: make(map[string]uint64),
		stop:                  make(chan struct{}),
	}
	if opts.Revocations != nil {
		go gateway.watchRevocations()
	}
	// Enrollment replacement and explicit revocation publish through live
	// state, so any gateway sharing that state fences the old credential even
	// when the caller did not install a direct callback.
	gateway.credentialUnsub = opts.State.SubscribeCredentialInvalidation(gateway.InvalidateCredential)
	return gateway, nil
}

func (g *Gateway) watchRevocations() {
	for {
		select {
		case <-g.stop:
			return
		case collectorID, ok := <-g.opts.Revocations:
			if !ok {
				return
			}
			g.RevokeCollector(collectorID)
		}
	}
}

// ConnectedCount reports authenticated active sessions.
func (g *Gateway) ConnectedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	count := 0
	for conn := range g.conns {
		if conn.active() {
			count++
		}
	}
	return count
}

// ServeHTTP authenticates and upgrades the collector WebSocket. Non-upgrade
// requests are handled by the HTTP server's 426 path.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if originPresent(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "invalid bearer credentials", http.StatusUnauthorized)
		return
	}
	session, status, reason := g.admit(r.Context(), token)
	if session == nil {
		http.Error(w, reason, status)
		return
	}
	// The slot is reserved and the connection is registered before the upgrade,
	// so the connection budget is atomic. Every path below releases it.
	defer g.unregister(session)

	if g.opts.testAfterReserve != nil {
		g.opts.testAfterReserve()
	}
	if session.closed() {
		http.Error(w, "collector credential revoked", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(protocol.MaxDetailBytes)
	if !session.attach(conn) {
		// Revoked while the upgrade completed; the socket must not linger.
		_ = conn.CloseNow()
		return
	}
	session.serve(r.Context())
}

// admit authenticates one bearer credential and atomically reserves a
// connection slot fenced by that collector's credential generation.
//
// The registry lookup runs with the revocation epoch snapshotted, and admission
// re-checks the epoch under the gateway mutex that every revocation takes. A
// revocation that completed while the credential was being read therefore
// forces a fresh lookup instead of admitting a stale identity. The reserved
// generation is re-checked again when hello activates the session.
func (g *Gateway) admit(ctx context.Context, token string) (*collectorConn, int, string) {
	for attempt := 0; attempt < 3; attempt++ {
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return nil, http.StatusServiceUnavailable, "hub is shutting down"
		}
		epoch := g.revocationEpoch
		g.mu.Unlock()

		identity, found, err := g.opts.Registry.Authenticate(ctx, token)
		if err != nil {
			g.log.Warn("collector authentication failed", "error", err)
			return nil, http.StatusServiceUnavailable, "collector authentication unavailable"
		}
		if !found {
			return nil, http.StatusUnauthorized, "invalid collector credentials"
		}

		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return nil, http.StatusServiceUnavailable, "hub is shutting down"
		}
		if g.revocationEpoch != epoch {
			// A credential was revoked while this one was being read. The read
			// may have happened before the revocation landed, so repeat it.
			g.mu.Unlock()
			continue
		}
		if len(g.conns) >= g.opts.MaxConnections {
			g.mu.Unlock()
			return nil, http.StatusServiceUnavailable, "too many collector connections"
		}
		// An identity enrolled or replaced while the hub is running must be
		// visible before its first samples arrive. Registering under the gateway
		// lock keeps it ordered with revocation, which removes the same row.
		if err := g.opts.State.RegisterMachineWithCredential(identity.CollectorID, identity.MachineID, identity.TokenHash); err != nil {
			g.opts.State.ReplaceMachineWithCredential(identity.CollectorID, identity.MachineID, identity.TokenHash)
		}
		key := credentialKey(identity)
		conn := &collectorConn{
			id:                   newID(),
			identity:             identity,
			gateway:              g,
			sessionID:            "",
			send:                 make(chan []byte, g.opts.SendQueueDepth),
			done:                 make(chan struct{}),
			lastActive:           time.Now(),
			tokens:               g.opts.MessageBurst,
			lastRefill:           time.Now(),
			generation:           g.generations[identity.CollectorID],
			credential:           key,
			credentialGeneration: g.credentialGenerations[key],
		}
		conn.sessionID = conn.id
		g.conns[conn] = struct{}{}
		g.byID[conn.id] = conn
		g.mu.Unlock()
		return conn, 0, ""
	}
	return nil, http.StatusServiceUnavailable, "collector authentication unavailable"
}

// SendDetail forwards one hub message to the collector currently serving a
// machine. It reports false when the machine has no active session or the
// connection cannot accept the message.
func (g *Gateway) SendDetail(machineID string, message protocol.HubMessage) bool {
	data, err := protocol.EncodeHubMessage(message)
	if err != nil {
		g.log.Warn("encode hub message", "error", err)
		return false
	}
	g.mu.Lock()
	var target *collectorConn
	for conn := range g.conns {
		if conn.machine() == machineID && conn.active() {
			target = conn
			break
		}
	}
	g.mu.Unlock()
	if target == nil {
		return false
	}
	return target.enqueue(data)
}

// RevokeCollector closes the live socket of one collector and removes its live
// state. The registry lookup already fails for the revoked credential, so a
// reconnect is refused with 401.
//
// The credential generation is bumped under the gateway mutex before anything
// else, fencing both an in-progress upgrade and a hello that has not activated
// yet. Removing the machine row publishes the removal to SSE subscribers;
// OnDisconnect releases process-detail interests for that machine.
func (g *Gateway) RevokeCollector(collectorID string) {
	var targets []*collectorConn
	g.mu.Lock()
	g.generations[collectorID]++
	g.revocationEpoch++
	// Removal runs under the same lock admission registers a machine with, so
	// an in-flight upgrade can never re-add the row after it is dropped.
	machineID, removed := g.opts.State.RemoveCollector(collectorID)
	for conn := range g.conns {
		if conn.identity.CollectorID == collectorID {
			targets = append(targets, conn)
		}
	}
	g.mu.Unlock()

	for _, conn := range targets {
		conn.fail(ClosePolicy, "collector credential revoked")
	}
	if removed && g.opts.OnDisconnect != nil {
		g.opts.OnDisconnect(machineID)
	}
}

// InvalidateCredential fences and closes only the connections authenticated by
// one exact credential. It does not bump the collector-wide generation: a
// replacement enrollment commonly reuses the collector id for a new credential,
// and a delayed notification must not close that legitimate successor. The live
// mapping is removed only when it still points to the invalidated credential.
func (g *Gateway) InvalidateCredential(invalidation live.CredentialInvalidation) {
	if invalidation.CollectorID == "" {
		return
	}
	var targets []*collectorConn
	g.mu.Lock()
	// Fence an in-progress admission that read the registry before the
	// replacement committed, without touching a different credential.
	g.revocationEpoch++
	if invalidation.TokenHash != "" {
		g.credentialGenerations[invalidation.TokenHash]++
	}
	for conn := range g.conns {
		if conn.identity.CollectorID != invalidation.CollectorID {
			continue
		}
		if invalidation.TokenHash != "" && credentialKey(conn.identity) != invalidation.TokenHash {
			continue
		}
		targets = append(targets, conn)
	}
	removedMachineID, removed := "", false
	if invalidation.MachineID != "" {
		removedMachineID, removed = g.opts.State.RemoveCollectorIf(
			invalidation.CollectorID, invalidation.MachineID, invalidation.TokenHash,
		)
	}
	g.mu.Unlock()

	for _, conn := range targets {
		conn.fail(ClosePolicy, "collector credential revoked")
	}
	if removed && g.opts.OnDisconnect != nil {
		g.opts.OnDisconnect(removedMachineID)
	}
}

// credentialKey names one credential for the gateway fence. Stored identities
// carry the hash; an identity constructed without one falls back to the token
// so two different credentials never share a fence.
func credentialKey(identity storage.Identity) string {
	if identity.TokenHash != "" {
		return identity.TokenHash
	}
	return identity.Token
}

// Close ends every collector session with 1001 and refuses new upgrades.
func (g *Gateway) Close() {
	g.stopOne.Do(func() {
		close(g.stop)
		g.mu.Lock()
		g.closed = true
		targets := make([]*collectorConn, 0, len(g.conns))
		for conn := range g.conns {
			targets = append(targets, conn)
		}
		g.mu.Unlock()
		for _, conn := range targets {
			conn.fail(CloseShutdown, "hub shutting down")
		}
		if g.credentialUnsub != nil {
			g.credentialUnsub()
			g.credentialUnsub = nil
		}
	})
}

func (g *Gateway) unregister(conn *collectorConn) {
	g.mu.Lock()
	delete(g.conns, conn)
	delete(g.byID, conn.id)
	g.mu.Unlock()
	conn.closeOnce.Do(func() { close(conn.done) })
}

// originPresent reports whether the request carried an Origin header at all.
// Collector endpoints reject the header's presence regardless of value: an
// empty Origin is still browser-controlled and must not pass.
func originPresent(r *http.Request) bool {
	return len(r.Header.Values("Origin")) > 0
}

// protocolRange is SupportedProtocolRange split for the negotiation call.
func protocolRange() (int64, int64) {
	return SupportedProtocolRange()
}

// negotiate selects the session version with this hub's window.
func negotiate(marker *int64) (int64, bool, error) {
	minVersion, maxVersion := protocolRange()
	return NegotiateProtocol(marker, minVersion, maxVersion)
}

// collectorConn is one authenticated collector socket.
type collectorConn struct {
	id       string
	identity storage.Identity
	gateway  *Gateway
	// generation is the collector's credential generation captured when this
	// connection reserved its slot. Activation re-checks it under the gateway
	// mutex that revocation bumps it with.
	generation uint64
	// credential is the exact credential this connection authenticated with
	// (its hash, or the token when a legacy identity carries no hash), and
	// credentialGeneration fences that one credential across a replacement.
	credential           string
	credentialGeneration uint64

	// mu guards the fields below for readers outside this socket's goroutines.
	mu        sync.Mutex
	ws        *websocket.Conn
	activeFlg bool
	sessionID string
	machineID string

	send       chan []byte
	done       chan struct{}
	closeOnce  sync.Once
	lastActive time.Time

	// Token bucket state is only touched by the read goroutine.
	tokens     float64
	lastRefill time.Time

	inFlight int
}

// attach installs the upgraded socket. It reports false when the connection was
// revoked while the upgrade was in progress, in which case the caller closes
// the socket instead of serving it.
func (c *collectorConn) attach(ws *websocket.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return false
	default:
	}
	c.ws = ws
	return true
}

// closed reports whether this connection has already been closed or revoked.
func (c *collectorConn) closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *collectorConn) active() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeFlg
}

func (c *collectorConn) machine() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.machineID
}

func (c *collectorConn) socket() *websocket.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws
}

func (c *collectorConn) serve(ctx context.Context) {
	ws := c.socket()
	if ws == nil {
		return
	}
	writerDone := make(chan struct{})
	go c.writeLoop(writerDone)

	hello := time.AfterFunc(c.gateway.opts.HelloTimeout, func() {
		if !c.active() {
			c.fail(CloseHelloTimeout, "hello deadline exceeded")
		}
	})
	defer hello.Stop()

	ping := time.NewTicker(c.gateway.opts.PingInterval)
	defer ping.Stop()
	pingDone := make(chan struct{})
	defer close(pingDone)
	go c.pingLoop(ping, pingDone)

	for {
		messageType, data, err := ws.Read(ctx)
		if err != nil {
			break
		}
		c.mu.Lock()
		c.lastActive = time.Now()
		c.mu.Unlock()
		if messageType != websocket.MessageText {
			c.fail(CloseInvalidMessage, "expected a JSON text message")
			break
		}
		if len(data) > protocol.MaxDetailBytes {
			c.fail(CloseMessageTooLarge, "message too large")
			break
		}
		if !c.refill() {
			c.fail(CloseRateLimit, "message rate limit exceeded")
			break
		}
		if !c.handle(data) {
			break
		}
	}
	c.endSession()
	c.fail(websocket.StatusNormalClosure, "connection closed")
	<-writerDone
}

// writeLoop serializes outbound frames. A queue overflow or a write that
// exceeds its deadline ends the connection rather than growing without bound.
func (c *collectorConn) writeLoop(done chan<- struct{}) {
	defer close(done)
	ws := c.socket()
	if ws == nil {
		return
	}
	for {
		select {
		case <-c.done:
			return
		case data := <-c.send:
			writeCtx, cancel := context.WithTimeout(context.Background(), defaultWriteTimeout)
			err := ws.Write(writeCtx, websocket.MessageText, data)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (c *collectorConn) pingLoop(ticker *time.Ticker, done <-chan struct{}) {
	ws := c.socket()
	if ws == nil {
		return
	}
	for {
		select {
		case <-c.done:
			return
		case <-done:
			return
		case <-ticker.C:
			c.mu.Lock()
			idleFor := time.Since(c.lastActive)
			c.mu.Unlock()
			if idleFor > c.gateway.opts.IdleTimeout {
				c.terminate()
				return
			}
			pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := ws.Ping(pingCtx)
			cancel()
			if err != nil {
				c.terminate()
				return
			}
			// A successful ping/pong proves the collector is alive even when it
			// sends no telemetry (for example a machine with no capabilities).
			c.mu.Lock()
			c.lastActive = time.Now()
			c.mu.Unlock()
		}
	}
}

func (c *collectorConn) refill() bool {
	now := time.Now()
	elapsed := now.Sub(c.lastRefill).Seconds()
	if elapsed > 0 {
		c.tokens += elapsed * c.gateway.opts.MessagesPerSecond
		if c.tokens > c.gateway.opts.MessageBurst {
			c.tokens = c.gateway.opts.MessageBurst
		}
	}
	c.lastRefill = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

// handle processes one decoded frame. It returns false when the connection must
// close.
func (c *collectorConn) handle(data []byte) bool {
	message, err := protocol.DecodeCollectorMessage(data)
	if err != nil {
		c.fail(CloseInvalidMessage, err.Error())
		return false
	}
	if !c.active() {
		hello, ok := message.(protocol.HelloMessage)
		if !ok {
			c.fail(CloseProtocolStep, "first message must be hello")
			return false
		}
		return c.handleHello(hello)
	}
	switch typed := message.(type) {
	case protocol.HelloMessage:
		c.fail(CloseProtocolStep, "duplicate hello")
		return false
	case protocol.SamplesMessage:
		return c.handleSamples(typed)
	case protocol.InventoryMessage:
		return c.handleInventory(typed)
	case protocol.StatusMessage:
		c.gateway.opts.State.SetDelivery(c.sessionID, typed)
		return true
	case protocol.DetailResult, protocol.DetailEnded:
		if c.gateway.opts.OnDetail != nil {
			c.gateway.opts.OnDetail(c.machineID, typed)
		}
		return true
	default:
		c.fail(CloseInvalidMessage, "unsupported collector message")
		return false
	}
}

func (c *collectorConn) handleHello(hello protocol.HelloMessage) bool {
	if hello.CollectorID != c.identity.CollectorID {
		c.fail(CloseIdentityMismatch, "hello collectorId does not match authenticated identity")
		return false
	}
	selected, present, err := negotiate(hello.Protocol)
	if err != nil {
		c.fail(ClosePolicy, err.Error())
		return false
	}
	if c.gateway.opts.testBeforeActivate != nil {
		c.gateway.opts.testBeforeActivate()
	}
	info, predecessor, err := c.activate(hello)
	if err != nil {
		switch {
		case errors.Is(err, errCollectorRevoked):
			c.fail(ClosePolicy, err.Error())
		case errors.Is(err, errHubShuttingDown):
			c.fail(CloseShutdown, err.Error())
		case errors.Is(err, live.ErrSessionConflict):
			c.fail(CloseSessionConflict, err.Error())
		case errors.Is(err, live.ErrIdentityMismatch):
			c.fail(CloseIdentityMismatch, err.Error())
		default:
			c.fail(ClosePolicy, err.Error())
		}
		return false
	}
	// Only the exact predecessor session is closed. A newer concurrent hello
	// has already been serialized through the same gateway lock, so it can
	// never be the socket this one terminates.
	if predecessor != nil {
		predecessor.terminate()
	}

	welcome := protocol.WelcomeMessage{
		HubRunID:           c.gateway.opts.State.HubRunID(),
		MachineID:          info.MachineID,
		Delivery:           protocol.DeliveryDurable,
		MaxInFlightBatches: protocol.MaxInFlightBatches,
	}
	if !c.gateway.opts.Telemetry.Persistent() {
		welcome.Delivery = protocol.DeliveryMemory
	}
	if present {
		version := selected
		welcome.Protocol = &version
	}
	if !c.sendMessage(welcome) {
		return false
	}
	c.gateway.log.Info("collector session opened",
		"collectorId", hello.CollectorID, "machineId", info.MachineID,
		"runId", hello.RunID, "protocol", selected)
	return true
}

var (
	// errCollectorRevoked means the credential generation captured at admission
	// is no longer current: the credential was revoked before activation.
	errCollectorRevoked = errors.New("collector credential revoked")
	// errHubShuttingDown means the hub closed between admission and activation.
	errHubShuttingDown = errors.New("hub is shutting down")
)

// activate performs the authenticated hello transition. The gateway mutex is
// held across the generation re-check and the state transition, which also
// serializes activation for one collector: concurrent hellos produce one
// ordered chain of predecessors instead of racing each other's socket closes.
// The returned predecessor is the exact session the transition replaced.
func (c *collectorConn) activate(hello protocol.HelloMessage) (live.SessionInfo, *collectorConn, error) {
	g := c.gateway
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return live.SessionInfo{}, nil, errHubShuttingDown
	}
	if g.generations[hello.CollectorID] != c.generation ||
		g.credentialGenerations[c.credential] != c.credentialGeneration {
		return live.SessionInfo{}, nil, errCollectorRevoked
	}
	result, err := g.opts.State.StartSession(live.StartSessionInput{
		SessionID:          c.sessionID,
		CollectorID:        hello.CollectorID,
		CredentialHash:     c.identity.TokenHash,
		RunID:              hello.RunID,
		BootID:             hello.BootID,
		Hostname:           hello.Hostname,
		ExpectedIntervalMS: hello.ExpectedIntervalMS,
	})
	if err != nil {
		return live.SessionInfo{}, nil, err
	}
	c.mu.Lock()
	c.activeFlg = true
	c.machineID = result.Session.MachineID
	c.mu.Unlock()
	var predecessor *collectorConn
	if result.ReplacedSessionID != "" {
		predecessor = g.byID[result.ReplacedSessionID]
	}
	return result.Session, predecessor, nil
}

func (c *collectorConn) handleSamples(samples protocol.SamplesMessage) bool {
	info, ok := c.gateway.opts.State.CurrentSession(c.sessionID)
	if !ok {
		c.fail(ClosePolicy, "session is no longer current")
		return false
	}
	for _, record := range samples.Records {
		envelope := envelopeOf(record)
		if envelope.CollectorID != c.identity.CollectorID {
			c.fail(CloseInvalidMessage, "record identity does not match authenticated session")
			return false
		}
		if samples.Mode == protocol.SampleModeLive {
			if envelope.RunID != info.RunID || envelope.BootID != info.BootID {
				c.fail(CloseInvalidMessage, "record identity does not match authenticated session")
				return false
			}
		}
	}
	c.mu.Lock()
	if c.inFlight >= protocol.MaxInFlightBatches {
		c.mu.Unlock()
		retry := int64(retryAfterOverloadMS)
		c.sendMessage(protocol.AckMessage{RetryAfterMS: &retry})
		return true
	}
	c.inFlight++
	c.mu.Unlock()
	receivedAt := c.gateway.now()
	// Live publication is intentionally independent of the commit: a slow
	// flush must not make the dashboard one flush behind. A retry or a later
	// failure cannot publish twice because the state ignores non-increasing
	// run/sequence pairs and a replaced session can no longer publish.
	if samples.Mode == protocol.SampleModeLive {
		if newest, ok := c.selectLiveRecord(info, samples.Records, receivedAt); ok {
			if c.gateway.opts.State.ApplyMetrics(c.sessionID, newest, receivedAt) {
				// Only the record that was actually published may reach the
				// alert engine; a duplicate or superseded sequence must not
				// evaluate a second time.
				if c.gateway.opts.OnLiveSample != nil {
					c.gateway.opts.OnLiveSample(info.MachineID, newest, receivedAt)
				}
			}
		}
	}
	expected := info.ExpectedIntervalMS
	hostname := info.Hostname
	go c.commitSamples(samples, storage.Batch{
		MachineID:          info.MachineID,
		CollectorID:        info.CollectorID,
		CredentialHash:     c.identity.TokenHash,
		Mode:               samples.Mode,
		ExpectedIntervalMs: &expected,
		Hostname:           &hostname,
		Records:            samples.Records,
		ReceivedAt:         receivedAt,
	})
	return true
}

// commitSamples waits for the storage commit and answers the batch only then.
func (c *collectorConn) commitSamples(samples protocol.SamplesMessage, batch storage.Batch) {
	defer func() {
		c.mu.Lock()
		c.inFlight--
		c.mu.Unlock()
	}()
	if c.gateway.opts.testBeforeCommit != nil {
		c.gateway.opts.testBeforeCommit()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A credential replaced or revoked after admission must not commit the
	// batch it already accepted. Re-check the gateway generation, the live
	// session and the registry's current credential immediately before
	// persistence. The registry read also covers the small window between a
	// replacement commit and the gateway notification.
	if !c.gateway.credentialCurrent(c) || !c.sessionCurrent() || !c.credentialInRegistry(ctx) {
		if c.gateway.opts.testFencedCommit != nil {
			c.gateway.opts.testFencedCommit()
		}
		c.fail(ClosePolicy, "collector credential is no longer current")
		return
	}
	result, err := c.gateway.opts.Telemetry.Enqueue(ctx, batch)
	if err != nil {
		if errors.Is(err, storage.ErrCredentialSuperseded) {
			// The credential was replaced while the batch waited in the
			// telemetry queue. Nothing was persisted and no acknowledgement was
			// sent, so close the stale socket instead of asking it to retry.
			c.gateway.log.Warn("sample batch dropped: collector credential is no longer current",
				"machineId", batch.MachineID, "records", len(batch.Records))
			c.fail(ClosePolicy, "collector credential is no longer current")
			return
		}
		retry := int64(retryAfterOverloadMS)
		if delay := storage.RetryAfter(err); delay > 0 {
			retry = delay.Milliseconds()
		}
		if retry < 1 {
			retry = retryAfterOverloadMS
		}
		c.gateway.log.Warn("sample batch not committed",
			"machineId", batch.MachineID, "records", len(batch.Records), "error", err)
		c.sendMessage(protocol.AckMessage{RetryAfterMS: &retry})
		return
	}
	accepted := result.Accepted
	if accepted == nil {
		accepted = []protocol.RecordRef{}
	}
	rejected := result.Rejected
	if rejected == nil {
		rejected = []protocol.RejectedRecord{}
	}
	c.sendMessage(protocol.AckMessage{Accepted: accepted, Rejected: rejected})
}

// credentialCurrent reports whether the collector credential generation
// captured at admission is still the current one. Revocation and replacement
// bump the generation under the gateway mutex this check takes.
func (g *Gateway) credentialCurrent(conn *collectorConn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	return g.generations[conn.identity.CollectorID] == conn.generation &&
		g.credentialGenerations[conn.credential] == conn.credentialGeneration
}

// sessionCurrent reports whether this connection still owns the live session.
func (c *collectorConn) sessionCurrent() bool {
	_, ok := c.gateway.opts.State.CurrentSession(c.sessionID)
	return ok
}

// credentialInRegistry re-reads the presented credential. A replacement
// enrollment deletes the old hash in its own transaction, so this closes the
// gap before the gateway notification reaches this connection.
func (c *collectorConn) credentialInRegistry(ctx context.Context) bool {
	identity, found, err := c.gateway.opts.Registry.Authenticate(ctx, c.identity.Token)
	if err != nil || !found {
		return false
	}
	return identity.CollectorID == c.identity.CollectorID && identity.MachineID == c.identity.MachineID
}

func (c *collectorConn) handleInventory(message protocol.InventoryMessage) bool {
	info, ok := c.gateway.opts.State.CurrentSession(c.sessionID)
	if !ok {
		c.fail(ClosePolicy, "session is no longer current")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), inventoryTimeout)
	capturedAt, err := time.Parse(time.RFC3339Nano, message.CapturedAt)
	if err != nil {
		capturedAt = time.Now()
	}
	err = c.gateway.opts.Telemetry.SaveInventory(ctx, info.MachineID, capturedAt, message.Inventory)
	cancel()
	if err != nil {
		c.gateway.log.Warn("inventory not persisted", "machineId", info.MachineID, "error", err)
	}
	if c.gateway.opts.testAfterInventoryPersist != nil {
		c.gateway.opts.testAfterInventoryPersist()
	}
	// The live application is session-aware: a collector session replaced
	// while the inventory was being persisted must not overwrite the newer
	// session's inventory.
	applied := c.gateway.opts.State.SetSessionInventory(c.sessionID, message.Inventory)
	if c.gateway.opts.testAfterInventoryApply != nil {
		c.gateway.opts.testAfterInventoryApply(applied)
	}
	if !applied {
		c.fail(ClosePolicy, "session is no longer current")
		return false
	}
	return true
}

func (c *collectorConn) endSession() {
	c.mu.Lock()
	wasActive := c.activeFlg
	localSession := c.sessionID
	c.activeFlg = false
	c.mu.Unlock()
	if !wasActive {
		return
	}
	// EndSession reports false when a newer run already replaced this session;
	// in that case the replacement stays connected and no disconnect fires.
	if machineID, ended := c.gateway.opts.State.EndSession(localSession); ended {
		if c.gateway.opts.OnDisconnect != nil {
			c.gateway.opts.OnDisconnect(machineID)
		}
	}
}

func (c *collectorConn) sendMessage(message protocol.HubMessage) bool {
	data, err := protocol.EncodeHubMessage(message)
	if err != nil {
		c.gateway.log.Warn("encode hub message", "error", err)
		return false
	}
	return c.enqueue(data)
}

// enqueue keeps one bounded outbound queue. A collector that cannot drain 64
// hub messages is disconnected so memory stays bounded.
func (c *collectorConn) enqueue(data []byte) bool {
	select {
	case <-c.done:
		return false
	case c.send <- data:
		return true
	default:
		c.fail(ClosePolicy, "collector is not draining hub messages")
		return false
	}
}

// fail closes with a status code and a bounded reason.
func (c *collectorConn) fail(code websocket.StatusCode, reason string) {
	if len(reason) > 120 {
		reason = reason[:120]
	}
	c.closeOnce.Do(func() {
		close(c.done)
		if ws := c.socket(); ws != nil {
			_ = ws.Close(code, reason)
		}
	})
}

// terminate closes without a handshake; used for idle and superseded sockets.
func (c *collectorConn) terminate() {
	c.closeOnce.Do(func() {
		close(c.done)
		if ws := c.socket(); ws != nil {
			_ = ws.CloseNow()
		}
	})
}

// envelopeOf returns the identity envelope of a metrics or gap record.
func envelopeOf(record protocol.TelemetryRecord) protocol.RecordEnvelope {
	switch typed := record.(type) {
	case protocol.MetricsRecord:
		return typed.RecordEnvelope
	case *protocol.MetricsRecord:
		return typed.RecordEnvelope
	case protocol.GapRecord:
		return typed.RecordEnvelope
	case *protocol.GapRecord:
		return typed.RecordEnvelope
	default:
		return protocol.RecordEnvelope{}
	}
}

// metricsOf returns the metrics record behind a telemetry record value or
// pointer. Other record kinds (gaps) have no metrics payload.
func metricsOf(record protocol.TelemetryRecord) (protocol.MetricsRecord, bool) {
	switch typed := record.(type) {
	case protocol.MetricsRecord:
		return typed, true
	case *protocol.MetricsRecord:
		if typed != nil {
			return *typed, true
		}
	}
	return protocol.MetricsRecord{}, false
}

// selectLiveRecord picks the record to publish from one LIVE batch.
//
// Selection matches the store's ordering for a single authenticated run: the
// highest sequence wins, not the newest capture time. A record outside the
// store's replay horizon or clock-skew allowance is never eligible, so a
// record the commit path rejects can never appear fresh on the dashboard.
func (c *collectorConn) selectLiveRecord(info live.SessionInfo, records []protocol.TelemetryRecord, receivedAt time.Time) (protocol.MetricsRecord, bool) {
	var best protocol.MetricsRecord
	found := false
	for _, record := range records {
		metrics, ok := metricsOf(record)
		if !ok {
			continue
		}
		if metrics.RunID != info.RunID || metrics.BootID != info.BootID {
			continue
		}
		if rejection := c.gateway.opts.Telemetry.CaptureRejection(metrics.CollectorID, metrics.CapturedAt, receivedAt); rejection != "" {
			continue
		}
		if !found || metrics.Sequence > best.Sequence {
			best = metrics
			found = true
		}
	}
	return best, found
}

// bearerToken parses an Authorization: Bearer credential. Tokens are never
// accepted from the URL.
func bearerToken(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if len(header) < len("bearer ") || !strings.EqualFold(header[:len("bearer ")], "bearer ") {
		return "", false
	}
	token := strings.TrimSpace(header[len("bearer "):])
	if token == "" || len(token) > 4096 {
		return "", false
	}
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e {
			return "", false
		}
	}
	return token, true
}

// newID returns a random RFC 4122 version 4 identifier.
func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("conn-%d", time.Now().UnixNano())
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
