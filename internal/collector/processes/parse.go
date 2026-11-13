package processes

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// Field truncation bounds. They are defensive: a hostile or buggy kernel value
// must not make a snapshot unbounded. They match the prototype scanner and the
// protocol's text limits.
const (
	maxNameLength    = 256
	maxUserLength    = 64
	maxUnitLength    = 256
	maxStateLength   = 16
	maxCommandLength = 4096
	maxThreads       = 10_000_000

	// maxCommandBytes bounds the bytes read from cmdline before conversion.
	// Three bytes per UTF-16 code unit is the worst case for a BMP character;
	// an astral character spends four bytes on two units, so 3*4096 bytes can
	// always fill the 4096-unit cap.
	maxCommandBytes = maxCommandLength * 3
)

// Integer bounds from the wire contract (src/contracts/telemetry.ts). Identity
// values outside them make a row unreadable rather than wrong; optional values
// outside them stay unknown (nil) instead of being clamped.
const (
	// maxPID is the contract's pid/ppid upper bound: 0..2^31 inclusive.
	maxPID = 1 << 31
	// maxWireInteger is JavaScript's Number.MAX_SAFE_INTEGER.
	maxWireInteger = protocol.MaxSafeInteger
)

// procStat is the subset of /proc/<pid>/stat the snapshot needs.
type procStat struct {
	name       string
	state      string
	ppid       int
	utime      int64
	stime      int64
	threads    *int
	startTicks int64
	rssPages   *int64
}

// parseStat parses /proc/<pid>/stat without trusting the command name.
//
// comm is wrapped in parentheses and may itself contain spaces and ')'. The
// last ')' is the only safe boundary because the rest of the line contains no
// parentheses. Fields after it start at field 3 (state).
func parseStat(pid int, text []byte) (procStat, bool) {
	open := bytes.IndexByte(text, '(')
	closeAt := bytes.LastIndexByte(text, ')')
	if open < 0 || closeAt <= open {
		return procStat{}, false
	}
	parsedPID, ok := safeInt(bytes.TrimSpace(text[:open]), 0, maxPID)
	if !ok || parsedPID != int64(pid) {
		return procStat{}, false
	}

	// rest[0] is field 3 (state); field N is rest[N-3]. Only the 22 fields the
	// snapshot reads are materialised, and they are iterated as bytes so no
	// per-field slice is allocated.
	var rest [22][]byte
	scan := byteFields{text: text[closeAt+1:]}
	count := 0
	for count < len(rest) {
		field, ok := scan.next()
		if !ok {
			break
		}
		rest[count] = field
		count++
	}
	if count < 20 {
		return procStat{}, false
	}
	state := string(rest[0])
	if state == "" {
		return procStat{}, false
	}
	ppid, ok := safeInt(rest[1], 0, maxPID)
	if !ok {
		return procStat{}, false
	}
	utime, ok := safeInt(rest[11], 0, maxWireInteger)
	if !ok {
		return procStat{}, false
	}
	stime, ok := safeInt(rest[12], 0, maxWireInteger)
	if !ok {
		return procStat{}, false
	}
	startTicks, ok := safeInt(rest[19], 0, maxWireInteger)
	if !ok {
		return procStat{}, false
	}

	var threads *int
	if value, ok := safeInt(rest[17], 0, maxThreads); ok {
		n := int(value)
		threads = &n
	}
	var rssPages *int64
	if count > 21 {
		if value, ok := safeInt(rest[21], -maxWireInteger, maxWireInteger); ok && value >= 0 {
			rssPages = &value
		}
	}

	return procStat{
		name:       string(text[open+1 : closeAt]),
		state:      state,
		ppid:       int(ppid),
		utime:      utime,
		stime:      stime,
		threads:    threads,
		startTicks: startTicks,
		rssPages:   rssPages,
	}, true
}

// procStatus is the subset of /proc/<pid>/status the snapshot needs.
type procStatus struct {
	uid     *int
	threads *int
}

// parseStatus reads the real UID (the first Uid field) and Threads. The
// prototype prefers status Threads over stat num_threads.
func parseStatus(text []byte) procStatus {
	var status procStatus
	lines := lineFields{text: text}
	for {
		line, ok := lines.next()
		if !ok {
			return status
		}
		line = bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(line, []byte("Uid:")):
			if status.uid != nil {
				continue
			}
			fields := byteFields{text: line[len("Uid:"):]}
			value, ok := fields.next()
			if !ok {
				continue
			}
			if uid, ok := safeInt(value, 0, maxPID); ok {
				n := int(uid)
				status.uid = &n
			}
		case bytes.HasPrefix(line, []byte("Threads:")):
			if status.threads != nil {
				continue
			}
			fields := byteFields{text: line[len("Threads:"):]}
			value, ok := fields.next()
			if !ok {
				continue
			}
			if threads, ok := safeInt(value, 0, maxThreads); ok {
				n := int(threads)
				status.threads = &n
			}
		}
	}
}

type procIO struct {
	readBytes  *int64
	writeBytes *int64
}

// parseIO reads the cumulative read_bytes/write_bytes counters. A missing or
// unreadable io file is not an inaccessible process: the rest of the row is
// still usable and both rates stay null.
func parseIO(text []byte) (procIO, bool) {
	var io procIO
	lines := lineFields{text: text}
	for {
		line, ok := lines.next()
		if !ok {
			break
		}
		switch {
		case bytes.HasPrefix(line, []byte("read_bytes:")):
			if io.readBytes != nil {
				continue
			}
			if value, ok := parseCounter(line[len("read_bytes:"):]); ok {
				io.readBytes = &value
			}
		case bytes.HasPrefix(line, []byte("write_bytes:")):
			if io.writeBytes != nil {
				continue
			}
			if value, ok := parseCounter(line[len("write_bytes:"):]); ok {
				io.writeBytes = &value
			}
		}
	}
	if io.readBytes == nil && io.writeBytes == nil {
		return procIO{}, false
	}
	return io, true
}

// parseCgroupUnit extracts the systemd unit from the unified cgroup path
// ("0::/..."). Only the final component counts, and only .service/.scope
// leaves are units; slices and arbitrary cgroup names yield no unit.
func parseCgroupUnit(text []byte) (string, bool) {
	lines := lineFields{text: text}
	for {
		line, ok := lines.next()
		if !ok {
			return "", false
		}
		if !bytes.HasPrefix(line, []byte("0::")) {
			continue
		}
		path := line[3:]
		// Walk the path components without splitting it into a slice.
		var last []byte
		for len(path) > 0 {
			var part []byte
			if slash := bytes.IndexByte(path, '/'); slash >= 0 {
				part, path = path[:slash], path[slash+1:]
			} else {
				part, path = path, nil
			}
			if len(part) != 0 {
				last = part
			}
		}
		if !bytes.HasSuffix(last, []byte(".service")) && !bytes.HasSuffix(last, []byte(".scope")) {
			return "", false
		}
		return string(last), true
	}
}

// parseCommandLine turns NUL-separated argv into one display string. It may
// modify text in place, so callers pass a buffer they own (the scan worker's
// scratch buffer).
func parseCommandLine(text []byte) string {
	for index := range text {
		if text[index] == 0 {
			text[index] = ' '
		}
	}
	return string(bytes.TrimSpace(text))
}

// parsePasswd maps UIDs to login names. The first entry for a UID wins.
func parsePasswd(text []byte) map[int]string {
	users := make(map[int]string)
	for _, line := range bytes.Split(text, []byte{'\n'}) {
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		fields := strings.Split(string(line), ":")
		if len(fields) < 3 {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil || uid < 0 {
			continue
		}
		if _, exists := users[uid]; exists {
			continue
		}
		users[uid] = fields[0]
	}
	return users
}

// byteFields iterates whitespace-separated fields of a byte slice without
// allocating a slice of fields. It follows bytes.Fields' definition of space
// (unicode.IsSpace for ASCII, which is all /proc uses).
type byteFields struct {
	text []byte
	at   int
}

func (f *byteFields) next() ([]byte, bool) {
	for f.at < len(f.text) && isASCIISpace(f.text[f.at]) {
		f.at++
	}
	if f.at >= len(f.text) {
		return nil, false
	}
	start := f.at
	for f.at < len(f.text) && !isASCIISpace(f.text[f.at]) {
		f.at++
	}
	return f.text[start:f.at], true
}

// lineFields iterates newline-separated lines without allocating a slice.
type lineFields struct {
	text []byte
}

func (f *lineFields) next() ([]byte, bool) {
	if len(f.text) == 0 {
		return nil, false
	}
	if index := bytes.IndexByte(f.text, '\n'); index >= 0 {
		line := f.text[:index]
		f.text = f.text[index+1:]
		return line, true
	}
	line := f.text
	f.text = nil
	return line, true
}

func isASCIISpace(value byte) bool {
	switch value {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

const (
	maxInt64 = int64(^uint64(0) >> 1)
	minInt64 = -maxInt64 - 1
)

// safeInt parses a strict base-10 integer within [min, max]. It mirrors the
// prototype's safeInt: whitespace, signs other than a leading '-', and values
// outside the allowed range are rejected rather than clamped. It parses bytes
// directly because it runs several times for every process on the hot path.
func safeInt(value []byte, min, max int64) (int64, bool) {
	if len(value) == 0 {
		return 0, false
	}
	const maxUint64 = ^uint64(0)
	negative := false
	index := 0
	if value[0] == '-' {
		if len(value) == 1 {
			return 0, false
		}
		negative = true
		index = 1
	}
	var magnitude uint64
	for ; index < len(value); index++ {
		digit := value[index]
		if digit < '0' || digit > '9' {
			return 0, false
		}
		parsed := uint64(digit - '0')
		if magnitude > (maxUint64-parsed)/10 {
			return 0, false
		}
		magnitude = magnitude*10 + parsed
	}

	var result int64
	if negative {
		const minMagnitude = uint64(1) << 63
		if magnitude > minMagnitude {
			return 0, false
		}
		if magnitude == minMagnitude {
			result = minInt64
		} else {
			result = -int64(magnitude)
		}
	} else {
		if magnitude > uint64(maxInt64) {
			return 0, false
		}
		result = int64(magnitude)
	}
	if result < min || result > max {
		return 0, false
	}
	return result, true
}

// parseCounter parses a non-negative decimal counter; negative values do not
// match, exactly like the prototype's /\d+/ reader.
func parseCounter(value []byte) (int64, bool) {
	return safeInt(bytes.TrimSpace(value), 0, maxWireInteger)
}

// truncateText caps a string at max UTF-16 code units (JavaScript's
// String#length, which is what the protocol's text limits count) without
// splitting a UTF-8 sequence. A rune above U+FFFF counts as two units.
func truncateText(value string, max int) string {
	if max <= 0 {
		return ""
	}
	units := 0
	for index, r := range value {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if units+width > max {
			return value[:index]
		}
		units += width
	}
	return value
}
