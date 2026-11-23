package store

import (
	"strings"
	"testing"
	"time"
)

func TestSearchTimeBoundsFilterByCreatedAt(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	if err := s.CreateSession("time-sess", "time-proj", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "time-sess", Type: "note", Title: "temporal probe",
		Content: "the quick brown fox jumps", Project: "time-proj",
	}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	future := time.Now().UTC().Add(2 * time.Hour).Format("2006-01-02 15:04:05")

	search := func(opts SearchOptions) (int, error) {
		opts.Project = "time-proj"
		opts.MatchMode = "all"
		res, err := s.Search("brown fox", opts)
		return len(res), err
	}
	if n, err := search(SearchOptions{Since: past}); err != nil || n != 1 {
		t.Fatalf("since past: n=%d err=%v", n, err)
	}
	if n, err := search(SearchOptions{Until: past}); err != nil || n != 0 {
		t.Fatalf("until past: n=%d err=%v", n, err)
	}
	if n, err := search(SearchOptions{Since: past, Until: future}); err != nil || n != 1 {
		t.Fatalf("window: n=%d err=%v", n, err)
	}
	// RFC3339 is accepted and normalized.
	if n, err := search(SearchOptions{Since: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)}); err != nil || n != 1 {
		t.Fatalf("rfc3339: n=%d err=%v", n, err)
	}
	if _, err := search(SearchOptions{Since: "not-a-date"}); err == nil || !strings.Contains(err.Error(), "invalid time bound") {
		t.Fatalf("invalid bound err=%v", err)
	}
	if _, err := search(SearchOptions{Since: future, Until: past}); err == nil || !strings.Contains(err.Error(), "invalid time range") {
		t.Fatalf("reversed range err=%v", err)
	}
}

func TestSearchTimeBoundsApplyToDetailedStrategy(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	if err := s.CreateSession("detail-sess", "detail-proj", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "detail-sess", Type: "decision", Title: "hierarchy probe",
		Content: "release rollback checklist", Project: "detail-proj",
		MemoryClass: MemoryClassProject, MemoryPath: "project/decisions/rollback",
	}); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(2 * time.Hour).Format("2006-01-02 15:04:05")
	resp, err := s.SearchDetailed("rollback", SearchOptions{
		Project: "detail-proj", Strategy: SearchStrategyHierarchical, Until: future, Detail: SearchDetailL0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results=%d", len(resp.Results))
	}
	resp, err = s.SearchDetailed("rollback", SearchOptions{
		Project: "detail-proj", Strategy: SearchStrategyHierarchical, Since: future, Detail: SearchDetailL0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("future since returned %d results", len(resp.Results))
	}
}
