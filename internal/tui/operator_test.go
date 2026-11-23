package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"recalldock.local/recalldock/internal/diagnostic"
	"recalldock.local/recalldock/internal/store"
)

func TestCandidateScreenApproveFlow(t *testing.T) {
	fx := newTestFixture(t)
	if _, err := fx.store.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{
		SessionID: "session-1", Project: "engram",
		Candidates: []store.MemoryCandidateInput{{Category: "decision", Statement: "tui candidate probe", Confidence: 0.8}},
	}); err != nil {
		t.Fatal(err)
	}
	m := New(fx.store, "")
	m.Cursor = menuCandidates
	updatedModel, cmd := m.handleDashboardSelection()
	updated := updatedModel.(Model)
	if updated.Screen != ScreenCandidates || cmd == nil {
		t.Fatalf("screen=%v", updated.Screen)
	}
	msg := cmd()
	loaded, ok := msg.(candidatesLoadedMsg)
	if !ok || loaded.err != nil || len(loaded.candidates) != 1 {
		t.Fatalf("candidates load: %+v", msg)
	}
	updatedModel, _ = updated.Update(loaded)
	updated = updatedModel.(Model)
	if len(updated.Candidates) != 1 {
		t.Fatalf("candidates=%d", len(updated.Candidates))
	}
	// 'a' opens the reason prompt; enter without a reason must not submit.
	updatedModel, _ = updated.handleCandidatesKeys("a")
	updated = updatedModel.(Model)
	if updated.CandidateAction != "approve" || !updated.CandidateReason.Focused() {
		t.Fatalf("prompt not opened: action=%q focused=%v", updated.CandidateAction, updated.CandidateReason.Focused())
	}
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	updatedModel, cmd = updated.Update(enter)
	updated = updatedModel.(Model)
	if cmd != nil || updated.CandidateFeedback == "" {
		t.Fatal("empty reason should be refused")
	}
	updated.CandidateReason.SetValue("tui approval")
	updatedModel, cmd = updated.handleCandidateReasonKeys(tea.KeyMsg{Type: tea.KeyEnter})
	updated = updatedModel.(Model)
	if cmd == nil {
		t.Fatal("reason submit should return the review command")
	}
	reviewed, ok := cmd().(candidateReviewedMsg)
	if !ok || reviewed.err != nil || !reviewed.approve {
		t.Fatalf("review: %+v", reviewed)
	}
	updatedModel, _ = updated.Update(reviewed)
	updated = updatedModel.(Model)
	if updated.CandidateFeedback != "approved "+reviewed.id && updated.CandidateFeedback == "" {
		t.Fatalf("feedback=%q", updated.CandidateFeedback)
	}
	list, err := fx.store.ListMemoryCandidates("engram", store.CandidateStatusReview, 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("inbox should be empty: %v %v", list, err)
	}
}

func TestDoctorScreenAndProjectCycle(t *testing.T) {
	fx := newTestFixture(t)
	root := t.TempDir()
	project, err := fx.store.CreateRegisteredProject("tui scope")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.store.BindProjectRoot(project.ID, root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	m := New(fx.store, "")
	if len(m.ProjectScopes) < 2 {
		t.Fatalf("expected all + detected scopes, got %d", len(m.ProjectScopes))
	}
	m.Cursor = menuDoctor
	updatedModel, cmd := m.handleDashboardSelection()
	updated := updatedModel.(Model)
	if updated.Screen != ScreenDoctor || cmd == nil {
		t.Fatalf("screen=%v", updated.Screen)
	}
	msg := cmd()
	report, ok := msg.(doctorLoadedMsg)
	if !ok || report.err != nil || len(report.report.Checks) == 0 {
		t.Fatalf("doctor report: %+v", report)
	}
	updatedModel, _ = updated.Update(report)
	updated = updatedModel.(Model)
	if updated.DoctorReport == nil || len(updated.DoctorReport.Checks) == 0 {
		t.Fatal("doctor report not stored")
	}

	// Project cycling rotates through all scopes and updates the label.
	before := updated.ProjectScopes[updated.ProjectIndex]
	updatedModel, cmd = updated.cycleProject()
	after := updatedModel.(Model)
	if cmd == nil {
		t.Fatal("cycle should refresh data")
	}
	if after.ProjectScopes[after.ProjectIndex] == before {
		t.Fatal("project scope did not change")
	}
	if after.Project != after.ProjectScopes[after.ProjectIndex].ID {
		t.Fatalf("project=%q scope=%q", after.Project, after.ProjectScopes[after.ProjectIndex].ID)
	}
}

func TestObservationDetailUsesIcons(t *testing.T) {
	project := "engram"
	m := New(nil, "")
	m.SelectedObservation = &store.Observation{ID: 7, Type: "note", Title: "icon probe", Content: "body", CreatedAt: "2026-01-01", Project: &project}
	out := m.viewObservationDetail()
	for _, want := range []string{"◆  Observation #7", "● Type:", "◆  Content"} {
		if !strings.Contains(out, want) {
			t.Fatalf("detail view missing %q", want)
		}
	}
}

func TestDoctorFilterAndDetailScrollClamp(t *testing.T) {
	m := New(nil, "")
	m.Width = 110
	m.Height = 30
	m.Screen = ScreenDoctor
	m.DoctorReport = &diagnostic.Report{
		Status:  "warning",
		Summary: diagnostic.Summary{Total: 2, OK: 1, Warnings: 1},
		Checks: []diagnostic.CheckResult{
			{CheckID: "passing_check", Result: "ok", ReasonCode: "ok_code", SafeNextStep: "No action required."},
			{CheckID: "warning_check", Result: "warning", ReasonCode: "warn_code", SafeNextStep: "Review the sample."},
		},
	}
	out := m.viewDoctor()
	if strings.Contains(out, "passing_check") {
		t.Fatal("passing checks should be hidden by default")
	}
	if !strings.Contains(out, "warning_check") {
		t.Fatal("warning check should be visible")
	}
	m.DoctorAll = true
	if out = m.viewDoctor(); !strings.Contains(out, "passing_check") {
		t.Fatal("DoctorAll should include passing checks")
	}

	// Detail scrolling stops at the real end of the wrapped content.
	m.Screen = ScreenObservationDetail
	m.SelectedObservation = &store.Observation{ID: 1, Content: "short body"}
	for i := 0; i < 5; i++ {
		updated, _ := m.handleObservationDetailKeys("down")
		m = updated.(Model)
	}
	if m.DetailScroll != 0 {
		t.Fatalf("short content must not scroll, got %d", m.DetailScroll)
	}
	m.SelectedObservation = &store.Observation{ID: 2, Content: strings.Repeat("line of text\n", 80)}
	updated, _ := m.handleObservationDetailKeys("end")
	m = updated.(Model)
	max := m.detailMaxScroll()
	if max == 0 || m.DetailScroll != max {
		t.Fatalf("end should jump to max scroll %d, got %d", max, m.DetailScroll)
	}
	updated, _ = m.handleObservationDetailKeys("down")
	if updated.(Model).DetailScroll != max {
		t.Fatal("down at the end must not grow the scroll counter")
	}
}

func TestProjectCycleWorksFromListScreens(t *testing.T) {
	fx := newTestFixture(t)
	m := New(fx.store, "")
	m.Screen = ScreenRecent
	m.RecentObservations = []store.Observation{{ID: 1, Title: "t", Content: "c"}}
	before := m.Project
	updatedModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	updated := updatedModel.(Model)
	if updated.Project == before && len(updated.ProjectScopes) > 1 {
		t.Fatalf("p should change the scope, still %q", updated.Project)
	}
	if updated.Screen != ScreenRecent {
		t.Fatalf("p should stay on the current screen, got %v", updated.Screen)
	}
	if cmd == nil {
		t.Fatal("p should reload data for the new scope")
	}
}

func TestCandidateCursorClampsAtBounds(t *testing.T) {
	fx := newTestFixture(t)
	m := New(fx.store, "")
	m.Screen = ScreenCandidates
	m.Candidates = []store.MemoryCandidate{{SyncID: "cand-1"}, {SyncID: "cand-2"}}
	for i := 0; i < 5; i++ {
		updated, _ := m.handleCandidatesKeys("down")
		m = updated.(Model)
	}
	if m.CandidateCursor != 1 {
		t.Fatalf("cursor should stop at the last candidate, got %d", m.CandidateCursor)
	}
	updated, _ := m.handleCandidatesKeys("home")
	if updated.(Model).CandidateCursor != 0 {
		t.Fatal("home should return to the first candidate")
	}
}

func TestTUISanitizesTerminalEscapes(t *testing.T) {
	m := New(nil, "")
	m.Width = 110
	m.Height = 30
	m.Screen = ScreenObservationDetail
	m.SelectedObservation = &store.Observation{
		ID:      9,
		Title:   "title \x1b[2J cleared",
		Content: "body \x1b[31mRED\x1b[0m \x1b]52;c;SGVsbG8=\x07 osc52",
	}
	out := m.viewObservationDetail()
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Fatalf("detail view leaked a terminal control sequence: %q", out)
	}
	if !strings.Contains(out, "RED") || !strings.Contains(out, "cleared") {
		t.Fatal("sanitizing must keep printable text")
	}

	m.Screen = ScreenCandidates
	m.Candidates = []store.MemoryCandidate{{SyncID: "cand-1", Statement: "statement \x1b[31mX\x1b[0m", Category: "decision", Project: "engram"}}
	if out := m.viewCandidates(); strings.ContainsAny(out, "\x1b\x07") {
		t.Fatalf("candidate view leaked a terminal control sequence: %q", out)
	}
}

func TestShortIDPreservesUTF8(t *testing.T) {
	value := "项目名称项目名称项目名称项目名称"
	short := shortID(value)
	if !utf8.ValidString(short) {
		t.Fatalf("shortID produced invalid UTF-8: %q", short)
	}
	if !strings.HasPrefix(short, "项目名称项目名称项目名") {
		t.Fatalf("shortID prefix = %q", short)
	}
	projectShort := shortProjectText(strings.Repeat("项目名称", 7))
	if !utf8.ValidString(projectShort) || !strings.Contains(projectShort, "…") {
		t.Fatalf("shortProjectText = %q", projectShort)
	}
}

func TestTimelineReturnsToDetail(t *testing.T) {
	m := New(nil, "")
	m.PrevScreen = ScreenSearchResults
	m.SelectedObservation = &store.Observation{ID: 7, Content: "x"}
	updated, cmd := m.handleObservationDetailKeys("t")
	if cmd == nil {
		t.Fatal("t should request a timeline")
	}
	back := updated.(Model)
	if back.PrevScreen != ScreenObservationDetail {
		t.Fatalf("timeline should return to detail, got %v", back.PrevScreen)
	}
}
