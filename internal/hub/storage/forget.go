package storage

import (
	"context"
	"database/sql"
	"errors"
)

// ErrNotRevoked means a collector must be revoked before it can be removed.
var ErrNotRevoked = errors.New("revoke the collector before removing it")

// ForgetMachine deletes every telemetry row stored for machineID: summary
// points, the series catalog, receipts, gaps, the latest snapshot and the
// inventory, plus the in-memory one-second ring. It is idempotent. Callers
// remove the machine only after its collector was revoked, so no new batches
// for it arrive; queued ones are flushed first so none lands afterwards.
func (t *Telemetry) ForgetMachine(ctx context.Context, machineID string) error {
	if err := t.Flush(ctx); err != nil {
		return err
	}
	var ids []int64
	err := t.db.Write(ctx, func(tx *Tx) error {
		rows, err := tx.Query("SELECT series_id FROM series WHERE machine_id = ?", machineID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, statement := range []string{
			"DELETE FROM points_10s WHERE series_id IN (SELECT series_id FROM series WHERE machine_id = ?)",
			"DELETE FROM points_1m WHERE series_id IN (SELECT series_id FROM series WHERE machine_id = ?)",
			"DELETE FROM points_1h WHERE series_id IN (SELECT series_id FROM series WHERE machine_id = ?)",
			"DELETE FROM series WHERE machine_id = ?",
			"DELETE FROM sample_receipts WHERE machine_id = ?",
			"DELETE FROM gaps WHERE machine_id = ?",
			"DELETE FROM machine_latest WHERE machine_id = ?",
			"DELETE FROM inventory WHERE machine_id = ?",
		} {
			if _, err := tx.Exec(statement, machineID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	t.catalog.removeIDs(ids)
	t.ring.Forget(machineID)
	return nil
}

// Forget drops one machine's one-second history from the ring.
func (r *Ring) Forget(machineID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	machine := r.machines[machineID]
	if machine == nil {
		return
	}
	r.bytes -= int64(len(machine.series)) * r.seriesBytes()
	delete(r.machines, machineID)
}

// RemoveTx deletes a revoked collector and everything the hub keeps for its
// machine in the control database: the registry row, its grants, the
// machine's name and tags, and its alert rules, incidents, pending
// notifications and progress. Telemetry is removed separately with
// Telemetry.ForgetMachine. It returns the machine id. An active collector is
// refused with ErrNotRevoked.
func (r *Registry) RemoveTx(tx *Tx, actor, collectorID string) (string, error) {
	if err := validateCollectorID(collectorID); err != nil {
		return "", err
	}
	var machineID string
	var revoked int
	err := tx.QueryRow("SELECT machine_id, revoked FROM collector_registry WHERE collector_id = ?", collectorID).Scan(&machineID, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", invalidError("collector does not exist")
	}
	if err != nil {
		return "", err
	}
	if revoked == 0 {
		return "", ErrNotRevoked
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"DELETE FROM collector_registry WHERE collector_id = ?", []any{collectorID}},
		{"DELETE FROM collector_grants WHERE consumed_by = ? OR collector_id = ? OR replace_machine_id = ?", []any{collectorID, collectorID, machineID}},
		{"DELETE FROM machine_meta WHERE machine_id = ?", []any{machineID}},
		{"DELETE FROM alert_observed_resources WHERE rule_id IN (SELECT id FROM alert_rules WHERE machine_id = ?)", []any{machineID}},
		{"DELETE FROM alert_jobs WHERE machine_id = ?", []any{machineID}},
		{"DELETE FROM alert_incidents WHERE machine_id = ?", []any{machineID}},
		{"DELETE FROM alert_rules WHERE machine_id = ?", []any{machineID}},
		{"DELETE FROM alert_machine_progress WHERE machine_id = ?", []any{machineID}},
	} {
		if _, err := tx.Exec(statement.query, statement.args...); err != nil {
			return "", err
		}
	}
	if err := appendAuditTx(tx, AuditScopeCollector, actor, "remove", collectorID, r.now()); err != nil {
		return "", err
	}
	return machineID, nil
}
