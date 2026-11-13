package procfs

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// fixtureSource opens one captured or hand-written tree under
// testdata/proc/<name> as an fs.FS rooted at "/".
func fixtureSource(t *testing.T, name string) Source {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..", "testdata", "proc", name)
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("fixture %q: %v", name, err)
	}
	return NewFSSource(os.DirFS(root))
}

// mapSource builds an in-memory Source. files and dirs use absolute kernel
// paths so the fixtures read like /proc/stat.
func mapSource(files map[string]string, dirs map[string][]string) Source {
	fsys := fstest.MapFS{}
	for path, content := range files {
		fsys[fsPath(path)] = &fstest.MapFile{Data: []byte(content)}
	}
	for dir, children := range dirs {
		fsys[fsPath(dir)] = &fstest.MapFile{Mode: os.ModeDir | 0o755}
		for _, child := range children {
			fsys[fsPath(dir+"/"+child)] = &fstest.MapFile{Mode: os.ModeDir | 0o755}
		}
	}
	return NewFSSource(fsys)
}

func i64p(value int64) *int64     { return &value }
func u64p(value uint64) *uint64   { return &value }
func f64p(value float64) *float64 { return &value }
func strp(value string) *string   { return &value }
func intp(value int) *int         { return &value }

func requireOK(t *testing.T, status Status) {
	t.Helper()
	if status != StatusOK {
		t.Fatalf("status = %q, want ok", status)
	}
}

func requireReason(t *testing.T, got, wantSubstring string) {
	t.Helper()
	if !strings.Contains(got, wantSubstring) {
		t.Fatalf("reason %q does not contain %q", got, wantSubstring)
	}
}

// statValues are the ten /proc/stat CPU columns.
type statValues struct {
	user, nice, system, idle, iowait, irq, softirq, steal, guest, guestNice int
}

func procStatLine(values statValues) string {
	return strings.Join([]string{
		strconv.Itoa(values.user),
		strconv.Itoa(values.nice),
		strconv.Itoa(values.system),
		strconv.Itoa(values.idle),
		strconv.Itoa(values.iowait),
		strconv.Itoa(values.irq),
		strconv.Itoa(values.softirq),
		strconv.Itoa(values.steal),
		strconv.Itoa(values.guest),
		strconv.Itoa(values.guestNice),
	}, " ")
}

// cpuLine is the two-core synthetic layout used by most rate tests: busy in
// user, idle in idle.
func cpuLine(totalBusy, totalIdle, core0Busy, core0Idle, core1Busy, core1Idle int) string {
	return "cpu " + procStatLine(statValues{user: totalBusy, idle: totalIdle}) + "\n" +
		"cpu0 " + procStatLine(statValues{user: core0Busy, idle: core0Idle}) + "\n" +
		"cpu1 " + procStatLine(statValues{user: core1Busy, idle: core1Idle}) + "\n"
}

// meminfoText renders /proc/meminfo. A nil override omits the field; absent
// keys use the fixture defaults.
func meminfoText(overrides map[string]*int) string {
	defaults := map[string]int{
		"MemTotal":     1000,
		"MemAvailable": 400,
		"SwapTotal":    100,
		"SwapFree":     60,
	}
	order := []string{"MemTotal", "MemAvailable", "SwapTotal", "SwapFree"}
	var lines []string
	for _, key := range order {
		var value int
		ok := true
		if override, present := overrides[key]; present {
			if override == nil {
				ok = false
			} else {
				value = *override
			}
		} else {
			value = defaults[key]
		}
		if ok {
			lines = append(lines, key+": "+strconv.Itoa(value)+" kB")
		}
	}
	return strings.Join(lines, "\n")
}

func netDevText(entries [][3]string) string {
	lines := []string{
		"Inter-|   Receive                                                |  Transmit",
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed",
	}
	for _, entry := range entries {
		lines = append(lines, entry[0]+": "+entry[1]+" 1 0 0 0 0 0 0 "+entry[2]+" 1 0 0 0 0 0 0")
	}
	return strings.Join(lines, "\n")
}

func diskstatsText(entries [][3]string) string {
	var lines []string
	for index, entry := range entries {
		lines = append(lines, "   8 "+strconv.Itoa(index)+" "+entry[0]+" 10 0 "+entry[1]+" 0 10 0 "+entry[2]+" 0 0 0 0 0 0 0")
	}
	return strings.Join(lines, "\n")
}

// diskStatValues is one full /proc/diskstats line with operation counters and
// times, so tests can exercise latency and utilization.
type diskStatValues struct {
	name         string
	readSectors  int
	writeSectors int
	reads        int
	readTimeMs   int
	writes       int
	writeTimeMs  int
	ioTimeMs     int
}

func diskstatsValues(entries []diskStatValues) string {
	var lines []string
	for index, entry := range entries {
		lines = append(lines, "   8 "+strconv.Itoa(index)+" "+entry.name+
			" "+strconv.Itoa(entry.reads)+" 0 "+strconv.Itoa(entry.readSectors)+" "+strconv.Itoa(entry.readTimeMs)+
			" "+strconv.Itoa(entry.writes)+" 0 "+strconv.Itoa(entry.writeSectors)+" "+strconv.Itoa(entry.writeTimeMs)+
			" 0 "+strconv.Itoa(entry.ioTimeMs)+" 0 0 0 0")
	}
	return strings.Join(lines, "\n")
}

// harness is the deterministic sampler fixture shared by the rate tests.
type harness struct {
	t         *testing.T
	files     map[string]string
	dirs      map[string][]string
	source    Source
	statfs    map[string]StatFSSnapshot
	addresses map[string][]InterfaceAddress
	clock     time.Time
	clockOK   bool
	sampler   *Sampler
	host      HostInfo
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:         t,
		clockOK:   true,
		clock:     time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC),
		host:      HostInfo{Platform: "linux", Release: "fixture-kernel", Arch: "x86_64", CPUCount: 2},
		statfs:    map[string]StatFSSnapshot{"/": {Bsize: 4096, Blocks: 1000, Bfree: 250}},
		addresses: map[string][]InterfaceAddress{},
		dirs: map[string][]string{
			"/sys/block":               {"sda"},
			"/sys/class/net":           {"lo"},
			"/sys/devices/virtual/net": {"lo"},
		},
	}
	h.files = map[string]string{
		"/proc/stat":                      cpuLine(100, 100, 50, 50, 50, 50),
		"/proc/meminfo":                   meminfoText(nil),
		"/proc/cpuinfo":                   "model name\t: Fixture CPU\n",
		"/etc/os-release":                 "PRETTY_NAME=\"Fixture Linux\"\n",
		"/proc/self/mountinfo":            "49 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw",
		"/proc/diskstats":                 diskstatsText([][3]string{{"sda", "100", "50"}}),
		"/proc/net/dev":                   netDevText([][3]string{{"lo", "1000", "2000"}}),
		"/proc/sys/kernel/osrelease":      "fixture-kernel\n",
		"/sys/block/sda/device/model":     "Fixture Disk\n",
		"/sys/block/sda/queue/rotational": "0\n",
		"/sys/block/sda/size":             "2000\n",
	}
	h.source = dynamicSource{files: h.files, dirs: h.dirs}
	h.sampler = h.newSampler()
	return h
}

func (h *harness) newSampler() *Sampler {
	h.t.Helper()
	statfs := func(path string) (StatFSSnapshot, error) {
		if snapshot, ok := h.statfs[path]; ok {
			return snapshot, nil
		}
		return StatFSSnapshot{Bsize: 4096, Blocks: 1000, Bfree: 400}, nil
	}
	addresses := func() (map[string][]InterfaceAddress, error) {
		return h.addresses, nil
	}
	host := h.host
	return NewSampler(SamplerOptions{
		Source:    h.source,
		StatFS:    statfs,
		Addresses: addresses,
		Host:      &host,
		Monotonic: func() (time.Time, bool) { return h.clock, h.clockOK },
	})
}

func (h *harness) advance(milliseconds int) {
	h.clock = h.clock.Add(time.Duration(milliseconds) * time.Millisecond)
}

func (h *harness) set(path, content string) {
	h.files[path] = content
}

func (h *harness) delete(path string) {
	delete(h.files, path)
}

// dynamicSource reads the harness's mutable file and directory maps,
// including later writes.
type dynamicSource struct {
	files map[string]string
	dirs  map[string][]string
}

func (s dynamicSource) ReadFile(path string) ([]byte, error) {
	if content, ok := s.files[path]; ok {
		return []byte(content), nil
	}
	return nil, os.ErrNotExist
}

func (s dynamicSource) ReadDir(path string) ([]string, error) {
	seen := map[string]bool{}
	for _, name := range s.dirs[path] {
		seen[name] = true
	}
	prefix := path
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	for file := range s.files {
		if !strings.HasPrefix(file, prefix) {
			continue
		}
		rest := file[len(prefix):]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			rest = rest[:slash]
		}
		seen[rest] = true
	}
	if len(seen) == 0 {
		return nil, os.ErrNotExist
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
