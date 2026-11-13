package cli

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Triet1402/rigscope/internal/collector/state"
	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/hub/storage"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// autoPairGrantTTL is deliberately longer than an administrator-issued grant:
// the hub may take a while to start for the first time. The hub imports the
// grant before it listens, or lazily on the first enrollment request when it was
// already running, so the collector's retry loop only has to wait for the
// process to come up.
const autoPairGrantTTL = storage.MaxBootstrapGrantTTL

// autoPairing is the one-time state an install stages when it enables both
// components locally and the collector has no destination yet. When the
// collector already holds a credential for the local hub, grant is empty and
// only the destination is recorded.
type autoPairing struct {
	destination config.Destination
	// collectorID is the installation identity the grant is bound to. The hub
	// rejects a redemption by any other collector id.
	collectorID string
	grant       string
	grantHash   string
	expiresAt   time.Time
	// notice explains why automatic pairing was declined. It is printed for
	// the operator instead of leaving a silently dormant collector.
	notice string
	// rebindFrom is the hub's previous local address when only its port or
	// origin changed and the collector already holds a credential for it.
	// The credential moves to destination; no new grant is needed.
	rebindFrom string
}

// planAutoPairing decides whether install should pair the local collector with
// the local hub. It records the destination on cfg and returns the grant
// material to stage. It is a no-op when either component is disabled, when a
// different hub destination is already configured, or when the hub cannot be
// reached in a way its Host policy accepts.
//
// Re-running install repairs an unfinished pairing: when the configured
// destination is the local automatic one and no credential is stored, a fresh
// grant with a fresh TTL is staged. The collector's existing recovery proof is
// preserved, so a lost enrollment response can still be replayed.
//
// priorHub is the hub configuration before this run's flags. When the stored
// destination is exactly the local address that configuration produced and
// the collector holds a credential for it, the hub only moved: the credential
// is rebound to the new address instead of leaving the collector stranded.
func planAutoPairing(deps Deps, paths config.Paths, cfg *config.Config, priorHub config.Hub) (*autoPairing, error) {
	if !cfg.Components.Hub.Enabled || !cfg.Components.Collector.Enabled {
		return nil, nil
	}
	destination, ok, notice := localHubDestination(cfg.Components.Hub)
	if !ok {
		if notice != "" {
			return &autoPairing{notice: notice}, nil
		}
		return nil, nil
	}
	// A hub that accepts forwarded headers from a loopback peer cannot tell that
	// reverse proxy apart from a collector on the same machine, so the hub
	// refuses a colocated redemption from it. Staging a grant the hub will never
	// honour would leave a silently dormant collector; an already-issued
	// credential keeps working, so only a new pairing is declined.
	if hubTrustsLoopbackProxy(cfg.Components.Hub) {
		enrolled, err := collectorHasCredential(paths, destination.URL)
		if err != nil {
			return nil, err
		}
		if !enrolled {
			return &autoPairing{notice: loopbackProxyNotice(destination.URL)}, nil
		}
	}
	switch len(cfg.Components.Collector.Destinations) {
	case 0:
	case 1:
		existing := cfg.Components.Collector.Destinations[0]
		if !sameDestination(existing, destination) {
			// An explicit, non-local destination is an operator choice. A
			// previous automatic loopback destination may be stale (the port
			// changed) or replaced by a now-configured origin, so an
			// unenrolled local collector is re-pointed; an enrolled one is
			// left for an explicit replacement-grant migration.
			if !isLocalDestination(existing) {
				return nil, nil
			}
			enrolled, err := collectorHasCredential(paths, existing.URL)
			if err != nil {
				return nil, err
			}
			if enrolled {
				previous, ok, _ := localHubDestination(priorHub)
				if !ok || !sameDestination(existing, previous) {
					return nil, nil
				}
				cfg.Components.Collector.Destinations = []config.Destination{destination}
				return &autoPairing{destination: destination, rebindFrom: existing.URL}, nil
			}
		}
	default:
		// Configuration validation rejects more than one destination.
		return nil, nil
	}
	cfg.Components.Collector.Destinations = []config.Destination{destination}

	enrolled, err := collectorHasCredential(paths, destination.URL)
	if err != nil {
		return nil, err
	}
	if enrolled {
		return &autoPairing{destination: destination}, nil
	}
	collectorID, err := collectorInstallationID(deps, paths)
	if err != nil {
		return nil, err
	}
	token, hash, err := storage.NewEnrollmentGrantToken()
	if err != nil {
		return nil, fmt.Errorf("generate pairing grant: %w", err)
	}
	return &autoPairing{
		destination: destination,
		collectorID: collectorID,
		grant:       token,
		grantHash:   hash,
		expiresAt:   time.Now().Add(autoPairGrantTTL).UTC(),
	}, nil
}

// loopbackProxyNotice explains why a hub that trusts a loopback reverse proxy
// cannot pair its own collector automatically.
func loopbackProxyNotice(destination string) string {
	return "automatic pairing is unavailable because the hub trusts a loopback reverse proxy, which cannot be told apart from a local collector; " +
		manualPairingHint(destination)
}

// manualPairingHint tells the operator how to pair this machine's collector by
// hand. (Re-running install never disables a component, so --role both only
// states the intent: this machine keeps its hub.)
func manualPairingHint(hubURL string) string {
	return fmt.Sprintf("create a grant in the hub (Administration > Add machine), then run: rigscope install --role both --hub %s --token GRANT", hubURL)
}

// hubTrustsLoopbackProxy reports whether the hub accepts forwarded headers from
// a loopback peer, which is exactly what a reverse proxy on this machine looks
// like.
func hubTrustsLoopbackProxy(hub config.Hub) bool {
	loopback := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	for _, entry := range hub.TrustedProxies {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			if ip.IsLoopback() {
				return true
			}
			continue
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			continue
		}
		for _, ip := range loopback {
			if network.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func sameDestination(a, b config.Destination) bool {
	return a.URL == b.URL && a.CAFile == b.CAFile
}

// isLocalDestination reports whether a stored destination names a loopback
// endpoint. The installer uses it to recognise its own earlier automatic
// destination when the hub's port or origin changed.
func isLocalDestination(destination config.Destination) bool {
	parsed, err := url.Parse(destination.URL)
	if err != nil || parsed.Host == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// collectorInstallationID returns the id the collector will present at
// enrollment, creating the installation identity if this is its first run. The
// grant is bound to that id, so another collector id cannot spend it. In system
// scope the file is handed to the collector account through its descriptor.
func collectorInstallationID(deps Deps, paths config.Paths) (string, error) {
	store, err := openCollectorStore(deps, paths.StateDir)
	if err != nil {
		return "", fmt.Errorf("read collector installation identity: %w", err)
	}
	var prepare func(*os.File) error
	if paths.Scope == config.ScopeSystem {
		prepare = func(file *os.File) error {
			return deps.chownFile(file, systemd.CollectorUser, systemd.SharedGroup)
		}
	}
	identity, err := store.EnsureIdentity(prepare)
	if err != nil {
		return "", fmt.Errorf("prepare collector installation identity: %w", err)
	}
	return identity.CollectorID, nil
}

// collectorHasCredential reports whether the collector already holds an issued
// credential for url. Re-running install must not create another grant once
// the pairing has completed.
func collectorHasCredential(paths config.Paths, url string) (bool, error) {
	store, err := state.Open(paths.StateDir, state.Options{})
	if err != nil {
		return false, fmt.Errorf("read collector enrollment state: %w", err)
	}
	_, found, err := store.Credential(url)
	if err != nil {
		return false, fmt.Errorf("read collector credential: %w", err)
	}
	return found, nil
}

// pairingGrantHash is the SHA-256 hex digest the hub stores for a grant token.
// The plaintext token lives only in the collector's private enrollment file.
func pairingGrantHash(grant string) string {
	sum := sha256.Sum256([]byte(grant))
	return hex.EncodeToString(sum[:])
}

// pairingBootstrapPath is the hub-private file the installer stages the hashed
// automatic pairing grant in.
func pairingBootstrapPath(paths config.Paths) string {
	return filepath.Join(paths.ComponentStateDir(systemd.Hub.String()), pairingBootstrapFileName)
}

// localHubDestination builds the destination for a collector on the hub's own
// machine. A plaintext hub uses a literal loopback address, with or without a
// configured origin: the hub accepts a loopback Host on its collector
// endpoints from a direct, unproxied loopback peer, so a reverse proxy in
// front of the hub (one that answers at the origin) does not strand the local
// collector. A hub that serves TLS itself with an origin is dialed at the
// origin authority and its certificate is pinned. When neither works the
// returned notice tells the operator how to pair with a grant instead.
func localHubDestination(hub config.Hub) (config.Destination, bool, string) {
	if hub.Origin != "" && hub.TLSCert != "" {
		origin, err := url.Parse(hub.Origin)
		if err != nil || origin.Scheme != "https" || origin.Host == "" {
			return config.Destination{}, false, fmt.Sprintf(
				"automatic pairing is unavailable for hub origin %s; %s",
				hub.Origin, manualPairingHint(hub.Origin),
			)
		}
		if !filepath.IsAbs(hub.TLSCert) {
			return config.Destination{}, false, fmt.Sprintf(
				"automatic pairing is unavailable because the hub TLS certificate path is not absolute; %s",
				manualPairingHint(hub.Origin),
			)
		}
		if !certificateCoversHost(hub.TLSCert, origin.Hostname()) {
			return config.Destination{}, false, fmt.Sprintf(
				"automatic pairing is unavailable because the hub certificate does not cover origin host %s; %s",
				origin.Hostname(), manualPairingHint(hub.Origin),
			)
		}
		destination := config.Destination{
			URL:    "wss://" + origin.Host + config.CollectorConnectPath,
			CAFile: hub.TLSCert,
		}
		if err := destination.Validate(); err != nil {
			return config.Destination{}, false, ""
		}
		return destination, true, ""
	}

	if hub.TLSCert == "" {
		host, ok := loopbackListenHost(hub.Listen)
		if !ok {
			return config.Destination{}, false, ""
		}
		destination := config.Destination{URL: "ws://" + net.JoinHostPort(host, strconv.Itoa(hub.Port)) + config.CollectorConnectPath}
		if err := destination.Validate(); err != nil {
			return config.Destination{}, false, ""
		}
		return destination, true, ""
	}

	// Choose the host first, then require the certificate to cover exactly that
	// host. A certificate for localhost is now a valid destination because a
	// wss:// hostname is kept as written and used for SNI and verification.
	if !filepath.IsAbs(hub.TLSCert) {
		return config.Destination{}, false, ""
	}
	for _, host := range loopbackDestinationHosts(hub.Listen) {
		if !certificateCoversHost(hub.TLSCert, host) {
			continue
		}
		destination := config.Destination{
			URL:    "wss://" + net.JoinHostPort(host, strconv.Itoa(hub.Port)) + config.CollectorConnectPath,
			CAFile: hub.TLSCert,
		}
		if err := destination.Validate(); err != nil {
			return config.Destination{}, false, ""
		}
		return destination, true, ""
	}
	return config.Destination{}, false, ""
}

// loopbackListenHost returns a literal loopback address for a listener that
// also accepts loopback connections. A listener bound to one specific
// non-loopback address cannot be reached through loopback, so there is no
// loopback destination for it.
func loopbackListenHost(listen string) (string, bool) {
	host := strings.TrimSpace(strings.Trim(listen, "[]"))
	switch host {
	case "", "0.0.0.0", "localhost":
		return "127.0.0.1", true
	case "::":
		return "::1", true
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", false
	}
	return ip.String(), true
}

// loopbackDestinationHosts returns the hosts to try, in order, for a TLS hub
// with no configured origin. The literal loopback address of the listener is
// preferred; localhost is the fallback for a certificate that only carries a
// DNS SAN.
func loopbackDestinationHosts(listen string) []string {
	host, ok := loopbackListenHost(listen)
	if !ok {
		return nil
	}
	return []string{host, "localhost"}
}

// certificateCoversHost reports whether the hub's own certificate verifies the
// exact host the collector will dial. The certificate is pinned as the
// destination CA, so the collector verifies the local hub it was paired with.
func certificateCoversHost(path, host string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return false
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return false
		}
		return certificate.VerifyHostname(host) == nil
	}
}

// stage writes the hub's hashed bootstrap file first and then the collector's
// private enrollment grant, both through the installation manifest. The hub is
// not listening yet, so writing the hub side first means a crash can never
// leave a collector that tries to enroll against a hub that has no matching
// hash. Both files are staged and recorded before publication, so any
// interruption is recoverable by re-running install.
// rebind stores the existing credential under the new local address. It runs
// before the configuration names that address, and keeps the old entry until
// finishRebind, so an interruption at any point leaves a usable credential.
func (p *autoPairing) rebind(deps Deps, paths config.Paths) error {
	if p == nil || p.rebindFrom == "" {
		return nil
	}
	store, err := openCollectorStore(deps, paths.StateDir)
	if err != nil {
		return fmt.Errorf("read collector enrollment state: %w", err)
	}
	if err := store.AliasCredential(p.rebindFrom, p.destination.URL, collectorFileOwner(deps, paths)); err != nil {
		return fmt.Errorf("move the collector credential to the new hub address: %w", err)
	}
	return nil
}

// finishRebind drops the credential for the old address once the
// configuration names the new one.
func (p *autoPairing) finishRebind(deps Deps, paths config.Paths) error {
	if p == nil || p.rebindFrom == "" {
		return nil
	}
	store, err := openCollectorStore(deps, paths.StateDir)
	if err != nil {
		return fmt.Errorf("read collector enrollment state: %w", err)
	}
	if err := store.DropCredential(p.rebindFrom, collectorFileOwner(deps, paths)); err != nil {
		return fmt.Errorf("remove the collector credential for the old hub address: %w", err)
	}
	return nil
}

// collectorFileOwner hands a collector-private file to the collector account
// in system scope. User scope files already belong to the invoking user.
func collectorFileOwner(deps Deps, paths config.Paths) func(*os.File) error {
	if paths.Scope != config.ScopeSystem {
		return nil
	}
	return func(file *os.File) error {
		return deps.chownFile(file, systemd.CollectorUser, systemd.SharedGroup)
	}
}

func (p *autoPairing) stage(deps Deps, paths config.Paths, m *manifest) error {
	if p == nil || p.grant == "" {
		return nil
	}
	if err := writePairingBootstrap(deps, paths, m, p); err != nil {
		return err
	}
	var prepare func(*os.File) error
	if paths.Scope == config.ScopeSystem {
		prepare = func(file *os.File) error {
			return deps.chownFile(file, systemd.CollectorUser, systemd.SharedGroup)
		}
	}
	document, err := state.EnrollmentDocument(paths.StateDir, p.destination.URL, p.grant, state.WriteOptions{})
	if err != nil {
		return fmt.Errorf("encode collector enrollment grant: %w", err)
	}
	if err := publishManifestedFile(deps, paths, m, paths.EnrollmentFile(), kindEnrollmentGrant, document, 0o600, prepare); err != nil {
		return fmt.Errorf("write collector enrollment grant: %w", err)
	}
	return nil
}

// writePairingBootstrap writes the hub-owned file under the hub state
// directory with mode 0600. In system scope the hub account owns it; ownership
// is applied through the staged descriptor before publication. The manifest
// owns the path, so a foreign file at the same name is refused rather than
// replaced.
func writePairingBootstrap(deps Deps, paths config.Paths, m *manifest, p *autoPairing) error {
	dir := paths.ComponentStateDir(systemd.Hub.String())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create hub state directory %s: %w", dir, err)
	}
	payload, err := json.MarshalIndent(pairingBootstrapFile{Grants: []pairingBootstrapGrant{{
		TokenHash:   p.grantHash,
		ExpiresAt:   p.expiresAt.Format(time.RFC3339),
		Use:         storage.BootstrapGrantUseColocatedPairing,
		CollectorID: p.collectorID,
	}}}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pairing bootstrap: %w", err)
	}
	payload = append(payload, '\n')
	var prepare func(*os.File) error
	if paths.Scope == config.ScopeSystem {
		prepare = func(file *os.File) error {
			return deps.chownFile(file, systemd.HubUser, systemd.SharedGroup)
		}
	}
	return publishManifestedFile(deps, paths, m, pairingBootstrapPath(paths), kindPairingBootstrap, payload, 0o600, prepare)
}
