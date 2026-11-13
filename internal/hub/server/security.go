package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/Triet1402/rigscope/internal/config"
)

// remoteOrigin is the exact canonical origin a hub behind TLS termination
// serves. It is a static trust input, never derived from request headers.
type remoteOrigin struct {
	origin   string
	host     string
	hostname string
}

// ipRange is one trusted proxy address or CIDR range.
type ipRange struct {
	ip      net.IP
	network *net.IPNet
}

func (r ipRange) contains(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if r.ip != nil {
		return r.ip.Equal(ip)
	}
	if r.network == nil {
		return false
	}
	return r.network.Contains(ip)
}

// securityPolicy owns the checks applied before every handler: request target
// shape, canonical Host, browser Origin, forwarded-header trust, body limits
// and the shared security headers.
type securityPolicy struct {
	mu sync.RWMutex

	remote   *remoteOrigin
	loopback bool
	bindHost string
	scheme   string
	hsts     bool

	port              int
	allowedHosts      map[string]struct{}
	loopbackHostnames map[string]struct{}

	forwardedFamily string
	trusted         []ipRange
	maxBodyBytes    int64
	logger          *slog.Logger
}

func newSecurityPolicy(hub hostSettings, loopback bool, tlsEnabled bool, trusted []ipRange, maxBodyBytes int64) (*securityPolicy, error) {
	family := hub.ForwardedHeaders
	if family == "" {
		family = config.DefaultForwardedHeaders
	}
	if family != config.ForwardedHeadersXForwarded && family != config.ForwardedHeadersForwarded {
		return nil, fmt.Errorf("server: forwardedHeaders %q must be %q or %q", family, config.ForwardedHeadersXForwarded, config.ForwardedHeadersForwarded)
	}
	policy := &securityPolicy{
		loopback:        loopback,
		bindHost:        strings.Trim(hub.Listen, "[]"),
		scheme:          "http",
		port:            hub.Port,
		forwardedFamily: family,
		trusted:         trusted,
		maxBodyBytes:    maxBodyBytes,
		loopbackHostnames: map[string]struct{}{
			"localhost": {},
			"127.0.0.1": {},
			"::1":       {},
		},
	}
	if tlsEnabled {
		policy.scheme = "https"
		policy.hsts = true
	}
	if hub.Listen != "" {
		policy.loopbackHostnames[strings.ToLower(strings.Trim(hub.Listen, "[]"))] = struct{}{}
	}
	if hub.Origin != "" {
		parsed, err := url.Parse(hub.Origin)
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("server: invalid origin %q", hub.Origin)
		}
		policy.remote = &remoteOrigin{
			origin:   hub.Origin,
			host:     parsed.Host,
			hostname: parsed.Hostname(),
		}
		// HSTS is emitted for direct TLS or when the canonical public origin is
		// HTTPS behind a trusted proxy. It is never set for a plaintext public
		// origin.
		if parsed.Scheme == "https" {
			policy.hsts = true
		}
	}
	policy.rebuildHostsLocked()
	return policy, nil
}

// hostSettings is the narrow view of the hub configuration this package
// needs. Keeping it separate from config.Hub avoids a second validation path.
type hostSettings struct {
	Listen           string
	Port             int
	Origin           string
	ForwardedHeaders string
}

// setBoundPort records the port actually bound (port 0 selects an ephemeral
// port). Host and same-origin checks use it, like the Node hub's listen
// callback.
func (p *securityPolicy) setBoundPort(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.port = port
	p.rebuildHostsLocked()
}

func (p *securityPolicy) rebuildHostsLocked() {
	p.allowedHosts = make(map[string]struct{})
	if p.remote != nil {
		// Remote mode accepts the canonical Host only. Loopback aliases are
		// not a shortcut around the configured public boundary, even when the
		// listener itself binds a loopback address behind a proxy.
		p.allowedHosts[strings.ToLower(p.remote.host)] = struct{}{}
		return
	}
	if p.loopback {
		for name := range p.loopbackHostnames {
			p.allowedHosts[hostWithPort(name, p.port, p.scheme)] = struct{}{}
			// Accept the explicit default port as well as the canonical bare
			// host, so a health probe that always joins host:port works.
			defaultPort := 80
			if p.scheme == "https" {
				defaultPort = 443
			}
			if p.port == defaultPort {
				p.allowedHosts[net.JoinHostPort(strings.Trim(name, "[]"), strconv.Itoa(p.port))] = struct{}{}
			}
		}
	}
}

func hostWithPort(name string, port int, scheme string) string {
	bare := strings.Trim(name, "[]")
	defaultPort := 80
	if scheme == "https" {
		defaultPort = 443
	}
	if port == defaultPort {
		if strings.Contains(bare, ":") {
			return "[" + bare + "]"
		}
		return strings.ToLower(bare)
	}
	return net.JoinHostPort(bare, strconv.Itoa(port))
}

func (p *securityPolicy) portValue() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.port
}

// check applies every security rule and returns false after writing a failure
// response. The request is rewritten in place only when the checks pass.
func (p *securityPolicy) check(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	if !p.checkTarget(w, r) {
		return false
	}
	if !p.checkHost(w, r) {
		return false
	}
	p.normalizeForwarded(r)
	if p.hsts {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	if !p.checkOrigin(w, r) {
		return false
	}
	if !isUpgrade(r) && p.maxBodyBytes > 0 && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, p.maxBodyBytes)
	}
	return true
}

func (p *securityPolicy) checkTarget(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodConnect {
		writeBoundaryFailure(w, http.StatusBadRequest, "invalid request target")
		return false
	}
	target := r.RequestURI
	if target == "" {
		if r.URL != nil {
			target = r.URL.RequestURI()
		}
		if target == "" {
			target = "/"
		}
	}
	if containsControlOrSpace(target) || strings.Contains(target, `\`) {
		writeBoundaryFailure(w, http.StatusBadRequest, "invalid request target")
		return false
	}
	// Absolute-form targets carry an authority the client chose. Go promotes
	// that authority into r.Host, which would let the target and the Host
	// header disagree; only origin-form is accepted.
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		writeBoundaryFailure(w, http.StatusBadRequest, "invalid request target")
		return false
	}
	// An HTTP/1.0 request without a Host has no authority to check the
	// canonical Host against.
	if r.ProtoMajor == 1 && r.ProtoMinor == 0 && r.Host == "" && r.Header.Get("Host") == "" {
		writeBoundaryFailure(w, http.StatusBadRequest, "host header required")
		return false
	}
	pathPart := target
	if index := strings.IndexByte(pathPart, '?'); index >= 0 {
		pathPart = pathPart[:index]
	}
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") ||
		strings.Contains(target, "#") || strings.Contains(strings.ToLower(pathPart), "%5c") {
		writeBoundaryFailure(w, http.StatusBadRequest, "invalid request target")
		return false
	}
	return true
}

func (p *securityPolicy) checkHost(w http.ResponseWriter, r *http.Request) bool {
	if len(r.Header.Values("Host")) > 1 {
		writeBoundaryFailure(w, http.StatusBadRequest, "duplicate host header")
		return false
	}
	host := r.Host
	if host == "" {
		host = r.Header.Get("Host")
	}
	if host == "" {
		writeBoundaryFailure(w, http.StatusBadRequest, "host header required")
		return false
	}
	if len(host) > 255 || containsControlOrSpace(host) {
		writeBoundaryFailure(w, http.StatusBadRequest, "invalid host header")
		return false
	}
	p.mu.RLock()
	_, allowed := p.allowedHosts[strings.ToLower(host)]
	p.mu.RUnlock()
	if !allowed && p.localCollectorRequest(r, host) {
		allowed = true
	}
	if !allowed {
		writeBoundaryFailure(w, http.StatusMisdirectedRequest, "misdirected request")
		return false
	}
	return true
}

// localCollectorRequest is the one exception to the canonical Host in remote
// mode: the collector on the hub's own machine keeps reaching the hub at its
// loopback address after an origin is configured. It applies only to the
// collector endpoints, which authenticate with a grant or credential rather
// than a browser session, only to a direct loopback peer, and only when the
// request carries no forwarding header, so nothing that came through a
// reverse proxy (which adds one) can use it. Browser pages, sign-in and the
// API still require the canonical Host.
func (p *securityPolicy) localCollectorRequest(r *http.Request, host string) bool {
	if p.remote == nil || !isCollectorPath(requestPathname(r)) {
		return false
	}
	peer := peerIP(r.RemoteAddr)
	if peer == nil || !peer.IsLoopback() || hasForwardingHeaders(r.Header) {
		return false
	}
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.Trim(name, "[]")) {
	case "127.0.0.1", "localhost", "::1":
	default:
		return false
	}
	return port == strconv.Itoa(p.portValue())
}

func (p *securityPolicy) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) > 1 {
		writeBoundaryFailure(w, http.StatusBadRequest, "duplicate origin header")
		return false
	}
	origin := ""
	if len(origins) == 1 {
		origin = origins[0]
	}
	path := requestPathname(r)

	// Remote mode validates every Origin before any handler, even for reads.
	// A present-but-empty Origin is a foreign origin, not an absent one: Node's
	// raw-header boundary compares the single value, and "" never equals the
	// canonical origin.
	if p.remote != nil {
		if len(origins) == 1 && origins[0] != p.remote.origin {
			writeBoundaryFailure(w, http.StatusForbidden, "origin not allowed")
			return false
		}
		if (isMutation(r.Method) || isUpgrade(r)) && !isOriginExemptPath(path) {
			if !p.trustedRemoteOrigin(r, origin) {
				writeBoundaryFailure(w, http.StatusForbidden, "origin not allowed")
				return false
			}
		}
		return true
	}

	// Loopback mode keeps the Node hub's rule: only state-changing requests
	// and WebSocket upgrades need an exact same-origin Origin.
	if !isMutation(r.Method) && !isUpgrade(r) {
		return true
	}
	if isOriginExemptPath(path) {
		// Health is an unauthenticated probe and collector endpoints carry a
		// grant; their handlers reject a bad method or Origin themselves.
		return true
	}
	if !p.trustedLoopbackOrigin(r, origin) {
		writeBoundaryFailure(w, http.StatusForbidden, "origin not allowed")
		return false
	}
	return true
}

func (p *securityPolicy) trustedRemoteOrigin(r *http.Request, origin string) bool {
	if p.remote == nil || origin == "" {
		return false
	}
	return origin == p.remote.origin && strings.EqualFold(r.Host, p.remote.host)
}

func (p *securityPolicy) trustedLoopbackOrigin(r *http.Request, origin string) bool {
	if origin == "" || len(origin) > 256 {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if parsed.Scheme != p.scheme {
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
	if _, ok := p.loopbackHostnames[hostname]; !ok {
		return false
	}
	p.mu.RLock()
	port := p.port
	p.mu.RUnlock()
	if parsed.Port() != "" {
		if parsed.Port() != strconv.Itoa(port) {
			return false
		}
	} else {
		defaultPort := 80
		if p.scheme == "https" {
			defaultPort = 443
		}
		if port != defaultPort {
			return false
		}
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

// trustedPeer reports whether a TCP peer may supply forwarded headers. Loopback
// is deliberately not implied: admission to the plaintext listener is a weaker
// property than being allowed to speak for a client.
func (p *securityPolicy) trustedPeer(ip net.IP) bool {
	for index := range p.trusted {
		if p.trusted[index].contains(ip) {
			return true
		}
	}
	return false
}

// admitsPeer reports whether the plaintext trusted-proxy listener may accept a
// TCP connection. Configured trusted proxies are admitted, and so is every
// loopback peer: local readiness probes and operator curl commands connect from
// 127.0.0.0/8 or ::1 and must keep working on a hub whose listener is also
// fronted by a reverse proxy. Only non-loopback, non-trusted peers are closed.
func (p *securityPolicy) admitsPeer(ip net.IP) bool {
	if ip != nil && ip.IsLoopback() {
		return true
	}
	return p.trustedPeer(ip)
}

// normalizeForwarded strips attacker-controlled forwarding headers unless the
// TCP peer is a configured trusted proxy. Exactly one header family is
// authoritative; the other is ignored and stripped. Trusted values are reduced
// to the client address, scheme and host, then re-serialized.
func (p *securityPolicy) normalizeForwarded(r *http.Request) {
	peer := peerIP(r.RemoteAddr)
	trusted := p.trustedPeer(peer)
	metadata := forwardedMetadata{
		clientIP: peerString(peer),
		scheme:   p.scheme,
		host:     r.Host,
		// Recorded before anything is stripped: a colocated pairing redemption
		// must be refused when the caller reached the hub through a forwarding
		// hop, even when that hop is a trusted proxy on this machine.
		forwardedPresent: hasForwardingHeaders(r.Header),
		proxyPeer:        trusted,
	}
	if trusted {
		client, proto, forwardedHost := p.forwardedFacts(r.Header)
		stripForwardedHeaders(r.Header)
		if client == "" {
			client = metadata.clientIP
		}
		if proto == "" {
			proto = p.scheme
		}
		if client != "" {
			r.Header.Set("X-Forwarded-For", client)
		}
		r.Header.Set("X-Forwarded-Proto", proto)
		if forwardedHost != "" {
			r.Header.Set("X-Forwarded-Host", forwardedHost)
		}
		metadata.clientIP = client
		metadata.scheme = proto
		if forwardedHost != "" {
			metadata.host = forwardedHost
		}
	} else {
		stripForwardedHeaders(r.Header)
	}
	*r = *r.WithContext(context.WithValue(r.Context(), forwardedMetadataKey{}, metadata))
}

// hasForwardingHeaders reports whether the request carried any header that
// claims to speak for another hop. It runs before normalization strips the
// family the hub does not trust.
func hasForwardingHeaders(headers http.Header) bool {
	for key := range headers {
		lower := strings.ToLower(key)
		if lower == "forwarded" || lower == "x-real-ip" || lower == "via" ||
			strings.HasPrefix(lower, "x-forwarded-") {
			return true
		}
	}
	return false
}

// forwardedFacts reads only the configured header family. The caller must have
// established that the TCP peer is trusted.
func (p *securityPolicy) forwardedFacts(headers http.Header) (client, proto, host string) {
	if p.forwardedFamily == config.ForwardedHeadersForwarded {
		return p.forwardedFactsRFC7239(headers)
	}
	if chain := csvValues(headers.Values("X-Forwarded-For")); len(chain) > 0 {
		client = p.clientFromChain(chain)
	}
	if value, ok := singleHeaderValue(headers.Values("X-Forwarded-Proto")); ok {
		value = strings.ToLower(value)
		if value == "http" || value == "https" {
			proto = value
		}
	}
	if value, ok := singleHeaderValue(headers.Values("X-Forwarded-Host")); ok && validForwardedHost(value) {
		host = value
	}
	return client, proto, host
}

// forwardedFactsRFC7239 reads RFC 7239 Forwarded. The header is parsed
// strictly: any syntax error discards every forwarded fact, so a crafted prefix
// can never smuggle a value past the trusted proxy that appended its element.
// Transport facts come from the nearest element a trusted hop contributed.
func (p *securityPolicy) forwardedFactsRFC7239(headers http.Header) (client, proto, host string) {
	elements, err := parseForwardedElements(headers.Values("Forwarded"))
	if err != nil {
		// One debug record per rejected header: the request still succeeds with
		// the TCP peer as its client address.
		if p.logger != nil {
			p.logger.Debug("ignoring malformed Forwarded header and using the TCP peer", "error", err)
		}
		return "", "", ""
	}
	// low is the leftmost element contributed by a trusted hop. Elements to its
	// left were appended by a hop whose address the chain does not vouch for.
	low := p.trustedElementFloor(elements)
	client = p.clientFromElements(elements, low)
	if value, ok := nearestParameter(elements, low, "proto"); ok {
		value = strings.ToLower(value)
		if value == "http" || value == "https" {
			proto = value
		}
	}
	if value, ok := nearestParameter(elements, low, "host"); ok && validForwardedHost(value) {
		host = value
	}
	return client, proto, host
}

// trustedElementFloor returns the index of the leftmost Forwarded element that
// a trusted hop contributed. The rightmost element is always contributed by the
// TCP peer, which is trusted by the time this runs. Moving left, an element
// counts only when the element to its right reports a trusted proxy address as
// the peer it saw.
func (p *securityPolicy) trustedElementFloor(elements []map[string]string) int {
	floor := len(elements) - 1
	for index := len(elements) - 2; index >= 0; index-- {
		value, ok := elements[index+1]["for"]
		if !ok {
			break
		}
		ip := parseForwardedIP(value)
		if ip == nil || !p.trustedPeer(ip) {
			break
		}
		floor = index
	}
	return floor
}

// clientFromElements walks the hop list from the right, within the trusted
// range, skipping addresses that are themselves trusted proxies. The first
// untrusted address is the client. An element without a usable `for` parameter
// is a trust barrier: the walk stops and the caller uses the TCP peer. A chain
// made only of trusted addresses also yields "".
func (p *securityPolicy) clientFromElements(elements []map[string]string, floor int) string {
	for index := len(elements) - 1; index >= floor; index-- {
		value, ok := elements[index]["for"]
		if !ok {
			return ""
		}
		ip := parseForwardedIP(value)
		if ip == nil {
			return ""
		}
		if p.trustedPeer(ip) {
			continue
		}
		return ip.String()
	}
	return ""
}

// nearestParameter returns the value of key from the rightmost element in the
// trusted range that carries it. A nearer occurrence always wins, so a client
// cannot add a farther element that overrides the transport facts.
func nearestParameter(elements []map[string]string, floor int, key string) (string, bool) {
	for index := len(elements) - 1; index >= floor; index-- {
		if value, ok := elements[index][key]; ok {
			value = strings.TrimSpace(value)
			if value == "" {
				return "", false
			}
			return value, true
		}
	}
	return "", false
}

// clientFromChain walks an X-Forwarded-For hop list from the right, which is
// the trusted peer, skipping addresses that are themselves trusted proxies. The
// first untrusted address is the client. A malformed hop, or a chain made only
// of trusted addresses, yields "" so the caller falls back to the TCP peer
// instead of trusting an attacker-supplied leftmost value.
func (p *securityPolicy) clientFromChain(chain []string) string {
	for index := len(chain) - 1; index >= 0; index-- {
		ip := parseForwardedIP(chain[index])
		if ip == nil {
			return ""
		}
		if p.trustedPeer(ip) {
			continue
		}
		return ip.String()
	}
	return ""
}

func csvValues(values []string) []string {
	var items []string
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item != "" {
				items = append(items, item)
			}
		}
	}
	return items
}

// singleHeaderValue returns a header only when every field line and every
// comma-separated element collapses to exactly one non-empty value. Multi-
// valued transport facts are ignored; the proxy must overwrite them.
func singleHeaderValue(values []string) (string, bool) {
	items := csvValues(values)
	if len(items) != 1 {
		return "", false
	}
	return items[0], true
}

// errMalformedForwarded marks a Forwarded header that violates RFC 7239. The
// whole header is discarded when any part of it fails to parse.
var errMalformedForwarded = errors.New("server: malformed Forwarded header")

// parseForwardedElements parses every Forwarded field line strictly, following
// RFC 7239 and the RFC 7230 token/quoted-string grammar:
//
//	Forwarded       = 1#forwarded-element
//	forwarded-element = forwarded-pair *( ";" forwarded-pair )
//	forwarded-pair  = token "=" ( token / quoted-string )
//
// Empty list elements between commas are skipped, as RFC 7230 requires of list
// parsers. Every other deviation is a syntax error: an unterminated
// quoted-string, a bad quoted-pair, an empty pair, a missing "=", a non-token
// name, or a duplicate parameter within one element (parameter names are
// case-insensitive).
func parseForwardedElements(values []string) ([]map[string]string, error) {
	elements := make([]map[string]string, 0, len(values))
	for _, value := range values {
		parsed, err := parseForwardedLine(value)
		if err != nil {
			return nil, err
		}
		elements = append(elements, parsed...)
	}
	if len(elements) == 0 {
		return nil, errMalformedForwarded
	}
	return elements, nil
}

func parseForwardedLine(value string) ([]map[string]string, error) {
	var elements []map[string]string
	index := 0
	for {
		skipOptionalWhitespace(value, &index)
		// A run of commas (including a leading or trailing one) contributes an
		// empty list element, which list parsers ignore.
		if index >= len(value) {
			return elements, nil
		}
		if value[index] == ',' {
			index++
			continue
		}
		element, err := parseForwardedElement(value, &index)
		if err != nil {
			return nil, err
		}
		elements = append(elements, element)
		skipOptionalWhitespace(value, &index)
		if index >= len(value) {
			return elements, nil
		}
		if value[index] != ',' {
			return nil, errMalformedForwarded
		}
		index++
	}
}

func parseForwardedElement(value string, index *int) (map[string]string, error) {
	parameters := map[string]string{}
	for {
		skipOptionalWhitespace(value, index)
		if *index >= len(value) || value[*index] == ',' {
			// A pair was required here, so "for=1;;proto=https" or a trailing
			// ";" is an empty pair and therefore malformed.
			return nil, errMalformedForwarded
		}
		name, err := parseForwardedToken(value, index)
		if err != nil {
			return nil, err
		}
		skipOptionalWhitespace(value, index)
		if *index >= len(value) || value[*index] != '=' {
			return nil, errMalformedForwarded
		}
		(*index)++
		skipOptionalWhitespace(value, index)
		parameterValue, err := parseForwardedValue(value, index)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(name)
		if _, duplicate := parameters[key]; duplicate {
			return nil, errMalformedForwarded
		}
		parameters[key] = parameterValue
		skipOptionalWhitespace(value, index)
		if *index >= len(value) {
			return parameters, nil
		}
		switch value[*index] {
		case ',':
			return parameters, nil
		case ';':
			(*index)++
		default:
			return nil, errMalformedForwarded
		}
	}
}

// parseForwardedToken consumes one RFC 7230 token.
func parseForwardedToken(value string, index *int) (string, error) {
	start := *index
	for *index < len(value) && isTokenCharacter(value[*index]) {
		(*index)++
	}
	if *index == start {
		return "", errMalformedForwarded
	}
	return value[start:*index], nil
}

// parseForwardedValue consumes one token or quoted-string. A quoted-string is
// unescaped; a quoted-pair may only escape HTAB, SP, VCHAR or obs-text.
func parseForwardedValue(value string, index *int) (string, error) {
	if *index >= len(value) {
		return "", errMalformedForwarded
	}
	if value[*index] != '"' {
		token, err := parseForwardedToken(value, index)
		if err != nil {
			return "", err
		}
		return token, nil
	}
	(*index)++
	var builder strings.Builder
	for {
		if *index >= len(value) {
			return "", errMalformedForwarded
		}
		character := value[*index]
		switch {
		case character == '"':
			(*index)++
			return builder.String(), nil
		case character == '\\':
			(*index)++
			if *index >= len(value) || !isQuotedPairCharacter(value[*index]) {
				return "", errMalformedForwarded
			}
			builder.WriteByte(value[*index])
			(*index)++
		case isQuotedStringCharacter(character):
			builder.WriteByte(character)
			(*index)++
		default:
			return "", errMalformedForwarded
		}
	}
}

func skipOptionalWhitespace(value string, index *int) {
	for *index < len(value) && (value[*index] == ' ' || value[*index] == '\t') {
		(*index)++
	}
}

// isTokenCharacter reports whether b may appear in an RFC 7230 token.
func isTokenCharacter(b byte) bool {
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// isQuotedStringCharacter reports whether b may appear unescaped in a
// quoted-string: qdtext = HTAB / SP / %x21 / %x23-5B / %x5D-7E / obs-text.
func isQuotedStringCharacter(b byte) bool {
	return b == '\t' || b == ' ' || b == 0x21 || (b >= 0x23 && b <= 0x5b) ||
		(b >= 0x5d && b <= 0x7e) || b >= 0x80
}

// isQuotedPairCharacter reports whether b may follow a backslash inside a
// quoted-string: HTAB / SP / VCHAR / obs-text.
func isQuotedPairCharacter(b byte) bool {
	return b == '\t' || b == ' ' || (b >= 0x21 && b <= 0x7e) || b >= 0x80
}

func parseForwardedIP(value string) net.IP {
	value = strings.Trim(strings.TrimSpace(value), `"`)
	if value == "" || strings.EqualFold(value, "unknown") || value == "_hidden" {
		return nil
	}
	if strings.HasPrefix(value, "[") {
		if end := strings.IndexByte(value, ']'); end > 0 {
			return net.ParseIP(value[1:end])
		}
		return nil
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return net.ParseIP(strings.Trim(host, "[]"))
	}
	return net.ParseIP(strings.Split(value, "%")[0])
}

type forwardedMetadata struct {
	clientIP string
	scheme   string
	host     string
	// forwardedPresent records that the request carried Forwarded,
	// X-Forwarded-*, X-Real-IP or Via before normalization stripped it.
	forwardedPresent bool
	// proxyPeer records that the direct TCP peer is a configured trusted proxy.
	proxyPeer bool
}

type forwardedMetadataKey struct{}

// ClientIP returns the effective client address: the trusted proxy's
// X-Forwarded-For/Forwarded value when the peer is trusted, otherwise the TCP
// peer address.
func ClientIP(r *http.Request) string {
	if metadata, ok := r.Context().Value(forwardedMetadataKey{}).(forwardedMetadata); ok {
		return metadata.clientIP
	}
	return peerString(peerIP(r.RemoteAddr))
}

// Scheme returns the effective request scheme, from a trusted proxy when
// configured, otherwise from the listener.
func Scheme(r *http.Request) string {
	if metadata, ok := r.Context().Value(forwardedMetadataKey{}).(forwardedMetadata); ok {
		return metadata.scheme
	}
	return "http"
}

// ForwardedHost returns the host a trusted proxy reported, or the request's
// own Host. Host validation itself always uses r.Host, never this value.
func ForwardedHost(r *http.Request) string {
	if metadata, ok := r.Context().Value(forwardedMetadataKey{}).(forwardedMetadata); ok {
		return metadata.host
	}
	return r.Host
}

// ForwardedHeadersPresent reports whether the request carried any forwarding
// header before normalization. A handler that must distinguish a direct caller
// from a proxied one uses it: the security layer strips the untrusted family
// before the handler runs.
func ForwardedHeadersPresent(r *http.Request) bool {
	if metadata, ok := r.Context().Value(forwardedMetadataKey{}).(forwardedMetadata); ok {
		return metadata.forwardedPresent
	}
	// Without the security layer in front of the handler no normalization has
	// run, so read the request as it arrived.
	return r != nil && hasForwardingHeaders(r.Header)
}

// TrustedProxyPeer reports whether the direct TCP peer is one of the configured
// trusted proxies. The policy records it during normalization, so a handler
// cannot mistake a proxy connection for an end client.
func TrustedProxyPeer(r *http.Request) bool {
	metadata, ok := r.Context().Value(forwardedMetadataKey{}).(forwardedMetadata)
	return ok && metadata.proxyPeer
}

func stripForwardedHeaders(headers http.Header) {
	for key := range headers {
		lower := strings.ToLower(key)
		if lower == "forwarded" || strings.HasPrefix(lower, "x-forwarded-") {
			delete(headers, key)
		}
	}
}

func validForwardedHost(value string) bool {
	if value == "" || len(value) > 255 || containsControlOrSpace(value) {
		return false
	}
	if strings.ContainsAny(value, `/\@?#`) {
		return false
	}
	parsed, err := url.Parse("//" + value)
	if err != nil {
		return false
	}
	return parsed.User == nil && parsed.Host == value && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func peerIP(remote string) net.IP {
	if remote == "" {
		return nil
	}
	host := remote
	if parsedHost, _, err := net.SplitHostPort(remote); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	if before, _, found := strings.Cut(host, "%"); found {
		host = before
	}
	return net.ParseIP(host)
}

func peerString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

func parseTrustedProxies(values []string) ([]ipRange, error) {
	ranges := make([]ipRange, 0, len(values))
	for _, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if ip := net.ParseIP(raw); ip != nil {
			ranges = append(ranges, ipRange{ip: ip})
			continue
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("server: trusted proxy %q is not an IP address or CIDR range", raw)
		}
		ranges = append(ranges, ipRange{network: network})
	}
	return ranges, nil
}

func containsControlOrSpace(value string) bool {
	for _, character := range value {
		if character <= 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func isMutation(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func isUpgrade(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !headerListContains(r.Header.Values("Connection"), "upgrade") {
		return false
	}
	return strings.TrimSpace(r.Header.Get("Upgrade")) != ""
}

func headerListContains(values []string, token string) bool {
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(item), token) {
				return true
			}
		}
	}
	return false
}

func isCollectorPath(pathname string) bool {
	return pathname == "/api/collector/enroll" || pathname == "/api/collector/connect"
}

func isOriginExemptPath(pathname string) bool {
	return pathname == "/api/health" || isCollectorPath(pathname)
}

func requestPathname(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "/"
	}
	if r.URL.Path == "" {
		return "/"
	}
	return r.URL.Path
}

func writeBoundaryFailure(w http.ResponseWriter, status int, message string) {
	body, _ := json.Marshal(map[string]string{"error": message})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

var errInvalidPattern = errors.New("server: mount pattern must start with /")
