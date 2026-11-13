package admin

import (
	"errors"
	"net/http"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/hub/auth"
	"github.com/Triet1402/rigscope/internal/hub/storage"
)

func (h *Handler) usersRoute(w http.ResponseWriter, r *http.Request) {
	session, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.listUsers(w, r)
	case http.MethodPost:
		if !h.mutationProof(w, r, session, api.ErrorMessageMissingOrInvalidCSRF) {
			return
		}
		h.upsertUser(w, r, session)
	case http.MethodDelete:
		if !h.mutationProof(w, r, session, api.ErrorMessageMissingOrInvalidCSRF) {
			return
		}
		h.deleteUser(w, r, session)
	default:
		auth.WriteError(w, http.StatusMethodNotAllowed, "method not allowed", "GET, POST, DELETE")
	}
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.users.List(r.Context())
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	list := api.AdminUserList{Users: make([]api.AdminUser, 0, len(users))}
	for _, user := range users {
		list.Users = append(list.Users, api.AdminUser{
			Username:   user.Username,
			MachineIDs: scopeFromStorage(user.Scope),
			Admin:      user.Admin,
			Disabled:   user.Disabled,
		})
	}
	body, err := api.EncodeAdminUserList(list)
	if err != nil {
		auth.WriteError(w, http.StatusInternalServerError, "administration temporarily unavailable", "")
		return
	}
	auth.WriteJSON(w, http.StatusOK, body)
}

func (h *Handler) upsertUser(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	body, ok := h.readJSONBody(w, r, maxAdminBodyBytes)
	if !ok {
		return
	}
	input, err := api.DecodeAdminUserInput(body)
	if err != nil {
		auth.WriteError(w, http.StatusBadRequest, "invalid user request", "")
		return
	}
	existing, getErr := h.users.Get(r.Context(), input.Username)
	if getErr != nil {
		if storage.CodeOf(getErr) != storage.CodeNotFound {
			h.adminFailure(w, getErr)
			return
		}
		existing = storage.User{}
	}
	hash := ""
	if input.Password != nil {
		if !h.acquireHashSlot() {
			auth.WriteError(w, http.StatusServiceUnavailable, "password hashing is busy; retry shortly", "")
			return
		}
		hash, err = h.hashPassword(*input.Password)
		h.releaseHashSlot()
		if err != nil {
			auth.WriteError(w, http.StatusBadRequest, "invalid user request", "")
			return
		}
	}
	disabled := existing.Disabled
	if input.Disabled != nil {
		disabled = *input.Disabled
	}
	if getErr != nil && input.Password == nil {
		auth.WriteError(w, http.StatusBadRequest, "a password is required for a new user", "")
		return
	}
	user := storage.User{
		Username:     input.Username,
		PasswordHash: hash,
		Scope:        scopeToStorage(input.MachineIDs),
		Admin:        input.Admin,
		Disabled:     disabled,
	}
	// The acting administrator is re-checked inside the same write transaction
	// (after the body read and password hashing) and every session of the
	// changed account is deleted there, so the change and its revocation commit
	// or roll back together.
	ended, err := h.users.MutateAccountSession(r.Context(), session.Username, session.TokenHash, input.Username, func(tx *storage.Tx) error {
		if getErr != nil {
			return h.users.CreateTx(tx, session.Username, user)
		}
		return h.users.UpdateTx(tx, session.Username, user)
	})
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	// An administrator editing their own account is signed out by this, as
	// documented.
	h.auth.EndSessions(ended)
	auth.WriteOK(w)
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request, session *auth.Session) {
	values := r.URL.Query()["username"]
	if len(values) != 1 {
		auth.WriteError(w, http.StatusBadRequest, "exactly one username is required", "")
		return
	}
	username := values[0]
	if !validIdentifier(username) {
		auth.WriteError(w, http.StatusBadRequest, "invalid identifier", "")
		return
	}
	ended, err := h.users.MutateAccountSession(r.Context(), session.Username, session.TokenHash, username, func(tx *storage.Tx) error {
		return h.users.DeleteTx(tx, session.Username, username)
	})
	if err != nil {
		if storage.CodeOf(err) == storage.CodeNotFound {
			auth.WriteError(w, http.StatusNotFound, "user not found", "")
			return
		}
		h.adminFailure(w, err)
		return
	}
	h.auth.EndSessions(ended)
	auth.WriteOK(w)
}

// adminFailure maps a storage classification to the administrative contract.
func (h *Handler) adminFailure(w http.ResponseWriter, err error) {
	switch storage.CodeOf(err) {
	case storage.CodeUnauthenticated:
		auth.WriteError(w, http.StatusUnauthorized, "authentication required", "")
	case storage.CodeForbidden:
		auth.WriteError(w, http.StatusForbidden, api.ErrorMessageAdministrativeAccessRequired, "")
	case storage.CodeInvalid:
		auth.WriteError(w, http.StatusBadRequest, "invalid user request", "")
	case storage.CodeConflict:
		auth.WriteError(w, http.StatusConflict, storageMessage(err), "")
	case storage.CodeNotFound:
		auth.WriteError(w, http.StatusNotFound, storageMessage(err), "")
	default:
		auth.WriteError(w, http.StatusServiceUnavailable, "administration is busy; retry shortly", "")
	}
}

func storageMessage(err error) string {
	var target *storage.Error
	if errors.As(err, &target) && target.Message != "" {
		return target.Message
	}
	return "administration temporarily unavailable"
}

func validIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
