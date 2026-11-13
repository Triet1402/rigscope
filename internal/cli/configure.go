package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// configureEdit holds the settings one `rigscope configure` changes beyond the
// port, origin, proxy and storage fields it shares with install. Every value
// is parsed and checked for spelling before the configuration is read; the
// combined result is validated before anything is written.
type configureEdit struct {
	listen           string
	setListen        bool
	tlsCert          string
	tlsKey           string
	clearTLS         bool
	forwardedHeaders string
	hub              *bool
	collector        *bool
	sharing          []shareChange
	samplingInterval *time.Duration
	gpuRegistersOff  bool
	// hubFlags and collectorFlags name the flags given for each component, so
	// a change aimed at a component that is off can say which flag needs it.
	hubFlags       []string
	collectorFlags []string
}

// shareFamily is one collector sharing switch. Names are the spellings
// `configure --share` accepts; field selects the configuration key.
type shareFamily struct {
	name      string
	what      string
	sensitive string
	parent    string
	field     func(*config.Sharing) *bool
}

type shareChange struct {
	family shareFamily
	on     bool
}

var shareFamilies = []shareFamily{
	{name: "cpu", what: "CPU usage, temperature and power", field: func(s *config.Sharing) *bool { return &s.CPU }},
	{name: "memory", what: "memory use", field: func(s *config.Sharing) *bool { return &s.Memory }},
	{name: "swap", what: "swap use", field: func(s *config.Sharing) *bool { return &s.Swap }},
	{name: "gpus", what: "GPU readings", field: func(s *config.Sharing) *bool { return &s.GPUs }},
	{name: "disks", what: "filesystems, disks and disk activity", field: func(s *config.Sharing) *bool { return &s.Disks }},
	{name: "network", what: "network interface names and traffic", field: func(s *config.Sharing) *bool { return &s.Network }},
	{
		name: "network-addresses", what: "interface IP addresses", parent: "network",
		sensitive: "IP addresses identify this machine on its network; everyone who can view it in the dashboard will see them",
		field:     func(s *config.Sharing) *bool { return &s.NetworkAddresses },
	},
	{name: "processes", what: "process lists when someone opens them", field: func(s *config.Sharing) *bool { return &s.Processes }},
	{
		name: "process-arguments", what: "full process command lines", parent: "processes",
		sensitive: "command lines can contain passwords, tokens and private file paths; everyone who can view this machine's processes will see them",
		field:     func(s *config.Sharing) *bool { return &s.ProcessArguments },
	},
}

func shareFamilyNamed(name string) (shareFamily, bool) {
	key := strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(strings.TrimSpace(name)))
	for _, family := range shareFamilies {
		if strings.ReplaceAll(family.name, "-", "") == key {
			return family, true
		}
	}
	return shareFamily{}, false
}

func shareFamilyNames() string {
	names := make([]string, 0, len(shareFamilies))
	for _, family := range shareFamilies {
		names = append(names, family.name)
	}
	return strings.Join(names, ", ")
}

// parseShare reads one --share value: FAMILY=on|off, or several separated by
// commas. A family named twice with different values is refused.
func (e *configureEdit) parseShare(value string) error {
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, state, found := strings.Cut(item, "=")
		if !found {
			return fmt.Errorf("%q needs a value, such as %s=off", item, strings.TrimSpace(item))
		}
		family, ok := shareFamilyNamed(name)
		if !ok {
			return fmt.Errorf("unknown family %q; use one of %s", strings.TrimSpace(name), shareFamilyNames())
		}
		on, err := parseOnOff(state)
		if err != nil {
			return fmt.Errorf("%s: %v", family.name, err)
		}
		for _, existing := range e.sharing {
			if existing.family.name == family.name && existing.on != on {
				return fmt.Errorf("%s is set both on and off", family.name)
			}
		}
		e.sharing = append(e.sharing, shareChange{family: family, on: on})
	}
	return nil
}

func parseOnOff(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("use on or off, not %q", value)
	}
}

// parseListen accepts an IP address (brackets optional for IPv6) or
// localhost. Host names are refused: the hub binds one address, and a name
// that resolves differently later would silently move the listener.
func parseListen(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "localhost" {
		return trimmed, nil
	}
	ip := net.ParseIP(strings.Trim(trimmed, "[]"))
	if ip == nil {
		return "", fmt.Errorf("%q is not an IP address; use 127.0.0.1 (this machine only), 0.0.0.0 (every IPv4 interface), :: (every interface) or one interface address", value)
	}
	return ip.String(), nil
}

// runConfigure changes settings of an existing installation. It runs the
// install pipeline without replacing the binary, previews the effective
// change, asks once before interrupting a running service and restarts what it
// changed. --show prints the current settings instead.
func runConfigure(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("configure", deps.Stderr)
	show := flags.Bool("show", false, "print the current settings and the changes configure supports")
	asJSON := flags.Bool("json", false, "with --show, print the settings as JSON for scripts")
	system := flags.Bool("system", false, "configure the root-owned system installation")
	hubToggle := flags.String("hub", "", "turn the hub (dashboard) on or off on this machine")
	collectorToggle := flags.String("collector", "", "turn the collector on or off on this machine")
	passwordStdin := flags.Bool("password-stdin", false, "with --hub on for a hub that never ran, read the owner's first password from standard input")
	telemetryMode := flags.String("telemetry-mode", "", "hub history storage: memory (lost on restart) or persistent (saved to disk)")
	port := flags.Int("port", -1, "hub listen port")
	listen := flags.String("listen", "", "hub listen address: 127.0.0.1 (this machine only), 0.0.0.0, :: or one interface address")
	origin := flags.String("origin", "", "public hub origin such as https://hub.example.com")
	local := flags.Bool("local", false, "remove the public origin and serve local access again")
	tlsCert := flags.String("tls-cert", "", "PEM certificate the hub serves HTTPS with; use with --tls-key")
	tlsKey := flags.String("tls-key", "", "PEM private key for --tls-cert")
	noTLS := flags.Bool("no-tls", false, "stop serving HTTPS directly and forget the certificate and key paths")
	trustedProxy := flags.String("trusted-proxy", "", "comma-separated proxy addresses the hub trusts for forwarded headers, or none")
	forwardedHeaders := flags.String("forwarded-headers", "", "header family trusted from those proxies: x-forwarded or forwarded")
	edit := configureEdit{}
	flags.Func("share", "FAMILY=on|off: choose what this machine's collector shares; repeat for several", edit.parseShare)
	samplingInterval := flags.String("sampling-interval", "", "how often the collector samples, such as 2s (100ms to 1h)")
	gpuRegisters := flags.String("gpu-registers", "", "off: stop reading GPU register temperatures (install --gpu-registers turns them on)")
	noRestart := flags.Bool("no-restart", false, "stop running services for the change and leave them stopped")
	yes := addYesFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		return deps.errorf("configure takes no positional arguments, got %q", flags.Arg(0))
	}
	given := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })
	hubFlagNames := []string{"telemetry-mode", "port", "listen", "origin", "local", "tls-cert", "tls-key", "no-tls", "trusted-proxy", "forwarded-headers"}
	collectorFlagNames := []string{"share", "sampling-interval", "gpu-registers"}
	for _, name := range hubFlagNames {
		if given[name] {
			edit.hubFlags = append(edit.hubFlags, "--"+name)
		}
	}
	for _, name := range collectorFlagNames {
		if given[name] {
			edit.collectorFlags = append(edit.collectorFlags, "--"+name)
		}
	}
	changeRequested := len(edit.hubFlags)+len(edit.collectorFlags) > 0 || given["hub"] || given["collector"]

	if *asJSON && !*show {
		return deps.errorf("--json only works with --show")
	}
	if *show {
		if changeRequested || given["no-restart"] || given["password-stdin"] {
			return deps.errorf("--show only prints settings; run the change without --show")
		}
		return runConfigureShow(ctx, deps, *system, *asJSON)
	}
	if !changeRequested {
		return deps.errorf("configure needs a change, such as --port PORT, --origin URL, --share FAMILY=on|off or --sampling-interval 5s; see rigscope configure --show")
	}

	if *local && *origin != "" {
		return deps.errorf("choose --origin or --local, not both")
	}
	for _, toggle := range []struct {
		name  string
		value string
		into  **bool
	}{{"hub", *hubToggle, &edit.hub}, {"collector", *collectorToggle, &edit.collector}} {
		if !given[toggle.name] {
			continue
		}
		on, err := parseOnOff(toggle.value)
		if err != nil {
			if toggle.name == "hub" && strings.Contains(toggle.value, "://") {
				return deps.errorf("configure --hub takes on or off; to send this collector to another hub, run rigscope install --hub URL --token TOKEN")
			}
			return deps.errorf("--%s: %v", toggle.name, err)
		}
		*toggle.into = &on
	}
	if *passwordStdin && (edit.hub == nil || !*edit.hub) {
		return deps.errorf("--password-stdin only works with --hub on")
	}
	if *telemetryMode != "" && *telemetryMode != "memory" && *telemetryMode != "persistent" {
		return deps.errorf("--telemetry-mode must be memory or persistent")
	}
	if given["listen"] {
		value, err := parseListen(*listen)
		if err != nil {
			return deps.errorf("--listen: %v", err)
		}
		edit.listen, edit.setListen = value, true
	}
	if *noTLS && (*tlsCert != "" || *tlsKey != "") {
		return deps.errorf("choose --tls-cert/--tls-key or --no-tls, not both")
	}
	if (*tlsCert != "") != (*tlsKey != "") {
		return deps.errorf("give --tls-cert FILE and --tls-key FILE together")
	}
	if *tlsCert != "" {
		for _, file := range []struct {
			value *string
			into  *string
		}{{tlsCert, &edit.tlsCert}, {tlsKey, &edit.tlsKey}} {
			absolute, err := filepath.Abs(*file.value)
			if err != nil {
				return deps.errorf("resolve %s: %v", *file.value, err)
			}
			*file.into = absolute
		}
	}
	edit.clearTLS = *noTLS
	if given["forwarded-headers"] {
		switch *forwardedHeaders {
		case config.ForwardedHeadersXForwarded, config.ForwardedHeadersForwarded:
			edit.forwardedHeaders = *forwardedHeaders
		default:
			return deps.errorf("--forwarded-headers must be x-forwarded (X-Forwarded-For/-Proto/-Host) or forwarded (RFC 7239 Forwarded)")
		}
	}
	if given["sampling-interval"] {
		interval, err := time.ParseDuration(strings.TrimSpace(*samplingInterval))
		if err != nil {
			return deps.errorf("--sampling-interval: %q is not a duration; use a value such as 500ms, 2s or 1m", *samplingInterval)
		}
		if interval < 100*time.Millisecond || interval > time.Hour {
			return deps.errorf("--sampling-interval must be between 100ms and 1h, got %s", interval)
		}
		edit.samplingInterval = &interval
	}
	if given["gpu-registers"] {
		switch strings.ToLower(strings.TrimSpace(*gpuRegisters)) {
		case "off":
			edit.gpuRegistersOff = true
		case "on":
			return deps.errorf("turning GPU register temperatures on also sets up hardware permissions; run rigscope install --gpu-registers")
		default:
			return deps.errorf("--gpu-registers takes off")
		}
	}
	proxySet := given["trusted-proxy"]
	proxies, err := parseTrustedProxies(*trustedProxy)
	if proxySet && err != nil {
		return deps.errorf("--trusted-proxy: %v", err)
	}
	return runSetup(ctx, deps, setupRequest{
		mode:              modeConfigure,
		system:            *system,
		noRestart:         *noRestart,
		yes:               *yes,
		port:              *port,
		origin:            *origin,
		clearOrigin:       *local,
		telemetryMode:     *telemetryMode,
		trustedProxies:    proxies,
		setTrustedProxies: proxySet,
		passwordStdin:     *passwordStdin,
		edit:              edit,
	})
}

// apply writes the edit into cfg. It never touches a key the edit does not
// name, so unrelated settings survive unchanged.
func (e configureEdit) apply(cfg *config.Config) {
	hub := &cfg.Components.Hub
	collector := &cfg.Components.Collector
	if e.hub != nil {
		hub.Enabled = *e.hub
	}
	if e.collector != nil {
		collector.Enabled = *e.collector
	}
	if e.setListen {
		hub.Listen = e.listen
	}
	if e.tlsCert != "" {
		hub.TLSCert, hub.TLSKey = e.tlsCert, e.tlsKey
	}
	if e.clearTLS {
		hub.TLSCert, hub.TLSKey = "", ""
	}
	if e.forwardedHeaders != "" {
		hub.ForwardedHeaders = e.forwardedHeaders
	}
	for _, change := range e.sharing {
		*change.family.field(&collector.Sharing) = change.on
	}
	if e.samplingInterval != nil {
		collector.SamplingInterval = config.Duration(*e.samplingInterval)
	}
	if e.gpuRegistersOff {
		collector.Hardware.GPURegisters = false
	}
}

// checkComponents refuses a change aimed at a component that will be off, and
// a configuration with nothing left to run.
func (e configureEdit) checkComponents(cfg *config.Config) error {
	hub, collector := cfg.Components.Hub.Enabled, cfg.Components.Collector.Enabled
	if !hub && !collector {
		return errors.New("at least one component must stay on; to remove Rigscope from this machine, run rigscope uninstall")
	}
	if !hub && len(e.hubFlags) > 0 {
		return fmt.Errorf("%s changes the hub, which is off on this machine; add --hub on to turn it on", strings.Join(e.hubFlags, ", "))
	}
	if !collector && len(e.collectorFlags) > 0 {
		return fmt.Errorf("%s changes the collector, which is off on this machine; add --collector on to turn it on", strings.Join(e.collectorFlags, ", "))
	}
	return nil
}

// sensitiveOptIns lists the sensitive sharing families this change starts
// sending. A family counts only when it takes effect, so turning processes on
// while process-arguments is already on is an opt-in too. Each one always
// needs an explicit yes, even when no service restarts.
func sensitiveOptIns(before, after config.Sharing) []shareFamily {
	var families []shareFamily
	for _, family := range shareFamilies {
		if family.sensitive != "" && !family.effective(before) && family.effective(after) {
			families = append(families, family)
		}
	}
	return families
}

// effective reports whether the family is actually shared: on, and its parent
// family (if any) on too.
func (f shareFamily) effective(sharing config.Sharing) bool {
	if !*f.field(&sharing) {
		return false
	}
	if f.parent == "" {
		return true
	}
	parent, _ := shareFamilyNamed(f.parent)
	return *parent.field(&sharing)
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

// settingChanges describes the effective difference between two normalized
// configurations, one line per changed setting.
func settingChanges(paths config.Paths, before, after *config.Config) []string {
	var changes []string
	bh, ah := before.Components.Hub, after.Components.Hub
	bc, ac := before.Components.Collector, after.Components.Collector
	if bh.Enabled != ah.Enabled {
		if ah.Enabled {
			changes = append(changes, "hub: off -> on (runs the dashboard on this machine)")
		} else {
			changes = append(changes, fmt.Sprintf("hub: on -> off (the dashboard stops; accounts, machines and history stay in %s)", paths.ComponentStateDir(systemd.Hub.String())))
		}
	}
	if bc.Enabled != ac.Enabled {
		if ac.Enabled {
			changes = append(changes, "collector: off -> on (this machine reports to its hub)")
		} else {
			changes = append(changes, fmt.Sprintf("collector: on -> off (this machine stops reporting; its identity stays in %s)", paths.CollectorDir()))
		}
	}
	if bh.TelemetryMode != ah.TelemetryMode {
		changes = append(changes, fmt.Sprintf("hub telemetry storage: %s -> %s", bh.TelemetryMode, ah.TelemetryMode))
		changes = append(changes, "RAM history is not copied when switching storage modes; restarting loses current RAM history")
	}
	if bh.Listen != ah.Listen {
		changes = append(changes, fmt.Sprintf("hub listen address: %s -> %s", bh.Listen, ah.Listen))
	}
	if bh.Port != ah.Port {
		changes = append(changes, fmt.Sprintf("hub port: %d -> %d", bh.Port, ah.Port))
	}
	if bh.Origin != ah.Origin {
		changes = append(changes, fmt.Sprintf("hub origin: %s -> %s", originLabel(bh.Origin), originLabel(ah.Origin)))
	}
	if bh.TLSCert != ah.TLSCert || bh.TLSKey != ah.TLSKey {
		changes = append(changes, fmt.Sprintf("hub HTTPS: %s -> %s", tlsLabel(bh), tlsLabel(ah)))
	}
	if !slices.Equal(bh.TrustedProxies, ah.TrustedProxies) {
		changes = append(changes, fmt.Sprintf("trusted proxies: %s -> %s", proxyList(bh.TrustedProxies), proxyList(ah.TrustedProxies)))
	}
	if bh.ForwardedHeaders != ah.ForwardedHeaders {
		changes = append(changes, fmt.Sprintf("forwarded headers: %s -> %s", bh.ForwardedHeaders, ah.ForwardedHeaders))
	}
	if bc.SamplingInterval != ac.SamplingInterval {
		changes = append(changes, fmt.Sprintf("collector sampling interval: %s -> %s", bc.SamplingInterval.Value(), ac.SamplingInterval.Value()))
	}
	for _, family := range shareFamilies {
		was, now := *family.field(&bc.Sharing), *family.field(&ac.Sharing)
		if was != now {
			changes = append(changes, fmt.Sprintf("share %s (%s): %s -> %s", family.name, family.what, onOff(was), onOff(now)))
		}
	}
	if bc.Hardware.GPURegisters != ac.Hardware.GPURegisters {
		changes = append(changes, fmt.Sprintf("GPU register temperatures: %s -> %s", onOff(bc.Hardware.GPURegisters), onOff(ac.Hardware.GPURegisters)))
	}
	return changes
}

func originLabel(origin string) string {
	if origin == "" {
		return "local only"
	}
	return origin
}

func tlsLabel(hub config.Hub) string {
	if hub.TLSCert == "" {
		return "off"
	}
	return "certificate " + hub.TLSCert
}

// configureNotes explains consequences of the change that are not visible in
// the settings themselves: costs, sharing that has no effect, and what other
// machines or the local collector will notice.
func configureNotes(before, after *config.Config) []string {
	var notes []string
	bh, ah := before.Components.Hub, after.Components.Hub
	bc, ac := before.Components.Collector, after.Components.Collector
	if ac.Enabled && bc.SamplingInterval != ac.SamplingInterval {
		switch {
		case ac.SamplingInterval.Value() < time.Second:
			notes = append(notes, "Sampling more often than once a second uses more CPU on this machine and more hub memory, storage and network.")
		case ac.SamplingInterval.Value() > time.Minute:
			notes = append(notes, "Sampling less than once a minute makes the dashboard update slowly and can miss short spikes.")
		}
	}
	if ac.Enabled && bc.Sharing != ac.Sharing {
		for _, family := range shareFamilies {
			if family.parent != "" && *family.field(&ac.Sharing) && !family.effective(ac.Sharing) {
				notes = append(notes, fmt.Sprintf("%s has no effect while %s is off.", family.name, family.parent))
			}
		}
	}
	if bc.Hardware.GPURegisters && !ac.Hardware.GPURegisters {
		notes = append(notes, "GPU register permissions (udev rule and group) stay installed; rigscope uninstall removes them, and rigscope install --gpu-registers turns sampling back on.")
	}
	if bh.Enabled && !ah.Enabled && ac.Enabled && len(ac.Destinations) == 1 && isLocalDestination(ac.Destinations[0]) {
		notes = append(notes, "This machine's collector sends to this hub; it keeps retrying until the hub is turned on again or the collector is moved with rigscope install --hub URL --token TOKEN.")
	}
	if !bc.Enabled && ac.Enabled && len(ac.Destinations) == 0 && !ah.Enabled {
		notes = append(notes, "The collector has no hub to send to yet; connect it with rigscope install --role collector --hub URL --token TOKEN.")
	}
	if ah.Enabled && !listenIsLoopback(ah.Listen) && (bh.Listen != ah.Listen || !bh.Enabled) {
		notes = append(notes, fmt.Sprintf("The hub will accept connections from other machines on port %d; make sure your firewall allows only the networks you expect.", ah.Port))
	}
	return notes
}

func listenIsLoopback(listen string) bool {
	host := strings.TrimSpace(listen)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// hubChanges reports whether the hub will run with settings it did not have.
func hubChanges(before, after *config.Config) bool {
	bh, ah := before.Components.Hub, after.Components.Hub
	return ah.Enabled && (!bh.Enabled || !reflect.DeepEqual(bh, ah))
}

// hubListenerProblem applies the hub's own startup refusal for an exposed
// listener, so configure fails before writing instead of leaving a hub that
// cannot start.
func hubListenerProblem(before, after *config.Config) error {
	if !hubChanges(before, after) {
		return nil
	}
	ah := after.Components.Hub
	if listenIsLoopback(ah.Listen) {
		return nil
	}
	if ah.Origin == "" {
		return fmt.Errorf("listening on %s makes the hub reachable from other machines; also give --origin with the address people will open, such as https://hub.example.com", ah.Listen)
	}
	if ah.TLSCert == "" && len(ah.TrustedProxies) == 0 {
		return fmt.Errorf("the hub refuses to serve plain HTTP on %s: give --tls-cert FILE --tls-key FILE so it serves HTTPS, or put an HTTPS reverse proxy in front and list it with --trusted-proxy ADDRESS", ah.Listen)
	}
	return nil
}

// hubServiceHiddenTrees are the trees the system hub unit cannot see:
// ProtectHome=yes hides home directories and PrivateTmp=yes gives the service
// its own empty /tmp and /var/tmp.
var hubServiceHiddenTrees = []string{"/home", "/root", "/run/user", "/tmp", "/var/tmp"}

// checkHubResult validates what the hub will do with the new settings beyond
// the schema rules: the same refusal the hub applies at startup and the TLS
// files the service account must read. It returns notes to show with the
// preview. It runs before anything is written or stopped.
func (d Deps) checkHubResult(paths config.Paths, before, after *config.Config) ([]string, error) {
	if !hubChanges(before, after) {
		return nil, nil
	}
	bh, ah := before.Components.Hub, after.Components.Hub
	serveTLS := ah.TLSCert != ""
	// A change to the certificate or to the names it must cover is held to
	// every TLS rule. Any other hub change still restarts the hub, so the
	// pair must keep loading, but an existing certificate's expiry is then
	// reported rather than blocking an unrelated change.
	tlsTouched := !bh.Enabled || bh.TLSCert != ah.TLSCert || bh.TLSKey != ah.TLSKey || bh.Origin != ah.Origin || bh.Listen != ah.Listen
	var notes []string
	if serveTLS && tlsTouched && strings.HasPrefix(ah.Origin, "http://") {
		return nil, fmt.Errorf("the hub serves HTTPS with --tls-cert, so the origin must start with https://, not %s", ah.Origin)
	}
	if serveTLS {
		tlsNotes, err := d.checkTLSFiles(paths, ah, tlsTouched)
		if err != nil {
			return nil, err
		}
		notes = append(notes, tlsNotes...)
	}
	collector := after.Components.Collector
	if collector.Enabled && len(collector.Destinations) == 1 && isLocalDestination(collector.Destinations[0]) {
		if _, ok, _ := localHubDestination(ah); !ok {
			notes = append(notes, fmt.Sprintf("This machine's collector sends to %s and cannot follow the hub to its new address automatically; reconnect it with an enrollment grant (Administration > Add machine) and rigscope install --hub URL --token GRANT.", collector.Destinations[0].URL))
		}
	}
	return notes, nil
}

// checkTLSFiles proves the certificate and key load as a pair, are current
// and, in system scope, are visible to and readable by the hub's service
// account. It never prints key material.
func (d Deps) checkTLSFiles(paths config.Paths, hub config.Hub, strict bool) ([]string, error) {
	for _, file := range []struct{ flag, path string }{{"certificate", hub.TLSCert}, {"key", hub.TLSKey}} {
		if !filepath.IsAbs(file.path) {
			return nil, fmt.Errorf("TLS %s path %s must be absolute", file.flag, file.path)
		}
		info, err := os.Stat(file.path)
		if err != nil {
			return nil, fmt.Errorf("TLS %s %s: %v", file.flag, file.path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("TLS %s %s is not a regular file", file.flag, file.path)
		}
	}
	pair, err := tls.LoadX509KeyPair(hub.TLSCert, hub.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("the TLS certificate and key do not load as a pair: %v", err)
	}
	leaf := pair.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return nil, fmt.Errorf("parse TLS certificate: %v", err)
		}
	}
	var notes []string
	now := time.Now()
	switch {
	case now.After(leaf.NotAfter) && strict:
		return nil, fmt.Errorf("the TLS certificate expired on %s; renew it first", leaf.NotAfter.UTC().Format("2006-01-02"))
	case now.After(leaf.NotAfter):
		notes = append(notes, fmt.Sprintf("The TLS certificate expired on %s; browsers and collectors refuse it until you renew it.", leaf.NotAfter.UTC().Format("2006-01-02")))
	case now.Before(leaf.NotBefore) && strict:
		return nil, fmt.Errorf("the TLS certificate is not valid until %s; use a currently valid certificate", leaf.NotBefore.UTC().Format("2006-01-02 15:04 MST"))
	case now.Before(leaf.NotBefore):
		notes = append(notes, fmt.Sprintf("The TLS certificate is not valid until %s.", leaf.NotBefore.UTC().Format("2006-01-02 15:04 MST")))
	case leaf.NotAfter.Sub(now) < 30*24*time.Hour:
		notes = append(notes, fmt.Sprintf("The TLS certificate expires on %s.", leaf.NotAfter.UTC().Format("2006-01-02")))
	}
	if host := tlsHostToCover(hub); host != "" && strict {
		if err := leaf.VerifyHostname(host); err != nil {
			return nil, fmt.Errorf("the TLS certificate does not cover %s; use a certificate issued for this hostname", host)
		}
	}
	if paths.Scope == config.ScopeSystem {
		for _, path := range []string{hub.TLSCert, hub.TLSKey} {
			note, err := d.serviceCanRead(path, systemd.HubUser, hubServiceHiddenTrees)
			if err != nil {
				return nil, err
			}
			if note != "" {
				notes = append(notes, note)
			}
		}
	}
	if strict {
		restart := "rigscope restart hub"
		if paths.Scope == config.ScopeSystem {
			restart = "sudo rigscope restart hub"
		}
		notes = append(notes, fmt.Sprintf("The hub reads the certificate when it starts; after each renewal run %s.", restart))
	}
	return notes, nil
}

func tlsHostToCover(hub config.Hub) string {
	if hub.Origin != "" {
		if parsed, err := url.Parse(hub.Origin); err == nil {
			return parsed.Hostname()
		}
		return ""
	}
	host := strings.Trim(hub.Listen, "[]")
	switch host {
	case "", "0.0.0.0", "::":
		return ""
	}
	return host
}

// serviceCanRead checks that a system service account can open path for
// reading: the path is not in a tree the unit hides, every directory on the
// way is searchable and the file is readable by the account's uid or groups.
// A path with a POSIX ACL that the mode bits alone do not allow returns a note
// instead of an error, because the ACL may grant access.
func (d Deps) serviceCanRead(path, account string, hidden []string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %v", path, err)
	}
	for _, candidate := range []string{path, resolved} {
		for _, tree := range hidden {
			if candidate == tree || strings.HasPrefix(candidate, tree+"/") {
				return "", fmt.Errorf("%s is under %s, which the system hub service cannot see; keep TLS files in a system directory such as /etc/ssl/rigscope or /etc/letsencrypt", path, tree)
			}
		}
	}
	uid, gids, err := serviceAccountIDs(d.accounts(), account)
	if err != nil {
		return "", err
	}
	check := func(target string, want uint32) (bool, bool, error) {
		info, err := os.Stat(target)
		if err != nil {
			return false, false, err
		}
		if accessAllowed(info, uid, gids, want) {
			return true, false, nil
		}
		return false, hasPOSIXACL(target), nil
	}
	var acl bool
	for _, chain := range [][]string{parentDirs(path), parentDirs(resolved)} {
		for _, dir := range chain {
			ok, hasACL, err := check(dir, 1)
			if err != nil {
				return "", fmt.Errorf("inspect %s: %v", dir, err)
			}
			if ok {
				continue
			}
			if hasACL {
				acl = true
				continue
			}
			return "", unreadableError(path, dir, account, true)
		}
	}
	ok, hasACL, err := check(resolved, 4)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %v", resolved, err)
	}
	if !ok && !hasACL {
		return "", unreadableError(path, resolved, account, false)
	}
	if !ok || acl {
		return fmt.Sprintf("Could not confirm from file modes that %s can read %s (an ACL is set); the hub fails to start if it cannot.", account, path), nil
	}
	return "", nil
}

func unreadableError(path, blocker, account string, directory bool) error {
	if directory {
		return fmt.Errorf("the hub runs as %s, which cannot enter %s to reach %s; allow it, for example: sudo setfacl -m u:%s:x %s", account, blocker, path, account, blocker)
	}
	return fmt.Errorf("the hub runs as %s, which cannot read %s; allow it, for example: sudo setfacl -m u:%s:r %s", account, path, account, blocker)
}

// parentDirs returns every directory above path, root first.
func parentDirs(path string) []string {
	var dirs []string
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		dirs = append([]string{dir}, dirs...)
		if dir == "/" || dir == "." || dir == filepath.Dir(dir) {
			break
		}
	}
	return dirs
}

// serviceAccountIDs returns the uid and every group of a service account:
// its primary group, groups that list it as a member and, for Rigscope's
// system units, the shared group set by Group=.
func serviceAccountIDs(db AccountDatabase, account string) (int, []int, error) {
	passwd, err := db.Passwd()
	if err != nil {
		return 0, nil, fmt.Errorf("read accounts: %v", err)
	}
	uid, primary, found := 0, 0, false
	for _, record := range passwd {
		if record.Name == account {
			uid, primary, found = record.UID, record.GID, true
			break
		}
	}
	if !found {
		return 0, nil, fmt.Errorf("service account %s does not exist; repair the installation with rigscope install --system", account)
	}
	gids := []int{primary}
	if shared, ok := groupGID(db, systemd.SharedGroup); ok {
		gids = append(gids, shared)
	}
	if groups, err := db.Groups(); err == nil {
		for _, group := range groups {
			if slices.Contains(group.Members, account) {
				gids = append(gids, group.GID)
			}
		}
	}
	return uid, gids, nil
}

// accessAllowed applies the classic owner/group/other permission check for
// want (4 read, 1 search). Root bypasses mode bits.
func accessAllowed(info os.FileInfo, uid int, gids []int, want uint32) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if uid == 0 {
		return true
	}
	mode := uint32(info.Mode().Perm())
	switch {
	case int(stat.Uid) == uid:
		return mode&(want<<6) != 0
	case slices.Contains(gids, int(stat.Gid)):
		return mode&(want<<3) != 0
	default:
		return mode&want != 0
	}
}

func hasPOSIXACL(path string) bool {
	size, err := syscall.Getxattr(path, "system.posix_acl_access", nil)
	return err == nil && size > 0
}

// cloneConfig copies a configuration, including its slices, so a "before"
// snapshot cannot change when the working copy is edited.
func cloneConfig(cfg *config.Config) config.Config {
	out := *cfg
	out.Components.Hub.TrustedProxies = slices.Clone(cfg.Components.Hub.TrustedProxies)
	out.Components.Collector.Destinations = slices.Clone(cfg.Components.Collector.Destinations)
	return out
}

// configureServicePlan says what configure does with each service.
type configureServicePlan struct {
	// restart lists running components whose settings change.
	restart []systemd.Component
	// turnOff lists running components the change turns off.
	turnOff []systemd.Component
	// start lists components the change turns on.
	start []systemd.Component
	// kept lists running components the change does not affect.
	kept []systemd.Component
	// idle lists enabled components that are not running and stay so.
	idle []systemd.Component
}

func planConfigureServices(active, changed []systemd.Component, before, after *config.Config, leaveStopped bool) configureServicePlan {
	var plan configureServicePlan
	for _, component := range installComponents {
		running := containsComponent(active, component)
		was, now := componentEnabled(before, component), componentEnabled(after, component)
		switch {
		case !now:
			if running {
				plan.turnOff = append(plan.turnOff, component)
			}
		case running && containsComponent(changed, component):
			plan.restart = append(plan.restart, component)
		case running:
			plan.kept = append(plan.kept, component)
		case !was && !leaveStopped:
			plan.start = append(plan.start, component)
		default:
			plan.idle = append(plan.idle, component)
		}
	}
	return plan
}

// approveConfigure explains every service interruption and sensitive opt-in
// and asks once. --yes approves; a change that interrupts nothing and turns no
// sensitive sharing on needs no answer.
func (d Deps) approveConfigure(plan configureServicePlan, leaveStopped bool, sensitive []shareFamily, assumeYes bool) error {
	marker := d.errStyle(styleCyan+";"+styleBold, "::")
	if len(plan.restart) > 0 {
		effect := "restart so the changes take effect"
		if leaveStopped {
			effect = "stop and stay stopped"
		}
		verb := "is"
		if len(plan.restart) > 1 {
			verb = "are"
		}
		fmt.Fprintf(d.Stderr, "%s The %s %s running and will %s.\n", marker, d.errStyle(styleBold, componentList(plan.restart)), verb, effect)
	}
	if len(plan.turnOff) > 0 {
		fmt.Fprintf(d.Stderr, "%s The running %s will stop and stay off.\n", marker, d.errStyle(styleBold, componentList(plan.turnOff)))
	}
	for _, family := range sensitive {
		fmt.Fprintf(d.Stderr, "%s %s %s: %s.\n", d.errStyle(styleYellow+";"+styleBold, "!!"), d.errStyle(styleBold, family.name+"=on"), "shares "+family.what, family.sensitive)
	}
	if len(plan.restart)+len(plan.turnOff)+len(sensitive) == 0 || assumeYes {
		return nil
	}
	ok, err := d.confirm("Continue?")
	if errors.Is(err, errNoTerminal) {
		return errors.New("this change needs confirmation and there is no terminal to ask on; run again with --yes to approve")
	}
	if err != nil {
		return err
	}
	if !ok {
		return errCancelled
	}
	return nil
}

// runConfigurePlan stops, restarts and starts services as planned and reports
// the outcome.
func (d Deps) runConfigurePlan(ctx context.Context, manager systemd.Manager, plan configureServicePlan, leaveStopped bool) (started, restarted, stopped, kept, turnedOff []string, err error) {
	// Stop in quiesce order: the collector before its hub.
	for i := len(installComponents) - 1; i >= 0; i-- {
		component := installComponents[i]
		off := containsComponent(plan.turnOff, component)
		if !off && !(leaveStopped && containsComponent(plan.restart, component)) {
			continue
		}
		if err := manager.Stop(ctx, component.UnitName()); err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("stop %s: %v", component, err)
		}
		if off {
			turnedOff = append(turnedOff, component.String())
		} else {
			stopped = append(stopped, component.String())
		}
	}
	for _, component := range installComponents {
		switch {
		case containsComponent(plan.kept, component):
			kept = append(kept, component.String())
		case leaveStopped:
		case containsComponent(plan.restart, component):
			if err := manager.Restart(ctx, component.UnitName()); err != nil {
				return nil, nil, nil, nil, nil, fmt.Errorf("restart %s: %v", component, err)
			}
			restarted = append(restarted, component.String())
		case containsComponent(plan.start, component):
			if err := manager.Start(ctx, component.UnitName()); err != nil {
				return nil, nil, nil, nil, nil, fmt.Errorf("start %s: %v", component, err)
			}
			started = append(started, component.String())
		}
	}
	return started, restarted, stopped, kept, turnedOff, nil
}

// settingsView is the --show --json document. It carries settings and service
// state only: enrollment grants, credentials and password material live in
// private state files and are never read here.
type settingsView struct {
	Scope      string `json:"scope"`
	ConfigFile string `json:"configFile"`
	Components struct {
		Hub       componentView `json:"hub"`
		Collector componentView `json:"collector"`
	} `json:"components"`
	Hub       hubSettingsView       `json:"hub"`
	Collector collectorSettingsView `json:"collector"`
}

type componentView struct {
	Enabled bool `json:"enabled"`
	// Service is "active", "inactive", "unknown", or "off" for a disabled
	// component.
	Service string `json:"service"`
}

type hubSettingsView struct {
	URL              string   `json:"url"`
	Listen           string   `json:"listen"`
	Port             int      `json:"port"`
	Origin           string   `json:"origin"`
	TLS              tlsView  `json:"tls"`
	TrustedProxies   []string `json:"trustedProxies"`
	ForwardedHeaders string   `json:"forwardedHeaders"`
	TelemetryMode    string   `json:"telemetryMode"`
}

type tlsView struct {
	Enabled  bool   `json:"enabled"`
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
}

type collectorSettingsView struct {
	Destination      *config.Destination `json:"destination"`
	SamplingInterval string              `json:"samplingInterval"`
	Sharing          map[string]bool     `json:"sharing"`
	GPURegisters     bool                `json:"gpuRegisters"`
}

func buildSettingsView(ctx context.Context, manager systemd.Manager, paths config.Paths, cfg *config.Config) settingsView {
	var view settingsView
	view.Scope = scopeLabel(paths.Scope)
	view.ConfigFile = paths.ConfigFile
	service := func(component systemd.Component) componentView {
		if !componentEnabled(cfg, component) {
			return componentView{Enabled: false, Service: "off"}
		}
		active, err := manager.IsActive(ctx, component.UnitName())
		switch {
		case err != nil:
			return componentView{Enabled: true, Service: "unknown"}
		case active:
			return componentView{Enabled: true, Service: "active"}
		default:
			return componentView{Enabled: true, Service: "inactive"}
		}
	}
	view.Components.Hub = service(systemd.Hub)
	view.Components.Collector = service(systemd.Collector)
	hub := cfg.Components.Hub
	view.Hub = hubSettingsView{
		URL:              hubURLForDisplay(cfg),
		Listen:           hub.Listen,
		Port:             hub.Port,
		Origin:           hub.Origin,
		TLS:              tlsView{Enabled: hub.TLSCert != "", CertFile: hub.TLSCert, KeyFile: hub.TLSKey},
		TrustedProxies:   slices.Clone(hub.TrustedProxies),
		ForwardedHeaders: hub.ForwardedHeaders,
		TelemetryMode:    hub.TelemetryMode,
	}
	collector := cfg.Components.Collector
	view.Collector = collectorSettingsView{
		SamplingInterval: collector.SamplingInterval.Value().String(),
		Sharing:          map[string]bool{},
		GPURegisters:     collector.Hardware.GPURegisters,
	}
	if len(collector.Destinations) > 0 {
		destination := collector.Destinations[0]
		view.Collector.Destination = &destination
	}
	sharing := collector.Sharing
	for _, family := range shareFamilies {
		view.Collector.Sharing[family.name] = *family.field(&sharing)
	}
	return view
}

// runConfigureShow prints the current settings. It reads only the
// configuration file and service state.
func runConfigureShow(ctx context.Context, deps Deps, system, asJSON bool) int {
	scope := deps.installedScope(system)
	paths, err := deps.paths(scope)
	if err != nil {
		return deps.errorf("%v", err)
	}
	if !deps.hasInstallation(scope) {
		return deps.errorf("configure: no Rigscope installation in %s scope; run rigscope install first", scopeLabel(scope))
	}
	cfg, err := config.Load(paths.ConfigFile)
	if errors.Is(err, fs.ErrPermission) && scope == config.ScopeSystem {
		if code, done := deps.ensureRoot(scope); done {
			return code
		}
	}
	if err != nil {
		return deps.errorf("%v", err)
	}
	view := buildSettingsView(ctx, deps.manager(scope), paths, cfg)
	if asJSON {
		data, err := json.MarshalIndent(view, "", "  ")
		if err != nil {
			return deps.errorf("encode settings: %v", err)
		}
		fmt.Fprintln(deps.Stdout, string(data))
		return 0
	}
	printSettingsView(deps, view)
	return 0
}

func printSettingsView(deps Deps, view settingsView) {
	w := deps.Stdout
	heading := func(text string) { fmt.Fprintf(w, "\n%s\n", deps.out(styleBold, text)) }
	row := func(label, value string) { fmt.Fprintf(w, "  %-19s %s\n", label, value) }
	fmt.Fprintf(w, "%s (%s scope)\n", deps.out(styleBold, "Rigscope settings"), view.Scope)
	row("config file", view.ConfigFile)

	heading("Components")
	for _, entry := range []struct {
		name string
		view componentView
	}{{"hub", view.Components.Hub}, {"collector", view.Components.Collector}} {
		state := "on, " + map[string]string{"active": "running", "inactive": "not running", "unknown": "service state unknown"}[entry.view.Service]
		if !entry.view.Enabled {
			state = "off"
		}
		row(entry.name, state)
	}

	hub := view.Hub
	heading("Hub")
	if !view.Components.Hub.Enabled {
		fmt.Fprintln(w, "  off on this machine (turn it on with rigscope configure --hub on)")
	} else {
		row("open at", deps.out(styleCyan, hub.URL))
		row("listen", fmt.Sprintf("%s port %d", hub.Listen, hub.Port))
		row("public origin", originLabel(hub.Origin))
		if hub.TLS.Enabled {
			row("HTTPS", "on")
			row("  certificate", hub.TLS.CertFile)
			row("  key", hub.TLS.KeyFile)
		} else {
			row("HTTPS", "off (served by a proxy, or local only)")
		}
		row("trusted proxies", proxyList(hub.TrustedProxies))
		row("forwarded headers", hub.ForwardedHeaders)
		storage := "memory (history is lost when the hub restarts)"
		if hub.TelemetryMode == "persistent" {
			storage = "persistent (history is saved to disk)"
		}
		row("history storage", storage)
	}

	collector := view.Collector
	heading("Collector")
	if !view.Components.Collector.Enabled {
		fmt.Fprintln(w, "  off on this machine (turn it on with rigscope configure --collector on)")
	} else {
		destination := "none yet (connect with rigscope install --hub URL --token TOKEN)"
		if collector.Destination != nil {
			destination = collector.Destination.URL
			if collector.Destination.CAFile != "" {
				destination += " (CA " + collector.Destination.CAFile + ")"
			}
		}
		row("sends to", destination)
		row("sampling interval", collector.SamplingInterval)
		row("GPU registers", onOff(collector.GPURegisters))
		var shared, withheld []string
		for _, family := range shareFamilies {
			switch {
			case !collector.Sharing[family.name]:
				withheld = append(withheld, family.name)
			case family.parent != "" && !collector.Sharing[family.parent]:
				withheld = append(withheld, family.name+" (on, but "+family.parent+" is off)")
			default:
				shared = append(shared, family.name)
			}
		}
		row("shares", strings.Join(shared, ", "))
		if len(withheld) > 0 {
			row("keeps private", strings.Join(withheld, ", "))
		}
	}

	heading("Change a setting")
	prefix := "rigscope configure"
	if view.Scope == "system" {
		prefix = "sudo rigscope configure --system"
	}
	examples := []string{}
	if view.Components.Collector.Enabled {
		examples = append(examples,
			"--share FAMILY=on|off         what this machine shares",
			"--sampling-interval 5s        how often it samples",
		)
		if collector.GPURegisters {
			examples = append(examples, "--gpu-registers off           stop GPU register temperatures")
		}
	}
	if view.Components.Hub.Enabled {
		examples = append(examples,
			"--telemetry-mode persistent   keep history across restarts",
			"--port PORT / --origin URL    where the dashboard answers",
			"--listen 0.0.0.0 --tls-cert FILE --tls-key FILE   serve HTTPS to other machines",
		)
	}
	examples = append(examples, "--hub on|off / --collector on|off   choose what runs here")
	for _, example := range examples {
		fmt.Fprintf(w, "  %s %s\n", prefix, example)
	}
	fmt.Fprintln(w, deps.out(styleDim, "  Each change is previewed and asks before restarting a service. Add --json to --show for scripts."))
}
