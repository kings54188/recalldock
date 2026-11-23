package store

import (
	"testing"
	"time"
)

func TestExpiredCandidateRetainsBodyAndCanBeRevived(t *testing.T) {
	s := newCandidateTestStore(t)
	now := time.Now().UTC().Add(-48 * time.Hour)
	r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session", Project: "candidate-project",
		Candidates: []MemoryCandidateInput{verifiedCandidate(CandidateCategoryDecision, "A late but durable decision")},
		Policy:     CandidateAdmissionPolicy{Now: now, TTL: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpireMemoryCandidates(now.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.GetMemoryCandidate(r.CandidateIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Status != CandidateStatusExpired || candidate.Statement == "" {
		t.Fatalf("expiry discarded the proposal: %+v", candidate)
	}
	digest, err := MemoryCandidateDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	revived, err := s.ReviveMemoryCandidateChecked(candidate.SyncID, digest, "Still valid after a long pause")
	if err != nil {
		t.Fatal(err)
	}
	if revived.Status != CandidateStatusReview {
		t.Fatalf("revived status = %q", revived.Status)
	}
	if _, err := s.ApproveMemoryCandidate(revived.SyncID, digest, "Reviewed after revival"); err == nil {
		// The digest changed because expiry is part of the snapshot; a fresh
		// digest must be required rather than silently accepted.
		t.Fatal("stale pre-revival digest approved")
	}
	fresh, _ := MemoryCandidateDigest(revived)
	if _, err := s.ApproveMemoryCandidate(revived.SyncID, fresh, "Reviewed after revival"); err != nil {
		t.Fatal(err)
	}
}

func TestTieredSweepKeepsHighConfidenceHistoricalClaimInReview(t *testing.T) {
	s := newCandidateTestStore(t)
	if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
		t.Fatal(err)
	}
	r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session", Project: "candidate-project",
		Candidates: []MemoryCandidateInput{{
			Category: CandidateCategoryDecision, Statement: "A high-confidence historical decision whose reference is no longer resolvable",
			Confidence: 0.96, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindSource, Ref: "historical note without a current file"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.GetMemoryCandidate(r.CandidateIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Status != CandidateStatusReview || r.Promoted != 0 || r.Results[0].Reason != "evidence_not_verified" {
		t.Fatalf("high-confidence claim bypassed evidence verification: %+v %+v", candidate, r)
	}
}

func TestCandidateCommitDoesNotSweepAnotherProject(t *testing.T) {
	for _, mode := range []string{"checked", "tiered"} {
		t.Run(mode, func(t *testing.T) {
			s := newCandidateTestStore(t)
			if err := s.CreateSession("foreign-session", "foreign-project", t.TempDir()); err != nil {
				t.Fatal(err)
			}
			id, err := s.AddObservation(AddObservationParams{SessionID: "foreign-session", Project: "foreign-project", Type: "decision", Title: "Foreign source", Content: "Reviewed foreign source"})
			if err != nil {
				t.Fatal(err)
			}
			source, err := s.GetObservation(id)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := s.SelectObservationForEdit(id)
			if err != nil {
				t.Fatal(err)
			}
			foreign, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "foreign-session", Project: "foreign-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Keep explicitly selected project admission isolated", Confidence: 0.9, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindObservation, Ref: source.SyncID, Digest: snapshot.SnapshotSHA256}}}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SetCandidateAdmissionMode(mode); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{verifiedCandidate(CandidateCategoryDecision, "Use the work ledger for unrelated task progress")}}); err != nil {
				t.Fatal(err)
			}
			candidate, err := s.GetMemoryCandidate(foreign.CandidateIDs[0])
			if err != nil || candidate.Status != CandidateStatusReview {
				t.Fatalf("project commit swept foreign knowledge: %+v %v", candidate, err)
			}
			swept, err := s.AdmitPendingMemoryCandidates()
			if err != nil || swept.Promoted != 1 {
				t.Fatalf("explicit operator sweep lost its configured scope: %+v %v", swept, err)
			}
		})
	}
}

func TestTieredSweepKeepsUncertainClaimInReview(t *testing.T) {
	s := newCandidateTestStore(t)
	if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
		t.Fatal(err)
	}
	r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session", Project: "candidate-project",
		Candidates: []MemoryCandidateInput{{
			Category: CandidateCategoryDecision, Statement: "A plausible but not yet proven decision",
			Confidence: 0.8, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindSource, Ref: "unsupported claim"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.GetMemoryCandidate(r.CandidateIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Status != CandidateStatusReview || r.Promoted != 0 {
		t.Fatalf("uncertain claim was not left for review: %+v %+v", candidate, r)
	}
}
