package diagnostic

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/store"
)

func newDiagnosticTestStore(t *testing.T) *store.Store {
	t.Helper()
	cfg, err := store.DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.DataDir = t.TempDir()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLiteLockContentionBranches(t *testing.T) {
	s := newDiagnosticTestStore(t)
	tests := []struct {
		name       string
		snapshot   store.SQLiteLockSnapshot
		probeErr   error
		wantStatus string
		wantReason string
	}{
		{
			name:       "healthy snapshot is ok",
			snapshot:   store.SQLiteLockSnapshot{JournalMode: "wal", BusyTimeoutMS: 5000, CheckpointBusy: 0, CheckpointLog: 2, CheckpointedFrames: 2},
			wantStatus: StatusOK,
			wantReason: CheckSQLiteLockContention + "_ok",
		},
		{
			name:       "checkpoint busy is warning",
			snapshot:   store.SQLiteLockSnapshot{JournalMode: "wal", BusyTimeoutMS: 5000, CheckpointBusy: 3, CheckpointLog: 7, CheckpointedFrames: 4},
			wantStatus: StatusWarning,
			wantReason: "sqlite_lock_contention_detected",
		},
		{
			name:       "probe failure is error",
			probeErr:   errors.New("probe unavailable"),
			wantStatus: StatusError,
			wantReason: "sqlite_lock_probe_failed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report, err := NewRunner().RunOne(context.Background(), Scope{
				Store:   s,
				Project: "engram",
				ReadSQLiteLockSnapshot: func(context.Context) (store.SQLiteLockSnapshot, error) {
					return tc.snapshot, tc.probeErr
				},
			}, CheckSQLiteLockContention)
			if err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			if report.Status != tc.wantStatus || report.Checks[0].ReasonCode != tc.wantReason {
				t.Fatalf("status=%s reason=%s report=%+v", report.Status, report.Checks[0].ReasonCode, report)
			}
		})
	}
}

func TestSilenceWarningsIsExplicitAndCannotHideErrors(t *testing.T) {
	report := buildReport("p", []CheckResult{{CheckID: CheckCandidateInbox, Result: StatusWarning, Severity: SeverityWarning}, {CheckID: CheckNetworkBoundary, Result: StatusError, Severity: SeverityError}})
	silenced, err := report.SilenceWarnings([]string{CheckCandidateInbox, CheckNetworkBoundary})
	if err != nil {
		t.Fatal(err)
	}
	if silenced.Summary.Warnings != 0 || silenced.Summary.SilencedWarnings != 1 || silenced.Summary.Errors != 1 || silenced.Status != StatusError || !silenced.Checks[0].Silenced || silenced.Checks[1].Silenced || report.Checks[0].Silenced {
		t.Fatalf("silence lost actual findings or mutated original: %+v", silenced)
	}
	if _, err := report.SilenceWarnings([]string{"candidateinbox"}); !errors.Is(err, ErrInvalidCheck) {
		t.Fatalf("unknown silence code ignored: %v", err)
	}
}

func TestRegistryLookupAndOrdering(t *testing.T) {
	codes := RegisteredCodes()
	want := []string{CheckCandidateInbox, CheckManualSessionNameProjectMismatch, CheckMemoryClassification, CheckNetworkBoundary, CheckNonEnrolledSyncBacklog, CheckSessionOrigin, CheckSessionProjectDirectoryMismatch, CheckSQLiteLockContention, CheckStaleOpenSessions, CheckSyncMutationRequiredFields}
	if strings.Join(codes, ",") != strings.Join(want, ",") {
		t.Fatalf("RegisteredCodes = %v, want %v", codes, want)
	}
	if _, err := DefaultRegistry().Lookup("not_real"); err == nil {
		t.Fatal("expected invalid check error")
	}
}

func TestRunnerRollsUpBlockedFindings(t *testing.T) {
	s := newDiagnosticTestStore(t)
	runner := NewRunnerWithRegistry(NewRegistry(fakeBlockedCheck{}))
	report, err := runner.RunOne(context.Background(), Scope{Store: s, Project: "engram", Now: time.Now()}, "fake_blocked")
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusBlocked || report.Summary.Blocked != 1 {
		t.Fatalf("status=%s summary=%+v", report.Status, report.Summary)
	}
	if got := report.Checks[0].Findings[0].ReasonCode; got != "fake_blocked_reason" {
		t.Fatalf("reason_code=%q", got)
	}
}

type fakeBlockedCheck struct{}

func (fakeBlockedCheck) Code() string { return "fake_blocked" }
func (fakeBlockedCheck) Run(context.Context, Scope) (CheckResult, error) {
	return resultFromFindings("fake_blocked", map[string]any{"evaluated": true}, []Finding{{CheckID: "fake_blocked", Severity: SeverityBlocking, ReasonCode: "fake_blocked_reason", Message: "blocked", Why: "test", Evidence: mustJSON(map[string]any{"ok": false}), SafeNextStep: "none"}}), nil
}

func TestSessionProjectDirectoryMismatchFinding(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if err := s.CreateSession("s1", "api", "/work/web"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "s1", Type: "decision", Title: "active", Content: "active mismatch", Project: "api"}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	report, err := NewRunner().RunOne(context.Background(), Scope{
		Store:   s,
		Project: "api",
		DetectProject: func(dir string) (DetectedProject, bool) {
			if dir == "/work/web" {
				return DetectedProject{Project: "web", Source: "test", Path: dir}, true
			}
			return DetectedProject{}, false
		},
	}, CheckSessionProjectDirectoryMismatch)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusWarning || len(report.Checks[0].Findings) != 1 {
		t.Fatalf("report=%+v", report)
	}
}

func TestSessionProjectDirectoryMismatchIgnoresExplainedDrift(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		project   string
		addActive bool
		detected  string
	}{
		{name: "historical session without active observations", sessionID: "historical", project: "api", detected: "web"},
		{name: "explicit manual project override", sessionID: "manual-save-api", project: "api", addActive: true, detected: "web"},
		{name: "canonical project identity", sessionID: "canonical", project: "web@123456abcdef", addActive: true, detected: "web"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newDiagnosticTestStore(t)
			if err := s.CreateSession(tc.sessionID, tc.project, "/work/web"); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if tc.addActive {
				if _, err := s.AddObservation(store.AddObservationParams{SessionID: tc.sessionID, Type: "decision", Title: "active", Content: "active explained drift", Project: tc.project}); err != nil {
					t.Fatalf("AddObservation: %v", err)
				}
			}
			report, err := NewRunner().RunOne(context.Background(), Scope{
				Store: s,
				DetectProject: func(string) (DetectedProject, bool) {
					return DetectedProject{Project: tc.detected, Source: "test", Path: "/work/web"}, true
				},
			}, CheckSessionProjectDirectoryMismatch)
			if err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			if report.Status != StatusOK || len(report.Checks[0].Findings) != 0 {
				t.Fatalf("report=%+v", report)
			}
		})
	}
}

func TestRunnerRunAllHealthyEvaluatesEveryMVPCheck(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if err := s.EnrollProject("engram"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}
	if err := s.CreateSession("manual-save-engram", "engram", "/work/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	report, err := NewRunner().RunAll(context.Background(), Scope{
		Store:   s,
		Project: "engram",
		ReadSQLiteLockSnapshot: func(context.Context) (store.SQLiteLockSnapshot, error) {
			return store.SQLiteLockSnapshot{JournalMode: "wal", BusyTimeoutMS: 5000, CheckpointBusy: 0}, nil
		},
	})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if report.Status != StatusOK || report.Summary.OK != 10 || len(report.Checks) != 10 {
		t.Fatalf("report=%+v", report)
	}
	for _, check := range report.Checks {
		if check.Result != StatusOK || len(check.Evidence) == 0 {
			t.Fatalf("expected ok check with evidence, got %+v", check)
		}
	}
}

func TestStaleOpenSessionsSeparatesManualSaveContainers(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if err := s.CreateSession("ordinary-stale", "engram", "/work/engram"); err != nil {
		t.Fatalf("CreateSession ordinary: %v", err)
	}
	if err := s.CreateSession("manual-save-engram", "engram", "/work/engram"); err != nil {
		t.Fatalf("CreateSession manual: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE sessions SET started_at = '2026-07-30 00:00:00' WHERE id IN ('ordinary-stale', 'manual-save-engram')`); err != nil {
		t.Fatalf("age sessions: %v", err)
	}

	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s, Project: "engram", Now: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}, CheckStaleOpenSessions)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusWarning || len(report.Checks[0].Findings) != 1 {
		t.Fatalf("report=%+v", report)
	}
	evidence := string(report.Checks[0].Evidence)
	if !strings.Contains(evidence, `"ordinary_stale":1`) || !strings.Contains(evidence, `"manual_stale":1`) {
		t.Fatalf("expected grouped ordinary/manual counts, evidence=%s", evidence)
	}
}

func TestNonEnrolledSyncBacklogReportsCountsAndBytesWithoutPayloads(t *testing.T) {
	s := newDiagnosticTestStore(t)
	if err := s.EnrollProject("engram"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}
	if err := s.CreateSession("backlog-sensitive-id", "engram", "/work/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.UnenrollProject("engram"); err != nil {
		t.Fatalf("UnenrollProject: %v", err)
	}

	report, err := NewRunner().RunOne(context.Background(), Scope{Store: s, Project: "engram"}, CheckNonEnrolledSyncBacklog)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if report.Status != StatusWarning || len(report.Checks[0].Findings) != 1 {
		t.Fatalf("report=%+v", report)
	}
	evidence := string(report.Checks[0].Evidence)
	if !strings.Contains(evidence, `"total_count":1`) || !strings.Contains(evidence, `"total_payload_bytes":`) {
		t.Fatalf("missing aggregate backlog evidence: %s", evidence)
	}
	if strings.Contains(evidence, "backlog-sensitive-id") || strings.Contains(evidence, "payload") && strings.Contains(evidence, "directory") {
		t.Fatalf("diagnostic evidence must not include mutation payloads: %s", evidence)
	}
}
