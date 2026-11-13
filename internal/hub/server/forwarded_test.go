package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/config"
)

type forwardedCapture struct {
	headers http.Header
	client  string
	scheme  string
	host    string
}

// newForwardedServer mounts a handler that records what the middleware derived
// for one request. It reports the client address, scheme and forwarded host.
func newForwardedServer(t *testing.T, hub config.Hub, logs *bytes.Buffer) (*Server, *forwardedCapture) {
	t.Helper()
	captured := &forwardedCapture{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.headers = r.Header.Clone()
		captured.client = ClientIP(r)
		captured.scheme = Scheme(r)
		captured.host = ForwardedHost(r)
		w.WriteHeader(http.StatusOK)
	})
	options := []func(*Options){}
	if logs != nil {
		options = append(options, func(options *Options) {
			options.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		})
	}
	server := newServer(t, hub, options...)
	if err := server.Mount("/api/", handler); err != nil {
		t.Fatal(err)
	}
	return server, captured
}

func forwardedRequest(t *testing.T, server *Server, peer string, values ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := request(http.MethodGet, "http://127.0.0.1:8081/api/x")
	req.RemoteAddr = peer
	for _, value := range values {
		req.Header.Add("Forwarded", value)
	}
	return serve(server, req)
}

func TestMalformedForwardedIsIgnored(t *testing.T) {
	hub := config.Hub{
		Listen:           "127.0.0.1",
		Port:             8081,
		TrustedProxies:   []string{"10.0.0.0/8"},
		ForwardedHeaders: config.ForwardedHeadersForwarded,
	}

	cases := []struct {
		name  string
		value string
	}{
		// The P1 repro: a client prefix whose unterminated quote would swallow
		// the element the trusted proxy appended.
		{"unterminated quote swallows appended element", `for=198.51.100.66;ext=", for=10.1.2.3`},
		{"unterminated quote", `for=198.51.100.66;ext="`},
		{"unterminated quoted value", `for="198.51.100.66`},
		{"trailing backslash", `for="198.51.100.66\`},
		{"empty pair", `for=198.51.100.66;;proto=https`},
		{"trailing semicolon", `for=198.51.100.66;`},
		{"leading semicolon", `;for=198.51.100.66`},
		{"missing value", `for=`},
		{"missing equals", `for`},
		{"duplicate parameter", `for=198.51.100.66;proto=https;proto=http`},
		{"duplicate parameter different case", `for=198.51.100.66;proto=https;PROTO=http`},
		{"invalid token name", `for=198.51.100.66;pr"oto=https`},
		{"trailing garbage", `for=198.51.100.66;proto=https garbage`},
		{"control character in quoted value", "for=198.51.100.66;ext=\"a\x01b\""},
		{"bad quoted-pair", "for=198.51.100.66;ext=\"a\\\x01b\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, captured := newForwardedServer(t, hub, nil)
			response := forwardedRequest(t, server, "10.1.2.3:1234", tc.value)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d", response.Code)
			}
			if captured.client != "10.1.2.3" {
				t.Fatalf("client %q, want the TCP peer", captured.client)
			}
			if captured.scheme != "http" {
				t.Fatalf("scheme %q, want the listener scheme", captured.scheme)
			}
			if captured.host != "127.0.0.1:8081" {
				t.Fatalf("host %q, want the request Host", captured.host)
			}
			if got := captured.headers.Get("X-Forwarded-For"); got != "10.1.2.3" {
				t.Fatalf("X-Forwarded-For %q, want the TCP peer", got)
			}
			if got := captured.headers.Get("Forwarded"); got != "" {
				t.Fatalf("the malformed Forwarded header survived: %q", got)
			}
		})
	}
}

func TestMalformedForwardedLogsOnce(t *testing.T) {
	hub := config.Hub{
		Listen:           "127.0.0.1",
		Port:             8081,
		TrustedProxies:   []string{"10.0.0.0/8"},
		ForwardedHeaders: config.ForwardedHeadersForwarded,
	}
	var logs bytes.Buffer
	server, _ := newForwardedServer(t, hub, &logs)
	forwardedRequest(t, server, "10.1.2.3:1234", `for=198.51.100.66;ext="`)

	if got := strings.Count(logs.String(), "ignoring malformed Forwarded"); got != 1 {
		t.Fatalf("logged %d malformed-header records, want 1: %s", got, logs.String())
	}
}

func TestValidForwardedQuoting(t *testing.T) {
	hub := config.Hub{
		Listen:           "127.0.0.1",
		Port:             8081,
		TrustedProxies:   []string{"10.0.0.0/8"},
		ForwardedHeaders: config.ForwardedHeadersForwarded,
	}
	server, captured := newForwardedServer(t, hub, nil)
	// An escaped quote and an escaped backslash are valid inside a quoted-string,
	// and the appended trusted element still supplies the client.
	response := forwardedRequest(t, server, "10.1.2.3:1234",
		`for=198.51.100.66;ext="a\"b\\c", for="203.0.113.9"`)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if captured.client != "203.0.113.9" {
		t.Fatalf("client %q, want 203.0.113.9", captured.client)
	}
}

func TestForwardedMultiHopKeepsNearestTransportFacts(t *testing.T) {
	hub := config.Hub{
		Listen:           "127.0.0.1",
		Port:             8081,
		TrustedProxies:   []string{"10.0.0.0/8"},
		ForwardedHeaders: config.ForwardedHeadersForwarded,
	}

	t.Run("both elements carry proto", func(t *testing.T) {
		server, captured := newForwardedServer(t, hub, nil)
		response := forwardedRequest(t, server, "10.1.2.3:1234",
			`for=203.0.113.9;proto=https;host=client.example, for=10.4.5.6;proto=https;host=hub.example.test`)
		if response.Code != http.StatusOK {
			t.Fatalf("status %d", response.Code)
		}
		if captured.client != "203.0.113.9" {
			t.Fatalf("client %q, want 203.0.113.9", captured.client)
		}
		if captured.scheme != "https" {
			t.Fatalf("scheme %q, want https", captured.scheme)
		}
		if captured.host != "hub.example.test" {
			t.Fatalf("host %q, want hub.example.test", captured.host)
		}
	})

	t.Run("nearest element wins over an attacker prefix", func(t *testing.T) {
		server, captured := newForwardedServer(t, hub, nil)
		response := forwardedRequest(t, server, "10.1.2.3:1234",
			`for=6.6.6.6;proto=https;host=evil.example, for=203.0.113.10;proto=http;host=hub.example.test`)
		if response.Code != http.StatusOK {
			t.Fatalf("status %d", response.Code)
		}
		if captured.client != "203.0.113.10" {
			t.Fatalf("client %q, want 203.0.113.10", captured.client)
		}
		if captured.scheme != "http" {
			t.Fatalf("scheme %q, want http from the nearest element", captured.scheme)
		}
		if captured.host != "hub.example.test" {
			t.Fatalf("host %q, want hub.example.test", captured.host)
		}
	})

	t.Run("earlier trusted element supplies a missing proto", func(t *testing.T) {
		server, captured := newForwardedServer(t, hub, nil)
		response := forwardedRequest(t, server, "10.1.2.3:1234",
			`for=203.0.113.11;proto=https, for=10.9.9.9`)
		if response.Code != http.StatusOK {
			t.Fatalf("status %d", response.Code)
		}
		if captured.client != "203.0.113.11" {
			t.Fatalf("client %q, want 203.0.113.11", captured.client)
		}
		if captured.scheme != "https" {
			t.Fatalf("scheme %q, want https", captured.scheme)
		}
	})

	t.Run("element without for is a client trust barrier", func(t *testing.T) {
		server, captured := newForwardedServer(t, hub, nil)
		// The rightmost element was appended by the trusted peer, so its proto is
		// trustworthy, but the head of the client chain is unknown.
		response := forwardedRequest(t, server, "10.1.2.3:1234",
			`for=198.51.100.70;proto=https, proto=https;host=hub.example.test`)
		if response.Code != http.StatusOK {
			t.Fatalf("status %d", response.Code)
		}
		if captured.client != "10.1.2.3" {
			t.Fatalf("client %q, want the TCP peer behind the barrier", captured.client)
		}
		if captured.scheme != "https" {
			t.Fatalf("scheme %q, want https from the trusted element", captured.scheme)
		}
		if captured.host != "hub.example.test" {
			t.Fatalf("host %q, want hub.example.test", captured.host)
		}
	})

	t.Run("multiple header lines form one chain", func(t *testing.T) {
		server, captured := newForwardedServer(t, hub, nil)
		response := forwardedRequest(t, server, "10.1.2.3:1234",
			`for=203.0.113.12;proto=https`, `for=10.4.5.6;host=hub.example.test`)
		if response.Code != http.StatusOK {
			t.Fatalf("status %d", response.Code)
		}
		if captured.client != "203.0.113.12" {
			t.Fatalf("client %q, want 203.0.113.12", captured.client)
		}
		if captured.scheme != "https" {
			t.Fatalf("scheme %q, want https", captured.scheme)
		}
		if captured.host != "hub.example.test" {
			t.Fatalf("host %q, want hub.example.test", captured.host)
		}
	})
}
