package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/protocol"
)

func TestUnknownFieldsRejected(t *testing.T) {
	cases := []struct {
		name   string
		data   string
		decode func([]byte) error
	}{
		{"health", `{"status":"ok","version":"1.0.0","mode":"runtime-spike","hubRunId":"hub-1","extra":1}`,
			func(data []byte) error { _, err := DecodeHealthResponse(data); return err }},
		{"session", `{"username":"a","csrfToken":"c","admin":true,"extra":1}`,
			func(data []byte) error { _, err := DecodeSessionResponse(data); return err }},
		{"ok", `{"ok":true,"extra":1}`,
			func(data []byte) error { _, err := DecodeOKResponse(data); return err }},
		{"machine", `{"hubRunId":"hub-1","machines":[{"machineId":"m","collectorId":"c","hostname":"h","connected":false,"lastReceivedAt":null,"extra":1}]}`,
			func(data []byte) error { _, err := DecodeHubSnapshot(data); return err }},
		{"process read", `{"state":"ready","result":null,"extra":1}`,
			func(data []byte) error { _, err := DecodeProcessReadResponse(data); return err }},
		{"login", `{"username":"a","password":"b","extra":1}`,
			func(data []byte) error { _, err := DecodeLoginRequest(data); return err }},
		{"admin user", `{"username":"a","machineIds":"*","admin":true,"extra":1}`,
			func(data []byte) error { _, err := DecodeAdminUserInput(data); return err }},
		{"machine metadata", `{"tags":[],"extra":1}`,
			func(data []byte) error { _, err := DecodeMachineMetaInput(data); return err }},
		{"alert rule", `{"name":"n","machineId":"m","metric":"cpu.usage","condition":"above","durationMs":1000,"threshold":1,"extra":1}`,
			func(data []byte) error { _, err := DecodeAlertRuleInput(data); return err }},
		{"webhook", `{"action":"clear","extra":1}`,
			func(data []byte) error { _, err := DecodeAlertWebhookRequest(data); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.decode([]byte(tc.data)); err == nil {
				t.Fatal("expected an unknown-field error")
			} else if !strings.Contains(err.Error(), "unexpected field") &&
				!strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("error does not mention the unknown field: %v", err)
			}
		})
	}
}

func TestRequestBodiesRejectMissingFields(t *testing.T) {
	if _, err := DecodeLoginRequest([]byte(`{"username":"a"}`)); err == nil {
		t.Fatal("a login body without a password must be rejected")
	}
	if _, err := DecodeAdminUserInput([]byte(`{"username":"a","machineIds":"*"}`)); err == nil {
		t.Fatal("a user body without admin must be rejected")
	}
	if _, err := DecodeEnrollmentRequest([]byte(`{"collectorId":"c","requestId":"r"}`)); err == nil {
		t.Fatal("an enrollment recovery needs both fields")
	}
}

func TestRequestBodySizeLimits(t *testing.T) {
	oversized := []byte(`{"username":"` + strings.Repeat("a", MaxLoginBodyBytes) + `","password":"p"}`)
	_, err := DecodeLoginRequest(oversized)
	var input *InputError
	if !errors.As(err, &input) || input.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected a 413 InputError, got %v", err)
	}
	if _, err := DecodeAlertRuleInput([]byte(strings.Repeat(" ", MaxAlertRuleBodyBytes+1))); err == nil {
		t.Fatal("expected an oversized rule body to be rejected")
	}
}

func TestOptionalFieldPresence(t *testing.T) {
	input, err := DecodeMachineMetaInput([]byte(`{"displayName":null}`))
	if err != nil {
		t.Fatalf("explicit null displayName must decode: %v", err)
	}
	if input.DisplayName == nil || input.DisplayName.Valid {
		t.Fatal("explicit null must be a present, invalid Optional")
	}
	if _, err := DecodeMachineMetaInput([]byte(`{}`)); err == nil {
		t.Fatal("an empty machine metadata body must be rejected")
	}

	snapshot, err := DecodeHubSnapshot([]byte(`{"hubRunId":"hub-1","machines":[
		{"machineId":"m","collectorId":"c","hostname":"h","connected":false,"lastReceivedAt":null}
	]}`))
	if err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snapshot.Machines[0].DisplayName != nil || snapshot.Machines[0].ExpectedIntervalMs != nil {
		t.Fatal("absent optional machine fields must stay nil")
	}

	snapshot, err = DecodeHubSnapshot([]byte(`{"hubRunId":"hub-1","machines":[
		{"machineId":"m","collectorId":"c","hostname":"h","connected":false,"lastReceivedAt":null,
		 "displayName":null,"expectedIntervalMs":null}
	]}`))
	if err != nil {
		t.Fatalf("decode snapshot with nulls: %v", err)
	}
	if snapshot.Machines[0].DisplayName == nil || snapshot.Machines[0].DisplayName.Valid {
		t.Fatal("a present null displayName must stay present and invalid")
	}
	if snapshot.Machines[0].ExpectedIntervalMs == nil || snapshot.Machines[0].ExpectedIntervalMs.Valid {
		t.Fatal("a present null cadence must stay present and invalid")
	}
}

func TestMachineStateValidatesCadenceAndTags(t *testing.T) {
	for _, data := range []string{
		`{"hubRunId":"hub-1","machines":[{"machineId":"m","collectorId":"c","hostname":"h","connected":false,"lastReceivedAt":null,"expectedIntervalMs":99}]}`,
		`{"hubRunId":"hub-1","machines":[{"machineId":"m","collectorId":"c","hostname":"h","connected":false,"lastReceivedAt":null,"tags":[""]}]}`,
		`{"hubRunId":"hub-1","machines":[{"machineId":"m","collectorId":"c","hostname":"h","connected":false,"lastReceivedAt":null,"tags":["a","a"]}]}`,
		`{"hubRunId":"hub-1","machines":[{"machineId":"m","collectorId":"c","hostname":"h","connected":false,"lastReceivedAt":null},{"machineId":"m","collectorId":"c2","hostname":"h","connected":false,"lastReceivedAt":null}]}`,
	} {
		if _, err := DecodeHubSnapshot([]byte(data)); err == nil {
			t.Fatalf("expected snapshot validation to fail for %s", data)
		}
	}
}

func TestSessionValidation(t *testing.T) {
	if _, err := DecodeSessionResponse([]byte(`{"username":"","csrfToken":"c"}`)); err == nil {
		t.Fatal("an empty username must be rejected")
	}
	if _, err := DecodeSessionResponse([]byte(`{"username":"a","csrfToken":"c","admin":false}`)); err == nil {
		t.Fatal("admin:false must be rejected because the hub omits the field")
	}
}

func TestLiveEventIDs(t *testing.T) {
	id, err := ParseLiveEventID("hub-1:1234567890123456789012345678901234567890")
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}
	if id.HubRunID != "hub-1" || id.Cursor != "1234567890123456789012345678901234567890" {
		t.Fatalf("unexpected id: %+v", id)
	}
	for _, bad := range []string{"", ":1", "hub-1:", "hub-1:abc", "hub-1:" + strings.Repeat("1", 41)} {
		if _, err := ParseLiveEventID(bad); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}

func TestLiveFrameParser(t *testing.T) {
	snapshot := `{"hubRunId":"hub-1","machines":[]}`
	frame := "id: hub-1:7\nevent: snapshot\ndata: " + snapshot + "\n\n"
	decoded, ok, err := DecodeLiveFrame([]byte(frame))
	if err != nil || !ok {
		t.Fatalf("decode frame: ok=%v err=%v", ok, err)
	}
	if decoded.Event != LiveEventSnapshot || decoded.ID.Cursor != "7" {
		t.Fatalf("unexpected frame: %+v", decoded)
	}
	if _, ok, err := DecodeLiveFrame([]byte(": keepalive 123\n\n")); err != nil || ok {
		t.Fatalf("a keepalive must decode as a non-event: ok=%v err=%v", ok, err)
	}
	if _, _, err := DecodeLiveFrame([]byte("id: hub-1:7\nevent: snapshot\ndata: {\"hubRunId\":\"other\",\"machines\":[]}\n\n")); err == nil {
		t.Fatal("id and snapshot hubRunId must agree")
	}
	if _, _, err := DecodeLiveFrame([]byte("id: hub-1:7\nevent: bogus\ndata: " + snapshot + "\n\n")); err == nil {
		t.Fatal("an unknown event name must be rejected")
	}
}

func TestEncodeRejectsInvalidValues(t *testing.T) {
	if _, err := EncodeSessionResponse(SessionResponse{Username: "a"}); err == nil {
		t.Fatal("an empty csrfToken must not encode")
	}
	if _, err := EncodeOKResponse(OKResponse{}); err == nil {
		t.Fatal("ok:false must not encode")
	}
	if _, err := EncodeHubSnapshot(HubSnapshot{HubRunID: "hub-1"}); err == nil {
		t.Fatal("a nil machines list must not encode")
	}
	if _, err := EncodeAlertListResponse(AlertListResponse{}); err == nil {
		t.Fatal("an empty alert list response must not encode")
	}
}

func TestEncodeRequestBodies(t *testing.T) {
	data, err := EncodeLoginRequest(LoginRequest{Username: "a", Password: "b"})
	if err != nil {
		t.Fatalf("encode login: %v", err)
	}
	if _, err := DecodeLoginRequest(data); err != nil {
		t.Fatalf("encoded login must decode: %v", err)
	}
	data, err = EncodeMachineMetaInput(MachineMetaInput{Tags: &[]string{}})
	if err != nil {
		t.Fatalf("encode metadata: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["tags"]) != "[]" {
		t.Fatalf("tags must encode as an empty array, got %s", decoded["tags"])
	}
}

func TestRequireJSONContentType(t *testing.T) {
	for _, accepted := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"Application/JSON",
		" application/json ; charset=UTF-8 ",
	} {
		if err := RequireJSONContentType(accepted); err != nil {
			t.Fatalf("a JSON content type must be accepted (%q): %v", accepted, err)
		}
	}
	for _, rejected := range []string{"text/plain", "application/json-patch+json", "", "application/"} {
		err := RequireJSONContentType(rejected)
		var input *InputError
		if !errors.As(err, &input) || input.Status != http.StatusUnsupportedMediaType {
			t.Fatalf("expected 415 for %q, got %v", rejected, err)
		}
	}
}

func TestCollectorRevokeRequest(t *testing.T) {
	body, err := DecodeCollectorRevokeRequest([]byte(`{"action":"revoke","collectorId":"collector-1"}`))
	if err != nil {
		t.Fatalf("decode revoke: %v", err)
	}
	if body.CollectorID != "collector-1" {
		t.Fatalf("unexpected body: %+v", body)
	}
	for _, bad := range []string{
		`{"action":"delete","collectorId":"c"}`,
		`{"action":"revoke"}`,
		`{"action":"revoke","collectorId":"c","extra":true}`,
		`{"action":"revoke","collectorId":null}`,
	} {
		if _, err := DecodeCollectorRevokeRequest([]byte(bad)); err == nil {
			t.Fatalf("expected %s to be rejected", bad)
		}
	}
}

func TestAlertWebhookRequestValidation(t *testing.T) {
	if _, err := DecodeAlertWebhookRequest([]byte(`{"action":"clear"}`)); err != nil {
		t.Fatalf("clear must decode: %v", err)
	}
	if _, err := DecodeAlertWebhookRequest([]byte(`{"action":"configure","url":"https://example.test/hook","headers":{"X-Token":"t"},"secret":null,"enabled":false}`)); err != nil {
		t.Fatalf("configure must decode: %v", err)
	}
	for _, bad := range []string{
		`{"action":"configure"}`,
		`{"action":"clear","url":"https://example.test"}`,
		`{"action":"configure","url":"https://example.test","headers":{"Host":"x"}}`,
		`{"action":"configure","url":"https://example.test","headers":{"bad header":"x"}}`,
		`{"action":"unknown"}`,
	} {
		if _, err := DecodeAlertWebhookRequest([]byte(bad)); err == nil {
			t.Fatalf("expected %s to be rejected", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// Regression tests for review findings
// ---------------------------------------------------------------------------

func TestLiveMetricsReceiptTimeEqualityIncludesNull(t *testing.T) {
	base := `{"hubRunId":"hub-1","machines":[{"machineId":"m","collectorId":"c","hostname":"h","connected":true,
		"lastReceivedAt":%s,"metrics":{"collectorId":"c","runId":"r","bootId":"b","sequence":1,
		"capturedAt":"2026-11-12T08:00:00.000Z","kind":"metrics",
		"payload":{"cpu":{"status":"unavailable","reason":"none"},"memory":{"status":"unavailable","reason":"none"},
		"swap":{"status":"unavailable","reason":"none"},"gpus":{"status":"unavailable","reason":"none"},
		"disks":{"status":"unavailable","reason":"none"},"network":{"status":"unavailable","reason":"none"}},
		"receivedAt":"2026-11-12T08:00:01.000Z"}}]}`

	// metrics present while lastReceivedAt is null must be rejected.
	if _, err := DecodeHubSnapshot([]byte(fmt.Sprintf(base, "null"))); err == nil {
		t.Fatal("a metrics row with a null lastReceivedAt must be rejected")
	}
	// Mismatched receipt times must be rejected.
	if _, err := DecodeHubSnapshot([]byte(fmt.Sprintf(base, `"2026-11-12T08:00:02.000Z"`))); err == nil {
		t.Fatal("mismatched receivedAt and lastReceivedAt must be rejected")
	}
	// A matching pair decodes.
	if _, err := DecodeHubSnapshot([]byte(fmt.Sprintf(base, `"2026-11-12T08:00:01.000Z"`))); err != nil {
		t.Fatalf("a matching receipt pair must decode: %v", err)
	}
}

func TestLiveMetricsSequenceAndCadenceBounds(t *testing.T) {
	// Sequence above the protocol safe-integer bound must be rejected even
	// though it fits int64.
	oversized := `{"hubRunId":"hub-1","machines":[{"machineId":"m","collectorId":"c","hostname":"h","connected":true,
		"lastReceivedAt":"2026-11-12T08:00:01.000Z","metrics":{"collectorId":"c","runId":"r","bootId":"b",
		"sequence":9007199254740992,"capturedAt":"2026-11-12T08:00:00.000Z","kind":"metrics",
		"payload":{"cpu":{"status":"unavailable","reason":"none"},"memory":{"status":"unavailable","reason":"none"},
		"swap":{"status":"unavailable","reason":"none"},"gpus":{"status":"unavailable","reason":"none"},
		"disks":{"status":"unavailable","reason":"none"},"network":{"status":"unavailable","reason":"none"}},
		"receivedAt":"2026-11-12T08:00:01.000Z"}}]}`
	if _, err := DecodeHubSnapshot([]byte(oversized)); err == nil {
		t.Fatal("a sequence above 2^53-1 must be rejected")
	}

	// A constructed snapshot with an out-of-range cadence must not encode.
	interval := int64(99)
	snapshot := HubSnapshot{HubRunID: "hub-1", Machines: []MachineState{{
		MachineID: "m", CollectorID: "c", Hostname: "h", Connected: true,
		ExpectedIntervalMs: &protocol.Optional[int64]{Valid: true, Value: interval},
	}}}
	if _, err := EncodeHubSnapshot(snapshot); err == nil {
		t.Fatal("a cadence below MIN_EXPECTED_INTERVAL_MS must not encode")
	}
	interval = protocol.MaxExpectedIntervalMS
	snapshot.Machines[0].ExpectedIntervalMs = &protocol.Optional[int64]{Valid: true, Value: interval}
	if _, err := EncodeHubSnapshot(snapshot); err != nil {
		t.Fatalf("the maximum cadence must encode: %v", err)
	}
}

func TestEncodeLiveFrameValidatesEnvelopeAndData(t *testing.T) {
	snapshot := HubSnapshot{HubRunID: "hub-1", Machines: []MachineState{}}
	if _, err := EncodeLiveFrame(LiveFrame{
		ID: LiveEventID{HubRunID: "other", Cursor: "1"}, Event: LiveEventSnapshot, Data: snapshot,
	}); err == nil {
		t.Fatal("an envelope whose hubRunId differs from the snapshot must be rejected")
	}
	// The checked snapshot encoder rejects a nil machines list.
	if _, err := EncodeLiveFrame(LiveFrame{
		ID: LiveEventID{HubRunID: "hub-1", Cursor: "1"}, Event: LiveEventSnapshot,
		Data: HubSnapshot{HubRunID: "hub-1"},
	}); err == nil {
		t.Fatal("EncodeLiveFrame must use the checked snapshot encoder")
	}
}

func TestMachineMetaInputTrimsAndDeduplicatesTags(t *testing.T) {
	input, err := DecodeMachineMetaInput([]byte(`{"tags":[" lab ","lab","rig"]}`))
	if err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if input.Tags == nil || len(*input.Tags) != 2 || (*input.Tags)[0] != "lab" || (*input.Tags)[1] != "rig" {
		t.Fatalf("request tags must be trimmed and deduplicated: %v", *input.Tags)
	}
	// Stored responses keep the strict trimmed/unique invariant.
	if _, err := DecodeMachineMeta([]byte(`{"displayName":null,"tags":[" lab ","lab"]}`)); err == nil {
		t.Fatal("a stored response with untrimmed or duplicate tags must be rejected")
	}
}

func TestAlertWebhookHeadersInspectTypesAndTrimNames(t *testing.T) {
	request, err := DecodeAlertWebhookRequest([]byte(`{"action":"configure","url":"https://example.test/hook","headers":{" X-Token ":"value"}}`))
	if err != nil {
		t.Fatalf("decode webhook: %v", err)
	}
	if request.Headers == nil {
		t.Fatal("headers missing")
	}
	if _, ok := (*request.Headers)["X-Token"]; !ok {
		t.Fatalf("header names must be trimmed: %v", *request.Headers)
	}
	for _, bad := range []string{
		`{"action":"configure","url":"https://example.test/hook","headers":{"X-Token":null}}`,
		`{"action":"configure","url":"https://example.test/hook","headers":{"X-Token":1}}`,
		`{"action":"configure","url":"https://example.test/hook","headers":null}`,
	} {
		if _, err := DecodeAlertWebhookRequest([]byte(bad)); err == nil {
			t.Fatalf("expected %s to be rejected", bad)
		}
	}
}

func TestLoginValidationMirrorsNodePreVerificationChecks(t *testing.T) {
	// Node only checks string type and length before password verification, so
	// an unusual but bounded username reaches the generic 401 path.
	for _, username := range []string{"", "has space", "control\u0001char", "\u00e9"} {
		body, err := json.Marshal(map[string]string{"username": username, "password": "p"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeLoginRequest(body); err != nil {
			t.Fatalf("username %q must reach password verification: %v", username, err)
		}
	}
	if _, err := DecodeLoginRequest([]byte(`{"username":"a","password":""}`)); err == nil {
		t.Fatal("an empty password must be rejected")
	}
	if _, err := DecodeLoginRequest([]byte(`{"username":"` + strings.Repeat("a", MaxUsernameLength+1) + `","password":"p"}`)); err == nil {
		t.Fatal("an over-long username must be rejected")
	}
}

func TestLogoutRequiresSessionMetadata(t *testing.T) {
	for _, endpoint := range Endpoints() {
		if endpoint.Method == "POST" && endpoint.Path == "/api/auth/logout" {
			if endpoint.Auth != AuthSession {
				t.Fatalf("logout auth must be %q, got %q", AuthSession, endpoint.Auth)
			}
			return
		}
	}
	t.Fatal("logout endpoint missing from the table")
}

func TestParseProcessViewerQuery(t *testing.T) {
	query, err := ParseProcessViewerQuery(map[string][]string{
		"machineId": {"machine-1"},
		"viewerId":  {"viewer-abc-123"},
	})
	if err != nil {
		t.Fatalf("parse viewer query: %v", err)
	}
	if query.MachineID != "machine-1" || query.ViewerID != "viewer-abc-123" {
		t.Fatalf("unexpected query: %+v", query)
	}
	for _, values := range []map[string][]string{
		{"viewerId": {"viewer"}},
		{"machineId": {"machine-1"}},
		{"machineId": {"machine-1"}, "viewerId": {"bad viewer"}},
		{"machineId": {strings.Repeat("m", 129)}, "viewerId": {"viewer"}},
	} {
		if _, err := ParseProcessViewerQuery(values); err == nil {
			t.Fatalf("expected %v to be rejected", values)
		}
	}
}
