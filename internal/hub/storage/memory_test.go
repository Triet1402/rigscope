package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryTelemetryIsolatedQueryableDeduplicatedAndLostOnRestart(t *testing.T) {
	f := setupTelemetry(t, TelemetryOptions{})
	// A mode switch must preserve existing disk history without loading it.
	f.ingest(t, liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli()})))
	open := func() *Telemetry {
		s, err := NewTelemetry(testContext(t), f.db, TelemetryOptions{Memory: true, Now: f.clock.now, FlushInterval: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close(context.Background()) })
		return s
	}
	s := open()
	if s.Persistent() || s.db.Path() != ":memory:" {
		t.Fatal("memory telemetry is durable")
	}
	if latest, err := s.LatestLive(testContext(t)); err != nil || len(latest) != 0 {
		t.Fatalf("loaded disk snapshot: %+v, %v", latest, err)
	}
	batch := liveBatch(makeMetricsRecord(recordOptions{sequence: 2, capturedAt: testBase.Add(time.Second).UnixMilli()}))
	for i := 0; i < 2; i++ {
		result, err := s.Enqueue(testContext(t), batch)
		if err != nil || len(result.Accepted) != 1 {
			t.Fatalf("enqueue: %+v, %v", result, err)
		}
	}
	if s.Stats().CommittedRecords != 1 || s.Stats().DuplicateRecords != 1 {
		t.Fatalf("dedup: %+v", s.Stats())
	}
	if scalar(t, s.db, "SELECT COUNT(*) FROM points_10s") == 0 {
		t.Fatal("RAM summaries missing")
	}
	result, err := s.Query(testContext(t), HistoryQuery{MachineID: "machine-1", Metrics: []string{"cpu.usage"}, FromMs: testBase.Add(-time.Second).UnixMilli(), ToMs: testBase.Add(time.Minute).UnixMilli()}, HistoryQueryBudget{})
	if err != nil || len(result.Series) == 0 {
		t.Fatalf("query: %+v, %v", result, err)
	}
	if err := s.SaveInventory(testContext(t), "machine-1", testBase, makeInventoryFixture()); err != nil {
		t.Fatal(err)
	}
	if scalar(t, f.db, "SELECT COUNT(*) FROM sample_receipts") != 1 || scalar(t, f.db, "SELECT COUNT(*) FROM inventory") != 0 {
		t.Fatal("RAM telemetry changed durable history")
	}
	if s.Stats().DBLimitBytes != DefaultMemoryDatabaseBytes {
		t.Fatalf("RAM limit: %+v", s.Stats())
	}
	if err := s.Close(testContext(t)); err != nil {
		t.Fatal(err)
	}
	restarted := open()
	if scalar(t, restarted.db, "SELECT COUNT(*) FROM sample_receipts") != 0 {
		t.Fatal("RAM receipts survived restart")
	}
	if inv, err := restarted.LoadInventory(testContext(t), "machine-1"); err != nil || inv != nil {
		t.Fatalf("RAM inventory survived restart: %+v %v", inv, err)
	}
	if scalar(t, f.db, "SELECT COUNT(*) FROM sample_receipts") != 1 {
		t.Fatal("persistent history removed")
	}
}

func TestMemoryTelemetryRejectsCredentialRevokedWhileQueued(t *testing.T) {
	f := setupTelemetry(t, TelemetryOptions{Memory: true, FlushInterval: 5 * time.Second})
	registry, err := NewRegistry(testContext(t), f.db, RegistryOptions{BootstrapIdentities: []Identity{{CollectorID: "collector-1", MachineID: "machine-1", Token: "test-token"}}})
	if err != nil {
		t.Fatal(err)
	}
	batch := liveBatch(makeMetricsRecord(recordOptions{sequence: 1, capturedAt: testBase.UnixMilli()}))
	batch.CredentialHash = hashToken("test-token")
	done := make(chan error, 1)
	go func() { _, err := f.store.Enqueue(testContext(t), batch); done <- err }()
	deadline := time.Now().Add(time.Second)
	for f.store.Stats().QueuedRecords == 0 {
		if time.Now().After(deadline) {
			t.Fatal("batch never queued")
		}
		time.Sleep(time.Millisecond)
	}
	if err := registry.Revoke(testContext(t), "owner", "collector-1"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Flush(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrCredentialSuperseded) {
		t.Fatalf("queued revoked batch: %v", err)
	}
	if scalar(t, f.store.db, "SELECT COUNT(*) FROM sample_receipts") != 0 {
		t.Fatal("revoked batch accepted into RAM")
	}
}
