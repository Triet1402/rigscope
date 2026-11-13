package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

func (h *Handler) collectorsRoute(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if h.registry == nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "collector administration temporarily unavailable", "")
		return
	}
	if r.URL.Path == collectorsPath && r.Method == http.MethodGet {
		h.listCollectors(w, r, session)
		return
	}
	if r.Method != http.MethodPost {
		allow := http.MethodPost
		if r.URL.Path == collectorsPath {
			allow = "GET, POST"
		}
		auth.WriteError(w, http.StatusMethodNotAllowed, "POST required", allow)
		return
	}
	if !h.mutationProof(w, r, session, "origin or csrf token not allowed") {
		return
	}
	if r.URL.Path == collectorGrants {
		h.createGrant(w, r, session)
		return
	}
	h.revokeCollector(w, r, session)
}

func (h *Handler) listCollectors(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	collectors, err := h.registry.List(r.Context())
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "collector administration temporarily unavailable", "")
		return
	}
	list := api.CollectorList{Collectors: make([]api.CollectorRecord, 0, len(collectors))}
	for _, collector := range collectors {
		if !includesMachine(session, collector.MachineID) {
			continue
		}
		list.Collectors = append(list.Collectors, api.CollectorRecord{
			CollectorID: collector.CollectorID,
			MachineID:   collector.MachineID,
			Revoked:     collector.Revoked,
			UpdatedAt:   isoTime(collector.UpdatedAt),
		})
	}
	body, err := api.EncodeCollectorList(list)
	if err != nil {
		auth.WriteError(w, http.StatusInternalServerError, "collector administration temporarily unavailable", "")
		return
	}
	auth.WriteJSON(w, http.StatusOK, body)
}

func (h *Handler) revokeCollector(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	body, ok := h.readJSONBody(w, r, maxCollectorBody)
	if !ok {
		return
	}
	request, err := api.DecodeCollectorRevokeRequest(body)
	if err != nil {
		auth.WriteError(w, http.StatusBadRequest, "explicit collector revocation required", "")
		return
	}
	collectors, err := h.registry.List(r.Context())
	if err != nil {
		auth.WriteError(w, http.StatusServiceUnavailable, "collector administration temporarily unavailable", "")
		return
	}
	found := false
	var target storage.Collector
	for _, collector := range collectors {
		if collector.CollectorID != request.CollectorID {
			continue
		}
		found = includesMachine(session, collector.MachineID)
		target = collector
		break
	}
	if !found {
		auth.WriteError(w, http.StatusForbidden, "machine access denied", "")
		return
	}
	if request.Action == "remove" {
		h.removeCollector(w, r, session, target)
		return
	}
	// Re-check the acting administrator inside the write transaction so a
	// demotion that commits first cannot leave a revoked collector behind.
	if err := h.users.AuthorizeSession(r.Context(), session.Username, session.TokenHash, func(tx *storage.Tx) error {
		return h.registry.RevokeTx(tx, session.Username, request.CollectorID)
	}); err != nil {
		h.collectorFailure(w, err)
		return
	}
	if h.onCollector != nil {
		h.onCollector(request.CollectorID)
	}
	auth.WriteOK(w)
}

// removeCollector deletes a revoked collector and everything stored for its
// machine. History goes first: if that fails the registry row stays, so the
// operator can retry, and a removed row never leaves orphaned history behind.
func (h *Handler) removeCollector(w http.ResponseWriter, r *http.Request, session *auth.Session, target storage.Collector) {
	if !target.Revoked {
		auth.WriteError(w, http.StatusConflict, storage.ErrNotRevoked.Error(), "")
		return
	}
	if h.onMachineRemoved != nil {
		if err := h.onMachineRemoved(r.Context(), target.MachineID); err != nil {
			h.logger.Warn("remove machine history", "machineId", target.MachineID, "error", err)
			auth.WriteError(w, http.StatusServiceUnavailable, "could not delete the machine's history; nothing was removed, try again", "")
			return
		}
	}
	if err := h.users.AuthorizeSession(r.Context(), session.Username, session.TokenHash, func(tx *storage.Tx) error {
		_, err := h.registry.RemoveTx(tx, session.Username, target.CollectorID)
		return err
	}); err != nil {
		if errors.Is(err, storage.ErrNotRevoked) {
			auth.WriteError(w, http.StatusConflict, storage.ErrNotRevoked.Error(), "")
			return
		}
		h.collectorFailure(w, err)
		return
	}
	auth.WriteOK(w)
}

func (h *Handler) createGrant(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	body, ok := h.readJSONBody(w, r, maxCollectorBody)
	if !ok {
		return
	}
	request, err := api.DecodeGrantRequest(body)
	if err != nil {
		auth.WriteError(w, http.StatusBadRequest, "invalid grant request", "")
		return
	}
	options := storage.GrantOptions{}
	if request.ReplaceMachineID != nil {
		if !includesMachine(session, *request.ReplaceMachineID) {
			auth.WriteError(w, http.StatusForbidden, "machine access denied", "")
			return
		}
		options.ReplaceMachineID = *request.ReplaceMachineID
	} else if !session.Scope.All {
		auth.WriteError(w, http.StatusForbidden, "machine access denied", "")
		return
	}
	if request.TTLMs != nil {
		options.TTL = time.Duration(*request.TTLMs) * time.Millisecond
	}
	var grant storage.EnrollmentGrant
	if err := h.users.AuthorizeSession(r.Context(), session.Username, session.TokenHash, func(tx *storage.Tx) error {
		issued, err := h.registry.CreateGrantTx(tx, session.Username, options)
		if err != nil {
			return err
		}
		grant = issued
		return nil
	}); err != nil {
		h.collectorFailure(w, err)
		return
	}
	response, err := api.EncodeGrantResponse(api.GrantResponse{
		Token:     grant.Token,
		ExpiresAt: isoTime(grant.ExpiresAt),
	})
	if err != nil {
		auth.WriteError(w, http.StatusInternalServerError, "collector administration temporarily unavailable", "")
		return
	}
	auth.WriteJSON(w, http.StatusCreated, response)
}

func (h *Handler) collectorFailure(w http.ResponseWriter, err error) {
	switch storage.CodeOf(err) {
	case storage.CodeUnauthenticated:
		auth.WriteError(w, http.StatusUnauthorized, "authentication required", "")
	case storage.CodeForbidden:
		auth.WriteError(w, http.StatusForbidden, api.ErrorMessageAdministrativeAccessRequired, "")
	case storage.CodeInvalid:
		auth.WriteError(w, http.StatusBadRequest, storageMessage(err), "")
	case storage.CodeConflict:
		auth.WriteError(w, http.StatusConflict, storageMessage(err), "")
	case storage.CodeNotFound:
		auth.WriteError(w, http.StatusNotFound, storageMessage(err), "")
	default:
		auth.WriteError(w, http.StatusServiceUnavailable, "collector administration temporarily unavailable", "")
	}
}
