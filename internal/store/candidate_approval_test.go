package store

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCandidateApprovalDigestAndAtomicRollback(t *testing.T) {
	s := newCandidateTestStore(t)
	result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{verifiedCandidate(CandidateCategoryDecision, "Use a durable reviewed decision")}})
	if err != nil {
		t.Fatal(err)
	}
	id := result.CandidateIDs[0]
	candidate, _ := s.GetMemoryCandidate(id)
	digest, _ := MemoryCandidateDigest(candidate)
	if _, err := s.ApproveMemoryCandidate(id, "wrong-digest", "Reviewed source"); err == nil {
		t.Fatal("changed snapshot accepted")
	}
	original := s.hooks.commit
	s.hooks.commit = func(tx *sql.Tx) error { tx.Rollback(); return errors.New("synthetic commit failure") }
	if _, err := s.ApproveMemoryCandidate(id, digest, "Reviewed source"); err == nil {
		t.Fatal("commit failure hidden")
	}
	s.hooks.commit = original
	stats, _ := s.Stats()
	candidate, _ = s.GetMemoryCandidate(id)
	if stats.TotalObservations != 0 || candidate.Status != CandidateStatusReview {
		t.Fatal("failed approval left an observation or changed candidate")
	}
	obs, err := s.ApproveMemoryCandidate(id, digest, "Reviewed source")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ApproveMemoryCandidate(id, digest, "Reviewed source")
	if err != nil || replay.ID != obs.ID {
		t.Fatalf("non-idempotent approval: %+v %v", replay, err)
	}
	if _, err := s.ExpireMemoryCandidates(time.Now().Add(30 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	candidate, _ = s.GetMemoryCandidate(id)
	if candidate.Status != CandidateStatusPromoted {
		t.Fatal("expiry erased approved provenance")
	}
}

func TestConcurrentApprovalsCreateOneObservation(t *testing.T) {
	s := newCandidateTestStore(t)
	r, _ := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{verifiedCandidate(CandidateCategoryDecision, "Keep promotion atomic across concurrent reviewers")}})
	candidate, _ := s.GetMemoryCandidate(r.CandidateIDs[0])
	digest, _ := MemoryCandidateDigest(candidate)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ApproveMemoryCandidate(candidate.SyncID, digest, "Operator reviewed source")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	stats, _ := s.Stats()
	if stats.TotalObservations != 1 {
		t.Fatalf("observations=%d", stats.TotalObservations)
	}
}

func TestApprovalDoesNotSilentlyUpdateTopicAndExactDuplicatesLinkExisting(t *testing.T) {
	s := newCandidateTestStore(t)
	existingID, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Existing", Content: "Keep existing knowledge", Scope: "project", TopicKey: "decision/policy"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetObservation(existingID)
	input := verifiedCandidate(CandidateCategoryDecision, "A different proposed policy must not overwrite silently")
	input.ProposedTopicKey = "decision/policy"
	r, _ := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{input}})
	candidate, _ := s.GetMemoryCandidate(r.CandidateIDs[0])
	digest, _ := MemoryCandidateDigest(candidate)
	if _, err := s.ApproveMemoryCandidate(candidate.SyncID, digest, "Reviewed proposal"); err == nil {
		t.Fatal("topic overwrite allowed without revision precondition")
	}
	after, _ := s.GetObservation(existingID)
	if after.Content != before.Content || after.RevisionCount != before.RevisionCount {
		t.Fatal("old knowledge changed")
	}
	input = verifiedCandidate(CandidateCategoryDecision, "Keep existing knowledge")
	r, _ = s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{input}})
	candidate, _ = s.GetMemoryCandidate(r.CandidateIDs[0])
	digest, _ = MemoryCandidateDigest(candidate)
	approved, err := s.ApproveMemoryCandidate(candidate.SyncID, digest, "Confirmed this fact is already known")
	if err != nil || approved.ID != existingID {
		t.Fatalf("exact duplicate: %+v %v", approved, err)
	}
	after, _ = s.GetObservation(existingID)
	if after.RevisionCount != before.RevisionCount || after.DuplicateCount != before.DuplicateCount {
		t.Fatal("linking an exact duplicate rewrote existing knowledge")
	}
}

func TestRejectedCandidatesBlockedAndOverdueReviewRetained(t *testing.T) {
	s := newCandidateTestStore(t)
	r, _ := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{verifiedCandidate(CandidateCategoryDecision, "Review a reusable operator decision")}})
	c, _ := s.GetMemoryCandidate(r.CandidateIDs[0])
	digest, _ := MemoryCandidateDigest(c)
	if err := s.RejectMemoryCandidateChecked(c.SyncID, "wrong", "Not reusable"); err == nil {
		t.Fatal("stale rejection accepted")
	}
	if err := s.RejectMemoryCandidateChecked(c.SyncID, digest, "Not reusable"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveMemoryCandidate(c.SyncID, digest, "Reviewed"); err == nil {
		t.Fatal("rejected candidate approved")
	}
	now := time.Now().Add(-48 * time.Hour)
	r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{verifiedCandidate(CandidateCategoryDecision, "A separate expired proposal")}, Policy: CandidateAdmissionPolicy{Now: now, TTL: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ = s.GetMemoryCandidate(r.CandidateIDs[0])
	digest, _ = MemoryCandidateDigest(c)
	if _, err := s.ApproveMemoryCandidate(c.SyncID, digest, "Reviewed"); err != nil {
		t.Fatalf("overdue review was discarded instead of retained: %v", err)
	}
	stats, _ := s.Stats()
	if stats.TotalObservations != 1 {
		t.Fatalf("overdue review did not reach memory: %+v", stats)
	}
}
