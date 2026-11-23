package store

import "testing"

func TestGlobalStatsReportsFailures(t *testing.T) {
	s := newTestStore(t)
	s.Close()
	if _, err := s.Stats(); err == nil {
		t.Fatal("global stats silently succeeded on closed database")
	}
}

func TestStatsForProjectCountsExactlyAndReportsFailures(t *testing.T) {
	s := newTestStore(t)
	for _, p := range []string{"scope-a", "scope-b"} {
		if err := s.CreateSession(p, p, "/tmp"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddObservation(AddObservationParams{SessionID: p, Project: p, Title: p, Content: p, Type: "decision"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddPrompt(AddPromptParams{SessionID: p, Project: p, Content: "synthetic prompt"}); err != nil {
			t.Fatal(err)
		}
	}
	scoped, err := s.StatsForProject("scope-a")
	if err != nil || scoped.TotalObservations != 1 || scoped.TotalSessions != 1 || scoped.TotalPrompts != 1 || len(scoped.Projects) != 1 {
		t.Fatalf("scoped %+v %v", scoped, err)
	}
	empty, err := s.StatsForProject("missing")
	if err != nil || empty.TotalObservations != 0 || len(empty.Projects) != 0 {
		t.Fatalf("missing %+v %v", empty, err)
	}
	if _, err := s.StatsForProject(""); err == nil {
		t.Fatal("empty scope became global")
	}
	s.Close()
	if _, err := s.StatsForProject("scope-a"); err == nil {
		t.Fatal("closed store silently returned counts")
	}
}
