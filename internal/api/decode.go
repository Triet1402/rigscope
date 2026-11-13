package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// objectFields strictly decodes a JSON object into its raw fields. It rejects
// a non-object, unknown keys and missing required keys; optional keys may be
// absent. The caller decodes each value with the matching typed helper.
func objectFields(data []byte, what string, required, optional []string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("%s: expected a JSON object: %w", what, err)
	}
	if fields == nil {
		return nil, fmt.Errorf("%s: expected a JSON object", what)
	}
	allowed := make(map[string]struct{}, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = struct{}{}
	}
	for _, key := range optional {
		allowed[key] = struct{}{}
	}
	for key := range fields {
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("%s: unexpected field %q", what, key)
		}
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("%s: missing field %q", what, key)
		}
	}
	return fields, nil
}

// decodeStrict decodes one JSON value with unknown fields rejected and no
// trailing data. Nested types with their own UnmarshalJSON keep their own
// strictness.
func decodeStrict[T any](data []byte, what string) (T, error) {
	var out T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return out, fmt.Errorf("%s: %w", what, err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return out, fmt.Errorf("%s: trailing data after the JSON value", what)
	}
	return out, nil
}

func rawString(data json.RawMessage, what string) (string, error) {
	if isNull(data) {
		return "", fmt.Errorf("%s must not be null", what)
	}
	return decodeStrict[string](data, what)
}

func rawBool(data json.RawMessage, what string) (bool, error) {
	if isNull(data) {
		return false, fmt.Errorf("%s must not be null", what)
	}
	return decodeStrict[bool](data, what)
}

func rawInt64(data json.RawMessage, what string) (int64, error) {
	if isNull(data) {
		return 0, fmt.Errorf("%s must not be null", what)
	}
	return decodeStrict[int64](data, what)
}

// nullableString decodes a required-but-nullable string field.
func nullableString(data json.RawMessage, what string) (*string, error) {
	if isNull(data) {
		return nil, nil
	}
	value, err := rawString(data, what)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func isNull(data json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}

// ---------------------------------------------------------------------------
// Response decoders
// ---------------------------------------------------------------------------

// decodeResponse decodes one response document after a reflection-driven shape
// check. The standard decoder silently accepts a missing field (zero value) or
// a JSON null for a non-nullable scalar, which would let a hub-side contract
// violation reach the browser. decodeResponse rejects both before decoding.
func decodeResponse[T any](data []byte, what string) (T, error) {
	var zero T
	if err := validateResponseShape(bytes.TrimSpace(data), reflect.TypeOf(zero), what); err != nil {
		return zero, err
	}
	return decodeStrict[T](data, what)
}

// optionalType reports whether t is protocol.Optional[T], whose JSON value may
// be absent, null or T.
func optionalType(t reflect.Type) (reflect.Type, bool) {
	if t.Kind() == reflect.Pointer {
		return optionalType(t.Elem())
	}
	if t.Kind() != reflect.Struct || t.PkgPath() != "github.com/Triet1402/rigscope/internal/protocol" || !strings.HasPrefix(t.Name(), "Optional[") {
		return nil, false
	}
	return t.Field(1).Type, true // Value
}

// validateResponseShape enforces required-key presence and nullability for the
// response struct graph. Protocol package types keep their own strict parsers
// and are not re-walked here.
func validateResponseShape(data []byte, t reflect.Type, path string) error {
	if t == nil {
		return nil
	}
	if t.Kind() == reflect.Pointer {
		if isNull(data) {
			return fmt.Errorf("%s must not be null", path)
		}
		return validateResponseShape(data, t.Elem(), path)
	}
	if t.PkgPath() == "github.com/Triet1402/rigscope/internal/protocol" {
		return nil
	}
	if t.PkgPath() == "github.com/Triet1402/rigscope/internal/api" && t.Name() == "MachineScope" {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		fields, err := jsonObject(data, path)
		if err != nil {
			return err
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name, optional := jsonField(field)
			if name == "" || name == "-" {
				continue
			}
			raw, present := fields[name]
			if !present {
				if !optional {
					return fmt.Errorf("%s: missing field %q", path, name)
				}
				continue
			}
			if err := validateShapeField(raw, field.Type, optional, path+"."+name); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		if isNull(data) {
			return fmt.Errorf("%s must be an array, not null", path)
		}
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return fmt.Errorf("%s: expected an array: %w", path, err)
		}
		for i, item := range items {
			if err := validateResponseShape(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if isNull(data) {
			return fmt.Errorf("%s must be an object, not null", path)
		}
		var items map[string]json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return fmt.Errorf("%s: expected an object: %w", path, err)
		}
		for key, item := range items {
			if err := validateResponseShape(item, t.Elem(), path+"."+key); err != nil {
				return err
			}
		}
		return nil
	default:
		if isNull(data) {
			return fmt.Errorf("%s must not be null", path)
		}
		return nil
	}
}

// validateShapeField applies field-level nullability: a required pointer is a
// nullable field; an optional non-Optional pointer is not nullable; and a
// protocol.Optional pointer may be absent or null.
func validateShapeField(data json.RawMessage, t reflect.Type, optional bool, path string) error {
	if elem, ok := optionalType(t); ok {
		if isNull(data) {
			return nil
		}
		return validateResponseShape(data, elem, path)
	}
	if t.Kind() == reflect.Pointer {
		if isNull(data) {
			if optional {
				return fmt.Errorf("%s must not be null", path)
			}
			return nil
		}
		return validateResponseShape(data, t.Elem(), path)
	}
	if isNull(data) {
		return fmt.Errorf("%s must not be null", path)
	}
	return validateResponseShape(data, t, path)
}

func jsonObject(data []byte, path string) (map[string]json.RawMessage, error) {
	if isNull(data) {
		return nil, fmt.Errorf("%s must not be null", path)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("%s: expected a JSON object: %w", path, err)
	}
	if fields == nil {
		return nil, fmt.Errorf("%s: expected a JSON object", path)
	}
	return fields, nil
}

// jsonField reads a struct field's JSON name and whether it is optional.
func jsonField(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("json")
	if tag == "" {
		return field.Name, false
	}
	parts := bytes.Split([]byte(tag), []byte(","))
	name := string(parts[0])
	optional := false
	for _, part := range parts[1:] {
		if string(part) == "omitempty" {
			optional = true
		}
	}
	return name, optional
}

// DecodeHealthResponse strictly decodes GET /api/health.
func DecodeHealthResponse(data []byte) (HealthResponse, error) {
	out, err := decodeResponse[HealthResponse](data, "health response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeSessionResponse strictly decodes a session payload.
func DecodeSessionResponse(data []byte) (SessionResponse, error) {
	out, err := decodeResponse[SessionResponse](data, "session response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAPIError strictly decodes an error body.
func DecodeAPIError(data []byte) (APIError, error) {
	out, err := decodeResponse[APIError](data, "error response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeOKResponse strictly decodes the shared {"ok":true} body.
func DecodeOKResponse(data []byte) (OKResponse, error) {
	out, err := decodeResponse[OKResponse](data, "ok response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeEnrollmentIdentity strictly decodes a successful enrollment.
func DecodeEnrollmentIdentity(data []byte) (EnrollmentIdentity, error) {
	out, err := decodeResponse[EnrollmentIdentity](data, "enrollment identity")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAdminUserList strictly decodes GET /api/admin/users.
func DecodeAdminUserList(data []byte) (AdminUserList, error) {
	out, err := decodeResponse[AdminUserList](data, "admin user list")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAuditEventList strictly decodes GET /api/admin/audit.
func DecodeAuditEventList(data []byte) (AuditEventList, error) {
	out, err := decodeResponse[AuditEventList](data, "audit event list")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeCollectorList strictly decodes GET /api/admin/collectors.
func DecodeCollectorList(data []byte) (CollectorList, error) {
	out, err := decodeResponse[CollectorList](data, "collector list")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeGrantResponse strictly decodes POST /api/admin/collector-grants.
func DecodeGrantResponse(data []byte) (GrantResponse, error) {
	out, err := decodeResponse[GrantResponse](data, "enrollment grant")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeMachineMeta strictly decodes the machine metadata response.
func DecodeMachineMeta(data []byte) (MachineMeta, error) {
	out, err := decodeResponse[MachineMeta](data, "machine metadata")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeCollectorRevokeRequest strictly decodes a collector revocation body.
func DecodeCollectorRevokeRequest(data []byte) (CollectorRevokeRequest, error) {
	if err := checkBodySize(data, MaxCollectorAdminBodyBytes); err != nil {
		return CollectorRevokeRequest{}, err
	}
	out, err := decodeStrict[CollectorRevokeRequest](data, "collector revoke request")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAlertListResponse strictly decodes GET /api/alerts.
func DecodeAlertListResponse(data []byte) (AlertListResponse, error) {
	out, err := decodeResponse[AlertListResponse](data, "alert list response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAlertRuleResponse strictly decodes the create/update rule response.
func DecodeAlertRuleResponse(data []byte) (AlertRuleResponse, error) {
	out, err := decodeResponse[AlertRuleResponse](data, "alert rule response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAlertWebhookResponse strictly decodes a configure/clear response.
func DecodeAlertWebhookResponse(data []byte) (AlertWebhookResponse, error) {
	out, err := decodeResponse[AlertWebhookResponse](data, "alert webhook response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAlertWebhookTestResponse strictly decodes a webhook test response.
func DecodeAlertWebhookTestResponse(data []byte) (AlertWebhookTestResponse, error) {
	out, err := decodeResponse[AlertWebhookTestResponse](data, "alert webhook test response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAlertDiagnostics strictly decodes GET /api/admin/diagnostics/alerts.
func DecodeAlertDiagnostics(data []byte) (AlertDiagnostics, error) {
	out, err := decodeResponse[AlertDiagnostics](data, "alert diagnostics response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeHistoryStatus strictly decodes GET /api/history/status.
func DecodeHistoryStatus(data []byte) (HistoryStatus, error) {
	out, err := decodeResponse[HistoryStatus](data, "history status")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeMetricCatalogResponse strictly decodes GET /api/history/metrics.
func DecodeMetricCatalogResponse(data []byte) (MetricCatalogResponse, error) {
	out, err := decodeResponse[MetricCatalogResponse](data, "metric catalog response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeHistoryResult strictly decodes GET /api/history.
func DecodeHistoryResult(data []byte) (HistoryResult, error) {
	out, err := decodeResponse[HistoryResult](data, "history result")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeProcessAcquireResponse strictly decodes POST /api/processes.
func DecodeProcessAcquireResponse(data []byte) (ProcessAcquireResponse, error) {
	out, err := decodeResponse[ProcessAcquireResponse](data, "process acquire response")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeProcessReadResponse strictly decodes GET /api/processes. A missing
// "result" key is an error; an explicit null is a valid loading/ended state.
func DecodeProcessReadResponse(data []byte) (ProcessReadResponse, error) {
	fields, err := objectFields(data, "process read response", []string{"state", "result"}, nil)
	if err != nil {
		return ProcessReadResponse{}, err
	}
	state, err := rawString(fields["state"], "state")
	if err != nil {
		return ProcessReadResponse{}, err
	}
	out := ProcessReadResponse{State: ProcessState(state)}
	if !isNull(fields["result"]) {
		var result protocol.DetailResult
		if err := json.Unmarshal(fields["result"], &result); err != nil {
			return ProcessReadResponse{}, fmt.Errorf("process result: %w", err)
		}
		out.Result = &result
	}
	return out, out.validate()
}

// DecodeHubSnapshot strictly decodes a live snapshot payload.
func DecodeHubSnapshot(data []byte) (HubSnapshot, error) {
	out, err := decodeResponse[HubSnapshot](data, "hub snapshot")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// ---------------------------------------------------------------------------
// Request body decoders
// ---------------------------------------------------------------------------

// DecodeLoginRequest strictly decodes a login body, enforcing the 4 KiB limit.
func DecodeLoginRequest(data []byte) (LoginRequest, error) {
	if err := checkBodySize(data, MaxLoginBodyBytes); err != nil {
		return LoginRequest{}, err
	}
	out, err := decodeStrict[LoginRequest](data, "login request")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeEnrollmentRequest strictly decodes an enrollment body, enforcing the
// 1 KiB limit.
func DecodeEnrollmentRequest(data []byte) (EnrollmentRequest, error) {
	if err := checkBodySize(data, MaxEnrollmentBodyBytes); err != nil {
		return EnrollmentRequest{}, err
	}
	out, err := decodeStrict[EnrollmentRequest](data, "enrollment request")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAdminUserInput strictly decodes a user upsert body, enforcing the
// 8 KiB limit.
func DecodeAdminUserInput(data []byte) (AdminUserInput, error) {
	if err := checkBodySize(data, MaxAdminUserBodyBytes); err != nil {
		return AdminUserInput{}, err
	}
	out, err := decodeStrict[AdminUserInput](data, "admin user input")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeGrantRequest strictly decodes a grant request, enforcing the 2 KiB
// limit.
func DecodeGrantRequest(data []byte) (GrantRequest, error) {
	if err := checkBodySize(data, MaxCollectorAdminBodyBytes); err != nil {
		return GrantRequest{}, err
	}
	out, err := decodeStrict[GrantRequest](data, "grant request")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeMachineMetaInput strictly decodes a machine metadata body, enforcing
// the 4 KiB limit.
func DecodeMachineMetaInput(data []byte) (MachineMetaInput, error) {
	if err := checkBodySize(data, MaxMachineMetaBodyBytes); err != nil {
		return MachineMetaInput{}, err
	}
	out, err := decodeStrict[MachineMetaInput](data, "machine metadata input")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAlertRuleInput strictly decodes a rule create/update body, enforcing
// the 16 KiB limit.
func DecodeAlertRuleInput(data []byte) (AlertRuleInput, error) {
	if err := checkBodySize(data, MaxAlertRuleBodyBytes); err != nil {
		return AlertRuleInput{}, err
	}
	out, err := decodeStrict[AlertRuleInput](data, "alert rule input")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// DecodeAlertWebhookRequest strictly decodes a webhook action body, enforcing
// the 8 KiB limit.
func DecodeAlertWebhookRequest(data []byte) (AlertWebhookRequest, error) {
	if err := checkBodySize(data, MaxAlertWebhookBodyBytes); err != nil {
		return AlertWebhookRequest{}, err
	}
	out, err := decodeStrict[AlertWebhookRequest](data, "alert webhook request")
	if err != nil {
		return out, err
	}
	return out, out.validate()
}

// RequireJSONContentType mirrors the 1.0 media-type rule: the parsed media type
// must be application/json, case-insensitively and with any parameters. A
// different type or a malformed header is a 415.
func RequireJSONContentType(contentType string) error {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return BodyError(http.StatusUnsupportedMediaType, "JSON required")
	}
	return nil
}

func checkBodySize(data []byte, maxBytes int) error {
	if len(data) > maxBytes {
		return sizeErr(maxBytes)
	}
	return nil
}
