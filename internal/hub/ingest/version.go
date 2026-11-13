package ingest

import (
	"fmt"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// UnsupportedProtocolError reports a hello whose highest supported version is
// below the hub's compatibility window. It is a policy failure, not a parse
// failure: the hub closes with an application close code and a human-readable
// reason until a typed welcome error ships as a planned protocol extension.
type UnsupportedProtocolError struct {
	Requested int64
	Min       int64
	Max       int64
}

func (e *UnsupportedProtocolError) Error() string {
	return fmt.Sprintf(
		"unsupported protocol version %d; this hub supports %d..%d, upgrade the collector",
		e.Requested, e.Min, e.Max,
	)
}

// SupportedProtocolRange returns the versions this hub accepts: the current
// version and the previous one.
func SupportedProtocolRange() (minVersion, maxVersion int64) {
	maxVersion = int64(protocol.Version)
	minVersion = maxVersion - 1
	if minVersion < 1 {
		minVersion = 1
	}
	return minVersion, maxVersion
}

// NegotiateProtocol selects the session version for one hello. A missing
// marker means version 1, the unversioned contract older peers speak. The
// collector advertises its highest version, not a demand, so a newer collector
// is answered with the newest version this hub supports; only a version below
// the compatibility window is refused.
//
// present reports whether the welcome should carry a protocol marker: peers
// that never sent one keep the unversioned welcome shape.
func NegotiateProtocol(marker *int64, minVersion, maxVersion int64) (selected int64, present bool, err error) {
	requested := int64(1)
	if marker != nil {
		requested = *marker
		present = true
	}
	if maxVersion < minVersion || minVersion < 1 {
		return 0, false, fmt.Errorf("hub protocol window %d..%d is invalid", minVersion, maxVersion)
	}
	selected = requested
	if selected > maxVersion {
		selected = maxVersion
	}
	if selected < minVersion {
		return 0, false, &UnsupportedProtocolError{Requested: requested, Min: minVersion, Max: maxVersion}
	}
	return selected, present, nil
}
