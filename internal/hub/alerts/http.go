package alerts

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

const (
	alertsRoot    = "/api/alerts"
	alertsRules   = "/api/alerts/rules"
	alertsWebhook = "/api/alerts/webhook"
)

// HTTPOptions configures the alert HTTP surface.
type HTTPOptions struct {
	// Engine evaluates rules and owns the webhook channel. Required.
	Engine *Engine
	// Auth supplies browser session lookup, the durable admin check, trusted
	// origin rules and CSRF comparison. Required.
	Auth *auth.Manager
}

// Handler serves /api/alerts and its rules/webhook children.
type Handler struct {
	engine *Engine
	auth   *auth.Manager

	// testBeforeMutation runs after the handler's own session/admin/origin
	// checks and before the engine write. Tests use it to revoke the session in
	// the interval the write transaction must close.
	testBeforeMutation func()
}

// NewHandler returns the alert HTTP router. Mount it at /api/alerts through
// the hub composition root; the handler dispatches the exact paths itself.
func NewHandler(opts HTTPOptions) (*Handler, error) {
	if opts.Engine == nil {
		return nil, errors.New("alerts: engine is required")
	}
	if opts.Auth == nil {
		return nil, errors.New("alerts: authentication manager is required")
	}
	return &Handler{engine: opts.Engine, auth: opts.Auth}, nil
}

// ServeHTTP dispatches one alert request. Every request requires a session;
// mutations additionally require a durable administrator, an exact trusted
// Origin and the session CSRF token. An unknown path is discovered only after
// those checks, so a non-admin receives 403 before 404.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path != alertsRoot && !strings.HasPrefix(path, alertsRoot+"/") {
		auth.WriteError(w, http.StatusNotFound, "not found", "")
		return
	}
	session, err := h.auth.Session(r)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "authentication temporarily unavailable", "")
		return
	}
	if session == nil {
		auth.WriteError(w, http.StatusUnauthorized, api.ErrorMessageAuthenticationRequired, "")
		return
	}
	if path == alertsRoot {
		h.list(w, r, session)
		return
	}
	if !h.authorizedMutation(w, r, session) {
		return
	}
	if h.testBeforeMutation != nil {
		h.testBeforeMutation()
	}
	switch path {
	case alertsRules:
		h.rulesRoute(w, r, session)
	case alertsWebhook:
		h.webhookRoute(w, r, session)
	default:
		auth.WriteError(w, http.StatusNotFound, api.ErrorMessageUnknownAlertsEndpoint, "")
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	if r.Method != http.MethodGet {
		auth.WriteError(w, http.StatusMethodNotAllowed, api.ErrorMessageGetRequired, http.MethodGet)
		return
	}
	limit := DefaultListLimit
	if values, present := r.URL.Query()["limit"]; present {
		raw := ""
		if len(values) > 0 {
			raw = values[0]
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > MaxListLimit {
			auth.WriteError(w, http.StatusBadRequest, "limit must be an integer from 1 to 500", "")
			return
		}
		limit = parsed
	}
	canAdmin, err := h.auth.IsAdmin(r.Context(), session.Username)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "alerts request temporarily unavailable", "")
		return
	}
	response, err := h.engine.ListResponse(r.Context(), session.Scope, canAdmin, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	body, err := api.EncodeAlertListResponse(response)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "alerts request temporarily unavailable", "")
		return
	}
	auth.WriteJSON(w, http.StatusOK, body)
}

func (h *Handler) authorizedMutation(w http.ResponseWriter, r *http.Request, session *auth.Session) bool {
	admin, err := h.auth.IsAdmin(r.Context(), session.Username)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "alerts request temporarily unavailable", "")
		return false
	}
	if !admin {
		auth.WriteError(w, http.StatusForbidden, api.ErrorMessageAdministrativeAccessRequired, "")
		return false
	}
	if !h.auth.Origin().Trusted(r) {
		auth.WriteError(w, http.StatusForbidden, api.ErrorMessageOriginNotAllowed, "")
		return false
	}
	if !auth.CSRFMatches(r, session.CSRFToken) {
		auth.WriteError(w, http.StatusForbidden, api.ErrorMessageMissingOrInvalidCSRF, "")
		return false
	}
	return true
}

func (h *Handler) rulesRoute(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	switch r.Method {
	case http.MethodPost:
		h.upsertRule(w, r, session)
	case http.MethodDelete:
		h.deleteRule(w, r, session)
	default:
		auth.WriteError(w, http.StatusMethodNotAllowed, "POST or DELETE required", "POST, DELETE")
	}
}

func (h *Handler) upsertRule(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	if err := api.RequireJSONContentType(r.Header.Get("Content-Type")); err != nil {
		h.writeInputError(w, err, "JSON required")
		return
	}
	body, ok := h.readBody(w, r, api.MaxAlertRuleBodyBytes)
	if !ok {
		return
	}
	input, err := api.DecodeAlertRuleInput(body)
	if err != nil {
		h.writeInputError(w, err, "invalid alert rule body")
		return
	}
	if !session.Scope.Contains(input.MachineID) {
		auth.WriteError(w, http.StatusForbidden, "machine access denied", "")
		return
	}
	engineInput := RuleInputFromAPI(input)
	if input.ID != nil {
		existing, err := h.engine.GetRule(r.Context(), *input.ID)
		if err != nil {
			h.fail(w, err)
			return
		}
		if !session.Scope.Contains(existing.MachineID) {
			auth.WriteError(w, http.StatusForbidden, "machine access denied", "")
			return
		}
		rule, err := h.engine.UpdateRule(r.Context(), authority(session), *input.ID, engineInput)
		if err != nil {
			h.fail(w, err)
			return
		}
		h.writeRule(w, http.StatusOK, rule)
		return
	}
	rule, err := h.engine.CreateRule(r.Context(), authority(session), engineInput)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeRule(w, http.StatusCreated, rule)
}

func (h *Handler) deleteRule(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	values, present := r.URL.Query()["id"]
	if !present || len(values) == 0 || values[0] == "" || utf16Len(values[0]) > 128 {
		auth.WriteError(w, http.StatusBadRequest, "a bounded rule id is required", "")
		return
	}
	if len(values) != 1 {
		auth.WriteError(w, http.StatusBadRequest, "exactly one rule id is required", "")
		return
	}
	rule, err := h.engine.GetRule(r.Context(), values[0])
	if err != nil {
		h.fail(w, err)
		return
	}
	if !session.Scope.Contains(rule.MachineID) {
		auth.WriteError(w, http.StatusForbidden, "machine access denied", "")
		return
	}
	if err := h.engine.DeleteRule(r.Context(), authority(session), values[0]); err != nil {
		h.fail(w, err)
		return
	}
	body, err := api.EncodeOKResponse(api.OKResponse{OK: true})
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "alerts request temporarily unavailable", "")
		return
	}
	auth.WriteJSON(w, http.StatusOK, body)
}

func (h *Handler) webhookRoute(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	if r.Method != http.MethodPost {
		auth.WriteError(w, http.StatusMethodNotAllowed, api.ErrorMessagePostRequired, http.MethodPost)
		return
	}
	if err := api.RequireJSONContentType(r.Header.Get("Content-Type")); err != nil {
		h.writeInputError(w, err, "JSON required")
		return
	}
	body, ok := h.readBody(w, r, api.MaxAlertWebhookBodyBytes)
	if !ok {
		return
	}
	request, err := api.DecodeAlertWebhookRequest(body)
	if err != nil {
		h.writeInputError(w, err, "invalid webhook request")
		return
	}
	switch request.Action {
	case api.AlertWebhookActionConfigure:
		config := WebhookConfig{URL: *request.URL, Enabled: request.Enabled}
		if request.Headers != nil {
			config.Headers = *request.Headers
		}
		if request.Secret != nil && request.Secret.Valid {
			secret := request.Secret.Value
			config.Secret = &secret
		}
		status, err := h.engine.ConfigureWebhook(r.Context(), authority(session), config)
		if err != nil {
			h.fail(w, err)
			return
		}
		h.writeWebhook(w, status)
	case api.AlertWebhookActionClear:
		status, err := h.engine.ClearWebhook(r.Context(), authority(session))
		if err != nil {
			h.fail(w, err)
			return
		}
		h.writeWebhook(w, status)
	case api.AlertWebhookActionTest:
		result, err := h.engine.TestWebhook(r.Context(), authority(session))
		if err != nil {
			h.fail(w, err)
			return
		}
		status, err := h.engine.WebhookStatus(r.Context())
		if err != nil {
			h.fail(w, err)
			return
		}
		encoded, err := api.EncodeAlertWebhookTestResponse(api.AlertWebhookTestResponse{Result: result, Status: status})
		if err != nil {
			auth.WriteError(w, http.StatusServiceUnavailable, "alerts request temporarily unavailable", "")
			return
		}
		auth.WriteJSON(w, http.StatusOK, encoded)
	default:
		auth.WriteError(w, http.StatusBadRequest, "unknown webhook action", "")
	}
}

func authority(session *auth.Session) SessionAuthority {
	return SessionAuthority{Actor: session.Username, TokenHash: session.TokenHash}
}

func (h *Handler) writeRule(w http.ResponseWriter, status int, rule api.AlertRule) {
	body, err := api.EncodeAlertRuleResponse(api.AlertRuleResponse{Rule: rule})
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "alerts request temporarily unavailable", "")
		return
	}
	auth.WriteJSON(w, status, body)
}

func (h *Handler) writeWebhook(w http.ResponseWriter, status api.AlertWebhookStatus) {
	body, err := api.EncodeAlertWebhookResponse(api.AlertWebhookResponse{Status: status})
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "alerts request temporarily unavailable", "")
		return
	}
	auth.WriteJSON(w, http.StatusOK, body)
}

func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, maxBytes int) ([]byte, bool) {
	body, status, message := auth.DecodeJSON(r, maxBytes)
	if status == http.StatusRequestEntityTooLarge {
		auth.WriteError(w, status, "body too large", "")
		return nil, false
	}
	if status == http.StatusBadRequest {
		auth.WriteError(w, status, "invalid JSON body", "")
		return nil, false
	}
	if status != 0 {
		auth.WriteError(w, status, message, "")
		return nil, false
	}
	return body, true
}

func (h *Handler) writeInputError(w http.ResponseWriter, err error, fallback string) {
	var input *api.InputError
	if errors.As(err, &input) {
		message := input.Message
		if input.Status == http.StatusRequestEntityTooLarge {
			message = "body too large"
		}
		auth.WriteError(w, input.Status, message, "")
		return
	}
	auth.WriteError(w, http.StatusBadRequest, fallback, "")
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	switch storage.CodeOf(err) {
	case storage.CodeUnauthenticated:
		auth.WriteError(w, http.StatusUnauthorized, api.ErrorMessageAuthenticationRequired, "")
		return
	case storage.CodeForbidden:
		auth.WriteError(w, http.StatusForbidden, api.ErrorMessageAdministrativeAccessRequired, "")
		return
	}
	if errors.Is(err, storage.ErrScopeForbidden) {
		auth.WriteError(w, http.StatusForbidden, "machine access denied", "")
		return
	}
	var validation *ValidationError
	if errors.As(err, &validation) {
		auth.WriteError(w, http.StatusBadRequest, validation.Message, "")
		return
	}
	var missing *NotFoundError
	if errors.As(err, &missing) {
		auth.WriteError(w, http.StatusNotFound, "alert rule not found", "")
		return
	}
	auth.WriteError(w, http.StatusServiceUnavailable, "alerts request temporarily unavailable", "")
}
