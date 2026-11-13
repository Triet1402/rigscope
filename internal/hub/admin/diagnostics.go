package admin

import (
	"net/http"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/auth"
)

// alertDiagnosticsRoute serves the admin-only alert evaluation loss report. It
// is a read: the session and durable admin flag are enough, no CSRF or Origin
// proof is required. The payload never contains webhook private state.
//
// Machine ids are filtered through the authenticated session's scope so a
// scoped administrator cannot learn about machines outside it. Global counters
// remain visible because they leak no ids.
func (h *Handler) alertDiagnosticsRoute(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		auth.WriteError(w, http.StatusMethodNotAllowed, api.ErrorMessageGetRequired, http.MethodGet)
		return
	}
	if h.alertDiagnostics == nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "alert diagnostics temporarily unavailable", "")
		return
	}
	payload := h.alertDiagnostics()
	breaks := make([]string, 0, len(payload.UnresolvedBreaks))
	for _, machineID := range payload.UnresolvedBreaks {
		if session.Scope.Contains(machineID) {
			breaks = append(breaks, machineID)
		}
	}
	payload.UnresolvedBreaks = breaks
	body, err := api.EncodeAlertDiagnostics(payload)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "alert diagnostics temporarily unavailable", "")
		return
	}
	auth.WriteJSON(w, http.StatusOK, body)
}
