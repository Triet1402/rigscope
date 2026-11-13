package api

import (
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ParseHistoryQuery parses and validates the GET /api/history query string.
//
// Defaults follow the reference hub: metrics defaults to `cpu.usage,memory.used`
// and maxPoints to 600. An empty metrics list, an unknown metric, a duplicate
// metric/resource/core, a non-integer or out-of-range epoch millisecond, an
// inverted or wider-than-400-day range and a non-positive maxPoints are
// rejected with an InputError carrying HTTP 400.
func ParseHistoryQuery(values url.Values) (HistoryQuery, error) {
	machineID := values.Get("machineId")

	metrics := []string{"cpu.usage", "memory.used"}
	if raw, ok := values["metrics"]; ok {
		metrics = strings.Split(raw[0], ",")
	}

	var resourceIDs []string
	if raw, ok := values["resourceIds"]; ok {
		resourceIDs = strings.Split(raw[0], ",")
	}

	var coreIDs *[]string
	if raw, ok := values["coreIds"]; ok {
		ids := []string{}
		for _, id := range strings.Split(raw[0], ",") {
			if id != "" {
				ids = append(ids, id)
			}
		}
		coreIDs = &ids
	}

	from, err := queryEpoch(values, "from")
	if err != nil {
		return HistoryQuery{}, err
	}
	to, err := queryEpoch(values, "to")
	if err != nil {
		return HistoryQuery{}, err
	}

	maxPoints := DefaultHistoryPoints
	if raw, ok := values["maxPoints"]; ok {
		value, err := queryInteger(raw[0], "maxPoints")
		if err != nil {
			return HistoryQuery{}, err
		}
		// The reference hub clamps an oversized maxPoints instead of rejecting it.
		if value > MaxHistoryPoints {
			value = MaxHistoryPoints
		}
		maxPoints = int(value)
	}

	query := HistoryQuery{
		MachineID:   machineID,
		Metrics:     metrics,
		ResourceIDs: resourceIDs,
		CoreIDs:     coreIDs,
		FromMs:      from,
		ToMs:        to,
		MaxPoints:   maxPoints,
	}
	if err := query.validate(); err != nil {
		return HistoryQuery{}, inputErr("%s", err.Error())
	}
	return query, nil
}

func queryEpoch(values url.Values, name string) (int64, error) {
	// The reference handler writes Number(searchParams.get(name)); a missing
	// parameter therefore behaves like zero and fails range validation.
	raw, ok := values[name]
	if !ok {
		return 0, nil
	}
	return queryInteger(raw[0], name)
}

func queryInteger(text, name string) (int64, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return 0, inputErr("%s must be a safe integer", name)
	}
	if value < -9007199254740991 || value > 9007199254740991 {
		return 0, inputErr("%s must be a safe integer", name)
	}
	return int64(value), nil
}

// ProcessViewerQuery is the GET/POST/DELETE /api/processes query.
//
//api:skip
type ProcessViewerQuery struct {
	MachineID string
	ViewerID  string
}

// ParseProcessViewerQuery parses the process-detail query and enforces the
// same identifier bounds as the reference hub: machineId 1..128 characters and
// viewerId matching ^[a-zA-Z0-9-]{1,128}$.
func ParseProcessViewerQuery(values url.Values) (ProcessViewerQuery, error) {
	query := ProcessViewerQuery{
		MachineID: values.Get("machineId"),
		ViewerID:  values.Get("viewerId"),
	}
	if err := validateText(query.MachineID, "machineId", 1, MaxMachineIDQueryLength); err != nil {
		return ProcessViewerQuery{}, inputErr("%s", err.Error())
	}
	if !viewerIDPattern.MatchString(query.ViewerID) {
		return ProcessViewerQuery{}, inputErr("invalid viewer identity")
	}
	return query, nil
}

var viewerIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// QueryValue renders a HistoryQuery back into URL values, mainly for tests and
// clients. It is not used by the hub.
func (q HistoryQuery) QueryValue() url.Values {
	values := url.Values{}
	values.Set("machineId", q.MachineID)
	values.Set("metrics", strings.Join(q.Metrics, ","))
	if len(q.ResourceIDs) > 0 {
		values.Set("resourceIds", strings.Join(q.ResourceIDs, ","))
	}
	if q.CoreIDs != nil {
		values.Set("coreIds", strings.Join(*q.CoreIDs, ","))
	}
	values.Set("from", strconv.FormatInt(q.FromMs, 10))
	values.Set("to", strconv.FormatInt(q.ToMs, 10))
	values.Set("maxPoints", strconv.Itoa(q.MaxPoints))
	return values
}
