package api

import (
	"encoding/json"
	"fmt"
)

// AdminUser is one row of GET /api/admin/users. disabled accounts keep their
// row but cannot sign in.
//
//api:response
type AdminUser struct {
	Username   string       `json:"username"`
	MachineIDs MachineScope `json:"machineIds"`
	Admin      bool         `json:"admin"`
	Disabled   bool         `json:"disabled"`
}

func (u AdminUser) validate() error {
	if err := validateID(u.Username, "username"); err != nil {
		return err
	}
	return u.MachineIDs.validate()
}

// AdminUserList is GET /api/admin/users.
//
//api:response
type AdminUserList struct {
	Users []AdminUser `json:"users"`
}

func (l AdminUserList) validate() error {
	if l.Users == nil {
		return fmt.Errorf("users must be an array")
	}
	seen := make(map[string]struct{}, len(l.Users))
	for _, user := range l.Users {
		if err := user.validate(); err != nil {
			return err
		}
		if _, duplicate := seen[user.Username]; duplicate {
			return fmt.Errorf("duplicate user %q", user.Username)
		}
		seen[user.Username] = struct{}{}
	}
	return nil
}

// AdminUserInput is the POST /api/admin/users body. username, machineIds and
// admin are required; disabled and password are optional. A present password
// is hashed by the hub and never returned.
type AdminUserInput struct {
	Username   string       `json:"username"`
	MachineIDs MachineScope `json:"machineIds"`
	Admin      bool         `json:"admin"`
	Disabled   *bool        `json:"disabled,omitempty"`
	Password   *string      `json:"password,omitempty"`
}

// UnmarshalJSON requires username, machineIds and admin. admin:false is a
// valid, present value, so presence is checked on the raw object.
func (u *AdminUserInput) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "admin user input",
		[]string{"username", "machineIds", "admin"}, []string{"disabled", "password"})
	if err != nil {
		return err
	}
	var out AdminUserInput
	if out.Username, err = rawString(fields["username"], "username"); err != nil {
		return err
	}
	if err := json.Unmarshal(fields["machineIds"], &out.MachineIDs); err != nil {
		return fmt.Errorf("machineIds: %w", err)
	}
	if out.Admin, err = rawBool(fields["admin"], "admin"); err != nil {
		return err
	}
	if raw, ok := fields["disabled"]; ok {
		value, err := rawBool(raw, "disabled")
		if err != nil {
			return err
		}
		out.Disabled = &value
	}
	if raw, ok := fields["password"]; ok {
		value, err := rawString(raw, "password")
		if err != nil {
			return err
		}
		out.Password = &value
	}
	*u = out
	return nil
}

func (u AdminUserInput) validate() error {
	if err := validateID(u.Username, "username"); err != nil {
		return err
	}
	if err := u.MachineIDs.validate(); err != nil {
		return err
	}
	if u.Password != nil {
		if n := utf16Len(*u.Password); n < 1 || n > MaxPasswordLength {
			return fmt.Errorf("password must be 1..%d characters", MaxPasswordLength)
		}
	}
	return nil
}

// AuditEvent is one administrative audit row. Audit rows never contain
// passwords, hashes or request bodies.
//
//api:response
type AuditEvent struct {
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	At     string `json:"at"`
}

func (e AuditEvent) validate() error {
	if err := validateText(e.Actor, "actor", 0, MaxIDLength); err != nil {
		return err
	}
	if err := validateText(e.Action, "action", 1, MaxIDLength); err != nil {
		return err
	}
	if err := validateText(e.Target, "target", 0, MaxResourceIDLength); err != nil {
		return err
	}
	return validateTimestamp(e.At, "at")
}

// AuditEventList is GET /api/admin/audit?limit=N, newest first.
//
//api:response
type AuditEventList struct {
	Events []AuditEvent `json:"events"`
}

func (l AuditEventList) validate() error {
	if l.Events == nil {
		return fmt.Errorf("events must be an array")
	}
	if len(l.Events) > MaxAuditLimit {
		return fmt.Errorf("events exceeds %d entries", MaxAuditLimit)
	}
	for _, event := range l.Events {
		if err := event.validate(); err != nil {
			return err
		}
	}
	return nil
}

// CollectorRecord is one enrolled collector as shown to an administrator. The
// collector's bearer credential is never part of this row.
//
//api:response
type CollectorRecord struct {
	CollectorID string `json:"collectorId"`
	MachineID   string `json:"machineId"`
	Revoked     bool   `json:"revoked"`
	UpdatedAt   string `json:"updatedAt"`
}

func (r CollectorRecord) validate() error {
	if err := validateID(r.CollectorID, "collectorId"); err != nil {
		return err
	}
	if err := validateID(r.MachineID, "machineId"); err != nil {
		return err
	}
	return validateTimestamp(r.UpdatedAt, "updatedAt")
}

// CollectorList is GET /api/admin/collectors, filtered to the caller's scope.
//
//api:response
type CollectorList struct {
	Collectors []CollectorRecord `json:"collectors"`
}

func (l CollectorList) validate() error {
	if l.Collectors == nil {
		return fmt.Errorf("collectors must be an array")
	}
	for _, record := range l.Collectors {
		if err := record.validate(); err != nil {
			return err
		}
	}
	return nil
}

// GrantRequest is the POST /api/admin/collector-grants body. Both fields are
// optional; an absent ttlMs means the hub default (15 minutes), bounded to
// 60_000..900_000 ms. replaceMachineId creates a replacement grant for an
// existing machine.
type GrantRequest struct {
	ReplaceMachineID *string `json:"replaceMachineId,omitempty"`
	TTLMs            *int64  `json:"ttlMs,omitempty"`
}

// UnmarshalJSON rejects explicit nulls for the optional fields, matching the
// reference handler's type checks.
func (r *GrantRequest) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "grant request", nil, []string{"replaceMachineId", "ttlMs"})
	if err != nil {
		return err
	}
	var out GrantRequest
	if raw, ok := fields["replaceMachineId"]; ok {
		value, err := rawString(raw, "replaceMachineId")
		if err != nil {
			return err
		}
		out.ReplaceMachineID = &value
	}
	if raw, ok := fields["ttlMs"]; ok {
		value, err := rawInt64(raw, "ttlMs")
		if err != nil {
			return err
		}
		out.TTLMs = &value
	}
	*r = out
	return nil
}

func (r GrantRequest) validate() error {
	if r.ReplaceMachineID != nil {
		if err := validateID(*r.ReplaceMachineID, "replaceMachineId"); err != nil {
			return err
		}
	}
	if r.TTLMs != nil {
		if *r.TTLMs < 60_000 || *r.TTLMs > 900_000 {
			return fmt.Errorf("ttlMs must be 60000..900000")
		}
	}
	return nil
}

// CollectorRevokeRequest is the POST /api/admin/collectors body. It has
// exactly one action on one collector; there is no bulk mutation. "revoke"
// disables the credential and keeps the machine and its history; "remove"
// deletes a revoked collector together with its machine's name, tags, alert
// rules, incidents and stored history.
type CollectorRevokeRequest struct {
	Action      string `json:"action"`
	CollectorID string `json:"collectorId"`
}

// UnmarshalJSON enforces exactly {"action":"revoke"|"remove","collectorId":"id"}.
func (r *CollectorRevokeRequest) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "collector revoke request",
		[]string{"action", "collectorId"}, nil)
	if err != nil {
		return err
	}
	var out CollectorRevokeRequest
	if out.Action, err = rawString(fields["action"], "action"); err != nil {
		return err
	}
	if out.CollectorID, err = rawString(fields["collectorId"], "collectorId"); err != nil {
		return err
	}
	if err := out.validate(); err != nil {
		return err
	}
	*r = out
	return nil
}

func (r CollectorRevokeRequest) validate() error {
	if r.Action != "revoke" && r.Action != "remove" {
		return fmt.Errorf("action must be %q or %q", "revoke", "remove")
	}
	return validateID(r.CollectorID, "collectorId")
}

// GrantResponse is the 201 response to POST /api/admin/collector-grants. The
// token is a short-lived, single-use bootstrap credential.
//
//api:response
type GrantResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

func (g GrantResponse) validate() error {
	if err := printableASCII(g.Token, 1, 256); err != nil {
		return fmt.Errorf("token: %w", err)
	}
	return validateTimestamp(g.ExpiresAt, "expiresAt")
}
