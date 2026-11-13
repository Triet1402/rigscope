package activation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

func TestReadinessRejectsAnActivatingUnit(t *testing.T) {
	services := systemd.NewFake()
	services.Active[HubUnit] = true
	services.Running[HubUnit] = false
	probed := false
	readiness := NewComponentReadiness(ReadinessOptions{
		Services: services,
		Settings: staticSettings{value: Settings{HubEnabled: true, HubListen: "127.0.0.1", HubPort: 8081}},
		Probe: func(context.Context, string, string, string, string) error {
			probed = true
			return nil
		},
		Attempts: 1,
		Sleep:    func(time.Duration) {},
	})
	err := readiness.Check(context.Background(), "1.1.0")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("activating unit was accepted: %v", err)
	}
	if probed {
		t.Fatal("readiness probed a unit that had not finished starting")
	}
	services.Running[HubUnit] = true
	if err := readiness.Check(context.Background(), "1.1.0"); err != nil {
		t.Fatalf("running unit was rejected: %v", err)
	}
}
