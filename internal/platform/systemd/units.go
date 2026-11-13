package systemd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Scope selects user or system units.
type Scope string

const (
	User   Scope = "user"
	System Scope = "system"
)

// Component is one independently managed Rigscope service.
type Component string

const (
	Hub       Component = "hub"
	Collector Component = "collector"
)

// ParseComponent parses "hub" or "collector".
func ParseComponent(value string) (Component, bool) {
	switch Component(value) {
	case Hub:
		return Hub, true
	case Collector:
		return Collector, true
	default:
		return "", false
	}
}

// UnitName is the stable 1.0 unit name. It deliberately does not use the 0.2
// hashed installation form.
func (c Component) UnitName() string {
	return "rigscope-" + string(c) + ".service"
}

// String returns the component name.
func (c Component) String() string { return string(c) }

// System account and group names. The hub and collector never run as root.
const (
	HubUser         = "rigscope-hub"
	CollectorUser   = "rigscope-collector"
	SharedGroup     = "rigscope"
	RAPLGroup       = "rigscope-rapl"
	GPUGroup        = "rigscope-gpu"
	markerPrefix    = "# Managed by rigscope."
	DefaultUnitMode = 0o644
)

// Options describes one unit to render.
type Options struct {
	Component Component
	Scope     Scope
	// Binary and ConfigFile are used in ExecStart.
	Binary     string
	ConfigFile string
	// StateDir is the component's writable state directory. In system scope it
	// is the only ReadWritePaths entry.
	StateDir string
	// User and Group default to the dedicated system account for the
	// component. They are ignored in user scope.
	User  string
	Group string
	// SupplementaryGroups is only used for the system collector. The installer
	// passes exactly the hardware groups whose udev rules it installed, so a
	// hub-only install never references a group that does not exist.
	SupplementaryGroups []string
}

// Render produces a complete unit file. Callers write it atomically with mode
// 0644.
func Render(options Options) (string, error) {
	if err := options.validate(); err != nil {
		return "", err
	}
	if options.Scope == User {
		return renderUser(options), nil
	}
	return renderSystem(options), nil
}

// IsManaged reports whether content was written by Rigscope 1.0. Uninstall
// refuses to remove files without this marker.
func IsManaged(content string) bool {
	return strings.HasPrefix(content, markerPrefix)
}

func (o Options) validate() error {
	switch o.Component {
	case Hub, Collector:
	default:
		return fmt.Errorf("unknown component %q", o.Component)
	}
	switch o.Scope {
	case User, System:
	default:
		return fmt.Errorf("unknown scope %q", o.Scope)
	}
	if !filepath.IsAbs(o.Binary) {
		return errors.New("binary path must be absolute")
	}
	if !filepath.IsAbs(o.ConfigFile) {
		return errors.New("config path must be absolute")
	}
	if o.Scope == System && !filepath.IsAbs(o.StateDir) {
		return errors.New("state directory must be absolute")
	}
	if o.Scope == System && !rawPathAllowed(o.StateDir) {
		return fmt.Errorf("state directory %q cannot be written to a unit file: it has a control character or surrounding whitespace", o.StateDir)
	}
	return nil
}

func (o Options) account() string {
	if o.Component == Collector {
		return CollectorUser
	}
	return HubUser
}

func renderUser(options Options) string {
	var b strings.Builder
	writeMarker(&b, options)
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", description(options.Component))
	b.WriteString("Documentation=https://rigscope.dev/docs\n")
	b.WriteString("After=network.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s %s --config %s\n", quote(options.Binary), options.Component, quote(options.ConfigFile))
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5s\n")
	b.WriteString("UMask=0077\n")
	b.WriteString("KillMode=control-group\n")
	fmt.Fprintf(&b, "TimeoutStopSec=%ds\n", stopTimeout(options.Component))
	b.WriteString("StandardOutput=journal\n")
	b.WriteString("StandardError=journal\n")
	fmt.Fprintf(&b, "SyslogIdentifier=rigscope-%s\n\n", options.Component)
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

func renderSystem(options Options) string {
	user := options.User
	if user == "" {
		user = options.account()
	}
	// Both components share a group so the root-owned config file can be
	// mode 0640 and still be read by either service account.
	group := options.Group
	if group == "" {
		group = SharedGroup
	}
	groups := options.SupplementaryGroups
	var b strings.Builder
	writeMarker(&b, options)
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", description(options.Component))
	b.WriteString("Documentation=https://rigscope.dev/docs\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("StartLimitIntervalSec=60\n")
	b.WriteString("StartLimitBurst=5\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "User=%s\n", user)
	fmt.Fprintf(&b, "Group=%s\n", group)
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", rawPath(options.StateDir))
	fmt.Fprintf(&b, "ExecStart=%s %s --config %s\n", quote(options.Binary), options.Component, quote(options.ConfigFile))
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=2s\n")
	b.WriteString("KillSignal=SIGTERM\n")
	b.WriteString("UMask=0077\n")
	b.WriteString("NoNewPrivileges=yes\n")
	b.WriteString("PrivateTmp=yes\n")
	if options.Component == Hub {
		b.WriteString("PrivateDevices=yes\n")
	}
	b.WriteString("ProtectSystem=strict\n")
	b.WriteString("ProtectHome=yes\n")
	b.WriteString("ProtectKernelTunables=yes\n")
	b.WriteString("ProtectKernelModules=yes\n")
	b.WriteString("ProtectKernelLogs=yes\n")
	b.WriteString("ProtectControlGroups=yes\n")
	b.WriteString("RestrictRealtime=yes\n")
	b.WriteString("RestrictSUIDSGID=yes\n")
	b.WriteString("RestrictNamespaces=yes\n")
	b.WriteString("LockPersonality=yes\n")
	b.WriteString("RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6\n")
	b.WriteString("CapabilityBoundingSet=\n")
	b.WriteString("AmbientCapabilities=\n")
	if len(groups) > 0 {
		fmt.Fprintf(&b, "SupplementaryGroups=%s\n", strings.Join(groups, " "))
	}
	fmt.Fprintf(&b, "ReadWritePaths=%s\n", quote(options.StateDir))
	fmt.Fprintf(&b, "ReadOnlyPaths=%s\n", quote(options.Binary))
	fmt.Fprintf(&b, "TimeoutStopSec=%ds\n", stopTimeout(options.Component))
	b.WriteString("StandardOutput=journal\n")
	b.WriteString("StandardError=journal\n")
	fmt.Fprintf(&b, "SyslogIdentifier=rigscope-%s\n\n", options.Component)
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String()
}

func writeMarker(b *strings.Builder, options Options) {
	fmt.Fprintf(b, "%s Do not edit; run \"rigscope install\" to rewrite.\n", markerPrefix)
	fmt.Fprintf(b, "# Component: %s; scope: %s\n", options.Component, options.Scope)
}

func description(component Component) string {
	if component == Hub {
		return "Rigscope hub"
	}
	return "Rigscope collector"
}

func stopTimeout(component Component) int {
	if component == Hub {
		return 15
	}
	return 10
}

// quote makes one systemd ExecStart/ReadWritePaths argument. systemd expands
// "$" and "%" even inside quotes, so both are escaped.
func quote(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
		"\"", "\\\"",
		"%", "%%",
		"$", "$$",
	)
	return "\"" + replacer.Replace(value) + "\""
}

// rawPath renders a path for a setting that takes the rest of the line as one
// path without quote processing, such as WorkingDirectory=. Quotes there are
// part of the value, so systemd rejects a quoted path as "not absolute"; only
// specifiers ("%") are expanded and need escaping. validate keeps control
// characters and surrounding whitespace out of such paths.
func rawPath(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

func rawPathAllowed(value string) bool {
	if strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

var legacyUnitPattern = regexp.MustCompile(`^rigscope-[0-9a-f]{12}(-[a-z]+)?\.service$`)

// LegacyUnits returns 0.2 unit filenames in a unit directory. 0.2 used
// rigscope-<12 hex>-hub.service, -collector.service and an older combined
// rigscope-<12 hex>.service. The 1.0 names never match.
func LegacyUnits(unitDir string) ([]string, error) {
	entries, err := os.ReadDir(unitDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var legacy []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if legacyUnitPattern.MatchString(entry.Name()) {
			legacy = append(legacy, entry.Name())
		}
	}
	return legacy, nil
}

// IsLegacyManaged reports whether content carries a 0.2 Rigscope marker. 0.2
// generated files start with "# Managed by Rigscope:"; 1.0 writes
// "# Managed by rigscope.". A 0.2 udev rule at the same path is still
// Rigscope's to replace, so install recognises it before rewriting.
func IsLegacyManaged(content string) bool {
	return strings.HasPrefix(content, "# Managed by Rigscope:")
}

// IsLegacySystemManaged reports whether content looks like a 0.2 system-scope
// unit, which reused the stable rigscope-hub.service and
// rigscope-collector.service names. 0.2 markers and its /opt/rigscope
// ExecStart are recognised; the current 1.x marker never matches.
func IsLegacySystemManaged(content string) bool {
	for _, marker := range []string{
		"# Managed by `rigscope system`",
		"# Managed by Rigscope system installation",
		"/opt/rigscope/",
	} {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}
