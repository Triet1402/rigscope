package activation

import "context"

// Unit names are the stable 1.0 names from docs/rewrite/layout-v1.md.
const (
	HubUnit       = "rigscope-hub.service"
	CollectorUnit = "rigscope-collector.service"
)

// Services is the service-manager surface the transaction needs. It is
// satisfied by *systemd.Exec and *systemd.Fake.
type Services interface {
	Stop(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
	IsActive(ctx context.Context, unit string) (bool, error)
	// UnitRunning is the readiness-grade answer: a unit still activating,
	// deactivating or reloading is not running yet.
	UnitRunning(ctx context.Context, unit string) (bool, error)
}

// Components returns the enabled units in the documented start order: the hub
// first, then the collector.
func (s Settings) Components() []string {
	var units []string
	if s.HubEnabled {
		units = append(units, HubUnit)
	}
	if s.CollectorEnabled {
		units = append(units, CollectorUnit)
	}
	return units
}

// StopOrder returns the enabled units in quiesce order: the collector first,
// then the hub.
func (s Settings) StopOrder() []string {
	var units []string
	if s.CollectorEnabled {
		units = append(units, CollectorUnit)
	}
	if s.HubEnabled {
		units = append(units, HubUnit)
	}
	return units
}
