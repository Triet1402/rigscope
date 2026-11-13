package live

import "testing"

func TestReplacedActiveSessionEndsOnceUnlessASuccessorIsLive(t *testing.T) {
	start := func(s *State, collector, session, run string) {
		t.Helper()
		if _, err := s.StartSession(StartSessionInput{
			CollectorID: collector, SessionID: session, RunID: run, BootID: "boot", Hostname: "host",
			ExpectedIntervalMS: 1000,
		}); err != nil {
			t.Fatalf("StartSession(%s): %v", session, err)
		}
	}

	// Replaced while connected, no successor yet: the old socket's end reports
	// the machine once.
	s := NewState("hub", nil)
	if err := s.RegisterMachine("c-old", "m"); err != nil {
		t.Fatal(err)
	}
	start(s, "c-old", "s-old", "run-old")
	s.ReplaceMachineWithCredential("c-new", "m", "hash-new")
	if machine, ended := s.EndSession("s-old"); !ended || machine != "m" {
		t.Fatalf("EndSession after replacement = %q, %v; want m, true", machine, ended)
	}
	if _, ended := s.EndSession("s-old"); ended {
		t.Fatal("a dropped session ended twice")
	}

	// A successor connected first: the old socket's end reports nothing.
	s = NewState("hub", nil)
	if err := s.RegisterMachine("c-old", "m"); err != nil {
		t.Fatal(err)
	}
	start(s, "c-old", "s-old", "run-old")
	s.ReplaceMachineWithCredential("c-new", "m", "hash-new")
	start(s, "c-new", "s-new", "run-new")
	if _, ended := s.EndSession("s-old"); ended {
		t.Fatal("the old session's end reported a disconnect while the successor is live")
	}

	// A removal already reported the machine: the old socket's end is silent.
	s = NewState("hub", nil)
	if err := s.RegisterMachine("c-old", "m"); err != nil {
		t.Fatal(err)
	}
	start(s, "c-old", "s-old", "run-old")
	s.ReplaceMachineWithCredential("c-new", "m", "hash-new")
	if _, removed := s.RemoveCollector("c-new"); !removed {
		t.Fatal("RemoveCollector did not remove the replaced machine")
	}
	if _, ended := s.EndSession("s-old"); ended {
		t.Fatal("the old session's end reported a disconnect a removal had already reported")
	}
}
