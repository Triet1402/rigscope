package api

import (
	"encoding/json"
	"fmt"
)

// validatable is implemented by every exported contract type. Encoding checks
// it first so a typed-nil, missing list or out-of-range value is rejected on
// the hub side instead of reaching a browser.
type validatable interface {
	validate() error
}

// encodeChecked marshals v and re-decodes the bytes with decode, so an encoder
// can never emit a document its own strict decoder would reject.
func encodeChecked[T any](v T, decode func([]byte) (T, error)) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if _, err := decode(data); err != nil {
		return nil, fmt.Errorf("encoded value is invalid: %w", err)
	}
	return data, nil
}

// encodeValidated marshals v after validating it. Used for request bodies,
// which have no response decoder.
func encodeValidated[T validatable](v T) ([]byte, error) {
	if err := v.validate(); err != nil {
		return nil, fmt.Errorf("encoded value is invalid: %w", err)
	}
	return json.Marshal(v)
}

// EncodeHealthResponse encodes GET /api/health.
func EncodeHealthResponse(v HealthResponse) ([]byte, error) {
	return encodeChecked(v, DecodeHealthResponse)
}

// EncodeSessionResponse encodes a session payload.
func EncodeSessionResponse(v SessionResponse) ([]byte, error) {
	return encodeChecked(v, DecodeSessionResponse)
}

// EncodeAPIError encodes an error body.
func EncodeAPIError(v APIError) ([]byte, error) { return encodeChecked(v, DecodeAPIError) }

// EncodeOKResponse encodes {"ok":true}.
func EncodeOKResponse(v OKResponse) ([]byte, error) { return encodeChecked(v, DecodeOKResponse) }

// EncodeEnrollmentIdentity encodes a successful enrollment.
func EncodeEnrollmentIdentity(v EnrollmentIdentity) ([]byte, error) {
	return encodeChecked(v, DecodeEnrollmentIdentity)
}

// EncodeAdminUserList encodes the user directory.
func EncodeAdminUserList(v AdminUserList) ([]byte, error) {
	return encodeChecked(v, DecodeAdminUserList)
}

// EncodeAuditEventList encodes audit events.
func EncodeAuditEventList(v AuditEventList) ([]byte, error) {
	return encodeChecked(v, DecodeAuditEventList)
}

// EncodeCollectorList encodes the collector registry view.
func EncodeCollectorList(v CollectorList) ([]byte, error) {
	return encodeChecked(v, DecodeCollectorList)
}

// EncodeGrantResponse encodes a new enrollment grant.
func EncodeGrantResponse(v GrantResponse) ([]byte, error) {
	return encodeChecked(v, DecodeGrantResponse)
}

// EncodeCollectorRevokeRequest encodes a collector revocation body.
func EncodeCollectorRevokeRequest(v CollectorRevokeRequest) ([]byte, error) {
	return encodeValidated(v)
}

// EncodeMachineMeta encodes machine metadata.
func EncodeMachineMeta(v MachineMeta) ([]byte, error) { return encodeChecked(v, DecodeMachineMeta) }

// EncodeAlertListResponse encodes the alert overview.
func EncodeAlertListResponse(v AlertListResponse) ([]byte, error) {
	return encodeChecked(v, DecodeAlertListResponse)
}

// EncodeAlertRuleResponse encodes a created or updated rule.
func EncodeAlertRuleResponse(v AlertRuleResponse) ([]byte, error) {
	return encodeChecked(v, DecodeAlertRuleResponse)
}

// EncodeAlertWebhookResponse encodes a webhook configure/clear result.
func EncodeAlertWebhookResponse(v AlertWebhookResponse) ([]byte, error) {
	return encodeChecked(v, DecodeAlertWebhookResponse)
}

// EncodeAlertWebhookTestResponse encodes a webhook test result.
func EncodeAlertWebhookTestResponse(v AlertWebhookTestResponse) ([]byte, error) {
	return encodeChecked(v, DecodeAlertWebhookTestResponse)
}

// EncodeAlertDiagnostics encodes the admin alert diagnostics response.
func EncodeAlertDiagnostics(v AlertDiagnostics) ([]byte, error) {
	return encodeChecked(v, DecodeAlertDiagnostics)
}

// EncodeHistoryStatus encodes the history backend status.
func EncodeHistoryStatus(v HistoryStatus) ([]byte, error) {
	return encodeChecked(v, DecodeHistoryStatus)
}

// EncodeMetricCatalogResponse encodes the metric catalog.
func EncodeMetricCatalogResponse(v MetricCatalogResponse) ([]byte, error) {
	return encodeChecked(v, DecodeMetricCatalogResponse)
}

// EncodeHistoryResult encodes a history query result.
func EncodeHistoryResult(v HistoryResult) ([]byte, error) {
	return encodeChecked(v, DecodeHistoryResult)
}

// EncodeProcessAcquireResponse encodes process viewer lease terms.
func EncodeProcessAcquireResponse(v ProcessAcquireResponse) ([]byte, error) {
	return encodeChecked(v, DecodeProcessAcquireResponse)
}

// EncodeProcessReadResponse encodes a process detail poll response.
func EncodeProcessReadResponse(v ProcessReadResponse) ([]byte, error) {
	return encodeChecked(v, DecodeProcessReadResponse)
}

// EncodeHubSnapshot encodes an SSE snapshot payload (without envelope).
func EncodeHubSnapshot(v HubSnapshot) ([]byte, error) {
	return encodeChecked(v, DecodeHubSnapshot)
}

// EncodeLoginRequest encodes a login body.
func EncodeLoginRequest(v LoginRequest) ([]byte, error) { return encodeValidated(v) }

// EncodeEnrollmentRequest encodes an enrollment body.
func EncodeEnrollmentRequest(v EnrollmentRequest) ([]byte, error) { return encodeValidated(v) }

// EncodeAdminUserInput encodes a user upsert body.
func EncodeAdminUserInput(v AdminUserInput) ([]byte, error) { return encodeValidated(v) }

// EncodeGrantRequest encodes a grant request body.
func EncodeGrantRequest(v GrantRequest) ([]byte, error) { return encodeValidated(v) }

// EncodeMachineMetaInput encodes a machine metadata body.
func EncodeMachineMetaInput(v MachineMetaInput) ([]byte, error) { return encodeValidated(v) }

// EncodeAlertRuleInput encodes a rule create/update body.
func EncodeAlertRuleInput(v AlertRuleInput) ([]byte, error) { return encodeValidated(v) }

// EncodeAlertWebhookRequest encodes a webhook action body.
func EncodeAlertWebhookRequest(v AlertWebhookRequest) ([]byte, error) { return encodeValidated(v) }
