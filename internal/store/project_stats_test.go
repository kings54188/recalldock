package store

import (
	"fmt"
	"testing"
)

func TestProjectStatsScopesCounts(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	for i, project := range []string{"proj-a", "proj-b"} {
		session := "stats-sess-" + project
		if err := s.CreateSession(session, project, dir); err != nil {
			t.Fatal(err)
		}
		count := 2
		if i == 1 {
			count = 1
		}
		for j := 0; j < count; j++ {
			if _, err := s.AddObservation(AddObservationParams{
				SessionID: session, Type: "note", Title: "scoped", Content: fmt.Sprintf("body %d", j), Project: project,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	stats, err := s.ProjectStats("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalObservations != 2 || stats.TotalSessions != 1 || len(stats.Projects) != 1 || stats.Projects[0] != "proj-a" {
		t.Fatalf("stats=%+v", stats)
	}
	global, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if global.TotalObservations != 3 {
		t.Fatalf("global=%+v", global)
	}
}

func TestSearchOffsetPagesWithoutOverlap(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	if err := s.CreateSession("offset-sess", "offset-proj", dir); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.AddObservation(AddObservationParams{
			SessionID: "offset-sess", Type: "note", Title: "paged token", Content: fmt.Sprintf("body %d", i), Project: "offset-proj",
		}); err != nil {
			t.Fatal(err)
		}
	}
	page := func(offset int) []SearchResult {
		results, err := s.Search("paged token", SearchOptions{Project: "offset-proj", Limit: 2, Offset: offset, MatchMode: "all"})
		if err != nil {
			t.Fatal(err)
		}
		return results
	}
	first, second, third := page(0), page(2), page(4)
	if len(first) != 2 || len(second) != 2 || len(third) != 1 {
		t.Fatalf("sizes %d %d %d", len(first), len(second), len(third))
	}
	seen := map[int64]bool{}
	for _, r := range append(append(first, second...), third...) {
		if seen[r.ID] {
			t.Fatalf("duplicate id %d across pages", r.ID)
		}
		seen[r.ID] = true
	}
	if len(seen) != 5 {
		t.Fatalf("seen=%d", len(seen))
	}
}
