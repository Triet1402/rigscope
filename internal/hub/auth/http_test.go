package auth

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

// fastHash is a test-only opaque password hash. Storage treats it as opaque;
// the injected verifier knows the format.
func fastHash(password string) (string, error) { return "test:" + password, nil }

func fastVerify(password, encoded string) (bool, error) {
	if strings.HasPrefix(encoded, "$argon2id$") {
		return VerifyPassword(password, encoded)
	}
	return encoded == "test:"+password, nil
}

func testScope(ids ...string) storage.Scope { return storage.MachineScope(ids...) }

type authHarness struct {
	t       *testing.T
	db      *storage.Database
	users   *storage.Users
	manager *Manager
	server  *httptest.Server
	origin  string
	secure  bool
	now     time.Time
	mu      sync.Mutex
}

func (h *authHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *authHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

type harnessOptions struct {
	users       []storage.User
	secure      bool
	ttl         time.Duration
	verify      func(string, string) (bool, error)
	origin      bool // false leaves OriginRules in loopback mode
	withServers bool
}

func newHarness(t *testing.T, opts harnessOptions) *authHarness {
	t.Helper()
	harness := &authHarness{t: t, now: time.Unix(1_700_000_000, 0)}
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{Now: harness.clock})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	harness.db = db
	users := storage.NewUsers(db)
	harness.users = users
	for _, user := range opts.users {
		if err := users.Create(context.Background(), "test", user); err != nil {
			t.Fatalf("create user %q: %v", user.Username, err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	scheme := "http"
	origin := ""
	if opts.secure {
		scheme = "https"
		origin = scheme + "://127.0.0.1:" + itoa(port)
	}
	rules, err := NewOriginRules(origin, "127.0.0.1", port, opts.secure)
	if err != nil {
		t.Fatalf("origin rules: %v", err)
	}
	manager, err := New(Options{
		Users:          users,
		Sessions:       storage.NewSessions(db),
		Origin:         rules,
		TTL:            opts.ttl,
		Now:            harness.clock,
		VerifyPassword: choose(opts.verify, fastVerify),
		SweepInterval:  time.Hour,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	harness.manager = manager
	if !opts.withServers {
		_ = listener.Close()
		return harness
	}
	handler := NewHandler(manager)
	mux := http.NewServeMux()
	mux.Handle("/api/auth/", handler)
	server := httptest.NewUnstartedServer(mux)
	server.Listener = listener
	if opts.secure {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	harness.server = server
	harness.origin = scheme + "://" + server.Listener.Addr().String()
	harness.secure = opts.secure
	return harness
}

func choose(value, fallback func(string, string) (bool, error)) func(string, string) (bool, error) {
	if value != nil {
		return value
	}
	return fallback
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

func (h *authHarness) client() *http.Client {
	if h.server != nil {
		return h.server.Client()
	}
	return http.DefaultClient
}

func (h *authHarness) do(t *testing.T, request *http.Request, body any) *http.Response {
	t.Helper()
	if body != nil {
		payload := bodyString(t, body)
		request.Body = io.NopCloser(strings.NewReader(payload))
		request.ContentLength = int64(len(payload))
		if request.Header.Get("Content-Type") == "" {
			request.Header.Set("Content-Type", "application/json")
		}
	}
	response, err := h.client().Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL, err)
	}
	return response
}

func readAll(t *testing.T, body io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func bodyString(t *testing.T, body any) string {
	t.Helper()
	if value, ok := body.(string); ok {
		return value
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (h *authHarness) url(path string) string {
	return h.origin + path
}

func (h *authHarness) login(t *testing.T, username, password string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.url(loginPath), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", h.origin)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := h.do(t, request, map[string]string{"username": username, "password": password})
	data, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	return response, data
}

func (h *authHarness) sessionCookie(t *testing.T, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, h.url(sessionPath), nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := h.client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeSession(t *testing.T, data []byte) api.SessionResponse {
	t.Helper()
	response, err := api.DecodeSessionResponse(data)
	if err != nil {
		t.Fatalf("decode session response %s: %v", data, err)
	}
	return response
}

func decodeError(t *testing.T, data []byte) api.APIError {
	t.Helper()
	body, err := api.DecodeAPIError(data)
	if err != nil {
		t.Fatalf("decode error body %q: %v", data, err)
	}
	return body
}

func TestLoginIssuesCookieAndPayload(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users:       []storage.User{{Username: "demo", PasswordHash: "test:demo-password", Scope: storage.AllMachines()}},
		withServers: true,
	})
	response, data := harness.login(t, "demo", "demo-password", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.StatusCode, data)
	}
	payload := decodeSession(t, data)
	if payload.Username != "demo" || payload.CSRFToken == "" || payload.Admin != nil {
		t.Fatalf("payload = %+v", payload)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != SessionCookieName || cookie.Value == "" {
		t.Fatalf("cookie = %+v", cookie)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Fatalf("attributes missing: %+v", cookie)
	}
	if cookie.Secure {
		t.Fatal("loopback plaintext cookie is Secure")
	}
	if cookie.MaxAge <= 0 || cookie.MaxAge > int(DefaultSessionTTL/time.Second) {
		t.Fatalf("Max-Age = %d", cookie.MaxAge)
	}
}

func TestSecureCookieOverHTTPS(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users:       []storage.User{{Username: "demo", PasswordHash: "test:demo-password", Scope: storage.AllMachines()}},
		secure:      true,
		withServers: true,
	})
	response, data := harness.login(t, "demo", "demo-password", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.StatusCode, data)
	}
	cookie := response.Cookies()[0]
	if cookie.Name != RemoteSessionCookieName || !cookie.Secure || !cookie.HttpOnly {
		t.Fatalf("remote cookie = %+v", cookie)
	}
	// Logout clears the same cookie with Max-Age=0 and Secure.
	payload := decodeSession(t, data)
	request, _ := http.NewRequest(http.MethodPost, harness.url(logoutPath), nil)
	request.Header.Set("Origin", harness.origin)
	request.Header.Set("X-CSRF-Token", payload.CSRFToken)
	request.AddCookie(cookie)
	response = harness.do(t, request, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d", response.StatusCode)
	}
	cleared := response.Cookies()[0]
	if cleared.Name != RemoteSessionCookieName || !cleared.Secure || cleared.MaxAge >= 0 {
		t.Fatalf("cleared cookie = %+v", cleared)
	}
}

func TestCookieAttributesExact(t *testing.T) {
	if got := sessionCookie(SessionCookieName, "token", 43200, false); got !=
		"rigscope_session=token; HttpOnly; SameSite=Strict; Path=/; Max-Age=43200" {
		t.Fatalf("local cookie = %q", got)
	}
	if got := sessionCookie(RemoteSessionCookieName, "token", 43200, true); got !=
		"__Host-rigscope_session=token; Secure; HttpOnly; SameSite=Strict; Path=/; Max-Age=43200" {
		t.Fatalf("remote cookie = %q", got)
	}
	if got := clearedSessionCookie(SessionCookieName, false); got !=
		"rigscope_session=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0" {
		t.Fatalf("cleared cookie = %q", got)
	}
}

func TestLoginRejections(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users: []storage.User{
			{Username: "demo", PasswordHash: "test:demo-password", Scope: storage.AllMachines()},
			{Username: "disabled", PasswordHash: "test:demo-password", Scope: storage.AllMachines(), Disabled: true},
		},
		withServers: true,
	})
	for _, test := range []struct {
		username string
		password string
	}{
		{"demo", "wrong"},
		{"unknown", "demo-password"},
		{"disabled", "demo-password"},
	} {
		response, data := harness.login(t, test.username, test.password, nil)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%q: status = %d", test.username, response.StatusCode)
		}
		if message := decodeError(t, data).Error; message != "invalid username or password" {
			t.Fatalf("%q: message = %q", test.username, message)
		}
		if len(response.Cookies()) != 0 {
			t.Fatalf("%q: failure set a cookie", test.username)
		}
	}
}

func TestLoginRequestShapes(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users:       []storage.User{{Username: "demo", PasswordHash: "test:demo-password", Scope: storage.AllMachines()}},
		withServers: true,
	})
	post := func(t *testing.T, headers map[string]string, body string) int {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, harness.url(loginPath), nil)
		request.Header.Set("Origin", harness.origin)
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response := harness.do(t, request, body)
		defer response.Body.Close()
		return response.StatusCode
	}
	jsonType := map[string]string{"Content-Type": "application/json"}
	if status := post(t, jsonType, `{"username":"demo"}`); status != http.StatusBadRequest {
		t.Fatalf("missing password = %d", status)
	}
	if status := post(t, jsonType, `{"username":"demo","password":"x","extra":true}`); status != http.StatusBadRequest {
		t.Fatalf("extra field = %d", status)
	}
	if status := post(t, jsonType, `not json`); status != http.StatusBadRequest {
		t.Fatalf("malformed JSON = %d", status)
	}
	if status := post(t, map[string]string{"Content-Type": "text/plain"}, `{}`); status != http.StatusUnsupportedMediaType {
		t.Fatalf("content type = %d", status)
	}
	// The 1.0 media-type rule is a parsed comparison, not a prefix: parameters
	// and a case-insensitive type are accepted, an unknown type is not.
	if status := post(t, map[string]string{"Content-Type": "Application/JSON; charset=utf-8"}, `{"username":"demo","password":"demo-password"}`); status != http.StatusOK {
		t.Fatalf("parameterised JSON = %d", status)
	}
	if status := post(t, map[string]string{"Content-Type": "application/jsonx"}, `{}`); status != http.StatusUnsupportedMediaType {
		t.Fatalf("prefix media type = %d", status)
	}
	oversized := `{"username":"demo","password":"` + strings.Repeat("x", 5000) + `"}`
	if status := post(t, jsonType, oversized); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d", status)
	}
	request, _ := http.NewRequest(http.MethodGet, harness.url(loginPath), nil)
	response := harness.do(t, request, nil)
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("GET login = %d %q", response.StatusCode, response.Header.Get("Allow"))
	}
}

func TestLoginOriginMatrix(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users:       []storage.User{{Username: "demo", PasswordHash: "test:demo-password", Scope: storage.AllMachines()}},
		withServers: true,
	})
	for _, origin := range []string{"", "http://127.0.0.1:1", "https://127.0.0.1:1", "http://evil.invalid", "null", "http://evil.example/path"} {
		request, _ := http.NewRequest(http.MethodPost, harness.url(loginPath), nil)
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		response := harness.do(t, request, map[string]string{"username": "demo", "password": "demo-password"})
		data := readAll(t, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q status = %d", origin, response.StatusCode)
		}
		if decodeError(t, data).Error != "origin not allowed" {
			t.Fatalf("origin %q message = %s", origin, data)
		}
		if len(response.Cookies()) != 0 {
			t.Fatalf("origin %q set a cookie", origin)
		}
	}
}

func TestSessionAndLogoutFlow(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users: []storage.User{
			{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		},
		withServers: true,
	})
	response, data := harness.login(t, "owner", "owner-password", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", response.StatusCode)
	}
	payload := decodeSession(t, data)
	cookie := response.Cookies()[0]

	response = harness.sessionCookie(t, map[string]string{"Cookie": cookie.Name + "=" + cookie.Value})
	sessionData := readAll(t, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("session = %d", response.StatusCode)
	}
	if got := decodeSession(t, sessionData); got.Username != "owner" || got.Admin == nil || !*got.Admin {
		t.Fatalf("session payload = %+v", got)
	}
	if response := harness.sessionCookie(t, map[string]string{"Cookie": SessionCookieName + "=" + strings.Repeat("A", 43)}); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown cookie = %d", response.StatusCode)
	}
	if response := harness.sessionCookie(t, nil); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no cookie = %d", response.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodPost, harness.url(sessionPath), nil)
	if response := harness.do(t, request, nil); response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST session = %d", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodHead, harness.url(sessionPath), nil)
	request.Header.Set("Cookie", cookie.Name+"="+cookie.Value)
	response, err := harness.client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(readAll(t, response.Body)) != 0 {
		t.Fatalf("HEAD session = %d with a body", response.StatusCode)
	}
	_ = response.Body.Close()

	logout := func(headers map[string]string) *http.Response {
		request, _ := http.NewRequest(http.MethodPost, harness.url(logoutPath), nil)
		request.Header.Set("Origin", harness.origin)
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		return harness.do(t, request, nil)
	}
	for _, test := range []struct {
		name    string
		headers map[string]string
	}{
		{"missing csrf", map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}},
		{"wrong csrf", map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, "X-CSRF-Token": payload.CSRFToken + "0"}},
	} {
		response := logout(test.headers)
		data := readAll(t, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s = %d", test.name, response.StatusCode)
		}
		if decodeError(t, data).Error != "missing or invalid csrf token" {
			t.Fatalf("%s message = %s", test.name, data)
		}
	}
	request, _ = http.NewRequest(http.MethodPost, harness.url(logoutPath), nil)
	request.Header.Set("Cookie", cookie.Name+"="+cookie.Value)
	request.Header.Set("X-CSRF-Token", payload.CSRFToken)
	if response := harness.do(t, request, nil); response.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without origin = %d", response.StatusCode)
	}
	response = logout(map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, "X-CSRF-Token": payload.CSRFToken})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Cookies()[0].MaxAge >= 0 {
		t.Fatalf("logout = %d", response.StatusCode)
	}
	if response := harness.sessionCookie(t, map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session after logout = %d", response.StatusCode)
	}
	response = logout(map[string]string{"Cookie": cookie.Name + "=" + cookie.Value, "X-CSRF-Token": payload.CSRFToken})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || response.Cookies()[0].MaxAge >= 0 {
		t.Fatalf("repeated logout = %d", response.StatusCode)
	}
}

func TestAuthNotConfigured(t *testing.T) {
	harness := newHarness(t, harnessOptions{withServers: true})
	if response := harness.sessionCookie(t, nil); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("session = %d", response.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodPost, harness.url(loginPath), nil)
	request.Header.Set("Origin", harness.origin)
	request.Header.Set("Content-Type", "application/json")
	response := harness.do(t, request, `{}`)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("login = %d", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodPost, harness.url(logoutPath), nil)
	request.Header.Set("Origin", harness.origin)
	response = harness.do(t, request, `{}`)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("logout = %d", response.StatusCode)
	}
}

func TestUnknownAuthEndpoint(t *testing.T) {
	harness := newHarness(t, harnessOptions{withServers: true})
	response, err := harness.client().Get(harness.url("/api/auth/unknown"))
	if err != nil {
		t.Fatal(err)
	}
	data := readAll(t, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound || decodeError(t, data).Error != "unknown auth endpoint" {
		t.Fatalf("status = %d, body %s", response.StatusCode, data)
	}
}

func TestRateLimitReturns429(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users:       []storage.User{{Username: "demo", PasswordHash: "test:demo-password", Scope: storage.AllMachines()}},
		withServers: true,
	})
	sawUnauthorized, sawLimited := false, false
	for i := 0; i < 20; i++ {
		response, _ := harness.login(t, "demo", "wrong", nil)
		switch response.StatusCode {
		case http.StatusUnauthorized:
			sawUnauthorized = true
		case http.StatusTooManyRequests:
			sawLimited = true
		}
	}
	if !sawUnauthorized || !sawLimited {
		t.Fatalf("unauthorized=%v limited=%v", sawUnauthorized, sawLimited)
	}
	harness.advance(time.Second)
	response, data := harness.login(t, "demo", "demo-password", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login after recovery = %d, body %s", response.StatusCode, data)
	}
}

func TestRevocationBroadcast(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users: []storage.User{
			{Username: "alice", PasswordHash: "test:alice-password", Scope: storage.AllMachines()},
			{Username: "bob", PasswordHash: "test:bob-password", Scope: storage.AllMachines()},
		},
		ttl: 1500 * time.Millisecond,
	})
	var mu sync.Mutex
	var events []Revocation
	unsubscribe := harness.manager.Subscribe(func(event Revocation) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})
	defer unsubscribe()

	first, _, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "alice", "alice-password")
	if err != nil || !ok {
		t.Fatalf("login alice: ok=%v err=%v", ok, err)
	}
	second, _, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "bob", "bob-password")
	if err != nil || !ok {
		t.Fatalf("login bob: ok=%v err=%v", ok, err)
	}
	if err := harness.manager.Logout(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(events) != 1 || events[0].Reason != ReasonLogout || events[0].Username != "alice" {
		t.Fatalf("logout events = %+v", events)
	}
	events = nil
	mu.Unlock()

	if ended, err := harness.manager.RevokeUser(context.Background(), "bob"); err != nil || ended != 1 {
		t.Fatalf("RevokeUser = %d, %v", ended, err)
	}
	mu.Lock()
	if len(events) != 1 || events[0].Reason != ReasonRevoked || events[0].TokenHash != second.TokenHash {
		t.Fatalf("revocation events = %+v", events)
	}
	events = nil
	mu.Unlock()

	alice, _, ok, err := harness.manager.Login(context.Background(), "203.0.113.2", "alice", "alice-password")
	if err != nil || !ok {
		t.Fatalf("relogin alice: ok=%v err=%v", ok, err)
	}
	harness.advance(2 * time.Minute)
	harness.manager.sweepExpired()
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].Reason != ReasonExpired || events[0].TokenHash != alice.TokenHash {
		t.Fatalf("expiry events = %+v", events)
	}
}

func TestLoginInvalidatedByConcurrentPasswordChange(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users: []storage.User{{Username: "owner", PasswordHash: "test:old-password", Scope: storage.AllMachines()}},
	})
	started := make(chan struct{})
	release := make(chan struct{})
	manager, err := New(Options{
		Users:    harness.users,
		Sessions: storage.NewSessions(harness.db),
		Origin:   &alwaysTrusted{},
		Now:      harness.clock,
		VerifyPassword: func(password, encoded string) (bool, error) {
			close(started)
			<-release
			return encoded == "test:"+password, nil
		},
		SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	result := make(chan bool)
	go func() {
		_, _, ok, _ := manager.Login(context.Background(), "203.0.113.9", "owner", "old-password")
		result <- ok
	}()
	<-started
	if err := harness.users.SetPasswordHash(context.Background(), "test", "owner", "test:new-password"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if <-result {
		t.Fatal("login minted a session from a stale password")
	}
	sessions := storage.NewSessions(harness.db)
	count, err := sessions.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale login stored %d sessions", count)
	}
}

type alwaysTrusted struct{}

func (alwaysTrusted) Trusted(*http.Request) bool { return true }

func TestBootstrapCreatesOwnerOnce(t *testing.T) {
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	users := storage.NewUsers(db)
	password, err := Bootstrap(context.Background(), users, BootstrapOptions{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(password) != 24 {
		t.Fatalf("password length = %d", len(password))
	}
	stored, err := users.Get(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Admin || stored.Disabled || !stored.Scope.All {
		t.Fatalf("owner row = %+v", stored)
	}
	if ok, err := VerifyPassword(password, stored.PasswordHash); err != nil || !ok {
		t.Fatalf("generated password does not verify: %v", err)
	}
	again, err := Bootstrap(context.Background(), users, BootstrapOptions{})
	if err != nil || again != "" {
		t.Fatalf("second Bootstrap = %q, %v", again, err)
	}
	// A deleted owner must not be resurrected: the durable marker wins.
	if err := users.Create(context.Background(), "test", storage.User{
		Username: "second", PasswordHash: "test:second-password", Scope: storage.AllMachines(), Admin: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := users.Delete(context.Background(), "test", "owner"); err != nil {
		t.Fatal(err)
	}
	third, err := Bootstrap(context.Background(), users, BootstrapOptions{})
	if err != nil || third != "" {
		t.Fatalf("Bootstrap after delete = %q, %v", third, err)
	}
}

func TestSessionSurvivesManagerRestart(t *testing.T) {
	harness := newHarness(t, harnessOptions{
		users: []storage.User{{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines()}},
	})
	session, token, ok, err := harness.manager.Login(context.Background(), "203.0.113.1", "owner", "owner-password")
	if err != nil || !ok {
		t.Fatalf("login: ok=%v err=%v", ok, err)
	}
	request := httptest.NewRequest(http.MethodGet, sessionPath, nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	if found, err := harness.manager.Session(request); err != nil || found == nil || found.TokenHash != session.TokenHash {
		t.Fatalf("live session lookup = %+v, %v", found, err)
	}
	// 1.0 stores sessions durably in SQLite: a new hub process on the same
	// database keeps the browser signed in until the fixed expiry.
	restarted, err := New(Options{
		Users:          harness.users,
		Sessions:       storage.NewSessions(harness.db),
		Origin:         &alwaysTrusted{},
		Now:            harness.clock,
		VerifyPassword: fastVerify,
		SweepInterval:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := restarted.Session(request)
	if err != nil || adopted == nil {
		t.Fatalf("session did not survive a restart: %+v, %v", adopted, err)
	}
	if adopted.TokenHash != session.TokenHash || adopted.CSRFToken != session.CSRFToken ||
		!adopted.ExpiresAt.Equal(session.ExpiresAt) || !adopted.Scope.All {
		t.Fatalf("adopted session = %+v, want the stored %+v", adopted, session)
	}
	// Logout through the restarted process is durable too: a third process
	// must not resurrect the cookie.
	if err := restarted.Logout(context.Background(), adopted); err != nil {
		t.Fatal(err)
	}
	_ = restarted.Close()
	third, err := New(Options{
		Users:          harness.users,
		Sessions:       storage.NewSessions(harness.db),
		Origin:         &alwaysTrusted{},
		Now:            harness.clock,
		VerifyPassword: fastVerify,
		SweepInterval:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if found, err := third.Session(request); err != nil || found != nil {
		t.Fatalf("logged-out session was resurrected: %+v, %v", found, err)
	}
}

func TestCookieNameSelection(t *testing.T) {
	if got := CookieName(false); got != SessionCookieName {
		t.Fatalf("http cookie = %q", got)
	}
	if got := CookieName(true); got != RemoteSessionCookieName {
		t.Fatalf("https cookie = %q", got)
	}
}
