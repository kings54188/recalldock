package store

import (
	"strings"
	"testing"
)

func TestSearchDetailedHierarchicalFindsChildrenAndHonorsRoot(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("retrieval", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	_, err := s.AddObservation(AddObservationParams{
		SessionID: "retrieval", Type: "procedure", Title: "Release gate",
		Content: "The release verification gate is documented here.", Project: "engram",
		MemoryClass: MemoryClassProject, MemoryPath: "project/runbooks/release",
		Abstract: "Release verification gate", Overview: "Parent procedure for validated releases.",
	})
	if err != nil {
		t.Fatal(err)
	}
	childID, err := s.AddObservation(AddObservationParams{
		SessionID: "retrieval", Type: "procedure", Title: "Windows command",
		Content: "Run the signed binary and retain the exit status.", Project: "engram",
		MemoryClass: MemoryClassProject, MemoryPath: "project/runbooks/release/windows",
		Abstract: "Windows verification command",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "retrieval", Type: "procedure", Title: "Unrelated",
		Content: "Release verification gate words outside the requested root.", Project: "engram",
		MemoryClass: MemoryClassProject, MemoryPath: "project/unrelated",
	}); err != nil {
		t.Fatal(err)
	}

	flat, err := s.SearchDetailed("release verification gate", SearchOptions{
		Project: "engram", Limit: 10, Strategy: SearchStrategyFlat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if searchResponseContains(flat, childID) {
		t.Fatalf("flat search unexpectedly found lexical-gap child: %+v", flat.Results)
	}

	hierarchical, err := s.SearchDetailed("release verification gate", SearchOptions{
		Project: "engram", MatchMode: "any", Limit: 10, Strategy: SearchStrategyHierarchical,
		RootPath: "project/runbooks", MaxDepth: 4, Detail: SearchDetailL1, Trace: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !searchResponseContains(hierarchical, childID) {
		t.Fatalf("hierarchical search missed child: %+v", hierarchical.Results)
	}
	for _, result := range hierarchical.Results {
		if result.ID == childID {
			if (result.ScoreSource != "node_match" && result.ScoreSource != "hierarchy_keyword_coverage") || result.Score < 0 || result.Score > 1 {
				t.Fatalf("hierarchy score metadata: %+v", result)
			}
		}
	}
	for _, result := range hierarchical.Results {
		if !strings.HasPrefix(result.MemoryPath, "project/runbooks") {
			t.Fatalf("root_path leaked result %q", result.MemoryPath)
		}
	}
	if hierarchical.StrategyUsed != SearchStrategyHierarchical || hierarchical.Budget.Estimator == "" {
		t.Fatalf("response metadata=%+v", hierarchical)
	}
	assertTraceContainsNoText(t, hierarchical.Trace, "signed binary")
}

func TestSearchDetailedBudgetAndAutoFallback(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("retrieval-budget", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{
		SessionID: "retrieval-budget", Type: "decision", Title: "Budget marker",
		Content: strings.Repeat("FULL-CONTENT-MARKER ", 100), Project: "engram",
		MemoryClass: MemoryClassProject, Abstract: "Budget marker abstract",
		Overview: strings.Repeat("overview ", 200),
	})
	if err != nil {
		t.Fatal(err)
	}

	response, err := s.SearchDetailed("budget marker", SearchOptions{
		Project: "engram", Limit: 10, Strategy: SearchStrategyAuto,
		Detail: SearchDetailL1, TokenBudget: 850, Trace: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StrategyUsed != SearchStrategyFlat || response.FallbackReason != "no_matching_hierarchy" {
		t.Fatalf("auto fallback=%+v", response)
	}
	if response.Budget.Used > response.Budget.Limit || response.Budget.Estimator != SearchTokenEstimator {
		t.Fatalf("budget=%+v", response.Budget)
	}
	if !searchResponseContains(response, id) {
		t.Fatalf("budgeted response dropped compact fallback: %+v", response.Results)
	}
	if response.Results[0].SelectedDetail != SearchDetailL0 {
		t.Fatalf("selected detail=%q, want l0 downgrade", response.Results[0].SelectedDetail)
	}
	if strings.Contains(response.Results[0].SelectedContent, "FULL-CONTENT-MARKER") {
		t.Fatal("budgeted search exposed full content")
	}
	assertTraceContainsNoText(t, response.Trace, "FULL-CONTENT-MARKER")
}

func TestSearchDetailedRejectsInvalidControls(t *testing.T) {
	s := newTestStore(t)
	tests := []SearchOptions{
		{Strategy: "vector"},
		{Detail: "l2"},
		{RootPath: "project/../secret", Strategy: SearchStrategyHierarchical},
		{MaxDepth: -1, Strategy: SearchStrategyHierarchical},
		{TokenBudget: -1},
	}
	for _, opts := range tests {
		if _, err := s.SearchDetailed("query", opts); err == nil {
			t.Errorf("SearchDetailed(%+v) succeeded", opts)
		}
	}
}

func searchResponseContains(response *SearchResponse, id int64) bool {
	for _, result := range response.Results {
		if result.ID == id {
			return true
		}
	}
	return false
}

func assertTraceContainsNoText(t *testing.T, trace []SearchTraceEntry, marker string) {
	t.Helper()
	for _, entry := range trace {
		if strings.Contains(entry.Reason, marker) || strings.Contains(entry.Path, marker) {
			t.Fatalf("trace leaked content marker: %+v", entry)
		}
	}
}
