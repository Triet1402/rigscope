package auth

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// OriginChecker is the only trusted-origin signal for browser mutations. It is
// deliberately an interface so handlers can be unit-tested with a fixed rule
// set without starting the hub listener.
type OriginChecker interface {
	// Trusted reports whether the request carries an Origin that exactly
	// matches this hub.
	Trusted(r *http.Request) bool
}

// OriginRules implements OriginChecker from static hub configuration. Remote
// mode (an explicit public origin) requires the exact canonical HTTPS origin
// and Host; loopback mode accepts http on an allowlisted loopback hostname
// with the actually bound port, and a Host header naming the same authority.
type OriginRules struct {
	remote *remoteOrigin
	scheme string
	port   int

	hostnames map[string]struct{}
}

type remoteOrigin struct {
	origin string
	host   string
}

// NewOriginRules builds the trusted-origin rules. bindHost and port are the
// values the listener actually uses; for port 0 call SetPort after Listen.
func NewOriginRules(origin, bindHost string, port int, tlsEnabled bool) (*OriginRules, error) {
	rules := &OriginRules{
		scheme:    "http",
		port:      port,
		hostnames: map[string]struct{}{"localhost": {}, "127.0.0.1": {}, "::1": {}, "[::1]": {}},
	}
	if tlsEnabled {
		rules.scheme = "https"
	}
	if host := strings.Trim(bindHost, "[]"); host != "" {
		rules.hostnames[strings.ToLower(host)] = struct{}{}
	}
	if origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("auth: invalid public origin %q", origin)
		}
		rules.remote = &remoteOrigin{origin: origin, host: parsed.Host}
	}
	return rules, nil
}

// SetPort records the port actually bound (port 0 selects an ephemeral one).
func (r *OriginRules) SetPort(port int) { r.port = port }

// Trusted implements OriginChecker.
func (r *OriginRules) Trusted(req *http.Request) bool {
	if r == nil || req == nil {
		return false
	}
	origins := req.Header.Values("Origin")
	if len(origins) != 1 {
		return false
	}
	origin := origins[0]
	if origin == "" || len(origin) > 256 {
		return false
	}
	if r.remote != nil {
		return origin == r.remote.origin && strings.EqualFold(req.Host, r.remote.host)
	}
	return r.trustedLoopback(req, origin)
}

func (r *OriginRules) trustedLoopback(req *http.Request, origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" {
		return false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return false
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return false
	}
	if parsed.Host == "" || parsed.RawPath != "" {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	if _, ok := r.hostnames[hostname]; !ok {
		return false
	}
	if parsed.Port() != "" {
		if parsed.Port() != strconv.Itoa(r.port) {
			return false
		}
	} else if r.port != 80 {
		return false
	}
	return strings.EqualFold(parsed.Host, req.Host)
}
