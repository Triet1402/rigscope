package storage

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// BenchmarkIngestFleet measures steady-state summary ingestion for a small
// fleet. Each iteration writes four machines × 250 one-second samples through
// batches of 32 records, which is the hub's contract batch size.
func BenchmarkIngestFleet(b *testing.B) {
	const (
		machines  = 4
		samples   = 250
		batchSize = 32
	)
	dir := privateTempDir(b)
	db, err := Open(context.Background(), dir+"/hub.db", Options{})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer db.Close()
	store, err := NewTelemetry(context.Background(), db, TelemetryOptions{
		FlushInterval:            time.Millisecond,
		MaxQueuedRecords:         20_000,
		MaxRecordsPerTransaction: 512,
		MaxPhysicalBytes:         negativeInt64(),
	})
	if err != nil {
		b.Fatalf("telemetry: %v", err)
	}
	defer store.Close(context.Background())

	payloads := make([]protocol.MetricsPayload, machines)
	payloadOptions := payloadOptions{
		cores: 48,
		gpus: []gpuFixture{
			{id: "gpu-0", utilization: f64(50), memoryUsed: i64(1024), memoryTotal: i64(4096), temperature: f64(60), power: f64(200)},
			{id: "gpu-1", utilization: f64(40), memoryUsed: i64(2048), memoryTotal: i64(4096), temperature: f64(55), power: f64(180)},
		},
		filesystems: func() []filesystemFixture {
			out := make([]filesystemFixture, 10)
			for index := range out {
				out[index] = filesystemFixture{id: "fs-" + itoa(index), used: int64(index * 100), total: 10_000}
			}
			return out
		}(),
		disks: func() []diskFixture {
			out := make([]diskFixture, 10)
			for index := range out {
				out[index] = diskFixture{id: "disk-" + itoa(index), read: float64(index * 10), write: float64(index * 5)}
			}
			return out
		}(),
		interfaces: func() []interfaceFixture {
			out := make([]interfaceFixture, 10)
			for index := range out {
				out[index] = interfaceFixture{id: "eth-" + itoa(index), rx: float64(index * 100), tx: float64(index * 50)}
			}
			return out
		}(),
	}
	for machine := range payloads {
		payloads[machine] = makePayload(payloadOptions)
	}

	// Capture times must sit inside the replay horizon (24h by default), so use
	// the wall clock rather than the fixed fixture date.
	base := time.Now().UTC().Truncate(time.Second)
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		var records int64
		runID := fmt.Sprintf("run-%d", iteration)
		started := time.Now()
		var wait sync.WaitGroup
		var once sync.Once
		var firstErr error
		for machine := 0; machine < machines; machine++ {
			wait.Add(1)
			go func(machine int) {
				defer wait.Done()
				for offset := 0; offset < samples; offset += batchSize {
					count := batchSize
					if offset+count > samples {
						count = samples - offset
					}
					batch := Batch{
						MachineID:   fmt.Sprintf("bench-machine-%d", machine),
						CollectorID: fmt.Sprintf("bench-collector-%d", machine),
						Mode:        protocol.SampleModeLive,
					}
					for index := 0; index < count; index++ {
						sequence := int64(offset + index + 1)
						record := makeMetricsRecord(recordOptions{
							collectorID: batch.CollectorID,
							runID:       runID,
							sequence:    sequence,
							capturedAt:  base.Add(time.Duration(sequence) * time.Second).UnixMilli(),
							payload:     &payloads[machine],
						})
						batch.Records = append(batch.Records, record)
					}
					if _, err := store.Enqueue(context.Background(), batch); err != nil {
						once.Do(func() { firstErr = err })
						return
					}
					atomic.AddInt64(&records, int64(count))
				}
			}(machine)
		}
		wait.Wait()
		if firstErr != nil {
			b.Fatalf("enqueue: %v", firstErr)
		}
		if err := store.Flush(context.Background()); err != nil {
			b.Fatalf("flush: %v", err)
		}
		elapsed := time.Since(started)
		b.ReportMetric(float64(records)/elapsed.Seconds(), "records/s")
	}
	stats := store.Stats()
	b.ReportMetric(float64(stats.DBBytes)/1024/1024, "db-MiB")
	b.ReportMetric(float64(stats.DBWALBytes)/1024/1024, "wal-MiB")
}

func negativeInt64() *int64 {
	value := int64(-1)
	return &value
}
