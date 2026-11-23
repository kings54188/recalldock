package store

import "testing"

// TestCandidateListReportsSessionOrigin ties session provenance to the review
// queue: an operator must be able to tell a host-hook session from a model one
// before approving a candidate. The value is provenance, not authentication.
func TestCandidateListReportsSessionOrigin(t *testing.T) {
	s := newTestStore(t)
	dir := t.TempDir()
	proj, err := s.CreateRegisteredProject("candidate origin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(proj.ID, dir); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSessionWithOrigin("host-sess", proj.ID, dir, SessionOriginHost); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "host-sess",
		Project:   proj.ID,
		Candidates: []MemoryCandidateInput{{
			Category:   "decision",
			Statement:  "Keep session provenance visible to the operator",
			Confidence: 0.8,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListMemoryCandidates(proj.ID, "", 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	if list[0].SessionOrigin != SessionOriginHost {
		t.Fatalf("origin=%q want host", list[0].SessionOrigin)
	}
	got, err := s.GetMemoryCandidate(list[0].SyncID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionOrigin != SessionOriginHost {
		t.Fatalf("selected origin=%q", got.SessionOrigin)
	}
}
