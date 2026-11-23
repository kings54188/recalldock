package store

import (
	"strings"
	"testing"
)

func TestAutomaticAdmissionRequiresOperatorPolicy(t *testing.T) {
	s := newCandidateTestStore(t)
	submit := func(body string) *CommitMemoryCandidatesResult {
		r, e := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{verifiedCandidate(CandidateCategoryDecision, body)}})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	if submit("Retain explicit transaction boundaries").Promoted != 0 {
		t.Fatal("default auto approved")
	}
	if e := s.SetCandidateAdmissionMode("duplicates"); e != nil {
		t.Fatal(e)
	}
	if submit("Keep unrelated new facts pending").Promoted != 0 {
		t.Fatal("unverified new fact approved")
	}
	if e := s.SetCandidateAdmissionMode("checked"); e != nil {
		t.Fatal(e)
	}
	if submit("Use distinct identities for separate projects").Promoted != 0 {
		t.Fatal("model-asserted test reference was treated as independently verified")
	}
	if submit("maybe this speculative statement is true").Promoted != 0 {
		t.Fatal("unsafe content admitted")
	}
	// checked 自动准入必须有证据门槛:无 evidence_refs 的候选继续等待操作员。
	bare := MemoryCandidateInput{Category: CandidateCategoryDecision, Statement: "Checked mode must not admit evidence-free statements", Confidence: 0.95}
	bareResult, e := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{bare}})
	if e != nil {
		t.Fatal(e)
	}
	if bareResult.Promoted != 0 || bareResult.Review != 1 {
		t.Fatalf("evidence-free candidate auto-admitted in checked mode: %+v", bareResult)
	}
	if e := s.SetCandidateAdmissionMode("manual"); e != nil {
		t.Fatal(e)
	}
	if submit("Retain a snapshot for review decisions").Promoted != 0 {
		t.Fatal("manual mode ignored")
	}
}

func TestTieredAdmissionAutoAdmitsOnlyVerifiedDurableHighConfidence(t *testing.T) {
	s := newCandidateTestStore(t)
	id, err := s.AddObservation(AddObservationParams{SessionID: "candidate-session", Project: "candidate-project", Type: "decision", Title: "Source fixture", Content: "Reviewed source for tiered admission"})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := s.GetObservation(id)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	ref := CandidateEvidenceRef{Kind: EvidenceKindObservation, Ref: obs.SyncID, Digest: snapshot.SnapshotSHA256}
	if err := s.SetCandidateAdmissionMode("tiered"); err != nil {
		t.Fatal(err)
	}
	if got := s.CandidateAdmissionMode(); got != "tiered" {
		t.Fatalf("mode = %q", got)
	}

	commit := func(statement string, confidence float64, category string, refs []CandidateEvidenceRef, topic string) *CommitMemoryCandidatesResult {
		t.Helper()
		r, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
			SessionID: "candidate-session", Project: "candidate-project",
			Candidates: []MemoryCandidateInput{{
				Category: category, Statement: statement, Confidence: confidence,
				EvidenceRefs: refs, ProposedTopicKey: topic,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Verified + durable + high confidence: admitted without review.
	if r := commit("Verified durable tiered decision", 0.9, CandidateCategoryDecision, []CandidateEvidenceRef{ref}, ""); r.Promoted != 1 || r.Review != 0 {
		t.Fatalf("tiered admission did not promote: %+v", r)
	}
	// Low confidence: stays in review.
	if r := commit("Low confidence tiered proposal", 0.5, CandidateCategoryDecision, []CandidateEvidenceRef{ref}, ""); r.Promoted != 0 || r.Review != 1 {
		t.Fatalf("low confidence admitted: %+v", r)
	}
	// Ephemeral category: stays in review.
	if r := commit("Ephemeral session note", 0.9, CandidateCategoryUserPreference, []CandidateEvidenceRef{ref}, ""); r.Promoted != 0 || r.Review != 1 {
		t.Fatalf("ephemeral category admitted: %+v", r)
	}
	// Topic-key updates are merges, not silent admissions.
	if r := commit("Topic update proposal", 0.9, CandidateCategoryDecision, []CandidateEvidenceRef{ref}, "topic-key"); r.Promoted != 0 || r.Review != 1 {
		t.Fatalf("topic update admitted automatically: %+v", r)
	}
	// Unverifiable evidence: stays in review even at high confidence.
	if r := commit("Unverified evidence proposal", 0.9, CandidateCategoryDecision, []CandidateEvidenceRef{{Kind: EvidenceKindTest, Ref: "no-receipt", Digest: strings.Repeat("a", 64)}}, ""); r.Promoted != 0 || r.Review != 1 {
		t.Fatalf("unverified evidence admitted: %+v", r)
	}
}
