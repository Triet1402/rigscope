package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// productEnviron builds the complete environment for one product process. Only
// values a program needs to run at all are inherited (an absolute-entry PATH,
// the locale, the timezone); every RIGSCOPE_*, DBUS_*, SYSTEMD_*, XDG_*, proxy,
// CA and shell-callback variable from the invoking shell is dropped. The
// fixture injects what it needs explicitly, so a developer's exported
// RIGSCOPE_HUB_PORT or RIGSCOPE_HUB_DATABASE_PATH cannot change what the
// lifecycle test proves.
func productEnviron(values map[string]string) []string {
	allowed := []string{"PATH", "LANG", "LC_ALL", "TZ"}
	out := make([]string, 0, len(allowed)+len(values))
	for _, key := range allowed {
		if _, overridden := values[key]; overridden {
			continue
		}
		value, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		if key == "PATH" {
			value = sanitizedPath(value)
		}
		out = append(out, key+"="+value)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

// sanitizedPath keeps only absolute entries so a relative or empty entry cannot
// resolve a tool from the work directory before the fake systemd binaries.
func sanitizedPath(path string) string {
	entries := strings.Split(path, string(os.PathListSeparator))
	kept := entries[:0]
	seen := map[string]bool{}
	for _, entry := range entries {
		if !filepath.IsAbs(entry) || seen[entry] {
			continue
		}
		seen[entry] = true
		kept = append(kept, entry)
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

// lookupEnv returns the value for key in a KEY=VALUE environment slice.
func lookupEnv(environ []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

// countEnv counts how many entries set key, to catch duplicate environment
// entries that could let a child read the wrong value.
func countEnv(environ []string, key string) int {
	prefix := key + "="
	count := 0
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			count++
		}
	}
	return count
}
