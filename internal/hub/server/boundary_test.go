package server

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/config"
)

// admissionListener builds the transport-level filter around fake TCP peers so
// the loopback rule can be tested without depending on the host's interfaces.
func admissionListener(t *testing.T, policy *securityPolicy, pending ...net.Conn) *trustedPeerListener {
	t.Helper()
	return &trustedPeerListener{
		Listener: &stubListener{pending: pending},
		policy:   policy,
		logger:   discardLogger(),
	}
}

// fakePeer wraps one end of a pipe with a chosen remote address.
func fakePeer(t *testing.T, remote string) (net.Conn, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	address, err := net.ResolveTCPAddr("tcp", remote)
	if err != nil {
		t.Fatalf("resolve %q: %v", remote, err)
	}
	return &remoteAddrConn{Conn: server, remote: address}, client
}

type remoteAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c *remoteAddrConn) RemoteAddr() net.Addr { return c.remote }

type stubListener struct {
	net.Listener
	pending []net.Conn
}

func (l *stubListener) Accept() (net.Conn, error) {
	if len(l.pending) == 0 {
		return nil, net.ErrClosed
	}
	connection := l.pending[0]
	l.pending = l.pending[1:]
	return connection, nil
}

func expectClosed(t *testing.T, client net.Conn) {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection was not closed")
	}
}

func TestPlaintextTrustedProxyListenerRejectsOtherPeers(t *testing.T) {
	server := newServer(t, config.Hub{
		Listen:         "0.0.0.0",
		Port:           0,
		Origin:         "https://hub.example.test",
		TrustedProxies: []string{"10.0.0.0/8"},
	})

	// A non-loopback peer outside trustedProxies is closed before it can send
	// anything; the next peer is then accepted.
	stranger, strangerClient := fakePeer(t, "192.0.2.10:4000")
	trusted, trustedClient := fakePeer(t, "10.1.2.3:4000")
	defer trustedClient.Close()

	listener := admissionListener(t, server.policy, stranger, trusted)
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got := accepted.RemoteAddr().String(); got != "10.1.2.3:4000" {
		t.Fatalf("accepted peer %q, want the trusted proxy", got)
	}
	expectClosed(t, strangerClient)
}

func TestPlaintextTrustedProxyListenerAdmitsLoopbackPeers(t *testing.T) {
	// Loopback is admitted even though it is not in trustedProxies, so local
	// readiness probes keep working.
	server := newServer(t, config.Hub{
		Listen:         "0.0.0.0",
		Port:           0,
		Origin:         "https://hub.example.test",
		TrustedProxies: []string{"10.0.0.0/8"},
	})

	for _, remote := range []string{"127.0.0.1:4000", "[::1]:4000"} {
		loopback, loopbackClient := fakePeer(t, remote)
		listener := admissionListener(t, server.policy, loopback)
		accepted, err := listener.Accept()
		if err != nil {
			t.Fatalf("%s: Accept: %v", remote, err)
		}
		if accepted.RemoteAddr().String() != remote {
			t.Fatalf("%s: accepted %q", remote, accepted.RemoteAddr())
		}
		_ = loopbackClient.Close()
	}
}

func TestLoopbackAdmissionDoesNotGrantForwardedTrust(t *testing.T) {
	// Admission is transport-level only: a loopback connection that is not a
	// configured proxy still cannot set the client address.
	server := newServer(t, config.Hub{
		Listen:         "0.0.0.0",
		Port:           0,
		Origin:         "https://hub.example.test",
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	if server.policy.trustedPeer(net.ParseIP("127.0.0.1")) {
		t.Fatal("loopback became a trusted forwarded-header source")
	}
	if !server.policy.admitsPeer(net.ParseIP("127.0.0.1")) {
		t.Fatal("loopback was not admitted to the plaintext listener")
	}
}

func TestPlaintextTrustedProxyListenerServesLoopbackProbe(t *testing.T) {
	server := newServer(t, config.Hub{
		Listen:         "0.0.0.0",
		Port:           0,
		Origin:         "https://hub.example.test",
		TrustedProxies: []string{"10.0.0.0/8"},
	})
	startServer(t, server)
	_, port, err := net.SplitHostPort(server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort("127.0.0.1", port)

	// A readiness probe connects directly over loopback with the canonical Host.
	client := &http.Client{Timeout: 5 * time.Second}
	probe, err := http.NewRequest(http.MethodGet, "http://"+address+"/api/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	probe.Host = "hub.example.test"
	response, err := client.Do(probe)
	if err != nil {
		t.Fatalf("loopback probe: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("probe status %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("probe body %q", body)
	}
}

func TestConnTrackerClosesLateRegistrations(t *testing.T) {
	tracker := newConnTracker()
	tracker.closeAll()

	server, client := net.Pipe()
	defer client.Close()
	tracker.add(server)

	_ = client.SetDeadline(time.Now().Add(time.Second))
	if _, err := client.Write([]byte("x")); err == nil {
		if _, err := client.Read(make([]byte, 1)); err == nil {
			t.Fatal("connection registered after close stayed open")
		}
	}
}

func TestStreamWriterRenewsWriteDeadline(t *testing.T) {
	recorder := &deadlineRecorder{ResponseWriter: newTestRecorder()}
	stream := NewStreamWriter(recorder)
	if _, err := stream.Write([]byte("event: ping\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := stream.FlushError(); err != nil {
		t.Fatal(err)
	}
	if recorder.deadlines < 2 {
		t.Fatalf("deadlines renewed %d times, want at least 2", recorder.deadlines)
	}
}

func TestStreamWriterImplementsHTTPFlusher(t *testing.T) {
	var flusher http.Flusher = NewStreamWriter(newTestRecorder())
	flusher.Flush()
}

func TestResponseControllerFlushRenewsStreamDeadline(t *testing.T) {
	recorder := &deadlineRecorder{ResponseWriter: newTestRecorder()}
	stream := NewStreamWriter(recorder)
	// http.NewResponseController prefers FlushError, which is the exported
	// error-reporting form of the http.Flusher contract.
	if err := http.NewResponseController(stream).Flush(); err != nil {
		t.Fatalf("ResponseController.Flush: %v", err)
	}
	if recorder.deadlines == 0 {
		t.Fatal("ResponseController.Flush did not renew the write deadline")
	}
}

func TestServeHTTPBoundsOrdinaryWrites(t *testing.T) {
	recorder := &deadlineRecorder{ResponseWriter: newTestRecorder()}
	server := newServer(t, config.Hub{Listen: "127.0.0.1", Port: 8081})
	server.ServeHTTP(recorder, request(http.MethodGet, "http://127.0.0.1:8081/"))
	if recorder.deadlines == 0 {
		t.Fatal("the middleware did not set a write deadline")
	}
}

// deadlineRecorder records SetWriteDeadline calls and delegates the rest.
type deadlineRecorder struct {
	http.ResponseWriter
	deadlines int
}

func (r *deadlineRecorder) SetWriteDeadline(time.Time) error {
	r.deadlines++
	return nil
}

func (r *deadlineRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *deadlineRecorder) Flush() {}

func newTestRecorder() http.ResponseWriter {
	return &flushRecorder{header: http.Header{}}
}

type flushRecorder struct {
	header http.Header
	status int
	body   []byte
}

func (r *flushRecorder) Header() http.Header { return r.header }
func (r *flushRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *flushRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body = append(r.body, data...)
	return len(data), nil
}
