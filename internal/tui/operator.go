package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"recalldock.local/recalldock/internal/diagnostic"
	"recalldock.local/recalldock/internal/store"
)

// ─── Project scope ───────────────────────────────────────────────────────────

type projectScope struct {
	ID    string
	Label string
}

// shortProject renders a scope compactly for the header.
func shortProject(scope projectScope) string {
	if scope.ID == "" {
		return "all projects"
	}
	if scope.Label != "" && scope.Label != scope.ID {
		label := sanitizeText(scope.Label)
		runes := []rune(label)
		if len(runes) > 22 {
			return string(runes[:22]) + "…"
		}
		return label
	}
	return shortID(scope.ID)
}

func currentScope(m Model) projectScope {
	if m.ProjectIndex >= 0 && m.ProjectIndex < len(m.ProjectScopes) {
		return m.ProjectScopes[m.ProjectIndex]
	}
	return projectScope{ID: m.Project, Label: m.Project}
}

// cycleProject moves to the next scope and reloads the visible screen.
func (m Model) cycleProject() (tea.Model, tea.Cmd) {
	if len(m.ProjectScopes) == 0 {
		return m, nil
	}
	m.ProjectIndex = (m.ProjectIndex + 1) % len(m.ProjectScopes)
	m.Project = m.ProjectScopes[m.ProjectIndex].ID
	m.Cursor = 0
	m.Scroll = 0
	m.CandidateCursor = 0
	m.DoctorScroll = 0
	m.SimilarCursor = 0
	m.SimilarScroll = 0
	return m, tea.Batch(loadStats(m.store, m.Project), m.refreshScreen(m.Screen))
}

// ─── Candidates ──────────────────────────────────────────────────────────────

type candidatesLoadedMsg struct {
	candidates []store.MemoryCandidate
	err        error
}

type candidateReviewedMsg struct {
	id      string
	approve bool
	err     error
}

// loadCandidates reads the review inbox for the active scope.
func loadCandidates(s *store.Store, project string) tea.Cmd {
	return func() tea.Msg {
		items, err := s.ListMemoryCandidates(project, store.CandidateStatusReview, 50)
		return candidatesLoadedMsg{candidates: items, err: err}
	}
}

// reviewCandidate applies an approve/reject decision with the snapshot digest
// computed at decision time; a stale candidate fails instead of being changed.
func reviewCandidate(s *store.Store, candidate store.MemoryCandidate, reason string, approve bool) tea.Cmd {
	return func() tea.Msg {
		digest, err := store.MemoryCandidateDigest(&candidate)
		if err != nil {
			return candidateReviewedMsg{id: candidate.SyncID, approve: approve, err: err}
		}
		if approve {
			_, err = s.ApproveMemoryCandidate(candidate.SyncID, digest, reason)
		} else {
			err = s.RejectMemoryCandidateChecked(candidate.SyncID, digest, reason)
		}
		return candidateReviewedMsg{id: candidate.SyncID, approve: approve, err: err}
	}
}

func (m Model) handleCandidatesKeys(key string) (tea.Model, tea.Cmd) {
	visible := m.candidateWindow()
	switch key {
	case "up", "k":
		m.CandidateCursor = clampIndex(m.CandidateCursor-1, len(m.Candidates))
	case "down", "j":
		m.CandidateCursor = clampIndex(m.CandidateCursor+1, len(m.Candidates))
	case "pgup":
		m.CandidateCursor = clampIndex(m.CandidateCursor-visible, len(m.Candidates))
	case "pgdown":
		m.CandidateCursor = clampIndex(m.CandidateCursor+visible, len(m.Candidates))
	case "home", "g":
		m.CandidateCursor = 0
	case "end", "G":
		if len(m.Candidates) > 0 {
			m.CandidateCursor = len(m.Candidates) - 1
		}
	case "a":
		if len(m.Candidates) > 0 && m.CandidateCursor < len(m.Candidates) {
			m.CandidateAction = "approve"
			m.CandidateReason.SetValue("")
			m.CandidateReason.Focus()
		}
	case "r":
		if len(m.Candidates) > 0 && m.CandidateCursor < len(m.Candidates) {
			m.CandidateAction = "reject"
			m.CandidateReason.SetValue("")
			m.CandidateReason.Focus()
		}
	case "esc", "q":
		if m.CandidateReason.Focused() {
			m.CandidateReason.Blur()
			m.CandidateAction = ""
			return m, nil
		}
		m.Screen = ScreenDashboard
		m.Cursor = 0
		m.CandidateCursor = 0
		return m, loadStats(m.store, m.Project)
	}
	return m, nil
}

// candidateWindow is how many candidate rows fit below the header and detail.
func (m Model) candidateWindow() int {
	limit := m.Height - 14
	if limit < 3 {
		limit = 3
	}
	return limit
}

func (m Model) handleCandidateReasonKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		reason := strings.TrimSpace(m.CandidateReason.Value())
		if reason == "" {
			m.CandidateFeedback = "必须填写理由"
			return m, nil
		}
		if m.CandidateCursor >= len(m.Candidates) {
			m.CandidateReason.Blur()
			m.CandidateAction = ""
			return m, nil
		}
		candidate := m.Candidates[m.CandidateCursor]
		approve := m.CandidateAction == "approve"
		m.CandidateReason.Blur()
		m.CandidateAction = ""
		return m, tea.Batch(
			reviewCandidate(m.store, candidate, reason, approve),
		)
	case "esc":
		m.CandidateReason.Blur()
		m.CandidateAction = ""
		return m, nil
	}
	var cmd tea.Cmd
	m.CandidateReason, cmd = m.CandidateReason.Update(msg)
	return m, cmd
}

// viewCandidates renders the review inbox. The reason prompt appears inline so
// a decision is never one keystroke away, and the selected candidate's digest is
// shown before the decision is submitted.
func (m Model) viewCandidates() string {
	var b strings.Builder
	b.WriteString(m.pageTitle("✓  Candidate review",
		fmt.Sprintf("%d pending · %s", len(m.Candidates), shortProject(currentScope(m)))))
	b.WriteString("\n")

	if len(m.Candidates) == 0 {
		b.WriteString(noResultsStyle.Render("No pending candidates."))
		b.WriteString("\n")
		b.WriteString(pageSubStyle.Render("  模型提交候选后会出现在这里；批准才写入正式知识。"))
		b.WriteString(m.footer("r reload   esc back"))
		return b.String()
	}

	limit := m.candidateWindow()
	start := 0
	if m.CandidateCursor >= limit {
		start = m.CandidateCursor - limit + 1
	}
	end := start + limit
	if end > len(m.Candidates) {
		end = len(m.Candidates)
	}
	width := m.innerWidth()
	for i := start; i < end; i++ {
		c := m.Candidates[i]
		if i == m.CandidateCursor {
			plain := fmt.Sprintf("%s  [%s]  %s", sanitizeText(c.SyncID), categoryLabel(c.Category),
				clipCells(sanitizeText(c.Statement), width-len(c.SyncID)-20))
			b.WriteString(listSelectedStyle.Width(width - 1).Render(clipCells("▸ "+plain, width-1)))
		} else {
			line := fmt.Sprintf("%s  %s  %s",
				idStyle.Render(sanitizeText(c.SyncID)),
				typeBadgeStyle.Render("["+categoryLabel(c.Category)+"]"),
				clipCells(sanitizeText(c.Statement), width-len(c.SyncID)-20))
			b.WriteString(listItemStyle.Render("  " + line))
		}
		b.WriteString("\n")
	}
	if len(m.Candidates) > limit {
		b.WriteString("  " + timestampStyle.Render(fmt.Sprintf("showing %d-%d of %d", start+1, end, len(m.Candidates))))
		b.WriteString("\n")
	}

	// Selected candidate detail: the digest is what the write is checked against.
	if m.CandidateCursor < len(m.Candidates) {
		c := m.Candidates[m.CandidateCursor]
		digest, _ := store.MemoryCandidateDigest(&c)
		b.WriteString("\n")
		b.WriteString(sectionHeadingStyle.Render("◆  选中候选"))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("  %s %s   %s %s\n",
			detailLabelStyle.Render("Category:"), typeBadgeStyle.Render(categoryLabel(c.Category)),
			detailLabelStyle.Render("Project:"), projectStyle.Render(shortID(c.Project))))
		b.WriteString(fmt.Sprintf("  %s %s\n", detailLabelStyle.Render("Expires:"), timestampStyle.Render(expiryText(c.ExpiresAt))))
		b.WriteString(fmt.Sprintf("  %s %s\n", detailLabelStyle.Render("Digest:"), timestampStyle.Render(shortID(digest))))
		b.WriteString("\n")
		for _, line := range wrapCells(sanitizeText(c.Statement), width-4) {
			b.WriteString(pageSubStyle.Render("  " + line))
			b.WriteString("\n")
		}
	}

	if m.CandidateAction != "" {
		action := "批准"
		if m.CandidateAction == "reject" {
			action = "拒绝"
		}
		b.WriteString("\n")
		b.WriteString(sectionHeadingStyle.Render("◆  " + action + "理由（必填）"))
		b.WriteString("\n")
		b.WriteString("  " + m.CandidateReason.View())
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("  enter 提交   esc 取消"))
		b.WriteString("\n")
	}
	if m.CandidateFeedback != "" {
		b.WriteString(copyFeedbackStyle.Render(" " + sanitizeText(m.CandidateFeedback) + " "))
	}
	b.WriteString(m.footer("j/k move   a approve   r reject   p project   esc back"))
	return b.String()
}

// ─── Doctor ──────────────────────────────────────────────────────────────────

type doctorLoadedMsg struct {
	report diagnostic.Report
	err    error
}

func runDoctor(s *store.Store, project string) tea.Cmd {
	return func() tea.Msg {
		report, err := diagnostic.NewRunner().RunAll(context.Background(), diagnostic.Scope{Store: s, Project: project, Now: time.Now()})
		return doctorLoadedMsg{report: report, err: err}
	}
}

// doctorVisibleChecks filters the report: passing checks are noise unless the
// operator explicitly asks for them.
func (m Model) doctorVisibleChecks() []diagnostic.CheckResult {
	if m.DoctorReport == nil {
		return nil
	}
	if m.DoctorAll {
		return m.DoctorReport.Checks
	}
	checks := make([]diagnostic.CheckResult, 0, len(m.DoctorReport.Checks))
	for _, check := range m.DoctorReport.Checks {
		if check.Result != "ok" {
			checks = append(checks, check)
		}
	}
	return checks
}

func (m Model) handleDoctorKeys(key string) (tea.Model, tea.Cmd) {
	checks := m.doctorVisibleChecks()
	switch key {
	case "r":
		return m, runDoctor(m.store, m.Project)
	case "a":
		m.DoctorAll = !m.DoctorAll
		m.DoctorScroll = 0
	case "up", "k":
		if m.DoctorScroll > 0 {
			m.DoctorScroll--
		}
	case "down", "j":
		if m.DoctorScroll < len(checks)-1 {
			m.DoctorScroll++
		}
	case "pgup":
		m.DoctorScroll -= 5
		if m.DoctorScroll < 0 {
			m.DoctorScroll = 0
		}
	case "pgdown":
		m.DoctorScroll += 5
		if m.DoctorScroll > len(checks)-1 {
			m.DoctorScroll = maxInt(0, len(checks)-1)
		}
	case "home", "g":
		m.DoctorScroll = 0
	case "end", "G":
		m.DoctorScroll = maxInt(0, len(checks)-1)
	case "esc", "q":
		m.Screen = ScreenDashboard
		m.Cursor = 0
		m.DoctorScroll = 0
		return m, loadStats(m.store, m.Project)
	}
	return m, nil
}

func (m Model) viewDoctor() string {
	var b strings.Builder
	b.WriteString(m.pageTitle("✚  Doctor", shortProject(currentScope(m))))
	b.WriteString("\n")
	if m.DoctorReport == nil {
		b.WriteString(noResultsStyle.Render("Running checks…"))
		return b.String()
	}
	report := m.DoctorReport
	statusBadge := copyFeedbackStyle
	label := "全部通过"
	switch {
	case report.Status == "error" || report.Status == "blocked":
		statusBadge = errorStyle
		label = "存在错误"
	case report.Status == "warning":
		statusBadge = updateBannerStyle
		label = "需要关注"
	}
	b.WriteString("  " + statusBadge.Render(" "+label+" ") + "  " +
		timestampStyle.Render(fmt.Sprintf("共 %d 项 · 通过 %d · 警告 %d · 错误 %d · 阻塞 %d",
			report.Summary.Total, report.Summary.OK, report.Summary.Warnings, report.Summary.Errors, report.Summary.Blocked)))
	b.WriteString("\n\n")

	checks := m.doctorVisibleChecks()
	width := m.innerWidth()
	if len(checks) == 0 {
		b.WriteString(noResultsStyle.Render("没有需要处理的问题。"))
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("  a 显示全部检查   r 重跑   esc 返回"))
		b.WriteString(m.footer("a show all   r rerun   esc back"))
		return b.String()
	}

	limit := m.Height - 13
	if limit < 3 {
		limit = 3
	}
	start := 0
	if m.DoctorScroll >= limit {
		start = m.DoctorScroll - limit + 1
	}
	end := start + limit
	if end > len(checks) {
		end = len(checks)
	}
	for i := start; i < end; i++ {
		check := checks[i]
		marker := lipgloss.NewStyle().Foreground(colorGreen).Render("✓")
		lineStyle := menuItemStyle
		switch {
		case check.Result == "error" || check.Result == "blocked":
			marker = lipgloss.NewStyle().Foreground(colorRed).Render("✗")
		case check.Result != "ok":
			marker = lipgloss.NewStyle().Foreground(colorPeach).Render("!")
		}
		if i == m.DoctorScroll {
			plain := fmt.Sprintf("! %-34s %s", sanitizeText(check.CheckID), sanitizeText(check.ReasonCode))
			b.WriteString(listSelectedStyle.Width(width - 1).Render(clipCells("▸ "+plain, width-1)))
		} else {
			line := fmt.Sprintf("%s %-34s %s", marker, sanitizeText(check.CheckID), timestampStyle.Render(sanitizeText(check.ReasonCode)))
			b.WriteString(lineStyle.Render("  " + line))
		}
		b.WriteString("\n")
	}
	if len(checks) > limit {
		b.WriteString("  " + timestampStyle.Render(fmt.Sprintf("showing %d-%d of %d", start+1, end, len(checks))))
		b.WriteString("\n")
	}

	if m.DoctorScroll < len(checks) {
		check := checks[m.DoctorScroll]
		b.WriteString("\n")
		b.WriteString(sectionHeadingStyle.Render("◆  " + sanitizeText(check.CheckID)))
		b.WriteString("\n")
		for _, finding := range check.Findings {
			for _, line := range wrapCells(sanitizeText(finding.Message), width-4) {
				b.WriteString(pageSubStyle.Render("  " + line))
				b.WriteString("\n")
			}
		}
		if check.SafeNextStep != "" {
			step := strings.TrimSpace(check.SafeNextStep)
			if strings.EqualFold(step, "No action required.") {
				step = "无需操作。"
			}
			for _, line := range wrapCells("next: "+sanitizeText(step), width-4) {
				b.WriteString(helpStyle.Render("  " + line))
				b.WriteString("\n")
			}
		}
	}
	if !m.DoctorAll && report.Summary.OK > 0 {
		b.WriteString("\n  " + timestampStyle.Render(fmt.Sprintf("已隐藏 %d 项通过的检查（a 显示全部）", report.Summary.OK)))
	}
	b.WriteString(m.footer("j/k scroll   a all checks   r rerun   esc back"))
	return b.String()
}

// categoryLabel is the operator-facing name for a candidate category.
func categoryLabel(value string) string {
	switch value {
	case "decision":
		return "决策"
	case "procedure":
		return "流程"
	case "project_constraint":
		return "项目约束"
	case "user_preference":
		return "用户偏好"
	case "user_fact":
		return "用户事实"
	case "failure_avoidance":
		return "避免重犯"
	case "tool_behavior":
		return "工具行为"
	case "verified_outcome":
		return "已验证结果"
	case "agent_identity":
		return "代理身份"
	case "session_context":
		return "会话上下文"
	case "task_state":
		return "任务状态"
	case "":
		return "未分类"
	default:
		return sanitizeText(value)
	}
}

// expiryText renders a candidate expiry as a short, direction-correct label.
func expiryText(value string) string {
	trimmed := strings.TrimSpace(sanitizeText(value))
	if trimmed == "" {
		return "—"
	}
	var parsed time.Time
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, time.RFC3339Nano} {
		if candidate, err := time.Parse(layout, trimmed); err == nil {
			parsed = candidate
			break
		}
	}
	if parsed.IsZero() {
		return trimmed
	}
	left := time.Until(parsed)
	if left <= 0 {
		return "已过期（" + parsed.Format("01-02") + "）"
	}
	if left < 48*time.Hour {
		return fmt.Sprintf("还剩 %d 小时", int(left.Hours())+1)
	}
	return fmt.Sprintf("还剩 %d 天", int(left.Hours()/24))
}

// clampIndex keeps a cursor inside [0, count-1]; an empty list pins it at 0.
func clampIndex(value, count int) int {
	if count <= 0 || value < 0 {
		return 0
	}
	if value > count-1 {
		return count - 1
	}
	return value
}

func clipText(value string, limit int) string {
	runes := []rune(strings.TrimSpace(sanitizeText(value)))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}
