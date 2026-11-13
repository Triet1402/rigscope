package processes

import "testing"

func TestSafeIntBounds(t *testing.T) {
	cases := []struct {
		value string
		min   int64
		max   int64
		want  int64
		ok    bool
	}{
		{"0", 0, maxInt64, 0, true},
		{"01", 0, maxInt64, 1, true},
		{"-1", minInt64, maxInt64, -1, true},
		{"-1", 0, maxInt64, 0, false},
		{"", 0, maxInt64, 0, false},
		{"-", minInt64, maxInt64, 0, false},
		{"+1", 0, maxInt64, 0, false},
		{"1 ", 0, maxInt64, 0, false},
		{"1a", 0, maxInt64, 0, false},
		{"9223372036854775807", 0, maxInt64, maxInt64, true},
		{"9223372036854775808", 0, maxInt64, 0, false},
		{"-9223372036854775808", minInt64, maxInt64, minInt64, true},
		{"-9223372036854775809", minInt64, maxInt64, 0, false},
		{"999999999999999999999999999999", 0, maxInt64, 0, false},
		{"2147483648", 0, 1 << 31, 1 << 31, true},
	}
	for _, testCase := range cases {
		got, ok := safeInt([]byte(testCase.value), testCase.min, testCase.max)
		if ok != testCase.ok || (ok && got != testCase.want) {
			t.Errorf("safeInt(%q, %d, %d) = %d/%v, want %d/%v",
				testCase.value, testCase.min, testCase.max, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestParseStatUsesTheLastParenAndRejectsOtherPids(t *testing.T) {
	line := "42 (a b) (c) S 1 100 100 0 -1 4194304 0 0 0 0 10 5 0 0 20 0 3 0 1000 123456 2 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	stat, ok := parseStat(42, []byte(line))
	if !ok {
		t.Fatal("valid stat rejected")
	}
	if stat.name != "a b) (c" {
		t.Errorf("name = %q", stat.name)
	}
	if stat.state != "S" || stat.ppid != 1 || stat.utime != 10 || stat.stime != 5 {
		t.Errorf("stat = %+v", stat)
	}
	if stat.threads == nil || *stat.threads != 3 {
		t.Errorf("threads = %v", stat.threads)
	}
	if stat.startTicks != 1000 {
		t.Errorf("startTicks = %d", stat.startTicks)
	}
	if stat.rssPages == nil || *stat.rssPages != 2 {
		t.Errorf("rssPages = %v", stat.rssPages)
	}

	if _, ok := parseStat(43, []byte(line)); ok {
		t.Fatal("a pid mismatch must reject the line")
	}
	if _, ok := parseStat(42, []byte("42 (no-close S 1")); ok {
		t.Fatal("a missing close paren must reject the line")
	}
	if _, ok := parseStat(42, []byte("42 (short) S 1")); ok {
		t.Fatal("a truncated stat must reject the line")
	}
}

func TestParseStatusPrefersTheFirstFields(t *testing.T) {
	status := parseStatus([]byte("Name:\tx\nUid:\t1000\t1001\t1002\t1003\nUid:\t2000\t0\t0\t0\nThreads:\t7\nThreads:\t9\n"))
	if status.uid == nil || *status.uid != 1000 {
		t.Errorf("uid = %v", status.uid)
	}
	if status.threads == nil || *status.threads != 7 {
		t.Errorf("threads = %v", status.threads)
	}
	empty := parseStatus([]byte("Name:\tx\n"))
	if empty.uid != nil || empty.threads != nil {
		t.Errorf("empty status = %+v", empty)
	}
}

func TestParseIOAndCgroupAndCommandLine(t *testing.T) {
	io, ok := parseIO([]byte("rchar: 5\nread_bytes: 100\nwrite_bytes: 200\n"))
	if !ok || io.readBytes == nil || *io.readBytes != 100 || io.writeBytes == nil || *io.writeBytes != 200 {
		t.Errorf("io = %+v/%v", io, ok)
	}
	if _, ok := parseIO([]byte("rchar: 5\n")); ok {
		t.Error("an io file without byte counters is absent, not zero")
	}
	if _, ok := parseIO([]byte("read_bytes: -5\n")); ok {
		t.Error("a negative counter is invalid")
	}

	unit, ok := parseCgroupUnit([]byte("12:pids:/foo\n0::/a/b/example.scope\n"))
	if !ok || unit != "example.scope" {
		t.Errorf("unit = %q/%v", unit, ok)
	}
	if _, ok := parseCgroupUnit([]byte("0::/a/b/slice\n0::/x.service\n")); ok {
		t.Error("the first unified line decides")
	}
	if _, ok := parseCgroupUnit([]byte("12:pids:/a.service\n")); ok {
		t.Error("a non-unified hierarchy is not a unit")
	}

	if got := parseCommandLine([]byte("node\x00server.js\x00--flag")); got != "node server.js --flag" {
		t.Errorf("command = %q", got)
	}
	if got := parseCommandLine([]byte("echo\x00hi\x00\n")); got != "echo hi" {
		t.Errorf("trailing whitespace not trimmed: %q", got)
	}
}

func TestTruncateTextDoesNotSplitUTF8(t *testing.T) {
	if got := truncateText("héllo", 2); got != "hé" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncateText("hello", 0); got != "" {
		t.Errorf("zero limit = %q", got)
	}
	if got := truncateText("hello", 99); got != "hello" {
		t.Errorf("short string = %q", got)
	}

	// The protocol counts UTF-16 code units, not runes: an astral character is
	// two units, and a limit that would split it keeps the whole character out.
	if got := truncateText("😀😀", 3); got != "😀" {
		t.Errorf("astral truncate = %q, want one whole emoji", got)
	}
	if got := truncateText("😀😀", 4); got != "😀😀" {
		t.Errorf("astral truncate at exact length = %q", got)
	}
	if got := truncateText("a😀", 2); got != "a" {
		t.Errorf("mixed truncate = %q", got)
	}
	if got := truncateText("😀", 1); got != "" {
		t.Errorf("a single astral character cannot fill one unit: %q", got)
	}
}

func TestParsePIDMatchesTheWireContractBounds(t *testing.T) {
	// The contract's pid bound is 0..2^31 inclusive. On a 32-bit build Go's int
	// cannot represent the top value, so it is unreadable there.
	topFits := int64(int(^uint(0)>>1)) >= 1<<31
	cases := []struct {
		name    string
		ok      bool
		wantPID int64
	}{
		{"", false, 0},
		{"0", true, 0},
		{"9001", true, 9001},
		{"2147483647", true, 2147483647},
		{"2147483648", topFits, 1 << 31}, // the contract's inclusive upper bound
		{"2147483649", false, 0},
		{"-1", false, 0},
		{"12a", false, 0},
		{"١٢", false, 0}, // non-ASCII digits are not a pid
	}
	for _, testCase := range cases {
		pid, ok := parsePID(testCase.name)
		if ok != testCase.ok || (ok && int64(pid) != testCase.wantPID) {
			t.Errorf("parsePID(%q) = %d/%v, want %d/%v", testCase.name, pid, ok, testCase.wantPID, testCase.ok)
		}
	}
}
