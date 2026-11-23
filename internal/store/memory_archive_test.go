package store

import (
	"encoding/json"
	"errors"
	"testing"
)

func archivePatch(t *testing.T, archived bool) UpdateObservationParams {
	t.Helper()
	var patch UpdateObservationParams
	body, _ := json.Marshal(map[string]bool{"archived": archived})
	if err := json.Unmarshal(body, &patch); err != nil {
		t.Fatal(err)
	}
	return patch
}

func TestMemoryArchiveCASRequiresExplicitReactivation(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	before, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := s.UpdateObservationCAS(id, before.SnapshotSHA256, archivePatch(t, true), "Reviewed obsolete knowledge", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	obs, err := s.GetObservation(id)
	if err != nil {
		t.Fatal(err)
	}
	if obs.State() != "archived" {
		t.Fatal("archive did not preserve selected archived state")
	}
	hits, err := s.Search(obs.Title, SearchOptions{Project: derefString(obs.Project)})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.ID == id {
			t.Fatal("archive remained in default recall")
		}
	}
	if _, err := s.UpdateObservationCAS(id, before.SnapshotSHA256, archivePatch(t, false), "Old selection", t.TempDir()); !errors.Is(err, ErrObservationSnapshotChanged) {
		t.Fatalf("stale reactivation accepted: %v", err)
	}
	repeated, err := s.UpdateObservationCAS(id, archived.SnapshotSHA256, archivePatch(t, true), "Same archived state", t.TempDir())
	if err != nil || repeated.RevisionCount != archived.RevisionCount {
		t.Fatal("archive replay created a new revision")
	}
	content := "Newly verified durable knowledge after archival"
	updated, err := s.UpdateObservationCAS(id, archived.SnapshotSHA256, UpdateObservationParams{Content: &content}, "Reviewed new evidence", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	obs, _ = s.GetObservation(id)
	if obs.State() != "archived" || obs.Content != content {
		t.Fatal("content edit changed explicit archive state")
	}
	if updated.RevisionCount != archived.RevisionCount+1 {
		t.Fatal("reactivation history not atomic")
	}
	versions, err := s.ObservationHistory(id, false, 20, 0)
	if err != nil || len(versions) != 3 {
		t.Fatalf("archive/revision history: %d %v", len(versions), err)
	}
}

func TestMemoryArchiveProtectsPinnedAndOperationalKnowledge(t *testing.T) {
	for _, kind := range []string{"pinned", "operational"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			id := seedHistoryObservation(t, s)
			if kind == "pinned" {
				if err := s.PinObservation(id); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.db.Exec("UPDATE observations SET memory_class='operational' WHERE id=?", id); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.SelectObservationForEdit(id)
			if _, err := s.UpdateObservationCAS(id, before.SnapshotSHA256, archivePatch(t, true), "Archive protected entry", t.TempDir()); err == nil {
				t.Fatal("protected memory was archived")
			}
			after, _ := s.SelectObservationForEdit(id)
			if before.SnapshotSHA256 != after.SnapshotSHA256 {
				t.Fatal("failed archive changed memory")
			}
		})
	}
}

func TestMemoryArchiveInterchangePreservesStateAndSelectedAccess(t *testing.T) {
	s := newTestStore(t)
	id := seedHistoryObservation(t, s)
	before, _ := s.SelectObservationForEdit(id)
	if _, err := s.UpdateObservationCAS(id, before.SnapshotSHA256, archivePatch(t, true), "Reviewed archival", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	index, err := s.ArchivedMemories("history-project", 0, 5)
	if err != nil || len(index) != 1 || index[0].ID != id {
		t.Fatalf("archive index: %+v %v", index, err)
	}
	exchange, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	fresh := newTestStore(t)
	if _, err := fresh.Import(exchange); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Import(exchange); err != nil {
		t.Fatal(err)
	}
	got, err := fresh.GetObservationBySyncID(index[0].SyncID)
	if err != nil || got.State() != ObservationStateArchived {
		t.Fatalf("archive lost on interchange: %+v %v", got, err)
	}
	selected, _ := fresh.SelectObservationForEdit(got.ID)
	if _, err := fresh.UpdateObservationCAS(got.ID, selected.SnapshotSHA256, archivePatch(t, false), "Restore selected archived knowledge", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got, _ = fresh.GetObservation(got.ID)
	if got.State() != ObservationStateActive {
		t.Fatal("explicit reactivation failed")
	}
}
