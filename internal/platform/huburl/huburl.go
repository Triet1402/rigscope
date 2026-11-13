// Package huburl owns the one collector hub-endpoint policy shared by the
// configuration file and the collector's private state files.
//
// The policy is:
//   - ws:// or wss:// only (the friendly https:// and http:// spellings are
//     accepted by Normalize and become wss:// and ws://);
//   - no userinfo, query or fragment (credentials travel in a header);
//   - the path is exactly /api/collector/connect;
//   - for plaintext ws:// the loopback names localhost, ip6-localhost and
//     localhost6 are stored as the literal addresses 127.0.0.1 and ::1, so a
//     plaintext endpoint never depends on name resolution;
//   - wss:// keeps its hostname unchanged, because TLS verifies the
//     certificate (and sends SNI) for exactly that name: a certificate issued
//     for localhost does not cover 127.0.0.1;
//   - plaintext ws:// is restricted to literal loopback addresses.
//
// Normalize is the only implementation of these rules. config.NormalizeHubURL,
// config.Destination.Validate and state.ValidateHubURL all call it, so a value
// accepted in the configuration file is accepted by the collector and vice
// versa.
package huburl

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ConnectPath is the one collector WebSocket endpoint.
const ConnectPath = "/api/collector/connect"

// MaxLength bounds a hub endpoint. It is generous for a URL and small enough
// that a corrupted state file cannot grow without limit.
const MaxLength = 2048

// Normalize returns the canonical collector endpoint for raw. It accepts the
// user-facing https:// and http:// spellings as well as ws:// and wss://. The
// result is the stored form: a WebSocket scheme, a literal loopback address
// when a plaintext input named one, and the collector connect path.
func Normalize(raw string) (string, error) {
	return normalize(raw, true)
}

// Validate returns the canonical collector endpoint for a value that must
// already be a WebSocket URL. It shares Normalize's host, path and plaintext
// policy; only the friendly http(s) spelling is refused.
func Validate(raw string) (string, error) {
	return normalize(raw, false)
}

func normalize(raw string, allowFriendlyScheme bool) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("hub url must be a non-empty string")
	}
	if len(trimmed) > MaxLength {
		return "", fmt.Errorf("hub url must be at most %d characters", MaxLength)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("hub url is not a valid URL")
	}
	if parsed.Opaque != "" {
		return "", errors.New("hub url is not a valid URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "wss":
		parsed.Scheme = "wss"
	case "ws":
		parsed.Scheme = "ws"
	case "https":
		if !allowFriendlyScheme {
			return "", errors.New("hub url must use ws:// or wss://")
		}
		parsed.Scheme = "wss"
	case "http":
		if !allowFriendlyScheme {
			return "", errors.New("hub url must use ws:// or wss://")
		}
		parsed.Scheme = "ws"
	default:
		if allowFriendlyScheme {
			return "", errors.New("hub url must use ws:// or wss:// (https:// and http:// are normalized)")
		}
		return "", errors.New("hub url must use ws:// or wss://")
	}
	if parsed.User != nil {
		return "", errors.New("hub url must not embed credentials; send the token in the Authorization header")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "", errors.New("hub url must not include a query string")
	}
	if parsed.Fragment != "" {
		return "", errors.New("hub url must not include a fragment")
	}
	hostname := parsed.Hostname()
	if hostname == "" {
		return "", errors.New("hub url must include a host")
	}
	// Only plaintext endpoints are rewritten: a wss:// hostname is the TLS
	// identity, so replacing it would break certificate verification and SNI.
	if canonical := canonicalHost(hostname); parsed.Scheme == "ws" && canonical != hostname {
		host := canonical
		if strings.Contains(canonical, ":") {
			host = "[" + canonical + "]"
		}
		if port := parsed.Port(); port != "" {
			host += ":" + port
		}
		parsed.Host = host
		hostname = canonical
	}
	switch parsed.Path {
	case "", "/":
		parsed.Path = ConnectPath
	case ConnectPath:
	default:
		return "", fmt.Errorf("hub url path must be exactly %s", ConnectPath)
	}
	if parsed.Scheme == "ws" && !IsLoopbackHost(hostname) {
		return "", errors.New("plaintext ws:// is restricted to literal loopback addresses; use wss:// instead")
	}
	return parsed.String(), nil
}

// canonicalHost maps the reserved loopback names onto literal loopback
// addresses. Any other host, including another DNS name, is returned unchanged.
func canonicalHost(host string) string {
	switch strings.ToLower(host) {
	case "localhost":
		return "127.0.0.1"
	case "ip6-localhost", "localhost6":
		return "::1"
	default:
		return host
	}
}

// IsLoopbackHost reports whether host is a literal loopback address. DNS names
// are never resolved, so a name that merely looks local is not accepted.
func IsLoopbackHost(host string) bool {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if trimmed == "::1" {
		return true
	}
	ip := net.ParseIP(trimmed)
	return ip != nil && ip.IsLoopback()
}
