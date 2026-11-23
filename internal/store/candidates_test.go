package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func newCandidateTestStore(t *testing.T) *Store {
	t.Helper()
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.DataDir = t.TempDir()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateSession("candidate-session", "candidate-project", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s
}

func verifiedCandidate(category, statement string) MemoryCandidateInput {
	return MemoryCandidateInput{
		Category:   category,
		Statement:  statement,
		Confidence: 0.95,
		EvidenceRefs: []CandidateEvidenceRef{{
			Kind:   EvidenceKindTest,
			Ref:    "go-test/internal-store/candidate-contract",
			Digest: strings.Repeat("a", 64),
		}},
	}
}

func TestCommitMemoryCandidatesStagesAllEightCategories(t *testing.T) {
	s := newCandidateTestStore(t)
	categories := []string{
		CandidateCategoryUserPreference,
		CandidateCategoryUserFact,
		CandidateCategoryProjectConstraint,
		CandidateCategoryDecision,
		CandidateCategoryProcedure,
		CandidateCategoryToolBehavior,
		CandidateCategoryFailureAvoidance,
		CandidateCategoryVerifiedOutcome,
	}
	candidates := make([]MemoryCandidateInput, 0, len(categories))
	for _, category := range categories {
		candidates = append(candidates, verifiedCandidate(category, "Verified "+category+" candidate"))
	}

	result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID:  "candidate-session",
		Project:    "candidate-project",
		Candidates: candidates,
	})
	if err != nil {
		t.Fatalf("CommitMemoryCandidates: %v", err)
	}
	if result.Promoted != 0 || result.Review != 8 || result.Rejected != 0 {
		t.Fatalf("unexpected counts: %+v", result)
	}
	if len(result.CandidateIDs) != 8 {
		t.Fatalf("candidate ids = %d, want 8", len(result.CandidateIDs))
	}
	for _, id := range result.CandidateIDs {
		candidate, err := s.GetMemoryCandidate(id)
		if err != nil {
			t.Fatalf("GetMemoryCandidate(%s): %v", id, err)
		}
		if candidate.Status != CandidateStatusReview || candidate.PromotedObservationSyncID != "" {
			t.Fatalf("candidate not promoted with provenance: %+v", candidate)
		}
	}
}

func TestEachCandidateCategoryRejectsInvalidInputWithoutBodyPersistence(t *testing.T) {
	s := newCandidateTestStore(t)
	categories := []string{
		CandidateCategoryUserPreference, CandidateCategoryUserFact,
		CandidateCategoryProjectConstraint, CandidateCategoryDecision,
		CandidateCategoryProcedure, CandidateCategoryToolBehavior,
		CandidateCategoryFailureAvoidance, CandidateCategoryVerifiedOutcome,
	}
	for _, category := range categories {
		result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
			SessionID: "candidate-session", Project: "candidate-project",
			Candidates: []MemoryCandidateInput{{Category: category, Statement: "invalid " + category, Confidence: 2}},
		})
		if err != nil {
			t.Fatalf("%s commit: %v", category, err)
		}
		if result.Rejected != 1 {
			t.Fatalf("%s result=%+v", category, result)
		}
		candidate, err := s.GetMemoryCandidate(result.CandidateIDs[0])
		if err != nil {
			t.Fatalf("%s GetMemoryCandidate: %v", category, err)
		}
		if candidate.Statement != "" || len(candidate.EvidenceRefs) != 0 || candidate.RejectionReason != "invalid_confidence" {
			t.Fatalf("%s rejected row not scrubbed: %+v", category, candidate)
		}
	}
}

func TestCandidateCorrectionRequiresReviewAndManualDecisions(t *testing.T) {
	s := newCandidateTestStore(t)
	fast, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session", Project: "candidate-project",
		Candidates: []MemoryCandidateInput{{
			Category: CandidateCategoryUserPreference, Statement: "Use Chinese for default communication", Confidence: 0.85,
			EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindUserCorrection, Ref: "prompt-event-42"}},
		}},
	})
	if err != nil || fast.Promoted != 0 || fast.Review != 1 {
		t.Fatalf("correction fast path: result=%+v err=%v", fast, err)
	}

	review, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session", Project: "candidate-project",
		Candidates: []MemoryCandidateInput{
			{Category: CandidateCategoryDecision, Statement: "manual promote", Confidence: 0.5},
			{Category: CandidateCategoryProcedure, Statement: "manual reject", Confidence: 0.5},
		},
	})
	if err != nil || review.Review != 2 {
		t.Fatalf("review admission: result=%+v err=%v", review, err)
	}
	if _, err := s.PromoteMemoryCandidate(review.CandidateIDs[0], "human verified"); err != nil {
		t.Fatalf("PromoteMemoryCandidate: %v", err)
	}
	if err := s.RejectMemoryCandidate(review.CandidateIDs[1], "not reusable"); err != nil {
		t.Fatalf("RejectMemoryCandidate: %v", err)
	}
	promoted, _ := s.GetMemoryCandidate(review.CandidateIDs[0])
	rejected, _ := s.GetMemoryCandidate(review.CandidateIDs[1])
	if promoted.Status != CandidateStatusPromoted || rejected.Status != CandidateStatusRejected || rejected.Statement != "" {
		t.Fatalf("manual decisions not persisted safely: promoted=%+v rejected=%+v", promoted, rejected)
	}
}

func TestCommitMemoryCandidatesRoutesWeakEvidenceToReview(t *testing.T) {
	s := newCandidateTestStore(t)
	result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session",
		Project:   "candidate-project",
		Candidates: []MemoryCandidateInput{
			{Category: CandidateCategoryDecision, Statement: "Low confidence", Confidence: 0.40, EvidenceRefs: []CandidateEvidenceRef{{Kind: EvidenceKindFile, Ref: "internal/store/store.go"}}},
			{Category: CandidateCategoryProcedure, Statement: "No evidence", Confidence: 0.99},
		},
	})
	if err != nil {
		t.Fatalf("CommitMemoryCandidates: %v", err)
	}
	if result.Review != 2 || result.Promoted != 0 || result.Rejected != 0 {
		t.Fatalf("unexpected counts: %+v", result)
	}

	observations, err := s.RecentObservations("candidate-project", "project", 20)
	if err != nil {
		t.Fatalf("RecentObservations: %v", err)
	}
	if len(observations) != 0 {
		t.Fatalf("review candidates must not become observations: %+v", observations)
	}
}

func TestCommitMemoryCandidatesRejectsSecretsAndTranscriptWithoutPersistingBodies(t *testing.T) {
	s := newCandidateTestStore(t)
	secret := "api_key=sk-test-abcdefghijklmnopqrstuvwxyz123456"
	transcript := "user: fix this\nassistant: I changed the file\ntool: go test ./..."
	result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session",
		Project:   "candidate-project",
		Candidates: []MemoryCandidateInput{
			verifiedCandidate(CandidateCategoryToolBehavior, secret),
			verifiedCandidate(CandidateCategoryVerifiedOutcome, transcript),
		},
	})
	if err != nil {
		t.Fatalf("CommitMemoryCandidates: %v", err)
	}
	if result.Rejected != 2 || result.Promoted != 0 || result.Review != 0 {
		t.Fatalf("unexpected counts: %+v", result)
	}

	var stored string
	if err := s.DB().QueryRow(`
		SELECT ifnull(group_concat(statement || ' ' || evidence_refs, ' '), '')
		FROM memory_candidates
	`).Scan(&stored); err != nil {
		t.Fatalf("read candidate storage: %v", err)
	}
	if strings.Contains(stored, secret) || strings.Contains(stored, transcript) || strings.Contains(stored, "abcdefghijklmnopqrstuvwxyz123456") {
		t.Fatalf("secret or transcript persisted in candidate staging: %q", stored)
	}
}

func TestOversizeStatementRejectsAsTooLongNotTranscript(t *testing.T) {
	s := newCandidateTestStore(t)
	// ~2400 bytes: durable knowledge that merely exceeds maxCandidateStatementBytes.
	long := strings.Repeat("durable-fact ", 200)
	transcript := "user: fix this\nassistant: I changed the file\ntool: go test ./..."
	result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session",
		Project:   "candidate-project",
		Candidates: []MemoryCandidateInput{
			verifiedCandidate(CandidateCategoryProcedure, long),
			verifiedCandidate(CandidateCategoryToolBehavior, transcript),
		},
	})
	if err != nil {
		t.Fatalf("CommitMemoryCandidates: %v", err)
	}
	if result.Rejected != 2 || len(result.Results) != 2 {
		t.Fatalf("unexpected counts: %+v", result)
	}
	if result.Results[0].Reason != "statement_too_long" {
		t.Fatalf("oversize statement should reject as statement_too_long, got %q", result.Results[0].Reason)
	}
	if result.Results[1].Reason != "raw_transcript" {
		t.Fatalf("transcript should still reject as raw_transcript, got %q", result.Results[1].Reason)
	}
}

func TestCommitMemoryCandidatesDedupeAndBatchLimit(t *testing.T) {
	s := newCandidateTestStore(t)
	candidate := verifiedCandidate(CandidateCategoryDecision, "Use deterministic candidate admission")
	first, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{candidate}})
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	second, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: []MemoryCandidateInput{candidate}})
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if second.Duplicate != 1 || second.Rejected != 0 || len(second.CandidateIDs) != 1 || second.CandidateIDs[0] != first.CandidateIDs[0] {
		t.Fatalf("duplicate should report status duplicate (not rejected) and return existing id without a second row: first=%+v second=%+v", first, second)
	}
	if len(second.Results) != 1 || second.Results[0].Status != CandidateStatusDuplicate {
		t.Fatalf("duplicate entry should carry explicit duplicate status: %+v", second.Results)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT count(*) FROM memory_candidates`).Scan(&count); err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if count != 1 {
		t.Fatalf("candidate rows = %d, want 1", count)
	}

	tooMany := make([]MemoryCandidateInput, MaxCandidatesPerCommit+1)
	for i := range tooMany {
		tooMany[i] = verifiedCandidate(CandidateCategoryProcedure, "candidate batch item "+string(rune('a'+i)))
	}
	if _, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "candidate-session", Project: "candidate-project", Candidates: tooMany}); err == nil {
		t.Fatal("expected batch larger than eight to fail atomically")
	}
}

func TestMemoryCandidateTTLAndCapacityAreBounded(t *testing.T) {
	s := newCandidateTestStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	policy := CandidateAdmissionPolicy{Now: now, TTL: time.Hour, CapacityPerProject: 1}
	first, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session", Project: "candidate-project", Policy: policy,
		Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "review backlog one", Confidence: 0.2}},
	})
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	second, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session", Project: "candidate-project", Policy: policy,
		Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "review backlog two", Confidence: 0.2}},
	})
	if err != nil {
		t.Fatalf("capacity commit: %v", err)
	}
	if second.Rejected != 1 {
		t.Fatalf("capacity overflow must reject: %+v", second)
	}

	expired, err := s.ExpireMemoryCandidates(now.Add(2 * time.Hour))
	if err != nil {
		t.Fatalf("ExpireMemoryCandidates: %v", err)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want 1", expired)
	}
	row, err := s.GetMemoryCandidate(first.CandidateIDs[0])
	if err != nil {
		t.Fatalf("GetMemoryCandidate: %v", err)
	}
	if row.Status != CandidateStatusExpired || row.Statement == "" {
		t.Fatalf("expired candidate must retain its body for revival: %+v", row)
	}
}

func TestMemoryCandidatesStayOutOfSyncAndExport(t *testing.T) {
	s := newCandidateTestStore(t)
	result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "candidate-session", Project: "candidate-project",
		Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "review only", Confidence: 0.2}},
	})
	if err != nil || result.Review != 1 {
		t.Fatalf("commit review candidate: result=%+v err=%v", result, err)
	}
	exported, err := s.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	encoded, err := json.Marshal(exported)
	if err != nil {
		t.Fatalf("Marshal export: %v", err)
	}
	if strings.Contains(string(encoded), "review only") || strings.Contains(string(encoded), "memory_candidates") {
		t.Fatalf("candidate staging leaked into export: %s", encoded)
	}
	var mutationCount int
	if err := s.DB().QueryRow(`SELECT count(*) FROM sync_mutations WHERE entity = 'candidate'`).Scan(&mutationCount); err != nil {
		t.Fatalf("count candidate mutations: %v", err)
	}
	if mutationCount != 0 {
		t.Fatalf("candidate staging must not sync, got %d mutations", mutationCount)
	}
}

func TestCandidateStatementDeletesCarriageReturns(t *testing.T) {
	prepared := prepareMemoryCandidate(MemoryCandidateInput{Category: CandidateCategoryProcedure, Statement: "RecallDock\r 发版\r(0\r.1\r.7\r 起稳定\r)", Confidence: 0.9}, normalizeCandidatePolicy(CandidateAdmissionPolicy{}))
	if prepared.status != CandidateStatusReview {
		t.Fatalf("candidate rejected: %+v", prepared)
	}
	if strings.Contains(prepared.statement, "\r") || !strings.Contains(prepared.statement, "(0.1.7 起稳定)") {
		t.Fatalf("carriage return was not deleted cleanly: %q", prepared.statement)
	}
}

func TestSuggestTopicKeyKeepsChineseText(t *testing.T) {
	got := SuggestTopicKey("manual", "测试主题键建议", "")
	if !strings.Contains(got, "测试主题键建议") || got == "topic/general" {
		t.Fatalf("Chinese topic key collapsed: %q", got)
	}
}
