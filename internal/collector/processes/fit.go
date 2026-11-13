package processes

import (
	"encoding/json"
)

// fitPlan is the shared byte-fitting work for one scanned snapshot.
//
// The expensive parts - sorting the rows and measuring each row's JSON encoding
// - happen once, outside the scheduler's mutex. Every subscription of that
// snapshot then only picks its fitting prefix with a binary search over
// cumulative row sizes plus the envelope size. The plan never mutates the
// snapshot it was built from; the two variants (as scanned, and with command
// lines stripped) share the same ranking.
type fitPlan struct {
	source ProcessSnapshot
	limit  int

	// ranked is the snapshot's rows in truncation order. rankedSizes holds the
	// cumulative encoded size of ranked, so rankedSizes[i] is the total JSON
	// bytes of rows[:i] including separators.
	ranked      []ProcessRow
	rankedSizes []int

	// stripped is ranked with every command removed. It is nil when no row
	// carries a command, in which case both variants are identical.
	stripped      []ProcessRow
	strippedSizes []int
}

// newFitPlan prepares the shared fitting work for snapshot. limit <= 0 means
// no byte fitting is wanted; the plan still provides the policy variant.
func newFitPlan(snapshot ProcessSnapshot, limit int) *fitPlan {
	plan := &fitPlan{source: snapshot, limit: limit}
	plan.ranked = append([]ProcessRow(nil), snapshot.Processes...)
	rankRows(plan.ranked)

	hasCommand := false
	for index := range plan.ranked {
		if plan.ranked[index].Command != nil {
			hasCommand = true
			break
		}
	}
	if hasCommand {
		plan.stripped = make([]ProcessRow, len(plan.ranked))
		for index, row := range plan.ranked {
			row.Command = nil
			plan.stripped[index] = row
		}
	}
	if limit > 0 {
		plan.rankedSizes = cumulativeRowSizes(plan.ranked)
		if plan.stripped != nil {
			plan.strippedSizes = cumulativeRowSizes(plan.stripped)
		}
	}
	return plan
}

// fitted returns the snapshot for one result: the best-ranked rows that fit
// within the plan's byte limit. withCommands selects the variant; when commands
// were not scanned or are now withheld, the stripped rows are used. A snapshot
// that already fits is returned with its original truncated flag.
func (p *fitPlan) fitted(subscriptionID, capturedAt string, withCommands bool) ProcessSnapshot {
	rows, cumulative := p.ranked, p.rankedSizes
	if !withCommands && p.stripped != nil {
		rows, cumulative = p.stripped, p.strippedSizes
	}
	if p.limit <= 0 {
		return p.snapshot(rows, p.source.Truncated)
	}

	envelope := p.envelopeSize(subscriptionID, capturedAt, p.source.Truncated)
	if cumulative[len(cumulative)-1] <= p.limit-envelope {
		return p.snapshot(rows, p.source.Truncated)
	}

	// The encoded size grows monotonically with the row count, so find the
	// largest prefix that fits. A dropped row sets truncated. The subtraction
	// keeps an unmeasurable row (maxInt) from overflowing the comparison.
	truncatedEnvelope := p.envelopeSize(subscriptionID, capturedAt, true)
	available := p.limit - truncatedEnvelope
	low, high := 0, len(rows)
	for low < high {
		middle := (low + high + 1) / 2
		if cumulative[middle] <= available {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return p.snapshot(rows[:low], true)
}

func (p *fitPlan) snapshot(rows []ProcessRow, truncated bool) ProcessSnapshot {
	return ProcessSnapshot{
		TotalProcesses: p.source.TotalProcesses,
		Truncated:      truncated,
		Inaccessible:   p.source.Inaccessible,
		Processes:      rows,
	}
}

// envelopeSize is the encoded size of a detail result with no rows. A result
// with rows is exactly this much plus the cumulative row size, because
// "processes":[] grows to "processes":[row,row].
func (p *fitPlan) envelopeSize(subscriptionID, capturedAt string, truncated bool) int {
	encoded, err := json.Marshal(DetailResult{
		Type:           "detail.result",
		SubscriptionID: subscriptionID,
		CapturedAt:     capturedAt,
		Snapshot: ProcessSnapshot{
			TotalProcesses: p.source.TotalProcesses,
			Truncated:      truncated,
			Inaccessible:   p.source.Inaccessible,
			Processes:      []ProcessRow{},
		},
	})
	if err != nil {
		return maxInt
	}
	return len(encoded)
}

const maxInt = int(^uint(0) >> 1)

// cumulativeRowSizes measures every row's JSON encoding exactly once and
// returns prefix sums, with the one-byte separators between rows included.
// A row that cannot be encoded makes its suffix unmeasurable; those rows are
// never selected.
func cumulativeRowSizes(rows []ProcessRow) []int {
	sizes := make([]int, len(rows)+1)
	writer := &countingWriter{}
	encoder := json.NewEncoder(writer)
	broken := false
	for index := range rows {
		if broken {
			sizes[index+1] = maxInt
			continue
		}
		writer.count = 0
		if err := encoder.Encode(rows[index]); err != nil {
			broken = true
			sizes[index+1] = maxInt
			continue
		}
		// Encode writes the value plus one trailing newline.
		size := writer.count - 1
		if index > 0 {
			size++
		}
		sizes[index+1] = sizes[index] + size
	}
	return sizes
}

// countingWriter counts encoded bytes without copying or retaining them.
type countingWriter struct {
	count int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.count += len(p)
	return len(p), nil
}

// fitSnapshotToBytes fits one subscription's result. It is the single-shot
// path; the scheduler shares a fitPlan across subscriptions instead.
func fitSnapshotToBytes(subscriptionID, capturedAt string, snapshot ProcessSnapshot, limit int) ProcessSnapshot {
	return newFitPlan(snapshot, limit).fitted(subscriptionID, capturedAt, true)
}

// detailSize is the exact encoded size of one detail result.
func detailSize(subscriptionID, capturedAt string, snapshot ProcessSnapshot) int {
	encoded, err := json.Marshal(DetailResult{
		Type:           "detail.result",
		SubscriptionID: subscriptionID,
		CapturedAt:     capturedAt,
		Snapshot:       snapshot,
	})
	if err != nil {
		return maxInt
	}
	return len(encoded)
}
