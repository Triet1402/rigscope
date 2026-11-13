package gpu

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Runtime power-management awareness for offload GPUs (hybrid laptops).
//
// A discrete GPU on a laptop powers down (runtime D3) only after it has been
// idle for a while. Every nvidia-smi invocation, and every read of amdgpu's
// gpu_busy_percent, mem_info_* or hwmon files, resumes the device and
// restarts that idle timer. Polled every two seconds, the GPU never sleeps.
//
// The gate therefore:
//
//   - never samples a device whose power/runtime_status is suspended (the
//     status file itself is a kernel bookkeeping read that does not resume
//     it);
//   - after a few consecutive idle samples of an awake device, revokes the
//     device's access leases and waits for them to drain, then pauses
//     sampling for a quiet window so the driver can suspend it. If it
//     suspends, sampling stays off until something else wakes it. If it stays
//     awake (a client such as a compositor or persistence daemon holds it),
//     sampling resumes and the next pause is backed off exponentially.
//
// Devices without runtime PM (control = on), the boot display adapter, and
// NVIDIA GPUs whose driver reports Runtime D3 disabled are never gated, so
// desktops and dGPU-only setups keep full-rate telemetry. Every exemption is
// evaluated only after the device is known not to be suspended; an unreadable
// or malformed power attribute denies sampling rather than guessing.
//
// An "unsupported" runtime_status only means runtime PM is disabled
// (disable_depth > 0); it masks the real power state, and a device disabled
// while suspended stays suspended. It is therefore accepted only when the
// cached PCI power_state attribute — which reports pci_dev->current_state
// without touching the device — reads exactly D0. A D3* reading is treated as
// suspended (and denied as powered down) even for an otherwise exempt device,
// and a missing or unrecognised reading denies access as unknown.

const (
	// PoweredDownReason is reported for a runtime-suspended device.
	PoweredDownReason = "GPU powered down (runtime suspend)"
	// PausedReason is reported while sampling is paused so an idle GPU can
	// power down.
	PausedReason = "GPU idle; sampling paused so it can power down"
	// BusyReason is reported when a quiet window cannot start because an
	// outstanding hardware operation did not drain within its bound. New
	// access stays denied.
	BusyReason = "GPU busy; hardware access did not drain, cannot yield"
	// AccessUnknownReason is reported when the runtime-PM attributes cannot
	// prove that the device is awake. Sampling is denied, not guessed.
	AccessUnknownReason = "GPU power state unknown; sampling denied"

	// DefaultPCIRoot is the PCI device root.
	DefaultPCIRoot = "/sys/bus/pci/devices"
	// DefaultNvidiaProcRoot is where the NVIDIA driver describes runtime D3.
	DefaultNvidiaProcRoot = "/proc/driver/nvidia/gpus"

	idleSamplesBeforeYieldDefault = 3
	minYieldWindow                = 20 * time.Second
	maxYieldWindow                = 120 * time.Second
	initialYieldBackoff           = 5 * time.Minute
	maxYieldBackoff               = time.Hour
)

// RuntimePMStatus is the kernel's runtime power state for one PCI device.
type RuntimePMStatus string

const (
	// RuntimePMUnmanaged means the kernel reports no runtime PM support and
	// the device's cached PCI power_state proves it is awake (D0). "No
	// runtime PM" alone is not enough: it is also reported while runtime PM
	// is disabled (disable_depth > 0), which masks a suspended device.
	RuntimePMUnmanaged RuntimePMStatus = "unmanaged"
	// RuntimePMActive means the device is powered on.
	RuntimePMActive RuntimePMStatus = "active"
	// RuntimePMSuspended means the device is powered down.
	RuntimePMSuspended RuntimePMStatus = "suspended"
	// RuntimePMUnknown means the status could not be read or held a value
	// the kernel does not document. It is never treated as unmanaged: an
	// unknown power state must not authorize a hardware access.
	RuntimePMUnknown RuntimePMStatus = "unknown"
)

// RuntimePMGateOptions configures a [RuntimePMGate]. Zero values select the
// real sysfs and procfs trees.
type RuntimePMGateOptions struct {
	// Read reads one attribute; nil means the host's "/" filesystem.
	Read TextReadFunc
	// Now is the injectable monotonic clock.
	Now func() time.Duration
	// OnEvent receives short diagnostic lines.
	OnEvent func(string)
	// PCIRoot is the PCI device root, [DefaultPCIRoot] by default.
	PCIRoot string
	// NvidiaProcRoot is the NVIDIA procfs root, [DefaultNvidiaProcRoot] by
	// default.
	NvidiaProcRoot string
	// IdleSamplesBeforeYield is how many consecutive idle samples are needed
	// before sampling pauses, 3 by default.
	IdleSamplesBeforeYield int
	// Access is the shared per-device arbiter. nil builds a private one; a
	// poller or metrics collector that touches the same devices must be
	// given the same arbiter.
	Access *AccessArbiter
	// AccessDrainTimeout bounds how long a revocation waits for outstanding
	// leases. Zero selects the arbiter's default.
	AccessDrainTimeout time.Duration
}

// GateDecision is the runtime-PM verdict for one device.
type GateDecision struct {
	Sample bool
	// Reason is set when Sample is false, and is reported to users verbatim.
	Reason string
}

// PowerStateReader re-reads a device's kernel runtime-PM status after the
// gate decision and immediately before a hardware operation. [RuntimePMGate]
// implements it and binds itself to the shared [AccessArbiter], so every
// [AccessLease.Recheck] consults the same sysfs view the decision came from.
type PowerStateReader interface {
	RuntimeStatus(address string) RuntimePMStatus
}

// PowerGate decides whether a PCI device may be sampled now and records
// whether it looked idle. [RuntimePMGate] implements it.
type PowerGate interface {
	Decide(address string) GateDecision
	Record(address string, idle bool)
}

type runtimePMDeviceState struct {
	staticallyUnmanaged *bool
	asleep              bool
	idleStreak          int
	yieldUntil          *time.Duration
	nextYieldAt         time.Duration
	backoff             time.Duration
	// yielding records that the current pause attempt reported "busy" so
	// the diagnostic is emitted once per attempt, not once per cycle.
	yielding bool
}

// RuntimePMGate decides which devices may be sampled without waking them.
// It is safe for concurrent use; several callers per sample (inventory and
// metrics) agree because only time, observed power state and access leases
// move the state.
type RuntimePMGate struct {
	read                   TextReadFunc
	now                    func() time.Duration
	onEvent                func(string)
	pciRoot                string
	nvidiaProcRoot         string
	idleSamplesBeforeYield int
	access                 *AccessArbiter

	mu      sync.Mutex
	devices map[string]*runtimePMDeviceState
}

// NewRuntimePMGate builds a gate.
func NewRuntimePMGate(options RuntimePMGateOptions) *RuntimePMGate {
	read := options.Read
	if read == nil {
		read = HostFS().ReadText
	}
	now := options.Now
	if now == nil {
		start := time.Now()
		now = func() time.Duration { return time.Since(start) }
	}
	pciRoot := options.PCIRoot
	if pciRoot == "" {
		pciRoot = DefaultPCIRoot
	}
	nvidiaProcRoot := options.NvidiaProcRoot
	if nvidiaProcRoot == "" {
		nvidiaProcRoot = DefaultNvidiaProcRoot
	}
	idleSamples := options.IdleSamplesBeforeYield
	if idleSamples <= 0 {
		idleSamples = idleSamplesBeforeYieldDefault
	}
	access := options.Access
	if access == nil {
		access = NewAccessArbiter()
	}
	if options.AccessDrainTimeout > 0 {
		access.SetDrainTimeout(options.AccessDrainTimeout)
	}
	gate := &RuntimePMGate{
		read:                   read,
		now:                    now,
		onEvent:                options.OnEvent,
		pciRoot:                pciRoot,
		nvidiaProcRoot:         nvidiaProcRoot,
		idleSamplesBeforeYield: idleSamples,
		access:                 access,
		devices:                make(map[string]*runtimePMDeviceState),
	}
	// Every hardware operation that holds a lease re-checks this gate's view
	// of runtime_status immediately before it touches the device.
	access.setPowerStateReader(gate)
	return gate
}

// Arbiter returns the per-device access arbiter this gate revokes. Sources
// that touch the same devices must use it for every hardware operation.
func (g *RuntimePMGate) Arbiter() *AccessArbiter { return g.access }

// GateAccess returns the arbiter behind a [PowerGate] when it has one. It lets
// callers outside this package (the metrics assembler) honor the same leases
// without widening the PowerGate interface.
func GateAccess(gate PowerGate) *AccessArbiter {
	if runtime, ok := gate.(*RuntimePMGate); ok {
		return runtime.Arbiter()
	}
	return nil
}

var runtimeD3Pattern = regexp.MustCompile(`(?i)Runtime D3 status:\s*(Disabled|Not supported)`)
var digitsPattern = regexp.MustCompile(`^\d+$`)

// ReadRuntimePM reports the current runtime-PM state of one PCI device
// without resuming it. The status file is kernel bookkeeping; reading it
// does not wake the device.
//
// An "unsupported" status is not proof that the device is awake: the kernel
// also reports it when runtime PM is disabled (disable_depth > 0), which
// masks the real power state, so a device disabled while suspended stays
// suspended. It is mapped to [RuntimePMUnmanaged] only when the cached
// power_state attribute reads exactly D0, to [RuntimePMSuspended] for a D3*
// reading, and to [RuntimePMUnknown] for anything else. Reading power_state
// returns the cached pci_dev->current_state and does not touch the device.
func ReadRuntimePM(read TextReadFunc, address, pciRoot string) RuntimePMStatus {
	if pciRoot == "" {
		pciRoot = DefaultPCIRoot
	}
	status := ""
	if raw, ok := read(pciRoot + "/" + address + "/power/runtime_status"); ok {
		status = strings.TrimSpace(raw)
	}
	switch status {
	case "suspended", "suspending":
		return RuntimePMSuspended
	case "active", "resuming":
		return RuntimePMActive
	case "unsupported":
		// The kernel reports no runtime PM support when runtime PM is
		// disabled, which does not reveal whether the device is suspended.
		switch powerState := readPowerState(read, address, pciRoot); {
		case powerState == "D0":
			return RuntimePMUnmanaged
		case strings.HasPrefix(powerState, "D3"):
			return RuntimePMSuspended
		default:
			// Missing, unreadable, "unknown", D1 and D2 cannot prove the
			// device is awake.
			return RuntimePMUnknown
		}
	default:
		// Missing, unreadable, empty, "error" and anything undocumented.
		return RuntimePMUnknown
	}
}

// readPowerState returns the cached PCI power state of one device, or "" when
// the attribute is missing or unreadable. The kernel exposes it from
// pci_dev->current_state, so reading it does not touch the device.
func readPowerState(read TextReadFunc, address, pciRoot string) string {
	if raw, ok := read(pciRoot + "/" + address + "/power_state"); ok {
		return strings.TrimSpace(raw)
	}
	return ""
}

// PoweredDown reports whether the device is currently runtime-suspended. The
// suspended state is checked before any exemption: a device the kernel says is
// off is never touched because of its boot or RTD3 setting.
func (g *RuntimePMGate) PoweredDown(address string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return ReadRuntimePM(g.read, address, g.pciRoot) == RuntimePMSuspended
}

// RuntimeStatus re-reads one device's kernel runtime-PM status. It takes no
// gate lock: the status file is kernel bookkeeping and the reader is
// immutable, so it is safe from the dispatcher goroutines that call
// [AccessLease.Recheck].
func (g *RuntimePMGate) RuntimeStatus(address string) RuntimePMStatus {
	if g == nil {
		return RuntimePMActive
	}
	return ReadRuntimePM(g.read, address, g.pciRoot)
}

// Decide reports whether the device may be sampled now. It is idempotent
// within a cycle: only time and observed power state move it.
func (g *RuntimePMGate) Decide(address string) GateDecision {
	g.mu.Lock()
	decision, event := g.decideLocked(address)
	g.mu.Unlock()
	if event != "" {
		g.emit(event)
	}
	return decision
}

func (g *RuntimePMGate) decideLocked(address string) (GateDecision, string) {
	status := ReadRuntimePM(g.read, address, g.pciRoot)

	// The suspended state comes before every exemption. A device the kernel
	// says is off must never be touched, whatever power/control says.
	if status == RuntimePMSuspended {
		state := g.stateLocked(address)
		event := ""
		if !state.asleep {
			event = fmt.Sprintf("gpu %s: powered down; sampling suspended until it wakes", address)
		}
		state.asleep = true
		state.idleStreak = 0
		state.yieldUntil = nil
		state.backoff = initialYieldBackoff
		state.nextYieldAt = 0
		state.yielding = false
		g.access.Revoke(address)
		return GateDecision{Sample: false, Reason: PoweredDownReason}, event
	}

	// An unreadable or undocumented status cannot prove the device is awake.
	if status == RuntimePMUnknown {
		g.access.Revoke(address)
		return GateDecision{Sample: false, Reason: AccessUnknownReason}, ""
	}

	// No runtime PM to manage, proven awake by the cached power_state: there
	// is nothing that can suspend it, so it is never gated. ReadRuntimePM
	// already mapped a D3* power_state to suspended and an unknown or missing
	// one to unknown above, so an exempt device cannot hide a D3* reading.
	if status == RuntimePMUnmanaged {
		return g.unmanagedLocked(address), ""
	}

	control, controlKnown := g.powerControlLocked(address)
	if control == "on" {
		// Known unmanaged: the kernel is not runtime-managing this device.
		return g.unmanagedLocked(address), ""
	}
	if g.staticallyUnmanagedLocked(address) {
		return g.unmanagedLocked(address), ""
	}
	if !controlKnown || control != "auto" {
		// Missing or malformed power/control: fail closed.
		g.access.Revoke(address)
		return GateDecision{Sample: false, Reason: AccessUnknownReason}, ""
	}
	if status != RuntimePMActive {
		// auto-managed but the kernel does not report an active device.
		g.access.Revoke(address)
		return GateDecision{Sample: false, Reason: AccessUnknownReason}, ""
	}

	state := g.stateLocked(address)
	now := g.now()
	if state.asleep {
		// Woken by another client: report it while it is in use.
		state.asleep = false
		state.idleStreak = 0
		state.yieldUntil = nil
		state.yielding = false
		g.access.Permit(address)
		return GateDecision{Sample: true}, fmt.Sprintf("gpu %s: woke up; sampling resumed", address)
	}
	if state.yieldUntil != nil {
		if now < *state.yieldUntil {
			return GateDecision{Sample: false, Reason: PausedReason}, ""
		}
		// Still awake after a quiet window: something else holds it.
		state.yieldUntil = nil
		state.idleStreak = 0
		state.nextYieldAt = now + state.backoff
		state.backoff = minDuration(state.backoff*2, maxYieldBackoff)
	}

	sample := GateDecision{Sample: true}
	if state.idleStreak >= g.idleSamplesBeforeYield && now >= state.nextYieldAt {
		// Revoke first, and start the quiet window only once every lease has
		// drained. The device is denied from the moment revocation begins,
		// so a new access cannot slip into the window. A previous attempt
		// that is still draining is reported immediately instead of waiting
		// again for the same leases.
		status := g.access.Status(address)
		if status.State != AccessPermitted && status.Outstanding > 0 {
			event := ""
			if !state.yielding {
				state.yielding = true
				event = fmt.Sprintf("gpu %s: %s", address, BusyReason)
			}
			return GateDecision{Sample: false, Reason: BusyReason}, event
		}
		drained := g.access.Revoke(address)
		if !drained.Drained {
			event := ""
			if !state.yielding {
				state.yielding = true
				event = fmt.Sprintf("gpu %s: %s", address, BusyReason)
			}
			return GateDecision{Sample: false, Reason: BusyReason}, event
		}
		window := g.yieldWindowLocked(address)
		// The drain above can take up to the arbiter's bound. Measure the
		// quiet window from the moment the device actually became free, not
		// from the moment the gate asked for it, or the driver gets less
		// quiet time than the window promises.
		until := g.now() + window
		state.yieldUntil = &until
		state.yielding = false
		return GateDecision{Sample: false, Reason: PausedReason},
			fmt.Sprintf("gpu %s: idle; sampling paused so it can power down", address)
	}
	g.access.Permit(address)
	return sample, ""
}

// unmanagedLocked handles a device the kernel does not runtime-manage. It
// clears any previous pause and re-authorizes access.
func (g *RuntimePMGate) unmanagedLocked(address string) GateDecision {
	state := g.stateLocked(address)
	if state.yieldUntil != nil {
		g.access.Permit(address)
	}
	state.asleep = false
	state.idleStreak = 0
	state.yieldUntil = nil
	state.nextYieldAt = 0
	state.backoff = initialYieldBackoff
	state.yielding = false
	g.access.Permit(address)
	return GateDecision{Sample: true}
}

// Record reports the outcome of a sample taken after [Decide] allowed it. It
// only counts idleness; the quiet window is started by a later Decide once the
// arbiter has drained the outstanding leases.
func (g *RuntimePMGate) Record(address string, idle bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.managedLocked(address) {
		return
	}
	state := g.stateLocked(address)
	if !idle {
		state.idleStreak = 0
		state.nextYieldAt = 0
		state.backoff = initialYieldBackoff
		return
	}
	state.idleStreak++
}

// yieldWindowLocked gives the driver enough quiet time to autosuspend.
func (g *RuntimePMGate) yieldWindowLocked(address string) time.Duration {
	delay := time.Duration(0)
	if raw, ok := g.read(g.pciRoot + "/" + address + "/power/autosuspend_delay_ms"); ok {
		trimmed := strings.TrimSpace(raw)
		if digitsPattern.MatchString(trimmed) {
			if value, err := strconv.ParseUint(trimmed, 10, 32); err == nil {
				delay = time.Duration(value) * time.Millisecond
			}
		}
	}
	window := delay + 10*time.Second
	if window < minYieldWindow {
		window = minYieldWindow
	}
	if window > maxYieldWindow {
		window = maxYieldWindow
	}
	return window
}

// powerControlLocked reads power/control. It reports false for a missing,
// unreadable or undocumented value; only an explicit "on" or "auto" is known.
func (g *RuntimePMGate) powerControlLocked(address string) (string, bool) {
	raw, ok := g.read(g.pciRoot + "/" + address + "/power/control")
	if !ok {
		return "", false
	}
	switch strings.TrimSpace(raw) {
	case "on":
		return "on", true
	case "auto":
		return "auto", true
	default:
		return "", false
	}
}

// managedLocked reports whether runtime PM governs this device. power/control
// is re-read every time: power managers such as TLP switch it between "on"
// (AC) and "auto" (battery) when the charger is unplugged.
func (g *RuntimePMGate) managedLocked(address string) bool {
	if g.staticallyUnmanagedLocked(address) {
		return false
	}
	control, ok := g.powerControlLocked(address)
	return ok && control == "auto"
}

// staticallyUnmanagedLocked covers the settings fixed for the life of the
// driver binding.
func (g *RuntimePMGate) staticallyUnmanagedLocked(address string) bool {
	state := g.stateLocked(address)
	if state.staticallyUnmanaged == nil {
		unmanaged := g.staticUnmanagedProbeLocked(address)
		state.staticallyUnmanaged = &unmanaged
	}
	return *state.staticallyUnmanaged
}

func (g *RuntimePMGate) staticUnmanagedProbeLocked(address string) bool {
	// The adapter driving the boot display cannot power down while it scans
	// out.
	if boot, ok := g.read(g.pciRoot + "/" + address + "/boot_vga"); ok && strings.TrimSpace(boot) == "1" {
		return true
	}
	// NVIDIA without fine-grained RTD3 holds the device awake regardless of
	// us; gating it would only punch holes in the data.
	if power, ok := g.read(g.nvidiaProcRoot + "/" + address + "/power"); ok {
		if runtimeD3Pattern.MatchString(power) {
			return true
		}
	}
	return false
}

func (g *RuntimePMGate) stateLocked(address string) *runtimePMDeviceState {
	state, ok := g.devices[address]
	if !ok {
		state = &runtimePMDeviceState{backoff: initialYieldBackoff}
		g.devices[address] = state
	}
	return state
}

func (g *RuntimePMGate) emit(line string) {
	if g.onEvent == nil {
		return
	}
	defer func() { _ = recover() }()
	g.onEvent(line)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
