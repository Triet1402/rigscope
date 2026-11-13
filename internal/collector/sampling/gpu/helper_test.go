package gpu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The helper tests re-execute the test binary: TestMain notices the mode
// environment variable and runs the behaviour instead of the test suite, the
// same way the collector re-executes itself as `rigscope internal pci-read`.
const (
	helperModeEnv    = "RIGSCOPE_GPU_HELPER_TEST"
	helperPIDFileEnv = "RIGSCOPE_GPU_HELPER_PID_FILE"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		os.Exit(runHelperTestMode(mode))
	}
	os.Exit(m.Run())
}

func runHelperTestMode(mode string) int {
	switch mode {
	case "roundtrip":
		return RunPCIReadHelper(os.Stdin, os.Stdout, os.Stderr)
	case "sleep":
		if path := os.Getenv(helperPIDFileEnv); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		time.Sleep(30 * time.Second)
		return 0
	case "malformed":
		fmt.Fprint(os.Stdout, "{not json")
		return 0
	case "oversized":
		chunk := strings.Repeat("1", 1024)
		for written := 0; written < 64; written++ {
			fmt.Fprint(os.Stdout, chunk)
		}
		return 0
	case "crash":
		fmt.Fprintln(os.Stderr, "simulated helper crash")
		return 7
	case "sigbus":
		code := runSigbusProbe()
		if code != 0 {
			return code
		}
		// A valid response is the proof the child survived the fault.
		fmt.Fprint(os.Stdout, "[null]")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown helper test mode %q\n", mode)
		return 1
	}
}

// runSigbusProbe reproduces a disappearing BAR inside the child: map a file,
// truncate it, then touch the mapping. If the fault is not contained the child
// dies from SIGBUS instead of reporting the unreadable register.
func runSigbusProbe() int {
	dir, err := os.MkdirTemp("", "rigscope-sigbus")
	if err != nil {
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	name := filepath.Join(dir, "resource0")
	if err := os.WriteFile(name, make([]byte, 8192), 0o600); err != nil {
		return 1
	}
	file, err := os.Open(name)
	if err != nil {
		return 1
	}
	defer file.Close()
	data, err := syscall.Mmap(int(file.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return 1
	}
	defer func() { _ = syscall.Munmap(data) }()
	if err := os.Truncate(name, 0); err != nil {
		return 1
	}
	sink := 0
	if !guardFault(func() { sink += int(data[0]) }) {
		return 1
	}
	return 0
}

// testHelperSource points the helper at the test binary itself. The explicit
// resource root lets fixture paths through; production never sets it.
func testHelperSource() HelperSource {
	return HelperSource{
		Executable:   func() (string, error) { return os.Args[0], nil },
		Timeout:      10 * time.Second,
		resourceRoot: "/",
	}
}

// writeHelperResource creates a resource file at a path the helper's shape
// validation accepts: <root>/sys/bus/pci/devices/<bdf>/resource0.
func writeHelperResource(t *testing.T, bdf string, size int, words map[int64]uint32) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sys", "bus", "pci", "devices", bdf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir resource dir: %v", err)
	}
	name := filepath.Join(dir, "resource0")
	data := make([]byte, size)
	for offset, word := range words {
		if offset < 0 || offset+4 > int64(size) {
			t.Fatalf("word at 0x%x does not fit in %d bytes", offset, size)
		}
		data[offset] = byte(word)
		data[offset+1] = byte(word >> 8)
		data[offset+2] = byte(word >> 16)
		data[offset+3] = byte(word >> 24)
	}
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatalf("write resource: %v", err)
	}
	return name
}

func TestHelperProtocolRoundTrip(t *testing.T) {
	resource := writeHelperResource(t, "0000:01:00.0", 0x21000, map[int64]uint32{
		0x2046c: 0xc0002480,
		0xe2a8:  0x80008500,
		0x1ffc:  0x11111111,
		0x2004:  0x22222222,
	})
	t.Setenv(helperModeEnv, "roundtrip")

	values, err := testHelperSource().ReadRegisters(context.Background(), resource,
		[]int64{0x2046c, 0xe2a8, 0x1ffc, 0x2004, 0x40000})
	if err != nil {
		t.Fatalf("helper read: %v", err)
	}
	requireWord(t, values, 0, 0xc0002480)
	requireWord(t, values, 1, 0x80008500)
	requireWord(t, values, 2, 0x11111111)
	requireWord(t, values, 3, 0x22222222)
	if values[4] != nil {
		t.Fatalf("beyond-EOF value = %v, want nil", *values[4])
	}
}

func TestHelperTimeoutKillsTheChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv(helperModeEnv, "sleep")
	t.Setenv(helperPIDFileEnv, pidFile)

	source := testHelperSource()
	source.Timeout = 150 * time.Millisecond
	resource := filepath.Join(t.TempDir(), "sys", "bus", "pci", "devices", "0000:01:00.0", "resource0")

	started := time.Now()
	values, err := source.ReadRegisters(context.Background(), resource, []int64{0})
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if len(values) != 1 || values[0] != nil {
		t.Fatalf("timed-out values = %v, want one nil", values)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took %s, want the deadline to kill the helper", elapsed)
	}

	pid := readHelperPID(t, pidFile)
	waitForProcessExit(t, pid)
}

func readHelperPID(t *testing.T, path string) int {
	t.Helper()
	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read helper pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse helper pid %q: %v", raw, err)
	}
	return pid
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper pid %d survived the deadline", pid)
}

func TestHelperRejectsMalformedOutput(t *testing.T) {
	t.Setenv(helperModeEnv, "malformed")
	values, err := testHelperSource().ReadRegisters(context.Background(),
		"/sys/bus/pci/devices/0000:01:00.0/resource0", []int64{0, 4})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("err = %v, want a malformed-JSON error", err)
	}
	if len(values) != 2 || values[0] != nil || values[1] != nil {
		t.Fatalf("values = %v, want two nils", values)
	}
}

func TestHelperRejectsOversizedOutput(t *testing.T) {
	t.Setenv(helperModeEnv, "oversized")
	values, err := testHelperSource().ReadRegisters(context.Background(),
		"/sys/bus/pci/devices/0000:01:00.0/resource0", []int64{0})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("err = %v, want an output-size error", err)
	}
	if len(values) != 1 || values[0] != nil {
		t.Fatalf("values = %v, want one nil", values)
	}
}

func TestHelperCrashBecomesNilReadings(t *testing.T) {
	t.Setenv(helperModeEnv, "crash")
	values, err := testHelperSource().ReadRegisters(context.Background(),
		"/sys/bus/pci/devices/0000:01:00.0/resource0", []int64{0})
	if err == nil || !strings.Contains(err.Error(), "simulated helper crash") {
		t.Fatalf("err = %v, want the helper's stderr", err)
	}
	if len(values) != 1 || values[0] != nil {
		t.Fatalf("values = %v, want one nil", values)
	}
}

func TestHelperContainsARealSIGBUS(t *testing.T) {
	t.Setenv(helperModeEnv, "sigbus")
	// The child maps a file, truncates it and touches the mapping. It only
	// answers with a valid response when guardFault contained the fault on its
	// own goroutine; otherwise the child dies from SIGBUS and the parent here
	// sees an error instead.
	values, err := testHelperSource().ReadRegisters(context.Background(),
		"/sys/bus/pci/devices/0000:01:00.0/resource0", []int64{0})
	if err != nil {
		t.Fatalf("fault probe: %v", err)
	}
	if len(values) != 1 || values[0] != nil {
		t.Fatalf("values = %v, want one nil from the contained fault", values)
	}
}

func TestHelperRejectsInvalidRequestsBeforeSpawning(t *testing.T) {
	source := testHelperSource()
	cases := []struct {
		name    string
		path    string
		offsets []int64
	}{
		{"not a resource", "/etc/passwd", []int64{0}},
		{"wrong BAR", "/sys/bus/pci/devices/0000:01:00.0/resource1", []int64{0}},
		{"relative", "sys/bus/pci/devices/0000:01:00.0/resource0", []int64{0}},
		{"unclean", "/sys/bus/pci/devices/0000:01:00.0/../0000:01:00.0/resource0", []int64{0}},
		{"unaligned", "/sys/bus/pci/devices/0000:01:00.0/resource0", []int64{2}},
		{"negative", "/sys/bus/pci/devices/0000:01:00.0/resource0", []int64{-4}},
		{"past the BAR window", "/sys/bus/pci/devices/0000:01:00.0/resource0", []int64{maxRegisterOffset + 4}},
		{"too many", "/sys/bus/pci/devices/0000:01:00.0/resource0", make([]int64, maxRegisterOffsets+1)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values, err := source.ReadRegisters(context.Background(), testCase.path, testCase.offsets)
			if err == nil {
				t.Fatal("err = nil, want a validation error")
			}
			if len(values) != len(testCase.offsets) {
				t.Fatalf("values = %d, want %d nils", len(values), len(testCase.offsets))
			}
			for index, value := range values {
				if value != nil {
					t.Fatalf("values[%d] = %v, want nil", index, *value)
				}
			}
		})
	}
}

func TestRunPCIReadHelperRejectsMalformedRequests(t *testing.T) {
	long := strings.Repeat(" ", helperRequestLimit+16)
	cases := map[string]string{
		"not json":        "{",
		"missing path":    `{"offsets":[0]}`,
		"no offsets":      `{"path":"/sys/bus/pci/devices/0000:01:00.0/resource0"}`,
		"path not a BAR":  `{"path":"/etc/passwd","offsets":[0]}`,
		"oversized input": long,
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := RunPCIReadHelper(strings.NewReader(request), &stdout, &stderr); code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr %q)", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want no response for a rejected request", stdout.String())
			}
		})
	}
}

func TestRunPCIReadHelperAnswersNilForAnUnreadableBAR(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sys", "bus", "pci", "devices", "0000:01:00.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	request := fmt.Sprintf(`{"path":%q,"offsets":[0,4]}`, filepath.Join(dir, "resource0"))
	var stdout, stderr bytes.Buffer
	if code := RunPCIReadHelper(strings.NewReader(request), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	var values []*uint32
	if err := json.Unmarshal(stdout.Bytes(), &values); err != nil {
		t.Fatalf("response %q: %v", stdout.String(), err)
	}
	if len(values) != 2 || values[0] != nil || values[1] != nil {
		t.Fatalf("values = %v, want two nils", values)
	}
}

// TestHelperBoundedWaitRefusesReplacementsWhileUnreaped reproduces an
// uninterruptible child: kill cannot reap it, so the caller must return at its
// deadline and later launches must be refused instead of spawning another
// process that could also wedge.
func TestHelperBoundedWaitRefusesReplacementsWhileUnreaped(t *testing.T) {
	proc := &fakeChildProcess{release: make(chan struct{})}
	var launches int32
	source := HelperSource{
		Executable: func() (string, error) { return os.Args[0], nil },
		Timeout:    20 * time.Millisecond,
		slot:       &reapSlot{},
		launch: func(io.Writer, io.Writer) (childProcess, error) {
			atomic.AddInt32(&launches, 1)
			return proc, nil
		},
	}
	path := "/sys/bus/pci/devices/0000:01:00.0/resource0"

	started := time.Now()
	_, err := source.ReadRegisters(context.Background(), path, []int64{0})
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("wait took %s, want the caller bounded by its deadline", elapsed)
	}
	if !proc.wasKilled() {
		t.Fatal("the child was not killed at the deadline")
	}

	_, err = source.ReadRegisters(context.Background(), path, []int64{0})
	if !errors.Is(err, errChildNotReaped) {
		t.Fatalf("second read err = %v, want a refusal while the child is unreaped", err)
	}
	if got := atomic.LoadInt32(&launches); got != 1 {
		t.Fatalf("launches = %d, want exactly one", got)
	}

	// Reaping the child frees the slot for a later read.
	close(proc.release)
	waitFor(t, 2*time.Second, func() bool {
		_, err := source.ReadRegisters(context.Background(), path, []int64{0})
		return !errors.Is(err, errChildNotReaped)
	})
}

func TestHelperSourceDoesNotOverwriteTheCallersOffsets(t *testing.T) {
	resource := writeHelperResource(t, "0000:01:00.0", 0x1000, map[int64]uint32{0: 0xfeedface})
	t.Setenv(helperModeEnv, "roundtrip")
	offsets := []int64{0, 4}
	if _, err := testHelperSource().ReadRegisters(context.Background(), resource, offsets); err != nil {
		t.Fatalf("read: %v", err)
	}
	if offsets[0] != 0 || offsets[1] != 4 {
		t.Fatalf("offsets after the call = %v", offsets)
	}
}
