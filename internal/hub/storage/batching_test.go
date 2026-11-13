package storage

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPersistentDefaultBatchesPastOneSecondAndFlushDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := Open(ctx, privateTempDir(t)+"/hub.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewTelemetry(ctx, db, TelemetryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(context.Background())
	now := time.Now().UTC()
	done := make(chan error, 2)
	for sequence := int64(1); sequence <= 2; sequence++ {
		batch := liveBatch(makeMetricsRecord(recordOptions{sequence: sequence, capturedAt: now.UnixMilli()}))
		go func() {
			result, err := store.Enqueue(ctx, batch)
			if err == nil && len(result.Accepted) != 1 {
				err = fmt.Errorf("accepted %d records, want 1", len(result.Accepted))
			}
			done <- err
		}()
	}
	// The former one-second default would acknowledge during this window.
	select {
	case err := <-done:
		t.Fatalf("acknowledged before the longer batch window: %v", err)
	case <-time.After(1200 * time.Millisecond):
	}
	if scalar(t, db, "SELECT COUNT(*) FROM sample_receipts") != 0 {
		t.Fatal("batch committed early")
	}
	if err := store.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if scalar(t, db, "SELECT COUNT(*) FROM sample_receipts") != 2 {
		t.Fatal("flush did not commit both queued records")
	}
}
