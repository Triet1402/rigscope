//go:build !linux

package gpu

import (
	"context"
	"io/fs"
	"time"
)

// MappedSource is the non-Linux stand-in. Rigscope only ships Linux
// collector builds; this exists so the package still compiles for tooling.
type MappedSource struct {
	FS      fs.FS
	Timeout time.Duration
}

// ReadRegisters implements [RegisterSource].
func (MappedSource) ReadRegisters(context.Context, string, []int64) ([]*uint32, error) {
	return nil, ErrMappingUnsupported
}

// readHostRegisters exists so the helper entry point compiles off Linux; the
// mapping itself is unsupported there.
func readHostRegisters(string, string, []int64) ([]*uint32, error) {
	return nil, ErrMappingUnsupported
}

// ProbeRegisterAccess exists so the CLI compiles off Linux. PCI register
// access is only supported on Linux.
func ProbeRegisterAccess(string) error {
	return ErrMappingUnsupported
}
