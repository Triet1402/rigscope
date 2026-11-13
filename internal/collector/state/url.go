package state

import (
	"github.com/Triet1402/rigscope/internal/platform/huburl"
)

// ConnectPath is the one collector WebSocket endpoint.
const ConnectPath = huburl.ConnectPath

// ValidateHubURL checks a hub connection URL and returns it normalized.
//
// It applies the shared huburl policy: ws:// or wss:// only (an http(s)
// spelling is normalized), no userinfo, query or fragment (credentials travel
// in a header), the path is exactly /api/collector/connect, and plaintext ws://
// is restricted to literal loopback addresses. For plaintext ws:// the loopback
// names localhost, ip6-localhost and localhost6 are stored as 127.0.0.1 and
// ::1; a wss:// hostname is kept because it is the TLS identity.
//
// Error messages never echo the URL, which may contain a credential by mistake.
func ValidateHubURL(raw string) (string, error) {
	return huburl.Normalize(raw)
}

func validateHubURL(raw string) error {
	_, err := ValidateHubURL(raw)
	return err
}

// IsLoopbackHost reports whether a URL host is a literal loopback address.
// DNS names are never resolved, so "localhost" is deliberately not accepted
// here: ValidateHubURL stores it as 127.0.0.1 instead, while this predicate
// only answers for an already canonical host.
func IsLoopbackHost(host string) bool {
	return huburl.IsLoopbackHost(host)
}

func isLoopbackHost(host string) bool { return IsLoopbackHost(host) }

// canonicalURL is the identity under which a hub endpoint's credential and
// enrollment proof are stored. Entries written before the shared URL policy
// (for example ws://localhost:8081/...) resolve to the same identity as the
// normalized destination in the configuration file, so normalizing the
// configuration never strands an existing credential. A value the policy
// rejects is returned unchanged and is refused by validation later.
func canonicalURL(raw string) string {
	if normalized, err := ValidateHubURL(raw); err == nil {
		return normalized
	}
	return raw
}
