package gpu

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecRunnerCapturesOutput(t *testing.T) {
	stdout, stderr, err := (ExecRunner{}).Run(context.Background(), "/bin/echo",
		[]string{"hello"}, time.Second)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout) != "hello" {
		t.Fatalf("stdout = %q, want hello", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}

// TestExecRunnerBoundedWaitRefusesReplacementsWhileUnreaped is the command
// equivalent of the helper bound: a child stuck in the kernel does not hold
// the caller past its timeout, and no replacement is spawned behind it.
func TestExecRunnerBoundedWaitRefusesReplacementsWhileUnreaped(t *testing.T) {
	proc := &fakeChildProcess{release: make(chan struct{})}
	runner := ExecRunner{
		slot: &reapSlot{},
		launch: func(io.Writer, io.Writer) (childProcess, error) {
			return proc, nil
		},
	}

	started := time.Now()
	_, _, err := runner.Run(context.Background(), "nvidia-smi", nil, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("run took %s, want the caller bounded by its deadline", elapsed)
	}
	if !proc.wasKilled() {
		t.Fatal("the command was not killed at the deadline")
	}

	_, _, err = runner.Run(context.Background(), "nvidia-smi", nil, 20*time.Millisecond)
	if !errors.Is(err, errChildNotReaped) {
		t.Fatalf("second run err = %v, want a refusal while the child is unreaped", err)
	}

	close(proc.release)
	waitFor(t, 2*time.Second, func() bool {
		_, _, err := runner.Run(context.Background(), "nvidia-smi", nil, 20*time.Millisecond)
		return !errors.Is(err, errChildNotReaped)
	})
}

// TestExecRunnerHoldsTheCleanupUntilTheChildIsReaped is the nvidia-smi half of
// the pci-read guarantee: the command's device leases are released by the
// reaper, never when the caller stops waiting. On success they are released
// before the call returns; on a timeout or cancellation an unreaped child keeps
// them held, so the runtime-PM gate keeps reporting the devices busy.
func TestExecRunnerHoldsTheCleanupUntilTheChildIsReaped(t *testing.T) {
	t.Run("success releases once the child is reaped", func(t *testing.T) {
		reaped := make(chan struct{})
		close(reaped)
		runner := ExecRunner{
			slot: &reapSlot{},
			launch: func(io.Writer, io.Writer) (childProcess, error) {
				return &fakeChildProcess{release: reaped}, nil
			},
		}
		var releases int32
		if _, _, err := runner.runReaping(context.Background(), "nvidia-smi", nil,
			time.Second, func() { atomic.AddInt32(&releases, 1) }); err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := atomic.LoadInt32(&releases); got != 1 {
			t.Fatalf("releases = %d, want 1 before the call returns", got)
		}
	})

	t.Run("timeout holds until the child is reaped", func(t *testing.T) {
		proc := &fakeChildProcess{release: make(chan struct{})}
		runner := ExecRunner{
			slot: &reapSlot{},
			launch: func(io.Writer, io.Writer) (childProcess, error) {
				return proc, nil
			},
		}
		var releases int32
		_, _, err := runner.runReaping(context.Background(), "nvidia-smi", nil,
			20*time.Millisecond, func() { atomic.AddInt32(&releases, 1) })
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want a timeout", err)
		}
		if got := atomic.LoadInt32(&releases); got != 0 {
			t.Fatalf("releases = %d while the child is unreaped, want 0", got)
		}
		close(proc.release)
		waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&releases) == 1 })
	})

	t.Run("cancellation holds until the child is reaped", func(t *testing.T) {
		proc := &fakeChildProcess{release: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		runner := ExecRunner{
			slot: &reapSlot{},
			launch: func(io.Writer, io.Writer) (childProcess, error) {
				cancel()
				return proc, nil
			},
		}
		var releases int32
		_, _, err := runner.runReaping(ctx, "nvidia-smi", nil, time.Second,
			func() { atomic.AddInt32(&releases, 1) })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the caller's cancellation", err)
		}
		if got := atomic.LoadInt32(&releases); got != 0 {
			t.Fatalf("releases = %d while the child is unreaped, want 0", got)
		}
		close(proc.release)
		waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&releases) == 1 })
	})

	t.Run("a refused launch releases immediately", func(t *testing.T) {
		slot := &reapSlot{}
		if !slot.tryAcquire() {
			t.Fatal("could not hold the command slot")
		}
		defer slot.release()
		runner := ExecRunner{slot: slot}
		var releases int32
		_, _, err := runner.runReaping(context.Background(), "nvidia-smi", nil,
			time.Second, func() { atomic.AddInt32(&releases, 1) })
		if !errors.Is(err, errChildNotReaped) {
			t.Fatalf("err = %v, want the slot refusal", err)
		}
		if got := atomic.LoadInt32(&releases); got != 1 {
			t.Fatalf("releases = %d, want 1 when no child was launched", got)
		}
	})
}

// TestExecRunnerKillsTheProcessGroupOnTimeout covers the shape the deadline
// must handle: a wrapper that starts a background child which inherits the
// capture pipes. Killing only the direct child leaves that grandchild holding
// the pipes, and the poller waits far past its budget.
func TestExecRunnerKillsTheProcessGroupOnTimeout(t *testing.T) {
	dir := t.TempDir()
	shellPID := filepath.Join(dir, "shell.pid")
	childPID := filepath.Join(dir, "child.pid")
	// The script records both pids, then keeps both processes alive.
	script := "echo $$ > " + shellPID + "; sleep 30 & echo $! > " + childPID + "; wait"

	started := time.Now()
	_, _, err := (ExecRunner{}).Run(context.Background(), "/bin/sh", []string{"-c", script},
		100*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("err = nil, want the timeout failure")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took %s, want the process group killed at the deadline", elapsed)
	}
	for _, file := range []string{shellPID, childPID} {
		waitForProcessExit(t, readHelperPID(t, file))
	}
}

// TestExecRunnerReleasesWhenLaunchPanics covers the window before the reaper
// owns the child: a panic in launch must release the launch slot and the
// device leases before it propagates, so a recovered panic cannot leave the
// devices non-drainable.
func TestExecRunnerReleasesWhenLaunchPanics(t *testing.T) {
	slot := &reapSlot{}
	runner := ExecRunner{
		slot: slot,
		launch: func(io.Writer, io.Writer) (childProcess, error) {
			panic("launch exploded")
		},
	}
	var releases int32
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("launch panic was swallowed")
			}
		}()
		_, _, _ = runner.runReaping(context.Background(), "nvidia-smi", nil,
			time.Second, func() { atomic.AddInt32(&releases, 1) })
	}()
	if got := atomic.LoadInt32(&releases); got != 1 {
		t.Fatalf("lease releases = %d, want 1", got)
	}
	if !slot.tryAcquire() {
		t.Fatal("launch slot stayed held after the panic")
	}
	slot.release()
}
