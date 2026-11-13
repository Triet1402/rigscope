package huburl

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://hub.example.com", "wss://hub.example.com/api/collector/connect"},
		{"https://hub.example.com/", "wss://hub.example.com/api/collector/connect"},
		{"https://hub.example.com:8443", "wss://hub.example.com:8443/api/collector/connect"},
		{"wss://hub.example.com/api/collector/connect", "wss://hub.example.com/api/collector/connect"},
		{"http://127.0.0.1:8081", "ws://127.0.0.1:8081/api/collector/connect"},
		{"http://localhost:8081", "ws://127.0.0.1:8081/api/collector/connect"},
		{"ws://localhost:8081/api/collector/connect", "ws://127.0.0.1:8081/api/collector/connect"},
		{"http://ip6-localhost:8081", "ws://[::1]:8081/api/collector/connect"},
		{"ws://localhost6/api/collector/connect", "ws://[::1]/api/collector/connect"},
		// TLS endpoints keep their hostname: it is the certificate identity.
		{"https://localhost:8443", "wss://localhost:8443/api/collector/connect"},
		{"https://ip6-localhost:8443", "wss://ip6-localhost:8443/api/collector/connect"},
		{"wss://localhost6", "wss://localhost6/api/collector/connect"},
		{"wss://[::1]:8443", "wss://[::1]:8443/api/collector/connect"},
		{"  wss://hub.example.com  ", "wss://hub.example.com/api/collector/connect"},
	}
	for _, c := range cases {
		got, err := Normalize(c.raw)
		if err != nil {
			t.Errorf("Normalize(%q) = %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
		// Normalization is idempotent: the stored form normalizes to itself.
		again, err := Normalize(got)
		if err != nil || again != got {
			t.Errorf("Normalize(Normalize(%q)) = %q, %v", c.raw, again, err)
		}
	}
}

func TestNormalizeRejectsNonLoopbackPlaintext(t *testing.T) {
	invalid := []string{
		"",
		"http://hub.example.com",
		"ws://hub.example.com/api/collector/connect",
		"ws://hub.example.com.localhost/api/collector/connect",
		"ftp://127.0.0.1/api/collector/connect",
		"https://user@hub.example.com",
		"https://hub.example.com/?x=1",
		"https://hub.example.com/other",
		"wss://hub.example.com/api/collector/connect#fragment",
		"wss://",
	}
	for _, raw := range invalid {
		if got, err := Normalize(raw); err == nil {
			t.Errorf("Normalize(%q) = %q, want an error", raw, got)
		}
	}
}

func TestValidateRequiresAWebSocketScheme(t *testing.T) {
	for _, raw := range []string{"https://hub.example.com", "http://127.0.0.1:8081"} {
		if _, err := Validate(raw); err == nil || !strings.Contains(err.Error(), "ws:// or wss://") {
			t.Errorf("Validate(%q) = %v, want a WebSocket scheme error", raw, err)
		}
	}
	got, err := Validate("wss://localhost:8443")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got != "wss://localhost:8443/api/collector/connect" {
		t.Fatalf("Validate = %q", got)
	}
}
