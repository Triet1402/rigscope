package admin

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

var auditLimitPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

func (h *Handler) auditRoute(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if r.Method != http.MethodGet {
		auth.WriteError(w, http.StatusMethodNotAllowed, "GET required", "GET")
		return
	}
	values := r.URL.Query()["limit"]
	limit := defaultAuditLimit
	if len(values) > 1 {
		auth.WriteError(w, http.StatusBadRequest, "at most one limit is allowed", "")
		return
	}
	if len(values) == 1 {
		if !auditLimitPattern.MatchString(values[0]) {
			auth.WriteError(w, http.StatusBadRequest, "limit must be a positive integer", "")
			return
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 {
			auth.WriteError(w, http.StatusBadRequest, "limit must be a positive integer", "")
			return
		}
		limit = parsed
		if limit > maxAuditLimit {
			limit = maxAuditLimit
		}
	}
	adminEntries, err := h.audit.List(r.Context(), storage.AuditScopeAdmin, limit)
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	collectorEntries, err := h.audit.List(r.Context(), storage.AuditScopeCollector, maxAuditLimit)
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	entries := append(adminEntries, collectorEntries...)
	events := make([]api.AuditEvent, 0, len(entries))
	for _, entry := range entries {
		events = append(events, api.AuditEvent{
			Actor:  entry.Actor,
			Action: entry.Action,
			Target: entry.Target,
			At:     isoTime(entry.At),
		})
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].At > events[j].At
	})
	if len(events) > limit {
		events = events[:limit]
	}
	body, err := api.EncodeAuditEventList(api.AuditEventList{Events: events})
	if err != nil {
		auth.WriteError(w, http.StatusInternalServerError, "administration temporarily unavailable", "")
		return
	}
	auth.WriteJSON(w, http.StatusOK, body)
}
