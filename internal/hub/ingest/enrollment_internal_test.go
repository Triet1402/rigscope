package ingest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLocalPeer covers the socket-level rule behind colocated redemption: a
// loopback peer, or a peer whose address is the listening socket's own address.
// The second shape is what an origin-based destination produces on the hub's
// own machine: the origin name resolves to one of the host's addresses, so the
// connection is a self-connection.
func TestLocalPeer(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		local  net.Addr
		want   bool
	}{
		{name: "loopback ipv4", remote: "127.0.0.1:51000", want: true},
		{name: "loopback ipv6", remote: "[::1]:51000", want: true},
		{name: "loopback on another port", remote: "127.0.0.5:51000", want: true},
		{
			name:   "self connection to the host's own address",
			remote: "192.168.1.5:51000",
			local:  &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 8443},
			want:   true,
		},
		{
			name:   "self connection with an ipv4 address rendered as ipv6",
			remote: "192.168.1.5:51000",
			local:  &net.TCPAddr{IP: net.ParseIP("::ffff:192.168.1.5"), Port: 8443},
			want:   true,
		},
		{
			name:   "remote peer against the host's own address",
			remote: "198.51.100.7:51000",
			local:  &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 8443},
			want:   false,
		},
		{name: "non-loopback peer with no local address", remote: "192.168.1.5:51000", want: false},
		{name: "missing peer", remote: "", local: &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 8443}, want: false},
		{name: "unparseable peer", remote: "not-an-address", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/collector/enroll", nil)
			r.RemoteAddr = tc.remote
			if tc.local != nil {
				r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, tc.local))
			}
			if got := localPeer(r); got != tc.want {
				t.Fatalf("localPeer(remote=%q, local=%v) = %v, want %v", tc.remote, tc.local, got, tc.want)
			}
		})
	}
}
