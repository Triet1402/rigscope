package admin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

func testHash(password string) (string, error) { return "test:" + password, nil }

func testVerify(password, encoded string) (bool, error) {
	if strings.HasPrefix(encoded, "$argon2id$") {
		return auth.VerifyPassword(password, encoded)
	}
	return encoded == "test:"+password, nil
}

type adminHarness struct {
	t           *testing.T
	db          *storage.Database
	users       *storage.Users
	registry    *storage.Registry
	authManager *auth.Manager
	handler     *Handler
	server      *httptest.Server
	origin      string
	revoked     []string
	removed     []string
	metaChanges []string
	mu          sync.Mutex
	now         time.Time
	// alertDiagnostics is the fixed payload the diagnostics route returns.
	alertDiagnostics api.AlertDiagnostics
	// hashHook replaces password hashing in tests that need to pause between
	// the handler's authorization and its write transaction.
	hashHook func(string) (string, error)
}

func newAdminHarness(t *testing.T, users []storage.User, identities []storage.Identity) *adminHarness {
	t.Helper()
	harness := &adminHarness{t: t, now: time.Unix(1_700_000_000, 0)}
	harness.alertDiagnostics = api.AlertDiagnostics{
		GeneratedAt:      "2026-11-12T10:00:00.000Z",
		QueueCapacity:    256,
		UnresolvedBreaks: []string{},
	}
	db, err := storage.Open(context.Background(), ":memory:", storage.Options{Now: harness.clock})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	harness.db = db
	harness.users = storage.NewUsers(db)
	for _, user := range users {
		if err := harness.users.Create(context.Background(), "test", user); err != nil {
			t.Fatalf("create user %q: %v", user.Username, err)
		}
	}
	registry, err := storage.NewRegistry(context.Background(), db, storage.RegistryOptions{BootstrapIdentities: identities})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	harness.registry = registry

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	rules, err := auth.NewOriginRules("", "127.0.0.1", port, false)
	if err != nil {
		t.Fatalf("origin rules: %v", err)
	}
	manager, err := auth.New(auth.Options{
		Users:          harness.users,
		Sessions:       storage.NewSessions(db),
		Origin:         rules,
		Now:            harness.clock,
		VerifyPassword: testVerify,
		SweepInterval:  time.Hour,
	})
	if err != nil {
		t.Fatalf("auth manager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	harness.authManager = manager
	handler, err := New(Options{
		Users:    harness.users,
		Audit:    storage.NewAudit(db),
		Machines: storage.NewMachines(db),
		Registry: registry,
		Auth:     manager,
		MachineExists: func(_ context.Context, machineID string) (bool, error) {
			return machineID == "m-1" || machineID == "m-2", nil
		},
		OnCollectorRevoked: func(collectorID string) {
			harness.mu.Lock()
			harness.revoked = append(harness.revoked, collectorID)
			harness.mu.Unlock()
		},
		OnMachineRemoved: func(_ context.Context, machineID string) error {
			harness.mu.Lock()
			harness.removed = append(harness.removed, machineID)
			harness.mu.Unlock()
			return nil
		},
		OnMachineMetaChanged: func(machineID string) {
			harness.mu.Lock()
			harness.metaChanges = append(harness.metaChanges, machineID)
			harness.mu.Unlock()
		},
		AlertDiagnostics: func() api.AlertDiagnostics {
			harness.mu.Lock()
			defer harness.mu.Unlock()
			return harness.alertDiagnostics
		},
		HashPassword: func(password string) (string, error) {
			if harness.hashHook != nil {
				return harness.hashHook(password)
			}
			return testHash(password)
		},
	})
	if err != nil {
		t.Fatalf("admin handler: %v", err)
	}
	harness.handler = handler
	mux := http.NewServeMux()
	mux.Handle("/api/auth/", auth.NewHandler(manager))
	mux.Handle("/api/admin/", handler)
	mux.Handle("/api/machines/meta", handler)
	server := httptest.NewUnstartedServer(mux)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	harness.server = server
	harness.origin = "http://" + server.Listener.Addr().String()
	return harness
}

func (h *adminHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *adminHarness) request(t *testing.T, method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		switch value := body.(type) {
		case string:
			reader = strings.NewReader(value)
		default:
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			reader = strings.NewReader(string(raw))
		}
	}
	request, err := http.NewRequest(method, h.origin+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil && request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	return response, data
}

func (h *adminHarness) login(t *testing.T, username, password string) (cookie, csrf string, admin bool) {
	t.Helper()
	response, data := h.request(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": username, "password": password},
		map[string]string{"Origin": h.origin})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login %s = %d, body %s", username, response.StatusCode, data)
	}
	payload, err := api.DecodeSessionResponse(data)
	if err != nil {
		t.Fatalf("decode login: %v", err)
	}
	cookie = response.Cookies()[0].Name + "=" + response.Cookies()[0].Value
	return cookie, payload.CSRFToken, payload.Admin != nil && *payload.Admin
}

func (h *adminHarness) adminRequest(t *testing.T, method, path string, body any, cookie, csrf string) (*http.Response, []byte) {
	t.Helper()
	headers := map[string]string{"Cookie": cookie}
	if csrf != "" {
		headers["X-CSRF-Token"] = csrf
	}
	if method != http.MethodGet {
		headers["Origin"] = h.origin
	}
	return h.request(t, method, path, body, headers)
}

func (h *adminHarness) countRevoked() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.revoked)
}

func (h *adminHarness) revokedCollectors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.revoked...)
}

func (h *adminHarness) metaUpdates() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.metaChanges...)
}

func TestAdminRequiresSessionAndAdmin(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "viewer", PasswordHash: "test:viewer-password", Scope: storage.AllMachines()},
	}, nil)
	if response, _ := harness.request(t, http.MethodGet, usersPath, nil, nil); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", response.StatusCode)
	}
	cookie, csrf, admin := harness.login(t, "viewer", "viewer-password")
	if admin {
		t.Fatal("viewer session carries admin")
	}
	if response, _ := harness.adminRequest(t, http.MethodGet, usersPath, nil, cookie, ""); response.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer GET = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, usersPath,
		map[string]any{"username": "escalated", "machineIds": "*", "admin": true, "password": "x"},
		cookie, csrf); response.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer POST = %d", response.StatusCode)
	}
}

func TestAdminListsAndCreatesUsers(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "viewer", PasswordHash: "test:viewer-password", Scope: storage.MachineScope("m-1")},
	}, nil)
	cookie, csrf, _ := harness.login(t, "owner", "owner-password")
	response, data := harness.adminRequest(t, http.MethodGet, usersPath, nil, cookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET users = %d, body %s", response.StatusCode, data)
	}
	list, err := api.DecodeAdminUserList(data)
	if err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(list.Users) != 2 {
		t.Fatalf("users = %+v", list.Users)
	}
	if strings.Contains(string(data), "test:") {
		t.Fatal("password hash leaked into the user list")
	}

	response, data = harness.adminRequest(t, http.MethodPost, usersPath, map[string]any{
		"username": "newbie", "machineIds": []string{"m-1"}, "admin": false, "password": "newbie-password",
	}, cookie, csrf)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST users = %d, body %s", response.StatusCode, data)
	}
	if _, err := api.DecodeOKResponse(data); err != nil {
		t.Fatalf("decode ok: %v", err)
	}
	created, err := harness.users.Get(context.Background(), "newbie")
	if err != nil {
		t.Fatalf("created user: %v", err)
	}
	if created.PasswordHash != "test:newbie-password" || created.Admin || created.Scope.All {
		t.Fatalf("created row = %+v", created)
	}
	if _, _, admin := harness.login(t, "newbie", "newbie-password"); admin {
		t.Fatal("new user is an administrator")
	}
}

func TestAdminUserCSRFMatrix(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
	}, nil)
	cookie, csrf, _ := harness.login(t, "owner", "owner-password")
	body := map[string]any{"username": "csrf-user", "machineIds": "*", "admin": false, "password": "password"}
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"missing csrf", map[string]string{"Cookie": cookie}},
		{"wrong csrf", map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf + "0", "Origin": harness.origin}},
		{"wrong origin", map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf, "Origin": "http://evil.invalid"}},
		{"missing origin", map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf}},
	}
	for _, test := range cases {
		response, data := harness.request(t, http.MethodPost, usersPath, body, test.headers)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s = %d", test.name, response.StatusCode)
		}
		if message := decodeError(t, data); message != "missing or invalid csrf token" {
			t.Fatalf("%s message = %q", test.name, message)
		}
	}
	if _, err := harness.users.Get(context.Background(), "csrf-user"); storage.CodeOf(err) != storage.CodeNotFound {
		t.Fatalf("CSRF failure created a user: %v", err)
	}
}

func TestAdminUserValidation(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
	}, nil)
	cookie, csrf, _ := harness.login(t, "owner", "owner-password")
	valid := map[string]any{"username": "valid", "machineIds": "*", "admin": false, "password": "password"}
	for _, body := range []any{
		withField(valid, "extra", true),
		withField(valid, "machineIds", []string{"dup", "dup"}),
		withField(valid, "username", "bad name"),
		withField(valid, "admin", "yes"),
		withField(valid, "password", ""),
	} {
		response, _ := harness.adminRequest(t, http.MethodPost, usersPath, body, cookie, csrf)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %v = %d", body, response.StatusCode)
		}
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, usersPath, "{not json", cookie, csrf); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed = %d", response.StatusCode)
	}
	response, _ := harness.request(t, http.MethodPost, usersPath,
		map[string]any{"username": "big", "machineIds": "*", "admin": false, "password": strings.Repeat("x", 9000)},
		map[string]string{"Cookie": cookie, "X-CSRF-Token": csrf, "Origin": harness.origin})
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized = %d", response.StatusCode)
	}
	response, _ = harness.request(t, http.MethodPost, usersPath, "{}", map[string]string{
		"Cookie": cookie, "X-CSRF-Token": csrf, "Origin": harness.origin, "Content-Type": "text/plain",
	})
	if response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodPut, usersPath, nil, cookie, ""); response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PUT users = %d", response.StatusCode)
	}
}

func withField(body map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	out[key] = value
	return out
}

func TestAdminLastAdministrator(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
	}, nil)
	cookie, csrf, _ := harness.login(t, "owner", "owner-password")
	response, _ := harness.adminRequest(t, http.MethodDelete, usersPath+"?username=owner", nil, cookie, csrf)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("delete last admin = %d", response.StatusCode)
	}
	// A failed account mutation must roll back the revocation too: the actor's
	// own session and the stored rows are untouched.
	if response, _ := harness.adminRequest(t, http.MethodGet, usersPath, nil, cookie, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("failed mutation revoked the actor's session: %d", response.StatusCode)
	}
	count, err := storage.NewSessions(harness.db).Count(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("failed mutation ended sessions: %d, %v", count, err)
	}
	response, _ = harness.adminRequest(t, http.MethodPost, usersPath, map[string]any{
		"username": "second", "machineIds": "*", "admin": true, "password": "second-password",
	}, cookie, csrf)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("create second = %d", response.StatusCode)
	}
	response, _ = harness.adminRequest(t, http.MethodDelete, usersPath+"?username=second", nil, cookie, csrf)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("delete second = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodDelete, usersPath+"?username=missing", nil, cookie, csrf); response.StatusCode != http.StatusNotFound {
		t.Fatalf("delete unknown = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodDelete, usersPath, nil, cookie, csrf); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete without username = %d", response.StatusCode)
	}
}

func TestAdminAudit(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
	}, nil)
	cookie, csrf, _ := harness.login(t, "owner", "owner-password")
	if response, _ := harness.adminRequest(t, http.MethodPost, usersPath, map[string]any{
		"username": "audited", "machineIds": "*", "admin": false, "password": "audited-password",
	}, cookie, csrf); response.StatusCode != http.StatusOK {
		t.Fatalf("create = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodDelete, usersPath+"?username=audited", nil, cookie, csrf); response.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", response.StatusCode)
	}
	response, data := harness.adminRequest(t, http.MethodGet, auditPath+"?limit=10", nil, cookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("audit = %d, body %s", response.StatusCode, data)
	}
	list, err := api.DecodeAuditEventList(data)
	if err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	if len(list.Events) < 2 || list.Events[0].Action != "user.delete" || list.Events[1].Action != "user.create" {
		t.Fatalf("events = %+v", list.Events)
	}
	if list.Events[0].Target != "audited" || list.Events[1].Target != "audited" {
		t.Fatalf("events = %+v", list.Events)
	}
	if strings.Contains(string(data), "audited-password") {
		t.Fatal("audit leaked a password")
	}
	for _, query := range []string{"?limit=0", "?limit=abc", "?limit=1&limit=2"} {
		if response, _ := harness.adminRequest(t, http.MethodGet, auditPath+query, nil, cookie, ""); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("audit %s = %d", query, response.StatusCode)
		}
	}
	if response, _ := harness.adminRequest(t, http.MethodGet, auditPath+"?limit=5000", nil, cookie, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("audit clamp = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, auditPath, nil, cookie, ""); response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST audit = %d", response.StatusCode)
	}
}

func TestAdminCollectors(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "scoped", PasswordHash: "test:scoped-password", Scope: storage.MachineScope("m-1"), Admin: true},
	}, []storage.Identity{
		{CollectorID: "c-1", MachineID: "m-1", Token: "token-1"},
		{CollectorID: "c-2", MachineID: "m-2", Token: "token-2"},
	})
	ownerCookie, ownerCSRF, _ := harness.login(t, "owner", "owner-password")
	response, data := harness.adminRequest(t, http.MethodGet, collectorsPath, nil, ownerCookie, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("collectors = %d, body %s", response.StatusCode, data)
	}
	list, err := api.DecodeCollectorList(data)
	if err != nil {
		t.Fatalf("decode collectors: %v", err)
	}
	if len(list.Collectors) != 2 {
		t.Fatalf("collectors = %+v", list.Collectors)
	}
	if strings.Contains(string(data), "token-1") {
		t.Fatal("collector credential leaked")
	}

	scopedCookie, scopedCSRF, _ := harness.login(t, "scoped", "scoped-password")
	response, data = harness.adminRequest(t, http.MethodGet, collectorsPath, nil, scopedCookie, "")
	list, err = api.DecodeCollectorList(data)
	if err != nil || len(list.Collectors) != 1 || list.Collectors[0].CollectorID != "c-1" {
		t.Fatalf("scoped collectors = %+v, %v", list.Collectors, err)
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, collectorsPath,
		map[string]any{"action": "revoke", "collectorId": "c-2"}, scopedCookie, scopedCSRF); response.StatusCode != http.StatusForbidden {
		t.Fatalf("out-of-scope revoke = %d", response.StatusCode)
	}
	response, data = harness.adminRequest(t, http.MethodPost, collectorsPath,
		map[string]any{"action": "revoke", "collectorId": "c-1"}, ownerCookie, string(ownerCSRF))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d, body %s", response.StatusCode, data)
	}
	if _, err := api.DecodeOKResponse(data); err != nil {
		t.Fatalf("decode ok: %v", err)
	}
	if harness.countRevoked() != 1 || harness.revokedCollectors()[0] != "c-1" {
		t.Fatalf("revoke callback = %v", harness.revokedCollectors())
	}
	collectors, err := harness.registry.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, collector := range collectors {
		if collector.CollectorID == "c-1" && !collector.Revoked {
			t.Fatal("collector was not marked revoked")
		}
	}
	// Collector administration reports its own CSRF message.
	response, data = harness.request(t, http.MethodPost, collectorsPath,
		map[string]any{"action": "revoke", "collectorId": "c-2"},
		map[string]string{"Cookie": ownerCookie, "Origin": harness.origin})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("missing csrf = %d", response.StatusCode)
	}
	if message := decodeError(t, data); message != "origin or csrf token not allowed" {
		t.Fatalf("message = %q", message)
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, collectorsPath,
		map[string]any{"action": "delete", "collectorId": "c-2"}, ownerCookie, ownerCSRF); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown action = %d", response.StatusCode)
	}

	// Remove is refused for an active collector and deletes a revoked one,
	// history first.
	if response, _ := harness.adminRequest(t, http.MethodPost, collectorsPath,
		map[string]any{"action": "remove", "collectorId": "c-2"}, ownerCookie, ownerCSRF); response.StatusCode != http.StatusConflict {
		t.Fatalf("remove active = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, collectorsPath,
		map[string]any{"action": "remove", "collectorId": "c-1"}, scopedCookie, scopedCSRF); response.StatusCode != http.StatusOK {
		t.Fatalf("scoped remove of its revoked machine = %d", response.StatusCode)
	}
	harness.mu.Lock()
	removed := append([]string(nil), harness.removed...)
	harness.mu.Unlock()
	if len(removed) != 1 || removed[0] != "m-1" {
		t.Fatalf("history removal callback = %v", removed)
	}
	collectors, err = harness.registry.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(collectors) != 1 || collectors[0].CollectorID != "c-2" {
		t.Fatalf("collectors after remove = %+v", collectors)
	}
}

func TestAdminCollectorGrants(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "scoped", PasswordHash: "test:scoped-password", Scope: storage.MachineScope("m-1"), Admin: true},
	}, []storage.Identity{
		{CollectorID: "c-1", MachineID: "m-1", Token: "token-1"},
	})
	ownerCookie, ownerCSRF, _ := harness.login(t, "owner", "owner-password")
	response, data := harness.adminRequest(t, http.MethodPost, collectorGrants, map[string]any{}, ownerCookie, ownerCSRF)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("grant = %d, body %s", response.StatusCode, data)
	}
	grant, err := api.DecodeGrantResponse(data)
	if err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if grant.Token == "" || grant.ExpiresAt == "" {
		t.Fatalf("grant = %+v", grant)
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, collectorGrants, map[string]any{"ttlMs": 1000}, ownerCookie, ownerCSRF); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("short ttl = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, collectorGrants, map[string]any{"replaceMachineId": "m-1"}, ownerCookie, ownerCSRF); response.StatusCode != http.StatusCreated {
		t.Fatalf("replacement grant = %d", response.StatusCode)
	}

	scopedCookie, scopedCSRF, _ := harness.login(t, "scoped", "scoped-password")
	if response, _ := harness.adminRequest(t, http.MethodPost, collectorGrants, map[string]any{}, scopedCookie, scopedCSRF); response.StatusCode != http.StatusForbidden {
		t.Fatalf("scoped unrestricted grant = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, collectorGrants, map[string]any{"replaceMachineId": "m-2"}, scopedCookie, scopedCSRF); response.StatusCode != http.StatusForbidden {
		t.Fatalf("scoped out-of-scope replacement = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, collectorGrants, map[string]any{"replaceMachineId": "m-1"}, scopedCookie, scopedCSRF); response.StatusCode != http.StatusCreated {
		t.Fatalf("scoped replacement = %d", response.StatusCode)
	}

	// Fill the grant budget, then expect a conflict rather than a token.
	for i := 0; i < storage.MaxEnrollmentGrants; i++ {
		if _, err := harness.registry.CreateGrant(context.Background(), "test", storage.GrantOptions{}); err != nil {
			if storage.CodeOf(err) != storage.CodeConflict {
				t.Fatalf("fill grant %d: %v", i, err)
			}
			break
		}
	}
	response, data = harness.adminRequest(t, http.MethodPost, collectorGrants, map[string]any{}, ownerCookie, ownerCSRF)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("exhausted grant budget = %d, body %s", response.StatusCode, data)
	}
}

func TestAdminMachineMeta(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "scoped", PasswordHash: "test:scoped-password", Scope: storage.MachineScope("m-1"), Admin: true},
		{Username: "viewer", PasswordHash: "test:viewer-password", Scope: storage.AllMachines()},
	}, []storage.Identity{{CollectorID: "c-1", MachineID: "m-1", Token: "token-1"}})
	ownerCookie, ownerCSRF, _ := harness.login(t, "owner", "owner-password")
	response, data := harness.adminRequest(t, http.MethodPost, machineMetaPath+"?machineId=m-1",
		map[string]any{"displayName": "Desk", "tags": []string{"lab", "prod"}}, ownerCookie, ownerCSRF)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("meta = %d, body %s", response.StatusCode, data)
	}
	meta, err := api.DecodeMachineMeta(data)
	if err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta.DisplayName == nil || *meta.DisplayName != "Desk" || len(meta.Tags) != 2 {
		t.Fatalf("meta = %+v", meta)
	}
	if len(harness.metaUpdates()) != 1 || harness.metaUpdates()[0] != "m-1" {
		t.Fatalf("meta callback = %v", harness.metaUpdates())
	}
	// Absent fields keep the stored value; an explicit null clears the name.
	if response, _ := harness.adminRequest(t, http.MethodPost, machineMetaPath+"?machineId=m-1",
		map[string]any{"tags": []string{"edge"}}, ownerCookie, ownerCSRF); response.StatusCode != http.StatusOK {
		t.Fatalf("tags-only update = %d", response.StatusCode)
	}
	response, data = harness.adminRequest(t, http.MethodPost, machineMetaPath+"?machineId=m-1",
		map[string]any{"displayName": nil}, ownerCookie, ownerCSRF)
	meta, err = api.DecodeMachineMeta(data)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("clear name = %d, %v", response.StatusCode, err)
	}
	if meta.DisplayName != nil || len(meta.Tags) != 1 || meta.Tags[0] != "edge" {
		t.Fatalf("cleared meta = %+v", meta)
	}
	if response, _ := harness.adminRequest(t, http.MethodGet, machineMetaPath+"?machineId=m-1", nil, ownerCookie, ""); response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET meta = %d", response.StatusCode)
	}
	if response, _ := harness.adminRequest(t, http.MethodPost, machineMetaPath+"?machineId=m-404",
		map[string]any{"tags": []string{}}, ownerCookie, ownerCSRF); response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown machine = %d", response.StatusCode)
	}
	scopedCookie, scopedCSRF, _ := harness.login(t, "scoped", "scoped-password")
	if response, _ := harness.adminRequest(t, http.MethodPost, machineMetaPath+"?machineId=m-2",
		map[string]any{"tags": []string{}}, scopedCookie, scopedCSRF); response.StatusCode != http.StatusForbidden {
		t.Fatalf("out-of-scope meta = %d", response.StatusCode)
	}
	viewerCookie, viewerCSRF, _ := harness.login(t, "viewer", "viewer-password")
	if response, _ := harness.adminRequest(t, http.MethodPost, machineMetaPath+"?machineId=m-1",
		map[string]any{"tags": []string{}}, viewerCookie, viewerCSRF); response.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin meta = %d", response.StatusCode)
	}
	if response, _ := harness.request(t, http.MethodPost, machineMetaPath+"?machineId=m-1",
		map[string]any{"tags": []string{}}, map[string]string{"Cookie": ownerCookie, "X-CSRF-Token": ownerCSRF}); response.StatusCode != http.StatusForbidden {
		t.Fatalf("meta without origin = %d", response.StatusCode)
	}
}

func TestAdminResponseDecoders(t *testing.T) {
	// Every success body is produced by the api encoders, which re-decode it;
	// this guards the wiring against a hand-built JSON response creeping in.
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
	}, []storage.Identity{{CollectorID: "c-1", MachineID: "m-1", Token: "token-1"}})
	cookie, csrf, _ := harness.login(t, "owner", "owner-password")
	checks := []struct {
		method string
		path   string
		body   any
		decode func([]byte) error
	}{
		{http.MethodGet, usersPath, nil, func(data []byte) error { _, err := api.DecodeAdminUserList(data); return err }},
		{http.MethodGet, auditPath, nil, func(data []byte) error { _, err := api.DecodeAuditEventList(data); return err }},
		{http.MethodGet, collectorsPath, nil, func(data []byte) error { _, err := api.DecodeCollectorList(data); return err }},
		{http.MethodPost, collectorGrants, map[string]any{}, func(data []byte) error { _, err := api.DecodeGrantResponse(data); return err }},
		{http.MethodPost, machineMetaPath + "?machineId=m-1", map[string]any{"displayName": "Desk"}, func(data []byte) error { _, err := api.DecodeMachineMeta(data); return err }},
	}
	for _, check := range checks {
		response, data := harness.adminRequest(t, check.method, check.path, check.body, cookie, csrf)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("%s %s = %d, body %s", check.method, check.path, response.StatusCode, data)
		}
		if err := check.decode(data); err != nil {
			t.Fatalf("%s %s response %s: %v", check.method, check.path, data, err)
		}
	}
}

func TestAdminContentTypeRule(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
	}, nil)
	cookie, csrf, _ := harness.login(t, "owner", "owner-password")
	headers := func(contentType string) map[string]string {
		return map[string]string{
			"Cookie": cookie, "X-CSRF-Token": csrf, "Origin": harness.origin, "Content-Type": contentType,
		}
	}
	body := map[string]any{"username": "ct-user", "machineIds": "*", "admin": false, "password": "password"}
	// The 1.0 media-type rule: the parsed media type is compared case
	// insensitively and parameters are allowed.
	response, data := harness.request(t, http.MethodPost, usersPath, body, headers("Application/JSON; charset=utf-8"))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("parameterised media type = %d, body %s", response.StatusCode, data)
	}
	// A prefix match would wrongly accept this; the media type must be exact.
	response, _ = harness.request(t, http.MethodPost, usersPath, body, headers("application/jsonx"))
	if response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("invalid media type = %d", response.StatusCode)
	}
}

func TestAdminMethodAllowHeaders(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
	}, nil)
	cookie, csrf, _ := harness.login(t, "owner", "owner-password")
	cases := []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodPut, collectorsPath, "GET, POST"},
		{http.MethodPut, collectorGrants, "POST"},
		{http.MethodGet, machineMetaPath, "POST"},
	}
	for _, test := range cases {
		response, data := harness.request(t, test.method, test.path, nil, map[string]string{
			"Cookie": cookie, "X-CSRF-Token": csrf, "Origin": harness.origin,
		})
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d", test.method, test.path, response.StatusCode)
		}
		if got := response.Header.Get("Allow"); got != test.allow {
			t.Fatalf("%s %s Allow = %q, want %q", test.method, test.path, got, test.allow)
		}
		if _, err := api.DecodeAPIError(data); err != nil {
			t.Fatalf("%s %s error body %q: %v", test.method, test.path, data, err)
		}
	}
}

// TestDemotedAdminCannotCompletePendingMutation pauses password hashing after
// the request's authorization and body read, demotes the actor durably, then
// lets the handler continue. The write transaction's own re-check must reject
// the mutation; nothing may be committed on stale privilege.
func TestDemotedAdminCannotCompletePendingMutation(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "actor", PasswordHash: "test:actor-password", Scope: storage.AllMachines(), Admin: true},
	}, nil)
	cookie, csrf, admin := harness.login(t, "actor", "actor-password")
	if !admin {
		t.Fatal("actor session is not administrative")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	harness.hashHook = func(password string) (string, error) {
		close(started)
		<-release
		return testHash(password)
	}
	type result struct {
		status int
		body   []byte
	}
	done := make(chan result, 1)
	go func() {
		response, data := harness.adminRequest(t, http.MethodPost, usersPath, map[string]any{
			"username": "late", "machineIds": "*", "admin": false, "password": "late-password",
		}, cookie, csrf)
		done <- result{response.StatusCode, data}
	}()
	<-started
	// The handler has already passed requireAdmin and its CSRF proof and is
	// hashing the password. Demote the actor before the write transaction.
	if err := harness.users.Update(context.Background(), "owner", storage.User{
		Username: "actor", Scope: storage.AllMachines(),
	}); err != nil {
		t.Fatalf("demote actor: %v", err)
	}
	close(release)
	got := <-done
	if got.status != http.StatusForbidden {
		t.Fatalf("pending mutation = %d, body %s", got.status, got.body)
	}
	if message := decodeError(t, got.body); message != "administrative access required" {
		t.Fatalf("message = %q", message)
	}
	if _, err := harness.users.Get(context.Background(), "late"); storage.CodeOf(err) != storage.CodeNotFound {
		t.Fatalf("demoted administrator created a user: %v", err)
	}
}

// TestRevokedAdminCannotCompletePendingMutation pauses a mutation after its
// body read, narrows and revokes the actor's originating session, then lets the
// handler continue. The same write transaction must reject the request instead
// of committing with the machine scope the session held when the request
// started.
func TestRevokedAdminCannotCompletePendingMutation(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "actor", PasswordHash: "test:actor-password", Scope: storage.AllMachines(), Admin: true},
	}, nil)
	cookie, csrf, admin := harness.login(t, "actor", "actor-password")
	if !admin {
		t.Fatal("actor session is not administrative")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	harness.hashHook = func(password string) (string, error) {
		close(started)
		<-release
		return testHash(password)
	}
	type result struct {
		status int
		body   []byte
	}
	done := make(chan result, 1)
	go func() {
		response, data := harness.adminRequest(t, http.MethodPost, usersPath, map[string]any{
			"username": "late", "machineIds": []string{"m-2"}, "admin": false, "password": "late-password",
		}, cookie, csrf)
		done <- result{response.StatusCode, data}
	}()
	<-started
	// The request has passed requireAdmin, the scope check and its CSRF proof.
	// Narrow the actor and delete every session of the account in one durable
	// transaction before the handler's write transaction runs.
	if _, err := harness.users.MutateAccount(context.Background(), "owner", "actor", func(tx *storage.Tx) error {
		return harness.users.UpdateTx(tx, "owner", storage.User{
			Username: "actor", Scope: storage.MachineScope("m-2"), Admin: true,
		})
	}); err != nil {
		t.Fatalf("narrow actor: %v", err)
	}
	close(release)
	got := <-done
	if got.status != http.StatusUnauthorized {
		t.Fatalf("pending mutation = %d, body %s", got.status, got.body)
	}
	if _, err := harness.users.Get(context.Background(), "late"); storage.CodeOf(err) != storage.CodeNotFound {
		t.Fatalf("revoked administrator created a user: %v", err)
	}
}

// TestAccountChangeDeletesSessionsBeforeResponse proves the account change and
// its revocation are one transaction: the durable rows are already gone when
// the success response is written, and the subscriber hears about them.
func TestAccountChangeDeletesSessionsBeforeResponse(t *testing.T) {
	harness := newAdminHarness(t, []storage.User{
		{Username: "owner", PasswordHash: "test:owner-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "bob", PasswordHash: "test:bob-password", Scope: storage.AllMachines()},
	}, nil)
	bobCookie, _, _ := harness.login(t, "bob", "bob-password")
	ownerCookie, ownerCSRF, _ := harness.login(t, "owner", "owner-password")
	// bob's session is live before the change (a non-admin still authenticates).
	if response, _ := harness.adminRequest(t, http.MethodGet, usersPath, nil, bobCookie, ""); response.StatusCode != http.StatusForbidden {
		t.Fatalf("bob before change = %d", response.StatusCode)
	}

	var mu sync.Mutex
	var events []auth.Revocation
	unsubscribe := harness.authManager.Subscribe(func(event auth.Revocation) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})
	defer unsubscribe()

	response, data := harness.adminRequest(t, http.MethodPost, usersPath, map[string]any{
		"username": "bob", "machineIds": "*", "admin": false, "disabled": true,
	}, ownerCookie, ownerCSRF)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("disable bob = %d, body %s", response.StatusCode, data)
	}
	count, err := storage.NewSessions(harness.db).Count(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("sessions after account change = %d, %v (want only the actor's)", count, err)
	}
	if response, _ := harness.adminRequest(t, http.MethodGet, usersPath, nil, bobCookie, ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled user's session survived: %d", response.StatusCode)
	}
	hash := auth.HashToken(strings.TrimPrefix(bobCookie, auth.SessionCookieName+"="))
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0].TokenHash != hash || events[0].Reason != auth.ReasonRevoked {
		t.Fatalf("revocation events = %+v, want %s", events, hash)
	}
}

func decodeError(t *testing.T, data []byte) string {
	t.Helper()
	body, err := api.DecodeAPIError(data)
	if err != nil {
		t.Fatalf("decode error %q: %v", data, err)
	}
	return body.Error
}
