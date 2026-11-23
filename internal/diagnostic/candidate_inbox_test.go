package diagnostic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/store"
)

func TestCandidateInboxWarnsOnSoonExpiry(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if err := s.CreateSession("inbox-session", "inbox-project", "/tmp/inbox"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Two candidates: one due in 30h (silent), one due in 23h (must warn).
	_, err := s.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{
		SessionID: "inbox-session", Project: "inbox-project",
		Policy: store.CandidateAdmissionPolicy{Now: time.Now().UTC(), TTL: 30 * time.Hour},
		Candidates: []store.MemoryCandidateInput{{
			Category: store.CandidateCategoryDecision, Statement: "far from expiry stays silent", Confidence: 0.9,
			EvidenceRefs: []store.CandidateEvidenceRef{{Kind: store.EvidenceKindTest, Ref: "t/far"}},
		}},
	})
	if err != nil {
		t.Fatalf("commit far: %v", err)
	}
	_, err = s.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{
		SessionID: "inbox-session", Project: "inbox-project",
		Policy: store.CandidateAdmissionPolicy{Now: time.Now().UTC(), TTL: 23 * time.Hour},
		Candidates: []store.MemoryCandidateInput{{
			Category: store.CandidateCategoryDecision, Statement: "due within a day must warn", Confidence: 0.9,
			EvidenceRefs: []store.CandidateEvidenceRef{{Kind: store.EvidenceKindTest, Ref: "t/near"}},
		}},
	})
	if err != nil {
		t.Fatalf("commit near: %v", err)
	}
	report, err := NewRunner().RunOne(context.Background(),
		Scope{Store: s, Project: "inbox-project", Now: time.Now()}, CheckCandidateInbox)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusWarning {
		t.Fatalf("expected warning, got %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Result != StatusWarning || report.Checks[0].ReasonCode != "candidate_review_due" {
		t.Fatalf("unexpected check results: %+v", report.Checks)
	}
	if !strings.Contains(report.Checks[0].SafeNextStep, "candidates list --all") {
		t.Fatalf("safe next step must point at the inbox command: %q", report.Checks[0].SafeNextStep)
	}
}

func TestCandidateInboxScopesCountsAndUsesInjectedClock(t *testing.T) {
	s := newDiagnosticTestStore(t)
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, project := range []string{"wanted", "foreign"} {
		if err := s.CreateSession(project, project, "/tmp/"+project); err != nil {
			t.Fatal(err)
		}
		_, err := s.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{SessionID: project, Project: project, Policy: store.CandidateAdmissionPolicy{Now: now, TTL: 30 * time.Hour}, Candidates: []store.MemoryCandidateInput{{Category: store.CandidateCategoryDecision, Statement: project + " durable fact", Confidence: 0.9, EvidenceRefs: []store.CandidateEvidenceRef{{Kind: store.EvidenceKindTest, Ref: "test-clock"}}}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s, Project: "wanted", Now: now}, CheckCandidateInbox)
	if err != nil {
		t.Fatal(err)
	}
	var evidence map[string]any
	if err := json.Unmarshal(report.Checks[0].Evidence, &evidence); err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusOK || evidence["pending_total"] != float64(1) || evidence["due_within_24h"] != float64(0) {
		t.Fatalf("wrong project or wall clock used: %+v, %#v", report, evidence)
	}
	report, err = NewRunner().RunOne(context.Background(), Scope{Store: s, Project: "wanted", Now: now.Add(31 * time.Hour)}, CheckCandidateInbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(report.Checks[0].Evidence, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence["overdue_total"] != float64(1) || evidence["due_within_24h"] != float64(0) || evidence["oldest_review_due"] == nil {
		t.Fatalf("overdue review conflated with upcoming expiry: %#v", evidence)
	}
}

func TestCandidateInboxHealthyWhenFarFromExpiry(t *testing.T) {
	s := newDiagnosticTestStore(t)
	report, err := NewRunner().RunOne(context.Background(),
		Scope{Store: s, Project: "", Now: time.Now()}, CheckCandidateInbox)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusOK || len(report.Checks) != 1 || report.Checks[0].Result != StatusOK {
		t.Fatalf("expected clean ok, got %+v", report)
	}
}
