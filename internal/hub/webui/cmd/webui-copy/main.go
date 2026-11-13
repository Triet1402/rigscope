// Command webui-copy copies the Next.js static export into the webui package
// so it can be embedded with //go:embed.
//
// It is normally run by `go generate ./internal/hub/webui` from that package
// directory. The destination keeps its committed `.keep` placeholder and is
// otherwise replaced wholesale, so stale hashed assets never survive a build.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Triet1402/rigscope/internal/hub/webui"
)

func main() {
	src := flag.String("src", "../../../out", "static export directory")
	dst := flag.String("dst", "dist", "embed destination directory")
	flag.Parse()

	if err := copyExport(*src, *dst); err != nil {
		fmt.Fprintf(os.Stderr, "webui-copy: %v\n", err)
		os.Exit(1)
	}
}

func copyExport(src, dst string) error {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	info, err := os.Stat(filepath.Join(srcAbs, "index.html"))
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s does not contain an index.html; run `npm run build:ui` first", srcAbs)
	}
	if err := os.MkdirAll(dstAbs, 0o755); err != nil {
		return err
	}
	// Clear everything except the committed placeholder so a failed copy
	// cannot leave a half-old export that embeds successfully.
	entries, err := os.ReadDir(dstAbs)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".keep" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dstAbs, entry.Name())); err != nil {
			return err
		}
	}

	files := 0
	err = filepath.WalkDir(srcAbs, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dstAbs, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; the static export must not contain symlinks", path)
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		if err := copyFile(path, target, info.Mode().Perm()); err != nil {
			return err
		}
		files++
		return nil
	})
	if err != nil {
		return err
	}
	if err := writeCSPManifest(dstAbs); err != nil {
		return err
	}
	placeholder := filepath.Join(dstAbs, ".keep")
	if _, err := os.Stat(placeholder); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(placeholder, []byte("Placeholder so //go:embed all:dist compiles before the UI export is copied in.\n"), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("webui-copy: copied %d files from %s to %s\n", files, srcAbs, dstAbs)
	return nil
}

// writeCSPManifest records the SHA-256 of every inline script in every copied
// document. The webui handler serves these hashes in each page's
// Content-Security-Policy, so the export works without 'unsafe-inline'.
func writeCSPManifest(dst string) error {
	documents := map[string][]string{}
	err := filepath.WalkDir(dst, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		if !strings.HasSuffix(strings.ToLower(relative), ".html") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		documents[filepath.ToSlash(relative)] = webui.InlineScriptHashes(data)
		return nil
	})
	if err != nil {
		return err
	}
	data, err := webui.MarshalCSPManifest(documents)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dst, webui.CSPManifestName), data, 0o644); err != nil {
		return err
	}
	fmt.Printf("webui-copy: recorded inline script hashes for %d documents\n", len(documents))
	return nil
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Do not follow a symlink at the destination either.
	if info, err := os.Lstat(dst); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", dst)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
