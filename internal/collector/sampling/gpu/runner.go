package gpu

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"sync"
	"time"
)

// maxCommandOutput bounds one nvidia-smi invocation, like the prototype's
// 4 MiB exec maxBuffer.
const maxCommandOutput = 4 * 1024 * 1024

// commandWaitDelay bounds how long Run waits for inherited pipes after the
// command has exited or been killed. A query that forks a helper holding
// stdout must not keep the poller past its timeout.
const commandWaitDelay = 250 * time.Millisecond

// Runner runs one external command with a per-invocation timeout. It exists
// so tests can feed canned nvidia-smi output and so the poller never reaches
// for exec directly. Implementations must return whatever output they did
// capture alongside the error.
type Runner interface {
	Run(ctx context.Context, command string, args []string, timeout time.Duration) (stdout string, stderr string, err error)
}

// reapObserver is the optional [Runner] extension that lets the caller tie
// device leases to the child's lifetime. runReaping behaves exactly like Run
// but calls onReap only once the launched child has actually been reaped — on
// success, timeout and cancellation alike — or immediately when no child was
// launched. A runner that does not implement it falls back to Run and the
// caller releases when the call returns.
type reapObserver interface {
	runReaping(ctx context.Context, command string, args []string, timeout time.Duration, onReap func()) (stdout string, stderr string, err error)
}

// ExecRunner runs a real process. The timeout also kills it — the whole
// process group, so a wrapper script's children die too — and a bounded
// [exec.Cmd.WaitDelay] stops inherited pipes from outliving the deadline.
//
// Waiting is bounded for the same reason as the PCI helper: a command stuck in
// uninterruptible kernel work must not hold the poller. One owned reaper calls
// Wait, and while a previous command has not been reaped, a new launch is
// refused instead of stacking processes.
type ExecRunner struct {
	// launch is a test seam: it starts the child and returns it. nil starts a
	// real process.
	launch func(stdout, stderr io.Writer) (childProcess, error)
	// slot is a test seam: the launch permit. nil uses the process-wide
	// command slot.
	slot *reapSlot
}

// commandSlot keeps at most one nvidia-smi child unreaped for the process.
var commandSlot = &reapSlot{}

// Run implements [Runner].
func (r ExecRunner) Run(ctx context.Context, command string, args []string, timeout time.Duration) (string, string, error) {
	return r.runReaping(ctx, command, args, timeout, nil)
}

// runReaping is [ExecRunner.Run] with an ownership hook for the device leases
// the command holds. The hook runs only after the child has been reaped; a
// caller that stopped waiting for a stuck child therefore keeps the devices
// non-drainable, and the runtime-PM gate keeps reporting them busy.
func (r ExecRunner) runReaping(ctx context.Context, command string, args []string, timeout time.Duration, onReap func()) (string, string, error) {
	stdout := &cappedBuffer{limit: maxCommandOutput}
	stderr := &cappedBuffer{limit: maxCommandOutput}
	var once sync.Once
	reap := func() {
		if onReap != nil {
			once.Do(onReap)
		}
	}
	if err := ctx.Err(); err != nil {
		reap()
		return "", "", err
	}
	slot := r.slot
	if slot == nil {
		slot = commandSlot
	}
	if !slot.tryAcquire() {
		reap()
		return "", "", errChildNotReaped
	}
	launch := r.launch
	if launch == nil {
		launch = func(stdout, stderr io.Writer) (childProcess, error) {
			// CommandContext is used only so configureChildProcess can install
			// the process-group kill; the deadline is enforced by awaitReap.
			cmd := exec.CommandContext(context.Background(), command, args...)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			cmd.WaitDelay = commandWaitDelay
			configureChildProcess(cmd)
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			return &execChild{cmd: cmd}, nil
		}
	}
	proc, err := launchOwned(launch, stdout, stderr, func() {
		slot.release()
		reap()
	})
	if err != nil {
		slot.release()
		reap()
		return "", stderr.String(), err
	}
	outcome, waitErr := awaitReap(ctx, startReaper(proc, func() {
		slot.release()
		reap()
	}), timeout)
	if stdout.Overflowed() || stderr.Overflowed() {
		return "", stderr.String(), fmt.Errorf("gpu: %s output exceeded %d bytes", command, maxCommandOutput)
	}
	switch outcome {
	case reapTimedOut:
		return stdout.String(), stderr.String(),
			fmt.Errorf("gpu: %s timed out after %s", command, timeout)
	case reapCanceled:
		return stdout.String(), stderr.String(), waitErr
	}
	return stdout.String(), stderr.String(), waitErr
}

// launchOwned runs launch while the caller still owns the launch slot and the
// device leases. Until startReaper takes them over, a panic in launch must not
// orphan them: cleanup runs, then the panic continues to the caller's recover.
func launchOwned(launch func(stdout, stderr io.Writer) (childProcess, error), stdout, stderr io.Writer, cleanup func()) (proc childProcess, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cleanup()
			panic(recovered)
		}
	}()
	return launch(stdout, stderr)
}

// NotFoundError marks "the command is not installed" without depending on
// the platform's exec error values. Runners may wrap [fs.ErrNotExist]
// instead; the poller treats both as absent tooling.
func NotFoundError(command string) error {
	return fmt.Errorf("gpu: %s: %w", command, fs.ErrNotExist)
}

// cappedBuffer collects command output up to a limit and records overflow so
// the caller can fail the invocation instead of buffering without bound.
type cappedBuffer struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.overflow = true
		return len(data), nil
	}
	if len(data) > remaining {
		b.overflow = true
		data = data[:remaining]
	}
	b.buffer.Write(data)
	return len(data), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// Overflowed reports whether a write was truncated. It is read while the
// child's output goroutine may still be writing, so it takes the same lock as
// Write.
func (b *cappedBuffer) Overflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.overflow
}

// Bytes returns the collected bytes as a copy-free slice of the buffer.
func (b *cappedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Bytes()
}
