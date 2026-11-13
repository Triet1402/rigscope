package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Triet1402/rigscope/internal/platform/fsutil"
)

// Default GitHub Releases API root.
const DefaultAPIBase = "https://api.github.com/repos/Triet1402/rigscope/releases"

// TestAPIBaseEnv is a loopback-only override of the GitHub Releases API root
// used by the end-to-end lifecycle test. A local HTTPS server stands in for
// GitHub so the test can exercise real downloads, checksum verification and
// the activation transaction without network access. The override is ignored
// unless it is an https URL on localhost or a loopback IP, so a stray value in
// a production environment cannot redirect a real upgrade. It does not relax
// the HTTPS or redirect policy; a test server still needs a trusted CA.
const TestAPIBaseEnv = "RIGSCOPE_RELEASE_BASE_URL"

// testAPIBase returns the loopback-only test override, if one is configured.
func testAPIBase(getenv func(string) string) (string, bool) {
	if getenv == nil {
		return "", false
	}
	raw := strings.TrimSpace(getenv(TestAPIBaseEnv))
	if raw == "" {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", false
		}
	}
	return strings.TrimRight(raw, "/"), true
}

// Download and extraction bounds. They are deliberately generous for the
// static binary but still small enough that a hostile server cannot exhaust
// memory or disk.
const (
	DefaultMaxMetadata     = 4 << 20
	DefaultMaxChecksums    = 1 << 20
	DefaultMaxArchive      = 128 << 20
	DefaultMaxBinary       = 64 << 20
	DefaultMaxDecompressed = 256 << 20
	DefaultMaxRawBytes     = 512 << 20
	DefaultMaxEntries      = 10_000
	DefaultVersionTimeout  = 10 * time.Second
	maxCapturedOutput      = 64 << 10
	maxRedirects           = 10
)

// versionPattern accepts a release version or a v-prefixed tag. It is used as a
// path component, so separators and traversal are rejected.
var versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)

// Options configures a Fetcher.
type Options struct {
	// APIBase is the GitHub Releases API root. Empty selects DefaultAPIBase.
	APIBase string
	// Client supplies transport, TLS and timeout settings. CheckRedirect is
	// always replaced by the fetcher's HTTPS-enforcing policy.
	Client *http.Client
	// GOARCH is the release architecture: amd64, arm64 or armv7.
	GOARCH string
	// StateRoot is the installed state directory; staging is written below it.
	StateRoot string
	// UserAgent is sent on every request.
	UserAgent string

	// AllowInsecureHTTP is a test-only transport exception. Production callers
	// must leave it false so both the initial URL and every redirect are HTTPS.
	AllowInsecureHTTP bool

	// MaxMetadata bounds the release JSON response in bytes.
	MaxMetadata int64
	// MaxChecksums bounds checksums.txt in bytes.
	MaxChecksums int64
	// MaxArchive bounds the compressed tarball in bytes.
	MaxArchive int64
	// MaxBinary bounds the extracted rigscope member in bytes.
	MaxBinary int64
	// MaxDecompressed bounds the sum of every tar entry size, including
	// entries that are skipped.
	MaxDecompressed int64
	// MaxEntries bounds the number of tar members.
	MaxEntries int64
	// MaxRawBytes bounds the raw decompressed tar stream, including headers,
	// PAX/GNU metadata and padding.
	MaxRawBytes int64

	// VersionTimeout bounds the staged binary's "version" command. Zero selects
	// DefaultVersionTimeout.
	VersionTimeout time.Duration

	// RunVersion reports the version a staged binary prints for "version". It
	// defaults to executing the binary under VersionTimeout.
	RunVersion func(ctx context.Context, binary string) (string, error)
}

// Release is one resolved GitHub release.
type Release struct {
	Version      string
	TagName      string
	ArchiveURL   string
	ChecksumsURL string
	ArchiveName  string
}

// Staged is a verified release binary written below the state root.
type Staged struct {
	Version string
	// Path is the staged executable.
	Path string
	// Dir is the version-specific staging directory.
	Dir string
}

// Fetcher resolves and stages releases.
type Fetcher struct {
	opts Options
}

// NewFetcher validates options and returns a Fetcher. Missing bounds select the
// package defaults.
func NewFetcher(opts Options) (*Fetcher, error) {
	if opts.StateRoot == "" {
		return nil, errors.New("release: state root is required")
	}
	if !filepath.IsAbs(opts.StateRoot) {
		return nil, errors.New("release: state root must be absolute")
	}
	if opts.APIBase == "" {
		opts.APIBase = DefaultAPIBase
	}
	opts.APIBase = strings.TrimRight(opts.APIBase, "/")
	// The loopback-only test override changes the default endpoint only. A
	// caller that selected its own APIBase (the unit tests do) keeps it, so
	// this can never silently redirect an explicitly configured fetcher.
	if opts.APIBase == DefaultAPIBase {
		if override, ok := testAPIBase(os.Getenv); ok {
			opts.APIBase = override
		}
	}
	if opts.MaxMetadata <= 0 {
		opts.MaxMetadata = DefaultMaxMetadata
	}
	if opts.MaxChecksums <= 0 {
		opts.MaxChecksums = DefaultMaxChecksums
	}
	if opts.MaxArchive <= 0 {
		opts.MaxArchive = DefaultMaxArchive
	}
	if opts.MaxBinary <= 0 {
		opts.MaxBinary = DefaultMaxBinary
	}
	if opts.MaxDecompressed <= 0 {
		opts.MaxDecompressed = DefaultMaxDecompressed
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if opts.MaxRawBytes <= 0 {
		opts.MaxRawBytes = DefaultMaxRawBytes
	}
	if opts.VersionTimeout <= 0 {
		opts.VersionTimeout = DefaultVersionTimeout
	}
	timeout := opts.VersionTimeout
	if opts.RunVersion == nil {
		opts.RunVersion = func(ctx context.Context, binary string) (string, error) {
			return runVersion(ctx, binary, timeout)
		}
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Fetcher{opts: opts}, nil
}

// Resolve looks up the latest release, or one exact version when version is
// non-empty. The returned version never carries the leading "v".
func (f *Fetcher) Resolve(ctx context.Context, version string) (Release, error) {
	endpoint := f.opts.APIBase + "/latest"
	if version != "" {
		tag := version
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		endpoint = f.opts.APIBase + "/tags/" + url.PathEscape(tag)
	}
	body, err := f.get(ctx, endpoint, f.opts.MaxMetadata)
	if err != nil {
		return Release{}, fmt.Errorf("look up release: %w", err)
	}
	var metadata struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return Release{}, fmt.Errorf("parse release metadata: %w", err)
	}
	if metadata.TagName == "" {
		return Release{}, errors.New("release metadata has no tag_name")
	}
	resolved := strings.TrimPrefix(metadata.TagName, "v")
	if !versionPattern.MatchString(resolved) {
		return Release{}, fmt.Errorf("release tag %q is not a safe version", metadata.TagName)
	}
	archive := fmt.Sprintf("rigscope_%s_linux_%s.tar.gz", resolved, f.opts.GOARCH)
	rel := Release{
		Version:     resolved,
		TagName:     metadata.TagName,
		ArchiveName: archive,
	}
	for _, asset := range metadata.Assets {
		switch asset.Name {
		case archive:
			rel.ArchiveURL = asset.URL
		case "checksums.txt":
			rel.ChecksumsURL = asset.URL
		}
	}
	if rel.ArchiveURL == "" {
		return Release{}, fmt.Errorf("release %s has no asset %s", metadata.TagName, archive)
	}
	if rel.ChecksumsURL == "" {
		return Release{}, fmt.Errorf("release %s has no checksums.txt", metadata.TagName)
	}
	return rel, nil
}

// Stage downloads, verifies and extracts rel, writes the binary to
// <stateRoot>/staging/<version>/rigscope, and confirms that the staged binary
// reports the expected version.
func (f *Fetcher) Stage(ctx context.Context, rel Release) (Staged, error) {
	if !versionPattern.MatchString(rel.Version) {
		return Staged{}, fmt.Errorf("release version %q is not a safe version", rel.Version)
	}
	if rel.ArchiveName == "" {
		rel.ArchiveName = fmt.Sprintf("rigscope_%s_linux_%s.tar.gz", rel.Version, f.opts.GOARCH)
	}
	sums, err := f.get(ctx, rel.ChecksumsURL, f.opts.MaxChecksums)
	if err != nil {
		return Staged{}, fmt.Errorf("download checksums: %w", err)
	}
	archive, err := f.get(ctx, rel.ArchiveURL, f.opts.MaxArchive)
	if err != nil {
		return Staged{}, fmt.Errorf("download archive: %w", err)
	}
	if err := VerifyChecksum(sums, rel.ArchiveName, archive); err != nil {
		return Staged{}, err
	}
	binary, err := ExtractBinary(archive, ExtractOptions{
		MaxBinary:       f.opts.MaxBinary,
		MaxDecompressed: f.opts.MaxDecompressed,
		MaxEntries:      f.opts.MaxEntries,
		MaxRawBytes:     f.opts.MaxRawBytes,
	})
	if err != nil {
		return Staged{}, fmt.Errorf("extract %s: %w", rel.ArchiveName, err)
	}

	dir := filepath.Join(f.opts.StateRoot, "staging", rel.Version)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return Staged{}, fmt.Errorf("create staging root: %w", err)
	}
	// Remove the previous attempt through the FD-anchored, mount-checked
	// helper: a staging directory must never delete a foreign filesystem that
	// happens to be mounted below it.
	if err := fsutil.RemoveAllPath(dir); err != nil {
		return Staged{}, fmt.Errorf("clear staging directory %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Staged{}, fmt.Errorf("create staging directory %s: %w", dir, err)
	}
	staged := filepath.Join(dir, "rigscope")
	if err := fsutil.WriteFileAtomic(staged, binary, 0o755); err != nil {
		return Staged{}, fmt.Errorf("stage binary: %w", err)
	}
	reported, err := f.opts.RunVersion(ctx, staged)
	if err != nil {
		_ = fsutil.RemoveAllPath(dir)
		return Staged{}, fmt.Errorf("staged binary reported no version: %w", err)
	}
	reported = strings.TrimPrefix(strings.TrimSpace(reported), "v")
	if reported != strings.TrimPrefix(rel.Version, "v") {
		_ = fsutil.RemoveAllPath(dir)
		return Staged{}, fmt.Errorf("staged binary reports version %q, expected %q", reported, rel.Version)
	}
	return Staged{Version: rel.Version, Path: staged, Dir: dir}, nil
}

// VerifyChecksum matches asset against a strict checksums.txt. Every non-empty
// line must have the form "<64 hex chars><space><space-or-asterisk><name>".
// Unknown names are ignored; a missing entry, duplicate conflicting entry or
// malformed line is an error.
func VerifyChecksum(sums []byte, asset string, data []byte) error {
	want := ""
	for index, raw := range strings.Split(string(sums), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		digest, name, err := parseChecksumLine(line)
		if err != nil {
			return fmt.Errorf("checksums.txt line %d: %w", index+1, err)
		}
		if name != asset {
			continue
		}
		if want != "" && want != digest {
			return fmt.Errorf("checksums.txt has conflicting entries for %s", asset)
		}
		want = digest
	}
	if want == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", asset)
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != want {
		return fmt.Errorf("checksum mismatch for %s", asset)
	}
	return nil
}

func parseChecksumLine(line string) (string, string, error) {
	if len(line) < sha256.Size*2+3 {
		return "", "", errors.New("line is too short")
	}
	digest := line[:sha256.Size*2]
	if _, err := hex.DecodeString(digest); err != nil {
		return "", "", errors.New("digest is not hexadecimal")
	}
	separator := line[sha256.Size*2]
	if separator != ' ' {
		return "", "", errors.New("digest must be followed by a space")
	}
	name := line[sha256.Size*2+1:]
	if strings.HasPrefix(name, "*") {
		name = name[1:]
	} else if strings.HasPrefix(name, " ") {
		name = name[1:]
	} else {
		return "", "", errors.New("expected two spaces or a space and an asterisk before the name")
	}
	if name == "" || strings.ContainsAny(name, " \t/\\") {
		return "", "", errors.New("file name is empty or contains a separator")
	}
	return strings.ToLower(digest), name, nil
}

// get downloads one URL with a hard byte bound. The initial URL and every
// redirect must be HTTPS unless the fetcher was built for tests.
func (f *Fetcher) get(ctx context.Context, target string, limit int64) ([]byte, error) {
	if target == "" {
		return nil, errors.New("empty download URL")
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parse URL %q: %w", target, err)
	}
	if err := f.checkScheme(parsed); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if f.opts.UserAgent != "" {
		request.Header.Set("User-Agent", f.opts.UserAgent)
	}
	response, err := f.client().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", response.StatusCode, target)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds the %d byte limit", target, limit)
	}
	return data, nil
}

// client clones the configured client so the redirect policy applies only to
// release traffic.
func (f *Fetcher) client() *http.Client {
	clone := *f.opts.Client
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if err := f.checkScheme(req.URL); err != nil {
			return err
		}
		return nil
	}
	return &clone
}

func (f *Fetcher) checkScheme(u *url.URL) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if f.opts.AllowInsecureHTTP {
			return nil
		}
		return fmt.Errorf("refusing plaintext HTTP URL %s; release downloads must use HTTPS", u.Redacted())
	default:
		return fmt.Errorf("refusing URL %s: scheme must be https", u.Redacted())
	}
}

func runVersion(ctx context.Context, binary string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultVersionTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, binary, "version")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 2 * time.Second
	stdout := &boundedWriter{limit: maxCapturedOutput}
	stderr := &boundedWriter{limit: maxCapturedOutput}
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("version command timed out after %s", timeout)
	}
	if runErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = runErr.Error()
		}
		return "", errors.New(message)
	}
	if stdout.overflow || stderr.overflow {
		return "", fmt.Errorf("version command produced more than %d bytes of output", maxCapturedOutput)
	}
	fields := strings.Fields(string(stdout.Bytes()))
	if len(fields) == 0 {
		return "", errors.New("empty version output")
	}
	return fields[len(fields)-1], nil
}

// boundedWriter keeps at most limit bytes and counts, rather than buffers,
// everything beyond the limit.
type boundedWriter struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	written := len(data)
	if w.buffer.Len() < w.limit {
		remaining := w.limit - w.buffer.Len()
		if len(data) > remaining {
			w.buffer.Write(data[:remaining])
			w.overflow = true
		} else {
			w.buffer.Write(data)
		}
	} else if len(data) > 0 {
		w.overflow = true
	}
	return written, nil
}

func (w *boundedWriter) String() string { return w.buffer.String() }

func (w *boundedWriter) Bytes() []byte { return w.buffer.Bytes() }

// checkPathMember rejects absolute paths and traversal for a tar member.
func checkPathMember(name string) (string, error) {
	if name == "" {
		return "", errors.New("empty member name")
	}
	if strings.ContainsRune(name, 0) {
		return "", errors.New("member name contains a NUL byte")
	}
	clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive entry %q escapes the archive root", name)
	}
	return clean, nil
}
