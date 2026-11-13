package admin

import (
	"net/http"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/auth"
)

// settingsRoute serves the read-only hub settings summary. Like the other
// admin reads it needs a session and the durable admin flag but no CSRF
// proof: it changes nothing. There is deliberately no write route. The hub
// runs unprivileged and cannot rewrite its configuration or restart itself;
// the dashboard hands a change to `rigscope configure` on the hub machine.
//
// The local collector's policy describes one machine, so an administrator
// scoped to particular machines does not receive it.
func (h *Handler) settingsRoute(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		auth.WriteError(w, http.StatusMethodNotAllowed, api.ErrorMessageGetRequired, http.MethodGet)
		return
	}
	if h.settings == nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "hub settings temporarily unavailable", "")
		return
	}
	payload, err := h.settings(r.Context())
	if err != nil {
		h.logger.Warn("read hub settings", "error", err)
		auth.WriteError(w, http.StatusServiceUnavailable, "hub settings temporarily unavailable", "")
		return
	}
	if !session.Scope.All {
		payload.LocalCollector = nil
	}
	body, err := api.EncodeHubSettings(payload)
	if err != nil {
		h.logger.Warn("encode hub settings", "error", err)
		auth.WriteError(w, http.StatusServiceUnavailable, "hub settings temporarily unavailable", "")
		return
	}
	auth.WriteJSON(w, http.StatusOK, body)
}
