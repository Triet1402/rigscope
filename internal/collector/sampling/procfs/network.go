package procfs

import (
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Triet1402/rigscope/internal/protocol"
)

var interfaceNameRE = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,64}$`)

// ParseSpeedMbps parses a sysfs link speed. Non-positive speeds, Linux's
// unknown sentinel (4294967295) and implausible values are nil.
func ParseSpeedMbps(raw *string) *int64 {
	if raw == nil {
		return nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(*raw), 10, 64)
	if err != nil || value <= 0 || value > 10_000_000 {
		return nil
	}
	return &value
}

// ParseInterfaceState maps sysfs operstate to the contract's three states.
func ParseInterfaceState(raw *string) string {
	if raw == nil {
		return "unknown"
	}
	switch strings.ToLower(strings.TrimSpace(*raw)) {
	case "up":
		return "up"
	case "down", "lowerlayerdown", "dormant", "notpresent":
		return "down"
	default:
		return "unknown"
	}
}

// DiscoverInterfaces enumerates interfaces from the address source, augmented
// with names present in sysfs so down links without addresses still show up.
// Loopback is kept; virtual links are marked so the UI never counts them as
// physical hardware.
func DiscoverInterfaces(addresses InterfaceAddressSource, src Source) []InterfaceInventory {
	byName := make(map[string][]InterfaceAddress)
	if addresses != nil {
		if reported, err := addresses(); err == nil {
			byName = reported
		}
	}
	names := make(map[string]bool, len(byName)+8)
	for name := range byName {
		names[name] = true
	}
	if entries, err := src.ReadDir("/sys/class/net"); err == nil {
		for _, name := range entries {
			names[name] = true
		}
	}
	virtualNames := make(map[string]bool)
	if entries, err := src.ReadDir("/sys/devices/virtual/net"); err == nil {
		for _, name := range entries {
			virtualNames[name] = true
		}
	}

	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	interfaces := make([]InterfaceInventory, 0, len(sorted))
	for _, name := range sorted {
		if !interfaceNameRE.MatchString(name) {
			continue
		}
		base := "/sys/class/net/" + name
		var mac *string
		if raw := strings.TrimSpace(readTextOrZero(src, base+"/address")); raw != "" {
			raw = CapText(raw, 64)
			mac = &raw
		}
		var speedRaw *string
		if raw, ok := readOptional(src, base+"/speed"); ok {
			speedRaw = &raw
		}
		speed := ParseSpeedMbps(speedRaw)
		var stateRaw *string
		if raw, ok := readOptional(src, base+"/operstate"); ok {
			stateRaw = &raw
		}
		state := ParseInterfaceState(stateRaw)

		interfaces = append(interfaces, InterfaceInventory{
			ID:        "net:" + name,
			Name:      name,
			MAC:       mac,
			Addresses: addressesFor(byName[name]),
			Virtual:   virtualNames[name],
			State:     state,
			SpeedMbps: speed,
		})
		if len(interfaces) >= protocol.MaxInterfaces {
			break
		}
	}
	return interfaces
}

func addressesFor(entries []InterfaceAddress) []InterfaceAddress {
	addresses := make([]InterfaceAddress, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		family := strings.ToLower(entry.Family)
		if family != "ipv4" && family != "ipv6" {
			continue
		}
		if len(entry.Address) < 2 || len(entry.Address) > 64 {
			continue
		}
		maxPrefix := 128
		if family == "ipv4" {
			maxPrefix = 32
		}
		prefix := entry.Prefix
		if prefix < 0 || prefix > maxPrefix {
			prefix = maxPrefix
		}
		key := family + ":" + entry.Address + "/" + strconv.Itoa(prefix)
		if seen[key] {
			continue
		}
		seen[key] = true
		addresses = append(addresses, InterfaceAddress{Family: family, Address: entry.Address, Prefix: prefix})
		if len(addresses) >= protocol.MaxAddressesPerInterface {
			break
		}
	}
	return addresses
}

// OSInterfaceAddresses enumerates live interfaces through the net package.
// Interfaces without addresses are omitted here; DiscoverInterfaces adds the
// sysfs-only names.
func OSInterfaceAddresses() (map[string][]InterfaceAddress, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make(map[string][]InterfaceAddress, len(interfaces))
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			family := "ipv6"
			maxPrefix := 128
			ip := ipNet.IP
			if ipv4 := ip.To4(); ipv4 != nil {
				family = "ipv4"
				maxPrefix = 32
				ip = ipv4
			}
			prefix := maxPrefix
			if ones, bits := ipNet.Mask.Size(); bits > 0 {
				prefix = ones
			}
			result[iface.Name] = append(result[iface.Name], InterfaceAddress{
				Family:  family,
				Address: ip.String(),
				Prefix:  prefix,
			})
		}
	}
	return result, nil
}
