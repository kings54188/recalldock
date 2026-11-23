package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestFlatSearchFiltersStaleAcrossQueryPaths(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("stale-scope", "scope", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"marker", "alpha-beta", "project/topic", "中文"} {
		t.Run(query, func(t *testing.T) {
			id, err := s.AddObservation(AddObservationParams{SessionID: "stale-scope", Project: "scope", Type: "decision", Title: query, Content: "中文 marker alpha beta", TopicKey: query})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE observations SET stale_at='2026-01-01 00:00:00' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
			results, err := s.Search(query, SearchOptions{Project: "scope"})
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 0 {
				t.Fatalf("stale result delivered: %d", len(results))
			}
			results, err = s.Search(query, SearchOptions{Project: "scope", IncludeStale: true})
			if err != nil || len(results) == 0 {
				t.Fatalf("explicit stale selection failed: %v", err)
			}
		})
	}
}

func TestEditsPreserveLifecycleAndNoopHasNoRevision(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("editing", "scope", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{SessionID: "editing", Project: "scope", Type: "decision", Title: "archived", Content: "original"})
	if err != nil {
		t.Fatal(err)
	}
	flag := true
	if _, err := s.UpdateObservation(id, UpdateObservationParams{Archived: &flag}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetObservation(id)
	if err != nil {
		t.Fatal(err)
	}
	title := "retitled"
	after, err := s.UpdateObservation(id, UpdateObservationParams{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	if after.ArchivedAt == nil || derefString(after.ArchivedAt) != derefString(before.ArchivedAt) || derefString(after.ReviewAfter) != derefString(before.ReviewAfter) {
		t.Fatal("edit changed lifecycle or review deadline")
	}
	unchanged, err := s.UpdateObservation(id, UpdateObservationParams{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.RevisionCount != after.RevisionCount {
		t.Fatal("identical edit created revision")
	}
}

func TestStoredTruncationKeepsUTF8AndArchivedDuplicateIsExplicit(t *testing.T) {
	cfg := mustDefaultConfig(t)
	cfg.DataDir = t.TempDir()
	cfg.MaxObservationLength = 101
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateSession("utf8", "scope", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{SessionID: "utf8", Project: "scope", Type: "decision", Title: "unicode", Content: strings.Repeat("中", 50), Overview: strings.Repeat("文", 50)})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := s.GetObservation(id)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(obs.Content) || !utf8.ValidString(obs.Overview) {
		t.Fatal("truncation broke UTF-8")
	}
	content := strings.Repeat("字", 50)
	obs, err = s.UpdateObservation(id, UpdateObservationParams{Content: &content})
	if err != nil || !utf8.ValidString(obs.Content) {
		t.Fatalf("edit truncation broke UTF-8: %v", err)
	}
	if _, err := s.SelectObservationForEdit(id); err != nil {
		t.Fatal(err)
	}
	p := AddObservationParams{SessionID: "utf8", Project: "scope", Type: "decision", Title: "duplicate", Content: "duplicate body"}
	duplicate, err := s.AddObservationNew(p)
	if err != nil {
		t.Fatal(err)
	}
	flag := true
	if _, err := s.UpdateObservation(duplicate, UpdateObservationParams{Archived: &flag}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservationNew(p); err == nil {
		t.Fatal("archived duplicate reported successful creation")
	}
}

func TestSameDayRelationBounds(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("relations", "scope", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.AddObservation(AddObservationParams{SessionID: "relations", Project: "scope", Type: "decision", Title: fmt.Sprintf("shared title %d", i), Content: "shared content"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ScanProject(ScanOptions{Project: "scope", Apply: true})
	if err != nil || first.RelationsInserted == 0 {
		t.Fatalf("scan seed: %+v %v", first, err)
	}
	now := time.Now().UTC().Add(-time.Hour)
	rows, err := s.ListRelations(ListRelationsOptions{Project: "scope", SinceTime: now, Limit: 10})
	if err != nil || len(rows) == 0 {
		t.Fatalf("same-day relation missing: %v", err)
	}
	result, err := s.ScanProject(ScanOptions{Project: "scope", Since: now})
	if err != nil || result.Inspected != 2 {
		t.Fatalf("same-day scan missing: %+v %v", result, err)
	}
}
