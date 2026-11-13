//go:build linux

package processes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// testUsers is a fixed UID lookup so tests never read the host's /etc/passwd.
type testUsers map[int]string

func (u testUsers) UserName(uid int) (string, bool) {
	name, ok := u[uid]
	return name, ok
}

var defaultTestUsers = testUsers{0: "root", 1000: "alice", 1001: "bob"}

type fixtureIO struct {
	readBytes  int64
	writeBytes int64
}

// fixtureProcess describes one synthetic /proc/<pid> directory.
type fixtureProcess struct {
	comm            string
	state           string
	ppid            int
	utime           int64
	stime           int64
	threads         int
	statusThreads   int
	starttime       int64
	rssPages        int64
	uid             int
	io              *fixtureIO
	cgroup          *string
	cmdline         *string
	omitStat        bool
	statAsDirectory bool
	omitStatus      bool
}

// writeProcTree materialises a synthetic /proc tree under root.
func writeProcTree(t *testing.T, root string, processes map[int]fixtureProcess) {
	t.Helper()
	for pid, process := range processes {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if !process.omitStat {
			if process.statAsDirectory {
				statDir := filepath.Join(dir, "stat")
				if err := os.MkdirAll(statDir, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", statDir, err)
				}
				writeFile(t, filepath.Join(statDir, "placeholder"), "not a stat file\n")
			} else {
				writeFile(t, filepath.Join(dir, "stat"), statText(pid, process))
			}
		}
		if !process.omitStatus {
			uid := process.uid
			threads := process.statusThreads
			if threads == 0 {
				threads = process.threads
			}
			if threads == 0 {
				threads = 1
			}
			status := fmt.Sprintf("Name:\t%s\nState:\t%s\nUid:\t%d\t%d\t%d\t%d\nThreads:\t%d\n",
				process.comm, stateOf(process), uid, uid, uid, uid, threads)
			writeFile(t, filepath.Join(dir, "status"), status)
		}
		if process.io != nil {
			writeFile(t, filepath.Join(dir, "io"), fmt.Sprintf(
				"rchar: 0\nread_bytes: %d\nwrite_bytes: %d\n", process.io.readBytes, process.io.writeBytes))
		}
		if process.cgroup != nil {
			writeFile(t, filepath.Join(dir, "cgroup"), *process.cgroup)
		}
		if process.cmdline != nil {
			writeFile(t, filepath.Join(dir, "cmdline"), *process.cmdline)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func stateOf(process fixtureProcess) string {
	if process.state == "" {
		return "S"
	}
	return process.state
}

// statText builds a /proc/<pid>/stat line with the same field layout the real
// kernel uses; field N after comm lives at rest[N-3].
func statText(pid int, process fixtureProcess) string {
	rest := make([]string, 50)
	for index := range rest {
		rest[index] = "0"
	}
	ppid := process.ppid
	if ppid == 0 {
		ppid = 1
	}
	threads := process.threads
	if threads == 0 {
		threads = 1
	}
	starttime := process.starttime
	if starttime == 0 {
		starttime = 1000
	}
	rssPages := process.rssPages
	if rssPages == 0 {
		rssPages = 10
	}
	rest[0] = stateOf(process)
	rest[1] = strconv.Itoa(ppid)
	rest[11] = strconv.FormatInt(process.utime, 10)
	rest[12] = strconv.FormatInt(process.stime, 10)
	rest[17] = strconv.Itoa(threads)
	rest[19] = strconv.FormatInt(starttime, 10)
	rest[21] = strconv.FormatInt(rssPages, 10)
	return fmt.Sprintf("%d (%s) %s", pid, process.comm, strings.Join(rest, " "))
}

func ptr(value string) *string {
	return &value
}

// fixturePath resolves a checked-in /proc fixture. Fixtures live at the
// repository root, shared by every package, so tests reach them from this
// package directory.
func fixturePath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "testdata", "procs"}, parts...)...)
}

func newTestScanner(t *testing.T, proc fs.FS, config ScannerConfig) *Scanner {
	t.Helper()
	config.Proc = proc
	scanner, err := NewScanner(config)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	return scanner
}

func findRow(t *testing.T, snapshot ProcessSnapshot, pid int) ProcessRow {
	t.Helper()
	for _, row := range snapshot.Processes {
		if row.PID == pid {
			return row
		}
	}
	t.Fatalf("pid %d not in snapshot (pids: %v)", pid, processIDs(snapshot))
	return ProcessRow{}
}

func processIDs(snapshot ProcessSnapshot) []int {
	ids := make([]int, len(snapshot.Processes))
	for index, row := range snapshot.Processes {
		ids[index] = row.PID
	}
	return ids
}

func TestScanReadsFixtureFields(t *testing.T) {
	scanner := newTestScanner(t, os.DirFS(fixturePath("basic")), ScannerConfig{
		Users: defaultTestUsers,
		Clock: func() time.Time { return time.Unix(0, 0) },
	})

	snapshot, err := scanner.Scan(context.Background(), ScanOptions{IncludeCommand: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if snapshot.TotalProcesses != 7 {
		t.Fatalf("totalProcesses = %d, want 7", snapshot.TotalProcesses)
	}
	if snapshot.Truncated {
		t.Fatal("basic fixture must not be truncated")
	}
	if snapshot.Inaccessible != 1 {
		t.Fatalf("inaccessible = %d, want 1", snapshot.Inaccessible)
	}
	if snapshot.Processes == nil {
		t.Fatal("processes must be an empty slice, not nil")
	}

	row := findRow(t, snapshot, 100)
	if row.Name != "my worker) (2" {
		t.Errorf("name = %q", row.Name)
	}
	if row.State != "R" {
		t.Errorf("state = %q", row.State)
	}
	if row.PPID != 1 || row.StartTicks != 1000 {
		t.Errorf("ppid/startTicks = %d/%d", row.PPID, row.StartTicks)
	}
	if row.User == nil || *row.User != "alice" {
		t.Errorf("user = %v, want alice", row.User)
	}
	if row.Threads == nil || *row.Threads != 3 {
		t.Errorf("threads = %v, want status Threads 3", row.Threads)
	}
	if row.MemoryRSSBytes == nil || *row.MemoryRSSBytes != 2*4096 {
		t.Errorf("memoryRssBytes = %v, want %d", row.MemoryRSSBytes, 2*4096)
	}
	if row.CPUPercent != nil {
		t.Errorf("first sighting cpuPercent = %v, want null", *row.CPUPercent)
	}
	if row.IOReadBytesPerSec != nil || row.IOWriteBytesPerSec != nil {
		t.Error("first sighting io rates must be null")
	}
	if row.Unit == nil || *row.Unit != "app-gnome-firefox-1234.scope" {
		t.Errorf("unit = %v", row.Unit)
	}
	if row.Command == nil || *row.Command != "node server.js --flag" {
		t.Errorf("command = %v", row.Command)
	}
	if row.GPUMemoryBytes != nil {
		t.Errorf("gpuMemoryBytes = %v, want null", *row.GPUMemoryBytes)
	}

	zombie := findRow(t, snapshot, 250)
	if zombie.State != "Z" {
		t.Errorf("zombie state = %q", zombie.State)
	}
	if zombie.User == nil || *zombie.User != "bob" {
		t.Errorf("zombie user = %v, want bob", zombie.User)
	}

	for _, pid := range []int{2, 3} {
		kernel := findRow(t, snapshot, pid)
		if kernel.User == nil || *kernel.User != "root" {
			t.Errorf("kernel thread %d user = %v, want root", pid, kernel.User)
		}
	}

	long := findRow(t, snapshot, 800)
	if long.Command == nil || len(*long.Command) != maxCommandLength {
		t.Fatalf("long command length = %v, want %d", long.Command, maxCommandLength)
	}
	if strings.Trim(*long.Command, "a") != "" {
		t.Error("long command must stay all 'a'")
	}
	simple := findRow(t, snapshot, 801)
	if simple.Command == nil || *simple.Command != "echo hi" {
		t.Errorf("simple command = %v, want %q", simple.Command, "echo hi")
	}
	if simple.User == nil || *simple.User != "bob" {
		t.Errorf("simple user = %v, want bob", simple.User)
	}

	stats := scanner.LastScanStats()
	if stats.Duration != 0 || stats.Processes != 7 || stats.Inaccessible != 1 {
		t.Errorf("stats = %+v", stats)
	}

	// The command field is absent unless explicitly requested.
	withoutCommand, err := scanner.Scan(context.Background(), ScanOptions{IncludeCommand: false})
	if err != nil {
		t.Fatalf("Scan without command: %v", err)
	}
	if findRow(t, withoutCommand, 100).Command != nil {
		t.Error("command must be absent when not requested")
	}
}

func TestScanCPUAndIORatesUseMonotonicDeltas(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(0, 0)
	processes := map[int]fixtureProcess{
		200: {
			comm: "worker", utime: 100, stime: 50, starttime: 5000,
			io: &fixtureIO{readBytes: 1000, writeBytes: 2000},
		},
	}
	writeProcTree(t, root, processes)
	scanner := newTestScanner(t, os.DirFS(root), ScannerConfig{
		Users: defaultTestUsers,
		Clock: func() time.Time { return now },
	})

	first, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	row := findRow(t, first, 200)
	if row.CPUPercent != nil || row.IOReadBytesPerSec != nil || row.IOWriteBytesPerSec != nil {
		t.Fatal("first sighting must not invent rates")
	}

	now = now.Add(time.Second)
	processes[200] = fixtureProcess{
		comm: "worker", utime: 150, stime: 50, starttime: 5000,
		io: &fixtureIO{readBytes: 3000, writeBytes: 1000},
	}
	writeProcTree(t, root, processes)
	second, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	row = findRow(t, second, 200)
	if row.CPUPercent == nil || *row.CPUPercent != 50 {
		t.Errorf("cpuPercent = %v, want 50 (50 ticks / 100 Hz over 1s)", row.CPUPercent)
	}
	if row.IOReadBytesPerSec == nil || *row.IOReadBytesPerSec != 2000 {
		t.Errorf("ioReadBytesPerSec = %v, want 2000", row.IOReadBytesPerSec)
	}
	if row.IOWriteBytesPerSec != nil {
		t.Errorf("a counter decrease is not a rate, got %v", *row.IOWriteBytesPerSec)
	}

	now = now.Add(time.Second)
	third, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("third scan: %v", err)
	}
	row = findRow(t, third, 200)
	if row.CPUPercent == nil || *row.CPUPercent != 0 {
		t.Errorf("stable counter cpuPercent = %v, want an honest zero", row.CPUPercent)
	}
	if row.IOReadBytesPerSec == nil || *row.IOReadBytesPerSec != 0 {
		t.Errorf("stable read counter = %v, want 0", row.IOReadBytesPerSec)
	}
	if row.IOWriteBytesPerSec == nil || *row.IOWriteBytesPerSec != 0 {
		t.Errorf("stable write counter = %v, want 0", row.IOWriteBytesPerSec)
	}
}

func TestScanPIDReuseNeverProducesADelta(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(0, 0)
	processes := map[int]fixtureProcess{
		300: {
			comm: "reused", utime: 100, starttime: 100,
			io: &fixtureIO{readBytes: 1000, writeBytes: 1000},
		},
	}
	writeProcTree(t, root, processes)
	scanner := newTestScanner(t, os.DirFS(root), ScannerConfig{Clock: func() time.Time { return now }})
	if _, err := scanner.Scan(context.Background(), ScanOptions{}); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	now = now.Add(time.Second)
	processes[300] = fixtureProcess{
		comm: "reused", utime: 200, starttime: 200,
		io: &fixtureIO{readBytes: 5000, writeBytes: 5000},
	}
	writeProcTree(t, root, processes)
	reused, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("reuse scan: %v", err)
	}
	row := findRow(t, reused, 300)
	if row.CPUPercent != nil || row.IOReadBytesPerSec != nil {
		t.Fatal("a new startTicks is a new identity and must not produce a delta")
	}

	now = now.Add(time.Second)
	processes[300] = fixtureProcess{
		comm: "reused", utime: 250, starttime: 200,
		io: &fixtureIO{readBytes: 6000, writeBytes: 6000},
	}
	writeProcTree(t, root, processes)
	reusedDelta, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("delta scan: %v", err)
	}
	row = findRow(t, reusedDelta, 300)
	if row.CPUPercent == nil || *row.CPUPercent != 50 {
		t.Errorf("cpuPercent = %v, want 50", row.CPUPercent)
	}
	if row.IOReadBytesPerSec == nil || *row.IOReadBytesPerSec != 1000 {
		t.Errorf("ioReadBytesPerSec = %v, want 1000", row.IOReadBytesPerSec)
	}

	// A vanished identity is pruned, so the same pid+startTicks reappearing
	// later is a first sighting again.
	if err := os.RemoveAll(filepath.Join(root, "300")); err != nil {
		t.Fatal(err)
	}
	empty, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("empty scan: %v", err)
	}
	if empty.TotalProcesses != 0 {
		t.Fatalf("totalProcesses = %d, want 0", empty.TotalProcesses)
	}

	now = now.Add(time.Second)
	processes[300] = fixtureProcess{
		comm: "reused", utime: 300, starttime: 200,
		io: &fixtureIO{readBytes: 7000, writeBytes: 7000},
	}
	writeProcTree(t, root, processes)
	reappeared, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("reappeared scan: %v", err)
	}
	row = findRow(t, reappeared, 300)
	if row.CPUPercent != nil || row.IOReadBytesPerSec != nil {
		t.Fatal("a reappearing process is a first sighting")
	}
}

func TestScanSkipsVanishedAndCountsInaccessible(t *testing.T) {
	scanner := newTestScanner(t, os.DirFS(fixturePath("basic")), ScannerConfig{
		Users: defaultTestUsers,
		Clock: func() time.Time { return time.Unix(0, 0) },
	})
	snapshot, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if snapshot.TotalProcesses != 7 || snapshot.Inaccessible != 1 {
		t.Fatalf("total/inaccessible = %d/%d, want 7/1", snapshot.TotalProcesses, snapshot.Inaccessible)
	}
	for _, pid := range []int{400, 401} {
		for _, row := range snapshot.Processes {
			if row.PID == pid {
				t.Fatalf("pid %d must not appear in the snapshot", pid)
			}
		}
	}
	if row := findRow(t, snapshot, 402); row.IOReadBytesPerSec != nil || row.IOWriteBytesPerSec != nil {
		t.Error("a missing io file is not inaccessible and yields null rates")
	}
	if scanner.LastScanStats().Inaccessible != 1 {
		t.Error("LastScanStats must report the inaccessible count")
	}
}

// hideFS disappears one path after the directory listing, simulating a process
// that exits between two reads of the same scan.
type hideFS struct {
	fs.FS
	path string
}

func (h hideFS) Open(name string) (fs.File, error) {
	if name == h.path {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return h.FS.Open(name)
}

func TestScanHandlesAProcessThatExitsBetweenReads(t *testing.T) {
	scanner := newTestScanner(t, hideFS{FS: os.DirFS(fixturePath("basic")), path: "402/status"}, ScannerConfig{
		Users: defaultTestUsers,
		Clock: func() time.Time { return time.Unix(0, 0) },
	})
	snapshot, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if snapshot.TotalProcesses != 6 {
		t.Errorf("totalProcesses = %d, want 6", snapshot.TotalProcesses)
	}
	if snapshot.Inaccessible != 1 {
		t.Errorf("inaccessible = %d, want 1 (only the EISDIR stat)", snapshot.Inaccessible)
	}
	for _, row := range snapshot.Processes {
		if row.PID == 402 {
			t.Fatal("a process that exited between reads is skipped, not reported")
		}
	}
}

func TestScanCgroupUnitsAndGPUMemory(t *testing.T) {
	root := t.TempDir()
	processes := map[int]fixtureProcess{
		600: {comm: "firefox", cgroup: ptr("0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-gnome-firefox-1234.scope\n")},
		601: {comm: "worker", cgroup: ptr("12:pids:/foo.slice\n0::/foo.slice/bar.slice\n")},
		602: {comm: "service", cgroup: ptr("0::/system.slice/example.service\n")},
		603: {comm: "none"},
	}
	writeProcTree(t, root, processes)
	scanner := newTestScanner(t, os.DirFS(root), ScannerConfig{
		Clock:          func() time.Time { return time.Unix(0, 0) },
		GPUMemoryByPID: func() map[int]int64 { return map[int]int64{600: 1234, 601: -1} },
	})
	snapshot, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if row := findRow(t, snapshot, 600); row.Unit == nil || *row.Unit != "app-gnome-firefox-1234.scope" {
		t.Errorf("600 unit = %v", row.Unit)
	}
	if row := findRow(t, snapshot, 600); row.GPUMemoryBytes == nil || *row.GPUMemoryBytes != 1234 {
		t.Errorf("600 gpuMemoryBytes = %v", row.GPUMemoryBytes)
	}
	if row := findRow(t, snapshot, 601); row.Unit != nil {
		t.Errorf("a slice leaf is not a unit, got %v", *row.Unit)
	}
	if row := findRow(t, snapshot, 601); row.GPUMemoryBytes != nil {
		t.Errorf("negative GPU memory is not a value, got %v", *row.GPUMemoryBytes)
	}
	if row := findRow(t, snapshot, 602); row.Unit == nil || *row.Unit != "example.service" {
		t.Errorf("602 unit = %v", row.Unit)
	}
	if row := findRow(t, snapshot, 603); row.Unit != nil {
		t.Errorf("603 unit = %v, want null", *row.Unit)
	}
}

// utf16Length counts JavaScript String#length units, which is what the wire
// contract's text limits use.
func utf16Length(value string) int {
	units := 0
	for _, r := range value {
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// TestScanCapsTextFieldsInUTF16Units covers the fields the protocol caps: a
// limit counts UTF-16 code units and never splits a character. The whole
// snapshot must then pass the real protocol validator.
func TestScanCapsTextFieldsInUTF16Units(t *testing.T) {
	root := t.TempDir()
	astral := "😀"
	longUser := strings.Repeat("b", 70)
	longUnitPath := "0::/system.slice/" + strings.Repeat("é", 260) + ".service\n"
	longState := "STATERUNNINGFOREVER"
	processes := map[int]fixtureProcess{
		900: {
			comm:    strings.Repeat("a", maxNameLength-1) + astral,
			uid:     1000,
			cgroup:  ptr(longUnitPath),
			state:   longState,
			cmdline: ptr(strings.Repeat(astral, 3000)),
		},
		901: {
			comm:    "plain",
			cmdline: ptr("node\x00server.js\x00"),
		},
	}
	writeProcTree(t, root, processes)
	users := testUsers{1000: longUser}
	scanner := newTestScanner(t, os.DirFS(root), ScannerConfig{
		Users: users,
		Clock: func() time.Time { return time.Unix(0, 0) },
	})
	snapshot, err := scanner.Scan(context.Background(), ScanOptions{IncludeCommand: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	row := findRow(t, snapshot, 900)
	if want := maxNameLength - 1; utf16Length(row.Name) != want {
		t.Errorf("name units = %d, want %d (the astral character must not be split)", utf16Length(row.Name), want)
	}
	if !utf8.ValidString(row.Name) {
		t.Errorf("name is not valid UTF-8: %q", row.Name)
	}
	if ceiling := maxCommandLength; utf16Length(derefString(row.Command)) != ceiling {
		t.Errorf("command units = %d, want %d", utf16Length(derefString(row.Command)), ceiling)
	}
	if want := strings.Repeat(astral, maxCommandLength/2); derefString(row.Command) != want {
		t.Error("command must be a whole number of astral characters")
	}
	if want := maxUnitLength; utf16Length(derefString(row.Unit)) != want {
		t.Errorf("unit units = %d, want %d", utf16Length(derefString(row.Unit)), want)
	}
	if want := maxUserLength; utf16Length(derefString(row.User)) != want {
		t.Errorf("user units = %d, want %d", utf16Length(derefString(row.User)), want)
	}
	if want := maxStateLength; utf16Length(row.State) != want {
		t.Errorf("state units = %d, want %d", utf16Length(row.State), want)
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if _, err := protocol.ParseProcessSnapshot(encoded); err != nil {
		t.Fatalf("the scanner produced a protocol-invalid snapshot: %v", err)
	}

	result := DetailResult{
		Type:           "detail.result",
		SubscriptionID: "sub-1",
		CapturedAt:     time.Now().UTC().Format(capturedAtLayout),
		Snapshot:       snapshot,
	}
	encoded, err = json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if _, err := protocol.DecodeCollectorMessage(encoded); err != nil {
		t.Fatalf("the scanner produced a protocol-invalid detail result: %v", err)
	}
}

// TestScanEnforcesWireIntegerBounds covers the identity/optional integer
// bounds: an unreadable identity is skipped, an optional value that cannot be
// carried becomes unknown rather than clamped.
func TestScanEnforcesWireIntegerBounds(t *testing.T) {
	root := t.TempDir()
	processes := map[int]fixtureProcess{
		// startTicks beyond 2^53-1 cannot be an identity on the wire.
		910: {comm: "bad-identity", starttime: protocol.MaxSafeInteger + 1},
		// RSS in bytes beyond 2^53-1 stays unknown.
		911: {comm: "huge-rss", starttime: 100, rssPages: protocol.MaxSafeInteger/4096 + 1},
		// A page count that overflows int64 during the byte conversion too.
		912: {comm: "overflow-rss", starttime: 101, rssPages: maxInt64/4096 + 1},
		// The largest carryable startTicks is still a row.
		913: {comm: "max-start", starttime: protocol.MaxSafeInteger},
	}
	writeProcTree(t, root, processes)
	scanner := newTestScanner(t, os.DirFS(root), ScannerConfig{
		Clock: func() time.Time { return time.Unix(0, 0) },
		GPUMemoryByPID: func() map[int]int64 {
			return map[int]int64{911: protocol.MaxSafeInteger + 1, 913: protocol.MaxSafeInteger}
		},
	})
	snapshot, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if snapshot.TotalProcesses != 3 {
		t.Errorf("totalProcesses = %d, want 3 (the unreadable identity is skipped)", snapshot.TotalProcesses)
	}
	if snapshot.Inaccessible != 1 {
		t.Errorf("inaccessible = %d, want 1", snapshot.Inaccessible)
	}
	for _, row := range snapshot.Processes {
		if row.PID == 910 {
			t.Fatal("a row whose startTicks exceeds the safe integer range must not appear")
		}
	}
	if row := findRow(t, snapshot, 911); row.MemoryRSSBytes != nil {
		t.Errorf("out-of-range RSS = %v, want null", *row.MemoryRSSBytes)
	}
	if row := findRow(t, snapshot, 911); row.GPUMemoryBytes != nil {
		t.Errorf("out-of-range GPU memory = %v, want null", *row.GPUMemoryBytes)
	}
	if row := findRow(t, snapshot, 912); row.MemoryRSSBytes != nil {
		t.Errorf("overflowing RSS = %v, want null", *row.MemoryRSSBytes)
	}
	maxRow := findRow(t, snapshot, 913)
	if maxRow.StartTicks != protocol.MaxSafeInteger {
		t.Errorf("startTicks = %d, want the maximum safe integer", maxRow.StartTicks)
	}
	if maxRow.GPUMemoryBytes == nil || *maxRow.GPUMemoryBytes != protocol.MaxSafeInteger {
		t.Errorf("max GPU memory = %v, want %d", maxRow.GPUMemoryBytes, protocol.MaxSafeInteger)
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := protocol.ParseProcessSnapshot(encoded); err != nil {
		t.Fatalf("out-of-range values leaked into the wire shape: %v", err)
	}
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// Ported from tests/processes-scan.test.ts: truncation keeps the highest CPU
// then RSS rows and reports the full count. The initial start times are
// 7000+pid, so the second scan sees the same process identities and computes a
// CPU delta rather than treating every row as a first sighting.
func TestScanTruncatesByCPUThenRSSThenPID(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(0, 0)
	processes := map[int]fixtureProcess{}
	for _, pid := range []int{700, 701, 702} {
		processes[pid] = fixtureProcess{comm: fmt.Sprintf("p%d", pid), starttime: int64(7000 + pid), rssPages: int64(pid - 700 + 1)}
	}
	writeProcTree(t, root, processes)
	scanner := newTestScanner(t, os.DirFS(root), ScannerConfig{
		Clock:   func() time.Time { return now },
		MaxRows: 2,
	})
	if _, err := scanner.Scan(context.Background(), ScanOptions{}); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	now = now.Add(time.Second)
	for pid, utime := range map[int]int64{700: 10, 701: 30, 702: 20} {
		processes[pid] = fixtureProcess{comm: fmt.Sprintf("p%d", pid), starttime: int64(7000 + pid), rssPages: int64(pid - 700 + 1), utime: utime}
	}
	writeProcTree(t, root, processes)
	snapshot, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if snapshot.TotalProcesses != 3 || !snapshot.Truncated {
		t.Fatalf("total/truncated = %d/%v, want 3/true", snapshot.TotalProcesses, snapshot.Truncated)
	}
	if got := processIDs(snapshot); !equalInts(got, []int{701, 702}) {
		t.Fatalf("truncated pids = %v, want [701 702]", got)
	}
	if row := findRow(t, snapshot, 701); row.CPUPercent == nil || *row.CPUPercent != 30 {
		t.Errorf("701 cpuPercent = %v, want 30", row.CPUPercent)
	}
	if row := findRow(t, snapshot, 702); row.CPUPercent == nil || *row.CPUPercent != 20 {
		t.Errorf("702 cpuPercent = %v, want 20", row.CPUPercent)
	}
}

func TestScanHugeTableRespectsTheRowLimit(t *testing.T) {
	root := materializeHugeTree(t)
	scanner := newTestScanner(t, os.DirFS(root), ScannerConfig{
		Clock: func() time.Time { return time.Unix(0, 0) },
	})
	snapshot, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if snapshot.TotalProcesses != 6010 {
		t.Errorf("totalProcesses = %d, want 6010", snapshot.TotalProcesses)
	}
	if !snapshot.Truncated {
		t.Error("snapshot must be truncated")
	}
	if len(snapshot.Processes) != maxProcessRows {
		t.Errorf("rows = %d, want %d", len(snapshot.Processes), maxProcessRows)
	}
	if scanner.LastScanStats().Processes != 6010 {
		t.Errorf("stats processes = %d, want 6010", scanner.LastScanStats().Processes)
	}
}

// materializeHugeTree expands testdata/procs/huge/pids.txt into a proc tree
// large enough to exceed the protocol row limit. A checked-in pids list keeps
// the fixture small instead of committing six thousand directories.
func materializeHugeTree(t *testing.T) string {
	t.Helper()
	text, err := os.ReadFile(fixturePath("huge", "pids.txt"))
	if err != nil {
		t.Fatalf("read huge fixture: %v", err)
	}
	root := t.TempDir()
	lines := strings.Fields(string(text))
	if len(lines) <= maxProcessRows {
		t.Fatalf("huge fixture has %d pids, need more than %d", len(lines), maxProcessRows)
	}
	for _, line := range lines {
		pid, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("invalid pid %q: %v", line, err)
		}
		dir := filepath.Join(root, line)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		writeFile(t, filepath.Join(dir, "stat"), statText(pid, fixtureProcess{comm: "synthetic", starttime: int64(pid)}))
		writeFile(t, filepath.Join(dir, "status"),
			fmt.Sprintf("Name:\tsynthetic\nUid:\t0\t0\t0\t0\nThreads:\t1\n"))
	}
	return root
}

func TestPasswdFileParsesAndRecovers(t *testing.T) {
	root := t.TempDir()
	passwdPath := filepath.Join(root, "passwd")
	writeFile(t, passwdPath, strings.Join([]string{
		"root:x:0:0:root:/root:/bin/bash",
		"# a comment",
		"alice:x:1000:1000:Alice:/home/alice:/bin/bash",
		"alias:x:1000:1000:Alias:/home/alias:/bin/sh",
		"bob:x:1001:1001:Bob:/home/bob:/bin/bash",
		"broken line",
	}, "\n"))

	users := NewPasswdFile(os.DirFS(root), "passwd")
	if name, ok := users.UserName(1000); !ok || name != "alice" {
		t.Errorf("uid 1000 = %q/%v, want alice (first entry wins)", name, ok)
	}
	if name, ok := users.UserName(1001); !ok || name != "bob" {
		t.Errorf("uid 1001 = %q/%v", name, ok)
	}
	if name, ok := users.UserName(0); !ok || name != "root" {
		t.Errorf("uid 0 = %q/%v", name, ok)
	}
	if name, ok := users.UserName(9999); ok {
		t.Errorf("unknown uid = %q/%v, want absent", name, ok)
	}

	// A lookup that could not read the file still maps root, and a later
	// successful read is not poisoned by the earlier failure.
	missing := NewPasswdFile(os.DirFS(root), "missing")
	if name, ok := missing.UserName(0); !ok || name != "root" {
		t.Errorf("root fallback = %q/%v", name, ok)
	}
	if _, ok := missing.UserName(1000); ok {
		t.Error("an unreadable passwd must not invent users")
	}
	missing.path = "passwd"
	if name, ok := missing.UserName(1000); !ok || name != "alice" {
		t.Errorf("recovered lookup = %q/%v", name, ok)
	}
}

func TestNewScannerValidation(t *testing.T) {
	if _, err := NewScanner(ScannerConfig{}); err == nil {
		t.Fatal("a missing Proc must be an error")
	}
	scanner, err := NewScanner(ScannerConfig{
		Proc:     os.DirFS(t.TempDir()),
		MaxRows:  999_999,
		PageSize: -1,
	})
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	if scanner.maxRows != maxProcessRows {
		t.Errorf("maxRows = %d, want hard cap %d", scanner.maxRows, maxProcessRows)
	}
	if scanner.pageSize != 4096 {
		t.Errorf("pageSize = %d, want 4096", scanner.pageSize)
	}
	if scanner.concurrency != defaultScanConcurrency() {
		t.Errorf("concurrency = %d, want %d", scanner.concurrency, defaultScanConcurrency())
	}
	if scanner.concurrency > 4 {
		t.Errorf("default concurrency = %d, want at most 4", scanner.concurrency)
	}
	explicit, err := NewScanner(ScannerConfig{Proc: os.DirFS(t.TempDir()), Concurrency: 7})
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	if explicit.concurrency != 7 {
		t.Errorf("explicit concurrency = %d, want 7", explicit.concurrency)
	}

	missingRoot := filepath.Join(t.TempDir(), "does-not-exist")
	missingScanner, err := NewScanner(ScannerConfig{Proc: os.DirFS(missingRoot)})
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	if _, err := missingScanner.Scan(context.Background(), ScanOptions{}); err == nil {
		t.Fatal("scanning a missing proc root must error")
	}
}

func TestScanVanishedErrorClassification(t *testing.T) {
	cases := []struct {
		err      error
		vanished bool
	}{
		{&fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}, true},
		{&fs.PathError{Op: "open", Path: "x", Err: errors.New("permission denied")}, false},
	}
	for _, testCase := range cases {
		if got := vanished(testCase.err); got != testCase.vanished {
			t.Errorf("vanished(%v) = %v, want %v", testCase.err, got, testCase.vanished)
		}
	}
}

func TestScanHonoursACancelledContext(t *testing.T) {
	scanner := newTestScanner(t, os.DirFS(fixturePath("basic")), ScannerConfig{
		Users: defaultTestUsers,
		Clock: func() time.Time { return time.Unix(0, 0) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scanner.Scan(ctx, ScanOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan error = %v, want context.Canceled", err)
	}

	// A nil context is tolerated rather than panicking.
	if _, err := scanner.Scan(nil, ScanOptions{}); err != nil {
		t.Fatalf("Scan with a nil context: %v", err)
	}
}

func TestScanRealProcSmoke(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc on this host")
	}
	scanner := newTestScanner(t, os.DirFS("/proc"), ScannerConfig{
		Users: NewPasswdFile(os.DirFS("/etc"), "passwd"),
	})
	first, err := scanner.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("first real scan: %v", err)
	}
	if first.TotalProcesses == 0 {
		t.Fatal("a real /proc must contain at least one process")
	}
	if first.Processes == nil {
		t.Fatal("processes must be a slice")
	}
	for _, row := range first.Processes {
		if row.PID < 0 || row.Name == "" || row.State == "" {
			t.Fatalf("contract-invalid row: %+v", row)
		}
	}
	second, err := scanner.Scan(context.Background(), ScanOptions{IncludeCommand: true})
	if err != nil {
		t.Fatalf("second real scan: %v", err)
	}
	if second.TotalProcesses == 0 {
		t.Fatal("second real scan returned no processes")
	}
	if scanner.LastScanStats().Duration < 0 {
		t.Fatal("scan duration must not be negative")
	}
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// BenchmarkScan2000Processes measures a full /proc-style scan with the same
// files a real host exposes for an ordinary process.
func BenchmarkScan2000Processes(b *testing.B) {
	root := b.TempDir()
	processCount := 2000
	for index := 0; index < processCount; index++ {
		pid := 1000 + index
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		files := map[string]string{
			"stat":   statText(pid, fixtureProcess{comm: "benchmark", utime: int64(index), starttime: int64(pid), rssPages: 100}),
			"status": fmt.Sprintf("Name:\tbenchmark\nUid:\t%d\t%d\t%d\t%d\nThreads:\t4\n", 1000+index%5, 1000+index%5, 1000+index%5, 1000+index%5),
			"io":     "rchar: 0\nread_bytes: 1000\nwrite_bytes: 2000\n",
			"cgroup": "0::/system.slice/benchmark.service\n",
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				b.Fatal(err)
			}
		}
	}
	users := make(testUsers)
	for uid := 1000; uid < 1005; uid++ {
		users[uid] = "user" + strconv.Itoa(uid)
	}
	scanner, err := NewScanner(ScannerConfig{
		Proc:  os.DirFS(root),
		Users: users,
		Clock: func() time.Time { return time.Now() },
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		snapshot, err := scanner.Scan(context.Background(), ScanOptions{IncludeCommand: false})
		if err != nil {
			b.Fatal(err)
		}
		if snapshot.TotalProcesses != processCount {
			b.Fatalf("totalProcesses = %d, want %d", snapshot.TotalProcesses, processCount)
		}
	}
}
