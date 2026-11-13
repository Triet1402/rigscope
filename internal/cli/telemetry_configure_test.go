package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

func TestConfigureTelemetryMode(t *testing.T) {
	for _, scenario := range []string{"restart", "decline", "no-restart", "invalid", "unchanged"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			paths := f.install(t)
			before, err := os.ReadFile(paths.ConfigFile)
			if err != nil {
				t.Fatal(err)
			}
			f.fake.Reset()
			f.prompts = nil
			f.out.Reset()
			args := []string{"configure", "--telemetry-mode", "persistent"}
			wantCode := 0
			switch scenario {
			case "decline":
				f.answer = false
				wantCode = 1
			case "no-restart":
				args = append(args, "--no-restart", "--yes")
			case "invalid":
				args[2] = "disk"
				wantCode = 1
			case "unchanged":
				args[2] = "memory"
			}
			if code := f.run(t, args...); code != wantCode {
				t.Fatalf("exit %d: %s", code, f.err.String())
			}
			if scenario == "decline" || scenario == "invalid" || scenario == "unchanged" {
				after, err := os.ReadFile(paths.ConfigFile)
				if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) {
					t.Fatalf("unexpected changes: %v", f.fake.Methods())
				}
				for _, component := range installComponents {
					for _, method := range []string{"stop", "start", "restart", "enable", "disable"} {
						if f.fake.HasCall(method, component.UnitName()) {
							t.Fatalf("unexpected mutation: %v", f.fake.Methods())
						}
					}
				}
				return
			}
			if got := loadConfig(t, paths).Components.Hub.TelemetryMode; got != "persistent" {
				t.Fatalf("mode = %s", got)
			}
			if !strings.Contains(f.out.String(), "RAM history is not copied") {
				t.Fatalf("missing warning: %s", f.out.String())
			}
			if scenario == "restart" {
				if len(f.prompts) != 1 || !f.fake.HasCall("restart", systemd.Hub.UnitName()) {
					t.Fatalf("prompts %v, calls %v", f.prompts, f.fake.Methods())
				}
				f.fake.Reset()
				f.prompts = nil
				if code := f.run(t, "configure", "--telemetry-mode", "memory", "--yes"); code != 0 {
					t.Fatalf("switch back: %s", f.err.String())
				}
				if loadConfig(t, paths).Components.Hub.TelemetryMode != "memory" || len(f.prompts) != 0 {
					t.Fatal("switch back failed")
				}
			} else if !f.fake.HasCall("stop", systemd.Hub.UnitName()) || f.fake.HasCall("restart", systemd.Hub.UnitName()) || f.fake.HasCall("start", systemd.Hub.UnitName()) {
				t.Fatal("hub restarted with --no-restart")
			}
		})
	}
}
