package webui

import (
	"embed"
	"fmt"
	"io/fs"
)

// The all: prefix is required. Without it Go drops `_`-prefixed paths, which
// removes out/_next/ (the entire client runtime) from the binary.
//
//go:generate go run ./cmd/webui-copy -src ../../../out -dst dist
//go:embed all:dist
var dist embed.FS

// FS returns the embedded export rooted at its own directory, so index.html is
// the root entry.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// The path is fixed at compile time; this can only fail if the
		// embed directive itself changed.
		panic(fmt.Sprintf("webui: embedded dist is unavailable: %v", err))
	}
	return sub
}

// Available reports whether a real export (an index.html document) is
// embedded. It is false for a source checkout that only has the placeholder.
func Available() bool {
	info, err := fs.Stat(FS(), "index.html")
	return err == nil && info.Mode().IsRegular()
}
