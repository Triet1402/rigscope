//go:build linux

package runtime

import (
	"context"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/Triet1402/rigscope/internal/collector/metrics"
	"github.com/Triet1402/rigscope/internal/collector/policy"
	"github.com/Triet1402/rigscope/internal/collector/processes"
	"github.com/Triet1402/rigscope/internal/collector/sampling/gpu"
	"github.com/Triet1402/rigscope/internal/collector/sampling/procfs"
	"github.com/Triet1402/rigscope/internal/collector/state"
	"github.com/Triet1402/rigscope/internal/collector/synthetic"
	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	// minimumGPUPollInterval is the GPU polling floor. Polling nvidia-smi at the
	// collector cadence would add measurable overhead on small hosts, so a faster
	// sampling interval does not speed the GPU poller up.
	minimumGPUPollInterval = 2 * time.Second
	// clockTicksPerSecond is Linux USER_HZ, matching the procfs sampler. The
	// process scanner converts stat's starttime and CPU counters with it.
	clockTicksPerSecond = 100
	// maxGPUDiagnosticRunes bounds one sanitized GPU diagnostic line. Command
	// stderr can be arbitrarily long and may echo a whole driver trace.
	maxGPUDiagnosticRunes = 240
)

// hostPageSize is a seam for the process scanner test. RSS pages from
// /proc/<pid>/stat are kernel page-size units, not always 4 KiB.
var hostPageSize = func() int64 { return int64(os.Getpagesize()) }

// SourceFactory builds the sampling source for one run. The cleanup function
// stops any background work the source owns; it may be nil.
type SourceFactory func(identity state.Identity, sequence *Sequence) (Source, func(), error)

// filteredSource applies the local sharing policy at the one boundary every
// source passes through, real or synthetic. A source must never be trusted to
// filter its own output: synthetic mode in particular bypassed the assembler's
// policy entirely before this wrapper existed.
type filteredSource struct {
	source  Source
	sharing func() policy.Policy
}

// Next returns the record with withheld metric families replaced.
func (f *filteredSource) Next() (protocol.MetricsRecord, error) {
	record, err := f.source.Next()
	if err != nil {
		return record, err
	}
	return policy.ApplyMetrics(record, f.sharing()), nil
}

// Inventory returns the inventory with withheld families and resources removed.
func (f *filteredSource) Inventory() protocol.Inventory {
	return policy.FilterInventory(f.source.Inventory(), f.sharing())
}

// InventoryVersion reports the underlying source's inventory version.
func (f *filteredSource) InventoryVersion() int64 { return f.source.InventoryVersion() }

// Failures returns only the collection failures the local policy shares.
func (f *filteredSource) Failures() []protocol.CollectionFailure {
	return policy.FilterFailures(f.source.Failures(), f.sharing())
}

// buildSource returns the sampling source for one run: the configured real
// host sampler, or explicit synthetic mode. The returned cleanup stops any
// background work the source owns.
func (c *Collector) buildSource(ctx context.Context, identity state.Identity, sequence *Sequence) (Source, func(), error) {
	sharing := func() policy.Policy { return sharingPolicy(c.cfg.Components.Collector.Sharing) }
	if c.newSource != nil {
		source, cleanup, err := c.newSource(identity, sequence)
		if err != nil {
			return nil, nil, err
		}
		return &filteredSource{source: source, sharing: sharing}, cleanup, nil
	}
	if c.cfg.Components.Collector.Synthetic {
		sampler, err := synthetic.NewSampler(synthetic.Options{
			CollectorID: identity.CollectorID,
			RunID:       identity.RunID,
			BootID:      identity.BootID,
			Sequence:    sequence,
		})
		if err != nil {
			return nil, nil, err
		}
		c.logf(Event{Level: "info", Message: "synthetic mode: host resources are not read"})
		return &filteredSource{source: sampler, sharing: sharing}, nil, nil
	}

	clock := metrics.NewSystemClock()
	diagnostics := c.gpuDiagnosticLogger(sharing)
	gate := gpu.NewRuntimePMGate(gpu.RuntimePMGateOptions{
		Now:     metrics.MonotonicDuration(clock),
		OnEvent: diagnostics,
	})
	var sensors gpu.SensorSource
	if c.cfg.Components.Collector.Hardware.GPURegisters {
		sensors = gpu.NewTempsPoller(gpu.TempsPollerOptions{
			OnEvent: diagnostics,
		})
	}
	interval := c.cfg.Components.Collector.SamplingInterval.Value()
	if interval < minimumGPUPollInterval {
		interval = minimumGPUPollInterval
	}
	poller := gpu.NewPoller(gpu.PollerOptions{
		Interval:     interval,
		Monotonic:    metrics.MonotonicDuration(clock),
		SensorSource: sensors,
		Power: &gpu.PowerSource{
			Gate: gate,
			Devices: func() []string {
				devices := gpu.DiscoverNvidiaDisplays(gpu.HostFS(), "")
				addresses := make([]string, 0, len(devices))
				for _, device := range devices {
					addresses = append(addresses, device.BDF)
				}
				return addresses
			},
		},
		OnEvent: diagnostics,
	})
	poller.Start(ctx)

	assembler, err := metrics.NewCollector(metrics.Options{
		Identity: metrics.Identity{CollectorID: identity.CollectorID, RunID: identity.RunID, BootID: identity.BootID},
		Sequence: sequence,
		Clock:    clock,
		Proc:     metrics.NewProcFSSampler(clock, procfs.SamplerOptions{}),
		GPU:      poller,
		Gate:     gate,
		Policy:   sharing,
	})
	if err != nil {
		// The initialization-error path is still an owner of the poller: stop
		// it and join its scheduling loop and sensor read before returning, so
		// no GPU work outlives the runtime lease.
		stopPoller(poller, c.drainTimeout)
		return nil, nil, err
	}
	return &filteredSource{source: assembler, sharing: sharing}, func() {
		stopPoller(poller, c.drainTimeout)
	}, nil
}

// stopPoller stops the GPU poller and joins its scheduling loop and any
// register-temperature read, bounded by budget so a wedged hardware read
// cannot hold shutdown (or an initialization error) forever.
func stopPoller(poller *gpu.Poller, budget time.Duration) {
	poller.Stop()
	if budget <= 0 {
		budget = defaultDrainTimeout
	}
	done := make(chan struct{})
	go func() {
		poller.Wait()
		close(done)
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// ansiEscapePattern matches CSI/ANSI escape sequences before control
// characters are stripped, so a colour code cannot survive as its parameter
// text (for instance "[31m").
var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// gpuDiagnosticLogger returns the callback every GPU component logs through.
// Diagnostics are sanitized before logging and, when GPUs are withheld, only a
// generic line is emitted: PCI addresses, device names and command stderr are
// hardware identity and never reach the journal under that policy.
func (c *Collector) gpuDiagnosticLogger(sharing func() policy.Policy) func(string) {
	return func(line string) {
		c.logGPUDiagnostic(sharing(), line)
	}
}

// logGPUDiagnostic applies the sharing policy to one GPU diagnostic line.
func (c *Collector) logGPUDiagnostic(sharing policy.Policy, line string) {
	if !sharing.GPUs {
		c.logf(Event{
			Level:   "info",
			Message: "gpu diagnostics withheld by the local sharing policy",
			Fields:  []Field{F("component", "gpu")},
		})
		return
	}
	sanitized := sanitizeDiagnostic(line)
	if sanitized == "" {
		return
	}
	c.logf(Event{Level: "info", Message: sanitized, Fields: []Field{F("component", "gpu")}})
}

// sanitizeDiagnostic strips escape sequences and control characters, collapses
// whitespace and bounds the result. It is applied to every GPU diagnostic,
// including command stderr embedded by the poller.
func sanitizeDiagnostic(line string) string {
	line = ansiEscapePattern.ReplaceAllString(line, "")
	var builder strings.Builder
	written := 0
	space := false
	for _, character := range line {
		if character == '\n' || character == '\r' || character == '\t' {
			character = ' '
		}
		if unicode.IsControl(character) {
			continue
		}
		if character == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		if written >= maxGPUDiagnosticRunes {
			break
		}
		builder.WriteRune(character)
		written++
	}
	return strings.TrimSpace(builder.String())
}

// processScanner returns the process-detail scanner. Synthetic mode never
// reads the host process table, matching the prototype's mock mode.
func (c *Collector) processScanner() (processes.SnapshotScanner, error) {
	if c.newScanner != nil {
		return c.newScanner()
	}
	if c.cfg.Components.Collector.Synthetic {
		return emptyScanner{}, nil
	}
	return processes.NewScanner(c.scannerConfig())
}

// scannerConfig is the real process scanner's configuration. PageSize is read
// from the running kernel: RSS is reported in stat pages, which are not 4 KiB
// on every architecture (arm64 kernels commonly use 16 KiB).
func (c *Collector) scannerConfig() processes.ScannerConfig {
	return processes.ScannerConfig{
		Proc:                os.DirFS("/proc"),
		Clock:               time.Now,
		ClockTicksPerSecond: clockTicksPerSecond,
		PageSize:            hostPageSize(),
	}
}

// emptyScanner is the synthetic mode's process table: an honest empty
// snapshot, never a fabricated list of processes.
type emptyScanner struct{}

// Scan implements processes.SnapshotScanner.
func (emptyScanner) Scan(context.Context, processes.ScanOptions) (processes.ProcessSnapshot, error) {
	return processes.ProcessSnapshot{Processes: []processes.ProcessRow{}}, nil
}
