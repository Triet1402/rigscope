package activation

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Settings is the subset of the configuration readiness needs. It is a local
// type so activation does not depend on the configuration schema version. The
// JSON tags let the upgrade journal persist it for config-independent recovery.
type Settings struct {
	HubEnabled       bool   `json:"hubEnabled"`
	CollectorEnabled bool   `json:"collectorEnabled"`
	HubListen        string `json:"hubListen,omitempty"`
	HubPort          int    `json:"hubPort,omitempty"`
	HubOrigin        string `json:"hubOrigin,omitempty"`
	HubTLSCert       string `json:"hubTLSCert,omitempty"`
	HubTLSKey        string `json:"hubTLSKey,omitempty"`
}

// SettingsSource loads current settings.
type SettingsSource interface {
	Settings() (Settings, error)
}

// Readiness decides whether the activated install is healthy. It retries
// internally within a bounded window.
type Readiness interface {
	Check(ctx context.Context, expectedVersion string) error
}

// CollectorCheck is the collector-specific readiness contract: unit active plus
// "rigscope collector --check" succeeding with the persisted configuration and
// state paths.
type CollectorCheck interface {
	Ready(ctx context.Context) error
}

// CollectorCheckTimeout bounds the installed binary's --check probe.
const CollectorCheckTimeout = 10 * time.Second

// ExecCollectorOptions configures the installed binary's readiness probe.
type ExecCollectorOptions struct {
	Binary     string
	ConfigFile string
	StateDir   string
	// Scope is "user" or "system". In system scope the probe runs as the
	// collector service account when this process is root.
	Scope string
	Log   io.Writer
	// Euid, Lookup, GroupID and Run are test seams. Nil values use the real
	// system.
	Euid    func() int
	Lookup  func(name string) (uid, gid int, err error)
	GroupID func(name string) (int, error)
	Run     func(ctx context.Context, command *exec.Cmd) error
}

// ExecCollectorCheck runs the installed binary's collector --check. In system
// scope it drops to the collector service account so the probe sees exactly
// the permissions the service has; when it cannot (not root), it records a
// note and skips the privilege change rather than reporting a false failure.
type ExecCollectorCheck struct {
	opts ExecCollectorOptions
}

// NewExecCollectorCheck returns the default probe.
func NewExecCollectorCheck(opts ExecCollectorOptions) *ExecCollectorCheck {
	return &ExecCollectorCheck{opts: opts}
}

// Ready implements CollectorCheck.
func (c *ExecCollectorCheck) Ready(ctx context.Context) error {
	if c.opts.Binary == "" {
		return errors.New("collector check: no installed binary was recorded")
	}
	if c.opts.ConfigFile == "" || c.opts.StateDir == "" {
		return errors.New("collector check: configuration and state paths are required")
	}
	args := []string{"collector", "--check", "--config", c.opts.ConfigFile, "--state-dir", c.opts.StateDir}
	system := c.opts.Scope == "system"
	if system {
		args = append(args, "--system")
	}
	probeCtx, cancel := context.WithTimeout(ctx, CollectorCheckTimeout)
	defer cancel()
	command := exec.CommandContext(probeCtx, c.opts.Binary, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	stdout := &boundedOutput{limit: 64 << 10}
	stderr := &boundedOutput{limit: 64 << 10}
	command.Stdout = stdout
	command.Stderr = stderr
	if system {
		euid := os.Geteuid()
		if c.opts.Euid != nil {
			euid = c.opts.Euid()
		}
		if euid != 0 {
			if c.opts.Log != nil {
				fmt.Fprintln(c.opts.Log, "note: skipping the collector-account collector check because this process is not root")
			}
			return nil
		}
		lookup := c.opts.Lookup
		if lookup == nil {
			lookup = lookupServiceAccount
		}
		uid, _, err := lookup("rigscope-collector")
		if err != nil {
			return fmt.Errorf("collector check: resolve the collector service account: %w", err)
		}
		// The configuration is 0640 root:rigscope, so the probe needs the
		// shared group as well as the collector's own user id.
		groupID := c.opts.GroupID
		if groupID == nil {
			groupID = lookupGroupID
		}
		shared, err := groupID("rigscope")
		if err != nil {
			return fmt.Errorf("collector check: resolve the shared rigscope group: %w", err)
		}
		command.SysProcAttr.Credential = &syscall.Credential{
			Uid:    uint32(uid),
			Gid:    uint32(shared),
			Groups: []uint32{uint32(shared)},
		}
	}
	run := c.opts.Run
	if run == nil {
		run = func(ctx context.Context, command *exec.Cmd) error { return command.Run() }
	}
	if err := run(probeCtx, command); err != nil {
		if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("collector check timed out after %s", CollectorCheckTimeout)
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("collector check failed: %s", message)
	}
	return nil
}

func lookupServiceAccount(name string) (int, int, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func lookupGroupID(name string) (int, error) {
	group, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(group.Gid)
}

// HubProbe fetches a health URL. serverName is the public hostname the TLS
// certificate must be valid for. It is a seam for CLI tests that inject their
// own probe.
type HubProbe func(ctx context.Context, target, hostHeader, serverName, expectedVersion string) error

// ReadinessOptions configures ComponentReadiness.
type ReadinessOptions struct {
	Services   Services
	Settings   SettingsSource
	Binary     string
	ConfigFile string
	StateDir   string
	// Scope selects the collector service account for the probe.
	Scope     string
	Client    *http.Client
	Collector CollectorCheck
	Probe     HubProbe
	Attempts  int
	Interval  time.Duration
	Sleep     func(time.Duration)
	Log       io.Writer
}

// ComponentReadiness is the default Readiness implementation. It checks every
// enabled unit is active, asks the collector checker, and requires the hub
// health endpoint to report the activated version.
type ComponentReadiness struct {
	opts ReadinessOptions
}

// NewComponentReadiness applies defaults to opts.
func NewComponentReadiness(opts ReadinessOptions) *ComponentReadiness {
	if opts.Attempts <= 0 {
		opts.Attempts = 20
	}
	if opts.Interval <= 0 {
		opts.Interval = 500 * time.Millisecond
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if opts.Collector == nil {
		opts.Collector = NewExecCollectorCheck(ExecCollectorOptions{
			Binary:     opts.Binary,
			ConfigFile: opts.ConfigFile,
			StateDir:   opts.StateDir,
			Scope:      opts.Scope,
			Log:        opts.Log,
		})
	}
	if opts.Probe == nil {
		client := opts.Client
		opts.Probe = func(ctx context.Context, target, hostHeader, serverName, expectedVersion string) error {
			return ProbeHubHealth(ctx, client, target, hostHeader, serverName, expectedVersion)
		}
	}
	return &ComponentReadiness{opts: opts}
}

// Check implements Readiness.
func (r *ComponentReadiness) Check(ctx context.Context, expectedVersion string) error {
	var last error
	for attempt := 0; attempt < r.opts.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = r.checkOnce(ctx, expectedVersion)
		if last == nil {
			return nil
		}
		if attempt < r.opts.Attempts-1 {
			r.opts.Sleep(r.opts.Interval)
		}
	}
	return last
}

func (r *ComponentReadiness) checkOnce(ctx context.Context, expectedVersion string) error {
	settings, err := r.opts.Settings.Settings()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	var problems []string
	if settings.HubEnabled {
		running, err := r.opts.Services.UnitRunning(ctx, "rigscope-hub.service")
		if err != nil {
			problems = append(problems, fmt.Sprintf("hub unit: %v", err))
		} else if !running {
			problems = append(problems, "hub unit is not running")
		} else {
			target, host, serverName, err := HubHealthURL(settings)
			if err != nil {
				problems = append(problems, fmt.Sprintf("hub health URL: %v", err))
			} else if err := r.opts.Probe(ctx, target, host, serverName, expectedVersion); err != nil {
				problems = append(problems, fmt.Sprintf("hub health: %v", err))
			}
		}
	}
	if settings.CollectorEnabled {
		running, err := r.opts.Services.UnitRunning(ctx, "rigscope-collector.service")
		if err != nil {
			problems = append(problems, fmt.Sprintf("collector unit: %v", err))
		} else if !running {
			problems = append(problems, "collector unit is not running")
		} else if err := r.opts.Collector.Ready(ctx); err != nil {
			problems = append(problems, fmt.Sprintf("collector check: %v", err))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// HubHealthURL builds the health URL, the Host header and the TLS server name
// used for the request. The URL always connects to the configured listen
// address; the Host header is the canonical origin host when one is configured,
// so a reverse proxy on loopback still answers. The server name is the
// canonical origin hostname so a certificate for the public name verifies even
// though the connection is made to a local listen address.
func HubHealthURL(settings Settings) (string, string, string, error) {
	host := settings.HubListen
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	scheme := "http"
	if settings.HubTLSCert != "" && settings.HubTLSKey != "" {
		scheme = "https"
	}
	hostHeader := ""
	serverName := ""
	if settings.HubOrigin != "" {
		origin, err := url.Parse(settings.HubOrigin)
		if err != nil {
			return "", "", "", fmt.Errorf("parse origin: %w", err)
		}
		if origin.Host == "" {
			return "", "", "", fmt.Errorf("origin %q has no host", settings.HubOrigin)
		}
		hostHeader = origin.Host
		if scheme == "https" {
			serverName = origin.Hostname()
			if serverName == "" {
				return "", "", "", fmt.Errorf("origin %q has no hostname", settings.HubOrigin)
			}
		}
	}
	target := scheme + "://" + net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(settings.HubPort)) + "/api/health"
	return target, hostHeader, serverName, nil
}

// ProbeHubHealth fetches the hub health endpoint and requires it to report
// expectedVersion. hostHeader overrides the Host header when a canonical origin
// is configured, and serverName is the public TLS name to verify when the
// connection uses the local listen address.
func ProbeHubHealth(ctx context.Context, client *http.Client, target, hostHeader, serverName, expectedVersion string) error {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	client, err := clientForServerName(client, serverName)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if hostHeader != "" {
		request.Host = hostHeader
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", target, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return err
	}
	var health struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return fmt.Errorf("%s did not return JSON health: %w", target, err)
	}
	if health.Version == "" {
		return fmt.Errorf("%s did not report a version", target)
	}
	got := strings.TrimPrefix(strings.TrimSpace(health.Version), "v")
	want := strings.TrimPrefix(strings.TrimSpace(expectedVersion), "v")
	if got != want {
		return fmt.Errorf("%s reports version %s, expected %s", target, health.Version, expectedVersion)
	}
	return nil
}

// clientForServerName returns a client whose TLS verification uses serverName
// instead of the dialed listen address. Certificate validation stays enabled.
func clientForServerName(client *http.Client, serverName string) (*http.Client, error) {
	if serverName == "" {
		return client, nil
	}
	base, ok := client.Transport.(*http.Transport)
	if client.Transport == nil {
		base, ok = http.DefaultTransport.(*http.Transport)
	}
	if !ok || base == nil {
		return nil, errors.New("cannot verify the public TLS hostname with a non-standard HTTP transport")
	}
	clone := *client
	transport := base.Clone()
	config := &tls.Config{}
	if transport.TLSClientConfig != nil {
		config = transport.TLSClientConfig.Clone()
	}
	config.ServerName = serverName
	config.InsecureSkipVerify = false
	transport.TLSClientConfig = config
	clone.Transport = transport
	return &clone, nil
}
