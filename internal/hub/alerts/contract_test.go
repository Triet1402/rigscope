package alerts

import (
	"context"
	"testing"

	"github.com/Triet1402/rigscope/internal/api"
)

// TestBrowserContractResponsesDecodeWithInternalAPI re-encodes every alert
// response through the contract encoder and decodes it with the browser
// decoder. A field, nullability or timestamp regression fails here even when
// the engine's own structs look correct.
func TestBrowserContractResponsesDecodeWithInternalAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rule, err := h.engine.CreateRule(ctx, h.authority, RuleInput{
		Name: "Contract CPU", MachineID: "m1", Metric: api.AlertMetricCPUUsage,
		Condition: api.AlertConditionAbove, Threshold: floatPtr(1), DurationMs: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	ruleBody, err := api.EncodeAlertRuleResponse(api.AlertRuleResponse{Rule: rule})
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := api.DecodeAlertRuleResponse(ruleBody); err != nil || decoded.Rule.ID != rule.ID {
		t.Fatalf("rule response = %+v, %v", decoded, err)
	}

	h.observe("m1", cpuPayload(99))
	h.process()
	list, err := h.engine.ListResponse(ctx, AllMachines(), true, 200)
	if err != nil {
		t.Fatal(err)
	}
	listBody, err := api.EncodeAlertListResponse(list)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := api.DecodeAlertListResponse(listBody)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.CanAdmin || len(decoded.Rules) != 1 || len(decoded.Incidents) != 1 {
		t.Fatalf("list = %+v", decoded)
	}
	if decoded.Incidents[0].StartedValue == nil || *decoded.Incidents[0].StartedValue != 99 {
		t.Fatalf("incident started value = %v", decoded.Incidents[0].StartedValue)
	}
	if decoded.Notifications.Delivered != 1 || len(decoded.Notifications.Recent) != 1 {
		t.Fatalf("notifications = %+v", decoded.Notifications)
	}
	if decoded.Rules[0].State != api.AlertRuleStateFiring || decoded.Rules[0].ActiveIncidentID == nil {
		t.Fatalf("rule = %+v", decoded.Rules[0])
	}

	status, err := h.engine.ConfigureWebhook(ctx, h.authority, WebhookConfig{URL: "http://127.0.0.1:9/sink"})
	if err != nil {
		t.Fatal(err)
	}
	webhookBody, err := api.EncodeAlertWebhookResponse(api.AlertWebhookResponse{Status: status})
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := api.DecodeAlertWebhookResponse(webhookBody); err != nil || !decoded.Status.Configured {
		t.Fatalf("webhook response = %+v, %v", decoded, err)
	}

	result, err := h.engine.TestWebhook(ctx, h.authority)
	if err != nil {
		t.Fatal(err)
	}
	testBody, err := api.EncodeAlertWebhookTestResponse(api.AlertWebhookTestResponse{Result: result, Status: status})
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := api.DecodeAlertWebhookTestResponse(testBody); err != nil || !decoded.Result.OK {
		t.Fatalf("test response = %+v, %v", decoded, err)
	}
}
