package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
	"github.com/coder/websocket"
)

var errStopped = errors.New("delivery: stopped")

// dialFailure carries the HTTP status of a rejected upgrade, which is how a
// revoked credential (401) is distinguished from a transient outage.
type dialFailure struct {
	err    error
	status int
}

func (e *dialFailure) Error() string { return e.err.Error() }
func (e *dialFailure) Unwrap() error { return e.err }

func httpStatusOf(err error) int {
	var failure *dialFailure
	if errors.As(err, &failure) {
		return failure.status
	}
	return 0
}

// Run connects, delivers and reconnects until ctx is cancelled or Close is
// called. It may be called once.
func (c *Client) Run(ctx context.Context) error {
	if !c.started.CompareAndSwap(false, true) {
		return errors.New("delivery: Run may only be called once")
	}
	defer func() {
		final := publicQueueStats(c.queue.status())
		c.mu.Lock()
		c.connected = false
		c.welcomed = false
		c.finalQueue = &final
		c.mu.Unlock()
		c.queue.close()
		close(c.detail)
		c.runExited.Store(true)
		close(c.runDone)
	}()

	attempts := 0
	for {
		if c.stopped() || ctx.Err() != nil {
			return nil
		}
		started := time.Now()
		welcomed, err := c.runSession(ctx)
		if c.stopped() || ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = errors.New("connection closed")
		}
		healthy := welcomed && time.Since(started) >= healthyConnectionReset
		if healthy {
			attempts = 0
		} else {
			attempts++
		}
		c.mu.Lock()
		c.connected = false
		c.welcomed = false
		c.lastError = err.Error()
		c.attempts = attempts
		c.reconnects++
		reported := c.reportedRejectedCredentials
		c.mu.Unlock()

		if welcomed {
			if onDisconnect := c.cfg.OnDisconnect; onDisconnect != nil {
				onDisconnect()
			}
		}

		delay := reconnectDelay(attempts, c.cfg.Rand, c.cfg.BackoffBase, c.cfg.BackoffMax)
		if httpStatusOf(err) == http.StatusUnauthorized {
			if delay < credentialsRejectedRetry {
				delay = credentialsRejectedRetry
			}
			if !reported {
				c.mu.Lock()
				c.reportedRejectedCredentials = true
				c.mu.Unlock()
				c.cfg.Logf("%s", credentialsRejectedMessage(c.cfg.HubURL))
			}
		}
		c.cfg.Logf("delivery: disconnected (%v); reconnecting in %s (attempt %d)", err, delay, attempts)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-c.stop:
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// reconnectDelay is the jittered exponential schedule: base*2^attempts capped
// at max, then multiplied by a jitter factor in [0.7, 1.3].
func reconnectDelay(attempts int, random func() float64, base, max time.Duration) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	exponential := float64(base)
	for step := 0; step < attempts && exponential < float64(max); step++ {
		exponential *= 2
	}
	if exponential > float64(max) {
		exponential = float64(max)
	}
	jitter := 0.7 + 0.6*random()
	delay := time.Duration(exponential * jitter)
	if delay < time.Millisecond {
		delay = time.Millisecond
	}
	if delay > max {
		delay = max
	}
	return delay
}

func credentialsRejectedMessage(hubURL string) string {
	hub := "https://your-hub"
	if parsed, err := url.Parse(hubURL); err == nil {
		switch parsed.Scheme {
		case "ws":
			parsed.Scheme = "http"
		case "wss":
			parsed.Scheme = "https"
		}
		hub = parsed.Scheme + "://" + parsed.Host
	}
	return "delivery: hub rejected this collector's credential (401): it was revoked or removed, or the hub no longer knows it. " +
		"In the hub, open Administration > Collectors and create a grant: Re-enroll for a revoked machine, Add machine for a removed one. " +
		"Then run here: rigscope collector enroll --hub-url '" + hub + "' --grant '<grant>' (a running collector restarts by itself)"
}

type inFlightBatch struct {
	refs  []protocol.RecordRef
	keys  map[string]bool
	mode  protocol.SampleMode
	timer *time.Timer
}

type readItem struct {
	message  protocol.HubMessage
	parseErr error
	err      error
}

type pingResult struct{ err error }

// wsSession is one welcome-to-close connection. Only its owning goroutine
// mutates the fields; the read and ping goroutines communicate through
// channels.
type wsSession struct {
	client *Client
	conn   *websocket.Conn
	ctx    context.Context

	inFlight     map[uint64]*inFlightBatch
	nextBatchID  uint64
	timeoutCh    chan uint64
	replayCh     chan struct{}
	replayTimer  *time.Timer
	window       int
	replayTokens float64
	lastRefill   time.Time
	pausedUntil  time.Time
}

// runSession performs one connection's handshake and delivery loop. It
// returns welcomed=true only when the welcome completed, which the retry
// policy uses to decide whether the connection was healthy.
func (c *Client) runSession(ctx context.Context) (bool, error) {
	dialCtx, cancelDial := context.WithTimeout(ctx, c.cfg.WelcomeTimeout)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.cfg.Credential)
	conn, response, err := c.dial(dialCtx, c.cfg.HubURL, &websocket.DialOptions{
		HTTPClient: c.httpClient,
		HTTPHeader: header,
	})
	cancelDial()
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
			if response.Body != nil {
				_ = response.Body.Close()
			}
		}
		return false, &dialFailure{err: fmt.Errorf("dial hub: %w", err), status: status}
	}
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	defer conn.Close(websocket.StatusNormalClosure, "collector stopping")
	conn.SetReadLimit(protocol.MaxDetailBytes)

	version := int64(protocol.Version)
	hello := protocol.HelloMessage{
		CollectorID:        c.cfg.Identity.CollectorID,
		RunID:              c.cfg.Identity.RunID,
		BootID:             c.cfg.Identity.BootID,
		Hostname:           c.cfg.Identity.Hostname,
		Capabilities:       append([]protocol.CapabilityName(nil), c.cfg.Capabilities...),
		ExpectedIntervalMS: int64(c.cfg.ExpectedInterval / time.Millisecond),
		Protocol:           &version,
	}
	helloData, err := protocol.EncodeCollectorMessage(hello)
	if err != nil {
		return false, fmt.Errorf("encode hello: %w", err)
	}
	if err := writeFrame(sessionCtx, conn, helloData); err != nil {
		return false, fmt.Errorf("send hello: %w", err)
	}

	readCh := make(chan readItem, 8)
	go c.readLoop(sessionCtx, conn, readCh)

	welcome, err := awaitWelcome(sessionCtx, c, readCh)
	if err != nil {
		return false, err
	}
	window := int(welcome.MaxInFlightBatches)
	if window > c.cfg.MaxLocalInFlightBatches {
		window = c.cfg.MaxLocalInFlightBatches
	}
	if window < 1 {
		window = 1
	}

	session := &wsSession{
		client:       c,
		conn:         conn,
		ctx:          sessionCtx,
		inFlight:     make(map[uint64]*inFlightBatch),
		timeoutCh:    make(chan uint64, protocol.MaxInFlightBatches),
		replayCh:     make(chan struct{}, 1),
		window:       window,
		replayTokens: 1,
		lastRefill:   time.Now(),
		// A retryAfterMs from the previous connection still applies: the hub
		// asked for time, not for a different connection.
		pausedUntil: c.replayPauseDeadline(),
	}
	defer session.finish()

	c.mu.Lock()
	c.connected = true
	c.welcomed = true
	c.attempts = 0
	c.lastError = ""
	c.mu.Unlock()
	c.cfg.Logf("delivery: welcomed (hubRunId=%s machineId=%s maxInFlightBatches=%d)",
		welcome.HubRunID, welcome.MachineID, welcome.MaxInFlightBatches)
	if c.cfg.OnWelcome != nil {
		c.cfg.OnWelcome()
	}

	pingCh := make(chan pingResult, 1)
	go c.pingLoop(sessionCtx, conn, pingCh)

	if inventory, ok := c.inventorySnapshot(); ok {
		if err := session.sendControlNow(protocol.InventoryMessage{
			CapturedAt: formatTimestamp(c.cfg.Clock()),
			Inventory:  inventory,
		}); err != nil {
			return true, err
		}
	}
	if err := session.pump(); err != nil {
		return true, err
	}

	idleTimeout := c.cfg.IdleTimeout
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()
	limits := time.NewTicker(10 * time.Second)
	defer limits.Stop()

	for {
		select {
		case <-sessionCtx.Done():
			return true, errStopped
		case <-c.stop:
			return true, errStopped
		case <-c.wake:
			if err := session.pump(); err != nil {
				return true, err
			}
		case data := <-c.control:
			if err := session.write(data); err != nil {
				return true, err
			}
		case item, ok := <-readCh:
			if !ok {
				return true, errors.New("hub connection closed")
			}
			if item.err != nil {
				return true, fmt.Errorf("read hub message: %w", item.err)
			}
			if item.parseErr != nil {
				// A malformed hub message is logged and ignored; it is not a
				// reason to discard the session.
				c.cfg.Logf("delivery: ignoring invalid hub message: %v", item.parseErr)
				resetTimer(idle, idleTimeout)
				continue
			}
			resetTimer(idle, idleTimeout)
			if err := session.handleHubMessage(item.message); err != nil {
				return true, err
			}
		case batchID := <-session.timeoutCh:
			if err := session.handleAckTimeout(batchID); err != nil {
				return true, err
			}
		case <-session.replayCh:
			if err := session.pumpReplay(); err != nil {
				return true, err
			}
		case result := <-pingCh:
			if result.err != nil {
				return true, fmt.Errorf("hub heartbeat: %w", result.err)
			}
			resetTimer(idle, idleTimeout)
		case <-idle.C:
			return true, errors.New("hub heartbeat timed out")
		case <-limits.C:
			c.queue.enforceLimits()
			if err := session.pump(); err != nil {
				return true, err
			}
		}
	}
}

func awaitWelcome(ctx context.Context, c *Client, readCh <-chan readItem) (protocol.WelcomeMessage, error) {
	timer := time.NewTimer(c.cfg.WelcomeTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return protocol.WelcomeMessage{}, ctx.Err()
	case <-c.stop:
		return protocol.WelcomeMessage{}, errStopped
	case <-timer.C:
		return protocol.WelcomeMessage{}, errors.New("welcome timed out")
	case item, ok := <-readCh:
		if !ok {
			return protocol.WelcomeMessage{}, errors.New("hub connection closed before welcome")
		}
		if item.err != nil {
			return protocol.WelcomeMessage{}, fmt.Errorf("read welcome: %w", item.err)
		}
		if item.parseErr != nil {
			return protocol.WelcomeMessage{}, fmt.Errorf("rejected welcome: %w", item.parseErr)
		}
		welcome, ok := item.message.(protocol.WelcomeMessage)
		if !ok {
			return protocol.WelcomeMessage{}, fmt.Errorf("first message is not a welcome (%T)", item.message)
		}
		if welcome.Protocol != nil && *welcome.Protocol != protocol.Version {
			c.cfg.Logf("delivery: hub selected protocol version %d; this collector supports %d. Upgrade the collector.",
				*welcome.Protocol, protocol.Version)
			return protocol.WelcomeMessage{}, fmt.Errorf("hub selected unsupported protocol version %d", *welcome.Protocol)
		}
		return welcome, nil
	}
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn, output chan<- readItem) {
	defer close(output)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			select {
			case output <- readItem{err: err}:
			case <-ctx.Done():
			}
			return
		}
		message, parseErr := protocol.DecodeHubMessage(data)
		select {
		case output <- readItem{message: message, parseErr: parseErr}:
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) pingLoop(ctx context.Context, conn *websocket.Conn, output chan<- pingResult) {
	ticker := time.NewTicker(c.cfg.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timeout := c.cfg.IdleTimeout / 3
			if timeout > 10*time.Second {
				timeout = 10 * time.Second
			}
			pingCtx, cancel := context.WithTimeout(ctx, timeout)
			err := conn.Ping(pingCtx)
			cancel()
			select {
			case output <- pingResult{err: err}:
			case <-ctx.Done():
			}
			if err != nil {
				return
			}
		}
	}
}

func (s *wsSession) finish() {
	for _, batch := range s.inFlight {
		batch.timer.Stop()
	}
	s.inFlight = make(map[uint64]*inFlightBatch)
	if s.replayTimer != nil {
		s.replayTimer.Stop()
		s.replayTimer = nil
	}
	s.client.queue.releaseAll()
}

func (s *wsSession) handleHubMessage(message protocol.HubMessage) error {
	switch typed := message.(type) {
	case protocol.AckMessage:
		return s.handleAck(typed)
	case protocol.DetailSubscribe:
		request := typed
		select {
		case s.client.detail <- DetailRequest{Subscribe: &request}:
		default:
			s.client.cfg.Logf("delivery: dropping detail.subscribe %s (caller is not keeping up)", request.SubscriptionID)
		}
		return nil
	case protocol.DetailCancel:
		request := typed
		select {
		case s.client.detail <- DetailRequest{Cancel: &request}:
		default:
			s.client.cfg.Logf("delivery: dropping detail.cancel %s (caller is not keeping up)", request.SubscriptionID)
		}
		return nil
	case protocol.WelcomeMessage:
		s.client.cfg.Logf("delivery: ignoring duplicate welcome")
		return nil
	default:
		return nil
	}
}

func (s *wsSession) write(data []byte) error {
	if err := writeFrame(s.ctx, s.conn, data); err != nil {
		return fmt.Errorf("send to hub: %w", err)
	}
	return nil
}

func writeFrame(ctx context.Context, conn *websocket.Conn, data []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}

func (s *wsSession) sendControlNow(message protocol.CollectorMessage) error {
	data, err := protocol.EncodeCollectorMessage(message)
	if err != nil {
		s.client.cfg.Logf("delivery: dropping invalid %T: %v", message, err)
		return nil
	}
	return s.write(data)
}

// liveReservation reserves flush-window slots so replay cannot starve fresh
// delivery. Legacy or explicitly small windows keep one slot.
func (s *wsSession) liveReservation() int {
	if s.window <= 1 {
		return 0
	}
	if s.window < protocol.MaxInFlightBatches {
		return 1
	}
	liveSlots := int((protocol.MaxPersistentFlushMS+int64(s.client.cfg.ExpectedInterval/time.Millisecond)-1)/
		int64(s.client.cfg.ExpectedInterval/time.Millisecond)) + 2
	if liveSlots > s.window-1 {
		liveSlots = s.window - 1
	}
	return liveSlots
}

func (s *wsSession) replayCapacity() int {
	capacity := s.window - s.liveReservation()
	if capacity < 0 {
		return 0
	}
	return capacity
}

func (s *wsSession) pump() error {
	if err := s.sendLive(); err != nil {
		return err
	}
	return s.pumpReplay()
}

func (s *wsSession) sendLive() error {
	if len(s.inFlight) >= s.window {
		return nil
	}
	reserved := s.client.queue.takeLive()
	if reserved == nil {
		return nil
	}
	return s.sendBatch(protocol.SampleModeLive, reserved)
}

func (s *wsSession) pumpReplay() error {
	now := time.Now()
	if now.Before(s.pausedUntil) {
		s.scheduleReplay(s.pausedUntil.Sub(now))
		return nil
	}
	s.refillTokens(now)
	capacity := s.replayCapacity()
	guard := 0
	for len(s.inFlight) < capacity && s.replayTokens >= 1 && guard < protocol.MaxBatchRecords*8 {
		guard++
		reserved := s.client.queue.takeReplay(
			protocol.MaxBatchRecords,
			maxInt(1, s.client.cfg.MaxBatchBytes-batchEnvelopeBytes),
		)
		if reserved == nil {
			return nil
		}
		if err := s.sendBatch(protocol.SampleModeReplay, reserved); err != nil {
			return err
		}
		s.replayTokens--
	}
	if s.replayTokens < 1 && len(s.inFlight) < capacity && s.client.queue.hasPending() {
		wait := time.Duration((1 - s.replayTokens) / s.client.cfg.ReplayBatchesPerSecond * float64(time.Second))
		s.scheduleReplay(wait)
	}
	return nil
}

func (s *wsSession) refillTokens(now time.Time) {
	if !now.After(s.lastRefill) {
		return
	}
	elapsed := now.Sub(s.lastRefill).Seconds()
	s.replayTokens += elapsed * s.client.cfg.ReplayBatchesPerSecond
	if s.replayTokens > 1 {
		s.replayTokens = 1
	}
	s.lastRefill = now
}

func (s *wsSession) scheduleReplay(delay time.Duration) {
	if s.ctx.Err() != nil || s.client.stopped() {
		return
	}
	if delay < time.Millisecond {
		delay = time.Millisecond
	}
	if s.replayTimer != nil {
		s.replayTimer.Stop()
	}
	s.replayTimer = time.AfterFunc(delay, func() {
		select {
		case s.replayCh <- struct{}{}:
		default:
		}
	})
}

func (s *wsSession) sendBatch(mode protocol.SampleMode, reserved *reservation) error {
	records := reserved.records
	message := protocol.SamplesMessage{Mode: mode, Records: records}
	data, err := protocol.EncodeCollectorMessage(message)
	if err != nil {
		// A queue record that cannot be encoded can never succeed: drop it
		// rather than stalling every later record forever.
		refs := refsOf(records)
		s.client.cfg.Logf("delivery: dropping unencodable batch: %v", err)
		s.client.queue.acknowledge(nil, refs)
		return nil
	}
	for len(data) > s.client.cfg.MaxBatchBytes && len(records) > 1 {
		// Trimmed entries were reserved atomically with the selection; return
		// them to the pending set so a later, smaller batch retries them.
		trimmed := reserved.entries[len(records)-1]
		records = records[:len(records)-1]
		reserved.entries = reserved.entries[:len(reserved.entries)-1]
		s.client.queue.releaseEntries([]*entry{trimmed})
		message.Records = records
		data, err = protocol.EncodeCollectorMessage(message)
		if err != nil {
			refs := refsOf(records)
			s.client.cfg.Logf("delivery: dropping unencodable batch: %v", err)
			s.client.queue.acknowledge(nil, refs)
			return nil
		}
	}
	refs := refsOf(records)
	if len(data) > s.client.cfg.MaxBatchBytes {
		s.client.cfg.Logf("delivery: dropping unsendable record %s#%d (%d bytes > %d)",
			refs[0].RunID, refs[0].Sequence, len(data), s.client.cfg.MaxBatchBytes)
		// The record is locally unsendable, not acknowledged. Account for it
		// as a known drop and emit a gap covering its capture interval.
		s.client.queue.dropUnsendable(refs[0])
		return nil
	}

	s.nextBatchID++
	batchID := s.nextBatchID
	batch := &inFlightBatch{
		refs: refs,
		keys: make(map[string]bool, len(refs)),
		mode: mode,
	}
	for _, ref := range refs {
		batch.keys[refKey(ref.RunID, ref.Sequence)] = true
	}
	batch.timer = time.AfterFunc(s.client.cfg.AckTimeout, func() {
		select {
		case s.timeoutCh <- batchID:
		default:
		}
	})
	s.inFlight[batchID] = batch
	if err := s.write(data); err != nil {
		batch.timer.Stop()
		delete(s.inFlight, batchID)
		s.client.queue.release(refs)
		return err
	}
	return nil
}

func (s *wsSession) handleAckTimeout(batchID uint64) error {
	batch, ok := s.inFlight[batchID]
	if !ok {
		return nil
	}
	batch.timer.Stop()
	delete(s.inFlight, batchID)
	s.client.queue.release(batch.refs)
	s.client.cfg.Logf("delivery: ack timed out for %d record(s); will resend", len(batch.refs))
	return s.pump()
}

func (s *wsSession) handleAck(ack protocol.AckMessage) error {
	rejected := make([]protocol.RecordRef, 0, len(ack.Rejected))
	for _, record := range ack.Rejected {
		rejected = append(rejected, record.RecordRef)
	}
	s.client.queue.acknowledge(ack.Accepted, rejected)
	if len(ack.Rejected) > 0 {
		s.client.cfg.Logf("delivery: hub rejected %d record(s): %s",
			len(ack.Rejected), ack.Rejected[0].Reason)
	}

	ackedKeys := make(map[string]bool, len(ack.Accepted)+len(rejected))
	for _, ref := range append(append([]protocol.RecordRef{}, ack.Accepted...), rejected...) {
		ackedKeys[refKey(ref.RunID, ref.Sequence)] = true
	}
	var touched []uint64
	for batchID, batch := range s.inFlight {
		intersects := false
		for key := range ackedKeys {
			if batch.keys[key] {
				delete(batch.keys, key)
				intersects = true
			}
		}
		if intersects {
			touched = append(touched, batchID)
		}
		if len(batch.keys) == 0 {
			batch.timer.Stop()
			delete(s.inFlight, batchID)
		}
	}

	if ack.RetryAfterMS != nil {
		delay := time.Duration(*ack.RetryAfterMS) * time.Millisecond
		pausedUntil := time.Now().Add(delay)
		if pausedUntil.After(s.pausedUntil) {
			s.pausedUntil = pausedUntil
		}
		// Keep the deadline on the client: a reconnect during the pause must
		// not start replay early.
		s.client.pauseReplayUntil(pausedUntil)
		var release []uint64
		switch {
		case len(touched) > 0:
			release = touched
		case len(ackedKeys) == 0:
			for batchID, batch := range s.inFlight {
				if batch.mode == protocol.SampleModeReplay {
					release = append(release, batchID)
				}
			}
		}
		for _, batchID := range release {
			batch, ok := s.inFlight[batchID]
			if !ok {
				continue
			}
			batch.timer.Stop()
			delete(s.inFlight, batchID)
			refs := make([]protocol.RecordRef, 0, len(batch.refs))
			for _, ref := range batch.refs {
				if batch.keys[refKey(ref.RunID, ref.Sequence)] {
					refs = append(refs, ref)
				}
			}
			s.client.queue.release(refs)
		}
		s.client.cfg.Logf("delivery: hub asked to retry after %s; replay paused", delay)
	}

	return s.pump()
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
