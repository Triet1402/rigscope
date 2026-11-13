package procfs

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	pressureLineRE = regexp.MustCompile(`^(some|full)\s+(.*)$`)
	pressureAvgRE  = regexp.MustCompile(`^avg10=([0-9]+(?:\.[0-9]+)?)$`)
)

// Pressure is one PSI file. Missing lines stay nil.
type Pressure struct {
	SomeAvg10 *float64
	FullAvg10 *float64
}

// ReadPressure reads one /proc/pressure file. A missing or unreadable file is
// an error; callers turn it into absent optional fields.
func ReadPressure(src Source, path string) (Pressure, error) {
	text, err := readText(src, path)
	if err != nil {
		return Pressure{}, err
	}
	return ParsePressure([]byte(text))
}

// ParsePressure parses the some/full lines of one PSI file. It rejects the
// file only when neither line is recognised; a missing or malformed avg10 on
// a recognised line stays nil.
func ParsePressure(text []byte) (Pressure, error) {
	var (
		pressure Pressure
		sawLine  bool
	)
	for _, rawLine := range strings.Split(string(text), "\n") {
		match := pressureLineRE.FindStringSubmatch(strings.TrimSpace(rawLine))
		if match == nil {
			continue
		}
		sawLine = true
		value := parseAvg10(match[2])
		if match[1] == "some" {
			pressure.SomeAvg10 = value
		} else {
			pressure.FullAvg10 = value
		}
	}
	if !sawLine {
		return Pressure{}, fmt.Errorf("%w: no some/full line", ErrMalformed)
	}
	return pressure, nil
}

func parseAvg10(fields string) *float64 {
	for _, field := range strings.Fields(fields) {
		match := pressureAvgRE.FindStringSubmatch(field)
		if match == nil {
			continue
		}
		value, err := strconv.ParseFloat(match[1], 64)
		if err != nil || value < 0 || value > 100 {
			return nil
		}
		return &value
	}
	return nil
}
