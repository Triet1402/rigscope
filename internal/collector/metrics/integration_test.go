package metrics

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/policy"
	"github.com/Triet1402/rigscope/internal/collector/processes"
	"github.com/Triet1402/rigscope/internal/collector/sampling/procfs"
	"github.com/Triet1402/rigscope/internal/protocol"
)

type fixtureUsers map[int]string

func (u fixtureUsers) UserName(uid int) (string, bool) {
	name, ok := u[uid]
	return name, ok
}

func TestAssembledProcessDetailRoundTripsThroughProtocol(t *testing.T) {
	clock := newFixedClock()
	scanner, err := processes.NewScanner(processes.ScannerConfig{
		Proc:                os.DirFS(testdataPath("procs", "basic")),
		Users:               fixtureUsers{0: "root", 1000: "alice"},
		Clock:               clock.Now,
		ClockTicksPerSecond: 100,
		PageSize:            4096,
		MaxRows:             10,
	})
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	snapshot, err := scanner.Scan(context.Background(), processes.ScanOptions{IncludeCommand: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(snapshot.Processes) == 0 {
		t.Fatal("fixture scan returned no rows")
	}
	result := processes.DetailResult{
		Type:           "detail.result",
		SubscriptionID: "sub-basic",
		CapturedAt:     "2026-11-12T10:00:00.000Z",
		Snapshot:       snapshot,
	}
	converted := ToProtocolDetailResult(result)

	shared, ok := policy.ApplyDetail(converted, policy.Default())
	if !ok {
		t.Fatal("default policy dropped process detail")
	}
	for _, row := range shared.Snapshot.Processes {
		if row.Command != nil {
			t.Fatalf("command leaked under the default policy: %q", *row.Command)
		}
	}
	if _, err := protocol.EncodeCollectorMessage(shared); err != nil {
		t.Fatalf("encode default detail: %v", err)
	}
	decoded, err := protocol.DecodeCollectorMessage(mustEncode(t, shared))
	if err != nil {
		t.Fatalf("decode default detail: %v", err)
	}
	if decoded.(protocol.DetailResult).Snapshot.TotalProcesses != int64(snapshot.TotalProcesses) {
		t.Fatalf("total processes changed in the round trip")
	}

	withArguments, ok := policy.ApplyDetail(converted, policy.Policy{Processes: true, ProcessArguments: true})
	if !ok {
		t.Fatal("arguments policy dropped process detail")
	}
	hasCommand := false
	for _, row := range withArguments.Snapshot.Processes {
		if row.Command != nil {
			hasCommand = true
			break
		}
	}
	if !hasCommand {
		t.Fatal("fixture rows must carry a command when arguments are allowed")
	}
	if _, err := protocol.EncodeCollectorMessage(withArguments); err != nil {
		t.Fatalf("encode detail with arguments: %v", err)
	}
}

func TestSchedulerTrimmedProcessTableRoundTripsThroughProtocol(t *testing.T) {
	rows := make([]processes.ProcessRow, 7000)
	for index := range rows {
		command := strings.Repeat("x", 4000)
		rows[index] = processes.ProcessRow{
			PID: index + 100, PPID: 1, StartTicks: int64(index + 100),
			Name: "worker", State: "S", Command: &command,
		}
	}
	scanner := &oversizedScanner{snapshot: processes.ProcessSnapshot{
		TotalProcesses: len(rows),
		Processes:      rows,
	}}
	var delivered []processes.DetailMessage
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	var pending func()
	scheduler, err := processes.NewScheduler(processes.SchedulerConfig{
		Scanner: scanner,
		Policy:  func() processes.Policy { return processes.Policy{Processes: true, ProcessArguments: true} },
		Send: func(message processes.DetailMessage) bool {
			delivered = append(delivered, message)
			return true
		},
		Clock:       func() time.Time { return now },
		Now:         func() time.Time { return now },
		MinInterval: time.Second,
		NewTimer: func(_ time.Duration, fn func()) processes.Timer {
			pending = fn
			return manualTimer{}
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	defer scheduler.Stop()
	scheduler.Subscribe(processes.DetailSubscribe{
		Type: "detail.subscribe", SubscriptionID: "sub-huge",
		Kind: "processes", IntervalMS: 1000, LeaseMS: 60000,
	})
	if pending == nil {
		t.Fatal("subscription did not arm a timer")
	}
	pending()
	if len(delivered) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(delivered))
	}
	result, ok := delivered[0].(processes.DetailResult)
	if !ok {
		t.Fatalf("delivered %T, want detail result", delivered[0])
	}
	if !result.Snapshot.Truncated {
		t.Fatal("oversized table was not marked truncated")
	}
	if len(result.Snapshot.Processes) == 0 || len(result.Snapshot.Processes) >= len(rows) {
		t.Fatalf("rows = %d, want a trimmed non-empty prefix", len(result.Snapshot.Processes))
	}
	if len(result.Snapshot.Processes) > protocol.MaxProcessRows {
		t.Fatalf("rows = %d exceeds the protocol limit", len(result.Snapshot.Processes))
	}

	converted := ToProtocolDetailResult(result)
	encoded, err := protocol.EncodeCollectorMessage(converted)
	if err != nil {
		t.Fatalf("encode trimmed detail: %v", err)
	}
	if len(encoded) > protocol.MaxDetailBytes {
		t.Fatalf("encoded detail = %d bytes, limit %d", len(encoded), protocol.MaxDetailBytes)
	}
	decoded, err := protocol.DecodeCollectorMessage(encoded)
	if err != nil {
		t.Fatalf("decode trimmed detail: %v", err)
	}
	if !decoded.(protocol.DetailResult).Snapshot.Truncated {
		t.Fatal("truncated flag lost in the round trip")
	}
}

func TestMaxSizeInventoryRoundTripsThroughProtocol(t *testing.T) {
	inventory := maxInventoryFixture()
	sharing := policy.Default()
	sharing.NetworkAddresses = true
	shared := policy.FilterInventory(inventory, sharing)
	encoded, err := protocol.EncodeCollectorMessage(protocol.InventoryMessage{
		CapturedAt: "2026-11-12T10:00:00.000Z", Inventory: shared,
	})
	if err != nil {
		t.Fatalf("encode max inventory: %v", err)
	}
	decoded, err := protocol.DecodeCollectorMessage(encoded)
	if err != nil {
		t.Fatalf("decode max inventory: %v", err)
	}
	got := decoded.(protocol.InventoryMessage).Inventory
	if len(got.GPUs) != protocol.MaxGPUs || len(got.Filesystems) != protocol.MaxFilesystems ||
		len(got.Disks) != protocol.MaxDisks || len(got.Network) != protocol.MaxInterfaces {
		t.Fatalf("counts = %d/%d/%d/%d", len(got.GPUs), len(got.Filesystems), len(got.Disks), len(got.Network))
	}
	if len(got.Network[0].Addresses) != 2 {
		t.Fatalf("addresses = %d, want the two fixture addresses", len(got.Network[0].Addresses))
	}
}

type oversizedScanner struct {
	snapshot processes.ProcessSnapshot
}

func (s *oversizedScanner) Scan(context.Context, processes.ScanOptions) (processes.ProcessSnapshot, error) {
	return s.snapshot, nil
}

type manualTimer struct{}

func (manualTimer) Stop() {}

func mustEncode(t *testing.T, message protocol.CollectorMessage) []byte {
	t.Helper()
	encoded, err := protocol.EncodeCollectorMessage(message)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return encoded
}

func TestNetworkInventoryCapsAddressesPerInterface(t *testing.T) {
	addresses := make([]procfs.InterfaceAddress, 0, protocol.MaxAddressesPerInterface+1)
	for index := 0; index < protocol.MaxAddressesPerInterface+1; index++ {
		addresses = append(addresses, procfs.InterfaceAddress{
			Family: "ipv4", Address: fmt.Sprintf("192.0.2.%d", index+1), Prefix: 24,
		})
	}
	converted := networkInventory([]procfs.InterfaceInventory{{
		ID: "net:eth0", Name: "eth0", Addresses: addresses, State: "up",
	}})
	if len(converted) != 1 {
		t.Fatalf("interfaces = %d", len(converted))
	}
	if len(converted[0].Addresses) != protocol.MaxAddressesPerInterface {
		t.Fatalf("addresses = %d, want %d", len(converted[0].Addresses), protocol.MaxAddressesPerInterface)
	}
}
