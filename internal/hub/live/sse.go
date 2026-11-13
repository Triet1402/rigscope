package live

import (
	"crypto/rand"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/server"
)

const (
	// MaxLiveClients bounds concurrent SSE viewers.
	MaxLiveClients = 64
	// LiveKeepaliveInterval is the comment cadence that keeps intermediaries
	// from timing out an idle stream.
	LiveKeepaliveInterval = 15 * time.Second
	// LiveCoalesceInterval is the update cadence: at most one full snapshot is
	// written per window, and changes between windows are represented by the
	// next snapshot. The window is anchored at subscription so independent hubs
	// streaming the same collector stay in step.
	LiveCoalesceInterval = 500 * time.Millisecond
	// LiveWriteTimeout bounds one frame write; a viewer that cannot drain a
	// full snapshot within it is disconnected and must resynchronize.
	LiveWriteTimeout = 30 * time.Second
)

// FeedOptions configures the GET /api/live handler.
type FeedOptions struct {
	Auth  Authenticator
	State *State

	MaxClients int
	Keepalive  time.Duration
	// Coalesce is how long state changes are batched before one full snapshot.
	// Zero or negative selects LiveCoalesceInterval (500 ms).
	Coalesce time.Duration
	// DisableCoalescing delivers every change immediately instead of batching.
	// It is an explicit opt-out for tests; production keeps the default.
	DisableCoalescing bool
	// Shutdown, when non-nil, ends every stream when closed (hub shutdown).
	Shutdown <-chan struct{}
	// CheckOrigin, when non-nil, must return true for a request that carries
	// an Origin header. A false result is 403 origin not allowed.
	CheckOrigin func(*http.Request) bool
}

// Feed serves the Server-Sent Events stream. It never replays: every
// connection starts with a full snapshot, and a reconnect that sends
// Last-Event-ID starts with a full reset snapshot.
type Feed struct {
	opts    FeedOptions
	clients atomic.Int64
	mu      sync.Mutex
	done    chan struct{}
	closed  bool

	// testHookAfterSnapshot, when non-nil, runs after the initial snapshot is
	// serialized and before it is written. It exists so the regression test can
	// change state inside that window and prove the change subscription is
	// already installed.
	testHookAfterSnapshot func()
}

// NewFeed creates the live feed handler.
func NewFeed(opts FeedOptions) *Feed {
	if opts.MaxClients <= 0 {
		opts.MaxClients = MaxLiveClients
	}
	if opts.Keepalive <= 0 {
		opts.Keepalive = LiveKeepaliveInterval
	}
	if opts.DisableCoalescing {
		opts.Coalesce = 0
	} else if opts.Coalesce <= 0 {
		opts.Coalesce = LiveCoalesceInterval
	}
	return &Feed{opts: opts, done: make(chan struct{})}
}

// ClientCount reports the number of open streams.
func (f *Feed) ClientCount() int { return int(f.clients.Load()) }

// Stop ends the feed and every open stream. It is safe to call more than once.
func (f *Feed) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	close(f.done)
}

func (f *Feed) stopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// ServeHTTP implements GET /api/live.
func (f *Feed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Pre-checks run in the documented order.
	if f.opts.CheckOrigin != nil && r.Header.Get("Origin") != "" && !f.opts.CheckOrigin(r) {
		writeError(w, http.StatusForbidden, "origin not allowed")
		return
	}
	session, ok := authenticate(w, f.opts.Auth, r)
	if !ok {
		return
	}
	if session.Scope.Empty() {
		writeError(w, http.StatusForbidden, "no machine access is granted to this account")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("method not allowed"))
		return
	}
	if f.clients.Load() >= int64(f.opts.MaxClients) {
		writeError(w, http.StatusServiceUnavailable, "too many live clients")
		return
	}
	if f.stopped() {
		writeError(w, http.StatusServiceUnavailable, "hub is shutting down")
		return
	}
	// Reserve a client slot atomically so concurrent requests cannot exceed the
	// documented bound.
	for {
		current := f.clients.Load()
		if current >= int64(f.opts.MaxClients) {
			writeError(w, http.StatusServiceUnavailable, "too many live clients")
			return
		}
		if f.clients.CompareAndSwap(current, current+1) {
			break
		}
	}
	defer f.clients.Add(-1)

	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	// StreamWriter renews the write deadline before every event and flush, so a
	// client that stops reading fails the next write instead of pinning the
	// handler. A flush error is fatal: the viewer is no longer receiving frames.
	stream := server.NewStreamWriter(w)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	stream.WriteHeader(http.StatusOK)

	event := api.LiveEventSnapshot
	if r.Header.Get("Last-Event-ID") != "" {
		event = api.LiveEventReset
	}
	cursor := newCursor()
	viewer := &sseViewer{
		feed:    f,
		writer:  stream,
		session: session,
	}
	// Subscribe before reading the initial snapshot. A change that lands while
	// the snapshot is being built is then queued on this viewer's channel, so
	// the update cannot be lost between snapshot and subscription.
	changes, unsubscribe := f.opts.State.SubscribeChanges()
	defer unsubscribe()

	initial, err := viewer.snapshot(event, cursor)
	if err != nil {
		return
	}
	if f.testHookAfterSnapshot != nil {
		f.testHookAfterSnapshot()
	}
	if err := viewer.write(initial); err != nil {
		return
	}

	keepalive := time.NewTicker(f.opts.Keepalive)
	defer keepalive.Stop()

	// Coalescing runs on a fixed cadence anchored at subscription, not at the
	// first change. Changes arriving between ticks are represented by the next
	// full snapshot, so at most one update per window is written and two hubs
	// streaming the same collector cannot drift into different windows because
	// of an unrelated earlier state change.
	var coalesce *time.Ticker
	var coalesceC <-chan time.Time
	if f.opts.Coalesce > 0 {
		coalesce = time.NewTicker(f.opts.Coalesce)
		coalesceC = coalesce.C
		defer coalesce.Stop()
	}
	dirty := false

	for {
		select {
		case <-r.Context().Done():
			return
		case <-f.done:
			return
		case <-session.Revoked:
			return
		case <-keepalive.C:
			if err := viewer.write([]byte(": keepalive " + itoa(f.nowMilli()) + "\n\n")); err != nil {
				return
			}
		case <-changes:
			if coalesce == nil {
				frame, err := viewer.snapshot(api.LiveEventUpdate, cursor)
				if err != nil || viewer.write(frame) != nil {
					return
				}
				continue
			}
			dirty = true
		case <-coalesceC:
			if !dirty {
				continue
			}
			dirty = false
			frame, err := viewer.snapshot(api.LiveEventUpdate, cursor)
			if err != nil || viewer.write(frame) != nil {
				return
			}
		}
	}
}

func (f *Feed) nowMilli() int64 {
	return time.Now().UnixMilli()
}

type sseViewer struct {
	feed     *Feed
	writer   *server.StreamWriter
	session  Session
	lastData string
}

// snapshot serializes one scoped full snapshot and advances the viewer cursor.
// Identical consecutive snapshots are skipped by returning nil bytes.
func (v *sseViewer) snapshot(event api.LiveEvent, cursor *big.Int) ([]byte, error) {
	snapshot := v.feed.opts.State.Snapshot()
	scoped := filterSnapshot(snapshot, v.session.Scope)
	data, err := api.EncodeHubSnapshot(scoped)
	if err != nil {
		return nil, err
	}
	if string(data) == v.lastData {
		return nil, nil
	}
	v.lastData = string(data)
	frame := api.LiveFrame{
		ID:    api.LiveEventID{HubRunID: snapshot.HubRunID, Cursor: cursor.String()},
		Event: event,
		Data:  scoped,
	}
	encoded, err := api.EncodeLiveFrame(frame)
	if err != nil {
		return nil, err
	}
	cursor.Add(cursor, big.NewInt(1))
	return encoded, nil
}

func (v *sseViewer) write(frame []byte) error {
	if len(frame) == 0 {
		return nil
	}
	if _, err := v.writer.Write(frame); err != nil {
		return err
	}
	return v.writer.FlushError()
}

// filterSnapshot applies the machine scope before serialization so an excluded
// row never enters the browser's bytes.
func filterSnapshot(snapshot api.HubSnapshot, scope api.MachineScope) api.HubSnapshot {
	if scope.All {
		return snapshot
	}
	machines := make([]api.MachineState, 0, len(snapshot.Machines))
	for _, machine := range snapshot.Machines {
		if scope.Includes(machine.MachineID) {
			machines = append(machines, machine)
		}
	}
	return api.HubSnapshot{HubRunID: snapshot.HubRunID, Machines: machines}
}

// newCursor returns a random 128-bit decimal starting value. The cursor is
// opaque and not comparable across viewers or hub runs.
func newCursor() *big.Int {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	value := new(big.Int).SetBytes(raw[:])
	if value.Sign() == 0 {
		value.SetInt64(1)
	}
	return value
}

// itoa renders a decimal integer without another import.
func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
