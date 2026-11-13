package gpu

import (
	"context"
	"errors"
	"os/exec"
	"sync/atomic"
	"time"
)

// killGrace bounds how long a caller waits for a killed child to be reaped
// after its deadline. A child stuck in uninterruptible kernel work never sees
// the SIGKILL; waiting past this would hold the sampler (and Poller.Wait)
// forever.
var killGrace = 250 * time.Millisecond

// errChildNotReaped marks a launch refused because the previous child has not
// been reaped yet. Spawning a replacement would stack unkillable processes
// behind a wedged device.
var errChildNotReaped = errors.New("gpu: previous child process has not been reaped")

// childProcess is one launched external command. wait reaps the child and
// reports its exit error; kill asks the operating system to terminate it. wait
// may block indefinitely when the child is stuck in uninterruptible kernel
// work, so exactly one owned reaper goroutine calls it and no caller waits for
// it without a bound.
type childProcess interface {
	wait() error
	kill()
}

// execChild is a started [exec.Cmd].
type execChild struct {
	cmd *exec.Cmd
}

func (c *execChild) wait() error { return c.cmd.Wait() }

func (c *execChild) kill() {
	if c.cmd.Process != nil {
		_ = killProcessGroup(c.cmd)
	}
}

// reapSlot is one launch permit whose lifetime ends when the child it guards
// is actually reaped, not when the caller gives up on it.
type reapSlot struct {
	held atomic.Bool
}

func (s *reapSlot) tryAcquire() bool { return s.held.CompareAndSwap(false, true) }

func (s *reapSlot) release() { s.held.Store(false) }

// reaper owns the single goroutine allowed to wait on one child.
type reaper struct {
	done chan struct{}
	proc childProcess
	err  error
}

// startReaper starts the owned wait goroutine. release runs once the child is
// actually reaped, before done closes, so a refused launch never observes a
// free slot while wait is still unwinding.
func startReaper(proc childProcess, release func()) *reaper {
	r := &reaper{done: make(chan struct{}), proc: proc}
	go func() {
		err := proc.wait()
		release()
		r.err = err
		close(r.done)
	}()
	return r
}

type reapOutcome int

const (
	reaped reapOutcome = iota
	reapTimedOut
	reapCanceled
)

// awaitReap waits for the reaper within the caller's bound. On a deadline or
// cancellation it kills the child and gives it a short grace to be reaped (the
// common case, which frees the slot). A child still stuck in the kernel is
// left to its reaper: the caller returns, the slot stays held and the next
// launch is refused rather than stacking processes.
func awaitReap(ctx context.Context, r *reaper, timeout time.Duration) (reapOutcome, error) {
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-r.done:
		return reaped, r.err
	case <-deadline:
		r.proc.kill()
		awaitReapGrace(r)
		if err := ctx.Err(); err != nil {
			return reapCanceled, err
		}
		return reapTimedOut, nil
	case <-ctx.Done():
		r.proc.kill()
		awaitReapGrace(r)
		return reapCanceled, ctx.Err()
	}
}

func awaitReapGrace(r *reaper) {
	timer := time.NewTimer(killGrace)
	defer timer.Stop()
	select {
	case <-r.done:
	case <-timer.C:
	}
}
