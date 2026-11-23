package diagnostic

import (
	"context"
	"strings"
	"time"

	"recalldock.local/recalldock/internal/store"
)

const CheckCandidateInbox = "candidate_inbox"

// CandidateInboxCheck surfaces overdue candidate reviews. TTL is a review SLA,
// not a deletion deadline: the proposal body and evidence stay readable and
// can still be revived or auto-admitted after a long pause.
type CandidateInboxCheck struct{}

func (CandidateInboxCheck) Code() string { return CheckCandidateInbox }

func (c CandidateInboxCheck) Run(ctx context.Context, scope Scope) (CheckResult, error) {
	_ = ctx
	now := scope.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	stats, err := scope.Store.CandidateReviewStats(scope.Project, now)
	if err != nil {
		return CheckResult{}, err
	}
	candidates, err := scope.Store.ListMemoryCandidates(scope.Project, store.CandidateStatusReview, 200)
	if err != nil {
		return CheckResult{}, err
	}
	due := make([]map[string]any, 0, 8)
	for _, cand := range candidates {
		reviewDue, err := time.Parse("2006-01-02 15:04:05", cand.ExpiresAt)
		if err != nil || !reviewDue.Before(now.Add(24*time.Hour)) {
			continue
		}
		if len(due) < 8 {
			preview := []rune(strings.TrimSpace(cand.Statement))
			if len(preview) > 64 {
				preview = preview[:64]
			}
			due = append(due, map[string]any{"id": cand.SyncID, "project": cand.Project, "review_due_at": cand.ExpiresAt, "preview": string(preview)})
		}
	}
	evidence := map[string]any{
		"pending_total": stats.PendingTotal, "due_within_24h": stats.DueWithin24h, "overdue_total": stats.OverdueTotal,
		"oldest_review_due": stats.OldestReviewDue, "due": due, "due_sample_limit": 200, "admission_mode": scope.Store.CandidateAdmissionMode(),
	}
	findings := make([]Finding, 0, 1)
	if stats.DueWithin24h+stats.OverdueTotal > 0 {
		findings = append(findings, Finding{
			CheckID: c.Code(), Severity: SeverityWarning, ReasonCode: "candidate_review_due",
			Message:              "Candidate reviews are due; their content is retained without automatic archival.",
			Why:                  "The due date is a review SLA, not a deletion deadline. Silence this warning explicitly only if deferred review is intentional.",
			Evidence:             mustJSON(evidence),
			SafeNextStep:         "Review `recalldock candidates list --all` or run the configured `candidates sweep`; defer notifications with `doctor --silence-warning candidate_inbox` without changing candidates.",
			RequiresConfirmation: false,
		})
	}
	result := resultFromFindings(c.Code(), evidence, findings)
	result.Evidence = mustJSON(evidence)
	return result, nil
}
