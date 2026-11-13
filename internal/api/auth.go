package api

import (
	"fmt"
	"regexp"
)

// SessionResponse is returned by POST /api/auth/login and
// GET /api/auth/session. The session cookie itself is HttpOnly and never
// appears in the body. "admin" is present and true only for administrators;
// the Node hub omits it for ordinary accounts rather than sending false.
//
//api:response
type SessionResponse struct {
	Username  string `json:"username"`  //api:length=1..128
	CSRFToken string `json:"csrfToken"` //api:length=1..512
	Admin     *bool  `json:"admin,omitempty"`
}

func (s SessionResponse) validate() error {
	if err := validateText(s.Username, "username", 1, MaxUsernameLength); err != nil {
		return err
	}
	if s.CSRFToken == "" || utf16Len(s.CSRFToken) > 512 {
		return fmt.Errorf("csrfToken must be 1..512 characters")
	}
	if s.Admin != nil && !*s.Admin {
		return fmt.Errorf("admin must be true when present")
	}
	return nil
}

// LoginRequest is the POST /api/auth/login body. Exactly these two fields are
// accepted. The password is never echoed or logged.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// UnmarshalJSON requires both fields; the Node hub rejects a body whose sorted
// key set is not exactly password,username.
func (r *LoginRequest) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "login request", []string{"username", "password"}, nil)
	if err != nil {
		return err
	}
	var out LoginRequest
	if out.Username, err = rawString(fields["username"], "username"); err != nil {
		return err
	}
	if out.Password, err = rawString(fields["password"], "password"); err != nil {
		return err
	}
	*r = out
	return nil
}

// validate applies only the Node hub's pre-verification type and length
// checks. A username that is not printable ASCII is still sent through the
// uniform password-verification path and receives the same generic 401 as an
// unknown account; rejecting it early would leak which shapes exist.
func (r LoginRequest) validate() error {
	if utf16Len(r.Username) > MaxUsernameLength {
		return fmt.Errorf("username must be at most %d characters", MaxUsernameLength)
	}
	if utf16Len(r.Password) == 0 || utf16Len(r.Password) > MaxPasswordLength {
		return fmt.Errorf("password must be 1..%d characters", MaxPasswordLength)
	}
	return nil
}

// OKResponse is the shared {"ok": true} success body (logout, admin mutations,
// alert rule deletion, process-viewer release).
//
//api:response
type OKResponse struct {
	OK bool `json:"ok"`
}

func (r OKResponse) validate() error {
	if !r.OK {
		return fmt.Errorf("ok must be true")
	}
	return nil
}

// EnrollmentRequest is the POST /api/collector/enroll body. requestId and
// recoverySecret are present together only when an installed collector retries
// a lost enrollment response; the same grant then returns the same credential.
type EnrollmentRequest struct {
	CollectorID    string  `json:"collectorId"`
	RequestID      *string `json:"requestId,omitempty"`
	RecoverySecret *string `json:"recoverySecret,omitempty"`
}

// UnmarshalJSON requires collectorId and accepts the recovery pair only as
// strings; null is not a valid substitute for an absent optional field.
func (r *EnrollmentRequest) UnmarshalJSON(data []byte) error {
	fields, err := objectFields(data, "enrollment request",
		[]string{"collectorId"}, []string{"requestId", "recoverySecret"})
	if err != nil {
		return err
	}
	var out EnrollmentRequest
	if out.CollectorID, err = rawString(fields["collectorId"], "collectorId"); err != nil {
		return err
	}
	if raw, ok := fields["requestId"]; ok {
		value, err := rawString(raw, "requestId")
		if err != nil {
			return err
		}
		out.RequestID = &value
	}
	if raw, ok := fields["recoverySecret"]; ok {
		value, err := rawString(raw, "recoverySecret")
		if err != nil {
			return err
		}
		out.RecoverySecret = &value
	}
	*r = out
	return nil
}

func (r EnrollmentRequest) validate() error {
	if err := validateID(r.CollectorID, "collectorId"); err != nil {
		return err
	}
	if (r.RequestID == nil) != (r.RecoverySecret == nil) {
		return fmt.Errorf("requestId and recoverySecret must be present together")
	}
	if r.RequestID != nil {
		if err := validateID(*r.RequestID, "requestId"); err != nil {
			return err
		}
		if !recoverySecretPattern.MatchString(*r.RecoverySecret) {
			return fmt.Errorf("recoverySecret must be 32..128 base64url characters")
		}
	}
	return nil
}

var recoverySecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)

// EnrollmentIdentity is the 201 response to POST /api/collector/enroll. token
// is the collector's hub-specific bearer credential and must be treated as a
// secret; it never appears in a browser response.
//
//api:response
type EnrollmentIdentity struct {
	CollectorID string `json:"collectorId"`
	MachineID   string `json:"machineId"`
	Token       string `json:"token"` //api:length=1..256
}

func (i EnrollmentIdentity) validate() error {
	if err := validateID(i.CollectorID, "collectorId"); err != nil {
		return err
	}
	if err := validateID(i.MachineID, "machineId"); err != nil {
		return err
	}
	if printableASCII(i.Token, 1, 256) != nil {
		return fmt.Errorf("token must be 1..256 printable ASCII characters")
	}
	return nil
}

func printableASCII(value string, minLen, maxLen int) error {
	n := utf16Len(value)
	if n < minLen || n > maxLen {
		return fmt.Errorf("length %d outside %d..%d", n, minLen, maxLen)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return fmt.Errorf("must be printable ASCII without spaces")
		}
	}
	return nil
}
