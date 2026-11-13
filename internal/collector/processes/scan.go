//go:build linux

package processes

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// maxDefaultScanConcurrency caps the default worker count. Process reads are
// latency-bound and a handful of workers already saturates /proc; more only
// multiply buffers and goroutines on large hosts.
const maxDefaultScanConcurrency = 4

// defaultScanConcurrency is the default worker count: min(4, GOMAXPROCS).
func defaultScanConcurrency() int {
	workers := runtime.GOMAXPROCS(0)
	if workers > maxDefaultScanConcurrency {
		workers = maxDefaultScanConcurrency
	}
	if workers < 1 {
		workers = 1
	}
	return workers
}

// Scanner reads the process table from a proc filesystem root.
//
// It is read-only and never spawns a helper: USER_HZ and the page size are
// passed in, and the clock is injectable. One Scan reads process directories
// in parallel but merges results in PID order. A Scanner keeps the previous
// scan's counters, so separate Scan calls must not overlap; Clock and Users
// may be called from several scan workers at once.
type Scanner struct {
	proc                fs.FS
	users               UserNames
	clock               func() time.Time
	clockTicksPerSecond float64
	pageSize            int64
	gpuMemoryByPID      func() map[int]int64
	maxRows             int
	concurrency         int

	previous  map[processIdentity]previousCounters
	lastStats ScanStats
}

// ScannerConfig configures a Scanner. Zero values select production defaults.
type ScannerConfig struct {
	// Proc is the proc filesystem root: os.DirFS("/proc") in production,
	// a testdata/procs tree in tests. Required.
	Proc fs.FS
	// Users resolves UIDs to names. A nil lookup still maps UID 0 to root.
	Users UserNames
	// Clock is the monotonic-capable clock for rate deltas. Default time.Now.
	Clock func() time.Time
	// ClockTicksPerSecond is USER_HZ; default 100. It is explicit so the
	// scanner never runs getconf on the hot path.
	ClockTicksPerSecond float64
	// PageSize converts stat RSS pages to bytes; default 4096.
	PageSize int64
	// GPUMemoryByPID optionally associates PIDs with GPU memory in bytes.
	// Returning nil means no GPU data; negative values are ignored.
	GPUMemoryByPID func() map[int]int64
	// MaxRows bounds the returned rows; default and hard cap maxProcessRows.
	MaxRows int
	// Concurrency is the number of process directories read in parallel;
	// default min(4, GOMAXPROCS). It only overlaps reads: results are merged
	// in PID order, so a scan stays deterministic.
	Concurrency int
}

// ScanStats describes the last completed scan.
type ScanStats struct {
	// Duration is measured with the injected clock.
	Duration time.Duration
	// Processes is the number of rows read successfully, before truncation.
	Processes int
	// Inaccessible counts processes unreadable for a reason other than exit.
	Inaccessible int
}

type processIdentity struct {
	pid        int
	startTicks uint64
}

type previousCounters struct {
	utime      int64
	stime      int64
	readBytes  *int64
	writeBytes *int64
	at         time.Time
}

// ScanOptions controls one snapshot.
type ScanOptions struct {
	// IncludeCommand reads /proc/<pid>/cmdline and fills row.Command. The rest
	// of the row is identical either way, so one shared scan can serve a
	// later policy decision.
	IncludeCommand bool
}

// NewScanner validates the configuration and returns a scanner.
func NewScanner(config ScannerConfig) (*Scanner, error) {
	if config.Proc == nil {
		return nil, errors.New("processes: ScannerConfig.Proc is required")
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	hz := config.ClockTicksPerSecond
	if !(hz > 0) || math.IsInf(hz, 0) || math.IsNaN(hz) {
		hz = 100
	}
	pageSize := config.PageSize
	if pageSize <= 0 {
		pageSize = 4096
	}
	maxRows := config.MaxRows
	if maxRows <= 0 {
		maxRows = maxProcessRows
	}
	if maxRows > maxProcessRows {
		maxRows = maxProcessRows
	}
	concurrency := config.Concurrency
	if concurrency <= 0 {
		concurrency = defaultScanConcurrency()
	}
	return &Scanner{
		proc:                config.Proc,
		users:               config.Users,
		clock:               clock,
		clockTicksPerSecond: hz,
		pageSize:            pageSize,
		gpuMemoryByPID:      config.GPUMemoryByPID,
		maxRows:             maxRows,
		concurrency:         concurrency,
		previous:            make(map[processIdentity]previousCounters),
	}, nil
}

// LastScanStats returns the stats of the last completed scan.
func (s *Scanner) LastScanStats() ScanStats {
	return s.lastStats
}

// Scan reads one process snapshot. Processes that exit during the scan are
// skipped silently; processes that cannot be read for another reason count as
// inaccessible. Only an unreadable proc root is a scan error.
//
// The context stops the scan early: workers check it between process
// directories and between the files of one process. Reads themselves are not
// interruptible, so cancellation is bounded by the slowest single read.
func (s *Scanner) Scan(ctx context.Context, options ScanOptions) (ProcessSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	started := s.clock()

	entries, err := fs.ReadDir(s.proc, ".")
	if err != nil {
		return ProcessSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return ProcessSnapshot{}, err
	}
	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		pid, ok := parsePID(entry.Name())
		if !ok {
			continue
		}
		pids = append(pids, pid)
	}
	// Deterministic input order also makes truncation ties stable.
	sort.Ints(pids)

	gpuMemory := s.readGPUMemory()

	rows := make([]ProcessRow, 0, len(pids))
	nextPrevious := make(map[processIdentity]previousCounters, len(pids))
	inaccessible := 0
	results, err := s.readProcesses(ctx, pids, options.IncludeCommand, gpuMemory)
	if err != nil {
		return ProcessSnapshot{}, err
	}
	for _, result := range results {
		switch result.state {
		case rowSkipped:
			continue
		case rowInaccessible:
			inaccessible++
			continue
		}
		rows = append(rows, result.row)
		nextPrevious[result.identity] = result.counters
	}
	// Pruning by replacement keeps memory bounded by the processes that exist
	// now, which also makes PID reuse safe.
	s.previous = nextPrevious

	totalProcesses := len(rows)
	truncated := totalProcesses > s.maxRows
	processes := rows
	if truncated {
		ranked := append([]ProcessRow(nil), rows...)
		rankRows(ranked)
		processes = ranked[:s.maxRows]
	}

	duration := s.clock().Sub(started)
	if duration < 0 {
		duration = 0
	}
	s.lastStats = ScanStats{Duration: duration, Processes: totalProcesses, Inaccessible: inaccessible}
	return ProcessSnapshot{
		TotalProcesses: totalProcesses,
		Truncated:      truncated,
		Inaccessible:   inaccessible,
		Processes:      processes,
	}, nil
}

type rowState int

const (
	rowOK rowState = iota
	rowSkipped
	rowInaccessible
)

type processResult struct {
	row      ProcessRow
	identity processIdentity
	counters previousCounters
	state    rowState
}

// readBuffer is a reusable read scratch space. Each scan worker owns one, so a
// scan allocates buffers once instead of once per file.
type readBuffer struct {
	text []byte
}

// readFile reads one file into the buffer and returns the populated prefix.
// The returned slice is valid only until the next call on the same buffer.
func (b *readBuffer) readFile(fsys fs.FS, name string) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return b.readFrom(file)
}

// readFileLimit reads at most limit bytes of one file into the buffer.
func (b *readBuffer) readFileLimit(fsys fs.FS, name string, limit int) ([]byte, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return b.readFrom(io.LimitReader(file, int64(limit)))
}

func (b *readBuffer) readFrom(reader io.Reader) ([]byte, error) {
	text := b.text[:0]
	for {
		if len(text) == cap(text) {
			grown := make([]byte, len(text), growCapacity(cap(text)))
			copy(grown, text)
			text = grown
		}
		read, err := reader.Read(text[len(text):cap(text)])
		text = text[:len(text)+read]
		if err == io.EOF {
			b.text = text
			return text, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func growCapacity(current int) int {
	if current == 0 {
		return 512
	}
	if current >= 64<<10 {
		return current + 64<<10
	}
	return current * 2
}

// readProcesses reads process directories with a bounded worker pool, like the
// prototype. Results stay in PID order because they are written to an indexed
// slice and merged by the caller in input order.
func (s *Scanner) readProcesses(
	ctx context.Context,
	pids []int,
	includeCommand bool,
	gpuMemory map[int]int64,
) ([]processResult, error) {
	results := make([]processResult, len(pids))
	if len(pids) == 0 {
		return results, nil
	}
	workers := s.concurrency
	if workers > len(pids) {
		workers = len(pids)
	}
	if workers < 1 {
		workers = 1
	}
	var cursor atomic.Int64
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer wait.Done()
			buffer := &readBuffer{}
			for {
				index := int(cursor.Add(1)) - 1
				if index >= len(pids) {
					return
				}
				if ctx.Err() != nil {
					return
				}
				results[index] = s.readProcess(ctx, buffer, pids[index], includeCommand, gpuMemory)
			}
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Scanner) readProcess(
	ctx context.Context,
	buffer *readBuffer,
	pid int,
	includeCommand bool,
	gpuMemory map[int]int64,
) processResult {
	dir := strconv.Itoa(pid)
	now := s.clock()

	statText, err := buffer.readFile(s.proc, dir+"/stat")
	if err != nil {
		return readFailure(err)
	}
	stat, ok := parseStat(pid, statText)
	if !ok {
		return processResult{state: rowInaccessible}
	}
	if ctx.Err() != nil {
		return processResult{state: rowSkipped}
	}

	statusText, err := buffer.readFile(s.proc, dir+"/status")
	if err != nil {
		return readFailure(err)
	}
	status := parseStatus(statusText)
	if ctx.Err() != nil {
		return processResult{state: rowSkipped}
	}

	var io procIO
	if text, err := buffer.readFile(s.proc, dir+"/io"); err == nil {
		io, _ = parseIO(text)
	}
	var unit *string
	if text, err := buffer.readFile(s.proc, dir+"/cgroup"); err == nil {
		if value, ok := parseCgroupUnit(text); ok {
			unit = stringPtr(truncateText(value, maxUnitLength))
		}
	}
	var command *string
	if includeCommand {
		if text, err := buffer.readFileLimit(s.proc, dir+"/cmdline", maxCommandBytes); err == nil {
			command = stringPtr(truncateText(parseCommandLine(text), maxCommandLength))
		}
	}

	previous, hasPrevious := s.previous[processIdentity{pid: pid, startTicks: uint64(stat.startTicks)}]
	elapsed := time.Duration(0)
	if hasPrevious {
		elapsed = now.Sub(previous.at)
	}

	var cpuPercent *float64
	if hasPrevious && elapsed > 0 {
		deltaTicks := stat.utime + stat.stime - previous.utime - previous.stime
		if deltaTicks >= 0 {
			value := clampFloat(float64(deltaTicks)/s.clockTicksPerSecond/elapsed.Seconds()*100, 0, 100*maxCores)
			cpuPercent = &value
		}
	}

	row := ProcessRow{
		PID:                pid,
		PPID:               stat.ppid,
		StartTicks:         stat.startTicks,
		Name:               truncateText(stat.name, maxNameLength),
		User:               s.userFor(status.uid),
		State:              truncateText(stat.state, maxStateLength),
		CPUPercent:         cpuPercent,
		MemoryRSSBytes:     s.bytesFromPages(stat.rssPages),
		GPUMemoryBytes:     gpuBytes(gpuMemory, pid),
		IOReadBytesPerSec:  rate(io.readBytes, previousBytes(previous, hasPrevious, true), elapsed),
		IOWriteBytesPerSec: rate(io.writeBytes, previousBytes(previous, hasPrevious, false), elapsed),
		Threads:            pickThreads(status.threads, stat.threads),
		Unit:               unit,
		Command:            command,
	}

	counters := previousCounters{
		utime:      stat.utime,
		stime:      stat.stime,
		readBytes:  io.readBytes,
		writeBytes: io.writeBytes,
		at:         now,
	}
	return processResult{
		row:      row,
		identity: processIdentity{pid: pid, startTicks: uint64(stat.startTicks)},
		counters: counters,
		state:    rowOK,
	}
}

func readFailure(err error) processResult {
	if vanished(err) {
		return processResult{state: rowSkipped}
	}
	return processResult{state: rowInaccessible}
}

func (s *Scanner) userFor(uid *int) *string {
	if uid == nil {
		return nil
	}
	users := s.users
	if users == nil {
		if *uid == 0 {
			return stringPtr("root")
		}
		return nil
	}
	if name, ok := users.UserName(*uid); ok {
		return stringPtr(truncateText(name, maxUserLength))
	}
	if *uid == 0 {
		return stringPtr("root")
	}
	return nil
}

// bytesFromPages converts RSS pages to bytes. The contract carries byte counts
// as safe integers, so a value that does not fit stays unknown.
func (s *Scanner) bytesFromPages(pages *int64) *int64 {
	if pages == nil {
		return nil
	}
	if *pages > math.MaxInt64/s.pageSize {
		return nil
	}
	bytes := *pages * s.pageSize
	if bytes > maxWireInteger {
		return nil
	}
	return &bytes
}

func (s *Scanner) readGPUMemory() (memory map[int]int64) {
	if s.gpuMemoryByPID == nil {
		return nil
	}
	// GPU association is best-effort: a broken source must not take the scan
	// down, matching the prototype's try/catch.
	defer func() {
		if recover() != nil {
			memory = nil
		}
	}()
	return s.gpuMemoryByPID()
}

func previousBytes(previous previousCounters, ok bool, read bool) *int64 {
	if !ok {
		return nil
	}
	if read {
		return previous.readBytes
	}
	return previous.writeBytes
}

// rate converts two cumulative counters and an elapsed interval into a
// per-second rate. A first sighting, a counter reset, or unreadable data yields
// nil rather than a fabricated zero.
func rate(current, previous *int64, elapsed time.Duration) *float64 {
	if current == nil || previous == nil || elapsed <= 0 {
		return nil
	}
	delta := *current - *previous
	if delta < 0 {
		return nil
	}
	value := clampFloat(float64(delta)/elapsed.Seconds(), 0, 1e13)
	return &value
}

func pickThreads(status, stat *int) *int {
	if status != nil {
		return status
	}
	return stat
}

// gpuBytes converts one GPU association to a wire-safe byte count. Negative or
// out-of-range values are not values.
func gpuBytes(memory map[int]int64, pid int) *int64 {
	if memory == nil {
		return nil
	}
	value, ok := memory[pid]
	if !ok || value < 0 || value > maxWireInteger {
		return nil
	}
	return &value
}

// rankRows orders rows the way the prototype truncates an oversized table:
// highest CPU first, then largest RSS, then lowest PID for a stable tie break.
func rankRows(rows []ProcessRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		if cpu(left.CPUPercent) != cpu(right.CPUPercent) {
			return cpu(left.CPUPercent) > cpu(right.CPUPercent)
		}
		if rss(left.MemoryRSSBytes) != rss(right.MemoryRSSBytes) {
			return rss(left.MemoryRSSBytes) > rss(right.MemoryRSSBytes)
		}
		return left.PID < right.PID
	})
}

func cpu(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func rss(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}

// parsePID validates one /proc directory name against the contract's pid
// bound (0..2^31 inclusive). On 32-bit builds the top value does not fit Go's
// int and is unreadable, which is harmless: Linux pid_t cannot reach it there.
func parsePID(name string) (int, bool) {
	if name == "" {
		return 0, false
	}
	for index := 0; index < len(name); index++ {
		if name[index] < '0' || name[index] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(name, 10, 64)
	if err != nil || value < 0 || value > maxPID {
		return 0, false
	}
	pid := int(value)
	if int64(pid) != value {
		return 0, false
	}
	return pid, true
}

// vanished reports whether a read error means the process went away while it
// was being read. Anything else (permissions, EISDIR, I/O) is inaccessible.
func vanished(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ESRCH) ||
		errors.Is(err, syscall.ENOTDIR)
}

func clampFloat(value, min, max float64) float64 {
	if math.IsNaN(value) {
		return min
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func stringPtr(value string) *string {
	return &value
}
