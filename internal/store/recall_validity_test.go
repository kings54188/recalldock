package store

import (
	"reflect"
	"testing"
)

func TestRecallSignalNeverRevalidatesKnowledge(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("recall-validity", "recall", "/tmp/recall"); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{SessionID: "recall-validity", Project: "recall", Type: "decision", Title: "Recall signal", Content: "A historical decision awaiting evidence review"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE observations SET stale_at='2000-01-01', summary_state='stale', archived_at='2000-01-02', review_after='2000-01-03' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetObservation(id)
	if err != nil {
		t.Fatal(err)
	}
	s.RecordSearchRecalls([]SearchResult{{Observation: Observation{ID: id}}})
	after, err := s.GetObservation(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.RecallCount != before.RecallCount+1 || after.LastRecallAt == nil {
		t.Fatal("recall signal missing")
	}
	after.RecallCount = before.RecallCount
	after.LastRecallAt = before.LastRecallAt
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("read changed fact validity: before=%+v after=%+v", before, after)
	}
}
