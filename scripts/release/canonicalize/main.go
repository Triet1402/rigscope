// Command canonicalize replaces Next.js's random per-build identifier in a
// static export with a deterministic one derived from the export's contents.
//
// `next build` generates a random build id and embeds it in every page, in the
// RSC payloads, and in the paths of two manifest directories. That makes the
// export (and therefore the binary that embeds it) differ between two builds of
// the same revision. This tool:
//
//  1. finds the id in _next/<id> and _next/static/<id>;
//  2. computes sha256 over every file (relative path and contents, with the id
//     replaced by a fixed token) and uses 16 hex characters as the new id;
//  3. rewrites the id in every UTF-8 file and renames both directories.
//
// Running it on the same export twice is a no-op. build.sh runs it between
// `npm run build:ui` and `go generate ./internal/hub/webui`.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// reserved names are directories under _next/static that are not a build id.
var reserved = map[string]bool{"chunks": true, "css": true, "media": true}

func main() {
	dir := flag.String("dir", "out", "static export directory")
	flag.Parse()
	if err := run(*dir); err != nil {
		fmt.Fprintf(os.Stderr, "canonicalize: %v\n", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	oldID, err := findBuildID(dir)
	if err != nil {
		return err
	}
	files, err := exportFiles(dir)
	if err != nil {
		return err
	}
	// Sort by the id-normalised path: the raw path contains the build id, so
	// sorting before normalisation would order two builds differently.
	sort.Slice(files, func(i, j int) bool {
		return stablePath(files[i], oldID) < stablePath(files[j], oldID)
	})
	digest := sha256.New()
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		digest.Write([]byte(stablePath(name, oldID)))
		digest.Write([]byte{0})
		digest.Write(replace(data, oldID, "RIGSCOPE_BUILD_ID"))
		digest.Write([]byte{0})
	}
	newID := hex.EncodeToString(digest.Sum(nil))[:16]
	if newID == oldID {
		fmt.Printf("canonicalize: build id %s is already canonical\n", oldID)
		return nil
	}

	for _, name := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !contains(data, oldID) {
			continue
		}
		if !utf8.Valid(data) {
			return fmt.Errorf("%s contains the build id but is not UTF-8", name)
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		replaced := replace(data, oldID, newID)
		if err := writeFile(path, replaced, info.Mode().Perm()); err != nil {
			return err
		}
	}

	for _, parent := range []string{
		filepath.Join(dir, "_next"),
		filepath.Join(dir, "_next", "static"),
	} {
		oldPath := filepath.Join(parent, oldID)
		if _, err := os.Stat(oldPath); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.Rename(oldPath, filepath.Join(parent, newID)); err != nil {
			return fmt.Errorf("rename %s: %w", oldPath, err)
		}
	}
	fmt.Printf("canonicalize: build id %s -> %s\n", oldID, newID)
	return nil
}

// findBuildID returns the random id from _next/static/<id>. The export also
// has an empty _next/<id> directory, which the rewrite renames.
func findBuildID(dir string) (string, error) {
	static := filepath.Join(dir, "_next", "static")
	entries, err := os.ReadDir(static)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", static, err)
	}
	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() || reserved[entry.Name()] {
			continue
		}
		if err := validateID(entry.Name()); err != nil {
			return "", err
		}
		ids = append(ids, entry.Name())
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("expected one build id directory in %s, found %v", static, ids)
	}
	return ids[0], nil
}

func validateID(id string) error {
	if len(id) < 8 || len(id) > 64 {
		return fmt.Errorf("build id %q has an unexpected length", id)
	}
	for _, character := range id {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-' || character == '_':
		default:
			return fmt.Errorf("build id %q contains an unsafe character", id)
		}
	}
	return nil
}

func exportFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func contains(data []byte, value string) bool {
	return strings.Contains(string(data), value)
}

func stablePath(name, buildID string) string {
	return strings.ReplaceAll(name, buildID, "RIGSCOPE_BUILD_ID")
}

func replace(data []byte, old, new string) []byte {
	return []byte(strings.ReplaceAll(string(data), old, new))
}

func writeFile(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".canonicalize-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
