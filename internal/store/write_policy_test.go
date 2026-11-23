package store

import (
	"strings"
	"testing"
)

func TestModelSuppliedEvidenceNeverAutoPromotes(t *testing.T) {
	for _, kind := range []string{EvidenceKindTest, EvidenceKindUserCorrection, EvidenceKindObservation} {
		t.Run(kind, func(t *testing.T) {
			s := newCandidateTestStore(t)
			result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Choose bounded local memory with explicit review", Confidence: 1, EvidenceRefs: []CandidateEvidenceRef{{Kind: kind, Ref: "model-asserted-source", Digest: strings.Repeat("a", 64)}}}}})
			if err != nil || result.Promoted != 0 || result.Review != 1 {
				t.Fatalf("self-asserted evidence must stay in review: %+v %v", result, err)
			}
			stats, _ := s.Stats()
			if stats.TotalObservations != 0 {
				t.Fatal("proposal wrote long-term memory")
			}
		})
	}
}

func TestCandidateNoiseAndMetadataAreNotPersisted(t *testing.T) {
	for _, input := range []MemoryCandidateInput{
		{Category: CandidateCategoryDecision, Statement: "Tests passed, task completed", Confidence: 1},
		{Category: CandidateCategoryDecision, Statement: "可能这个配置会更快", Confidence: 1},
		{Category: CandidateCategoryProcedure, Statement: "$ go test ./...\nINFO running\nERROR failed", Confidence: 1},
		{Category: "password=abcdefghijklmnopqrstuvwxyz1234", Statement: "Sensitive category must not leak", Confidence: 1},
		{Category: CandidateCategoryDecision, Statement: "Bounded metadata", Confidence: 1, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindFile, Ref: "fixture", Digest: strings.Repeat("d", 5000)}}},
		{Category: CandidateCategoryDecision, Statement: "Bounded paths", Confidence: 1, ProposedTopicKey: "api_key=abcdefghijklmnopqrstuvwxyz1234"},
	} {
		s := newCandidateTestStore(t)
		result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{input}})
		if err != nil || result.Rejected != 1 {
			t.Fatalf("unsafe/noisy candidate accepted: %+v %v", result, err)
		}
		row, err := s.GetMemoryCandidate(result.CandidateIDs[0])
		if err != nil {
			t.Fatal(err)
		}
		if row.Statement != "" || len(row.EvidenceRefs) != 0 || strings.Contains(row.Category, "password=") || row.ProposedTopicKey != "" {
			t.Fatalf("rejection retained input body: %+v", row)
		}
	}
}

func TestCandidateInboxGlobalCapAcrossProjects(t *testing.T) {
	s := newCandidateTestStore(t)
	input := verifiedCandidate(CandidateCategoryDecision, "Keep an explicit global inbox bound")
	policy := CandidateAdmissionPolicy{CapacityTotalPending: 1}
	first, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{input}, Policy: policy})
	if err != nil || first.Review != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	if err := s.CreateSession("other", "another-project", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	second, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "other", Project: "another-project", Candidates: []MemoryCandidateInput{input}, Policy: policy})
	if err != nil || second.Rejected != 1 || second.Review != 0 {
		t.Fatalf("project name bypassed global cap: %+v %v", second, err)
	}
}

func TestCandidateChineseTranscriptsAndShortPrefixedSecretsAreRedacted(t *testing.T) {
	for _, statement := range []string{"DEEPSEEK_API_KEY=small", "password=short", "密码：不记录", "用户：你好\n助手：原始对话"} {
		st := newCandidateTestStore(t)
		input := verifiedCandidate(CandidateCategoryDecision, statement)
		result, err := st.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{input}})
		if err != nil || result.Rejected != 1 {
			t.Fatalf("unsafe candidate not rejected: %v", err)
		}
		row, err := st.GetMemoryCandidate(result.CandidateIDs[0])
		if err != nil || row.Statement != "" || len(row.EvidenceRefs) != 0 {
			t.Fatal("rejection retained unsafe input")
		}
	}
}
