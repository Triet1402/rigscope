package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/webui"
	"github.com/Triet1402/rigscope/internal/version"
)

const (
	defaultMaxBodyBytes      = 1 << 20
	defaultReadHeaderTimeout = 10 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultMaxHeaderBytes    = 64 << 10
	// defaultResponseWriteTimeout is the longest a response may go without
	// progress. Each write of up to writeChunkBytes renews it, so a large
	// download that keeps moving is never truncated, while a client that stops
	// reading is cut off. SSE handlers renew the deadline per write through
	// StreamWriter.
	defaultResponseWriteTimeout = 30 * time.Second
	// writeChunkBytes is the largest slice of a handler write between two
	// deadline renewals.
	writeChunkBytes      = 64 << 10
	collectorConnectPath = "/api/collector/connect"
)

// Options configures one hub HTTP server. Hub is the validated hub section of
// the installation configuration; everything else is an explicit override for
// tests and embedding.
type Options struct {
	Hub      config.Hub
	StateDir string
	Version  string

	// UI is the static export. A nil value uses the export embedded through
	// internal/hub/webui; a server without a built export refuses to start.
	UI fs.FS
	// Logger receives lifecycle and handler errors. A nil logger discards them.
	Logger *slog.Logger

	MaxBodyBytes      int64
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	// ResponseWriteTimeout is the idle-write timeout: the longest a response
	// write may make no progress before the connection is abandoned. Zero uses
	// the default; a negative value disables the deadline.
	ResponseWriteTimeout time.Duration

	// HubRunID overrides the random per-process identifier in /api/health.
	// Tests use a fixed value; production leaves it empty.
	HubRunID string
}

type mount struct {
	prefix  string
	handler http.Handler
}

// Server owns one listener, the static export and the security middleware.
// API, SSE and collector handlers are mounted by the composition root; this
// package deliberately does not implement them.
type Server struct {
	opts   Options
	hub    config.Hub
	logger *slog.Logger
	policy *securityPolicy
	ui     *webui.Handler
	runID  string
	tls    *tls.Config
	// trustedPeersOnly is set for a plaintext non-loopback listener that is
	// admitted only because a trusted reverse proxy fronts it. Such a listener
	// accepts loopback peers and peers from trustedProxies, and closes every
	// other connection before any handler sees it.
	trustedPeersOnly bool
	// responseWriteTimeout is the idle-write timeout renewed by each response
	// chunk; zero disables the deadline.
	responseWriteTimeout time.Duration
	baseCtx              context.Context
	cancel               context.CancelFunc
	done                 chan struct{}
	closed               sync.Once
	conns                *connTracker

	mu       sync.RWMutex
	mounts   []mount
	upgrades []mount
	listener net.Listener
	serving  bool

	http *http.Server
}

// New validates the configuration and prepares (but does not bind) a server.
func New(options Options) (*Server, error) {
	hub := options.Hub
	if strings.TrimSpace(hub.Listen) == "" {
		hub.Listen = "127.0.0.1"
	}
	if strings.ContainsAny(hub.Listen, " \t\r\n") {
		return nil, fmt.Errorf("server: listen address %q contains whitespace", hub.Listen)
	}
	if hub.Port < 0 || hub.Port > 65535 {
		return nil, fmt.Errorf("server: port %d is outside 0-65535", hub.Port)
	}
	loopback := isLoopbackHost(hub.Listen)
	if hub.Origin != "" {
		canonical := config.CanonicalOrigin(hub.Origin)
		if canonical == "" {
			return nil, fmt.Errorf("server: origin %q is not a valid absolute http(s) origin", hub.Origin)
		}
		if canonical != hub.Origin {
			return nil, fmt.Errorf("server: origin %q is not canonical; use %q", hub.Origin, canonical)
		}
	} else if !loopback {
		return nil, fmt.Errorf("server: origin is required when listening on non-loopback address %q", hub.Listen)
	}
	if (hub.TLSCert == "") != (hub.TLSKey == "") {
		return nil, errors.New("server: tlsCert and tlsKey must be set together")
	}

	trusted, err := parseTrustedProxies(hub.TrustedProxies)
	if err != nil {
		return nil, err
	}
	if !loopback && hub.TLSCert == "" && len(trusted) == 0 {
		return nil, fmt.Errorf("server: refusing to bind non-loopback address %q without TLS or a configured trusted reverse proxy", hub.Listen)
	}

	var tlsConfig *tls.Config
	if hub.TLSCert != "" {
		certificate, err := tls.LoadX509KeyPair(hub.TLSCert, hub.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("server: load TLS certificate: %w", err)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			},
		}
	}

	export := options.UI
	if export == nil {
		export = webui.FS()
		if !webui.Available() {
			return nil, errors.New("server: the UI export is not embedded; run `npm run build:ui` and `go generate ./internal/hub/webui`")
		}
	}
	ui, err := webui.New(export)
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}

	maxBody := options.MaxBodyBytes
	if maxBody == 0 {
		maxBody = defaultMaxBodyBytes
	}
	policy, err := newSecurityPolicy(hostSettings{
		Listen:           strings.Trim(hub.Listen, "[]"),
		Port:             hub.Port,
		Origin:           hub.Origin,
		ForwardedHeaders: hub.ForwardedHeaders,
	}, loopback, tlsConfig != nil, trusted, maxBody)
	if err != nil {
		return nil, err
	}

	runID := options.HubRunID
	if runID == "" {
		runID, err = newUUID()
		if err != nil {
			return nil, fmt.Errorf("server: generate hub run id: %w", err)
		}
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	policy.logger = logger

	buildVersion := options.Version
	if buildVersion == "" {
		buildVersion = version.Version
	}

	baseCtx, cancel := context.WithCancel(context.Background())
	server := &Server{
		opts:             options,
		hub:              hub,
		logger:           logger,
		policy:           policy,
		ui:               ui,
		runID:            runID,
		tls:              tlsConfig,
		trustedPeersOnly: !loopback && tlsConfig == nil && len(trusted) > 0,
		baseCtx:          baseCtx,
		cancel:           cancel,
		done:             make(chan struct{}),
		conns:            newConnTracker(),
	}
	server.responseWriteTimeout = durationOrDefault(options.ResponseWriteTimeout, defaultResponseWriteTimeout)
	server.http = &http.Server{
		Handler:           server,
		ReadTimeout:       durationOrDefault(options.ReadTimeout, defaultReadTimeout),
		ReadHeaderTimeout: durationOrDefault(options.ReadHeaderTimeout, defaultReadHeaderTimeout),
		IdleTimeout:       durationOrDefault(options.IdleTimeout, defaultIdleTimeout),
		MaxHeaderBytes:    intOrDefault(options.MaxHeaderBytes, defaultMaxHeaderBytes),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	server.opts.Version = buildVersion
	return server, nil
}

// Handler exposes the fully wrapped handler for embedding in another server.
// Prefer Listen/Serve; the security policy uses the configured or actually
// bound port either way.
func (s *Server) Handler() http.Handler { return s }

// Listen binds the configured address. A port of 0 selects an ephemeral port;
// the security policy learns the real port before any request is served.
func (s *Server) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return errors.New("server: already listening")
	}
	host := strings.Trim(s.hub.Listen, "[]")
	address := net.JoinHostPort(host, strconv.Itoa(s.hub.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	s.listener = listener
	if tcp, ok := listener.Addr().(*net.TCPAddr); ok {
		s.policy.setBoundPort(tcp.Port)
	}
	return nil
}

// StopAccepting closes the listener while leaving established connections and
// their handlers running. It is the first shutdown step: the composition root
// then ends SSE/WebSocket streams deliberately, drains with Shutdown and only
// afterwards closes storage.
func (s *Server) StopAccepting() error {
	s.mu.Lock()
	listener := s.listener
	s.listener = nil
	s.mu.Unlock()
	if listener == nil {
		return nil
	}
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// Serve accepts requests on the bound listener until Shutdown or Close.
func (s *Server) Serve() error {
	s.mu.Lock()
	listener := s.listener
	if listener == nil {
		s.mu.Unlock()
		return errors.New("server: not listening; call Listen first")
	}
	s.serving = true
	s.mu.Unlock()

	if s.tls != nil {
		listener = tls.NewListener(listener, s.tls)
	}
	if s.trustedPeersOnly {
		listener = &trustedPeerListener{Listener: listener, policy: s.policy, logger: s.logger}
	}
	err := s.http.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// ListenAndServe binds and serves until Shutdown; it is the blocking form.
func (s *Server) ListenAndServe() error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve()
}

// Addr reports the bound address, or nil before Listen.
func (s *Server) Addr() net.Addr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// URL returns a human-readable URL for the bound listener. A configured
// origin wins because that is the address browsers must use.
func (s *Server) URL() string {
	if s.hub.Origin != "" {
		return s.hub.Origin
	}
	return s.ListenURL()
}

// ListenURL is the address the hub's own socket serves, which differs from
// URL when a reverse proxy answers at the configured origin.
func (s *Server) ListenURL() string {
	address := s.Addr()
	if address == nil {
		return ""
	}
	scheme := "http"
	if s.tls != nil {
		scheme = "https"
	}
	return scheme + "://" + address.String()
}

// Done is closed when shutdown starts. Long-lived handlers (SSE, process
// detail) select on it in addition to their request context.
func (s *Server) Done() <-chan struct{} { return s.done }

// Mount registers an HTTP handler for a path prefix such as "/api/". The
// longest matching prefix wins, so a specific route can sit above a broad
// /api/ mount. Mounting after Listen is refused.
func (s *Server) Mount(pattern string, handler http.Handler) error {
	return s.addMount(&s.mounts, pattern, handler)
}

// MountUpgrade registers the handler for a WebSocket upgrade path prefix.
func (s *Server) MountUpgrade(pattern string, handler http.Handler) error {
	return s.addMount(&s.upgrades, pattern, handler)
}

func (s *Server) addMount(list *[]mount, pattern string, handler http.Handler) error {
	if pattern == "" || !strings.HasPrefix(pattern, "/") {
		return errInvalidPattern
	}
	if strings.ContainsAny(pattern, "?#") {
		return errors.New("server: mount pattern must not contain a query or fragment")
	}
	if handler == nil {
		return errors.New("server: mount handler is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil || s.serving {
		return errors.New("server: mounts must be registered before Listen")
	}
	*list = append(*list, mount{prefix: pattern, handler: handler})
	// Keep the longest prefixes first so route lookup is simple and stable.
	sort.SliceStable(*list, func(i, j int) bool { return len((*list)[i].prefix) > len((*list)[j].prefix) })
	return nil
}

// Shutdown stops accepting requests, cancels request contexts (which ends
// SSE and process-detail streams that select on them), closes hijacked
// WebSocket connections, and drains in-flight requests until ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
	s.closed.Do(func() {
		close(s.done)
		s.cancel()
		s.conns.closeAll()
	})
	s.mu.RLock()
	listener := s.listener
	serving := s.serving
	s.mu.RUnlock()
	if listener != nil && !serving {
		// Serve never registered this listener with http.Server, so its
		// shutdown cannot close it.
		_ = listener.Close()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.http.Shutdown(ctx); err != nil {
		_ = s.http.Close()
		return err
	}
	return nil
}

// Close stops the server immediately without draining.
func (s *Server) Close() error {
	s.closed.Do(func() {
		close(s.done)
		s.cancel()
		s.conns.closeAll()
	})
	return s.http.Close()
}

// ServeHTTP applies the security middleware before every mounted handler, the
// health endpoint and the static UI.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Bound ordinary responses by an idle-write timeout: a client that stops
	// reading cannot pin a handler forever, while a response that keeps making
	// progress (a large static file through io.Copy) is never truncated. Each
	// chunk of up to writeChunkBytes renews the deadline. Streaming handlers
	// renew per write with StreamWriter, and a hijacked WebSocket owns its own
	// deadlines.
	bounded := &progressWriter{
		ResponseWriter: w,
		controller:     http.NewResponseController(w),
		timeout:        s.responseWriteTimeout,
	}
	bounded.extend()
	if !s.policy.check(bounded, r) {
		return
	}
	s.route(bounded, r)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	pathname := requestPathname(r)
	if isUpgrade(r) {
		s.serveUpgrade(w, r)
		return
	}
	if pathname == "/api/health" {
		s.serveHealth(w, r)
		return
	}
	if pathname == collectorConnectPath {
		// HTTP access to the collector endpoint is an explicit upgrade error.
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = io.WriteString(w, "upgrade required")
		return
	}
	if matched := s.match(s.mounts, pathname); matched != nil {
		matched.handler.ServeHTTP(w, r)
		return
	}
	if pathname == "/api" || strings.HasPrefix(pathname, "/api/") {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	s.ui.ServeHTTP(w, r)
}

func (s *Server) serveUpgrade(w http.ResponseWriter, r *http.Request) {
	pathname := requestPathname(r)
	matched := s.match(s.upgrades, pathname)
	if matched == nil {
		if pathname == collectorConnectPath {
			http.Error(w, "collector ingestion is not available in this build", http.StatusNotImplemented)
			return
		}
		// Production has no framework: only mounted upgrade paths exist.
		http.Error(w, "framework upgrades are disabled", http.StatusForbidden)
		return
	}
	tracked := &trackingResponseWriter{ResponseWriter: w, tracker: s.conns}
	matched.handler.ServeHTTP(tracked, r)
}

func (s *Server) match(list []mount, pathname string) *mount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for index := range list {
		if patternMatches(list[index].prefix, pathname) {
			return &list[index]
		}
	}
	return nil
}

func patternMatches(prefix, pathname string) bool {
	if prefix == "/" {
		return true
	}
	if pathname == prefix {
		return true
	}
	trimmed := strings.TrimSuffix(prefix, "/")
	return strings.HasPrefix(pathname, trimmed+"/")
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	body := fmt.Sprintf(`{"error":%s}`, strconv.Quote(message))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	if value < 0 {
		return 0
	}
	return value
}

func intOrDefault(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	if value < 0 {
		return 0
	}
	return value
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func newUUID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16]), nil
}

// progressWriter turns one request-wide write deadline into an idle-write
// deadline. A handler write is split into chunks of at most writeChunkBytes and
// the deadline is pushed forward before each chunk, so a large response that a
// client keeps reading is never cut off at a fixed total time. A client that
// stops reading stops draining the socket, the next chunk blocks, and the
// deadline that was renewed for that chunk fires.
//
// It implements Unwrap, Flush and Hijack so http.ResponseController, streaming
// handlers and WebSocket upgrades keep working through the wrapper.
type progressWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
}

// Write renews the deadline once per chunk and never writes more than
// writeChunkBytes at a time.
func (w *progressWriter) Write(data []byte) (int, error) {
	total := 0
	for len(data) > 0 {
		chunk := data
		if len(chunk) > writeChunkBytes {
			chunk = chunk[:writeChunkBytes]
		}
		w.extend()
		written, err := w.ResponseWriter.Write(chunk)
		total += written
		data = data[written:]
		if err != nil {
			return total, err
		}
		if written != len(chunk) {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

// WriteHeader renews the deadline so the header write itself is bounded.
func (w *progressWriter) WriteHeader(status int) {
	w.extend()
	w.ResponseWriter.WriteHeader(status)
}

// Flush renews the deadline, then flushes the wrapped writer when it supports
// flushing.
func (w *progressWriter) Flush() {
	w.extend()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack lets WebSocket upgrades take over the connection. The deadline set for
// the HTTP response is cleared by the upgrade handler.
func (w *progressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.controller.Hijack()
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (w *progressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *progressWriter) extend() {
	if w.timeout <= 0 {
		// The deadline is disabled for this server.
		return
	}
	_ = w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
}

// trackingResponseWriter records hijacked connections so shutdown can close
// WebSocket sessions, and clears the server's read deadline after a hijack.
type trackingResponseWriter struct {
	http.ResponseWriter
	tracker *connTracker
}

func (t *trackingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("server: response writer does not support hijacking")
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	_ = connection.SetDeadline(time.Time{})
	tracked := &trackedConn{Conn: connection, tracker: t.tracker}
	t.tracker.add(tracked)
	return tracked, buffered, nil
}

func (t *trackingResponseWriter) Flush() {
	if flusher, ok := t.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (t *trackingResponseWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

type connTracker struct {
	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

func newConnTracker() *connTracker {
	return &connTracker{conns: map[net.Conn]struct{}{}}
}

// add registers a hijacked connection. Once shutdown has started, a late
// registration is closed immediately: there is no window in which a WebSocket
// can outlive the server it belongs to.
func (t *connTracker) add(connection net.Conn) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = connection.Close()
		return
	}
	if t.conns == nil {
		t.conns = map[net.Conn]struct{}{}
	}
	t.conns[connection] = struct{}{}
	t.mu.Unlock()
}

func (t *connTracker) closeAll() {
	t.mu.Lock()
	t.closed = true
	connections := make([]net.Conn, 0, len(t.conns))
	for connection := range t.conns {
		connections = append(connections, connection)
	}
	t.conns = map[net.Conn]struct{}{}
	t.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func (t *connTracker) remove(connection net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conns, connection)
}

// trackedConn removes itself from the tracker when the WebSocket handler
// closes it, so a long-running hub does not retain every connection ever
// hijacked.
type trackedConn struct {
	net.Conn
	tracker *connTracker
	once    sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.tracker.remove(c) })
	return c.Conn.Close()
}

// trustedPeerListener enforces the trusted-proxy boundary at the transport
// level. Serve installs it for a plaintext non-loopback listener that only
// exists because a reverse proxy fronts the hub: any peer that is neither a
// configured trusted proxy nor loopback is closed before request parsing, so no
// handler, redirect or header can be reached. Loopback stays open so local
// readiness probes and operator requests keep working.
type trustedPeerListener struct {
	net.Listener
	policy *securityPolicy
	logger *slog.Logger
}

func (l *trustedPeerListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		peer := peerIP(connection.RemoteAddr().String())
		if l.policy.admitsPeer(peer) {
			return connection, nil
		}
		l.logger.Debug("refused peer on plaintext trusted-proxy listener; address is neither a trusted proxy nor loopback",
			"peer", connection.RemoteAddr().String())
		_ = connection.Close()
	}
}
