package gpu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// PCI register reads run in a short-lived copy of this binary:
//
//	rigscope internal pci-read
//
// The parent writes one JSON request on the child's stdin and reads one JSON
// array of words back. Mapping a device BAR can hang the CPU (a wedged PCIe
// device does not necessarily complete a read) or fault with SIGBUS; doing
// that in the collector process can stall sampling or kill it outright.
// Containing the work in a child means the parent can always kill it, and a
// fault only takes down the helper.
//
// A nil array element means that offset was outside the BAR or the page could
// not be mapped; it never means "zero degrees".

const (
	// HelperCommand is the hidden CLI verb of the pci-read helper. It is wired
	// up by cmd/rigscope and deliberately absent from the user-facing help.
	HelperCommand = "pci-read"

	// HelperExecutableArgs are the arguments the parent passes to itself. They
	// must match the dispatch in cmd/rigscope/main.go.
	helperInternalArg = "internal"

	// helperReadTimeout bounds one helper run. It is the prototype python
	// helper's budget, and the hard deadline a wedged device cannot escape.
	helperReadTimeout = 1500 * time.Millisecond
	// helperWaitDelay additionally bounds how long the parent waits for
	// inherited pipes (a stuck grandchild) once the process is gone.
	helperWaitDelay = 250 * time.Millisecond

	// helperRequestLimit and helperResponseLimit cap the protocol payloads.
	// The largest legitimate response is 512 words (~7 KiB).
	helperRequestLimit  = 16 * 1024
	helperResponseLimit = 16 * 1024
	helperStderrLimit   = 4 * 1024
)

// sysfsPCIResourcePrefix is the only request path production accepts: the
// kernel's symlink view of a PCI function's BAR0.
const sysfsPCIResourcePrefix = "/sys/bus/pci/devices/"

// resolvedSysfsDeviceRoot is where that symlink must resolve to.
const resolvedSysfsDeviceRoot = "/sys/devices/"

// helperChildRoot and helperLaunchEnv are test-only hooks. Production leaves
// both nil: the helper always enforces the host /sys/devices boundary and the
// launch command has no fixture environment. Tests install the hooks from
// export_test.go, so no production code path can be activated by an
// environment variable or a request field.
var (
	helperChildRoot func() string
	helperLaunchEnv func(resourceRoot string) []string
)

// helperFixtureRoot returns the fixture root a test build installed for the
// helper child. Production always reads the host sysfs boundary.
func helperFixtureRoot() string {
	if helperChildRoot == nil {
		return ""
	}
	return helperChildRoot()
}

// pciReadRequest is the helper's stdin document.
type pciReadRequest struct {
	Path    string  `json:"path"`
	Offsets []int64 `json:"offsets"`
}

// resourceBDF extracts the canonical bus/device/function from a request path
// of the form .../devices/<bdf>/resource0. It rejects a non-canonical BDF and
// a path that is not clean and absolute.
func resourceBDF(resourcePath string) (string, bool) {
	if strings.ContainsRune(resourcePath, 0) || !filepath.IsAbs(resourcePath) ||
		filepath.Clean(resourcePath) != resourcePath {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(resourcePath, "/"), "/")
	if len(parts) < 4 {
		return "", false
	}
	name := parts[len(parts)-1]
	bdf := parts[len(parts)-2]
	parent := parts[len(parts)-3]
	if name != "resource0" || parent != "devices" {
		return "", false
	}
	canonical, ok := CanonicalPCIAddress(bdf)
	if !ok || canonical != bdf {
		return "", false
	}
	return bdf, true
}

// withinRoot reports whether name lives under root. The root itself is never
// treated as a file name.
func withinRoot(name, root string) bool {
	root = filepath.Clean(root)
	if root == string(filepath.Separator) {
		return filepath.IsAbs(name)
	}
	return strings.HasPrefix(name, root+string(filepath.Separator))
}

// resolveResourcePath resolves the request's symlinked sysfs path and enforces
// that it really is a PCI function's resource0. root is the host's
// "/sys/devices" boundary in production (empty string) or a test-only fixture
// root, which must still contain the resolved path.
func resolveResourcePath(resourcePath, root string) (string, error) {
	bdf, ok := resourceBDF(resourcePath)
	if !ok {
		return "", fmt.Errorf("gpu: pci-read path %q is not a /sys/bus/pci/devices/<bdf>/resource0 path",
			resourcePath)
	}
	real, err := filepath.EvalSymlinks(resourcePath)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(real, string(filepath.Separator)+bdf+string(filepath.Separator)+"resource0") {
		return "", fmt.Errorf("gpu: pci-read path %q resolves to %q, not a BAR0 of %s",
			resourcePath, real, bdf)
	}
	if root == "" {
		if !strings.HasPrefix(real, resolvedSysfsDeviceRoot) {
			return "", fmt.Errorf("gpu: pci-read path %q resolves outside %s",
				resourcePath, resolvedSysfsDeviceRoot)
		}
		return real, nil
	}
	if !withinRoot(real, root) {
		return "", fmt.Errorf("gpu: pci-read path %q resolves outside the fixture root", resourcePath)
	}
	return real, nil
}

// validatePCIReadRequest rejects anything that is not a BAR0 resource of a
// single PCI function with a bounded, aligned offset list. Both the parent and
// the child validate: the child trusts nothing it reads from stdin.
func validatePCIReadRequest(request pciReadRequest) error {
	if len(request.Offsets) == 0 {
		return errors.New("gpu: pci-read request has no offsets")
	}
	if len(request.Offsets) > maxRegisterOffsets {
		return fmt.Errorf("gpu: pci-read request has %d offsets, limit is %d",
			len(request.Offsets), maxRegisterOffsets)
	}
	for _, offset := range request.Offsets {
		if offset < 0 || offset%4 != 0 || offset > maxRegisterOffset {
			return fmt.Errorf("gpu: pci-read offset 0x%x is not an aligned offset below 0x%x",
				offset, maxRegisterOffset)
		}
	}
	if _, ok := resourceBDF(request.Path); !ok {
		return fmt.Errorf("gpu: pci-read path %q is not a clean /sys/bus/pci/devices/<bdf>/resource0 path",
			request.Path)
	}
	return nil
}

// validateResourceRequest rejects a path production must never hand to the
// helper. The request reaches the child too, which repeats the resolution and
// opens the real path with O_NOFOLLOW; this check stops an arbitrary-root path
// from ever spawning a child.
func (h HelperSource) validateResourceRequest(resourcePath string) error {
	bdf, ok := resourceBDF(resourcePath)
	if !ok {
		return fmt.Errorf("gpu: pci-read path %q is not a /sys/bus/pci/devices/<bdf>/resource0 path",
			resourcePath)
	}
	if h.resourceRoot != "" {
		if !withinRoot(resourcePath, h.resourceRoot) {
			return fmt.Errorf("gpu: pci-read path %q is outside the fixture root", resourcePath)
		}
		return nil
	}
	if !strings.HasPrefix(resourcePath, sysfsPCIResourcePrefix) ||
		strings.TrimPrefix(resourcePath, sysfsPCIResourcePrefix) != bdf+"/resource0" {
		return fmt.Errorf("gpu: pci-read path %q is outside %s", resourcePath, sysfsPCIResourcePrefix)
	}
	return nil
}

// helperSlots keeps the parent to one helper at a time. Register reads are
// already serialized by the temperature cycle; the slot keeps a second caller
// (or a stuck child the kernel has not reaped yet) from piling up processes.
var helperSlots = &reapSlot{}

// HelperSource reads PCI registers through the self-exec helper.
//
// It implements [RegisterSource]. Every failure — no executable, a missing or
// invalid path, a timeout, a crash, malformed or oversized output — comes back
// as all-nil readings plus an error, so a register temperature is simply
// absent and the collector keeps running.
type HelperSource struct {
	// Executable resolves the binary to re-exec, [os.Executable] by default.
	Executable func() (string, error)
	// Timeout bounds one helper run, [helperReadTimeout] by default.
	Timeout time.Duration
	// WaitDelay bounds the wait for inherited pipes after the kill,
	// [helperWaitDelay] by default.
	WaitDelay time.Duration
	// Access is the per-device arbiter. The lease is released by the owned
	// reaper when the child is actually reaped, never when the caller stops
	// waiting: an unreaped child may still be touching the BAR.
	Access *AccessArbiter

	// launch is a test seam: it starts the child and returns it. nil starts a
	// real `pci-read` helper process.
	launch func(stdout, stderr io.Writer) (childProcess, error)
	// slot is a test seam: the launch permit. nil uses the process-wide helper
	// slot.
	slot *reapSlot
	// resourceRoot is a test-only fixture root the request path must live
	// under. Empty means production: only the canonical
	// /sys/bus/pci/devices/<bdf>/resource0 path is accepted.
	resourceRoot string
}

// helperExecutable resolves the binary to execute. The production answer is
// the kernel's own /proc/self/exe handle rather than a pathname that an
// upgrade may have replaced; tests can keep injecting an override.
func (h HelperSource) helperExecutable() (string, error) {
	if h.Executable != nil {
		return h.Executable()
	}
	return selfExecutablePath()
}

// ReadRegisters implements [RegisterSource].
func (h HelperSource) ReadRegisters(ctx context.Context, resourcePath string, offsets []int64) ([]*uint32, error) {
	nils := make([]*uint32, len(offsets))
	if len(offsets) == 0 {
		return []*uint32{}, nil
	}
	request := pciReadRequest{Path: resourcePath, Offsets: offsets}
	if err := validatePCIReadRequest(request); err != nil {
		return nils, err
	}
	if err := h.validateResourceRequest(resourcePath); err != nil {
		return nils, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nils, err
	}
	bdf, ok := resourceBDF(resourcePath)
	if !ok {
		return nils, fmt.Errorf("gpu: pci-read path %q does not name a PCI function", resourcePath)
	}

	path, err := h.helperExecutable()
	if err != nil {
		return nils, fmt.Errorf("gpu: locate the pci-read helper: %w", err)
	}

	timeout := h.Timeout
	if timeout <= 0 {
		timeout = helperReadTimeout
	}
	waitDelay := h.WaitDelay
	if waitDelay <= 0 {
		waitDelay = helperWaitDelay
	}

	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nils, err
		}
	}

	// Take the device lease before dispatch. Revocation waits for it, and it
	// is only released by the reaper below, so a stopped caller can never
	// leave an unaccounted child behind.
	var lease *AccessLease
	if h.Access != nil {
		lease, err = h.Access.Acquire(ctx, bdf)
		if err != nil {
			return nils, err
		}
		defer func() { lease.Release() }()
		// Re-read the kernel power state under the lease, immediately before
		// the dispatch: the gate's decision may predate an autosuspend. A
		// device the kernel no longer reports usable is never mapped, and the
		// caller gets the powered-down or unknown reason instead of a
		// permissions message.
		if err := lease.Recheck(); err != nil {
			return nils, err
		}
	}

	slot := h.slot
	if slot == nil {
		slot = helperSlots
	}
	if !slot.tryAcquire() {
		return nils, fmt.Errorf("gpu: pci-read helper: %w", errChildNotReaped)
	}

	stdout := &cappedBuffer{limit: helperResponseLimit}
	stderr := &cappedBuffer{limit: helperStderrLimit}
	launch := h.launch
	if launch == nil {
		commandCtx := ctx
		if commandCtx == nil {
			commandCtx = context.Background()
		}
		launch = func(stdout, stderr io.Writer) (childProcess, error) {
			// CommandContext ties cancellation to the caller as well as
			// installing the process-group kill; the hard deadline is still
			// enforced by awaitReap.
			cmd := exec.CommandContext(commandCtx, path, helperInternalArg, HelperCommand)
			cmd.Stdin = bytes.NewReader(payload)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			// WaitDelay is the second half of the pipe bound: killing the group
			// ends the helper, and this keeps inherited pipe copies from
			// holding Wait.
			cmd.WaitDelay = waitDelay
			if helperLaunchEnv != nil {
				if env := helperLaunchEnv(h.resourceRoot); len(env) > 0 {
					cmd.Env = append(os.Environ(), env...)
				}
			}
			configureChildProcess(cmd)
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return &execChild{cmd: cmd}, nil
		}
	}
	// The access lease still has its deferred release here; launchOwned frees
	// the slot if launch panics before the reaper owns it.
	proc, err := launchOwned(launch, stdout, stderr, slot.release)
	if err != nil {
		slot.release()
		return nils, fmt.Errorf("gpu: start the pci-read helper: %w", err)
	}
	// One owned reaper per child: it is the only goroutine that calls Wait.
	// The slot and the access lease are released only when the child is
	// actually reaped, not when this caller gives up on the wait.
	heldLease := lease
	lease = nil // ownership passes to the reaper
	reaper := startReaper(proc, func() {
		slot.release()
		heldLease.Release()
	})
	outcome, waitErr := awaitReap(ctx, reaper, timeout)
	if stdout.Overflowed() {
		return nils, fmt.Errorf("gpu: pci-read helper output exceeded %d bytes", helperResponseLimit)
	}
	switch outcome {
	case reapTimedOut:
		return nils, fmt.Errorf("gpu: PCI resource read timed out after %s", timeout)
	case reapCanceled:
		return nils, waitErr
	}
	if waitErr != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nils, fmt.Errorf("gpu: pci-read helper failed: %w: %s", waitErr, detail)
		}
		return nils, fmt.Errorf("gpu: pci-read helper failed: %w", waitErr)
	}

	var values []*uint32
	if err := json.Unmarshal(stdout.Bytes(), &values); err != nil {
		return nils, fmt.Errorf("gpu: pci-read helper returned malformed JSON: %w", err)
	}
	if len(values) != len(offsets) {
		return nils, fmt.Errorf("gpu: pci-read helper returned %d values for %d offsets",
			len(values), len(offsets))
	}
	return values, nil
}

// RunPCIReadHelper is the entry point of the `rigscope internal pci-read`
// child. It reads one bounded JSON request from stdin, maps the requested BAR0
// pages and writes one JSON array with an element per offset (null when the
// offset was unreadable). It returns the process exit code: 0 for a completed
// read (including an all-nil answer), 2 for a malformed request, 3 when the
// response cannot be produced.
func RunPCIReadHelper(stdin io.Reader, stdout, stderr io.Writer) int {
	raw, err := io.ReadAll(io.LimitReader(stdin, helperRequestLimit+1))
	if err != nil {
		fmt.Fprintf(stderr, "pci-read: read request: %v\n", err)
		return 2
	}
	if len(raw) > helperRequestLimit {
		fmt.Fprintf(stderr, "pci-read: request exceeds %d bytes\n", helperRequestLimit)
		return 2
	}
	var request pciReadRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		fmt.Fprintf(stderr, "pci-read: malformed request: %v\n", err)
		return 2
	}
	if err := validatePCIReadRequest(request); err != nil {
		fmt.Fprintf(stderr, "pci-read: %v\n", err)
		return 2
	}

	// The root is a test-only fixture override; production leaves it unset and
	// resolveResourcePath enforces the host /sys/devices boundary.
	values, err := readHostRegisters(request.Path, helperFixtureRoot(), request.Offsets)
	if err != nil || len(values) != len(request.Offsets) {
		// An unreadable BAR is a normal outcome: answer with a nil per offset
		// so the parent keeps the sensor fields absent.
		values = make([]*uint32, len(request.Offsets))
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		fmt.Fprintf(stderr, "pci-read: encode response: %v\n", err)
		return 3
	}
	if len(encoded) > helperResponseLimit {
		fmt.Fprintf(stderr, "pci-read: response exceeds %d bytes\n", helperResponseLimit)
		return 3
	}
	if _, err := stdout.Write(encoded); err != nil {
		fmt.Fprintf(stderr, "pci-read: write response: %v\n", err)
		return 3
	}
	return 0
}
