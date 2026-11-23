package store

import (
	"errors"
	"testing"
)

func TestUnknownIDsReturnDomainErrors(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetObservation(999999); !errors.Is(err, ErrObservationNotFound) {
		t.Fatalf("GetObservation err=%v", err)
	}
	if _, err := s.ObservationHistory(999999, false, 5, 0); !errors.Is(err, ErrObservationNotFound) {
		t.Fatalf("ObservationHistory err=%v", err)
	}
	if _, err := s.ObservationVersion(999999, 1, false); !errors.Is(err, ErrObservationNotFound) {
		t.Fatalf("ObservationVersion err=%v", err)
	}
	if _, err := s.GetMemoryCandidate("cand-missing"); !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("GetMemoryCandidate err=%v", err)
	}
	if err := s.RejectMemoryCandidateChecked("cand-missing", "00", "reason"); !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("RejectMemoryCandidateChecked err=%v", err)
	}

	// A known observation with an unknown sequence is a version miss, not a
	// missing observation.
	if err := s.CreateSession("s1", "not-found-probe", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{SessionID: "s1", Type: "note", Title: "t", Content: "c", Project: "not-found-probe"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObservationVersion(id, 99, false); !errors.Is(err, ErrObservationVersionNotFound) {
		t.Fatalf("ObservationVersion sequence err=%v", err)
	}
}
