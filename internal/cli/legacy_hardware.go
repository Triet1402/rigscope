package cli

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// These are the default 0.2 templates, preserved after removal of the Node
// runtime. A marker alone never authorizes deleting arbitrary root-owned text.
//
//go:embed legacy-hardware/*
var legacyHardwareTemplates embed.FS

type legacyHardwareFile struct {
	path string
	dir  *fsutil.Dir
	id   fsutil.Identity
	data []byte
}

func runCleanupLegacyHardware(ctx context.Context, deps Deps, args []string) int {
	flags := newFlagSet("cleanup-legacy-hardware", deps.Stderr)
	apply := flags.Bool("apply", false, "remove recognized leftover 0.2 hardware-access files (requires root)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return deps.errorf("cleanup-legacy-hardware takes no positional arguments")
	}
	if *apply {
		if code, done := deps.ensureRoot(config.ScopeSystem); done {
			return code
		}
		if deps.RunCommand == nil {
			return deps.errorf("udev command runner is unavailable; nothing was removed")
		}
	}
	var files []legacyHardwareFile
	defer func() {
		for _, file := range files {
			_ = file.dir.Close()
		}
	}()
	// Inspect every candidate before deleting anything. Custom groups, edited
	// files and 1.x templates require an operator's inspection instead.
	for _, parts := range [][]string{
		{"etc", "udev", "rules.d", "90-rigscope-rapl.rules"},
		{"etc", "udev", "rules.d", "90-rigscope-gpu-temps.rules"},
		{"etc", "sysusers.d", "rigscope-rapl.conf"},
	} {
		path := deps.hostPath(parts...)
		dir, err := fsutil.OpenDirNoFollow(filepath.Dir(path))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return deps.errorf("inspect %s: %v; nothing was removed", path, err)
		}
		data, id, err := readLegacyHardwareFile(dir, filepath.Base(path))
		if errors.Is(err, os.ErrNotExist) {
			_ = dir.Close()
			continue
		}
		if err != nil {
			_ = dir.Close()
			return deps.errorf("inspect %s: %v; nothing was removed", path, err)
		}
		if systemd.IsManaged(string(data)) {
			// A Rigscope 1.x file (the 0.2 marker is "Managed by Rigscope").
			// It belongs to the current installation, not to 0.2.
			_ = dir.Close()
			fmt.Fprintf(deps.Stdout, "  kept: %s %s\n", path, deps.out(styleDim, "(Rigscope 1.x file; rigscope uninstall removes it)"))
			continue
		}
		want, err := legacyHardwareTemplates.ReadFile("legacy-hardware/" + filepath.Base(path))
		if err != nil || !bytes.Equal(bytes.TrimSpace(data), bytes.TrimSpace(want)) {
			_ = dir.Close()
			return deps.errorf("refusing %s: not an unchanged default 0.2 hardware-access file; nothing was removed", path)
		}
		files = append(files, legacyHardwareFile{path: path, dir: dir, id: id, data: data})
		fmt.Fprintf(deps.Stdout, "  legacy hardware file: %s\n", path)
	}
	if len(files) == 0 && !*apply {
		fmt.Fprintln(deps.Stdout, "No leftover 0.2 hardware-access files found; nothing to clean up.")
		return 0
	}
	if !*apply {
		fmt.Fprintln(deps.Stdout, "Preview only; no files changed. After uninstalling old Rigscope installations, run as root: rigscope cleanup-legacy-hardware --apply")
		return 0
	}
	if err := checkLegacyHardwareUnused(deps); err != nil {
		return deps.errorf("refusing hardware cleanup: %v; nothing was removed", err)
	}
	// Recheck the pinned directory and each file before the first deletion.
	for _, file := range files {
		parent, err := fsutil.IdentityOfPath(filepath.Dir(file.path))
		if err != nil {
			return deps.errorf("recheck %s: %v", file.path, err)
		}
		pinned, err := file.dir.Identity()
		if err != nil || !pinned.SameFile(parent) {
			return deps.errorf("parent changed for %s; nothing was removed", file.path)
		}
		data, id, err := readLegacyHardwareFile(file.dir, filepath.Base(file.path))
		if err != nil || id != file.id || !bytes.Equal(data, file.data) {
			return deps.errorf("file changed during inspection: %s; nothing was removed", file.path)
		}
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return deps.errorf("cleanup interrupted: %v; rerun cleanup to finish", err)
		}
		if err := file.dir.Remove(filepath.Base(file.path)); err != nil {
			return deps.errorf("remove %s: %v; rerun cleanup to finish", file.path, err)
		}
		fmt.Fprintf(deps.Stdout, "  removed: %s\n", file.path)
	}
	// Reload even with no files: the previous attempt may have removed the
	// files but failed at this step. No state directory is needed for retry.
	if err := deps.RunCommand(ctx, "udevadm", "control", "--reload"); err != nil {
		return deps.errorf("legacy files removed, but udev reload failed: %v; rerun cleanup to finish", err)
	}
	fmt.Fprintln(deps.Stdout, "Legacy hardware cleanup complete. Existing device permissions return to kernel defaults on reboot; accounts, groups and application data were not removed.")
	return 0
}

func readLegacyHardwareFile(dir *fsutil.Dir, name string) ([]byte, fsutil.Identity, error) {
	id, err := dir.Stat(name)
	if err != nil {
		return nil, id, err
	}
	// Refuse special files before open(2), which could block on a FIFO.
	if id.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil, id, fmt.Errorf("%s must be a regular file", name)
	}
	return dir.ReadRegular(name, 16*1024, false)
}

func checkLegacyHardwareUnused(deps Deps) error {
	patterns := []string{
		deps.hostPath("etc", "systemd", "system", "rigscope*.service"),
		deps.hostPath("usr", "lib", "systemd", "system", "rigscope*.service"),
		deps.hostPath("home", "*", ".config", "systemd", "user", "rigscope*.service"),
		deps.hostPath("root", ".config", "systemd", "user", "rigscope*.service"),
		deps.hostPath("var", "lib", "rigscope", "install-manifest.json"),
		deps.hostPath("home", "*", ".local", "state", "rigscope", "install-manifest.json"),
		deps.hostPath("root", ".local", "state", "rigscope", "install-manifest.json"),
	}
	userPaths, err := deps.paths(config.ScopeUser)
	if err != nil {
		return err
	}
	patterns = append(patterns, filepath.Join(userPaths.UnitDir, "rigscope*.service"), userPaths.ManifestFile())
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		for _, path := range matches {
			name := filepath.Base(path)
			if name == "install-manifest.json" || name == "rigscope.service" || strings.Contains(name, "hub") || strings.Contains(name, "collector") {
				return fmt.Errorf("installation still present at %s; uninstall it first", path)
			}
		}
	}
	processes, err := os.ReadDir(deps.hostPath("proc"))
	if err != nil {
		return fmt.Errorf("cannot inspect running processes: %w", err)
	}
	for _, process := range processes {
		pid, err := strconv.Atoi(process.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		data, err := os.ReadFile(deps.hostPath("proc", process.Name(), "cmdline"))
		if errors.Is(err, os.ErrNotExist) {
			continue // exited during inspection
		}
		if err != nil {
			return fmt.Errorf("cannot inspect process %d: %w", pid, err)
		}
		args := strings.Split(string(data), "\x00")
		if len(args) == 0 {
			continue
		}
		// Match the Go CLI or a Node process running the 0.2 worker. Do not
		// inspect shell command text, which may merely mention Rigscope.
		base := filepath.Base(args[0])
		if !strings.Contains(base, "rigscope") && !strings.HasPrefix(base, "node") {
			continue
		}
		for _, arg := range args[1:] {
			goWorker := strings.Contains(base, "rigscope") && (arg == "hub" || arg == "collector")
			workerFile := filepath.Base(arg)
			nodeWorker := strings.Contains(arg, "rigscope") && (strings.Contains(arg, "/hub/") || strings.Contains(arg, "/collector/") || workerFile == "hub.cjs" || workerFile == "collector.cjs")
			if goWorker || nodeWorker {
				return fmt.Errorf("Rigscope process %d is still running; stop and uninstall it first", pid)
			}
		}
	}
	return nil
}

// isDefaultLegacyUdev recognizes content, not a marker or filename alone. It
// opens the parent and leaf without following symlinks, just like cleanup.
func isDefaultLegacyUdev(path, kind string) bool {
	if kind != kindUdevRAPL && kind != kindUdevGPU {
		return false
	}
	want, err := legacyHardwareTemplates.ReadFile("legacy-hardware/" + filepath.Base(path))
	if err != nil {
		return false
	}
	dir, err := fsutil.OpenDirNoFollow(filepath.Dir(path))
	if err != nil {
		return false
	}
	defer dir.Close()
	data, _, err := readLegacyHardwareFile(dir, filepath.Base(path))
	return err == nil && bytes.Equal(bytes.TrimSpace(data), bytes.TrimSpace(want))
}

// previewLegacyRules describes replacement only for rules this installation
// needs. Unused rules are left alone and do not become owned by the new install.
func previewLegacyRules(deps Deps, paths config.Paths, wanted *manifest) bool {
	replaces := false
	for _, file := range []struct{ path, kind string }{{paths.UdevRAPLFile, kindUdevRAPL}, {paths.UdevGPUTempsFile, kindUdevGPU}} {
		if !isDefaultLegacyUdev(file.path, file.kind) {
			continue
		}
		if wanted.entry(file.path, file.kind) != nil {
			fmt.Fprintf(deps.Stdout, "  replace recognized default 0.2 rule: %s (record ownership for this installation)\n", file.path)
			replaces = true
		} else {
			fmt.Fprintf(deps.Stdout, "  keep unused default 0.2 rule: %s (not adopted by this installation)\n", file.path)
		}
	}
	return replaces
}
