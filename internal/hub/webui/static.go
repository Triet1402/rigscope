package webui

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	immutableCache = "public, max-age=31536000, immutable"
	documentCache  = "no-cache"
	maxGzipBytes   = 1024
)

var contentTypes = map[string]string{
	".avif":        "image/avif",
	".css":         "text/css; charset=utf-8",
	".gif":         "image/gif",
	".htm":         "text/html; charset=utf-8",
	".html":        "text/html; charset=utf-8",
	".ico":         "image/x-icon",
	".jpeg":        "image/jpeg",
	".jpg":         "image/jpeg",
	".js":          "text/javascript; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".otf":         "font/otf",
	".png":         "image/png",
	".svg":         "image/svg+xml; charset=utf-8",
	".ttf":         "font/ttf",
	".txt":         "text/plain; charset=utf-8",
	".wasm":        "application/wasm",
	".webmanifest": "application/manifest+json; charset=utf-8",
	".webp":        "image/webp",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".xml":         "application/xml; charset=utf-8",
}

var compressible = regexp.MustCompile(`^(?:text/|application/(?:json|javascript|manifest\+json|xml)|image/svg\+xml)`)

// Legacy overview URL shape, kept only for redirecting old links.
var (
	legacyMachinePath = regexp.MustCompile(`^/machines/([^/]+)/?$`)
)

// Handler serves one static export. It never authenticates and never serves a
// request that has not already passed the hub's security middleware. HTML
// documents carry the export's Content-Security-Policy, including the SHA-256
// hashes of their inline scripts.
type Handler struct {
	fsys fs.FS             // embed or in-memory export when root is nil
	root *os.Root          // directory export, opened once; nil for an fs.FS export
	csp  map[string]string // export file name -> complete CSP header value
}

type fileRef struct {
	name string
	info fs.FileInfo
	file *os.File // already-open handle for a directory export
}

func (r *fileRef) close() {
	if r != nil && r.file != nil {
		_ = r.file.Close()
	}
}

// New validates an fs.FS export rooted at the export directory. index.html is
// required so an unrelated or empty filesystem cannot masquerade as the UI.
func New(fsys fs.FS) (*Handler, error) {
	if fsys == nil {
		return nil, errors.New("webui: filesystem is nil")
	}
	h := &Handler{fsys: fsys}
	h.loadCSP(loadContentSecurity(fsys))
	ref, err := h.findFile("/index.html")
	if err != nil || ref == nil {
		return nil, errors.New("webui: UI export is missing an index.html")
	}
	ref.close()
	return h, nil
}

// NewDir serves an export from disk. The root is opened with os.Root, so a
// symlink or path component can never resolve outside it. Every component is
// still checked so the export cannot contain symlinks at all. Callers should
// Close the handler when they are done with it.
func NewDir(rootPath string) (*Handler, error) {
	real, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return nil, fmt.Errorf("webui: resolve export root %s: %w", rootPath, err)
	}
	root, err := os.OpenRoot(real)
	if err != nil {
		return nil, fmt.Errorf("webui: open export root %s: %w", real, err)
	}
	info, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("webui: stat export root %s: %w", real, err)
	}
	if !info.IsDir() {
		_ = root.Close()
		return nil, fmt.Errorf("webui: export root %s is not a directory", real)
	}
	h := &Handler{fsys: root.FS(), root: root}
	h.loadCSP(loadContentSecurity(h.fsys))
	ref, err := h.findFile("/index.html")
	if err != nil || ref == nil {
		_ = root.Close()
		return nil, fmt.Errorf("webui: UI export is missing an index.html in %s", real)
	}
	ref.close()
	return h, nil
}

// Close releases the directory handle held by a NewDir handler.
func (h *Handler) Close() error {
	if h.root == nil {
		return nil
	}
	err := h.root.Close()
	h.root = nil
	return err
}

func (h *Handler) loadCSP(documents map[string][]string) {
	h.csp = make(map[string]string, len(documents))
	for name, hashes := range documents {
		h.csp[name] = contentSecurityPolicy(hashes)
	}
}

func (h *Handler) contentSecurityPolicy(name string) string {
	if policy, ok := h.csp[name]; ok && policy != "" {
		return policy
	}
	return contentSecurityPolicy(nil)
}

// Handle is an alias for ServeHTTP so callers can use *Handler as a
// http.Handler without an adapter.
func (h *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	h.ServeHTTP(w, r)
}

// ServeHTTP implements the export route table described in the package
// documentation.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	if method != http.MethodGet && method != http.MethodHead {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Cache-Control", documentCache)
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, "method not allowed")
		return
	}

	rawPath := requestPath(r)
	pathname, err := decodePathname(rawPath)
	if err != nil || r.URL == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", documentCache)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad request")
		return
	}

	if strings.HasPrefix(pathname, "/_next/") {
		ref, _ := h.findFile(pathname)
		if ref == nil {
			h.notFound(w, r, false)
			return
		}
		cache := documentCache
		if strings.HasPrefix(pathname, "/_next/static/") {
			cache = immutableCache
		}
		h.sendFile(w, r, ref, http.StatusOK, cache)
		return
	}

	// Old bookmarks and links: /machines/<id> → /machines?machineId=<id>.
	// Never redirect a framework data probe.
	if !isNextDataRequest(r) && h.redirectLegacyMachine(w, r, rawPath) {
		return
	}

	for _, candidate := range documentCandidates(pathname) {
		ref, _ := h.findFile(candidate)
		if ref != nil {
			h.sendFile(w, r, ref, http.StatusOK, documentCache)
			return
		}
	}
	h.notFound(w, r, true)
}

// requestPath returns the undecoded request path exactly as routed. net/http
// keeps percent-encoding in RawPath, which matters for the legacy
// /machines/<id> redirect: an id containing %2F must stay one path segment.
func requestPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "/"
	}
	if escaped := r.URL.EscapedPath(); escaped != "" {
		return escaped
	}
	return "/"
}

// decodePathname decodes the path exactly once and rejects anything that
// cannot be a plain export path. A second decode would let an encoded
// traversal sequence through.
func decodePathname(raw string) (string, error) {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", err
	}
	if decoded == "" {
		decoded = "/"
	}
	if strings.ContainsRune(decoded, 0) || strings.Contains(decoded, `\`) {
		return "", errors.New("invalid path")
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("invalid path")
		}
	}
	return decoded, nil
}

// isNextDataRequest reports Next's client-router probes. Those must never be
// redirected or answered with an HTML document.
func isNextDataRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if len(r.Header.Values("RSC")) > 0 {
		return true
	}
	if len(r.Header.Values("Next-Router-State-Tree")) > 0 {
		return true
	}
	if strings.Contains(r.Header.Get("Accept"), "text/x-component") {
		return true
	}
	if r.URL != nil {
		if _, ok := r.URL.Query()["_rsc"]; ok {
			return true
		}
	}
	return false
}

func documentCandidates(pathname string) []string {
	if pathname == "/" {
		return []string{"/index.html"}
	}
	if strings.HasSuffix(pathname, "/") {
		withoutSlash := strings.TrimSuffix(pathname, "/")
		return []string{pathname + "index.html", withoutSlash + ".html"}
	}
	return []string{pathname, pathname + ".html"}
}

// findFile resolves one export path to a regular file inside the export root,
// or nil. Every path component is checked for a symlink, and dotfiles (such as
// the CSP manifest) are never served.
func (h *Handler) findFile(name string) (*fileRef, error) {
	if !strings.HasPrefix(name, "/") || name == "/" {
		return nil, nil
	}
	relative := strings.TrimPrefix(name, "/")
	if relative == "" {
		return nil, nil
	}
	segments := strings.Split(relative, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." ||
			strings.HasPrefix(segment, ".") || strings.Contains(segment, `\`) {
			return nil, nil
		}
	}
	if h.root != nil {
		return h.findDiskFile(segments)
	}
	return h.findFSFile(segments)
}

func (h *Handler) findFSFile(segments []string) (*fileRef, error) {
	prefix := ""
	var info fs.FileInfo
	for index, segment := range segments {
		if prefix == "" {
			prefix = segment
		} else {
			prefix += "/" + segment
		}
		current, err := fs.Stat(h.fsys, prefix)
		if err != nil {
			return nil, nil
		}
		if current.Mode()&fs.ModeSymlink != 0 {
			return nil, nil
		}
		if index < len(segments)-1 {
			if !current.IsDir() {
				return nil, nil
			}
			continue
		}
		if !current.Mode().IsRegular() {
			return nil, nil
		}
		info = current
	}
	if info == nil {
		return nil, nil
	}
	return &fileRef{name: strings.Join(segments, "/"), info: info}, nil
}

// findDiskFile opens a candidate through the os.Root handle, then stats the
// opened handle and serves from it. There is no window between the check and
// the open in which a path component could be swapped for a symlink that
// escapes the export.
func (h *Handler) findDiskFile(segments []string) (*fileRef, error) {
	// Root already refuses escapes; this keeps the stricter export rule that a
	// symlink is never served, even when it points inside the root.
	for index := range segments {
		info, err := h.root.Lstat(strings.Join(segments[:index+1], "/"))
		if err != nil {
			return nil, nil
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, nil
		}
		if index < len(segments)-1 && !info.IsDir() {
			return nil, nil
		}
	}
	name := strings.Join(segments, "/")
	file, err := h.root.Open(name)
	if err != nil {
		return nil, nil
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil
	}
	return &fileRef{name: name, info: info, file: file}, nil
}

func (h *Handler) open(ref *fileRef) (fs.File, error) {
	if ref.file != nil {
		return ref.file, nil
	}
	return h.fsys.Open(ref.name)
}

func (h *Handler) contentHeaders(ref *fileRef, cache string) map[string]string {
	contentType := contentTypes[strings.ToLower(path.Ext(ref.name))]
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	headers := map[string]string{
		"Content-Type":           contentType,
		"Content-Length":         strconv.FormatInt(ref.info.Size(), 10),
		"Cache-Control":          cache,
		"X-Content-Type-Options": "nosniff",
	}
	if strings.HasPrefix(contentType, "text/html") {
		headers["Content-Security-Policy"] = h.contentSecurityPolicy(ref.name)
	}
	if compressible.MatchString(contentType) {
		// Mark both variants so an intermediary cache cannot hand a gzip
		// body to a client that did not ask for one.
		headers["Vary"] = "Accept-Encoding"
	}
	return headers
}

func (h *Handler) sendFile(w http.ResponseWriter, r *http.Request, ref *fileRef, status int, cache string) {
	defer ref.close()
	headers := h.contentHeaders(ref, cache)
	gzipPossible := compressible.MatchString(headers["Content-Type"]) && ref.info.Size() >= maxGzipBytes
	encodingValue, encodingPresent := headerValue(r, "Accept-Encoding")
	representation := chooseRepresentation(encodingValue, encodingPresent, gzipPossible)
	if representation == "" {
		if status == http.StatusOK {
			notAcceptable(w, r)
			return
		}
		representation = "identity"
	}
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	if representation == "gzip" {
		w.Header().Set("Content-Encoding", "gzip")
		// The compressed length is unknown until the stream ends; HEAD must
		// not advertise the identity length next to a gzip Content-Encoding.
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if representation == "gzip" {
		// Flush the headers before the compressed body so net/http cannot
		// infer a Content-Length for a small body: a compressed response must
		// not advertise the identity length.
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	file, err := h.open(ref)
	if err != nil {
		return
	}
	if ref.file == nil {
		defer file.Close()
	}
	if representation == "gzip" {
		compressor := gzip.NewWriter(w)
		if _, err := io.Copy(compressor, file); err != nil {
			_ = compressor.Close()
			return
		}
		_ = compressor.Close()
		return
	}
	_, _ = io.Copy(w, file)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request, html bool) {
	if html {
		if page, _ := h.findFile("/404.html"); page != nil {
			h.sendFile(w, r, page, http.StatusNotFound, documentCache)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", documentCache)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "not found")
	}
}

func notAcceptable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Accept-Encoding")
	w.WriteHeader(http.StatusNotAcceptable)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "not acceptable")
	}
}

// redirectLegacyMachine rewrites /machines/<id> to the exported page's query
// form, preserving every other parameter.
func (h *Handler) redirectLegacyMachine(w http.ResponseWriter, r *http.Request, rawPath string) bool {
	match := legacyMachinePath.FindStringSubmatch(rawPath)
	if match == nil {
		return false
	}
	id, err := url.PathUnescape(match[1])
	if err != nil {
		return false
	}
	if id == "" || id == "." || id == ".." || strings.Contains(id, `\`) || strings.ContainsRune(id, 0) {
		return false
	}
	// An RSC probe (<id>.txt) is a framework data request, not a machine URL.
	if strings.HasSuffix(id, ".txt") {
		return false
	}
	query := setQueryParam(r.URL.RawQuery, "machineId", url.QueryEscape(id))
	location := "/machines"
	if query != "" {
		location += "?" + query
	}
	w.Header().Set("Location", location)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusPermanentRedirect)
	return true
}

// setQueryParam replaces the first matching parameter in place or appends it,
// preserving the order and raw spelling of all other parameters (Go's
// url.Values.Encode sorts keys, which would rewrite the legacy redirect).
func setQueryParam(rawQuery, key, encodedValue string) string {
	if rawQuery == "" {
		return key + "=" + encodedValue
	}
	parts := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(parts)+1)
	found := false
	for _, part := range parts {
		rawKey := part
		if equals := strings.IndexByte(part, '='); equals >= 0 {
			rawKey = part[:equals]
		}
		decodedKey, err := url.QueryUnescape(rawKey)
		if err != nil || decodedKey != key {
			kept = append(kept, part)
			continue
		}
		if found {
			// URLSearchParams.set keeps the first occurrence and drops the
			// rest.
			continue
		}
		kept = append(kept, key+"="+encodedValue)
		found = true
	}
	if !found {
		kept = append(kept, key+"="+encodedValue)
	}
	return strings.Join(kept, "&")
}

func headerValue(r *http.Request, name string) (string, bool) {
	if r == nil {
		return "", false
	}
	values := r.Header.Values(name)
	if len(values) == 0 {
		return "", false
	}
	return strings.Join(values, ","), true
}

// qualityOf returns an explicit coding quality, then `*`, then the default.
func qualityOf(qualities map[string]float64, coding string, fallback float64) float64 {
	if qualities == nil {
		return fallback
	}
	if value, ok := qualities[coding]; ok {
		return value
	}
	if value, ok := qualities["*"]; ok {
		return value
	}
	return fallback
}

// acceptEncodingQualities parses Accept-Encoding into coding → quality.
// Duplicate codings keep the lowest quality (conservative); an invalid q is
// treated as 0 rather than guessed.
func acceptEncodingQualities(header string, present bool) map[string]float64 {
	if !present {
		return nil
	}
	qualities := map[string]float64{}
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		if name == "" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			key := parameter
			value := ""
			if equals := strings.IndexByte(parameter, '='); equals >= 0 {
				key = parameter[:equals]
				value = parameter[equals+1:]
			}
			if strings.ToLower(strings.TrimSpace(key)) != "q" {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || math.IsNaN(parsed) || parsed < 0 || parsed > 1 {
				quality = 0
			} else {
				quality = parsed
			}
			break
		}
		if previous, ok := qualities[name]; !ok || quality < previous {
			qualities[name] = quality
		}
	}
	return qualities
}

// chooseRepresentation returns "gzip", "identity", or "" for 406.
func chooseRepresentation(header string, present, gzipPossible bool) string {
	qualities := acceptEncodingQualities(header, present)
	// identity is acceptable by default; gzip is only acceptable when asked.
	gzipQuality := qualityOf(qualities, "gzip", 0)
	identityQuality := qualityOf(qualities, "identity", 1)
	if gzipPossible && gzipQuality > 0 && gzipQuality >= identityQuality {
		return "gzip"
	}
	if identityQuality > 0 {
		return "identity"
	}
	if gzipPossible && gzipQuality > 0 {
		return "gzip"
	}
	return ""
}
