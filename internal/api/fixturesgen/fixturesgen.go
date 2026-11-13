// Package fixturesgen re-encodes the captured browser API fixtures with the
// strict Go decoders and encoders and constructs a few response variants that
// the Node capture cannot produce deterministically. `go generate ./internal/api`
// writes the result to testdata/api/reencoded; a test fails when the committed
// directory is stale.
package fixturesgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Triet1402/rigscope/internal/api"
	"github.com/Triet1402/rigscope/internal/protocol"
)

const (
	protocolFixtures = "../../testdata/protocol/valid"
)

type manifestEntry struct {
	Path      string `json:"path"`
	Validator string `json:"validator"`
	Shape     string `json:"shape"`
}

// Generate reads every response fixture under src and writes its re-encoded
// form plus constructed edge cases under dst. Files in dst that are no longer
// produced are removed so the directory is an exact drift baseline.
func Generate(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	written := make(map[string]struct{})
	manifest := make(map[string]manifestEntry)
	for _, entry := range entries {
		if entry.IsDir() {
			if entry.Name() == "reencoded" {
				continue
			}
			if entry.Name() == "live" {
				if err := generateLive(filepath.Join(src, entry.Name()), dst, written, manifest); err != nil {
					return err
				}
				continue
			}
			if err := generateDir(filepath.Join(src, entry.Name()), entry.Name(), dst, written, manifest); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == "manifest.json" || entry.Name() == "index.json" {
			continue
		}
		if err := writeReencoded(src, "", entry.Name(), dst, written, manifest); err != nil {
			return err
		}
	}
	if err := generateConstructed(src, dst, written, manifest); err != nil {
		return err
	}
	if err := writeManifest(dst, written, manifest); err != nil {
		return err
	}
	return prune(dst, written)
}

func generateDir(dir, prefix, dst string, written map[string]struct{}, manifest map[string]manifestEntry) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := writeReencoded(dir, prefix, entry.Name(), dst, written, manifest); err != nil {
			return err
		}
	}
	return nil
}

func generateLive(dir, dst string, written map[string]struct{}, manifest map[string]manifestEntry) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		var envelope struct {
			ID    string          `json:"id"`
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return fmt.Errorf("live/%s: %w", entry.Name(), err)
		}
		snapshot, err := api.DecodeHubSnapshot(envelope.Data)
		if err != nil {
			return fmt.Errorf("live/%s: %w", entry.Name(), err)
		}
		encoded, err := api.EncodeHubSnapshot(snapshot)
		if err != nil {
			return fmt.Errorf("live/%s: %w", entry.Name(), err)
		}
		out := struct {
			ID    string          `json:"id"`
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
		}{ID: envelope.ID, Event: envelope.Event, Data: encoded}
		rel := filepath.ToSlash(filepath.Join("live", entry.Name()))
		if err := writeJSON(filepath.Join(dst, rel), out, written); err != nil {
			return err
		}
		manifest[rel] = manifestEntry{Path: rel, Validator: "HubSnapshot", Shape: "sse-envelope"}
	}
	return nil
}

func writeReencoded(dir, prefix, name, dst string, written map[string]struct{}, manifest map[string]manifestEntry) error {
	path := name
	if prefix != "" {
		path = filepath.Join(prefix, name)
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	encoded, validator, err := reencode(path, data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	rel := filepath.ToSlash(path)
	manifest[rel] = manifestEntry{Path: rel, Validator: validator, Shape: "json"}
	return writeRaw(filepath.Join(dst, path), encoded, written)
}

func reencode(path string, data []byte) ([]byte, string, error) {
	switch path {
	case "health/get.json":
		return pairFor(data, api.DecodeHealthResponse, api.EncodeHealthResponse, "HealthResponse")
	case "auth/login.json", "auth/session.json":
		return pairFor(data, api.DecodeSessionResponse, api.EncodeSessionResponse, "SessionResponse")
	case "auth/session-unauthorized.json", "auth/login-invalid.json", "errors/unauthorized.json":
		return pairFor(data, api.DecodeAPIError, api.EncodeAPIError, "APIError")
	case "auth/logout.json", "admin/users-upsert.json", "admin/users-delete.json",
		"admin/collectors-revoke.json", "processes/release.json", "alerts/rule-delete.json":
		return pairFor(data, api.DecodeOKResponse, api.EncodeOKResponse, "OKResponse")
	case "enrollment/enroll.json":
		return pairFor(data, api.DecodeEnrollmentIdentity, api.EncodeEnrollmentIdentity, "EnrollmentIdentity")
	case "admin/users.json":
		return pairFor(data, api.DecodeAdminUserList, api.EncodeAdminUserList, "AdminUserList")
	case "admin/audit.json":
		return pairFor(data, api.DecodeAuditEventList, api.EncodeAuditEventList, "AuditEventList")
	case "admin/collectors.json":
		return pairFor(data, api.DecodeCollectorList, api.EncodeCollectorList, "CollectorList")
	case "admin/collector-grants.json":
		return pairFor(data, api.DecodeGrantResponse, api.EncodeGrantResponse, "GrantResponse")
	case "machines/meta.json":
		return pairFor(data, api.DecodeMachineMeta, api.EncodeMachineMeta, "MachineMeta")
	case "alerts/list.json", "alerts/list-active.json":
		return pairFor(data, api.DecodeAlertListResponse, api.EncodeAlertListResponse, "AlertListResponse")
	case "alerts/rule-create.json", "alerts/rule-update.json":
		return pairFor(data, api.DecodeAlertRuleResponse, api.EncodeAlertRuleResponse, "AlertRuleResponse")
	case "alerts/webhook.json", "alerts/webhook-configured.json":
		return pairFor(data, api.DecodeAlertWebhookResponse, api.EncodeAlertWebhookResponse, "AlertWebhookResponse")
	case "alerts/webhook-test.json":
		return pairFor(data, api.DecodeAlertWebhookTestResponse, api.EncodeAlertWebhookTestResponse, "AlertWebhookTestResponse")
	case "history/status.json", "history/status-persistent.json":
		return pairFor(data, api.DecodeHistoryStatus, api.EncodeHistoryStatus, "HistoryStatus")
	case "history/metrics.json":
		return pairFor(data, api.DecodeMetricCatalogResponse, api.EncodeMetricCatalogResponse, "MetricCatalogResponse")
	case "history/query.json", "history/query-unavailable.json":
		return pairFor(data, api.DecodeHistoryResult, api.EncodeHistoryResult, "HistoryResult")
	case "history/invalid-query.json":
		return pairFor(data, api.DecodeAPIError, api.EncodeAPIError, "APIError")
	case "processes/acquire.json":
		return pairFor(data, api.DecodeProcessAcquireResponse, api.EncodeProcessAcquireResponse, "ProcessAcquireResponse")
	case "processes/read.json", "processes/read-with-processes.json":
		return pairFor(data, api.DecodeProcessReadResponse, api.EncodeProcessReadResponse, "ProcessReadResponse")
	case "processes/budget-error.json":
		return pairFor(data, api.DecodeAPIError, api.EncodeAPIError, "APIError")
	default:
		return nil, "", fmt.Errorf("no re-encoder for fixture")
	}
}

func pairFor[T any](data []byte, decode func([]byte) (T, error), encode func(T) ([]byte, error), validator string) ([]byte, string, error) {
	value, err := decode(data)
	if err != nil {
		return nil, "", err
	}
	encoded, err := encode(value)
	return encoded, validator, err
}

func generateConstructed(src, dst string, written map[string]struct{}, manifest map[string]manifestEntry) error {
	health := api.HealthResponse{Status: "ok", Version: "1.0.0", Mode: api.HealthModeGo, HubRunID: "hub-constructed"}
	if err := writeConstructed(dst, written, manifest, "constructed/health.json", "HealthResponse", func() ([]byte, error) {
		return api.EncodeHealthResponse(health)
	}); err != nil {
		return err
	}

	status := persistentStatus()
	if err := writeConstructed(dst, written, manifest, "constructed/history-status-persistent.json", "HistoryStatus", func() ([]byte, error) {
		return api.EncodeHistoryStatus(status)
	}); err != nil {
		return err
	}

	result := unavailableHistory()
	if err := writeConstructed(dst, written, manifest, "constructed/history-query-unavailable.json", "HistoryResult", func() ([]byte, error) {
		return api.EncodeHistoryResult(result)
	}); err != nil {
		return err
	}

	list, err := constructedAlerts(src)
	if err != nil {
		return err
	}
	if err := writeConstructed(dst, written, manifest, "constructed/alerts-list-incident.json", "AlertListResponse", func() ([]byte, error) {
		return api.EncodeAlertListResponse(list)
	}); err != nil {
		return err
	}

	testResponse := api.AlertWebhookTestResponse{
		Result: api.AlertWebhookTestResult{
			OK: true, Status: int64Ptr(204), Error: nil, TestedAt: "2026-11-12T08:10:00.000Z",
		},
		Status: list.Webhook,
	}
	if err := writeConstructed(dst, written, manifest, "constructed/alerts-webhook-test.json", "AlertWebhookTestResponse", func() ([]byte, error) {
		return api.EncodeAlertWebhookTestResponse(testResponse)
	}); err != nil {
		return err
	}

	detail, err := protocolDetail()
	if err != nil {
		return err
	}
	processRead := api.ProcessReadResponse{State: api.ProcessStateReady, Result: &detail}
	if err := writeConstructed(dst, written, manifest, "constructed/processes-read-with-processes.json", "ProcessReadResponse", func() ([]byte, error) {
		return api.EncodeProcessReadResponse(processRead)
	}); err != nil {
		return err
	}

	budget := api.APIError{Error: "Process viewer budget exhausted. Close another Processes view and retry.", Code: protocol.Ptr(api.ErrorCodeProcessViewerBudget)}
	if err := writeConstructed(dst, written, manifest, "constructed/errors-process-budget.json", "APIError", func() ([]byte, error) {
		return api.EncodeAPIError(budget)
	}); err != nil {
		return err
	}

	meta := api.MachineMeta{DisplayName: protocol.Ptr("Fixture machine"), Tags: []string{"lab", "rig"}}
	if err := writeConstructed(dst, written, manifest, "constructed/machines-meta-tags.json", "MachineMeta", func() ([]byte, error) {
		return api.EncodeMachineMeta(meta)
	}); err != nil {
		return err
	}

	return nil
}

func persistentStatus() api.HistoryStatus {
	lastFlush := int64(1794470876011)
	return api.HistoryStatus{
		Mode:              api.HistoryModePersistent,
		PersistentHistory: true,
		AvailableFromMs:   int64Ptr(1794470876011),
		AvailableToMs:     int64Ptr(1794470880145),
		Memory:            nil,
		Persistence: &api.PersistentHistoryStatus{
			Enabled:             true,
			StorageTier:         api.StorageTierSummary,
			FlushIntervalMs:     5000,
			QueuedRecords:       0,
			QueuedBatches:       0,
			LastFlushMs:         &lastFlush,
			LastFlushDurationMs: int64Ptr(12),
			WriteFailures:       0,
			LastWriteError:      nil,
			DBBytes:             49152,
			DBWalBytes:          0,
			DBBytesLimit:        int64Ptr(1 << 30),
			Tiers: []api.HistoryTierStatus{
				{Tier: api.HistoryTierRaw, StepMs: 1000, RetentionMs: 172800000, Bytes: 65536},
				{Tier: api.HistoryTierTenSecond, StepMs: 10000, RetentionMs: 7200000, Bytes: 32768},
			},
		},
		Gaps:            0,
		AcceptedRecords: 12,
		GeneratedAt:     "2026-11-12T08:10:00.000Z",
	}
}

func unavailableHistory() api.HistoryResult {
	return api.HistoryResult{
		MachineID: "fixture-machine-1",
		FromMs:    1794470820000,
		ToMs:      1794470880000,
		StepMs:    1000,
		Tier:      api.HistoryTierOneSecond,
		Series: []api.HistorySeries{{
			Metric:     "cpu.usage",
			ResourceID: "",
			Unit:       api.MetricUnitPercent,
			Points: []api.HistoryPoint{
				{T: 1794470876000, Min: nil, Max: nil, Avg: nil},
				{T: 1794470877000, Min: protocol.Ptr(1.5), Max: protocol.Ptr(2.5), Avg: protocol.Ptr(2.0)},
			},
		}},
		Gaps: []api.HistoryGap{{
			FromMs: 1794470876000,
			ToMs:   1794470877000,
			Kind:   api.GapKindMissing,
			Reason: protocol.Ptr("expected interval missed"),
		}},
		Truncated: false,
		ElapsedMs: 1.25,
	}
}

func constructedAlerts(src string) (api.AlertListResponse, error) {
	base, err := os.ReadFile(filepath.Join(src, "alerts", "list.json"))
	if err != nil {
		return api.AlertListResponse{}, err
	}
	list, err := api.DecodeAlertListResponse(base)
	if err != nil {
		return api.AlertListResponse{}, err
	}
	threshold := float64(90)
	lastValue := float64(93.5)
	rule := api.AlertRule{
		ID: "fixture-rule-1", Name: "Fixture CPU high", MachineID: "fixture-machine-1",
		Metric: api.AlertMetricCPUUsage, ResourceID: "", Condition: api.AlertConditionAbove,
		Threshold: &threshold, DurationMs: 60000, Enabled: true, MaintenanceUntil: nil,
		State: api.AlertRuleStateFiring, PendingSince: nil, ActiveIncidentID: protocol.Ptr("fixture-incident-1"),
		LastValue: &lastValue, LastObservedAt: protocol.Ptr("2026-11-12T08:09:58.000Z"),
		CreatedAt: "2026-11-12T08:09:00.000Z", UpdatedAt: "2026-11-12T08:09:58.000Z",
	}
	startedValue := float64(93.1)
	incident := api.AlertIncident{
		ID: "fixture-incident-1", RuleID: rule.ID, RuleName: rule.Name, MachineID: rule.MachineID,
		Metric: rule.Metric, ResourceID: "", Condition: rule.Condition, Status: api.AlertIncidentOpen,
		StartedAt: "2026-11-12T08:09:58.000Z", ResolvedAt: nil, StartedValue: &startedValue,
		ResolvedValue: nil, LastObservedAt: "2026-11-12T08:09:58.000Z", Message: "Fixture CPU high: cpu.usage 93.5%",
	}
	notification := api.AlertNotification{
		ID: "fixture-notification-1", RuleID: rule.ID, IncidentID: incident.ID,
		Event: api.AlertEventFired, Status: api.AlertJobDelivered, Attempts: 1, MaxAttempts: 8,
		NextAttemptAt: nil, LastAttemptAt: protocol.Ptr("2026-11-12T08:09:59.000Z"),
		LastError: nil, CreatedAt: "2026-11-12T08:09:58.000Z",
		DeliveredAt: protocol.Ptr("2026-11-12T08:09:59.000Z"),
	}
	transport := api.AlertWebhookTransportLoopbackHTTP
	testedAt := "2026-11-12T08:10:00.000Z"
	lastTestOK := true
	list.Rules = []api.AlertRule{rule}
	list.Incidents = []api.AlertIncident{incident}
	list.Notifications = api.AlertNotificationSummary{
		Pending: 0, Delivered: 1, Failed: 0,
		Recent:         []api.AlertNotification{notification},
		LastDeliveryAt: protocol.Ptr("2026-11-12T08:09:59.000Z"),
		LastError:      nil,
		Delivery:       api.DeliveryAtLeastOnceValue,
	}
	list.Webhook = api.AlertWebhookStatus{
		Channel: api.WebhookChannelValue, Configured: true, Enabled: true,
		Transport: &transport, LastTestAt: &testedAt, LastTestOK: &lastTestOK,
		LastError: nil, Delivery: api.DeliveryAtLeastOnceValue,
	}
	return list, nil
}

func protocolDetail() (protocol.DetailResult, error) {
	data, err := os.ReadFile(filepath.Join(protocolFixtures, "collector-detail-result.json"))
	if err != nil {
		return protocol.DetailResult{}, err
	}
	message, err := protocol.DecodeCollectorMessage(data)
	if err != nil {
		return protocol.DetailResult{}, err
	}
	detail, ok := message.(protocol.DetailResult)
	if !ok {
		return protocol.DetailResult{}, fmt.Errorf("protocol fixture is %T, not a detail result", message)
	}
	return detail, nil
}

func writeConstructed(dst string, written map[string]struct{}, manifest map[string]manifestEntry, rel, validator string, encode func() ([]byte, error)) error {
	data, err := encode()
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	path := filepath.Join(dst, rel)
	if err := writeRaw(path, data, written); err != nil {
		return err
	}
	manifest[rel] = manifestEntry{Path: rel, Validator: validator, Shape: "json"}
	return nil
}

func writeRaw(path string, data []byte, written map[string]struct{}) error {
	pretty, err := prettyJSON(data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, pretty, 0o644); err != nil {
		return err
	}
	written[path] = struct{}{}
	return nil
}

func writeJSON(path string, value any, written map[string]struct{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeRaw(path, data, written)
}

func prettyJSON(data []byte) ([]byte, error) {
	var buffer bytes.Buffer
	if err := json.Indent(&buffer, data, "", "  "); err != nil {
		return nil, err
	}
	buffer.WriteByte('\n')
	return buffer.Bytes(), nil
}

func writeManifest(dst string, written map[string]struct{}, manifest map[string]manifestEntry) error {
	entries := make([]manifestEntry, 0, len(manifest))
	for _, entry := range manifest {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(dst, "manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	written[path] = struct{}{}
	return nil
}

func prune(dst string, written map[string]struct{}) error {
	var keep []string
	for path := range written {
		keep = append(keep, path)
	}
	sort.Strings(keep)
	var found []string
	err := filepath.WalkDir(dst, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		found = append(found, path)
		return nil
	})
	if err != nil {
		return err
	}
	for _, path := range found {
		if _, ok := written[path]; !ok {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

func int64Ptr(value int64) *int64 { return &value }
