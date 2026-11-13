package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// decodeJSONValue decodes exactly one JSON value, preserving numbers as
// json.Number so integer checks stay exact. Trailing non-whitespace data is an
// error, as are malformed documents.
func decodeJSONValue(data []byte) (any, error) {
	if err := validateUnicodeEscapes(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("invalid JSON: trailing data after message")
		}
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return v, nil
}

// describe renders an invalid value for error messages, mirroring the
// TypeScript parser's compact JSON rendering.
func describe(v any) string {
	if b, err := json.Marshal(v); err == nil {
		s := string(b)
		if len(s) <= 80 {
			return s
		}
		return s[:77] + "..."
	}
	return fmt.Sprintf("%v", v)
}

// utf16Len reports the JavaScript string length (UTF-16 code units) so
// length limits match the TypeScript parser for non-ASCII text.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// timestamp is the strict wire timestamp grammar shared by Go and TypeScript:
//
//	YYYY-MM-DDTHH:MM:SS(.fraction of 1-9 digits)?(Z|±HH:MM)
//
// Calendar and clock fields must be in range and the offset must be within
// ±23:59. Fraction digits beyond milliseconds are truncated, so ordering and
// the sanity window are millisecond precision.
var minTimestamp = time.Date(minTimestampYear, 1, 1, 0, 0, 0, 0, time.UTC)
var maxTimestamp = time.Date(maxTimestampYear, 1, 1, 0, 0, 0, 0, time.UTC)

// parseTimestampValue parses a wire timestamp and checks the sanity window. The
// returned instant is in UTC at millisecond precision.
func parseTimestampValue(s string) (time.Time, bool) {
	// The shortest accepted form is "YYYY-MM-DDTHH:MM:SSZ".
	if len(s) < 20 {
		return time.Time{}, false
	}
	if s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' {
		return time.Time{}, false
	}
	year, okYear := parseTimestampDigits(s[0:4])
	month, okMonth := parseTimestampDigits(s[5:7])
	day, okDay := parseTimestampDigits(s[8:10])
	hour, okHour := parseTimestampDigits(s[11:13])
	minute, okMinute := parseTimestampDigits(s[14:16])
	second, okSecond := parseTimestampDigits(s[17:19])
	if !okYear || !okMonth || !okDay || !okHour || !okMinute || !okSecond {
		return time.Time{}, false
	}
	if month < 1 || month > 12 || day < 1 || day > timestampDaysInMonth(year, month) ||
		hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, false
	}

	rest := s[19:]
	millisecond := 0
	if len(rest) > 1 && rest[0] == '.' {
		end := 1
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		digits := end - 1
		if digits < 1 || digits > 9 {
			return time.Time{}, false
		}
		for i := 0; i < 3; i++ {
			digit := 0
			if i < digits {
				digit = int(rest[1+i] - '0')
			}
			millisecond = millisecond*10 + digit
		}
		rest = rest[end:]
	}

	offsetSeconds := 0
	switch {
	case rest == "Z":
	case len(rest) == 6 && (rest[0] == '+' || rest[0] == '-') && rest[3] == ':':
		offsetHour, okHour := parseTimestampDigits(rest[1:3])
		offsetMinute, okMinute := parseTimestampDigits(rest[4:6])
		if !okHour || !okMinute || offsetHour > 23 || offsetMinute > 59 {
			return time.Time{}, false
		}
		offsetSeconds = (offsetHour*60 + offsetMinute) * 60
		if rest[0] == '-' {
			offsetSeconds = -offsetSeconds
		}
	default:
		return time.Time{}, false
	}

	t := time.Date(year, time.Month(month), day, hour, minute, second, millisecond*1_000_000, time.UTC)
	t = t.Add(-time.Duration(offsetSeconds) * time.Second)
	if t.Before(minTimestamp) || t.After(maxTimestamp) {
		return time.Time{}, false
	}
	return t, true
}

// parseTimestampDigits parses a fixed-width run of ASCII digits.
func parseTimestampDigits(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// timestampDaysInMonth returns the length of a Gregorian month.
func timestampDaysInMonth(year, month int) int {
	switch month {
	case 4, 6, 9, 11:
		return 30
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	default:
		return 31
	}
}

// validateUnicodeEscapes rejects JSON strings containing lone UTF-16 surrogate
// escapes. encoding/json would silently replace them with U+FFFD, which would
// change identity strings such as collectorId.
func validateUnicodeEscapes(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			i++
			if i >= len(data) {
				// A trailing escape is malformed JSON; the decoder reports it.
				return nil
			}
			if data[i] != 'u' || i+4 >= len(data) {
				continue
			}
			high, ok := parseHex4(data[i+1 : i+5])
			if !ok {
				continue
			}
			switch {
			case high >= 0xD800 && high <= 0xDBFF:
				if i+10 < len(data) && data[i+5] == '\\' && data[i+6] == 'u' {
					if low, ok := parseHex4(data[i+7 : i+11]); ok && low >= 0xDC00 && low <= 0xDFFF {
						i += 10
						continue
					}
				}
				return fmt.Errorf("invalid JSON: lone UTF-16 surrogate escape \\u%04X", high)
			case high >= 0xDC00 && high <= 0xDFFF:
				return fmt.Errorf("invalid JSON: lone UTF-16 surrogate escape \\u%04X", high)
			}
			i += 4
		}
	}
	return nil
}

// parseHex4 parses exactly four hexadecimal digits.
func parseHex4(s []byte) (int, bool) {
	n := 0
	for _, c := range s {
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return 0, false
		}
		n = n*16 + v
	}
	return n, true
}
