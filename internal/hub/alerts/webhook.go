package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Triet1402/rigscope/internal/api"
)

// parsedWebhookURL is a validated endpoint and its transport.
type parsedWebhookURL struct {
	url       string
	transport api.AlertWebhookTransport
}

// isLoopbackHostname reports whether hostname is a controlled loopback sink.
// The reference accepts localhost, ::1 and any 127/8 literal; the Go hub also
// requires a parsed loopback address, so a malformed literal such as
// 127.0.0.999 is refused.
func isLoopbackHostname(hostname string) bool {
	host := strings.ToLower(strings.Trim(hostname, "[]"))
	if host == "localhost" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseWebhookURL accepts exact HTTPS endpoints and HTTP only for
// loopback-controlled sinks. Userinfo and fragments are rejected; a query is
// allowed because receivers commonly carry a channel token there and the URL
// never leaves the hub.
func parseWebhookURL(raw string) (parsedWebhookURL, error) {
	value := strings.TrimSpace(raw)
	if value == "" || utf16Len(value) > MaxWebhookURL {
		return parsedWebhookURL{}, validationf("webhook url length is invalid")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return parsedWebhookURL{}, validationf("webhook url is not a valid URL")
	}
	if parsed.User != nil {
		return parsedWebhookURL{}, validationf("webhook url must not embed credentials")
	}
	if parsed.Fragment != "" || strings.Contains(value, "#") {
		return parsedWebhookURL{}, validationf("webhook url must not include a fragment")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "https" {
		if parsed.Hostname() == "" {
			return parsedWebhookURL{}, validationf("webhook url must include a host")
		}
		return parsedWebhookURL{url: parsed.String(), transport: api.AlertWebhookTransportHTTPS}, nil
	}
	if scheme == "http" && isLoopbackHostname(parsed.Hostname()) {
		return parsedWebhookURL{url: parsed.String(), transport: api.AlertWebhookTransportLoopbackHTTP}, nil
	}
	return parsedWebhookURL{}, validationf("webhook url must be HTTPS, or loopback HTTP for a controlled local sink")
}

var headerNamePattern = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

var reservedHeaders = map[string]struct{}{
	"host": {}, "content-length": {}, "content-type": {}, "idempotency-key": {},
}

// parseWebhookHeaders validates the configured headers. Names are trimmed and
// must be HTTP tokens; Content-Type, Content-Length, Host and Idempotency-Key
// are set by the sender and cannot be overridden.
func parseWebhookHeaders(raw map[string]string) (map[string]string, error) {
	if len(raw) > MaxWebhookHeaders {
		return nil, validationf("too many webhook headers")
	}
	headers := make(map[string]string, len(raw))
	for rawName, value := range raw {
		name := strings.TrimSpace(rawName)
		if name == "" || utf16Len(name) > MaxWebhookHeaderKey || !headerNamePattern.MatchString(name) {
			return nil, validationf("webhook header name is invalid or reserved")
		}
		if _, reserved := reservedHeaders[strings.ToLower(name)]; reserved {
			return nil, validationf("webhook header name is invalid or reserved")
		}
		if _, duplicate := headers[name]; duplicate {
			return nil, validationf("duplicate webhook header name")
		}
		if utf16Len(value) > MaxWebhookHeaderValue {
			return nil, validationf("webhook header value is invalid")
		}
		if hasControl(value) {
			return nil, validationf("webhook header value must not contain control characters")
		}
		headers[name] = value
	}
	return headers, nil
}

// parseWebhookSecret normalizes an optional HMAC secret.
func parseWebhookSecret(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		return nil, nil
	}
	if utf16Len(value) > MaxWebhookSecret || hasControl(value) {
		return nil, validationf("webhook secret is invalid")
	}
	return &value, nil
}

// WebhookRequest is one delivery attempt. Body is JSON.
type WebhookRequest struct {
	URL     string
	Headers map[string]string
	Secret  *string
	Body    []byte
	JobID   string
	Timeout time.Duration
}

// WebhookResult is the bounded outcome of one delivery attempt.
type WebhookResult struct {
	OK     bool
	Status *int
	Error  string
}

// WebhookSender delivers one webhook request. Tests inject a deterministic
// sender; production uses NewWebhookSender.
type WebhookSender interface {
	Send(ctx context.Context, request WebhookRequest) WebhookResult
}

// WebhookSenderFunc adapts a function to WebhookSender.
type WebhookSenderFunc func(ctx context.Context, request WebhookRequest) WebhookResult

// Send implements WebhookSender.
func (f WebhookSenderFunc) Send(ctx context.Context, request WebhookRequest) WebhookResult {
	return f(ctx, request)
}

// IPResolver resolves a hostname for the delivery address policy. *net.Resolver
// satisfies it; tests may inject a fixed resolver.
type IPResolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// WebhookSenderOptions configures the real HTTP sender.
type WebhookSenderOptions struct {
	// AllowPrivateTargets permits delivery to loopback, private, link-local and
	// other non-public addresses for HTTPS endpoints. It is false by default.
	// A loopback-HTTP endpoint is always allowed to reach loopback only.
	AllowPrivateTargets bool
	// Resolver overrides DNS for tests. Nil uses net.DefaultResolver.
	Resolver IPResolver
	// DialTimeout bounds one TCP connection. Zero means 5 s.
	DialTimeout time.Duration
}

// SSRF policy choice: the reference validates the URL scheme and host at
// configure time and then connects to whatever DNS returns. The Go hub keeps
// the same HTTPS-or-loopback-HTTP rule, but additionally validates the actual
// address it connects to on every delivery. A hostname that resolves to
// loopback, private, link-local, multicast or otherwise non-public space is
// refused before a connection is made, which also closes DNS-rebinding. A
// loopback HTTP sink may reach loopback only; an operator can opt into private
// and loopback HTTPS targets with AllowPrivateTargets.
type webhookSender struct {
	allowPrivate bool
	resolver     IPResolver
	dialer       net.Dialer
	client       *http.Client
}

// NewWebhookSender builds the production sender. Proxy environment variables
// are deliberately ignored: a proxy would let the hub connect to an address the
// policy never checked.
func NewWebhookSender(opts WebhookSenderOptions) WebhookSender {
	dialTimeout := opts.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	sender := &webhookSender{
		allowPrivate: opts.AllowPrivateTargets,
		resolver:     resolver,
		dialer:       net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second},
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           sender.dialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
	}
	sender.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirectRefused
		},
	}
	return sender
}

var (
	errRedirectRefused = errors.New("webhook redirect is not allowed")
	errTargetRefused   = errors.New("webhook target address is not allowed")
)

type loopbackSinkKey struct{}

// Send implements WebhookSender.
func (s *webhookSender) Send(ctx context.Context, request WebhookRequest) WebhookResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, request.Timeout)
		defer cancel()
	}
	parsed, err := parseWebhookURL(request.URL)
	if err != nil {
		return WebhookResult{Error: "webhook url is invalid"}
	}
	if err := s.checkPolicy(parsed); err != nil {
		return WebhookResult{Error: err.Error()}
	}
	// A loopback-HTTP endpoint is explicitly a controlled local sink, so its
	// dial may reach loopback. Every other transport reaches loopback only when
	// the operator opted in.
	ctx = context.WithValue(ctx, loopbackSinkKey{}, parsed.transport == api.AlertWebhookTransportLoopbackHTTP)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.url, bytes.NewReader(request.Body))
	if err != nil {
		return WebhookResult{Error: "delivery failed"}
	}
	for name, value := range request.Headers {
		httpRequest.Header.Set(name, value)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Idempotency-Key", request.JobID)
	if request.Secret != nil {
		mac := hmac.New(sha256.New, []byte(*request.Secret))
		_, _ = mac.Write(request.Body)
		httpRequest.Header.Set("X-Rigscope-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	response, err := s.client.Do(httpRequest)
	if err != nil {
		return WebhookResult{Error: deliveryError(err)}
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBodyBytes))
	status := response.StatusCode
	if status < 200 || status > 299 {
		return WebhookResult{Status: &status, Error: fmt.Sprintf("HTTP %d", status)}
	}
	return WebhookResult{OK: true, Status: &status}
}

func deliveryError(err error) string {
	switch {
	case errors.Is(err, errRedirectRefused):
		return errRedirectRefused.Error()
	case errors.Is(err, errTargetRefused):
		return errTargetRefused.Error()
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), isTimeout(err):
		return "delivery timed out"
	default:
		return "delivery failed"
	}
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func (s *webhookSender) checkPolicy(target parsedWebhookURL) error {
	parsed, err := url.Parse(target.url)
	if err != nil {
		return errTargetRefused
	}
	host := parsed.Hostname()
	if host == "" {
		return errTargetRefused
	}
	// A literal address is checked now; a hostname is checked after resolution
	// in dialContext so a rebinding answer cannot slip through.
	if ip := net.ParseIP(host); ip != nil {
		if !s.targetAddressAllowed(ip, target.transport == api.AlertWebhookTransportLoopbackHTTP) {
			return errTargetRefused
		}
	}
	return nil
}

// dialContext resolves the target itself and connects only to an address the
// policy allows. It never hands the hostname back to the network stack, so a
// second DNS answer cannot redirect the connection.
func (s *webhookSender) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errTargetRefused
	}
	loopbackSink := false
	if value, ok := ctx.Value(loopbackSinkKey{}).(bool); ok && value {
		loopbackSink = true
	}
	addresses, err := s.resolveAllowed(ctx, host, loopbackSink)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range addresses {
		connection, err := s.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return connection, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// resolveAllowed validates every address the target resolves to and returns
// exactly that validated set. A loopback-HTTP sink may reach loopback only:
// every resolved answer must be loopback, so a hostname that returns a mixed
// public/loopback set is refused rather than partially dialled.
func (s *webhookSender) resolveAllowed(ctx context.Context, host string, loopbackSink bool) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !s.targetAddressAllowed(ip, loopbackSink) {
			return nil, errTargetRefused
		}
		return []net.IP{ip}, nil
	}
	addresses, err := s.resolver.LookupIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, fmt.Errorf("%w: %v", errTargetRefused, err)
	}
	for _, ip := range addresses {
		if !s.targetAddressAllowed(ip, loopbackSink) {
			return nil, errTargetRefused
		}
	}
	return addresses, nil
}

// targetAddressAllowed applies the loopback-sink exception as an exact
// loopback-only rule; every other target uses the operator's private-target
// policy.
func (s *webhookSender) targetAddressAllowed(ip net.IP, loopbackSink bool) bool {
	if loopbackSink {
		return ip != nil && ip.IsLoopback()
	}
	return s.addressAllowed(ip, s.allowPrivate)
}

func (s *webhookSender) addressAllowed(ip net.IP, allowLoopback bool) bool {
	if ip == nil {
		return false
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if ip.IsLoopback() {
		return allowLoopback || s.allowPrivate
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return s.allowPrivate
	}
	if ip4 := ip.To4(); ip4 != nil && isSharedAddressSpace(ip4) {
		return s.allowPrivate
	}
	return true
}

// isSharedAddressSpace reports RFC 6598 carrier-grade NAT space, which is not
// private under net.IP.IsPrivate but still not a public webhook target.
func isSharedAddressSpace(ip net.IP) bool {
	return ip[0] == 100 && ip[1]&0xc0 == 64 // 100.64.0.0/10
}

// sanitizeWebhookError reduces a sender failure to a bounded, non-secret
// message before it is stored.
func sanitizeWebhookError(err error) string {
	if err == nil {
		return "delivery failed"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "delivery timed out"
	default:
		return "delivery failed"
	}
}
