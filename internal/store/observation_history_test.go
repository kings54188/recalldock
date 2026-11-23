package store

import (
	"database/sql"
	"errors"
	"testing"
)

func seedHistoryObservation(t *testing.T, s *Store) int64 {
	t.Helper()
	if err := s.CreateSession("history-session", "history-project", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{SessionID: "history-session", Project: "history-project", Type: "decision", Title: "Stable fact", Content: "Original durable knowledge", TopicKey: "architecture/history"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestObservationHistoryCapturesStateWithoutReadOrDedupNoise(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	versions, err := s.ObservationHistory(id, false, 20, 0)
	if err != nil || len(versions) != 1 {
		t.Fatalf("creation history: %d %v", len(versions), err)
	}
	if versions[0].Sequence != 1 || versions[0].ContentRevision != 1 {
		t.Fatal("wrong initial identity/counter")
	}
	for i := 0; i < 3; i++ {
		if _, err := s.GetObservation(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddObservation(AddObservationParams{SessionID: "history-session", Project: "history-project", Type: "decision", Title: "Stable fact", Content: "Original durable knowledge"}); err != nil {
		t.Fatal(err)
	}
	versions, _ = s.ObservationHistory(id, false, 20, 0)
	if len(versions) != 1 {
		t.Fatal("read/dedup created useless versions")
	}
	next := "Revised durable knowledge"
	if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &next}); err != nil {
		t.Fatal(err)
	}
	versions, err = s.ObservationHistory(id, false, 20, 0)
	if err != nil || len(versions) != 2 || versions[0].Sequence != 2 {
		t.Fatalf("revision history: %+v %v", versions, err)
	}
	first, err := s.ObservationVersion(id, 1, false)
	if err != nil || first.State.Content != "Original durable knowledge" {
		t.Fatal("lost original fact")
	}
	second, err := s.ObservationVersion(id, 2, false)
	if err != nil || second.State.Content != next {
		t.Fatal("lost revised fact")
	}
}

func TestObservationHistoryIncludesTopicAndStateOnlyChanges(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	same, err := s.AddObservation(AddObservationParams{SessionID: "history-session", Project: "history-project", Type: "decision", Title: "Stable fact", Content: "Trusted old API topic correction", TopicKey: "architecture/history"})
	if err != nil || same != id {
		t.Fatalf("main ID corrupted by trigger: %d %v", same, err)
	}
	if _, err := s.db.Exec("UPDATE observations SET pinned=1 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	versions, err := s.ObservationHistory(id, false, 20, 0)
	if err != nil || len(versions) != 3 {
		t.Fatalf("state history: %d %v", len(versions), err)
	}
	pinned, err := s.ObservationVersion(id, 3, false)
	if err != nil || !pinned.State.Pinned || pinned.ContentRevision != 2 {
		t.Fatal("state sequence must differ from content revision")
	}
}

func TestObservationHistoryRollsBackAndHardDeletePurges(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	next := "Must rollback atomically"
	commit := s.hooks.commit
	s.hooks.commit = func(tx *sql.Tx) error { tx.Rollback(); return errors.New("synthetic commit failure") }
	if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &next}); err == nil {
		t.Fatal("commit failure hidden")
	}
	s.hooks.commit = commit
	versions, _ := s.ObservationHistory(id, false, 20, 0)
	if len(versions) != 1 {
		t.Fatal("orphan version survived rollback")
	}
	if err := s.DeleteObservation(id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObservationHistory(id, false, 20, 0); err == nil {
		t.Fatal("soft-deleted history visible by default")
	}
	versions, err := s.ObservationHistory(id, true, 20, 0)
	if err != nil || len(versions) != 2 {
		t.Fatalf("soft delete lost history: %d %v", len(versions), err)
	}
	if err := s.DeleteObservation(id, true); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM observation_versions WHERE observation_id=?", id).Scan(&count); err != nil || count != 0 {
		t.Fatal("hard delete retained historical body")
	}
}

func TestLegacyHistoryStartsAtKnownBaselineAndPagesWithoutBodies(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	// Simulate an upgraded row whose prior versions were never captured.
	if _, err := s.db.Exec("DELETE FROM observation_versions WHERE observation_id=?", id); err != nil {
		t.Fatal(err)
	}
	changed := "Known post-upgrade state"
	if _, err := s.UpdateObservation(id, UpdateObservationParams{Content: &changed}); err != nil {
		t.Fatal(err)
	}
	versions, err := s.ObservationHistory(id, false, 1, 0)
	if err != nil || len(versions) != 1 || versions[0].Sequence != 2 {
		t.Fatal("latest history page invalid")
	}
	older, err := s.ObservationHistory(id, false, 1, versions[0].Sequence)
	if err != nil || len(older) != 1 || older[0].Kind != "legacy_baseline" {
		t.Fatal("missing honest upgrade baseline")
	}
	if _, err := s.ObservationHistory(id, false, 21, 0); err == nil {
		t.Fatal("unbounded history request accepted")
	}
	results, err := s.Search("Original durable knowledge", SearchOptions{Project: "history-project"})
	if err != nil || len(results) != 0 {
		t.Fatal("old knowledge leaked into current FTS")
	}
}
