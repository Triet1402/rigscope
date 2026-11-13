package storage

import (
	"testing"
	"time"
)

func TestRingKeepsOneSecondBucketsAndUnavailableReadings(t *testing.T) {
	ring := NewRing(RingOptions{Retention: 10 * time.Second})
	base := testBase.UnixMilli()
	ring.Add("machine-1", "cpu.usage", "", base, f64(10))
	ring.Add("machine-1", "cpu.usage", "", base+200, f64(30))
	ring.Add("machine-1", "cpu.usage", "", base+1000, nil)
	ring.Add("machine-1", "cpu.usage", "", base+1000, f64(20))
	ring.Add("machine-2", "cpu.usage", "", base, f64(99))

	buckets := ring.read("machine-1", "cpu.usage", "", base, base+2000)
	if len(buckets) != 2 {
		t.Fatalf("buckets = %+v", buckets)
	}
	if buckets[0].count != 2 || buckets[0].min != 10 || buckets[0].max != 30 || buckets[0].sum != 40 {
		t.Fatalf("first bucket = %+v", buckets[0])
	}
	if buckets[1].count != 1 || buckets[1].nulls != 1 || buckets[1].sum != 20 {
		t.Fatalf("second bucket = %+v", buckets[1])
	}
	stats := ring.Stats()
	if stats.Capacity != 10 || stats.Machines != 2 || stats.Series != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.OldestMs != base || stats.NewestMs != base+1000 {
		t.Fatalf("range = %d..%d", stats.OldestMs, stats.NewestMs)
	}
}

func TestRingOverwritesBucketsBeyondItsCapacity(t *testing.T) {
	ring := NewRing(RingOptions{Retention: 3 * time.Second})
	base := testBase.UnixMilli()
	ring.Add("machine-1", "cpu.usage", "", base, f64(1))
	ten := base + 10_000
	ring.Add("machine-1", "cpu.usage", "", ten, f64(10))
	buckets := ring.read("machine-1", "cpu.usage", "", base, ten)
	if len(buckets) != 1 {
		t.Fatalf("buckets = %+v", buckets)
	}
	if at := (buckets[0].stamp - 1) * ringStepMS; at != ten {
		t.Fatalf("bucket time = %d, want %d", at, ten)
	}
}

func TestRingDropsLateSamplesInsteadOfCorruptingNewerBuckets(t *testing.T) {
	ring := NewRing(RingOptions{Retention: 3 * time.Second})
	base := testBase.UnixMilli()
	ring.Add("machine-1", "cpu.usage", "", base, f64(1))
	ring.Add("machine-1", "cpu.usage", "", base+10_000, f64(10))
	ring.Add("machine-1", "cpu.usage", "", base+1000, f64(999)) // far too late
	buckets := ring.read("machine-1", "cpu.usage", "", base, base+10_000)
	if len(buckets) != 1 {
		t.Fatalf("buckets = %+v", buckets)
	}
	if at := (buckets[0].stamp - 1) * ringStepMS; at != base+10_000 || buckets[0].sum != 10 {
		t.Fatalf("late sample corrupted the newer bucket: %+v", buckets[0])
	}
}

func TestRingBoundsMachinesSeriesAndBytes(t *testing.T) {
	ring := NewRing(RingOptions{
		Retention:   15 * time.Minute,
		MaxMachines: 8,
		MaxBytes:    200 * 1024,
	})
	base := testBase.UnixMilli()
	for machine := 0; machine < 20; machine++ {
		for series := 0; series < 20; series++ {
			ring.Add("machine-"+itoa(machine), "cpu.core", "core:"+itoa(series), base+int64(machine)*1000, f64(float64(series)))
		}
	}
	stats := ring.Stats()
	if stats.UsedBytes > stats.BudgetBytes {
		t.Fatalf("used %d > budget %d", stats.UsedBytes, stats.BudgetBytes)
	}
	if stats.Machines > 8 {
		t.Fatalf("machines = %d", stats.Machines)
	}
	if stats.EvictedMachines == 0 || stats.RejectedSeries == 0 {
		t.Fatalf("bounds did not evict/reject: %+v", stats)
	}
	if stats.Series == 0 || stats.UsedBytes == 0 {
		t.Fatalf("ring evicted everything: %+v", stats)
	}
	// A single machine that alone exceeds the budget is rejected, not allowed
	// to grow without bound.
	tiny := NewRing(RingOptions{Retention: 15 * time.Minute, MaxBytes: 1024})
	for series := 0; series < 100; series++ {
		tiny.Add("machine", "cpu.usage", "r"+itoa(series), base, f64(1))
	}
	if tiny.Stats().UsedBytes > 1024 || tiny.Stats().RejectedSeries == 0 {
		t.Fatalf("tiny ring stats = %+v", tiny.Stats())
	}
}

func TestRingCoverageStartsAtFirstStoredSample(t *testing.T) {
	ring := NewRing(RingOptions{Retention: 5 * time.Second})
	base := testBase.UnixMilli()
	ring.Add("machine-1", "cpu.usage", "", base+3000, f64(1))
	series := []KnownSeries{{Metric: "cpu.usage"}}
	if ring.covers("machine-1", series, base) {
		t.Fatal("ring claims coverage before its first stored sample")
	}
	if !ring.covers("machine-1", series, base+3000) {
		t.Fatal("ring does not cover its own sample time")
	}
	if ring.covers("machine-1", []KnownSeries{{Metric: "memory.used"}}, base+3000) {
		t.Fatal("ring claims coverage for an unseen series")
	}
	// Once the capacity wraps, coverage moves with the oldest retained slot.
	for index := 1; index <= 8; index++ {
		ring.Add("machine-1", "cpu.usage", "", base+3000+int64(index)*1000, f64(1))
	}
	if ring.covers("machine-1", series, base+3000) {
		t.Fatal("ring claims coverage for buckets it already overwrote")
	}
	if !ring.covers("machine-1", series, base+7000) {
		t.Fatal("ring does not cover its retained window")
	}
}

func TestRingCountRowsIsBounded(t *testing.T) {
	ring := NewRing(RingOptions{Retention: time.Minute})
	base := testBase.UnixMilli()
	for index := 0; index < 60; index++ {
		ring.Add("machine-1", "cpu.usage", "", base+int64(index)*1000, f64(1))
	}
	series := []KnownSeries{{Metric: "cpu.usage", ResourceID: ""}}
	if got := ring.countRows("machine-1", series, base, base+60_000, 1000); got != 60 {
		t.Fatalf("count = %d", got)
	}
	if got := ring.countRows("machine-1", series, base, base+60_000, 10); got <= 10 {
		t.Fatalf("bounded count = %d, want > 10", got)
	}
}
