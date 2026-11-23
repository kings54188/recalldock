package diagnostic

import (
	"context"
	"strings"
	"time"

	"recalldock.local/recalldock/internal/store"
)

const (
	CheckNonEnrolledSyncBacklog = "non_enrolled_sync_backlog"
	CheckStaleOpenSessions      = "stale_open_sessions"
)

type NonEnrolledSyncBacklogCheck struct{}
type StaleOpenSessionsCheck struct{}

func (NonEnrolledSyncBacklogCheck) Code() string { return CheckNonEnrolledSyncBacklog }
func (StaleOpenSessionsCheck) Code() string      { return CheckStaleOpenSessions }

func (c NonEnrolledSyncBacklogCheck) Run(ctx context.Context, scope Scope) (CheckResult, error) {
	_ = ctx
	counts, err := scope.Store.CountPendingNonEnrolledSyncMutations(store.DefaultSyncTargetKey)
	if err != nil {
		return CheckResult{}, err
	}
	projectFilter := normalizeProjectName(scope.Project)
	projects := make([]store.PendingSyncMutationProjectCount, 0, len(counts))
	var totalCount, totalBytes int64
	for _, count := range counts {
		if projectFilter != "" && normalizeProjectName(count.Project) != projectFilter {
			continue
		}
		count.MaxSeq = 0
		projects = append(projects, count)
		totalCount += count.Count
		totalBytes += count.PayloadBytes
	}
	evidence := map[string]any{
		"project_count": len(projects), "total_count": totalCount,
		"total_payload_bytes": totalBytes, "projects": projects,
	}
	findings := make([]Finding, 0, 1)
	if totalCount > 0 {
		findings = append(findings, Finding{
			CheckID: c.Code(), Severity: SeverityWarning, ReasonCode: "non_enrolled_sync_backlog_detected",
			Message:              "Pending sync mutations have no enrolled project consumer.",
			Why:                  "A non-enrolled mutation journal cannot be drained by autosync and grows indefinitely unless it is reviewed or explicitly skipped.",
			Evidence:             mustJSON(evidence),
			SafeNextStep:         "Run `recalldock sync cleanup --non-enrolled --dry-run --json`; apply only after reviewing counts and the reported online backup path.",
			RequiresConfirmation: true,
		})
	}
	result := resultFromFindings(c.Code(), evidence, findings)
	result.Evidence = mustJSON(evidence)
	return result, nil
}

func (c StaleOpenSessionsCheck) Run(ctx context.Context, scope Scope) (CheckResult, error) {
	_ = ctx
	sessions, err := scope.Store.ListDiagnosticSessions(scope.Project)
	if err != nil {
		return CheckResult{}, err
	}
	now := scope.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	const threshold = 24 * time.Hour
	type staleSession struct {
		ID        string  `json:"id"`
		Project   string  `json:"project"`
		StartedAt string  `json:"started_at"`
		AgeHours  float64 `json:"age_hours"`
	}
	sample := make([]staleSession, 0, 20)
	ordinaryOpen, ordinaryStale, manualOpen, manualStale, invalidTimes := 0, 0, 0, 0, 0
	for _, session := range sessions {
		if session.EndedAt != nil && strings.TrimSpace(*session.EndedAt) != "" {
			continue
		}
		manual := strings.HasPrefix(session.ID, "manual-save")
		if manual {
			manualOpen++
		} else {
			ordinaryOpen++
		}
		startedAt, ok := parseDiagnosticSessionTime(session.StartedAt)
		if !ok {
			invalidTimes++
			continue
		}
		age := now.Sub(startedAt)
		if age < threshold {
			continue
		}
		if manual {
			manualStale++
			continue
		}
		ordinaryStale++
		if len(sample) < cap(sample) {
			sample = append(sample, staleSession{ID: session.ID, Project: session.Project, StartedAt: session.StartedAt, AgeHours: age.Hours()})
		}
	}
	evidence := map[string]any{
		"threshold_hours": 24, "ordinary_open": ordinaryOpen, "ordinary_stale": ordinaryStale,
		"manual_open": manualOpen, "manual_stale": manualStale, "invalid_started_at": invalidTimes,
		"ordinary_stale_sample": sample,
	}
	findings := make([]Finding, 0, 1)
	if ordinaryStale > 0 {
		findings = append(findings, Finding{
			CheckID: c.Code(), Severity: SeverityWarning, ReasonCode: "stale_ordinary_sessions_detected",
			Message:              "Ordinary sessions have remained open beyond the 24-hour operational threshold.",
			Why:                  "Open ordinary sessions can make lifecycle metrics and interruption recovery appear active long after work stopped; manual-save containers are counted separately and are not treated as leaks.",
			Evidence:             mustJSON(evidence),
			SafeNextStep:         "Review the sampled session IDs and end only sessions whose owning task is confirmed inactive; do not bulk-close manual-save containers.",
			RequiresConfirmation: true,
		})
	}
	result := resultFromFindings(c.Code(), evidence, findings)
	result.Evidence = mustJSON(evidence)
	return result, nil
}

func parseDiagnosticSessionTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}
