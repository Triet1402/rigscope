package gpu

import (
	"errors"
	"runtime/debug"
)

// Bounds for one PCI register batch, carried over from the prototype's python
// helper: at most 512 offsets, 4-byte aligned, inside the first 16 MiB of the
// BAR.
const (
	maxRegisterOffsets = 512
	maxRegisterOffset  = int64(0x1000000)
)

// ErrMappingUnsupported is returned on platforms without syscall.Mmap.
var ErrMappingUnsupported = errors.New("gpu: PCI resource mapping is only supported on Linux")

// guardFault runs fn with debug.SetPanicOnFault enabled and turns a fault
// panic into a true return value instead of crashing the process.
//
// A PCI BAR can disappear under a running program (device hot-unplug, driver
// unbind, VM passthrough teardown), and touching memory mapped from it then
// delivers SIGBUS. Without SetPanicOnFault that signal kills the process;
// with it the runtime panics on the faulting goroutine, and recover here
// converts the fault into "these registers are unreadable".
//
// SetPanicOnFault is per-goroutine, not process-wide: the setting affects only
// the goroutine that calls it. guardFault therefore needs no lock and is safe
// to call from several goroutines at once. The helper child runs the reads on
// one goroutine, so a fault there can only take down that child, never the
// collector.
func guardFault(fn func()) (faulted bool) {
	previous := debug.SetPanicOnFault(true)
	defer debug.SetPanicOnFault(previous)
	defer func() {
		if recover() != nil {
			faulted = true
		}
	}()
	fn()
	return false
}
