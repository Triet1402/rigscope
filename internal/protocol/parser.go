package protocol

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// parser carries the JSON path of the value being checked so errors name the
// offending field (for example
// "samples.records[3].payload.cpu.usagePercent: 150 outside 0..100").
type parser struct {
	path []string
}

func newParser() *parser {
	return &parser{}
}

// child returns a parser whose path is extended by one field or segment.
func (p *parser) child(seg string) *parser {
	next := make([]string, len(p.path), len(p.path)+1)
	copy(next, p.path)
	return &parser{path: append(next, seg)}
}

// at returns a parser whose last path segment is indexed, e.g. records[3].
func (p *parser) at(i int) *parser {
	next := make([]string, len(p.path))
	copy(next, p.path)
	if len(next) == 0 {
		next = append(next, fmt.Sprintf("[%d]", i))
	} else {
		next[len(next)-1] = fmt.Sprintf("%s[%d]", next[len(next)-1], i)
	}
	return &parser{path: next}
}

// errf formats an error prefixed with the current path.
func (p *parser) errf(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if len(p.path) == 0 {
		return fmt.Errorf("%s", msg)
	}
	return fmt.Errorf("%s: %s", strings.Join(p.path, "."), msg)
}

func (p *parser) object(v any) (map[string]any, error) {
	o, ok := v.(map[string]any)
	if !ok {
		return nil, p.errf("expected object, got %s", describe(v))
	}
	return o, nil
}

// keys enforces an exact key set: unexpected fields are rejected and required
// fields must be present. A JSON null counts as present, matching the
// TypeScript parser, and then fails its value check.
func (p *parser) keys(o map[string]any, required, optional []string) error {
	allowed := make(map[string]struct{}, len(required)+len(optional))
	for _, k := range required {
		allowed[k] = struct{}{}
	}
	for _, k := range optional {
		allowed[k] = struct{}{}
	}
	unexpected := make([]string, 0)
	for k := range o {
		if _, ok := allowed[k]; !ok {
			unexpected = append(unexpected, k)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		return p.errf("unexpected field %q", unexpected[0])
	}
	for _, k := range required {
		if _, ok := o[k]; !ok {
			return p.errf("missing field %q", k)
		}
	}
	return nil
}

func (p *parser) array(v any, min, max int) ([]any, error) {
	a, ok := v.([]any)
	if !ok {
		return nil, p.errf("expected array")
	}
	if len(a) < min || len(a) > max {
		return nil, p.errf("length %d outside %d..%d", len(a), min, max)
	}
	return a, nil
}

func (p *parser) boolean(v any) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, p.errf("expected boolean, got %s", describe(v))
	}
	return b, nil
}

func (p *parser) oneOf(v any, options ...string) (string, error) {
	s, ok := v.(string)
	if ok {
		for _, option := range options {
			if s == option {
				return s, nil
			}
		}
	}
	return "", p.errf("unexpected value %s", describe(v))
}

func (p *parser) text(v any, min, max int) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", p.errf("expected string, got %s", describe(v))
	}
	if n := utf16Len(s); n < min || n > max {
		return "", p.errf("length %d outside %d..%d", n, min, max)
	}
	return s, nil
}

// id reads a 1..MaxIDLength identity string.
func (p *parser) id(v any) (string, error) {
	return p.text(v, 1, MaxIDLength)
}

// resourceID reads a 1..MaxResourceIDLength stable resource id.
func (p *parser) resourceID(v any) (string, error) {
	return p.text(v, 1, MaxResourceIDLength)
}

// integer reads a JavaScript-safe integer within min..max. A numerically
// integral JSON number written as 1.0 or 1e3 is accepted, as it is in
// TypeScript.
func (p *parser) integer(v any, min, max int64) (int64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, p.errf("expected safe integer, got %s", describe(v))
	}
	f, err := parseJSONNumber(n)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f) || f < float64(-MaxSafeInteger) || f > float64(MaxSafeInteger) {
		return 0, p.errf("expected safe integer, got %s", describe(n))
	}
	i := int64(f)
	if i < min || i > max {
		return 0, p.errf("%d outside %d..%d", i, min, max)
	}
	return i, nil
}

// number reads a finite JSON number within min..max.
func (p *parser) number(v any, min, max float64) (float64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, p.errf("expected finite number, got %s", describe(v))
	}
	f, err := parseJSONNumber(n)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, p.errf("expected finite number, got %s", describe(n))
	}
	if f < min || f > max {
		return 0, p.errf("%s outside %s..%s", formatNumber(f), formatNumber(min), formatNumber(max))
	}
	return f, nil
}

// parseJSONNumber converts a syntactically valid JSON number. An underflow to
// zero (for example 1e-400) is zero in JavaScript and is accepted the same way.
func parseJSONNumber(n json.Number) (float64, error) {
	f, err := strconv.ParseFloat(n.String(), 64)
	if err != nil && f == 0 {
		return 0, nil
	}
	return f, err
}

func formatNumber(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// timestamp reads a bounded, parseable timestamp string.
func (p *parser) timestamp(v any) (string, error) {
	s, err := p.text(v, 1, MaxTimestampLength)
	if err != nil {
		return "", err
	}
	if _, ok := parseTimestampValue(s); !ok {
		return "", p.errf("not a parseable timestamp")
	}
	return s, nil
}

// nullableText reads a required-nullable string field.
func (p *parser) nullableText(v any, min, max int) (*string, error) {
	if v == nil {
		return nil, nil
	}
	s, err := p.text(v, min, max)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// nullableInteger reads a required-nullable integer field.
func (p *parser) nullableInteger(v any, min, max int64) (*int64, error) {
	if v == nil {
		return nil, nil
	}
	i, err := p.integer(v, min, max)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// nullableNumber reads a required-nullable number field.
func (p *parser) nullableNumber(v any, min, max float64) (*float64, error) {
	if v == nil {
		return nil, nil
	}
	f, err := p.number(v, min, max)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// nullableTimestamp reads a required-nullable timestamp field.
func (p *parser) nullableTimestamp(v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	s, err := p.timestamp(v)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// nullableBool reads a required-nullable boolean field.
func (p *parser) nullableBool(v any) (*bool, error) {
	if v == nil {
		return nil, nil
	}
	b, err := p.boolean(v)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// optional parses an optional-nullable field. A missing key returns nil; a
// present null returns Optional with Valid == false; any other value is parsed
// and returned with Valid == true.
func optional[T any](p *parser, o map[string]any, name string, parse func(*parser, any) (T, error)) (*Optional[T], error) {
	v, ok := o[name]
	if !ok {
		return nil, nil
	}
	field := p.child(name)
	if v == nil {
		return Null[T](), nil
	}
	value, err := parse(field, v)
	if err != nil {
		return nil, err
	}
	return Some(value), nil
}

func optionalNumber(p *parser, o map[string]any, name string, min, max float64) (*Optional[float64], error) {
	return optional(p, o, name, func(fp *parser, v any) (float64, error) {
		return fp.number(v, min, max)
	})
}

func optionalInteger(p *parser, o map[string]any, name string, min, max int64) (*Optional[int64], error) {
	return optional(p, o, name, func(fp *parser, v any) (int64, error) {
		return fp.integer(v, min, max)
	})
}
