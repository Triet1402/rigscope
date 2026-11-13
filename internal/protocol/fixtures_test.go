package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fixtureRoot = "../../testdata/protocol"

// fixtureFiles returns the sorted .json files in testdata/protocol/<kind>.
func fixtureFiles(t *testing.T, kind string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(fixtureRoot, kind))
	if err != nil {
		t.Fatalf("read fixture directory: %v", err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		files = append(files, filepath.Join(fixtureRoot, kind, entry.Name()))
	}
	if len(files) == 0 {
		t.Fatalf("no %s fixtures under %s", kind, fixtureRoot)
	}
	return files
}

// decodeFixture picks the decoder from the file-name direction prefix.
func decodeFixture(t *testing.T, name string, data []byte) (any, error) {
	t.Helper()
	switch {
	case strings.HasPrefix(name, "collector-"):
		return DecodeCollectorMessage(data)
	case strings.HasPrefix(name, "hub-"):
		return DecodeHubMessage(data)
	default:
		t.Fatalf("fixture %q has no collector-/hub- direction prefix", name)
		return nil, nil
	}
}

func fixtureName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".json")
}

func TestValidFixturesDecode(t *testing.T) {
	files := fixtureFiles(t, "valid")
	for _, path := range files {
		path := path
		t.Run(fixtureName(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeFixture(t, fixtureName(path), data); err != nil {
				t.Fatalf("expected a valid fixture to decode: %v", err)
			}
		})
	}
}

func TestInvalidFixturesRejected(t *testing.T) {
	files := fixtureFiles(t, "invalid")
	for _, path := range files {
		path := path
		t.Run(fixtureName(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeFixture(t, fixtureName(path), data); err == nil {
				t.Fatalf("expected the fixture to be rejected")
			}
		})
	}
}

// encodeFixture encodes a decoded message through the validated outbound API so
// every valid fixture exercises the same path senders must use.
func encodeFixture(t *testing.T, name string, message any) []byte {
	t.Helper()
	var (
		data []byte
		err  error
	)
	switch value := message.(type) {
	case CollectorMessage:
		data, err = EncodeCollectorMessage(value)
	case HubMessage:
		data, err = EncodeHubMessage(value)
	default:
		t.Fatalf("%s: unexpected message type %T", name, message)
	}
	if err != nil {
		t.Fatalf("%s: encode: %v", name, err)
	}
	return data
}

func TestValidFixturesRoundTrip(t *testing.T) {
	files := fixtureFiles(t, "valid")
	for _, path := range files {
		path := path
		t.Run(fixtureName(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			name := fixtureName(path)
			first, err := decodeFixture(t, name, data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			encoded := encodeFixture(t, name, first)
			second, err := decodeFixture(t, name, encoded)
			if err != nil {
				t.Fatalf("decode of re-encoded fixture: %v\n%s", err, encoded)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("round trip changed the value\nfirst:  %#v\nsecond: %#v", first, second)
			}
		})
	}
}

func TestReencodedFixturesMatch(t *testing.T) {
	files := fixtureFiles(t, "valid")
	expected := make(map[string]struct{}, len(files))
	for _, path := range files {
		name := fixtureName(path)
		expected[name+".json"] = struct{}{}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		message, err := decodeFixture(t, name, data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded := encodeFixture(t, name, message)
		var buffer bytes.Buffer
		if err := json.Indent(&buffer, encoded, "", "  "); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded = append(buffer.Bytes(), '\n')
		committed, err := os.ReadFile(filepath.Join(fixtureRoot, "reencoded", name+".json"))
		if err != nil {
			t.Fatalf("%s: read re-encoded fixture: %v (run go generate ./internal/protocol)", name, err)
		}
		if !bytes.Equal(encoded, committed) {
			t.Fatalf("%s: re-encoded fixture is stale (run go generate ./internal/protocol)\nwant:\n%s\ngot:\n%s", name, encoded, committed)
		}
	}
	entries, err := os.ReadDir(filepath.Join(fixtureRoot, "reencoded"))
	if err != nil {
		t.Fatalf("read re-encoded directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, ok := expected[entry.Name()]; !ok {
			t.Fatalf("stale re-encoded fixture %s (run go generate ./internal/protocol)", entry.Name())
		}
	}
}

// TestReencodedFixturesDecode proves the committed Go-encoded fixtures are
// themselves valid protocol messages.
func TestReencodedFixturesDecode(t *testing.T) {
	dir := filepath.Join(fixtureRoot, "reencoded")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read re-encoded directory: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		count++
		name := strings.TrimSuffix(entry.Name(), ".json")
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeFixture(t, name, data); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
	}
	if count == 0 {
		t.Fatal("no re-encoded fixtures; run go generate ./internal/protocol")
	}
}

func TestFixtureCoverage(t *testing.T) {
	valid := fixtureFiles(t, "valid")
	if len(valid)+len(fixtureFiles(t, "invalid")) < 40 {
		t.Fatalf("expected at least 40 fixtures, found %d", len(valid)+len(fixtureFiles(t, "invalid")))
	}

	collectorTypes := map[string]bool{
		"hello": false, "samples": false, "inventory": false,
		"status": false, "detail.result": false, "detail.ended": false,
	}
	hubTypes := map[string]bool{
		"welcome": false, "ack": false, "detail.subscribe": false, "detail.cancel": false,
	}
	for _, path := range valid {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		message, err := decodeFixture(t, fixtureName(path), data)
		if err != nil {
			t.Fatalf("%s: %v", fixtureName(path), err)
		}
		switch m := message.(type) {
		case HelloMessage:
			collectorTypes["hello"] = true
		case SamplesMessage:
			collectorTypes["samples"] = true
		case InventoryMessage:
			collectorTypes["inventory"] = true
		case StatusMessage:
			collectorTypes["status"] = true
		case DetailResult:
			collectorTypes["detail.result"] = true
		case DetailEnded:
			collectorTypes["detail.ended"] = true
		case WelcomeMessage:
			hubTypes["welcome"] = true
		case AckMessage:
			hubTypes["ack"] = true
		case DetailSubscribe:
			hubTypes["detail.subscribe"] = true
		case DetailCancel:
			hubTypes["detail.cancel"] = true
		default:
			t.Fatalf("%s: unexpected message type %T", fixtureName(path), m)
		}
	}
	for name, seen := range collectorTypes {
		if !seen {
			t.Errorf("no valid fixture covers collector message %q", name)
		}
	}
	for name, seen := range hubTypes {
		if !seen {
			t.Errorf("no valid fixture covers hub message %q", name)
		}
	}
}
