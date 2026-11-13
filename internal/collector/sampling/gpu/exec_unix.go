//go:build unix

package gpu

import (
	"os/exec"
	"syscall"
)

// configureChildProcess gives a command its own process group and makes the
// context cancellation kill that whole group.
//
// The command helpers here may inherit stdout/stderr and outlive their parent
// shell. Killing only the direct child leaves those grandchildren holding the
// pipes, which is what makes an exec deadline ineffective; a group kill
// reaches all of them instead.
func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
}

// killProcessGroup SIGKILLs the child's process group. It reports success when
// the group is already gone, because "nothing left to kill" is exactly the
// state cancellation wants.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return nil
}
