package alerts

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

type httpCreds struct {
	cookie string
	csrf   string
}

type httpHarness struct {
	t         *testing.T
	server    *httptest.Server
	origin    string
	db        *storage.Database
	engine    *Engine
	handler   *Handler
	manager   *auth.Manager
	clock     *testClock
	authority SessionAuthority
	admin     httpCreds
	scoped    httpCreds
	viewer    httpCreds
}

func newHTTPHarness(t *testing.T) *httpHarness {
	t.Helper()
	ctx := context.Background()
	clock := newTestClock(time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC).UnixMilli())
	db, err := storage.Open(ctx, ":memory:", storage.Options{Now: clock.Now})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	users := storage.NewUsers(db)
	for _, user := range []storage.User{
		{Username: "admin", PasswordHash: "test:admin-password", Scope: storage.AllMachines(), Admin: true},
		{Username: "admin-scoped", PasswordHash: "test:scoped-password", Scope: storage.MachineScope("m1"), Admin: true},
		{Username: "viewer", PasswordHash: "test:viewer-password", Scope: storage.MachineScope("m1")},
	} {
		if err := users.Create(ctx, "test", user); err != nil {
			t.Fatalf("create user %q: %v", user.Username, err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	originRules, err := auth.NewOriginRules("", "127.0.0.1", port, false)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := auth.New(auth.Options{
		Users:    users,
		Sessions: storage.NewSessions(db),
		Origin:   originRules,
		TTL:      60 * time.Minute,
		Now:      clock.Now,
		VerifyPassword: func(password, encoded string) (bool, error) {
			return encoded == "test:"+password, nil
		},
		SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Options{Database: db, Now: clock.Now, DeliveryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(HTTPOptions{Engine: engine, Auth: manager})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	h := &httpHarness{t: t, server: server, origin: "http://" + server.Listener.Addr().String(),
		db: db, engine: engine, handler: handler, manager: manager, clock: clock, authority: testAuthority(t, db)}
	h.admin = h.login("admin", "admin-password", "203.0.113.1")
	h.scoped = h.login("admin-scoped", "scoped-password", "203.0.113.2")
	h.viewer = h.login("viewer", "viewer-password", "203.0.113.3")
	t.Cleanup(func() {
		engine.Stop()
		_ = manager.Close()
		_ = db.Close()
	})
	return h
}

func (h *httpHarness) login(username, password, ip string) httpCreds {
	h.t.Helper()
	_, token, ok, err := h.manager.Login(context.Background(), ip, username, password)
	if err != nil || !ok {
		h.t.Fatalf("login %q: ok=%v err=%v", username, ok, err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	session, err := h.manager.Session(request)
	if err != nil || session == nil {
		h.t.Fatalf("session for %q: %v", username, err)
	}
	return httpCreds{cookie: token, csrf: session.CSRFToken}
}

func (h *httpHarness) do(method, path, body string, creds httpCreds, mutate bool) *http.Response {
	h.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, h.origin+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if creds.cookie != "" {
		request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: creds.cookie})
	}
	if mutate {
		request.Header.Set("Origin", h.origin)
		request.Header.Set("X-CSRF-Token", creds.csrf)
	}
	response, err := h.server.Client().Do(request)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return response
}

func readJSON(t *testing.T, response *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return body
}

func decodeError(t *testing.T, response *http.Response) api.APIError {
	t.Helper()
	body := readJSON(t, response)
	decoded, err := api.DecodeAPIError(body)
	if err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	return decoded
}

func TestAlertsHTTPRequiresSessionAndFiltersByScope(t *testing.T) {
	h := newHTTPHarness(t)
	ctx := context.Background()
	for _, input := range []RuleInput{
		{Name: "m1 rule", MachineID: "m1", Metric: api.AlertMetricCPUUsage, Condition: api.AlertConditionAbove, Threshold: floatPtr(80)},
		{Name: "m2 rule", MachineID: "m2", Metric: api.AlertMetricCPUUsage, Condition: api.AlertConditionAbove, Threshold: floatPtr(80)},
		{Name: "m1 fire", MachineID: "m1", Metric: api.AlertMetricCPUUsage, Condition: api.AlertConditionAbove, Threshold: floatPtr(1)},
	} {
		if _, err := h.engine.CreateRule(ctx, h.authority, input); err != nil {
			t.Fatal(err)
		}
	}
	h.engine.ObserveAt("m1", metricsRecord(cpuPayload(99), 1), Live, h.clock.Now())
	if err := h.engine.ProcessNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.ConfigureWebhook(ctx, h.authority, WebhookConfig{
		URL: "http://127.0.0.1:9/sink", Secret: stringPtr("never-returned"),
	}); err != nil {
		t.Fatal(err)
	}

	unauthorized := h.do(http.MethodGet, "/api/alerts", "", httpCreds{}, false)
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.StatusCode)
	}
	if body := decodeError(t, unauthorized); body.Error != api.ErrorMessageAuthenticationRequired {
		t.Fatalf("error = %q", body.Error)
	}

	response := h.do(http.MethodGet, "/api/alerts", "", h.viewer, false)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	body := readJSON(t, response)
	list, err := api.DecodeAlertListResponse(body)
	if err != nil {
		t.Fatalf("contract decode: %v", err)
	}
	if list.CanAdmin {
		t.Fatal("viewer reported as admin")
	}
	for _, rule := range list.Rules {
		if rule.MachineID != "m1" {
			t.Fatalf("out-of-scope rule: %+v", rule)
		}
	}
	for _, incident := range list.Incidents {
		if incident.MachineID != "m1" {
			t.Fatalf("out-of-scope incident: %+v", incident)
		}
	}
	if !list.Webhook.Configured {
		t.Fatal("webhook not reported configured")
	}
	serialized := string(body)
	if strings.Contains(serialized, "http://127.0.0.1:9") || strings.Contains(serialized, "never-returned") {
		t.Fatalf("list leaks webhook private state: %s", serialized)
	}

	adminResponse := h.do(http.MethodGet, "/api/alerts", "", h.admin, false)
	adminBody := readJSON(t, adminResponse)
	adminList, err := api.DecodeAlertListResponse(adminBody)
	if err != nil || !adminList.CanAdmin {
		t.Fatalf("admin list = %+v, %v", adminList, err)
	}
}

func TestAlertsHTTPMutationsRequireAdminOriginCSRFAndScope(t *testing.T) {
	h := newHTTPHarness(t)
	body := `{"name":"Scoped rule","machineId":"m1","metric":"cpu.usage","condition":"above","threshold":90,"durationMs":0}`

	viewer := h.do(http.MethodPost, "/api/alerts/rules", body, h.viewer, true)
	if viewer.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer status = %d", viewer.StatusCode)
	}
	if got := decodeError(t, viewer); got.Error != api.ErrorMessageAdministrativeAccessRequired {
		t.Fatalf("viewer error = %q", got.Error)
	}

	outOfScope := h.do(http.MethodPost, "/api/alerts/rules",
		`{"name":"Scoped rule","machineId":"m2","metric":"cpu.usage","condition":"above","threshold":90,"durationMs":0}`,
		h.scoped, true)
	if outOfScope.StatusCode != http.StatusForbidden {
		t.Fatalf("out-of-scope status = %d", outOfScope.StatusCode)
	}

	badOriginRequest, _ := http.NewRequest(http.MethodPost, h.origin+"/api/alerts/rules", strings.NewReader(body))
	badOriginRequest.Header.Set("Content-Type", "application/json")
	badOriginRequest.Header.Set("Origin", "http://evil.example")
	badOriginRequest.Header.Set("X-CSRF-Token", h.admin.csrf)
	badOriginRequest.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.admin.cookie})
	badOrigin, err := h.server.Client().Do(badOriginRequest)
	if err != nil {
		t.Fatal(err)
	}
	if badOrigin.StatusCode != http.StatusForbidden {
		t.Fatalf("bad origin status = %d", badOrigin.StatusCode)
	}
	if got := decodeError(t, badOrigin); got.Error != api.ErrorMessageOriginNotAllowed {
		t.Fatalf("bad origin error = %q", got.Error)
	}

	missingCSRFRequest, _ := http.NewRequest(http.MethodPost, h.origin+"/api/alerts/rules", strings.NewReader(body))
	missingCSRFRequest.Header.Set("Content-Type", "application/json")
	missingCSRFRequest.Header.Set("Origin", h.origin)
	missingCSRFRequest.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.admin.cookie})
	missingCSRF, err := h.server.Client().Do(missingCSRFRequest)
	if err != nil {
		t.Fatal(err)
	}
	if missingCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("missing csrf status = %d", missingCSRF.StatusCode)
	}
	if got := decodeError(t, missingCSRF); got.Error != api.ErrorMessageMissingOrInvalidCSRF {
		t.Fatalf("missing csrf error = %q", got.Error)
	}

	unknownKey := h.do(http.MethodPost, "/api/alerts/rules",
		`{"name":"x","machineId":"m1","metric":"cpu.usage","condition":"above","threshold":90,"durationMs":0,"destinationUrl":"https://evil.example"}`,
		h.admin, true)
	if unknownKey.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown key status = %d", unknownKey.StatusCode)
	}

	created := h.do(http.MethodPost, "/api/alerts/rules", body, h.admin, true)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", created.StatusCode, readJSON(t, created))
	}
	createdBody := readJSON(t, created)
	decoded, err := api.DecodeAlertRuleResponse(createdBody)
	if err != nil {
		t.Fatalf("contract decode: %v", err)
	}
	if decoded.Rule.MachineID != "m1" {
		t.Fatalf("rule = %+v", decoded.Rule)
	}

	deleted := h.do(http.MethodDelete, "/api/alerts/rules?id="+decoded.Rule.ID, "", h.admin, true)
	if deleted.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", deleted.StatusCode)
	}
	if _, err := api.DecodeOKResponse(readJSON(t, deleted)); err != nil {
		t.Fatal(err)
	}
	missing := h.do(http.MethodDelete, "/api/alerts/rules?id="+decoded.Rule.ID, "", h.admin, true)
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing delete status = %d", missing.StatusCode)
	}
}

// TestAlertMutationsRecheckSessionInsideWriteTransaction revokes the session
// after the handler read its cookie but before the engine acquires the writer.
// The durable check inside the write transaction must answer 401 and persist
// nothing, including for webhook test sends.
func TestAlertMutationsRecheckSessionInsideWriteTransaction(t *testing.T) {
	h := newHTTPHarness(t)
	ctx := context.Background()
	body := `{"name":"Revoked rule","machineId":"m1","metric":"cpu.usage","condition":"above","threshold":90,"durationMs":0}`

	h.handler.testBeforeMutation = func() {
		if _, err := storage.NewSessions(h.db).Delete(ctx, auth.HashToken(h.admin.cookie)); err != nil {
			t.Errorf("delete admin session: %v", err)
		}
	}
	revoked := h.do(http.MethodPost, "/api/alerts/rules", body, h.admin, true)
	if revoked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked create status = %d, body = %s", revoked.StatusCode, readJSON(t, revoked))
	}
	rules, err := h.engine.ListRules(ctx, AllMachines())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 0 {
		t.Fatalf("revoked create persisted %d rules", len(rules))
	}

	// A stale authority (scope/admin changed after the cookie was minted) is
	// forbidden rather than persisted with the session's old scope.
	h = newHTTPHarness(t)
	h.handler.testBeforeMutation = func() {
		users := storage.NewUsers(h.db)
		if err := users.Update(ctx, "test", storage.User{
			Username: "admin-scoped", Scope: storage.AllMachines(), Admin: true,
		}); err != nil {
			t.Errorf("change scoped admin: %v", err)
		}
	}
	stale := h.do(http.MethodPost, "/api/alerts/rules",
		`{"name":"Stale rule","machineId":"m1","metric":"cpu.usage","condition":"above","threshold":90,"durationMs":0}`,
		h.scoped, true)
	if stale.StatusCode != http.StatusForbidden {
		t.Fatalf("stale authority status = %d, body = %s", stale.StatusCode, readJSON(t, stale))
	}
	rules, err = h.engine.ListRules(ctx, AllMachines())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 0 {
		t.Fatalf("stale create persisted %d rules", len(rules))
	}

	// The webhook test side effect is gated by a session check immediately
	// before the outbound request.
	h = newHTTPHarness(t)
	configure := h.do(http.MethodPost, "/api/alerts/webhook",
		`{"action":"configure","url":"http://127.0.0.1:9/sink"}`, h.admin, true)
	if configure.StatusCode != http.StatusOK {
		t.Fatalf("configure status = %d", configure.StatusCode)
	}
	_ = readJSON(t, configure)
	h.handler.testBeforeMutation = func() {
		if _, err := storage.NewSessions(h.db).Delete(ctx, auth.HashToken(h.admin.cookie)); err != nil {
			t.Errorf("delete webhook admin session: %v", err)
		}
	}
	tested := h.do(http.MethodPost, "/api/alerts/webhook", `{"action":"test"}`, h.admin, true)
	if tested.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked webhook test status = %d, body = %s", tested.StatusCode, readJSON(t, tested))
	}
}

// TestScopedAlertEngineWritesRefuseOutOfScopeMachines proves the engine rechecks
// the session's current scope itself, not only the HTTP pre-check.
func TestScopedAlertEngineWritesRefuseOutOfScopeMachines(t *testing.T) {
	h := newHTTPHarness(t)
	ctx := context.Background()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.scoped.cookie})
	session, err := h.manager.Session(request)
	if err != nil || session == nil {
		t.Fatalf("scoped session: %+v, %v", session, err)
	}
	authority := SessionAuthority{Actor: session.Username, TokenHash: session.TokenHash}

	if _, err := h.engine.CreateRule(ctx, authority, RuleInput{
		Name: "Out of scope", MachineID: "m2", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(90),
	}); !errors.Is(err, storage.ErrScopeForbidden) {
		t.Fatalf("out-of-scope create error = %v", err)
	}
	inside, err := h.engine.CreateRule(ctx, authority, RuleInput{
		Name: "In scope", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(90),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.UpdateRule(ctx, authority, inside.ID, RuleInput{
		Name: "Moved out", MachineID: "m2", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(90),
	}); !errors.Is(err, storage.ErrScopeForbidden) {
		t.Fatalf("out-of-scope update error = %v", err)
	}
}

func TestAlertsWebhookHTTPNeverReturnsPrivateState(t *testing.T) {
	h := newHTTPHarness(t)
	configured := h.do(http.MethodPost, "/api/alerts/webhook",
		`{"action":"configure","url":"http://127.0.0.1:9/sink","secret":"hidden-secret"}`,
		h.admin, true)
	if configured.StatusCode != http.StatusOK {
		t.Fatalf("configure status = %d body = %s", configured.StatusCode, readJSON(t, configured))
	}
	configuredBody := readJSON(t, configured)
	decoded, err := api.DecodeAlertWebhookResponse(configuredBody)
	if err != nil {
		t.Fatalf("contract decode: %v", err)
	}
	if !decoded.Status.Configured {
		t.Fatalf("status = %+v", decoded.Status)
	}
	if strings.Contains(string(configuredBody), "127.0.0.1") || strings.Contains(string(configuredBody), "hidden-secret") {
		t.Fatalf("configure response leaks private state: %s", configuredBody)
	}

	bad := h.do(http.MethodPost, "/api/alerts/webhook",
		`{"action":"configure","url":"http://example.com/sink"}`, h.admin, true)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad url status = %d", bad.StatusCode)
	}

	cleared := h.do(http.MethodPost, "/api/alerts/webhook", `{"action":"clear"}`, h.admin, true)
	if cleared.StatusCode != http.StatusOK {
		t.Fatalf("clear status = %d", cleared.StatusCode)
	}
	clearBody := readJSON(t, cleared)
	if _, err := api.DecodeAlertWebhookResponse(clearBody); err != nil {
		t.Fatal(err)
	}

	extra := h.do(http.MethodPost, "/api/alerts/webhook", `{"action":"test","url":"http://127.0.0.1:9"}`, h.admin, true)
	if extra.StatusCode != http.StatusBadRequest {
		t.Fatalf("extra field status = %d", extra.StatusCode)
	}

	// A test action on the configured loopback sink is an explicit delivery.
	if _, err := h.engine.ConfigureWebhook(context.Background(), h.authority, WebhookConfig{URL: "http://127.0.0.1:9/sink"}); err != nil {
		t.Fatal(err)
	}
	tested := h.do(http.MethodPost, "/api/alerts/webhook", `{"action":"test"}`, h.admin, true)
	if tested.StatusCode != http.StatusOK {
		t.Fatalf("test status = %d", tested.StatusCode)
	}
	testedBody := readJSON(t, tested)
	if _, err := api.DecodeAlertWebhookTestResponse(testedBody); err != nil {
		t.Fatalf("test contract decode: %v", err)
	}
}

func TestAlertsUnknownPathOrderingAndMethods(t *testing.T) {
	h := newHTTPHarness(t)
	nonAdmin := h.do(http.MethodGet, "/api/alerts/nope", "", h.viewer, true)
	if nonAdmin.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin unknown path status = %d", nonAdmin.StatusCode)
	}
	admin := h.do(http.MethodGet, "/api/alerts/nope", "", h.admin, true)
	if admin.StatusCode != http.StatusNotFound {
		t.Fatalf("admin unknown path status = %d", admin.StatusCode)
	}
	if got := decodeError(t, admin); got.Error != api.ErrorMessageUnknownAlertsEndpoint {
		t.Fatalf("unknown path error = %q", got.Error)
	}

	rootPost := h.do(http.MethodPost, "/api/alerts", "", h.viewer, true)
	if rootPost.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("root POST status = %d", rootPost.StatusCode)
	}
	if allow := rootPost.Header.Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q", allow)
	}

	rulesGet := h.do(http.MethodGet, "/api/alerts/rules", "", h.admin, true)
	if rulesGet.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("rules GET status = %d", rulesGet.StatusCode)
	}
	if allow := rulesGet.Header.Get("Allow"); allow != "POST, DELETE" {
		t.Fatalf("Allow = %q", allow)
	}

	webhookGet := h.do(http.MethodGet, "/api/alerts/webhook", "", h.admin, true)
	if webhookGet.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("webhook GET status = %d", webhookGet.StatusCode)
	}
	if allow := webhookGet.Header.Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q", allow)
	}
}

func TestAlertsLimitAndBodyValidation(t *testing.T) {
	h := newHTTPHarness(t)
	for _, query := range []string{"?limit=0", "?limit=501", "?limit=nope", "?limit="} {
		response := h.do(http.MethodGet, "/api/alerts"+query, "", h.viewer, false)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d", query, response.StatusCode)
		}
		if got := decodeError(t, response); got.Error != "limit must be an integer from 1 to 500" {
			t.Fatalf("%s error = %q", query, got.Error)
		}
	}
	ok := h.do(http.MethodGet, "/api/alerts?limit=1", "", h.viewer, false)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("limit=1 status = %d", ok.StatusCode)
	}

	nonJSON, _ := http.NewRequest(http.MethodPost, h.origin+"/api/alerts/rules", strings.NewReader(`{"name":"x"}`))
	nonJSON.Header.Set("Content-Type", "text/plain")
	nonJSON.Header.Set("Origin", h.origin)
	nonJSON.Header.Set("X-CSRF-Token", h.admin.csrf)
	nonJSON.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: h.admin.cookie})
	response, err := h.server.Client().Do(nonJSON)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON status = %d", response.StatusCode)
	}

	tooLarge := h.do(http.MethodPost, "/api/alerts/rules",
		`{"name":"`+strings.Repeat("x", api.MaxAlertRuleBodyBytes)+`"}`, h.admin, true)
	if tooLarge.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d", tooLarge.StatusCode)
	}
}
