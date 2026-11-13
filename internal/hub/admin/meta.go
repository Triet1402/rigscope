package admin

import (
	"net/http"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

func (h *Handler) machineMetaRoute(w http.ResponseWriter, r *http.Request) {
	if h.machines == nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "monitoring request temporarily unavailable", "")
		return
	}
	session, err := h.auth.Session(r)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "monitoring request temporarily unavailable", "")
		return
	}
	if session == nil {
		auth.WriteError(w, http.StatusUnauthorized, "authentication required", "")
		return
	}
	if origin := r.Header.Values("Origin"); len(origin) > 0 && !h.auth.Origin().Trusted(r) {
		auth.WriteError(w, http.StatusForbidden, "origin not allowed", "")
		return
	}
	if r.Method != http.MethodPost {
		auth.WriteError(w, http.StatusMethodNotAllowed, "POST required", http.MethodPost)
		return
	}
	if !auth.CSRFMatches(r, session.CSRFToken) || !h.auth.Origin().Trusted(r) {
		auth.WriteError(w, http.StatusForbidden, "missing or invalid csrf token", "")
		return
	}
	values := r.URL.Query()["machineId"]
	machineID := ""
	if len(values) == 1 {
		machineID = values[0]
	}
	if machineID == "" || len(machineID) > 128 || !includesMachine(session, machineID) {
		auth.WriteError(w, http.StatusForbidden, "machine access denied", "")
		return
	}
	exists, err := h.machineExists(r.Context(), machineID)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "monitoring request temporarily unavailable", "")
		return
	}
	if !exists {
		auth.WriteError(w, http.StatusNotFound, "machine not found", "")
		return
	}
	admin, err := h.auth.IsAdmin(r.Context(), session.Username)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "monitoring request temporarily unavailable", "")
		return
	}
	if !admin {
		auth.WriteError(w, http.StatusForbidden, "administrative access required", "")
		return
	}
	body, ok := h.readJSONBody(w, r, maxMachineMetaBody)
	if !ok {
		return
	}
	input, err := api.DecodeMachineMetaInput(body)
	if err != nil {
		auth.WriteError(w, http.StatusBadRequest, "invalid machine metadata", "")
		return
	}
	stored, err := h.machines.Get(r.Context(), machineID)
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "monitoring request temporarily unavailable", "")
		return
	}
	update := storage.MetaUpdate{}
	displayName := stored.DisplayName
	if input.DisplayName != nil {
		if input.DisplayName.Valid {
			value := input.DisplayName.Value
			displayName = &value
			update.DisplayName = &value
		} else {
			displayName = nil
			update.ClearDisplayName = true
		}
	}
	// Tags is a required, non-null list in the contract: never encode nil.
	tags := append([]string{}, stored.Tags...)
	if input.Tags != nil {
		tags = append([]string{}, (*input.Tags)...)
		update.Tags = &tags
	}
	// The acting administrator is re-checked inside the same write transaction
	// as the update, so a demotion that commits first cannot leave a committed
	// metadata change behind.
	if err := h.users.AuthorizeSession(r.Context(), session.Username, session.TokenHash, func(tx *storage.Tx) error {
		return h.machines.UpdateTx(tx, machineID, update)
	}); err != nil {
		switch storage.CodeOf(err) {
		case storage.CodeUnauthenticated:
			auth.WriteError(w, http.StatusUnauthorized, "authentication required", "")
		case storage.CodeForbidden:
			auth.WriteError(w, http.StatusForbidden, api.ErrorMessageAdministrativeAccessRequired, "")
		default:
			auth.WriteError(w, http.StatusServiceUnavailable, "monitoring request temporarily unavailable", "")
		}
		return
	}
	result, err := api.EncodeMachineMeta(api.MachineMeta{DisplayName: displayName, Tags: tags})
	if err != nil {
		auth.WriteError(w, http.StatusInternalServerError, "monitoring request temporarily unavailable", "")
		return
	}
	if h.onMachineMeta != nil {
		h.onMachineMeta(machineID)
	}
	auth.WriteJSON(w, http.StatusOK, result)
}
