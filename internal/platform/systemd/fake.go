package systemd

import (
	"context"
	"fmt"
	"sync"
)

// Call records one Fake Manager invocation.
type Call struct {
	Method string
	Unit   string
}

// Fake is an in-memory Manager for tests. It never invokes systemctl.
type Fake struct {
	mu sync.Mutex
	// Calls is the ordered list of commands received.
	Calls []Call
	// Active maps unit names to an active state. Missing units fall back to
	// ActiveByDefault. A unit may be active and not yet running; Running
	// overrides the running answer for transition-state tests.
	Active          map[string]bool
	ActiveByDefault bool
	// Running maps unit names to a running state. Missing units follow their
	// active state, so most tests only configure Active.
	Running map[string]bool
	// Enabled maps unit names to enablement; Enable/Disable update it.
	Enabled map[string]bool
	// Known maps unit names to whether systemd knows them. Missing units fall
	// back to KnownByDefault.
	Known          map[string]bool
	KnownByDefault bool
	// Fail maps "Method unit" (or "Method" for DaemonReload) to an error.
	Fail map[string]error
	// LingerCalls counts EnableLinger calls.
	LingerCalls int
}

// NewFake returns a Fake with initialised maps.
func NewFake() *Fake {
	return &Fake{
		Active:         map[string]bool{},
		Running:        map[string]bool{},
		Enabled:        map[string]bool{},
		Known:          map[string]bool{},
		KnownByDefault: true,
		Fail:           map[string]error{},
	}
}

func (f *Fake) record(method, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: method, Unit: unit})
	if key := method + " " + unit; f.Fail[key] != nil {
		return f.Fail[key]
	}
	if f.Fail[method] != nil {
		return f.Fail[method]
	}
	return nil
}

// DaemonReload implements Manager.
func (f *Fake) DaemonReload(context.Context) error { return f.record("daemon-reload", "") }

// Enable implements Manager.
func (f *Fake) Enable(_ context.Context, unit string) error {
	if err := f.record("enable", unit); err != nil {
		return err
	}
	f.mu.Lock()
	f.Enabled[unit] = true
	f.mu.Unlock()
	return nil
}

// Disable implements Manager.
func (f *Fake) Disable(_ context.Context, unit string) error {
	if err := f.record("disable", unit); err != nil {
		return err
	}
	f.mu.Lock()
	f.Enabled[unit] = false
	f.mu.Unlock()
	return nil
}

// Start implements Manager.
func (f *Fake) Start(_ context.Context, unit string) error {
	if err := f.record("start", unit); err != nil {
		return err
	}
	f.setActive(unit, true)
	return nil
}

// Stop implements Manager.
func (f *Fake) Stop(_ context.Context, unit string) error {
	if err := f.record("stop", unit); err != nil {
		return err
	}
	f.setActive(unit, false)
	return nil
}

// Restart implements Manager.
func (f *Fake) Restart(_ context.Context, unit string) error {
	if err := f.record("restart", unit); err != nil {
		return err
	}
	f.setActive(unit, true)
	return nil
}

// IsActive implements Manager.
func (f *Fake) IsActive(_ context.Context, unit string) (bool, error) {
	if err := f.record("is-active", unit); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if active, ok := f.Active[unit]; ok {
		return active, nil
	}
	return f.ActiveByDefault, nil
}

// UnitRunning implements Manager.
func (f *Fake) UnitRunning(_ context.Context, unit string) (bool, error) {
	if err := f.record("unit-running", unit); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if running, ok := f.Running[unit]; ok {
		return running, nil
	}
	if active, ok := f.Active[unit]; ok {
		return active, nil
	}
	return f.ActiveByDefault, nil
}

// UnitKnown implements Manager.
func (f *Fake) UnitKnown(_ context.Context, unit string) (bool, error) {
	if err := f.record("unit-known", unit); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if known, ok := f.Known[unit]; ok {
		return known, nil
	}
	return f.KnownByDefault, nil
}

// EnableLinger implements Manager.
func (f *Fake) EnableLinger(context.Context) error {
	if err := f.record("enable-linger", ""); err != nil {
		return err
	}
	f.mu.Lock()
	f.LingerCalls++
	f.mu.Unlock()
	return nil
}

// Reset clears recorded calls while keeping configured state.
func (f *Fake) Reset() {
	f.mu.Lock()
	f.Calls = nil
	f.LingerCalls = 0
	f.mu.Unlock()
}

// Methods returns the call method names in order.
func (f *Fake) Methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	methods := make([]string, len(f.Calls))
	for i, call := range f.Calls {
		methods[i] = call.Method + " " + call.Unit
	}
	return methods
}

// HasCall reports whether method+unit was invoked.
func (f *Fake) HasCall(method, unit string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.Calls {
		if call.Method == method && call.Unit == unit {
			return true
		}
	}
	return false
}

func (f *Fake) setActive(unit string, active bool) {
	f.mu.Lock()
	f.Active[unit] = active
	// A completed start/stop also settles the transition override.
	f.Running[unit] = active
	f.mu.Unlock()
}

var _ Manager = (*Fake)(nil)

// String helps test failures read well.
func (c Call) String() string { return fmt.Sprintf("%s %s", c.Method, c.Unit) }
