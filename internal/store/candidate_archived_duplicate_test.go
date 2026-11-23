package store

import "testing"

func TestAutomaticAdmissionDoesNotPromoteUnavailableDuplicate(t *testing.T) {
	for _, state := range []string{"archived", "stale", "review_overdue", "invalid_review_time"} {
		t.Run(state, func(t *testing.T) {
			for _, mode := range []string{"duplicates", "checked", "tiered"} {
				t.Run(mode, func(t *testing.T) {
					s := newCandidateTestStore(t)
					const statement = "Preserve archived matching candidate for explicit review"
					archivedID, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Archived duplicate", Content: statement})
					if err != nil {
						t.Fatal(err)
					}
					switch state {
					case "archived":
						flag := true
						if _, err := s.UpdateObservation(archivedID, UpdateObservationParams{Archived: &flag}); err != nil {
							t.Fatal(err)
						}
					case "stale":
						if _, err := s.db.Exec(`UPDATE observations SET stale_at=datetime('now') WHERE id=?`, archivedID); err != nil {
							t.Fatal(err)
						}
					case "review_overdue":
						if _, err := s.db.Exec(`UPDATE observations SET review_after='2000-01-01 00:00:00' WHERE id=?`, archivedID); err != nil {
							t.Fatal(err)
						}
					case "invalid_review_time":
						if _, err := s.db.Exec(`UPDATE observations SET review_after='invalid' WHERE id=?`, archivedID); err != nil {
							t.Fatal(err)
						}
					}
					sourceID, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Current evidence", Content: "Current evidence is active"})
					if err != nil {
						t.Fatal(err)
					}
					source, err := s.GetObservation(sourceID)
					if err != nil {
						t.Fatal(err)
					}
					snapshot, err := s.SelectObservationForEdit(sourceID)
					if err != nil {
						t.Fatal(err)
					}
					if err := s.SetCandidateAdmissionMode(mode); err != nil {
						t.Fatal(err)
					}
					r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: statement, Confidence: 0.95, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindObservation, Ref: source.SyncID, Digest: snapshot.SnapshotSHA256}}}}})
					if err != nil {
						t.Fatal(err)
					}
					c, err := s.GetMemoryCandidate(r.CandidateIDs[0])
					if err != nil {
						t.Fatal(err)
					}
					if state == "review_overdue" || state == "invalid_review_time" {
						if r.Promoted != 1 || c.Status != CandidateStatusPromoted {
							t.Fatalf("review clock changed availability: result=%+v candidate=%+v", r, c)
						}
						return
					}
					if r.Promoted != 0 || c.Status != CandidateStatusReview {
						t.Fatalf("unavailable duplicate incorrectly promoted: result=%+v candidate=%+v", r, c)
					}
					if state == "archived" {
						archived, err := s.GetObservation(archivedID)
						if err != nil || archived.ArchivedAt == nil {
							t.Fatalf("archived knowledge changed: %+v %v", archived, err)
						}
					}
				})
			}
		})
	}
}

func TestAdmissionPrefersActiveExactDuplicate(t *testing.T) {
	s := newCandidateTestStore(t)
	const statement = "Existing active duplicate should be selected for review"
	archivedID, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Old duplicate", Content: statement})
	if err != nil {
		t.Fatal(err)
	}
	flag := true
	if _, err := s.UpdateObservation(archivedID, UpdateObservationParams{Archived: &flag}); err != nil {
		t.Fatal(err)
	}
	activeID, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Active duplicate", Content: statement})
	if err != nil {
		t.Fatal(err)
	}
	if activeID == archivedID {
		t.Fatal("fixture needs separate observations")
	}
	active, err := s.GetObservation(activeID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCandidateAdmissionMode("duplicates"); err != nil {
		t.Fatal(err)
	}
	r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{verifiedCandidate(CandidateCategoryDecision, statement)}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.GetMemoryCandidate(r.CandidateIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if r.Promoted != 1 || c.PromotedObservationSyncID != active.SyncID {
		t.Fatalf("active duplicate not selected: %+v %+v", r, c)
	}
	archived, err := s.GetObservation(archivedID)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("archive modified: %+v %v", archived, err)
	}
}
