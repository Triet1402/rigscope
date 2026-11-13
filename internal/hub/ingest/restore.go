package ingest

import (
	"context"

	"github.com/Triet1402/rigscope/internal/hub/live"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// Restore binds the durable registry and the persisted latest readings back
// into live state at startup. Restored machines are not connected: no session
// is created, and no receipt is manufactured.
func Restore(
	ctx context.Context,
	registry *storage.Registry,
	telemetry *storage.Telemetry,
	machines *storage.Machines,
	state *live.State,
) error {
	identities, err := registry.Identities(ctx)
	if err != nil {
		return err
	}
	identityByCollector := make(map[string]storage.Identity, len(identities))
	for _, identity := range identities {
		if err := state.RegisterMachine(identity.CollectorID, identity.MachineID); err != nil {
			return err
		}
		identityByCollector[identity.CollectorID] = identity
	}

	latest, err := telemetry.LatestLive(ctx)
	if err != nil {
		return err
	}
	for _, record := range latest {
		identity, ok := identityByCollector[record.CollectorID]
		if !ok || identity.MachineID != record.MachineID {
			// A revoked or replaced collector keeps its history but no longer
			// appears in live state.
			continue
		}
		record := record
		state.RestoreMetrics(record.MachineID, protocol.MetricsRecord{
			RecordEnvelope: protocol.RecordEnvelope{
				CollectorID: record.CollectorID,
				RunID:       record.RunID,
				BootID:      record.BootID,
				Sequence:    record.Sequence,
				CapturedAt:  record.CapturedAt.Format("2006-01-02T15:04:05.000Z"),
			},
			Payload: record.Payload,
		}, record.ReceivedAt, record.Hostname, record.ExpectedIntervalMs)
	}

	// Inventory and presentation metadata exist even for machines that have
	// not produced a metrics record yet.
	for _, identity := range identities {
		if stored, err := telemetry.LoadInventory(ctx, identity.MachineID); err != nil {
			return err
		} else if stored != nil {
			state.SetInventory(identity.MachineID, stored.Inventory)
		}
		meta, err := machines.Get(ctx, identity.MachineID)
		if err != nil {
			return err
		}
		if meta.DisplayName != nil || len(meta.Tags) > 0 {
			state.SetMetadata(identity.MachineID, meta.DisplayName, meta.Tags)
		}
	}
	return nil
}
