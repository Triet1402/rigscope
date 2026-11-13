// Command gen writes the shared protocol fixtures:
//
//   - a few fixtures constructed from Go values and encoded through the
//     validated outbound API, written into valid/;
//   - testdata/protocol/reencoded copies of every valid fixture.
//
// It exists so the TypeScript parser can be tested against output produced by
// the Go encoder:
//
//	go generate ./internal/protocol
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Triet1402/rigscope/internal/protocol"
)

func main() {
	src := flag.String("src", "../../testdata/protocol", "fixture root")
	dst := flag.String("dst", "../../testdata/protocol/reencoded", "output directory")
	flag.Parse()

	if err := run(*src, *dst); err != nil {
		fmt.Fprintf(os.Stderr, "reencode fixtures: %v\n", err)
		os.Exit(1)
	}
}

func run(src, dst string) error {
	validDir := filepath.Join(src, "valid")
	if err := writeConstructed(validDir); err != nil {
		return err
	}
	entries, err := os.ReadDir(validDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	kept := make(map[string]struct{}, len(entries))
	written := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		data, err := os.ReadFile(filepath.Join(validDir, entry.Name()))
		if err != nil {
			return err
		}
		message, err := decode(name, data)
		if err != nil {
			return fmt.Errorf("%s: %w", entry.Name(), err)
		}
		encoded, err := encodeValidated(message)
		if err != nil {
			return fmt.Errorf("%s: %w", entry.Name(), err)
		}
		encoded, err = pretty(encoded)
		if err != nil {
			return fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), encoded, 0o644); err != nil {
			return err
		}
		kept[entry.Name()] = struct{}{}
		written++
	}
	existing, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, entry := range existing {
		if entry.IsDir() {
			continue
		}
		if _, ok := kept[entry.Name()]; !ok {
			if err := os.Remove(filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
	}
	fmt.Printf("reencoded %d valid fixtures into %s\n", written, dst)
	return nil
}

// writeConstructed builds a few messages in Go, encodes them through the
// validated outbound API and stores them as valid fixtures. The re-encode pass
// copies them into reencoded/ for the TypeScript fixture test.
func writeConstructed(validDir string) error {
	fixtures := []struct {
		name    string
		message any
	}{
		{"collector-samples-constructed", constructedSamples()},
		{"collector-hello-constructed-unicode", constructedUnicodeHello()},
		{"hub-ack-constructed", constructedAck()},
	}
	for _, fixture := range fixtures {
		encoded, err := encodeValidated(fixture.message)
		if err != nil {
			return fmt.Errorf("%s: %w", fixture.name, err)
		}
		encoded, err = pretty(encoded)
		if err != nil {
			return fmt.Errorf("%s: %w", fixture.name, err)
		}
		if err := os.WriteFile(filepath.Join(validDir, fixture.name+".json"), encoded, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func constructedSamples() protocol.SamplesMessage {
	return protocol.SamplesMessage{
		Mode: protocol.SampleModeLive,
		Records: []protocol.TelemetryRecord{
			protocol.MetricsRecord{
				RecordEnvelope: protocol.RecordEnvelope{
					CollectorID: "collector-constructed",
					RunID:       "run-constructed",
					BootID:      "boot-constructed",
					Sequence:    1,
					CapturedAt:  "2026-11-12T10:00:00.500Z",
				},
				Payload: protocol.MetricsPayload{
					CPU: protocol.CpuAvailable{
						UsagePercent: 12.5,
						PerCore:      []*float64{float64Ptr(10), nil},
					},
					Memory: protocol.Unavailable{Reason: "not supported"},
					Swap:   protocol.BytesUsageAvailable{UsedBytes: 0, TotalBytes: 0},
					GPUs: protocol.GpusAvailable{Devices: []protocol.GpuReading{
						protocol.GpuAvailable{ID: "gpu:GPU-constructed", UtilizationPercent: float64Ptr(4)},
					}},
					Disks: protocol.DisksAvailable{
						Filesystems: []protocol.FilesystemReading{
							protocol.FilesystemAvailable{ID: "fs:/", UsedBytes: 1, TotalBytes: 2},
						},
						Devices: []protocol.DiskIoReading{},
					},
					Network: protocol.NetworkAvailable{Interfaces: []protocol.InterfaceReading{}},
				},
			},
		},
	}
}

func constructedUnicodeHello() protocol.HelloMessage {
	return protocol.HelloMessage{
		CollectorID:        "collector-constructed-unicode",
		RunID:              "run-constructed-unicode",
		BootID:             "boot-constructed-unicode",
		Hostname:           "rigscope-\U0001F600", // a valid surrogate pair on the wire
		Capabilities:       []protocol.CapabilityName{protocol.CapabilityMetrics},
		ExpectedIntervalMS: 5_000,
	}
}

func constructedAck() protocol.AckMessage {
	return protocol.AckMessage{
		Accepted:     []protocol.RecordRef{{RunID: "run-constructed", Sequence: 1}},
		Rejected:     []protocol.RejectedRecord{},
		RetryAfterMS: int64Ptr(100),
	}
}

func float64Ptr(v float64) *float64 { return &v }

func int64Ptr(v int64) *int64 { return &v }

// encodeValidated encodes a message through the package's outbound API, which
// re-runs the strict decoder and enforces the wire size limits.
func encodeValidated(message any) ([]byte, error) {
	switch value := message.(type) {
	case protocol.CollectorMessage:
		return protocol.EncodeCollectorMessage(value)
	case protocol.HubMessage:
		return protocol.EncodeHubMessage(value)
	default:
		return nil, fmt.Errorf("unsupported message type %T", message)
	}
}

func pretty(encoded []byte) ([]byte, error) {
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, encoded, "", "  "); err != nil {
		return nil, err
	}
	buffer.WriteByte('\n')
	return buffer.Bytes(), nil
}

func decode(name string, data []byte) (any, error) {
	switch {
	case strings.HasPrefix(name, "collector-"):
		return protocol.DecodeCollectorMessage(data)
	case strings.HasPrefix(name, "hub-"):
		return protocol.DecodeHubMessage(data)
	default:
		return nil, fmt.Errorf("fixture name %q has no collector-/hub- direction prefix", name)
	}
}
