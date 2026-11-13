package gpu

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Every hardware-touching operation on one PCI function is serialized through
// that function's [AccessArbiter] entry. A device moves between three states:
//
//	permitted  new leases are granted;
//	revoking   no new leases are granted, and outstanding leases are draining;
//	denied     no leases are granted and none are outstanding.
//
// The runtime-PM gate revokes a device before it starts a quiet window, so a
// quiet window can never overlap an operation that would wake the device
// again. Revocation is bounded: when the outstanding operations do not settle
// in time the device stays revoking (still denying new access) and the caller
// reports "busy, cannot yield" instead of pretending the window started.
const defaultAccessDrainTimeout = time.Second

// AccessDeniedReason is the user-facing reason for an operation refused by
// the arbiter. It must never be confused with a permissions failure.
const AccessDeniedReason = "GPU hardware access denied by the runtime-PM gate"

// AccessState is the state of one device in its [AccessArbiter].
type AccessState string

const (
	// AccessPermitted means a new access lease may be acquired.
	AccessPermitted AccessState = "permitted"
	// AccessRevoking means no new lease is granted; existing leases drain.
	AccessRevoking AccessState = "revoking"
	// AccessDenied means no new lease is granted and none is outstanding.
	AccessDenied AccessState = "denied"
)

var (
	// ErrAccessDenied is returned by [AccessArbiter.Acquire] when the device
	// is revoking or denied. Access fails closed: the operation must not
	// touch the device.
	ErrAccessDenied = errors.New("gpu: hardware access denied")
	// ErrUnknownPCIDevice is returned for an address that cannot identify a
	// PCI function, because there is nothing to arbitrate.
	ErrUnknownPCIDevice = errors.New("gpu: unrecognized PCI address")
)

// AccessStatus is a snapshot of one device's arbitration state.
type AccessStatus struct {
	State       AccessState
	Outstanding int
}

// RevokeResult reports the outcome of a bounded revocation.
type RevokeResult struct {
	// Drained is true when no lease is outstanding and the device is denied.
	Drained bool
	// State is the state the device reached.
	State AccessState
	// Outstanding counts leases still held when the drain bound expired.
	Outstanding int
	// Superseded is true when a later transition — typically a concurrent
	// [AccessArbiter.Permit] — replaced the revocation this call started.
	// The verdict is always Drained:false: a device that was re-authorized
	// mid-revocation must not be treated as yielded, and this call must never
	// publish a denial over a lease that the newer transition granted.
	Superseded bool
}

// AccessArbiter owns one [deviceArbiter] per canonical PCI address. It is safe
// for concurrent use and shared by every source that can touch a device: the
// nvidia-smi poller, the register-temperature helper and DRM sysfs reads.
type AccessArbiter struct {
	mu           sync.Mutex
	devices      map[string]*deviceArbiter
	drainTimeout time.Duration
	// power, when set, is re-read immediately before each access so a device
	// that suspended between the gate decision and the hardware operation is
	// refused. [RuntimePMGate] binds itself here.
	power PowerStateReader
}

type deviceArbiter struct {
	mu     sync.Mutex
	state  AccessState
	leases int
	// generation advances on every deliberate transition (Revoke start,
	// Permit). A revocation records the generation it began with and only
	// applies its verdict while that generation is still current, so a
	// Permit that re-authorizes the device mid-drain wins and the revocation
	// reports itself superseded instead of denying a live lease.
	generation uint64
	// drained is closed when the last outstanding lease is released while
	// the device is revoking. A waiter that timed out leaves it in place so
	// a later lease release still drives the transition to denied.
	drained chan struct{}
}

// NewAccessArbiter builds an empty arbiter. Every device starts permitted;
// the gate's first observation moves it to the appropriate state.
func NewAccessArbiter() *AccessArbiter {
	return &AccessArbiter{
		devices:      make(map[string]*deviceArbiter),
		drainTimeout: defaultAccessDrainTimeout,
	}
}

// SetDrainTimeout bounds how long [AccessArbiter.Revoke] waits for outstanding
// leases. A non-positive value restores the default.
func (a *AccessArbiter) SetDrainTimeout(timeout time.Duration) {
	if a == nil {
		return
	}
	if timeout <= 0 {
		timeout = defaultAccessDrainTimeout
	}
	a.mu.Lock()
	a.drainTimeout = timeout
	a.mu.Unlock()
}

// AccessLease is one authorized hardware operation. It must be released only
// once the operation has fully settled; for the pci-read helper that is when
// the child process has been reaped, not when the caller stopped waiting.
// Release is idempotent and safe on a nil lease.
type AccessLease struct {
	arbiter *AccessArbiter
	device  *deviceArbiter
	address string
	once    sync.Once
}

// Release returns the lease. Releasing the last lease of a revoking device
// completes the revocation and lets a waiting quiet window start.
func (l *AccessLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.device != nil {
			l.device.release()
		}
	})
}

// Recheck re-reads the device's kernel runtime-PM status under the live lease
// and refuses the access unless the kernel still reports it usable. It narrows
// the window in which the kernel can autosuspend a device between the gate's
// decision and the actual hardware operation; user space cannot close that
// window completely (nvidia-smi and sysfs offer no non-waking power reference),
// so this is the last check before dispatch, not a guarantee.
//
// It is a no-op on an ungated arbiter (no power reader bound) and returns a
// typed error whose user-facing reason is [PoweredDownReason] for a suspended
// device and [AccessUnknownReason] for an unreadable one.
func (l *AccessLease) Recheck() error {
	if l == nil || l.arbiter == nil {
		return nil
	}
	reader := l.arbiter.powerReader()
	if reader == nil {
		return nil
	}
	switch status := reader.RuntimeStatus(l.address); status {
	case RuntimePMActive, RuntimePMUnmanaged:
		// Active, or runtime PM disabled with the cached power_state proving
		// the device is awake (D0).
		return nil
	default:
		return &powerStateError{address: l.address, status: status}
	}
}

// powerReader returns the bound runtime-PM reader, if any.
func (a *AccessArbiter) powerReader() PowerStateReader {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.power
}

// setPowerStateReader binds the reader every [AccessLease.Recheck] consults.
func (a *AccessArbiter) setPowerStateReader(reader PowerStateReader) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.power = reader
	a.mu.Unlock()
}

func (d *deviceArbiter) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.leases > 0 {
		d.leases--
	}
	if d.leases == 0 && d.state == AccessRevoking {
		d.state = AccessDenied
		if d.drained != nil {
			close(d.drained)
			d.drained = nil
		}
	}
}

// Acquire takes an access lease on one device. It refuses immediately when the
// device is revoking or denied; an operation must never wait for a gate to
// change its mind, because the device may be powered down already.
func (a *AccessArbiter) Acquire(ctx context.Context, address string) (*AccessLease, error) {
	canonical, ok := CanonicalPCIAddress(address)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPCIDevice, address)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	device := a.device(canonical)
	device.mu.Lock()
	defer device.mu.Unlock()
	if device.state != AccessPermitted {
		return nil, fmt.Errorf("%w: %s is %s", ErrAccessDenied, canonical, device.state)
	}
	device.leases++
	return &AccessLease{arbiter: a, device: device, address: canonical}, nil
}

// Revoke stops granting leases on one device and waits, bounded by the
// arbiter's drain timeout, for the outstanding ones to be released. A device
// that does not drain stays revoking: it keeps denying new access and the
// caller must report that it is busy instead of starting a quiet window.
//
// The verdict is applied only if no later transition superseded this
// revocation. A [AccessArbiter.Permit] that arrives while the drain is waiting
// re-authorizes the device, so this call returns Drained:false,
// Superseded:true and leaves whatever the newer transition established — it
// never reports a drain or writes a denial over a lease granted after it
// started.
func (a *AccessArbiter) Revoke(address string) RevokeResult {
	canonical, ok := CanonicalPCIAddress(address)
	if !ok {
		// Nothing can be authorized for an unparseable address, so the safe
		// answer is a drained denial.
		return RevokeResult{Drained: true, State: AccessDenied}
	}
	device := a.device(canonical)

	device.mu.Lock()
	device.generation++
	generation := device.generation
	if device.leases == 0 {
		device.state = AccessDenied
		if device.drained != nil {
			close(device.drained)
			device.drained = nil
		}
		device.mu.Unlock()
		return RevokeResult{Drained: true, State: AccessDenied}
	}
	device.state = AccessRevoking
	waiter := device.drained
	if waiter == nil {
		waiter = make(chan struct{})
		device.drained = waiter
	}
	device.mu.Unlock()

	a.mu.Lock()
	timeout := a.drainTimeout
	a.mu.Unlock()
	if timeout <= 0 {
		timeout = defaultAccessDrainTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-waiter:
	case <-timer.C:
	}

	// One lock, one validation, one transition: the state that is observed is
	// exactly the state this call acts on.
	device.mu.Lock()
	defer device.mu.Unlock()
	if device.generation != generation {
		return RevokeResult{
			Drained:     false,
			Superseded:  true,
			State:       device.state,
			Outstanding: device.leases,
		}
	}
	if device.leases > 0 {
		return RevokeResult{Drained: false, State: device.state, Outstanding: device.leases}
	}
	device.state = AccessDenied
	if device.drained != nil {
		close(device.drained)
		device.drained = nil
	}
	return RevokeResult{Drained: true, State: AccessDenied}
}

// Permit re-authorizes a device after a gate decision allows it again, for
// example when an external client woke it or a quiet window expired. It
// supersedes a revocation that is still waiting: that revocation reports
// [RevokeResult.Superseded] instead of denying access.
func (a *AccessArbiter) Permit(address string) {
	canonical, ok := CanonicalPCIAddress(address)
	if !ok {
		return
	}
	device := a.device(canonical)
	device.mu.Lock()
	defer device.mu.Unlock()
	device.generation++
	device.state = AccessPermitted
	if device.drained != nil {
		close(device.drained)
		device.drained = nil
	}
}

// Status returns the current state and outstanding lease count.
func (a *AccessArbiter) Status(address string) AccessStatus {
	canonical, ok := CanonicalPCIAddress(address)
	if !ok {
		return AccessStatus{State: AccessDenied}
	}
	device := a.device(canonical)
	device.mu.Lock()
	defer device.mu.Unlock()
	return AccessStatus{State: device.state, Outstanding: device.leases}
}

// device returns the arbiter entry for a canonical address, creating a
// permitted one on first use.
func (a *AccessArbiter) device(address string) *deviceArbiter {
	a.mu.Lock()
	defer a.mu.Unlock()
	device, ok := a.devices[address]
	if !ok {
		device = &deviceArbiter{state: AccessPermitted}
		a.devices[address] = device
	}
	return device
}

// powerStateError reports that the pre-access re-check found the device no
// longer usable. It wraps [ErrAccessDenied], so generic refusals still fail
// closed, and carries the reason a user is shown.
type powerStateError struct {
	address string
	status  RuntimePMStatus
}

func (e *powerStateError) Error() string {
	return fmt.Sprintf("%s: %s is %s at access time", AccessDeniedReason, e.address, e.status)
}

func (e *powerStateError) Unwrap() error { return ErrAccessDenied }

// reason is the user-facing explanation for the refusal.
func (e *powerStateError) reason() string {
	if e.status == RuntimePMSuspended {
		return PoweredDownReason
	}
	return AccessUnknownReason
}

// accessDeniedReason turns an access refusal into a user-facing reason that
// never masquerades as a permissions problem.
func accessDeniedReason(err error) string {
	var power *powerStateError
	if errors.As(err, &power) {
		return power.reason()
	}
	if errors.Is(err, ErrUnknownPCIDevice) {
		return "unrecognized PCI address"
	}
	return AccessDeniedReason
}

// powerRefusalReason reports whether an error is the pre-access power-state
// refusal — the device suspended between the gate decision and the launch —
// and the reason to show. It is not a command failure: no access happened.
func powerRefusalReason(err error) (string, bool) {
	var power *powerStateError
	if errors.As(err, &power) {
		return power.reason(), true
	}
	return "", false
}
