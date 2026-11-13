package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixtureRoot is the captured Node-hub response corpus.
const fixtureRoot = "../../testdata/api"

type fixtureDecoder struct {
	decode func([]byte) (any, error)
	typ    reflect.Type
}

func decoder[T any](fn func([]byte) (T, error)) fixtureDecoder {
	return fixtureDecoder{
		decode: func(data []byte) (any, error) {
			value, err := fn(data)
			return value, err
		},
		typ: reflect.TypeOf((*T)(nil)).Elem(),
	}
}

// responseFixtures maps each captured file to the type that documents it.
func responseFixtures() map[string]fixtureDecoder {
	return map[string]fixtureDecoder{
		"health/get.json":                    decoder(DecodeHealthResponse),
		"auth/login.json":                    decoder(DecodeSessionResponse),
		"auth/session.json":                  decoder(DecodeSessionResponse),
		"auth/session-unauthorized.json":     decoder(DecodeAPIError),
		"auth/login-invalid.json":            decoder(DecodeAPIError),
		"auth/logout.json":                   decoder(DecodeOKResponse),
		"history/status.json":                decoder(DecodeHistoryStatus),
		"history/status-persistent.json":     decoder(DecodeHistoryStatus),
		"history/metrics.json":               decoder(DecodeMetricCatalogResponse),
		"history/query.json":                 decoder(DecodeHistoryResult),
		"history/query-unavailable.json":     decoder(DecodeHistoryResult),
		"history/invalid-query.json":         decoder(DecodeAPIError),
		"machines/meta.json":                 decoder(DecodeMachineMeta),
		"admin/users.json":                   decoder(DecodeAdminUserList),
		"admin/users-upsert.json":            decoder(DecodeOKResponse),
		"admin/users-delete.json":            decoder(DecodeOKResponse),
		"admin/audit.json":                   decoder(DecodeAuditEventList),
		"admin/collectors.json":              decoder(DecodeCollectorList),
		"admin/collectors-revoke.json":       decoder(DecodeOKResponse),
		"admin/collector-grants.json":        decoder(DecodeGrantResponse),
		"enrollment/enroll.json":             decoder(DecodeEnrollmentIdentity),
		"processes/acquire.json":             decoder(DecodeProcessAcquireResponse),
		"processes/read.json":                decoder(DecodeProcessReadResponse),
		"processes/read-with-processes.json": decoder(DecodeProcessReadResponse),
		"processes/budget-error.json":        decoder(DecodeAPIError),
		"processes/release.json":             decoder(DecodeOKResponse),
		"alerts/list.json":                   decoder(DecodeAlertListResponse),
		"alerts/list-active.json":            decoder(DecodeAlertListResponse),
		"alerts/rule-create.json":            decoder(DecodeAlertRuleResponse),
		"alerts/rule-update.json":            decoder(DecodeAlertRuleResponse),
		"alerts/webhook.json":                decoder(DecodeAlertWebhookResponse),
		"alerts/webhook-configured.json":     decoder(DecodeAlertWebhookResponse),
		"alerts/webhook-test.json":           decoder(DecodeAlertWebhookTestResponse),
		"alerts/rule-delete.json":            decoder(DecodeOKResponse),
		"errors/unauthorized.json":           decoder(DecodeAPIError),
	}
}

// TestResponseFixturesRoundTrip decodes every captured response with the
// strict API types, re-encodes it, and compares the JSON values. An unknown
// field, missing field or out-of-range value fails the decode.
func TestResponseFixturesRoundTrip(t *testing.T) {
	decoders := responseFixtures()
	for path, entry := range decoders {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(fixtureRoot, path))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			value, err := entry.decode(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			assertJSONEqual(t, data, encoded)
		})
	}
}

// TestFixtureCorpusIsCovered fails when a captured fixture has no decoder, so
// a new endpoint cannot be added without a strict type.
func TestFixtureCorpusIsCovered(t *testing.T) {
	decoders := responseFixtures()
	covered := make(map[string]bool, len(decoders))
	for path := range decoders {
		covered[path] = true
	}
	var found []string
	err := filepath.WalkDir(fixtureRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		rel, err := filepath.Rel(fixtureRoot, path)
		if err != nil {
			return err
		}
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk fixtures: %v", err)
	}
	for _, path := range found {
		if !covered[path] && !strings.HasPrefix(path, "live/") && !strings.HasPrefix(path, "reencoded/") && path != "manifest.json" {
			t.Errorf("fixture %q has no strict decoder; add it to responseFixtures", path)
		}
	}
}

// liveFixture is the sanitized SSE envelope written by the capture script.
type liveFixture struct {
	ID    string          `json:"id"`
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// TestLiveFixturesRoundTrip decodes the captured SSE events, rebuilds the wire
// frame, and parses it back with the SSE decoder.
func TestLiveFixturesRoundTrip(t *testing.T) {
	for _, name := range []string{"snapshot", "reset", "update"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(fixtureRoot, "live", name+".json"))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var envelope liveFixture
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			id, err := ParseLiveEventID(envelope.ID)
			if err != nil {
				t.Fatalf("parse event id: %v", err)
			}
			event := LiveEvent(envelope.Event)
			if err := validateLiveEvent(event); err != nil {
				t.Fatalf("event: %v", err)
			}
			snapshot, err := DecodeHubSnapshot(envelope.Data)
			if err != nil {
				t.Fatalf("decode snapshot: %v", err)
			}
			frame, err := EncodeLiveFrame(LiveFrame{ID: id, Event: event, Data: snapshot})
			if err != nil {
				t.Fatalf("encode frame: %v", err)
			}
			decoded, ok, err := DecodeLiveFrame(frame)
			if err != nil {
				t.Fatalf("decode frame: %v", err)
			}
			if !ok {
				t.Fatal("expected an event frame, got a comment")
			}
			if decoded.ID != id || decoded.Event != event {
				t.Fatalf("frame envelope changed: %+v", decoded)
			}
			encoded, err := json.Marshal(decoded.Data)
			if err != nil {
				t.Fatalf("re-encode snapshot: %v", err)
			}
			assertJSONEqual(t, envelope.Data, encoded)
		})
	}
}

// assertJSONEqual compares two JSON documents by value. Numbers are compared as
// float64 so 10000000000000 and 1e+13 are equal (the same JSON number), while
// strings, nulls, arrays and objects must match exactly.
func assertJSONEqual(t *testing.T, want, got []byte) {
	t.Helper()
	wantValue := decodeCanonical(t, want)
	gotValue := decodeCanonical(t, got)
	if !reflect.DeepEqual(wantValue, gotValue) {
		t.Fatalf("JSON value changed\nwant: %s\ngot:  %s", compact(want), compact(got))
	}
}

func decodeCanonical(t *testing.T, data []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return canonicalize(value)
}

func canonicalize(value any) any {
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return v.String()
		}
		return f
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = canonicalize(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = canonicalize(item)
		}
		return out
	default:
		return value
	}
}

func compact(data []byte) string {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, data); err != nil {
		return string(data)
	}
	return buffer.String()
}

// ---------------------------------------------------------------------------
// Mutation tests generated from the captured fixtures
// ---------------------------------------------------------------------------

type fixtureMutation struct {
	doc  any
	what string
}

// TestResponseFixturesRejectMissingAndNullFields deletes every required key and
// nulls every non-nullable value in each captured response, one at a time, and
// asserts the strict decoder rejects each mutation. Optional nullable fields
// are left alone because null/absence is valid there.
func TestResponseFixturesRejectMissingAndNullFields(t *testing.T) {
	for path, entry := range responseFixtures() {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(fixtureRoot, path))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var doc any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatalf("decode fixture JSON: %v", err)
			}
			mutations := collectFixtureMutations(doc, entry.typ, path)
			if len(mutations) == 0 {
				t.Fatal("no required/non-nullable mutations were generated")
			}
			for _, mutation := range mutations {
				mutated, err := json.Marshal(mutation.doc)
				if err != nil {
					t.Fatalf("%s: marshal mutation: %v", mutation.what, err)
				}
				if _, err := entry.decode(mutated); err == nil {
					t.Errorf("%s was accepted:\n%s", mutation.what, compact(mutated))
				}
			}
		})
	}
}

func collectFixtureMutations(doc any, t reflect.Type, path string) []fixtureMutation {
	if t == nil {
		return nil
	}
	if elem, ok := optionalType(t); ok {
		t = elem
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if skipShapeType(t) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := doc.(map[string]any)
		if !ok {
			return nil
		}
		var out []fixtureMutation
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.PkgPath != "" {
				continue
			}
			name, optional := jsonField(field)
			if name == "" || name == "-" {
				continue
			}
			value, present := obj[name]
			if !present {
				continue
			}
			_, optionalValue := optionalType(field.Type)
			pointer := field.Type.Kind() == reflect.Pointer
			if !optional {
				clone := cloneObject(obj)
				delete(clone, name)
				out = append(out, fixtureMutation{doc: clone, what: path + "." + name + " (deleted)"})
			}
			if value != nil && !optionalValue && !(pointer && !optional) {
				clone := cloneObject(obj)
				clone[name] = nil
				out = append(out, fixtureMutation{doc: clone, what: path + "." + name + " (null)"})
			}
			childType := field.Type
			if elem, ok := optionalType(childType); ok {
				childType = elem
			} else if childType.Kind() == reflect.Pointer {
				childType = childType.Elem()
			}
			out = append(out, collectFixtureMutations(value, childType, path+"."+name)...)
		}
		return out
	case reflect.Slice:
		items, ok := doc.([]any)
		if !ok {
			return nil
		}
		var out []fixtureMutation
		for i, item := range items {
			out = append(out, collectFixtureMutations(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i))...)
		}
		return out
	case reflect.Map:
		obj, ok := doc.(map[string]any)
		if !ok {
			return nil
		}
		var out []fixtureMutation
		for key, value := range obj {
			out = append(out, collectFixtureMutations(value, t.Elem(), path+"."+key)...)
		}
		return out
	default:
		return nil
	}
}

func skipShapeType(t reflect.Type) bool {
	if t == nil {
		return true
	}
	if t.PkgPath() == "github.com/Triet1402/rigscope/internal/protocol" {
		return true
	}
	return t.PkgPath() == "github.com/Triet1402/rigscope/internal/api" && t.Name() == "MachineScope"
}

func cloneObject(obj map[string]any) map[string]any {
	out := make(map[string]any, len(obj))
	for key, value := range obj {
		out[key] = value
	}
	return out
}
