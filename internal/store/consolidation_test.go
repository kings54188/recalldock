package store

import (
	"strings"
	"testing"
)

func TestConsolidateArchivesSourcesAndKeepsLineage(t *testing.T) {
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateSession("consolidate-session", "consolidate-proj", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	add := func(title, content string) int64 {
		id, err := s.AddObservation(AddObservationParams{SessionID: "consolidate-session", Type: "procedure", Title: title, Content: content, Project: "consolidate-proj"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	target := add("Refined pitfall summary", "One distilled rule with the key exception.")
	first := add("messy source one", "source one detail")
	second := add("messy source two", "source two detail")
	pinned := add("pinned source", "must stay active")
	if err := s.PinObservation(pinned); err != nil {
		t.Fatal(err)
	}

	absorbed, err := s.ConsolidateObservations(target, []int64{first, second, pinned}, "merge duplicate pitfalls", "")
	if err == nil {
		t.Fatal("pinned source should be refused")
	}
	if len(absorbed) != 2 {
		t.Fatalf("absorbed = %v", absorbed)
	}
	for _, id := range []int64{first, second} {
		obs, err := s.GetObservation(id)
		if err != nil || obs.ArchivedAt == nil {
			t.Fatalf("source %d not archived: %+v %v", id, obs, err)
		}
	}
	pinnedObs, _ := s.GetObservation(pinned)
	if pinnedObs.ArchivedAt != nil {
		t.Fatal("pinned source was archived")
	}
	sources, err := s.ConsolidatedSources(target)
	if err != nil || len(sources) != 2 {
		t.Fatalf("sources = %d err=%v", len(sources), err)
	}
	// Sources stay readable by ID.
	if _, err := s.GetObservation(first); err != nil {
		t.Fatalf("source no longer readable: %v", err)
	}
	// Already-absorbed sources are skipped on retry, not double-archived.
	if again, err := s.ConsolidateObservations(target, []int64{first, second}, "retry", ""); err != nil || len(again) != 0 {
		t.Fatalf("retry absorbed=%v err=%v", again, err)
	}
	// Default context must show the refined target, never the archived sources.
	ctx, err := s.HostContext("consolidate-proj", 400)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ctx.Text, "Refined pitfall summary") {
		t.Fatalf("target missing from context: %s", ctx.Text)
	}
	for _, banned := range []string{"messy source one", "messy source two"} {
		if strings.Contains(ctx.Text, banned) {
			t.Fatalf("archived source leaked into context: %s", ctx.Text)
		}
	}
}
