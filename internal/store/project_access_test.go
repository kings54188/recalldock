package store

import "testing"

func TestTimelineDoesNotCrossProjectInsideLegacyMixedSession(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("mixed", "visible", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	anchor, err := s.AddObservation(AddObservationParams{SessionID: "mixed", Project: "visible", Type: "decision", Title: "Visible anchor", Content: "Visible durable fact"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.AddObservation(AddObservationParams{SessionID: "mixed", Project: "hidden", Type: "decision", Title: "Foreign neighbor", Content: "FOREIGN_NEIGHBOR_BODY"})
	if err != nil {
		t.Fatal(err)
	}
	timeline, err := s.Timeline(anchor, 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline.Before) != 0 || len(timeline.After) != 0 || timeline.TotalInRange != 1 {
		t.Fatalf("legacy mixed session crossed project: %+v", timeline)
	}
	if _, err := s.Timeline(foreign, 5, 5); err == nil {
		t.Fatal("inconsistent anchor/session projects were relabeled")
	}
}
