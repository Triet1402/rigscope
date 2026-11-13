package api

import (
	"fmt"
	"net/http"
)

// APIError is the JSON error body every /api/* endpoint returns on failure.
// The browser reads the "error" string as a human-readable message. "code" is
// present only where the UI needs to distinguish a failure programmatically;
// today that is only "process-viewer-budget".
//
//api:response
type APIError struct {
	Error string  `json:"error"` //api:length=1..1024
	Code  *string `json:"code,omitempty"`
}

func (e APIError) validate() error {
	if e.Error == "" {
		return fmt.Errorf("api error: an error message is required")
	}
	if utf16Len(e.Error) > 1024 {
		return fmt.Errorf("api error: message exceeds 1024 characters")
	}
	if e.Code != nil && *e.Code == "" {
		return fmt.Errorf("api error: code must not be empty when present")
	}
	return nil
}

// ErrorCodeProcessViewerBudget marks a 429 process-viewer budget exhaustion.
const ErrorCodeProcessViewerBudget = "process-viewer-budget"

// Standard 403 and 405 messages. The 1.0 contract standardizes on these bodies;
// collector administration keeps its more specific origin/CSRF wording.
const (
	ErrorMessageAuthenticationRequired       = "authentication required"
	ErrorMessageAdministrativeAccessRequired = "administrative access required"
	ErrorMessageOriginNotAllowed             = "origin not allowed"
	ErrorMessageMissingOrInvalidCSRF         = "missing or invalid csrf token"
	ErrorMessageOriginOrCSRFNotAllowed       = "origin or csrf token not allowed"
	ErrorMessageJSONRequired                 = "JSON required"
	ErrorMessageGetRequired                  = "GET required"
	ErrorMessagePostRequired                 = "POST required"
	ErrorMessageUnknownAlertsEndpoint        = "unknown alerts endpoint"
)

// StandardAPIError builds the shared JSON error body for a standardized
// failure. Callers that need a machine-readable code use APIError directly.
func StandardAPIError(message string) APIError {
	return APIError{Error: message}
}

// InputError reports a rejected request body or query with the HTTP status the
// reference hub uses for that rejection.
//
//api:skip
type InputError struct {
	Status  int
	Message string
}

func (e *InputError) Error() string { return e.Message }

// BodyError is returned by the Decode*RequestBody functions for a body that
// cannot be accepted. It satisfies the same status contract as the Node hub:
// 413 for an oversized body, 400 for malformed or invalid JSON.
func BodyError(status int, message string) *InputError {
	return &InputError{Status: status, Message: message}
}

func inputErr(format string, args ...any) *InputError {
	return &InputError{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, args...)}
}

func sizeErr(maxBytes int) *InputError {
	return &InputError{Status: http.StatusRequestEntityTooLarge, Message: "request body too large"}
}
