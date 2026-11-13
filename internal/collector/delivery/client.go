package delivery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/state"
	"github.com/Triet1402/rigscope/internal/protocol"
	"github.com/coder/websocket"
)

// Identity is the per-run collector identity advertised in hello.
type Identity struct {
	CollectorID string
	// RunID is new on every process launch.
	RunID    string
	BootID   string
	Hostname string
}

// DialFunc establishes the WebSocket. It is replaceable for tests; the default
// is websocket.Dial.
type DialFunc func(ctx context.Context, url string, options *websocket.DialOptions) (*websocket.Conn, *http.Response, error)

// Config configures one delivery client.
type Config struct {
	// HubURL is the ws:// or wss:// collector endpoint.
	HubURL string
	// Credential is the bearer token. It is only ever placed in the
	// Authorization header.
	Credential string
	// CAFile optionally pins a PEM CA bundle. When set it replaces the system
	// trust store for this connection; when empty the system roots are used.
	CAFile   string
	Identity Identity

	// ExpectedInterval is the nominal delivery cadence advertised in hello.
	// Defaults to one second.
	ExpectedInterval time.Duration
	// Capabilities defaults to metrics + inventory.
	Capabilities []protocol.CapabilityName

	Clock func() time.Time
	Rand  func() float64
	Logf  func(format string, args ...any)

	// AllocateSequence is the run's one sequence allocator. It numbers both
	// sampler records and the gap records delivery creates, so no two records
	// of one run can share a sequence. Required: a private gap counter would
	// collide with metric sequences and corrupt hub deduplication.
	AllocateSequence func() int64

	// QueueLimits overrides the RAM queue defaults. Zero fields keep the
	// default.
	QueueLimits QueueLimits

	WelcomeTimeout time.Duration
	IdleTimeout    time.Duration
	PingInterval   time.Duration
	AckTimeout     time.Duration

	ReplayBatchesPerSecond  float64
	MaxLocalInFlightBatches int
	MaxBatchBytes           int

	BackoffBase time.Duration
	BackoffMax  time.Duration

	// HTTPClient overrides the TLS transport used by the default dialer.
	HTTPClient *http.Client
	// Dial overrides the WebSocket dialer (tests and future transports).
	Dial DialFunc
	// OnDisconnect is called after a welcomed session is lost. Detail
	// subscriptions bound to that connection should be cancelled.
	OnDisconnect func()
	// OnWelcome is called after every successful welcome, before inventory and
	// the delivery loop start. It is optional; a caller can use it to report
	// first-connect readiness or reset connection-scoped work.
	OnWelcome func()
}

const (
	defaultExpectedInterval = time.Second
	defaultWelcomeTimeout   = 10 * time.Second
	defaultIdleTimeout      = 45 * time.Second
	defaultPingInterval     = 15 * time.Second
	defaultAckTimeout       = 30 * time.Second
	defaultReplayRate       = 4.0
	defaultMaxInFlight      = 64
	defaultMaxBatchBytes    = 200_000
	defaultBackoffBase      = time.Second
	defaultBackoffMax       = 30 * time.Second

	// healthyConnectionReset is how long a session must stay welcomed before
	// the reconnect attempt counter resets.
	healthyConnectionReset = 30 * time.Second
	// credentialsRejectedRetry is the minimum retry delay after HTTP 401: the
	// credential is revoked or the hub forgot it, so retrying every few
	// seconds cannot help.
	credentialsRejectedRetry = 5 * time.Minute
	// controlQueueDepth bounds connection-scoped control messages. Telemetry is
	// never dropped by this bound; a full control queue drops the message.
	controlQueueDepth = 64
	// detailQueueDepth bounds hub subscription requests handed to the caller.
	detailQueueDepth = 16
	// writeTimeout bounds one outbound frame write.
	writeTimeout = 10 * time.Second
	// batchEnvelopeBytes is slack for the samples envelope when asking the
	// queue for a byte-bounded replay batch.
	batchEnvelopeBytes = 4096
)

// DetailRequest is one hub detail subscription or cancellation.
type DetailRequest struct {
	Subscribe *protocol.DetailSubscribe
	Cancel    *protocol.DetailCancel
}

// Stats is a point-in-time operational view.
type Stats struct {
	Connected  bool
	Welcomed   bool
	Attempts   int
	Reconnects int64
	LastError  string
	Queue      QueueStats
}

// QueueStats is the delivery queue's observable state.
type QueueStats struct {
	PendingRecords          int64
	PendingBytes            int64
	OldestPendingCapturedAt *time.Time
	InFlightRecords         int64
	DroppedRecords          int64
	GapRecords              int64
	Replaying               bool
	LastAckAt               *time.Time
}

// Client owns one hub connection and its queue.
type Client struct {
	cfg        Config
	queue      *queue
	dial       DialFunc
	httpClient *http.Client

	control chan []byte
	detail  chan DetailRequest
	wake    chan struct{}
	stop    chan struct{}
	runDone chan struct{}

	stopOnce  sync.Once
	started   atomic.Bool
	draining  atomic.Bool
	closed    atomic.Bool
	runExited atomic.Bool

	mu         sync.Mutex
	connected  bool
	welcomed   bool
	attempts   int
	reconnects int64
	lastError  string
	inventory  *protocol.Inventory
	// replayPausedUntil is a hub retryAfterMs deadline. It belongs to the
	// client, not one connection, so a reconnect does not discard the pause.
	replayPausedUntil time.Time
	// reportedRejectedCredentials is set after the first 401 so the guidance
	// line is written once, not on every long backoff.
	reportedRejectedCredentials bool
	// finalQueue preserves the last queue counters after Run exits, so
	// shutdown diagnostics do not read as an empty queue.
	finalQueue *QueueStats
}

// New validates the configuration and creates a client. Nothing connects until
// Run is called.
func New(cfg Config) (*Client, error) {
	if _, err := state.ValidateHubURL(cfg.HubURL); err != nil {
		return nil, fmt.Errorf("delivery: %w", err)
	}
	if !printableASCII(cfg.Credential, 1, 256) {
		return nil, errors.New("delivery: credential must be 1..256 printable ASCII characters")
	}
	if !printableASCII(cfg.Identity.CollectorID, 1, 128) {
		return nil, errors.New("delivery: identity collector id must be 1..128 printable ASCII characters")
	}
	if !printableASCII(cfg.Identity.RunID, 1, 128) {
		return nil, errors.New("delivery: identity run id must be 1..128 printable ASCII characters")
	}
	if len(cfg.Identity.BootID) > 128 || len(cfg.Identity.Hostname) > 255 {
		return nil, errors.New("delivery: boot id or hostname exceeds its wire bound")
	}
	if cfg.ExpectedInterval == 0 {
		cfg.ExpectedInterval = defaultExpectedInterval
	}
	if cfg.ExpectedInterval < time.Duration(protocol.MinExpectedIntervalMS)*time.Millisecond ||
		cfg.ExpectedInterval > time.Duration(protocol.MaxExpectedIntervalMS)*time.Millisecond {
		return nil, fmt.Errorf("delivery: expected interval must be %d..%d ms",
			protocol.MinExpectedIntervalMS, protocol.MaxExpectedIntervalMS)
	}
	if len(cfg.Capabilities) == 0 {
		cfg.Capabilities = []protocol.CapabilityName{protocol.CapabilityMetrics, protocol.CapabilityInventory}
	}
	if err := validateCapabilities(cfg.Capabilities); err != nil {
		return nil, err
	}
	if cfg.WelcomeTimeout == 0 {
		cfg.WelcomeTimeout = defaultWelcomeTimeout
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = defaultPingInterval
	}
	if cfg.AckTimeout == 0 {
		cfg.AckTimeout = defaultAckTimeout
	}
	if cfg.ReplayBatchesPerSecond == 0 {
		cfg.ReplayBatchesPerSecond = defaultReplayRate
	}
	if cfg.MaxLocalInFlightBatches == 0 {
		cfg.MaxLocalInFlightBatches = defaultMaxInFlight
	}
	if cfg.MaxBatchBytes == 0 {
		cfg.MaxBatchBytes = defaultMaxBatchBytes
	}
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = defaultBackoffBase
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = defaultBackoffMax
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Dial == nil {
		cfg.Dial = websocket.Dial
	}
	if cfg.WelcomeTimeout <= 0 || cfg.IdleTimeout <= 0 || cfg.PingInterval <= 0 || cfg.AckTimeout <= 0 {
		return nil, errors.New("delivery: timeouts must be positive")
	}
	if cfg.PingInterval >= cfg.IdleTimeout {
		cfg.PingInterval = cfg.IdleTimeout / 2
	}
	if cfg.ReplayBatchesPerSecond <= 0 {
		return nil, errors.New("delivery: replay rate must be positive")
	}
	if cfg.MaxLocalInFlightBatches < 1 || cfg.MaxLocalInFlightBatches > protocol.MaxInFlightBatches {
		return nil, fmt.Errorf("delivery: local in-flight window must be 1..%d", protocol.MaxInFlightBatches)
	}
	if cfg.MaxBatchBytes < 1 || cfg.MaxBatchBytes >= protocol.MaxMessageBytes {
		return nil, fmt.Errorf("delivery: MaxBatchBytes must be 1..%d", protocol.MaxMessageBytes-1)
	}
	if cfg.BackoffBase <= 0 || cfg.BackoffMax < cfg.BackoffBase {
		return nil, errors.New("delivery: backoff bounds are invalid")
	}
	if cfg.AllocateSequence == nil {
		return nil, errors.New("delivery: AllocateSequence is required so metrics and gaps share one per-run sequence allocator")
	}
	limits := DefaultQueueLimits()
	if cfg.QueueLimits.MaxAge != 0 {
		limits.MaxAge = cfg.QueueLimits.MaxAge
	}
	if cfg.QueueLimits.MaxBytes != 0 {
		limits.MaxBytes = cfg.QueueLimits.MaxBytes
	}
	if cfg.QueueLimits.MaxGapRecords != 0 {
		limits.MaxGapRecords = cfg.QueueLimits.MaxGapRecords
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	httpClient, err := buildHTTPClient(cfg.HTTPClient, cfg.CAFile)
	if err != nil {
		return nil, err
	}

	return &Client{
		cfg: cfg,
		queue: newQueue(queueOptions{
			limits: limits,
			identity: QueueIdentity{
				CollectorID: cfg.Identity.CollectorID,
				RunID:       cfg.Identity.RunID,
				BootID:      cfg.Identity.BootID,
			},
			allocate: cfg.AllocateSequence,
			clock:    cfg.Clock,
		}),
		dial:       cfg.Dial,
		httpClient: httpClient,
		control:    make(chan []byte, controlQueueDepth),
		detail:     make(chan DetailRequest, detailQueueDepth),
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
		runDone:    make(chan struct{}),
	}, nil
}

func validateCapabilities(capabilities []protocol.CapabilityName) error {
	if len(capabilities) > len(protocol.CapabilityNames) {
		return errors.New("delivery: too many capabilities")
	}
	known := make(map[protocol.CapabilityName]bool, len(protocol.CapabilityNames))
	for _, name := range protocol.CapabilityNames {
		known[name] = true
	}
	seen := make(map[protocol.CapabilityName]bool, len(capabilities))
	for _, capability := range capabilities {
		if !known[capability] {
			return fmt.Errorf("delivery: unsupported capability %q", capability)
		}
		if seen[capability] {
			return fmt.Errorf("delivery: duplicate capability %q", capability)
		}
		seen[capability] = true
	}
	if !seen[protocol.CapabilityMetrics] {
		return errors.New("delivery: capabilities must include metrics")
	}
	return nil
}

// buildHTTPClient creates the TLS transport for the WebSocket (and nothing
// else). Without CAFile it uses the system roots. With CAFile it trusts only
// that pinned bundle, so a certificate from any other CA is rejected even when
// that other CA is in the system store.
//
// Redirects are never followed. coder/websocket follows HTTP redirects by
// default, and Go preserves the Authorization header on a same-host
// https -> http redirect, which would send the credential in cleartext and
// bypass the loopback-only rule for plaintext.
func buildHTTPClient(existing *http.Client, caFile string) (*http.Client, error) {
	rejectRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if existing != nil {
		clone := *existing
		clone.CheckRedirect = rejectRedirect
		return &clone, nil
	}
	var pool *x509.CertPool
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("delivery: read CA file: %w", err)
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("delivery: CA file contains no usable certificate")
		}
	} else {
		system, err := x509.SystemCertPool()
		if err != nil || system == nil {
			system = x509.NewCertPool()
		}
		pool = system
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport, CheckRedirect: rejectRedirect}, nil
}

// Enqueue admits one already-filtered telemetry record. It never blocks. A
// record that cannot fit the queue is accounted for with a gap record.
func (c *Client) Enqueue(record protocol.TelemetryRecord) error {
	if c.draining.Load() || c.closed.Load() {
		return errors.New("delivery: client is shutting down")
	}
	if err := c.queue.append(record); err != nil {
		return err
	}
	c.notify()
	return nil
}

// SetInventory stores the latest inventory and sends it when connected.
// Inventory is connection-scoped state and is re-announced after every
// welcome.
func (c *Client) SetInventory(inventory protocol.Inventory) bool {
	detached, err := detachInventory(inventory)
	if err != nil {
		c.cfg.Logf("delivery: rejected inventory: %v", err)
		return false
	}
	c.mu.Lock()
	c.inventory = &detached
	connected := c.connected && c.welcomed
	c.mu.Unlock()
	if !connected {
		return false
	}
	return c.sendInventory(detached)
}

func (c *Client) sendInventory(inventory protocol.Inventory) bool {
	message := protocol.InventoryMessage{
		CapturedAt: formatTimestamp(c.cfg.Clock()),
		Inventory:  inventory,
	}
	return c.sendControl(message)
}

func detachInventory(inventory protocol.Inventory) (protocol.Inventory, error) {
	raw, err := json.Marshal(inventory)
	if err != nil {
		return protocol.Inventory{}, err
	}
	return protocol.ParseInventory(raw)
}

// SendStatus sends one status snapshot built from the current queue state.
func (c *Client) SendStatus(failures []protocol.CollectionFailure) bool {
	return c.SendStatusMessage(c.StatusSnapshot(failures))
}

// StatusSnapshot captures the current queue state as a status message without
// sending it. A caller that samples at the same instant can capture the state
// before admitting the new record and send the message afterwards, so the
// report never describes a record it just produced as backlog.
func (c *Client) StatusSnapshot(failures []protocol.CollectionFailure) protocol.StatusMessage {
	queueStats := c.queue.status()
	return protocol.StatusMessage{
		CapturedAt: formatTimestamp(c.cfg.Clock()),
		Delivery:   deliveryStatusOf(queueStats),
		Failures:   failures,
	}
}

// SendStatusMessage sends a previously captured status snapshot.
func (c *Client) SendStatusMessage(message protocol.StatusMessage) bool {
	return c.sendControl(message)
}

func deliveryStatusOf(stats queueStats) protocol.DeliveryStatus {
	mode := protocol.QueueModeMemory
	status := protocol.DeliveryStatus{
		QueueMode:       &mode,
		PendingRecords:  stats.PendingRecords,
		PendingBytes:    stats.PendingBytes,
		InFlightRecords: stats.InFlightRecords,
		DroppedRecords:  stats.DroppedRecords,
		Replaying:       stats.Replaying,
	}
	if stats.OldestPendingCapturedAt != nil {
		value := formatTimestamp(*stats.OldestPendingCapturedAt)
		status.OldestPendingCapturedAt = &value
	}
	if stats.LastAckAt != nil {
		value := formatTimestamp(*stats.LastAckAt)
		status.LastAckAt = &value
	}
	return status
}

// Detail returns the channel of hub subscription requests. It is closed when
// the client stops. A slow consumer loses requests rather than blocking
// delivery; lease expiry is the hub's safety net.
func (c *Client) Detail() <-chan DetailRequest { return c.detail }

// SendDetail sends one request-scoped process snapshot. It is never queued.
func (c *Client) SendDetail(result protocol.DetailResult) bool {
	return c.sendControl(result)
}

// SendDetailEnded closes a detail subscription.
func (c *Client) SendDetailEnded(ended protocol.DetailEnded) bool {
	return c.sendControl(ended)
}

func (c *Client) sendControl(message protocol.CollectorMessage) bool {
	c.mu.Lock()
	connected := c.connected && c.welcomed
	c.mu.Unlock()
	if !connected || c.closed.Load() {
		return false
	}
	data, err := protocol.EncodeCollectorMessage(message)
	if err != nil {
		c.cfg.Logf("delivery: dropping invalid %T: %v", message, err)
		return false
	}
	select {
	case c.control <- data:
		return true
	default:
		c.cfg.Logf("delivery: dropping %T (control queue full)", message)
		return false
	}
}

// Stats returns a consistent snapshot for diagnostics and tests.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	stats := Stats{
		Connected:  c.connected,
		Welcomed:   c.welcomed,
		Attempts:   c.attempts,
		Reconnects: c.reconnects,
		LastError:  c.lastError,
	}
	final := c.finalQueue
	c.mu.Unlock()
	if c.runExited.Load() && final != nil {
		stats.Queue = *final
		return stats
	}
	stats.Queue = publicQueueStats(c.queue.status())
	return stats
}

func publicQueueStats(queueStats queueStats) QueueStats {
	return QueueStats{
		PendingRecords:          queueStats.PendingRecords,
		PendingBytes:            queueStats.PendingBytes,
		OldestPendingCapturedAt: queueStats.OldestPendingCapturedAt,
		InFlightRecords:         queueStats.InFlightRecords,
		DroppedRecords:          queueStats.DroppedRecords,
		GapRecords:              queueStats.GapRecords,
		Replaying:               queueStats.Replaying,
		LastAckAt:               queueStats.LastAckAt,
	}
}

// Drain stops accepting records, waits (bounded by ctx) for the queue to be
// acknowledged, then closes the connection and waits for Run to return.
//
// Records still queued when ctx expires are dropped by design: the RAM queue
// does not survive shutdown. The hub reports the resulting hole as normal
// missing samples; the collector does not fabricate a gap for an interval it
// could not deliver.
func (c *Client) Drain(ctx context.Context) error {
	c.draining.Store(true)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		stats := c.queue.status()
		if stats.PendingRecords == 0 && stats.InFlightRecords == 0 {
			break
		}
		select {
		case <-ctx.Done():
			c.Close()
			return fmt.Errorf("delivery: drain incomplete: %w", ctx.Err())
		case <-c.runDone:
			c.Close()
			return errors.New("delivery: drain incomplete: delivery stopped before the queue emptied")
		case <-ticker.C:
		}
	}
	c.Close()
	if !c.started.Load() {
		return nil
	}
	select {
	case <-c.runDone:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("delivery: drain did not finish: %w", ctx.Err())
	}
}

// Close stops the client without draining. Pending records are discarded.
func (c *Client) Close() {
	c.stopOnce.Do(func() {
		c.closed.Store(true)
		close(c.stop)
	})
}

func (c *Client) stopped() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

func (c *Client) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Client) setSessionState(connected, welcomed bool, lastError string, attempts int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = connected
	c.welcomed = welcomed
	c.lastError = lastError
	c.attempts = attempts
}

func (c *Client) noteReconnect() {
	c.mu.Lock()
	c.reconnects++
	c.mu.Unlock()
}

// pauseReplayUntil records a hub retryAfterMs deadline. Live delivery is not
// throttled; only replay waits.
func (c *Client) pauseReplayUntil(deadline time.Time) {
	c.mu.Lock()
	if deadline.After(c.replayPausedUntil) {
		c.replayPausedUntil = deadline
	}
	c.mu.Unlock()
}

// replayPauseDeadline returns the current replay pause deadline. A new session
// inherits it so a reconnect cannot defeat the hub's backpressure.
func (c *Client) replayPauseDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replayPausedUntil
}

func (c *Client) inventorySnapshot() (protocol.Inventory, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inventory == nil {
		return protocol.Inventory{}, false
	}
	return *c.inventory, true
}

func printableASCII(value string, min, max int) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}
