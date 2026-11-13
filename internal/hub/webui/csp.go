package webui

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// CSPManifestName is the per-document script-hash manifest the copy step writes
// next to the export. It is not served: export paths whose name starts with a
// dot are refused.
const CSPManifestName = ".csp.json"

// baseCSP is the export-wide policy. script-src is appended per document from
// the inline script hashes. style-src keeps 'unsafe-inline' because React sets
// style attributes for gauges and charts; scripts stay hash- or self-only.
const baseCSP = "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; " +
	"form-action 'self'; object-src 'none'; img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; connect-src 'self'"

// InlineScriptHashes returns the SHA-256 of every inline script body in an
// exported HTML document, base64-encoded without the "'sha256-" prefix. Script
// elements with a src attribute are external and contribute no hash. The copy
// step and the handler share this parser so the emitted policy always matches
// what a browser hashes.
func InlineScriptHashes(document []byte) []string {
	text := string(document)
	seen := map[string]struct{}{}
	var hashes []string
	index := 0
	for {
		start := indexFold(text, "<script", index)
		if start < 0 {
			break
		}
		tagEnd := findTagEnd(text, start+len("<script"))
		if tagEnd < 0 {
			break
		}
		closeStart := indexFold(text, "</script", tagEnd+1)
		if closeStart < 0 {
			break
		}
		attributes := text[start+len("<script") : tagEnd]
		if !scriptSrcAttribute.MatchString(attributes) {
			body := text[tagEnd+1 : closeStart]
			if body != "" {
				sum := sha256.Sum256([]byte(body))
				hash := base64.StdEncoding.EncodeToString(sum[:])
				if _, ok := seen[hash]; !ok {
					seen[hash] = struct{}{}
					hashes = append(hashes, hash)
				}
			}
		}
		index = closeStart + len("</script")
	}
	return hashes
}

// contentSecurityPolicy builds the complete header value for one document.
func contentSecurityPolicy(hashes []string) string {
	var builder strings.Builder
	builder.WriteString(baseCSP)
	builder.WriteString("; script-src 'self'")
	for _, hash := range hashes {
		builder.WriteString(" 'sha256-")
		builder.WriteString(hash)
		builder.WriteString("'")
	}
	return builder.String()
}

// loadContentSecurity reads the generated manifest, falling back to hashing
// every exported document when the manifest is absent or unreadable (a source
// checkout, or a directory export served without running the copy step).
func loadContentSecurity(fsys fs.FS) map[string][]string {
	if data, err := fs.ReadFile(fsys, CSPManifestName); err == nil {
		var manifest map[string][]string
		if err := json.Unmarshal(data, &manifest); err == nil {
			return manifest
		}
	}
	return computeContentSecurity(fsys)
}

func computeContentSecurity(fsys fs.FS) map[string][]string {
	documents := map[string][]string{}
	_ = fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry == nil || entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(name), ".html") {
			return nil
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil
		}
		documents[name] = InlineScriptHashes(data)
		return nil
	})
	return documents
}

// MarshalCSPManifest renders a script-hash manifest. The copy step builds the
// map while walking the export and writes the result as CSPManifestName.
func MarshalCSPManifest(documents map[string][]string) ([]byte, error) {
	names := make([]string, 0, len(documents))
	for name := range documents {
		names = append(names, name)
	}
	sort.Strings(names)
	data, err := json.MarshalIndent(documents, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

var scriptSrcAttribute = regexp.MustCompile(`(?i)(?:^|[\s/])src\s*=`)

func indexFold(text, needle string, from int) int {
	if from < 0 {
		from = 0
	}
	for index := from; index+len(needle) <= len(text); index++ {
		if strings.EqualFold(text[index:index+len(needle)], needle) {
			return index
		}
	}
	return -1
}

func findTagEnd(text string, from int) int {
	var quote byte
	for index := from; index < len(text); index++ {
		character := text[index]
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '"', '\'':
			quote = character
		case '>':
			return index
		}
	}
	return -1
}
