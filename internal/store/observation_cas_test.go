package store

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
)

func TestObservationCASRejectsStaleSnapshotAndKeepsAtomicHistory(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	selected, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	content := "Operator-reviewed revised knowledge"
	updated, err := s.UpdateObservationCAS(id, selected.SnapshotSHA256, UpdateObservationParams{Content: &content}, "Reviewed source change", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if updated.RevisionCount != 2 || updated.State.Content != content {
		t.Fatal("update did not return committed state")
	}
	other := "Must not replace newer state"
	if _, err := s.UpdateObservationCAS(id, selected.SnapshotSHA256, UpdateObservationParams{Content: &other}, "Review older selection", "/tmp"); !errors.Is(err, ErrObservationSnapshotChanged) {
		t.Fatalf("stale overwrite accepted: %v", err)
	}
	versions, _ := s.ObservationHistory(id, false, 20, 0)
	if len(versions) != 2 {
		t.Fatal("stale update created history")
	}
	var audits int
	if err := s.db.QueryRow("SELECT count(*) FROM observation_edit_audits WHERE observation_id=?", id).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("audit not atomic")
	}
}

func TestObservationCASNormalizedNoOpKeepsHistoryAuditAndDependencies(t *testing.T) {
	path := "project/example"
	paddedPath := " project/example "
	paddedTitle := " Stable fact "
	paddedContent := " Original durable knowledge "
	for _, tt := range []struct {
		name    string
		initial UpdateObservationParams
		patch   UpdateObservationParams
	}{
		{"memory path whitespace", UpdateObservationParams{MemoryPath: &path}, UpdateObservationParams{MemoryPath: &paddedPath}},
		{"title whitespace", UpdateObservationParams{}, UpdateObservationParams{Title: &paddedTitle}},
		{"content whitespace", UpdateObservationParams{}, UpdateObservationParams{Content: &paddedContent}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			id := seedHistoryObservation(t, s)
			if tt.initial != (UpdateObservationParams{}) {
				if _, err := s.UpdateObservation(id, tt.initial); err != nil {
					t.Fatal(err)
				}
			}
			dependentID, err := s.AddObservation(AddObservationParams{SessionID: "history-session", Project: "history-project", Type: "decision", Title: "Derived fact", Content: "Knowledge derived from the source"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: id, DependentObservationID: dependentID, Reason: "derived test fact"}); err != nil {
				t.Fatal(err)
			}
			selected, err := s.SelectObservationForEdit(id)
			if err != nil {
				t.Fatal(err)
			}
			dependentBefore, err := s.SelectObservationForEdit(dependentID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.ObservationHistory(id, false, 20, 0)
			if err != nil {
				t.Fatal(err)
			}
			var syncBefore int
			if err := s.db.QueryRow("SELECT count(*) FROM sync_mutations").Scan(&syncBefore); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				updated, err := s.UpdateObservationCAS(id, selected.SnapshotSHA256, tt.patch, "Review equivalent normalized state", "/tmp")
				if err != nil {
					t.Errorf("no-op attempt %d: %v", attempt+1, err)
					continue
				}
				if updated.RevisionCount != selected.RevisionCount || updated.SnapshotSHA256 != selected.SnapshotSHA256 {
					t.Errorf("no-op attempt %d changed selection: %+v", attempt+1, updated)
				}
			}
			after, err := s.ObservationHistory(id, false, 20, 0)
			if err != nil || len(after) != len(before) {
				t.Fatalf("no-op history: before=%d after=%d err=%v", len(before), len(after), err)
			}
			var audits, syncAfter int
			if err := s.db.QueryRow("SELECT count(*) FROM observation_edit_audits WHERE observation_id=?", id).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if audits != 0 {
				t.Errorf("no-op created %d audits", audits)
			}
			if err := s.db.QueryRow("SELECT count(*) FROM sync_mutations").Scan(&syncAfter); err != nil {
				t.Fatal(err)
			}
			if syncAfter != syncBefore {
				t.Errorf("no-op sync mutations: before=%d after=%d", syncBefore, syncAfter)
			}
			dependentAfter, err := s.SelectObservationForEdit(dependentID)
			if err != nil || dependentAfter.SnapshotSHA256 != dependentBefore.SnapshotSHA256 {
				t.Fatalf("no-op invalidated dependent: %+v err=%v", dependentAfter, err)
			}
			dependencies, err := s.ListMemoryDependencies(MemoryDependencyListOptions{SourceObservationSyncID: selected.SyncID})
			if err != nil || len(dependencies) != 1 || dependencies[0].Status != DependencyStatusActive || dependencies[0].SourceRevision != selected.RevisionCount {
				t.Fatalf("no-op changed dependency: %+v err=%v", dependencies, err)
			}
			content := "Operator-reviewed changed source knowledge"
			updated, err := s.UpdateObservationCAS(id, selected.SnapshotSHA256, UpdateObservationParams{Content: &content}, "Reviewed actual source change", "/tmp")
			if err != nil || updated.RevisionCount != selected.RevisionCount+1 {
				t.Fatalf("real change: %+v err=%v", updated, err)
			}
			history, err := s.ObservationHistory(id, false, 20, 0)
			if err != nil || len(history) != len(before)+1 {
				t.Fatalf("real change history: count=%d err=%v", len(history), err)
			}
			var expectedSnapshot, auditReason string
			if err := s.db.QueryRow("SELECT count(*),expected_snapshot_sha256,operator_reason FROM observation_edit_audits WHERE observation_id=?", id).Scan(&audits, &expectedSnapshot, &auditReason); err != nil {
				t.Fatal(err)
			}
			if audits != 1 || expectedSnapshot != selected.SnapshotSHA256 || auditReason != "Reviewed actual source change" {
				t.Errorf("real change audit: count=%d snapshot=%q reason=%q", audits, expectedSnapshot, auditReason)
			}
			dependent, err := s.GetObservation(dependentID)
			if err != nil || dependent.StaleAt == nil {
				t.Fatalf("real change did not invalidate dependent: %+v err=%v", dependent, err)
			}
		})
	}
}

func TestObservationCASDigestIgnoresReadsButProtectsPinAndProject(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	first, _ := s.SelectObservationForEdit(id)
	if _, err := s.db.Exec("UPDATE observations SET duplicate_count=duplicate_count+1,last_seen_at=datetime('now') WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	after, _ := s.SelectObservationForEdit(id)
	if first.SnapshotSHA256 != after.SnapshotSHA256 {
		t.Fatal("read/hit noise invalidated knowledge snapshot")
	}
	same := first.State.Content
	if _, err := s.UpdateObservationCAS(id, first.SnapshotSHA256, UpdateObservationParams{Content: &same}, "No fact change", "/tmp"); err != nil {
		t.Fatal(err)
	}
	versions, _ := s.ObservationHistory(id, false, 20, 0)
	if len(versions) != 1 {
		t.Fatal("no-op created fake revision")
	}
	if err := s.PinObservation(id); err != nil {
		t.Fatal(err)
	}
	changed := "Pinned state must be re-reviewed"
	if _, err := s.UpdateObservationCAS(id, first.SnapshotSHA256, UpdateObservationParams{Content: &changed}, "Old snapshot", "/tmp"); !errors.Is(err, ErrObservationSnapshotChanged) {
		t.Fatal("pin/protection state ignored")
	}
	current, _ := s.SelectObservationForEdit(id)
	foreign := "another-project"
	if _, err := s.UpdateObservationCAS(id, current.SnapshotSHA256, UpdateObservationParams{Project: &foreign}, "Move via edit", "/tmp"); err == nil {
		t.Fatal("CAS silently migrated project")
	}
}

func TestConcurrentObservationCASAcrossStoresHasOneWinner(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	other, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	selected, _ := s.SelectObservationForEdit(id)
	results := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, st := range []*Store{s, other} {
		wg.Add(1)
		go func(i int, st *Store) {
			defer wg.Done()
			<-start
			content := []string{"Concurrent fact A", "Concurrent fact B"}[i]
			_, err := st.UpdateObservationCAS(id, selected.SnapshotSHA256, UpdateObservationParams{Content: &content}, "Reviewed concurrently", "/tmp")
			results <- err
		}(i, st)
	}
	close(start)
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrObservationSnapshotChanged) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("winners=%d stale=%d", success, stale)
	}
	versions, _ := s.ObservationHistory(id, false, 20, 0)
	if len(versions) != 2 {
		t.Fatal("lost or duplicate concurrent history")
	}
}

func TestObservationCASCommitFailureLeavesNoVersionOrAudit(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	selected, _ := s.SelectObservationForEdit(id)
	content := "Rollback this correction"
	commit := s.hooks.commit
	s.hooks.commit = func(tx *sql.Tx) error { tx.Rollback(); return errors.New("synthetic commit failure") }
	if _, err := s.UpdateObservationCAS(id, selected.SnapshotSHA256, UpdateObservationParams{Content: &content}, "Reviewed change", "/tmp"); err == nil {
		t.Fatal("commit failure hidden")
	}
	s.hooks.commit = commit
	current, _ := s.SelectObservationForEdit(id)
	if current.SnapshotSHA256 != selected.SnapshotSHA256 {
		t.Fatal("failed edit changed fact")
	}
	versions, _ := s.ObservationHistory(id, false, 20, 0)
	if len(versions) != 1 {
		t.Fatal("failed edit retained historical body")
	}
	var audits int
	s.db.QueryRow("SELECT count(*) FROM observation_edit_audits WHERE observation_id=?", id).Scan(&audits)
	if audits != 0 {
		t.Fatal("failed edit retained audit")
	}
}

func TestOperatorCreateIsIdempotentAndCannotUpsertTopic(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	params := AddObservationParams{SessionID: "history-session", Project: "history-project", Type: "decision", Title: "Stable fact", Content: "Original durable knowledge", TopicKey: "architecture/history"}
	duplicate, err := s.AddObservationNew(params)
	if err != nil || duplicate != id {
		t.Fatalf("identical create not reused: %d %v", duplicate, err)
	}
	params.Content = "Unreviewed topic overwrite"
	if _, err := s.AddObservationNew(params); err == nil {
		t.Fatal("create silently updated existing topic")
	}
	selected, _ := s.SelectObservationForEdit(id)
	if selected.RevisionCount != 1 || selected.State.Content != "Original durable knowledge" {
		t.Fatal("create mutated current fact")
	}
	versions, _ := s.ObservationHistory(id, false, 20, 0)
	if len(versions) != 1 {
		t.Fatal("duplicate create generated history noise")
	}
}

func TestObservationCASCannotMarkOldPartialSummaryFresh(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	abstract, overview := "Original abstract", "Original detailed overview"
	if _, err := s.UpdateObservation(id, UpdateObservationParams{Abstract: &abstract, Overview: &overview}); err != nil {
		t.Fatal(err)
	}
	selected, _ := s.SelectObservationForEdit(id)
	content, newAbstract := "New source fact", "New abstract"
	if _, err := s.UpdateObservationCAS(id, selected.SnapshotSHA256, UpdateObservationParams{Content: &content, Abstract: &newAbstract}, "Reviewed source", "/tmp"); err == nil {
		t.Fatal("old overview would become incorrectly fresh")
	}
	if _, err := s.UpdateObservationCAS(id, selected.SnapshotSHA256, UpdateObservationParams{Content: &content}, "Leave summaries stale", "/tmp"); err != nil {
		t.Fatal(err)
	}
	current, _ := s.SelectObservationForEdit(id)
	if current.State.SummaryState != SummaryStateStale {
		t.Fatal("derived summaries not stale")
	}
}
