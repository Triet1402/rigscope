package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Triet1402/rigscope/internal/collector/enroll"
	"github.com/Triet1402/rigscope/internal/collector/state"
	"github.com/Triet1402/rigscope/internal/config"
)

const (
	hubA = "wss://hub-a.example/api/collector/connect"
	hubB = "wss://hub-b.example/api/collector/connect"
)

// enrolledCollector installs a collector that is enrolled with hub A.
func enrolledCollector(t *testing.T) (*fixture, config.Paths) {
	t.Helper()
	f := newFixture(t)
	paths := f.install(t, "--no-start", "--role", "collector", "--hub", "https://hub-a.example", "--token", "grant-a")
	saveCredential(t, paths, hubA)
	f.out.Reset()
	f.err.Reset()
	return f, paths
}

func saveCredential(t *testing.T, paths config.Paths, url string) {
	t.Helper()
	store, err := state.Open(paths.StateDir, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCredential(state.Credential{URL: url, Token: "credential"}); err != nil {
		t.Fatal(err)
	}
}

func destinationOf(t *testing.T, paths config.Paths) string {
	t.Helper()
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Components.Collector.Destinations) != 1 {
		t.Fatalf("destinations = %+v", cfg.Components.Collector.Destinations)
	}
	return cfg.Components.Collector.Destinations[0].URL
}

func TestInstallMovesAnEnrolledCollectorOnlyAfterTheNewHubAccepts(t *testing.T) {
	f, paths := enrolledCollector(t)
	var asked string
	f.deps.Confirm = func(question string) (bool, error) { asked = question; return true, nil }
	f.deps.Enroll = func(_ context.Context, opts enroll.Options) (enroll.Result, error) {
		if opts.HubURL != hubB || opts.Grant != "grant-b" {
			t.Fatalf("enroll options = %+v", opts)
		}
		credential := state.Credential{URL: opts.HubURL, Token: "credential-b"}
		if err := opts.Store.SaveCredential(credential); err != nil {
			return enroll.Result{}, err
		}
		return enroll.Result{Credential: credential, MachineID: "m-b"}, nil
	}
	if code := f.run(t, "install", "--no-start", "--role", "collector", "--hub", "https://hub-b.example", "--token", "grant-b"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if asked != "Move it?" || !strings.Contains(f.err.String(), "This collector sends to https://hub-a.example") {
		t.Fatalf("move was not confirmed: asked %q, stderr %s", asked, f.err.String())
	}
	if got := destinationOf(t, paths); got != hubB {
		t.Fatalf("destination = %s, want %s", got, hubB)
	}
	if !strings.Contains(f.out.String(), "moved from https://hub-a.example to https://hub-b.example (machine m-b)") {
		t.Fatalf("install output = %s", f.out.String())
	}
	// The grant was redeemed during install, so it is not staged again.
	if data, err := os.ReadFile(paths.EnrollmentFile()); err == nil && strings.Contains(string(data), "grant-b") {
		t.Fatalf("the redeemed grant was staged: %s", data)
	}
}

func TestInstallKeepsTheCollectorWhenTheNewHubRefusesTheGrant(t *testing.T) {
	f, paths := enrolledCollector(t)
	f.deps.Confirm = func(string) (bool, error) { return true, nil }
	f.deps.Enroll = func(context.Context, enroll.Options) (enroll.Result, error) {
		return enroll.Result{}, &enroll.RejectionError{Status: 403}
	}
	code := f.run(t, "install", "--no-start", "--role", "collector", "--hub", "https://hub-b.example", "--token", "grant-b")
	if code == 0 {
		t.Fatal("install succeeded although the hub refused the grant")
	}
	if !strings.Contains(f.err.String(), "nothing changed: this collector still sends to https://hub-a.example") {
		t.Fatalf("stderr = %s", f.err.String())
	}
	if got := destinationOf(t, paths); got != hubA {
		t.Fatalf("destination = %s, want the working %s", got, hubA)
	}
}

func TestInstallDoesNotMoveACollectorWithoutConsent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		confirm func(string) (bool, error)
		want    string
	}{
		{"declined", func(string) (bool, error) { return false, nil }, "cancelled"},
		{"no terminal", nil, "run again with --yes to move it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, paths := enrolledCollector(t)
			f.deps.Confirm = tc.confirm
			f.deps.Enroll = func(context.Context, enroll.Options) (enroll.Result, error) {
				t.Fatal("enrolled without consent")
				return enroll.Result{}, nil
			}
			if code := f.run(t, "install", "--no-start", "--role", "collector", "--hub", "https://hub-b.example", "--token", "grant-b"); code == 0 {
				t.Fatal("install moved the collector without consent")
			}
			if !strings.Contains(f.err.String(), tc.want) {
				t.Fatalf("stderr = %s, want %q", f.err.String(), tc.want)
			}
			if got := destinationOf(t, paths); got != hubA {
				t.Fatalf("destination = %s", got)
			}
		})
	}
}

func TestInstallWithTheCurrentHubDoesNotUseTheGrant(t *testing.T) {
	f, paths := enrolledCollector(t)
	f.deps.Confirm = func(string) (bool, error) { t.Fatal("asked to move to the same hub"); return false, nil }
	if code := f.run(t, "install", "--no-start", "--role", "collector", "--hub", "https://hub-a.example", "--token", "grant-a2"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), "already enrolled with https://hub-a.example; the grant was not used") {
		t.Fatalf("install output = %s", f.out.String())
	}
	if data, err := os.ReadFile(paths.EnrollmentFile()); err == nil && strings.Contains(string(data), "grant-a2") {
		t.Fatalf("the unused grant was staged: %s", data)
	}
}

func TestInstallPointsAnUnenrolledCollectorWithoutAsking(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start", "--role", "collector", "--hub", "https://hub-a.example", "--token", "grant-a")
	f.deps.Confirm = func(string) (bool, error) { t.Fatal("asked although nothing was enrolled"); return false, nil }
	f.deps.Enroll = func(context.Context, enroll.Options) (enroll.Result, error) {
		t.Fatal("redeemed during install; the service should")
		return enroll.Result{}, nil
	}
	if code := f.run(t, "install", "--no-start", "--role", "collector", "--hub", "https://hub-b.example", "--token", "grant-b"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if got := destinationOf(t, paths); got != hubB {
		t.Fatalf("destination = %s", got)
	}
	store, err := state.Open(paths.StateDir, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending, found, err := store.PendingEnrollment(hubB); err != nil || !found || pending.Grant != "grant-b" {
		t.Fatalf("staged grant = %+v, found=%v, err=%v", pending, found, err)
	}
}

func TestInstallWithThisMachinesOwnHubKeepsTheLocalAddress(t *testing.T) {
	f := newFixture(t)
	paths := f.install(t, "--no-start")
	local := destinationOf(t, paths)
	if code := f.run(t, "configure", "--origin", "https://laptop.example.ts.net:8081", "--yes"); code != 0 {
		t.Fatalf("configure exit %d: %s", code, f.err.String())
	}
	f.deps.Enroll = func(context.Context, enroll.Options) (enroll.Result, error) {
		t.Fatal("enrolled through the public address")
		return enroll.Result{}, nil
	}

	// Not enrolled yet: the grant is staged for the local address.
	f.out.Reset()
	if code := f.run(t, "install", "--no-start", "--role", "collector", "--hub", "https://laptop.example.ts.net:8081", "--token", "grant-own"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if got := destinationOf(t, paths); got != local {
		t.Fatalf("destination = %s, want the local %s", got, local)
	}
	if !strings.Contains(f.out.String(), "--hub is this machine's own hub; the collector uses its direct local address "+local) {
		t.Fatalf("install output = %s", f.out.String())
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil || !cfg.Components.Hub.Enabled {
		t.Fatalf("hub enabled = %v (%v); --role collector must never disable it", cfg.Components.Hub.Enabled, err)
	}

	// Enrolled: the grant is not used.
	saveCredential(t, paths, local)
	f.out.Reset()
	if code := f.run(t, "install", "--no-start", "--role", "collector", "--hub", "http://127.0.0.1:8081", "--token", "grant-own2"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), "already enrolled with http://127.0.0.1:8081") {
		t.Fatalf("install output = %s", f.out.String())
	}
}

func TestInstallReportsAnUpgrade(t *testing.T) {
	f := newFixture(t)
	f.install(t, "--no-start")
	f.deps.Version = "1.0.1"
	f.deps.BinaryVersion = func(string) (string, error) { return "1.0.1", nil }
	f.out.Reset()
	if code := f.run(t, "install", "--no-start"); code != 0 {
		t.Fatalf("install exit %d: %s", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), "Upgraded Rigscope from 1.0.0 to 1.0.1") {
		t.Fatalf("install output = %s", f.out.String())
	}
}
