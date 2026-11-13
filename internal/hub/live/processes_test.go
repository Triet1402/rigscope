package live_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/protocol"
)

type recordingSender struct {
	mu       sync.Mutex
	messages []protocol.HubMessage
	sent     chan protocol.HubMessage
	allow    bool
}

func newRecordingSender() *recordingSender {
	return &recordingSender{sent: make(chan protocol.HubMessage, 64), allow: true}
}

func (r *recordingSender) SendDetail(_ string, message protocol.HubMessage) bool {
	r.mu.Lock()
	r.messages = append(r.messages, message)
	allow := r.allow
	r.mu.Unlock()
	if !allow {
		return false
	}
	select {
	case r.sent <- message:
	default:
	}
	return true
}

func (r *recordingSender) last() protocol.HubMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.messages) == 0 {
		return nil
	}
	return r.messages[len(r.messages)-1]
}

func (r *recordingSender) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messages)
}

func processHub(t *testing.T, options live.ProcessOptions) (*live.State, *recordingSender, live.Session, *httptest.Server) {
	t.Helper()
	state, info := newState(t)
	session, _ := newSession()
	inventory := protocol.Inventory{
		Capabilities: protocol.Capabilities{
			CPU:       protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Memory:    protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Swap:      protocol.CapabilityState{Status: protocol.CapabilityUnsupported},
			GPUs:      protocol.CapabilityState{Status: protocol.CapabilityUnsupported},
			Disks:     protocol.CapabilityState{Status: protocol.CapabilitySupported},
			Network:   protocol.CapabilityState{Status: protocol.CapabilityUnsupported},
			Processes: protocol.CapabilityState{Status: protocol.CapabilitySupported},
		},
	}
	state.SetInventory(info.MachineID, inventory)
	sender := newRecordingSender()
	options.Auth = &fakeAuth{session: session}
	options.State = state
	options.Sender = sender
	if options.RenewEvery == 0 {
		options.RenewEvery = 20 * time.Millisecond
	}
	if options.SweepEvery == 0 {
		options.SweepEvery = 10 * time.Millisecond
	}
	interests := live.NewProcessInterests(options)
	server := httptest.NewServer(interests)
	t.Cleanup(server.Close)
	t.Cleanup(interests.Close)
	return state, sender, session, server
}

func call(t *testing.T, method, url string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response, buffer.Bytes()
}

func TestProcessInterestsLifecycle(t *testing.T) {
	state, sender, _, server := processHub(t, live.ProcessOptions{})
	machineID := "m-1"

	response, body := call(t, http.MethodPost, server.URL+"?machineId="+machineID+"&viewerId=viewer-1")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("acquire status = %d: %s", response.StatusCode, body)
	}
	terms, err := api.DecodeProcessAcquireResponse(body)
	if err != nil {
		t.Fatalf("decode acquire: %v", err)
	}
	if terms.LeaseMs != live.DefaultViewerLease.Milliseconds() || terms.IntervalMs != 1000 {
		t.Fatalf("lease terms = %+v", terms)
	}
	subscribe, ok := sender.last().(protocol.DetailSubscribe)
	if !ok {
		t.Fatalf("first collector message = %T", sender.last())
	}
	if subscribe.Kind != protocol.DetailKindProcesses || subscribe.IntervalMS != 1000 || subscribe.LeaseMS != live.DefaultCollectorLease.Milliseconds() {
		t.Fatalf("subscribe = %+v", subscribe)
	}

	// Before a result the viewer is loading with a null result.
	response, body = call(t, http.MethodGet, server.URL+"?machineId="+machineID+"&viewerId=viewer-1")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("read status = %d: %s", response.StatusCode, body)
	}
	read, err := api.DecodeProcessReadResponse(body)
	if err != nil {
		t.Fatalf("decode read: %v", err)
	}
	if read.State != "loading" || read.Result != nil {
		t.Fatalf("read = %+v", read)
	}

	// Re-acquiring keeps the same subscription and does not duplicate state.
	response, body = call(t, http.MethodPost, server.URL+"?machineId="+machineID+"&viewerId=viewer-1")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("renew status = %d: %s", response.StatusCode, body)
	}
	renewed, err := api.DecodeProcessAcquireResponse(body)
	if err != nil {
		t.Fatalf("decode renew: %v", err)
	}
	if renewed.LeaseMs != terms.LeaseMs {
		t.Fatalf("renewed terms = %+v", renewed)
	}

	// Deleting the only viewer cancels the collector lease.
	response, body = call(t, http.MethodDelete, server.URL+"?machineId="+machineID+"&viewerId=viewer-1")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", response.StatusCode, body)
	}
	sender.mu.Lock()
	var cancelled bool
	for _, message := range sender.messages {
		if cancel, ok := message.(protocol.DetailCancel); ok && cancel.SubscriptionID == subscribe.SubscriptionID {
			cancelled = true
		}
	}
	sender.mu.Unlock()
	if !cancelled {
		t.Fatalf("detail.cancel was not sent")
	}
	response, _ = call(t, http.MethodGet, server.URL+"?machineId="+machineID+"&viewerId=viewer-1")
	if response.StatusCode != http.StatusGone {
		t.Fatalf("expired read status = %d", response.StatusCode)
	}
	_ = state
}

// TestProcessInterestsResultAndRenewal exercises Receive and the lease renewal
// sweep with a handler the test owns.
func TestProcessInterestsResultAndRenewal(t *testing.T) {
	state, info := newState(t)
	state.SetInventory(info.MachineID, protocol.Inventory{
		Capabilities: protocol.Capabilities{Processes: protocol.CapabilityState{Status: protocol.CapabilitySupported}},
	})
	session, _ := newSession()
	sender := newRecordingSender()
	interests := live.NewProcessInterests(live.ProcessOptions{
		Auth:           &fakeAuth{session: session},
		State:          state,
		Sender:         sender,
		RenewEvery:     15 * time.Millisecond,
		SweepEvery:     5 * time.Millisecond,
		CollectorLease: 30 * time.Millisecond,
	})
	t.Cleanup(interests.Close)
	server := httptest.NewServer(interests)
	t.Cleanup(server.Close)

	if response, body := call(t, http.MethodPost, server.URL+"?machineId=m-1&viewerId=v-1"); response.StatusCode != http.StatusOK {
		t.Fatalf("acquire status = %d: %s", response.StatusCode, body)
	}
	subscribe, ok := sender.last().(protocol.DetailSubscribe)
	if !ok {
		t.Fatalf("no subscribe")
	}
	interests.Receive("m-1", protocol.DetailResult{
		SubscriptionID: subscribe.SubscriptionID,
		CapturedAt:     time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Snapshot:       protocol.ProcessSnapshot{TotalProcesses: 2, Processes: []protocol.ProcessRow{}},
	})
	response, body := call(t, http.MethodGet, server.URL+"?machineId=m-1&viewerId=v-1")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("read status = %d: %s", response.StatusCode, body)
	}
	read, err := api.DecodeProcessReadResponse(body)
	if err != nil {
		t.Fatalf("decode read: %v", err)
	}
	if read.State != "ready" || read.Result == nil || read.Result.Snapshot.TotalProcesses != 2 {
		t.Fatalf("read = %+v", read)
	}

	// The sweep renews the collector lease while the viewer stays.
	before := sender.count()
	time.Sleep(60 * time.Millisecond)
	if sender.count() <= before {
		t.Fatalf("collector lease was not renewed")
	}
	for _, message := range sender.snapshot() {
		if renew, ok := message.(protocol.DetailSubscribe); ok && renew.SubscriptionID != subscribe.SubscriptionID {
			t.Fatalf("renewal duplicated the subscription")
		}
	}

	// Ending the browser session drops the interest and cancels the lease.
	interests.EndSession(session.ID)
	if response, _ := call(t, http.MethodGet, server.URL+"?machineId=m-1&viewerId=v-1"); response.StatusCode != http.StatusGone {
		t.Fatalf("ended session read status = %d", response.StatusCode)
	}

	// A disconnected collector is reported as such.
	if response, body := call(t, http.MethodPost, server.URL+"?machineId=m-1&viewerId=v-2"); response.StatusCode != http.StatusOK {
		t.Fatalf("re-acquire status = %d: %s", response.StatusCode, body)
	}
	interests.Disconnect("m-1")
	sender.mu.Lock()
	sender.allow = false
	sender.mu.Unlock()
	response, body = call(t, http.MethodGet, server.URL+"?machineId=m-1&viewerId=v-2")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("disconnected read status = %d: %s", response.StatusCode, body)
	}
	read, err = api.DecodeProcessReadResponse(body)
	if err != nil {
		t.Fatalf("decode disconnected: %v", err)
	}
	if read.State != "disconnected" || read.Result != nil {
		t.Fatalf("disconnected read = %+v", read)
	}
}

func (r *recordingSender) snapshot() []protocol.HubMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]protocol.HubMessage{}, r.messages...)
}

func TestProcessInterestsEndOnSessionRevocation(t *testing.T) {
	state, info := newState(t)
	state.SetInventory(info.MachineID, protocol.Inventory{
		Capabilities: protocol.Capabilities{Processes: protocol.CapabilityState{Status: protocol.CapabilitySupported}},
	})
	session, revoked := newSession()
	sender := newRecordingSender()
	interests := live.NewProcessInterests(live.ProcessOptions{
		Auth:   &fakeAuth{session: session},
		State:  state,
		Sender: sender,
	})
	t.Cleanup(interests.Close)
	server := httptest.NewServer(interests)
	t.Cleanup(server.Close)

	if response, body := call(t, http.MethodPost, server.URL+"?machineId=m-1&viewerId=v-1"); response.StatusCode != http.StatusOK {
		t.Fatalf("acquire status = %d: %s", response.StatusCode, body)
	}
	close(revoked)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, _ := call(t, http.MethodGet, server.URL+"?machineId=m-1&viewerId=v-1")
		if response.StatusCode == http.StatusGone {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if response, _ := call(t, http.MethodGet, server.URL+"?machineId=m-1&viewerId=v-1"); response.StatusCode != http.StatusGone {
		t.Fatalf("revoked session still owns a viewer: %d", response.StatusCode)
	}
	var cancelled bool
	for _, message := range sender.snapshot() {
		if _, ok := message.(protocol.DetailCancel); ok {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatalf("collector lease was not cancelled after revocation")
	}
}

func TestProcessAcquireAfterLogoutIsRejected(t *testing.T) {
	state, info := newState(t)
	state.SetInventory(info.MachineID, protocol.Inventory{
		Capabilities: protocol.Capabilities{Processes: protocol.CapabilityState{Status: protocol.CapabilitySupported}},
	})
	session, revoked := newSession()
	close(revoked)
	sender := newRecordingSender()
	interests := live.NewProcessInterests(live.ProcessOptions{
		Auth:   &fakeAuth{session: session},
		State:  state,
		Sender: sender,
	})
	t.Cleanup(interests.Close)
	server := httptest.NewServer(interests)
	t.Cleanup(server.Close)

	// The session was revoked before the request reached the handler, even
	// though authentication still resolved it. No viewer and no collector lease
	// may be created.
	response, body := call(t, http.MethodPost, server.URL+"?machineId=m-1&viewerId=v-1")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked acquire status = %d: %s", response.StatusCode, body)
	}
	if sender.count() != 0 {
		t.Fatalf("revoked session sent collector messages: %+v", sender.snapshot())
	}
	if interests.ViewerCount() != 0 {
		t.Fatalf("revoked session owns %d viewers", interests.ViewerCount())
	}
}

func TestProcessBudgetsAndCapabilities(t *testing.T) {
	state, info := newState(t)
	state.SetInventory(info.MachineID, protocol.Inventory{
		Capabilities: protocol.Capabilities{Processes: protocol.CapabilityState{
			Status: protocol.CapabilityWithheld,
			Reason: stringPtr("processes are withheld by the collector policy"),
		}},
	})
	session, _ := newSession()
	sender := newRecordingSender()
	interests := live.NewProcessInterests(live.ProcessOptions{
		Auth:   &fakeAuth{session: session},
		State:  state,
		Sender: sender,
	})
	t.Cleanup(interests.Close)
	server := httptest.NewServer(interests)
	t.Cleanup(server.Close)

	response, body := call(t, http.MethodPost, server.URL+"?machineId=m-1&viewerId=v-1")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("withheld status = %d: %s", response.StatusCode, body)
	}
	var apiError api.APIError
	if err := json.Unmarshal(body, &apiError); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if apiError.Error != "processes are withheld by the collector policy" {
		t.Fatalf("withheld reason = %q", apiError.Error)
	}

	// Supported again: the per-session budget is eight viewers.
	state.SetInventory(info.MachineID, protocol.Inventory{
		Capabilities: protocol.Capabilities{Processes: protocol.CapabilityState{Status: protocol.CapabilitySupported}},
	})
	for index := 0; index < live.MaxViewersPerSession; index++ {
		url := server.URL + "?machineId=m-1&viewerId=" + string(rune('a'+index))
		if response, body := call(t, http.MethodPost, url); response.StatusCode != http.StatusOK {
			t.Fatalf("viewer %d status = %d: %s", index, response.StatusCode, body)
		}
	}
	response, body = call(t, http.MethodPost, server.URL+"?machineId=m-1&viewerId=overflow")
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("budget status = %d: %s", response.StatusCode, body)
	}
	if err := json.Unmarshal(body, &apiError); err != nil {
		t.Fatalf("decode budget error: %v", err)
	}
	if apiError.Code == nil || *apiError.Code != api.ErrorCodeProcessViewerBudget {
		t.Fatalf("budget code = %+v", apiError.Code)
	}

	// An offline machine never acquires a viewer.
	state.EndSession("collector-session-1")
	response, body = call(t, http.MethodPost, server.URL+"?machineId=m-1&viewerId=offline")
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("offline status = %d: %s", response.StatusCode, body)
	}

	// Scope and identifier checks run before anything else.
	response, _ = call(t, http.MethodPost, server.URL+"?machineId=m-1&viewerId=bad%20id")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("viewer id status = %d", response.StatusCode)
	}
	response, _ = call(t, http.MethodGet, server.URL+"?machineId=m-unknown&viewerId=valid")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown machine status = %d", response.StatusCode)
	}
}
