package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// committedGeneratedPath is src/contracts/api.generated.ts relative to this
// package directory (internal/api/gen).
const (
	committedGeneratedPath  = "../../../src/contracts/api.generated.ts"
	committedValidatorsPath = "../../../src/contracts/api.validators.generated.ts"
)

// TestGeneratedFilesAreCurrent runs the generator twice from the package
// sources and compares both outputs with the committed TypeScript files. A
// stale file, a non-deterministic generator or an ungenerated type fails CI.
func TestGeneratedFilesAreCurrent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		committed string
		generate  func(string) ([]byte, error)
		marker    string
	}{
		{"api.generated.ts", committedGeneratedPath, generate, "export const API_ENDPOINTS"},
		{"api.validators.generated.ts", committedValidatorsPath, generateValidators, "export const API_RESPONSE_VALIDATORS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, err := tc.generate("..")
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			second, err := tc.generate("..")
			if err != nil {
				t.Fatalf("generate (second run): %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("generation is not deterministic: two runs differ")
			}
			if len(first) == 0 {
				t.Fatal("generation produced an empty file")
			}
			if !bytes.Contains(first, []byte(tc.marker)) {
				t.Fatalf("generation did not emit %q", tc.marker)
			}
			committed, err := os.ReadFile(filepath.Clean(tc.committed))
			if err != nil {
				t.Fatalf("read committed file: %v", err)
			}
			if !bytes.Equal(first, committed) {
				lines := strings.Split(string(committed), "\n")
				generatedLines := strings.Split(string(first), "\n")
				for i := 0; i < len(lines) && i < len(generatedLines); i++ {
					if lines[i] != generatedLines[i] {
						t.Fatalf("committed %s is stale at line %d:\ncommitted: %s\ngenerated: %s\nrun go generate ./internal/api",
							tc.name, i+1, lines[i], generatedLines[i])
					}
				}
				t.Fatalf("committed %s is stale (%d committed lines, %d generated lines); run go generate ./internal/api",
					tc.name, len(lines), len(generatedLines))
			}
		})
	}
}

// TestGeneratedFiniteTypes fails when a finite field loses its literal union
// or the webhook body loses its discriminated union.
func TestGeneratedFiniteTypes(t *testing.T) {
	types, err := generate("..")
	if err != nil {
		t.Fatalf("generate types: %v", err)
	}
	for _, want := range []string{
		`export type AlertWebhookRequest = ({ action: "configure"; url: string;`,
		`{ action: "clear" }`,
		`{ action: "test" }`,
		`export type StorageTier = "raw" | "summary";`,
		`export type ProcessState = "loading" | "ready" | "expired"`,
		`export type AlertIncidentStatus = "open" | "recovered";`,
		`export type WebhookChannel = "webhook";`,
	} {
		if !bytes.Contains(types, []byte(want)) {
			t.Errorf("generated types are missing %q", want)
		}
	}
	validators, err := generateValidators("..")
	if err != nil {
		t.Fatalf("generate validators: %v", err)
	}
	for _, want := range []string{
		`__enum(o.storageTier, ["raw", "summary"]`,
		`__enum(o.state, ["loading", "ready", "expired"`,
		`__enum(o.status, ["open", "recovered"]`,
	} {
		if !bytes.Contains(validators, []byte(want)) {
			t.Errorf("generated validators are missing %q", want)
		}
	}
}

func TestValidatorsCoverResponses(t *testing.T) {
	contract, err := load("..")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	response, err := responseClosure(contract)
	if err != nil {
		t.Fatalf("response closure: %v", err)
	}
	if len(response) < 20 {
		t.Fatalf("expected at least 20 response validators, found %d", len(response))
	}
	for _, name := range []string{"HealthResponse", "SessionResponse", "HistoryStatus", "HistoryResult",
		"AlertListResponse", "ProcessReadResponse", "HubSnapshot", "MachineState"} {
		found := false
		for _, candidate := range response {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("response type %q has no generated validator", name)
		}
	}
}
