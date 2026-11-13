package ingest_test

import (
	"context"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/hub/ingest"
	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
)

func TestRestoreRebindsPersistedState(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, ":memory:", storage.Options{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	identity := storage.Identity{CollectorID: "c-restore", MachineID: "m-restore", Token: "token-restore"}
	registry, err := storage.NewRegistry(ctx, db, storage.RegistryOptions{BootstrapIdentities: []storage.Identity{identity}})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	telemetry, err := storage.NewTelemetry(ctx, db, storage.TelemetryOptions{FlushInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("open telemetry: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = telemetry.Close(closeCtx)
	})

	captured := time.Now().UTC().Add(-5 * time.Second)
	interval := int64(2000)
	hostname := "rig-restore"
	if _, err := telemetry.Enqueue(ctx, storage.Batch{
		MachineID:          identity.MachineID,
		CollectorID:        identity.CollectorID,
		Mode:               protocol.SampleModeLive,
		ExpectedIntervalMs: &interval,
		Hostname:           &hostname,
		Records: []protocol.TelemetryRecord{
			metricsRecord(t, identity.CollectorID, "run-restore", "boot-restore", 4, captured),
		},
	}); err != nil {
		t.Fatalf("enqueue sample: %v", err)
	}
	if err := telemetry.SaveInventory(ctx, identity.MachineID, captured, inventoryFixture(t)); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	name := "workshop"
	tags := []string{"lab"}
	if err := storage.NewMachines(db).Update(ctx, identity.MachineID, storage.MetaUpdate{DisplayName: &name, Tags: &tags}); err != nil {
		t.Fatalf("save metadata: %v", err)
	}

	state := live.NewState("hub-run-restore", nil)
	if err := ingest.Restore(ctx, registry, telemetry, storage.NewMachines(db), state); err != nil {
		t.Fatalf("restore: %v", err)
	}
	snapshot := state.Snapshot()
	if len(snapshot.Machines) != 1 {
		t.Fatalf("machines = %d", len(snapshot.Machines))
	}
	machine := snapshot.Machines[0]
	if machine.Connected {
		t.Fatalf("a restored machine must not appear connected")
	}
	if machine.Metrics == nil || machine.Metrics.Sequence != 4 {
		t.Fatalf("metrics were not restored: %+v", machine.Metrics)
	}
	if machine.Metrics.CollectorID != identity.CollectorID || machine.Metrics.RunID != "run-restore" {
		t.Fatalf("restored identity = %+v", machine.Metrics)
	}
	if machine.ExpectedIntervalMs == nil || !machine.ExpectedIntervalMs.Valid || machine.ExpectedIntervalMs.Value != 2000 {
		t.Fatalf("cadence was not restored: %+v", machine.ExpectedIntervalMs)
	}
	if machine.LastReceivedAt == nil || machine.Metrics.ReceivedAt != *machine.LastReceivedAt {
		t.Fatalf("receipt was not restored consistently")
	}
	if machine.Inventory == nil {
		t.Fatalf("inventory was not restored")
	}
	if machine.DisplayName == nil || !machine.DisplayName.Valid || machine.DisplayName.Value != "workshop" {
		t.Fatalf("display name was not restored: %+v", machine.DisplayName)
	}
	if machine.Tags == nil || len(*machine.Tags) != 1 || (*machine.Tags)[0] != "lab" {
		t.Fatalf("tags were not restored: %+v", machine.Tags)
	}
}
