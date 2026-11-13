//go:build !unix

package gpu

import "os/exec"

// configureChildProcess is the non-unix stand-in: exec's default cancellation
// kills the direct child. Rigscope only ships Linux collector builds.
func configureChildProcess(*exec.Cmd) {}
