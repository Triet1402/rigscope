package procfs

import (
	"testing"

	"github.com/Triet1402/rigscope/internal/protocol"
)

func itoaTest(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	for value > 0 {
		pos--
		digits[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[pos:])
}

func TestParseSpeedMbps(t *testing.T) {
	cases := []struct {
		raw  *string
		want *int64
	}{
		{nil, nil},
		{strp("1000\n"), i64p(1000)},
		{strp("10000"), i64p(10000)},
		{strp("-1\n"), nil},
		{strp("0\n"), nil},
		{strp("4294967295\n"), nil},
		{strp("garbage\n"), nil},
		{strp("\n"), nil},
	}
	for _, tc := range cases {
		got := ParseSpeedMbps(tc.raw)
		if (got == nil) != (tc.want == nil) {
			t.Fatalf("ParseSpeedMbps(%v) = %v, want %v", tc.raw, got, tc.want)
		}
		if got != nil && *got != *tc.want {
			t.Fatalf("ParseSpeedMbps(%v) = %d, want %d", tc.raw, *got, *tc.want)
		}
	}
}

func TestParseInterfaceState(t *testing.T) {
	cases := map[string]string{
		"up\n":             "up",
		"down\n":           "down",
		"lowerlayerdown\n": "down",
		"dormant\n":        "down",
		"notpresent\n":     "down",
		"unknown\n":        "unknown",
		"":                 "unknown",
		"something-else\n": "unknown",
	}
	for raw, want := range cases {
		value := raw
		if got := ParseInterfaceState(&value); got != want {
			t.Errorf("ParseInterfaceState(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := ParseInterfaceState(nil); got != "unknown" {
		t.Errorf("ParseInterfaceState(nil) = %q, want unknown", got)
	}
}

func TestDiscoverInterfaces(t *testing.T) {
	addresses := map[string][]InterfaceAddress{
		"eth0": {{Family: "ipv4", Address: "192.0.2.5", Prefix: 24}},
		"lo":   {{Family: "ipv4", Address: "127.0.0.1", Prefix: 8}},
	}
	interfaces := DiscoverInterfaces(func() (map[string][]InterfaceAddress, error) { return addresses, nil }, fixtureSource(t, "minimal"))
	if len(interfaces) != 2 {
		t.Fatalf("interfaces = %+v", interfaces)
	}
	if interfaces[0].Name != "eth0" || interfaces[1].Name != "lo" {
		t.Fatalf("interfaces = %+v", interfaces)
	}
	eth0 := interfaces[0]
	if eth0.ID != "net:eth0" || eth0.Virtual || eth0.State != "up" {
		t.Errorf("eth0 = %+v", eth0)
	}
	if eth0.MAC == nil || *eth0.MAC != "02:00:00:00:00:10" {
		t.Errorf("eth0 mac = %v", eth0.MAC)
	}
	if eth0.SpeedMbps == nil || *eth0.SpeedMbps != 1000 {
		t.Errorf("eth0 speed = %v", eth0.SpeedMbps)
	}
	if len(eth0.Addresses) != 1 || eth0.Addresses[0] != (InterfaceAddress{Family: "ipv4", Address: "192.0.2.5", Prefix: 24}) {
		t.Errorf("eth0 addresses = %+v", eth0.Addresses)
	}
	lo := interfaces[1]
	if !lo.Virtual || lo.State != "unknown" || lo.SpeedMbps != nil {
		t.Errorf("lo = %+v", lo)
	}
}

func TestDiscoverInterfacesDedupesAndCapsAddresses(t *testing.T) {
	var raw []InterfaceAddress
	for i := 0; i < 40; i++ {
		raw = append(raw, InterfaceAddress{Family: "ipv4", Address: "192.0.2." + itoaTest(i), Prefix: 24})
	}
	raw = append(raw,
		InterfaceAddress{Family: "IPv4", Address: "192.0.2.1", Prefix: 24}, // duplicate
		InterfaceAddress{Family: "ipx", Address: "192.0.2.9", Prefix: 8},   // unsupported family
		InterfaceAddress{Family: "ipv6", Address: "x", Prefix: 64},         // too short
	)
	addresses := map[string][]InterfaceAddress{
		"eth0": raw,
		"eth1": {{Family: "ipv6", Address: "2001:db8::1", Prefix: 999}},
	}
	src := mapSource(nil, map[string][]string{"/sys/class/net": {"eth0", "eth1"}})
	interfaces := DiscoverInterfaces(func() (map[string][]InterfaceAddress, error) { return addresses, nil }, src)
	if len(interfaces) != 2 {
		t.Fatalf("interfaces = %+v", interfaces)
	}
	addrs := interfaces[0].Addresses
	if len(addrs) != protocol.MaxAddressesPerInterface {
		t.Fatalf("addresses = %d, want %d", len(addrs), protocol.MaxAddressesPerInterface)
	}
	if addrs[0].Address != "192.0.2.0" || addrs[1].Address != "192.0.2.1" {
		t.Fatalf("dedupe failed: %+v", addrs[:2])
	}
	if got := interfaces[1].Addresses; len(got) != 1 || got[0].Prefix != 128 {
		t.Fatalf("invalid prefix was not clamped: %+v", got)
	}
}

func TestDiscoverInterfacesCapsAndFiltersNames(t *testing.T) {
	interfaces := DiscoverInterfaces(nil, fixtureSource(t, "many"))
	if len(interfaces) != protocol.MaxInterfaces {
		t.Fatalf("interfaces = %d, want %d", len(interfaces), protocol.MaxInterfaces)
	}
	if interfaces[0].Name != "eth0" || interfaces[0].State != "up" || interfaces[0].SpeedMbps == nil {
		t.Fatalf("first interface = %+v", interfaces[0])
	}

	src := mapSource(nil, map[string][]string{"/sys/class/net": {"good0", "bad name", "if!ace"}})
	filtered := DiscoverInterfaces(nil, src)
	if len(filtered) != 1 || filtered[0].Name != "good0" {
		t.Fatalf("filtered interfaces = %+v", filtered)
	}
}
