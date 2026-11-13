package live

import (
	"bufio"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/server"
	"github.com/Triet1402/rigscope/internal/protocol"
)

type fixedAuth struct{ session Session }

func (f fixedAuth) Authenticate(*http.Request) (Session, error) { return f.session, nil }

func internalState(t *testing.T) (*State, SessionInfo) {
	t.Helper()
	state := NewState("hub-run-1", nil)
	if err := state.RegisterMachine("c-1", "m-1"); err != nil {
		t.Fatalf("register machine: %v", err)
	}
	result, err := state.StartSession(StartSessionInput{
		SessionID:          "collector-session-1",
		CollectorID:        "c-1",
		RunID:              "run-1",
		BootID:             "boot-1",
		Hostname:           "rig-1",
		ExpectedIntervalMS: 1000,
	})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	return state, result.Session
}

func internalRecord(sequence int64, capturedAt time.Time) protocol.MetricsRecord {
	return protocol.MetricsRecord{
		RecordEnvelope: protocol.RecordEnvelope{
			CollectorID: "c-1",
			RunID:       "run-1",
			BootID:      "boot-1",
			Sequence:    sequence,
			CapturedAt:  capturedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		},
		Payload: protocol.MetricsPayload{
			CPU: protocol.CpuAvailable{
				UsagePercent: 10,
				PerCore:      []*float64{},
			},
			Memory:  protocol.MemoryAvailable{UsedBytes: 1, TotalBytes: 2},
			Swap:    protocol.BytesUsageAvailable{UsedBytes: 0, TotalBytes: 0},
			GPUs:    protocol.GpusAvailable{Devices: []protocol.GpuReading{}},
			Disks:   protocol.DisksAvailable{Filesystems: []protocol.FilesystemReading{}, Devices: []protocol.DiskIoReading{}},
			Network: protocol.NetworkAvailable{Interfaces: []protocol.InterfaceReading{}},
		},
	}
}

func readLiveFrame(t *testing.T, reader *bufio.Reader) api.LiveFrame {
	t.Helper()
	for {
		block := readLiveBlock(t, reader)
		frame, ok, err := api.DecodeLiveFrame([]byte(block))
		if err != nil {
			t.Fatalf("decode SSE frame %q: %v", block, err)
		}
		if ok {
			return frame
		}
	}
}

func readLiveBlock(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var block strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE block: %v", err)
		}
		if line == "\n" {
			return block.String()
		}
		block.WriteString(line)
	}
}

// TestFeedSubscribesBeforeInitialSnapshot proves a state change that lands
// after the initial snapshot is serialized cannot be lost. The test hook mutates
// state in exactly that window; because the change subscription is installed
// first, the mutation still produces an update frame.
func TestFeedSubscribesBeforeInitialSnapshot(t *testing.T) {
	state, info := internalState(t)
	session := Session{ID: "browser-1", Username: "demo", Scope: api.MachineScope{All: true}}
	feed := NewFeed(FeedOptions{Auth: fixedAuth{session: session}, State: state, DisableCoalescing: true})
	mutated := false
	feed.testHookAfterSnapshot = func() {
		if mutated {
			return
		}
		mutated = true
		state.ApplyMetrics(info.SessionID, internalRecord(1, time.Now()), time.Now())
	}
	server := httptest.NewServer(feed)
	t.Cleanup(server.Close)
	t.Cleanup(feed.Stop)

	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)

	initial := readLiveFrame(t, reader)
	if initial.Event != api.LiveEventSnapshot {
		t.Fatalf("first event = %q", initial.Event)
	}
	if initial.Data.Machines[0].Metrics != nil {
		t.Fatalf("initial snapshot unexpectedly carried the later change")
	}

	// If the subscription were installed after the snapshot, this update would
	// be lost until an unrelated mutation arrived.
	update := readLiveFrame(t, reader)
	if update.Event != api.LiveEventUpdate {
		t.Fatalf("second event = %q", update.Event)
	}
	if update.Data.Machines[0].Metrics == nil || update.Data.Machines[0].Metrics.Sequence != 1 {
		t.Fatalf("change made during the initial snapshot was not delivered")
	}
}

// TestFeedCoalescingDefaults checks the documented 500 ms default and the
// explicit test opt-out.
func TestFeedCoalescingDefaults(t *testing.T) {
	state, _ := internalState(t)
	session := Session{ID: "browser-1", Scope: api.MachineScope{All: true}}
	defaults := NewFeed(FeedOptions{Auth: fixedAuth{session: session}, State: state})
	t.Cleanup(defaults.Stop)
	if defaults.opts.Coalesce != LiveCoalesceInterval {
		t.Fatalf("default coalesce = %v, want %v", defaults.opts.Coalesce, LiveCoalesceInterval)
	}
	disabled := NewFeed(FeedOptions{Auth: fixedAuth{session: session}, State: state, DisableCoalescing: true})
	t.Cleanup(disabled.Stop)
	if disabled.opts.Coalesce != 0 {
		t.Fatalf("disabled coalesce = %v, want 0", disabled.opts.Coalesce)
	}
	explicit := NewFeed(FeedOptions{Auth: fixedAuth{session: session}, State: state, Coalesce: 7 * time.Millisecond})
	t.Cleanup(explicit.Stop)
	if explicit.opts.Coalesce != 7*time.Millisecond {
		t.Fatalf("explicit coalesce = %v", explicit.opts.Coalesce)
	}
}

// TestStateRemovalPublishesOverSSE proves a revocation-style removal reaches
// subscribers as an update carrying the machine-free snapshot.
func TestStateRemovalPublishesOverSSE(t *testing.T) {
	state, _ := internalState(t)
	session := Session{ID: "browser-1", Scope: api.MachineScope{All: true}}
	feed := NewFeed(FeedOptions{Auth: fixedAuth{session: session}, State: state, DisableCoalescing: true})
	server := httptest.NewServer(feed)
	t.Cleanup(server.Close)
	t.Cleanup(feed.Stop)

	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	initial := readLiveFrame(t, reader)
	if len(initial.Data.Machines) != 1 {
		t.Fatalf("initial machines = %+v", initial.Data.Machines)
	}

	if _, removed := state.RemoveCollector("c-1"); !removed {
		t.Fatalf("collector was not removed")
	}
	update := readLiveFrame(t, reader)
	if update.Event != api.LiveEventUpdate || len(update.Data.Machines) != 0 {
		t.Fatalf("removal frame = %+v", update)
	}
}

type failingWriter struct {
	header http.Header
}

func (w *failingWriter) Header() http.Header { return w.header }
func (w *failingWriter) WriteHeader(int)     {}
func (w *failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

// TestViewerWritePropagatesErrors ensures a failed write or flush ends the
// stream instead of being swallowed.
func TestViewerWritePropagatesErrors(t *testing.T) {
	state, _ := internalState(t)
	session := Session{ID: "browser-1", Scope: api.MachineScope{All: true}}
	writer := &failingWriter{header: make(http.Header)}
	viewer := &sseViewer{feed: &Feed{opts: FeedOptions{State: state}}, writer: server.NewStreamWriter(writer), session: session}
	if err := viewer.write([]byte("data: x\n\n")); err == nil {
		t.Fatalf("a failed write must be reported")
	}

	// A writer that cannot flush reports the flush error.
	recorder := httptest.NewRecorder()
	plain := &nonFlushingWriter{header: recorder.Header()}
	viewer = &sseViewer{feed: &Feed{opts: FeedOptions{State: state}}, writer: server.NewStreamWriter(plain), session: session}
	if err := viewer.write([]byte("data: x\n\n")); err == nil {
		t.Fatalf("an unsupported flush must be reported")
	}
}

type nonFlushingWriter struct {
	header http.Header
}

func (w *nonFlushingWriter) Header() http.Header            { return w.header }
func (w *nonFlushingWriter) WriteHeader(int)                {}
func (w *nonFlushingWriter) Write(data []byte) (int, error) { return len(data), nil }
