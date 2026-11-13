package storage

import (
	"errors"
	"testing"
	"time"
)

// Removing a machine deletes its telemetry (summary points, series, receipts,
// latest snapshot, inventory, one-second ring) and its control rows
// (registry, grants, metadata, alerts) while leaving other machines intact.
func TestRemoveMachineDeletesEverything(t *testing.T) {
	for _, memory := range []bool{false, true} {
		name := "persistent"
		if memory {
			name = "memory"
		}
		t.Run(name, func(t *testing.T) {
			f := setupTelemetry(t, TelemetryOptions{Memory: memory})
			registry, err := NewRegistry(testContext(t), f.db, RegistryOptions{BootstrapIdentities: []Identity{
				{CollectorID: "collector-1", MachineID: "machine-1", Token: "token-1"},
				{CollectorID: "collector-2", MachineID: "machine-2", Token: "token-2"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			for _, machine := range []struct{ machine, collector string }{{"machine-1", "collector-1"}, {"machine-2", "collector-2"}} {
				batch := liveBatch(makeMetricsRecord(recordOptions{collectorID: machine.collector, sequence: 1, capturedAt: testBase.UnixMilli()}))
				batch.MachineID, batch.CollectorID = machine.machine, machine.collector
				if result := f.ingest(t, batch); len(result.Accepted) != 1 {
					t.Fatalf("%s batch: %+v", machine.machine, result)
				}
			}
			if err := f.store.Flush(testContext(t)); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UnixMilli()
			if err := f.db.Write(testContext(t), func(tx *Tx) error {
				for _, machine := range []string{"machine-1", "machine-2"} {
					if _, err := tx.Exec("INSERT INTO machine_meta (machine_id, display_name, tags_json, updated_at_ms) VALUES (?, 'name', '[]', ?)", machine, now); err != nil {
						return err
					}
					if _, err := tx.Exec("INSERT INTO alert_rules (id, name, machine_id, metric, condition, threshold, created_at_ms, updated_at_ms) VALUES (?, 'r', ?, 'cpu.usage', 'above', 90, ?, ?)", "rule-"+machine, machine, now, now); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			telemetryDB := f.store.db
			for _, machine := range []string{"machine-1", "machine-2"} {
				if scalar(t, telemetryDB, "SELECT COUNT(*) FROM series WHERE machine_id = ?", machine) == 0 {
					t.Fatalf("fixture stored no series for %s", machine)
				}
			}

			// An active collector cannot be removed.
			err = f.db.Write(testContext(t), func(tx *Tx) error {
				_, err := registry.RemoveTx(tx, "owner", "collector-1")
				return err
			})
			if !errors.Is(err, ErrNotRevoked) {
				t.Fatalf("remove active = %v", err)
			}
			if err := registry.Revoke(testContext(t), "owner", "collector-1"); err != nil {
				t.Fatal(err)
			}
			if err := f.store.ForgetMachine(testContext(t), "machine-1"); err != nil {
				t.Fatal(err)
			}
			var machineID string
			if err := f.db.Write(testContext(t), func(tx *Tx) error {
				machineID, err = registry.RemoveTx(tx, "owner", "collector-1")
				return err
			}); err != nil || machineID != "machine-1" {
				t.Fatalf("remove = %q, %v", machineID, err)
			}

			for _, table := range []string{"series", "sample_receipts", "gaps", "machine_latest", "inventory"} {
				if n := scalar(t, telemetryDB, "SELECT COUNT(*) FROM "+table+" WHERE machine_id = 'machine-1'"); n != 0 {
					t.Fatalf("%s kept %d rows for the removed machine", table, n)
				}
			}
			for _, table := range []string{"points_10s", "points_1m", "points_1h"} {
				if n := scalar(t, telemetryDB, "SELECT COUNT(*) FROM "+table+" WHERE series_id NOT IN (SELECT series_id FROM series)"); n != 0 {
					t.Fatalf("%s kept %d orphaned points", table, n)
				}
			}
			if n := scalar(t, telemetryDB, "SELECT COUNT(*) FROM series WHERE machine_id = 'machine-2'"); n == 0 {
				t.Fatal("the other machine's history was deleted")
			}
			for _, query := range []string{
				"SELECT COUNT(*) FROM collector_registry WHERE machine_id = 'machine-1'",
				"SELECT COUNT(*) FROM machine_meta WHERE machine_id = 'machine-1'",
				"SELECT COUNT(*) FROM alert_rules WHERE machine_id = 'machine-1'",
			} {
				if n := scalar(t, f.db, query); n != 0 {
					t.Fatalf("%s = %d", query, n)
				}
			}
			for _, query := range []string{
				"SELECT COUNT(*) FROM collector_registry WHERE machine_id = 'machine-2'",
				"SELECT COUNT(*) FROM machine_meta WHERE machine_id = 'machine-2'",
				"SELECT COUNT(*) FROM alert_rules WHERE machine_id = 'machine-2'",
			} {
				if n := scalar(t, f.db, query); n != 1 {
					t.Fatalf("%s = %d, the other machine must stay", query, n)
				}
			}
			if n := scalar(t, f.db, "SELECT COUNT(*) FROM audit_events WHERE action = 'remove' AND target = 'collector-1'"); n != 1 {
				t.Fatalf("remove audit events = %d", n)
			}
			if f.store.ring.machines["machine-1"] != nil {
				t.Fatal("the one-second ring kept the removed machine")
			}
		})
	}
}
