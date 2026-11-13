package gpu

import (
	"errors"
	"io/fs"
	"math"
	"strconv"
	"strings"
)

// The exact nvidia-smi queries of the prototype. Changing these changes the
// CSV column order the parsers below expect.
const (
	gpuQuery = "--query-gpu=uuid,name,pci.bus_id,utilization.gpu,memory.used," +
		"memory.total,temperature.gpu,power.draw"
	computeAppsQuery = "--query-compute-apps=pid,gpu_uuid,used_memory"
	csvFormat        = "--format=csv,noheader,nounits"
)

// GPUQueryArgs returns the argument list of the GPU query, for diagnostics
// and for tests that assert what the poller runs.
func GPUQueryArgs() []string {
	return append(strings.Split(gpuQuery, " "), csvFormat)
}

// ComputeAppsQueryArgs returns the argument list of the compute-app query.
func ComputeAppsQueryArgs() []string {
	return append(strings.Split(computeAppsQuery, " "), csvFormat)
}

// ParsedGPURow is one parsed `--query-gpu` row. nil means the field was
// reported as absent (`[N/A]`, `[Not Supported]`, empty).
type ParsedGPURow struct {
	UUID               string
	Name               string
	PCIBusID           *string
	UtilizationPercent *float64
	MemoryUsedBytes    *int64
	MemoryTotalBytes   *int64
	TemperatureC       *float64
	PowerWatts         *float64
}

// ParsedComputeApp is one parsed `--query-compute-apps` row.
type ParsedComputeApp struct {
	PID       int
	GPUUUID   string
	UsedBytes int64
}

// isNullToken reports whether an nvidia-smi cell means "no value". The
// prototype's exact token set is kept: real cards answer with different
// spellings depending on driver and field.
func isNullToken(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "n/a", "[n/a]", "na", "not supported", "[not supported]",
		"unknown", "[unknown]", "[unknown error]", "unknown error":
		return true
	}
	return false
}

// jsNumber parses a cell the way JavaScript's Number() does for the shapes
// nvidia-smi emits: surrounding whitespace is ignored and an empty cell is
// zero. Anything else that is not finite is invalid.
func jsNumber(raw string) (float64, bool) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, true
	}
	if len(text) > 2 && text[0] == '0' && (text[1] == 'x' || text[1] == 'X') {
		value, err := strconv.ParseUint(text[2:], 16, 64)
		if err != nil {
			return 0, false
		}
		return float64(value), true
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, the bound the
// prototype used to reject nonsensical conversions.
const maxSafeInteger = float64(1<<53 - 1)

func nullableNumber(raw string) *float64 {
	if isNullToken(raw) {
		return nil
	}
	value, ok := jsNumber(raw)
	if !ok {
		return nil
	}
	return pointer(value)
}

func nullableText(raw string) *string {
	if isNullToken(raw) {
		return nil
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	return &value
}

func mibToBytes(raw string) *int64 {
	mib := nullableNumber(raw)
	if mib == nil || *mib < 0 {
		return nil
	}
	bytes := math.Round(*mib * 1024 * 1024)
	if bytes > maxSafeInteger || bytes < -maxSafeInteger {
		return nil
	}
	return pointer(int64(bytes))
}

// ParseGPUCSV parses
// `--query-gpu=uuid,name,pci.bus_id,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw`
// with `--format=csv,noheader,nounits`.
//
// Rows with fewer than three fields or no UUID are skipped; a repeated UUID
// keeps its first row, so a driver that lists a device twice cannot produce
// duplicate resource ids.
func ParseGPUCSV(stdout string) []ParsedGPURow {
	rows := make([]ParsedGPURow, 0)
	seen := make(map[string]struct{})
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) < 3 {
			continue
		}
		uuid := nullableText(fields[0])
		if uuid == nil {
			continue
		}
		if _, duplicate := seen[*uuid]; duplicate {
			continue
		}
		seen[*uuid] = struct{}{}
		name := "NVIDIA GPU"
		if parsed := nullableText(fields[1]); parsed != nil {
			name = *parsed
		}
		rows = append(rows, ParsedGPURow{
			UUID:               *uuid,
			Name:               name,
			PCIBusID:           nullableText(fields[2]),
			UtilizationPercent: nullableNumber(fieldAt(fields, 3)),
			MemoryUsedBytes:    mibToBytes(fieldAt(fields, 4)),
			MemoryTotalBytes:   mibToBytes(fieldAt(fields, 5)),
			TemperatureC:       nullableNumber(fieldAt(fields, 6)),
			PowerWatts:         nullableNumber(fieldAt(fields, 7)),
		})
	}
	return rows
}

func fieldAt(fields []string, index int) string {
	if index < len(fields) {
		return fields[index]
	}
	return ""
}

// ParseComputeAppsCSV parses `--query-compute-apps=pid,gpu_uuid,used_memory`.
// Rows with a non-integral PID, a missing GPU UUID or an unreadable memory
// cell are skipped: a half-parsed row would attribute memory to the wrong
// process.
func ParseComputeAppsCSV(stdout string) []ParsedComputeApp {
	apps := make([]ParsedComputeApp, 0)
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) < 3 {
			continue
		}
		pidValue, ok := jsNumber(fields[0])
		if !ok || pidValue != math.Trunc(pidValue) || pidValue < 0 || pidValue > maxSafeInteger {
			continue
		}
		gpuUUID := nullableText(fields[1])
		used := mibToBytes(fields[2])
		if gpuUUID == nil || used == nil {
			continue
		}
		apps = append(apps, ParsedComputeApp{
			PID:       int(pidValue),
			GPUUUID:   *gpuUUID,
			UsedBytes: *used,
		})
	}
	return apps
}

// isNotFound reports whether a command failed because the executable does
// not exist. The process runner wraps [fs.ErrNotExist]; the message check
// keeps parity with the prototype for runners that only report text.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "enoent") || strings.Contains(message, "not found")
}
