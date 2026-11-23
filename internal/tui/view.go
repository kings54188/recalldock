package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"recalldock.local/recalldock/internal/timeutil"
	"recalldock.local/recalldock/internal/version"
)

// ─── Layout primitives ───────────────────────────────────────────────────────

// innerWidth is the usable content width inside the app padding.
func (m Model) innerWidth() int {
	width := m.Width - 4
	if width < 40 {
		width = 40
	}
	if width > 160 {
		width = 160
	}
	return width
}

// header renders the persistent brand/scope bar and a rule under it.
func (m Model) header() string {
	left := brandStyle.Render("◆") + " " + brandNameStyle.Render("RecallDock") + "  " + versionStyle.Render(sanitizeText(m.Version))
	scope := scopeStyle.Render("● " + shortProject(currentScope(m)))
	gap := m.innerWidth() - lipgloss.Width(left) - lipgloss.Width(scope)
	if gap < 1 {
		gap = 1
	}
	lines := []string{left + strings.Repeat(" ", gap) + scope}
	if m.ProjectHint != "" {
		lines = append(lines, scopeHintStyle.Render("  未绑定目录（git 提示：")+scopeHintStyle.Render(sanitizeText(m.ProjectHint))+scopeHintStyle.Render("）· p 切换项目"))
	}
	lines = append(lines, ruleStyle.Render(strings.Repeat("─", m.innerWidth())))
	return strings.Join(lines, "\n")
}

// pageTitle renders the screen title with an optional dim subtitle.
func (m Model) pageTitle(title, subtitle string) string {
	out := pageTitleStyle.Render(sanitizeText(title))
	if subtitle != "" && m.Width >= 76 {
		out += "   " + pageSubStyle.Render(sanitizeText(subtitle))
	}
	return out + "\n"
}

// footer renders the rule plus key hints for the active screen.
func (m Model) footer(keys string) string {
	return "\n" + ruleStyle.Render(strings.Repeat("─", m.innerWidth())) + "\n" + helpStyle.Render(keys)
}

// ─── View (main router) ─────────────────────────────────────────────────────

func (m Model) View() string {
	// A 40-column frame silently truncates every table: refuse to pretend.
	if m.Width > 0 && (m.Width < 58 || m.Height < 16) {
		message := pageTitleStyle.Render("Terminal too small") + "\n\n" +
			pageSubStyle.Render(fmt.Sprintf("current %dx%d · need at least 58x16", m.Width, m.Height))
		return lipgloss.Place(m.Width, m.Height, lipgloss.Center, lipgloss.Center, message)
	}

	var content string
	switch m.Screen {
	case ScreenDashboard:
		content = m.viewDashboard()
	case ScreenSearch:
		content = m.viewSearch()
	case ScreenSearchResults:
		content = m.viewSearchResults()
	case ScreenRecent:
		content = m.viewRecent()
	case ScreenObservationDetail:
		content = m.viewObservationDetail()
	case ScreenTimeline:
		content = m.viewTimeline()
	case ScreenSessions:
		content = m.viewSessions()
	case ScreenSessionDetail:
		content = m.viewSessionDetail()
	case ScreenCandidates:
		content = m.viewCandidates()
	case ScreenSimilar:
		content = m.viewSimilar()
	case ScreenDoctor:
		content = m.viewDoctor()
	case ScreenAbout:
		content = m.viewAbout()
	default:
		content = "Unknown screen"
	}

	if m.ErrorMsg != "" {
		content += "\n" + errorStyle.Render("Error: "+sanitizeText(m.ErrorMsg))
	}
	if m.CopyFeedback != "" {
		content += "\n" + copyFeedbackStyle.Render(" "+sanitizeText(m.CopyFeedback)+" ")
	}

	return appStyle.Render(m.header() + "\n" + content)
}

// ─── Dashboard ───────────────────────────────────────────────────────────────

func (m Model) viewDashboard() string {
	var b strings.Builder

	b.WriteString(m.pageTitle("Overview", "本地知识库概览"))
	b.WriteString("\n")

	if m.UpdateMsg != "" {
		bannerStyle := updateBannerStyle
		if m.UpdateStatus == version.StatusCheckFailed {
			bannerStyle = errorStyle
		}
		b.WriteString(bannerStyle.Render(sanitizeText(m.UpdateMsg)))
		b.WriteString("\n\n")
	}

	if m.Stats != nil {
		b.WriteString(m.statsRow())
		b.WriteString("\n\n")
		b.WriteString(m.projectsLine())
		b.WriteString("\n")
	} else {
		b.WriteString(statCardStyle.Render(timestampStyle.Render("Loading stats…")))
		b.WriteString("\n\n")
	}

	b.WriteString(titleStyle.Render("Actions"))
	b.WriteString("\n")
	for i, item := range dashboardMenu {
		b.WriteString(m.menuRow(i == m.Cursor, item))
		b.WriteString("\n")
	}
	if m.Width < 84 {
		// The descriptions do not fit; keep the key hints meaningful.
		b.WriteString(helpStyle.Render("  enter open · j/k move"))
	}
	b.WriteString(m.footer("j/k move   enter open   s search   p project   r refresh   q quit"))
	return b.String()
}

// statsRow lays four cards across the full content width.
func (m Model) statsRow() string {
	values := []struct {
		label string
		value int
	}{
		{"sessions", m.Stats.TotalSessions},
		{"observations", m.Stats.TotalObservations},
		{"prompts", m.Stats.TotalPrompts},
		{"projects", len(m.Stats.Projects)},
	}
	const gap = 2
	// Every card except the last carries the gap as a right margin, so the row
	// fills the content width exactly.
	cardWidth := (m.innerWidth() - gap*(len(values)-1)) / len(values)
	if cardWidth < 14 {
		cardWidth = 14
	}
	cards := make([]string, 0, len(values))
	for i, item := range values {
		// Width() is the content box: the rounded border adds two columns and
		// the horizontal padding adds four.
		content := lipgloss.JoinVertical(lipgloss.Left,
			statNumberStyle.Render(fmt.Sprintf("%d", item.value)),
			statLabelStyle.Render(item.label),
		)
		card := statCardStyle
		if i < len(values)-1 {
			card = card.MarginRight(gap)
		}
		// Width() is the content width; the border adds two columns.
		cards = append(cards, card.Width(cardWidth-2).Render(content))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cards...)
}

// projectsLine shows the scopes the operator can rotate through.
func (m Model) projectsLine() string {
	if len(m.ProjectScopes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m.ProjectScopes))
	for _, scope := range m.ProjectScopes {
		label := shortProject(scope)
		if scope.ID == m.Project {
			parts = append(parts, scopeStyle.Render("● "+label))
			continue
		}
		parts = append(parts, projectStyle.Render("○ "+label))
	}
	return titleStyle.Render("Projects") + "\n  " + strings.Join(parts, "   ")
}

// menuRow renders one action as a full-width row so the selection is obvious.
func (m Model) menuRow(selected bool, item menuEntry) string {
	desc := item.desc
	marker := " "
	if selected {
		marker = "▸"
	}
	// A fixed label column keeps the short Chinese descriptions readable
	// instead of pushing them to the far edge of a wide terminal.
	label := fmt.Sprintf("%-24s", item.label)
	left := " " + marker + " " + item.icon + "  " + label
	row := left + "  " + desc
	if selected {
		// Pad the highlighted bar to the full width so the selection reads as
		// one row rather than a coloured fragment.
		if width := m.innerWidth() - lipgloss.Width(row); width > 0 {
			row += strings.Repeat(" ", width)
		}
		return menuSelectedStyle.Render(row)
	}
	// Color only the glyph: styling the whole row would hide the description.
	return menuItemStyle.Render(" "+marker+" "+menuIconStyle.Render(item.icon)+"  "+label+"  ") +
		menuDescStyle.Render(desc)
}

// ─── Search ──────────────────────────────────────────────────────────────────

func (m Model) viewSearch() string {
	var b strings.Builder
	b.WriteString(m.pageTitle("Search Memories", "在当前范围内全文检索"))
	b.WriteString("\n")
	b.WriteString(searchInputStyle.Render(m.SearchInput.View()))
	b.WriteString("\n\n")
	b.WriteString(pageSubStyle.Render("  Enter 搜索 · Esc 返回 · 结果里 c 复制正文、t 时间线"))
	b.WriteString(m.footer("/ or i focus   esc back"))
	return b.String()
}

// ─── Search Results ──────────────────────────────────────────────────────────

func (m Model) viewSearchResults() string {
	var b strings.Builder
	count := len(m.SearchResults)
	subtitle := fmt.Sprintf("%d results · %s", count, shortProject(currentScope(m)))
	b.WriteString(m.pageTitle(fmt.Sprintf("Search: %q", m.SearchQuery), subtitle))
	b.WriteString("\n")

	if count == 0 {
		b.WriteString(noResultsStyle.Render("No memories found. Try a different query."))
		b.WriteString(m.footer("/ new search   esc back"))
		return b.String()
	}

	visibleItems := m.observationWindow()
	end := m.Scroll + visibleItems
	if end > count {
		end = count
	}
	for i := m.Scroll; i < end; i++ {
		r := m.SearchResults[i]
		b.WriteString(m.renderObservationListItem(i, r.ID, r.Type, r.Title, r.Content, r.CreatedAt, r.Project, r.State(), r.ReviewAfter, r.Pinned))
	}
	if count > visibleItems {
		b.WriteString("\n  " + timestampStyle.Render(fmt.Sprintf("showing %d-%d of %d", m.Scroll+1, end, count)))
	}
	b.WriteString(m.footer("j/k navigate   enter detail   c copy   t timeline   / search   r rerun   esc back"))
	return b.String()
}

// ─── Recent Observations ─────────────────────────────────────────────────────

func (m Model) viewRecent() string {
	var b strings.Builder
	count := len(m.RecentObservations)
	b.WriteString(m.pageTitle("Recent Observations", fmt.Sprintf("%d total · %s", count, shortProject(currentScope(m)))))
	b.WriteString("\n")

	if count == 0 {
		b.WriteString(noResultsStyle.Render("No observations yet."))
		b.WriteString(m.footer("esc back"))
		return b.String()
	}

	visibleItems := m.observationWindow()
	end := m.Scroll + visibleItems
	if end > count {
		end = count
	}
	for i := m.Scroll; i < end; i++ {
		o := m.RecentObservations[i]
		b.WriteString(m.renderObservationListItem(i, o.ID, o.Type, o.Title, o.Content, o.CreatedAt, o.Project, o.State(), o.ReviewAfter, o.Pinned))
	}
	if count > visibleItems {
		b.WriteString("\n  " + timestampStyle.Render(fmt.Sprintf("showing %d-%d of %d", m.Scroll+1, end, count)))
	}
	b.WriteString(m.footer("j/k navigate   enter detail   c copy   t timeline   r refresh   esc back"))
	return b.String()
}

// ─── Observation Detail ──────────────────────────────────────────────────────

func (m Model) viewObservationDetail() string {
	var b strings.Builder
	if m.SelectedObservation == nil {
		b.WriteString(m.pageTitle("◆  Observation Detail", ""))
		b.WriteString(noResultsStyle.Render("Loading..."))
		return b.String()
	}
	obs := m.SelectedObservation
	b.WriteString(m.pageTitle(fmt.Sprintf("◆  Observation #%d", obs.ID), shortProject(currentScope(m))))
	b.WriteString("\n")

	type detailRow struct {
		label string
		value string
		style lipgloss.Style
	}
	stateStyle := detailValueStyle
	if obs.State() == "needs_review" {
		stateStyle = stateWarningBadgeStyle
	}
	rows := []detailRow{
		{"● Type:", obs.Type, typeBadgeStyle},
		{"◆ Title:", obs.Title, detailValueStyle.Bold(true)},
		{"■ Session:", obs.SessionID, idStyle},
		{"▲ Created:", localTime(obs.CreatedAt), timestampStyle},
		{"✓ State:", obs.State(), stateStyle},
		{"◆ Pinned:", fmt.Sprintf("%t", obs.Pinned), detailValueStyle},
	}
	if obs.ReviewAfter != nil {
		rows = append(rows, detailRow{"▲ Review:", formatReviewDate(*obs.ReviewAfter), timestampStyle})
	}
	if obs.ToolName != nil {
		rows = append(rows, detailRow{"✚ Tool:", *obs.ToolName, detailValueStyle})
	}
	if obs.Project != nil {
		rows = append(rows, detailRow{"■ Project:", *obs.Project, projectStyle})
	}
	for _, row := range rows {
		label := detailLabelStyle.Render(row.label)
		indent := strings.Repeat(" ", lipgloss.Width(label)+1)
		available := m.innerWidth() - lipgloss.Width(label) - 1
		for i, line := range wrapCells(sanitizeText(row.value), available) {
			if i == 0 {
				b.WriteString(label + " " + row.style.Render(line) + "\n")
				continue
			}
			b.WriteString(indent + row.style.Render(line) + "\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(sectionHeadingStyle.Render("◆  Content"))
	b.WriteString("\n")

	lines := m.detailContentLines()
	maxLines := m.detailVisibleLines()
	visible := m.DetailScroll + maxLines
	if visible > len(lines) {
		visible = len(lines)
	}
	for i := m.DetailScroll; i < visible; i++ {
		b.WriteString(lines[i])
		b.WriteString("\n")
	}
	if len(lines) > maxLines {
		b.WriteString("\n  " + timestampStyle.Render(fmt.Sprintf("line %d-%d of %d", m.DetailScroll+1, visible, len(lines))))
	}
	b.WriteString(m.footer("j/k scroll   PgUp/PgDn page   g/G top+end   c copy   t timeline   esc back"))
	return b.String()
}

// detailContentLines wraps the selected body to the terminal width.
func (m Model) detailContentLines() []string {
	if m.SelectedObservation == nil {
		return nil
	}
	wrapWidth := m.innerWidth() - 4
	if wrapWidth < 20 {
		wrapWidth = 20
	}
	wrapped := wrapCells(sanitizeText(m.SelectedObservation.Content), wrapWidth)
	lines := make([]string, 0, len(wrapped))
	for _, line := range wrapped {
		lines = append(lines, detailContentStyle.Render(line))
	}
	return lines
}

func (m Model) detailVisibleLines() int {
	maxLines := m.Height - 16
	if maxLines < 5 {
		maxLines = 5
	}
	return maxLines
}

func (m Model) detailMaxScroll() int {
	return maxInt(0, len(m.detailContentLines())-m.detailVisibleLines())
}

// ─── Timeline ────────────────────────────────────────────────────────────────

func (m Model) timelineLines() []string {
	if m.Timeline == nil {
		return nil
	}
	tl := m.Timeline
	var lines []string
	if tl.SessionInfo != nil {
		lines = append(lines, fmt.Sprintf("  %s %s   %s %s",
			detailLabelStyle.Render("Session:"), idStyle.Render(sanitizeText(tl.SessionInfo.ID)),
			detailLabelStyle.Render("Project:"), projectStyle.Render(sanitizeText(tl.SessionInfo.Project))))
		lines = append(lines, "")
	}
	if len(tl.Before) > 0 {
		lines = append(lines, sectionHeadingStyle.Render("◆  Before"))
		for _, e := range tl.Before {
			lines = append(lines, fmt.Sprintf("  %s %s %s  %s",
				timelineConnectorStyle.Render("│"), idStyle.Render(fmt.Sprintf("#%-4d", e.ID)),
				typeBadgeStyle.Render(fmt.Sprintf("[%-12s]", sanitizeText(e.Type))), timelineItemStyle.Render(truncateStr(e.Title, 60))))
		}
		lines = append(lines, "  "+timelineConnectorStyle.Render("│"))
	}
	focusContent := fmt.Sprintf("  %s %s  %s\n  %s",
		idStyle.Render(fmt.Sprintf("#%d", tl.Focus.ID)),
		typeBadgeStyle.Render("["+sanitizeText(tl.Focus.Type)+"]"),
		lipgloss.NewStyle().Bold(true).Foreground(colorLavender).Render(sanitizeText(tl.Focus.Title)),
		detailContentStyle.Render(truncateStr(tl.Focus.Content, 120)))
	lines = append(lines, strings.Split(timelineFocusStyle.Render(focusContent), "\n")...)
	if len(tl.After) > 0 {
		lines = append(lines, "  "+timelineConnectorStyle.Render("│"), sectionHeadingStyle.Render("◆  After"))
		for _, e := range tl.After {
			lines = append(lines, fmt.Sprintf("  %s %s %s  %s",
				timelineConnectorStyle.Render("│"), idStyle.Render(fmt.Sprintf("#%-4d", e.ID)),
				typeBadgeStyle.Render(fmt.Sprintf("[%-12s]", sanitizeText(e.Type))), timelineItemStyle.Render(truncateStr(e.Title, 60))))
		}
	}
	return lines
}

func (m Model) viewTimeline() string {
	var b strings.Builder
	if m.Timeline == nil {
		b.WriteString(m.pageTitle("◆  Timeline", ""))
		b.WriteString(noResultsStyle.Render("Loading..."))
		return b.String()
	}
	tl := m.Timeline
	b.WriteString(m.pageTitle(fmt.Sprintf("◆  Timeline — Observation #%d", tl.Focus.ID),
		fmt.Sprintf("%d total in session", tl.TotalInRange)))
	b.WriteString("\n")

	lines := m.timelineLines()
	visible := m.listWindow(8)
	end := m.Scroll + visible
	if end > len(lines) {
		end = len(lines)
	}
	for i := m.Scroll; i < end; i++ {
		b.WriteString(lines[i])
		b.WriteString("\n")
	}
	if len(lines) > visible {
		b.WriteString("\n  " + timestampStyle.Render(fmt.Sprintf("line %d-%d of %d", m.Scroll+1, end, len(lines))))
	}
	b.WriteString(m.footer("j/k scroll   PgUp/PgDn   g/G top+end   esc back"))
	return b.String()
}

// ─── Sessions ────────────────────────────────────────────────────────────────

func (m Model) viewSessions() string {
	var b strings.Builder
	count := len(m.Sessions)
	b.WriteString(m.pageTitle(fmt.Sprintf("Sessions — %d total", count), "按时间倒序"))
	b.WriteString("\n")

	if count == 0 {
		b.WriteString(noResultsStyle.Render("No sessions yet."))
		b.WriteString(m.footer("esc back"))
		return b.String()
	}

	visibleItems := m.Height - 10
	if visibleItems < 5 {
		visibleItems = 5
	}
	end := m.Scroll + visibleItems
	if end > count {
		end = count
	}
	for i := m.Scroll; i < end; i++ {
		s := m.Sessions[i]
		summary := ""
		if s.Summary != nil {
			summary = truncateStr(*s.Summary, 50)
		}
		plain := fmt.Sprintf("%-24s  %s  %3d obs  %s",
			shortProjectText(s.Project), shortTime(localTime(s.StartedAt)),
			s.ObservationCount, clipCells(summary, m.innerWidth()-70))
		if i == m.Cursor {
			b.WriteString(m.selectableRow(true, plain))
		} else {
			line := fmt.Sprintf("%-24s  %s  %3d obs  %s",
				projectStyle.Render(shortProjectText(s.Project)),
				timestampStyle.Render(shortTime(localTime(s.StartedAt))),
				s.ObservationCount, clipCells(summary, m.innerWidth()-70))
			b.WriteString(m.selectableRow(false, line))
		}
		b.WriteString("\n")
	}
	if count > visibleItems {
		b.WriteString("\n  " + timestampStyle.Render(fmt.Sprintf("showing %d-%d of %d", m.Scroll+1, end, count)))
	}
	b.WriteString(m.footer("j/k navigate   enter view session   r refresh   esc back"))
	return b.String()
}

// ─── Session Detail ──────────────────────────────────────────────────────────

func (m Model) viewSessionDetail() string {
	var b strings.Builder
	if m.SelectedSessionIdx >= len(m.Sessions) {
		b.WriteString(m.pageTitle("Session Detail", ""))
		b.WriteString(noResultsStyle.Render("Session not found."))
		return b.String()
	}
	sess := m.Sessions[m.SelectedSessionIdx]
	b.WriteString(m.pageTitle("Session", shortProjectText(sess.Project)+" · "+shortTime(localTime(sess.StartedAt))))
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("%s %s\n", detailLabelStyle.Render("■ Session:"), clipCells(sanitizeText(sess.ID), m.innerWidth()-18)))
	b.WriteString(fmt.Sprintf("%s %s\n", detailLabelStyle.Render("■ Project:"), projectStyle.Render(sanitizeText(sess.Project))))
	b.WriteString(fmt.Sprintf("%s %s\n", detailLabelStyle.Render("▲ Started:"), timestampStyle.Render(localTime(sess.StartedAt))))
	if sess.Summary != nil {
		b.WriteString("\n")
		for _, line := range wrapCells(sanitizeText(*sess.Summary), m.innerWidth()-4) {
			b.WriteString(pageSubStyle.Render("  " + line))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	count := len(m.SessionObservations)
	b.WriteString(sectionHeadingStyle.Render(fmt.Sprintf("Observations (%d)", count)))
	b.WriteString("\n")
	if count == 0 {
		b.WriteString(noResultsStyle.Render("No observations in this session."))
		b.WriteString(m.footer("esc back"))
		return b.String()
	}
	visibleItems := m.listWindow(12)
	end := m.SessionDetailScroll + visibleItems
	if end > count {
		end = count
	}
	for i := m.SessionDetailScroll; i < end; i++ {
		o := m.SessionObservations[i]
		b.WriteString(m.renderObservationListItem(i, o.ID, o.Type, o.Title, o.Content, o.CreatedAt, o.Project, o.State(), o.ReviewAfter, o.Pinned))
	}
	if count > visibleItems {
		b.WriteString("\n  " + timestampStyle.Render(fmt.Sprintf("showing %d-%d of %d", m.SessionDetailScroll+1, end, count)))
	}
	b.WriteString(m.footer("j/k navigate   enter detail   c copy   t timeline   esc back"))
	return b.String()
}

// ─── About ───────────────────────────────────────────────────────────────────

func (m Model) viewAbout() string {
	var b strings.Builder
	b.WriteString(m.pageTitle("About & shortcuts", "版本、存储位置与常用命令"))
	b.WriteString("\n")

	storePath, storeID := "", ""
	if m.store != nil {
		if identity, err := m.store.Identity(); err == nil {
			storePath = identity.DatabasePath
			storeID = identity.StoreID
		}
	}
	rows := [][2]string{
		{"Version", m.Version},
		{"Scope", shortProject(currentScope(m))},
		{"Database", storePath},
		{"Store ID", storeID},
	}
	for _, row := range rows {
		b.WriteString(fmt.Sprintf("  %s  %s\n", detailLabelStyle.Width(10).Render(row[0]), detailValueStyle.Render(sanitizeText(row[1]))))
	}

	b.WriteString("\n")
	b.WriteString(sectionHeadingStyle.Render("常用命令"))
	b.WriteString("\n")
	commands := [][2]string{
		{"recalldock ui", "本机网页操作界面（127.0.0.1:7438）"},
		{"recalldock backup --json", "在线备份知识库"},
		{"recalldock doctor", "十项只读诊断"},
		{"recalldock projects bind <id> --directory <path>", "绑定目录（写操作，仅 CLI）"},
		{"recalldock candidates list", "查看候选收件箱"},
		{"recalldock memory similar", "扫相似组（只读）"},
	}
	for _, cmd := range commands {
		b.WriteString(fmt.Sprintf("  %s  %s\n", idStyle.Render(cmd[0]), pageSubStyle.Render(cmd[1])))
	}

	b.WriteString("\n")
	b.WriteString(sectionHeadingStyle.Render("快捷键"))
	b.WriteString("\n")
	keys := [][2]string{
		{"j / k", "上下移动"},
		{"enter", "打开选中项"},
		{"s", "搜索"},
		{"p", "切换项目范围"},
		{"r", "刷新当前列表"},
		{"[ / ]", "相似扫描放宽 / 收紧"},
		{"esc / q", "返回"},
	}
	for _, key := range keys {
		b.WriteString(fmt.Sprintf("  %s  %s\n", typeBadgeStyle.Render(fmt.Sprintf("%-9s", key[0])), pageSubStyle.Render(key[1])))
	}
	b.WriteString(m.footer("esc back to overview"))
	return b.String()
}

// ─── Shared renderers ────────────────────────────────────────────────────────

// selectableRow paints the whole row, so a selected line is visible even on a
// dark terminal where a subtle background disappears.
func (m Model) selectableRow(selected bool, line string) string {
	if selected {
		return listSelectedStyle.Width(m.innerWidth() - 1).Render(clipCells("▸ "+line, m.innerWidth()-1))
	}
	return listItemStyle.Render("  " + line)
}

func (m Model) renderObservationListItem(index int, id int64, obsType, title, content, createdAt string, project *string, state string, reviewAfter *string, pinned bool) string {
	width := m.innerWidth()
	obsType, title, content = sanitizeText(obsType), sanitizeText(title), sanitizeText(content)
	projectText := ""
	if project != nil && *project != "" {
		projectText = shortID(*project)
	}
	statePlain := ""
	if state == "needs_review" {
		statePlain = " [needs_review]"
	}
	pinPlain := ""
	if pinned {
		pinPlain = " [pinned]"
	}
	metaPlain := fmt.Sprintf("#%-5d [%-12s]%s%s %s", id, obsType, statePlain, pinPlain, shortTime(localTime(createdAt)))
	if projectText != "" {
		metaPlain += "  ·  " + projectText
	}
	metaPlain = clipCells(metaPlain, width-2)
	titleLine := clipCells(title, width-2)
	preview := clipCells(strings.ReplaceAll(content, "\n", " "), width-2)

	if index == m.Cursor {
		// Nested colour resets would cut the selection bar short, so the
		// selected row is rendered as one plain, pre-padded block: applying
		// the background once keeps every line of the bar the same colour.
		lines := []string{
			padCells(clipCells("▸ "+metaPlain, width-1), width-1),
			padCells(clipCells("  "+titleLine, width-1), width-1),
		}
		if preview != "" {
			lines = append(lines, padCells(clipCells("  "+preview, width-1), width-1))
		}
		return listSelectedStyle.Render(strings.Join(lines, "\n")) + "\n"
	}

	meta := idStyle.Render(fmt.Sprintf("#%-5d", id)) +
		" " + typeBadgeStyle.Render(fmt.Sprintf("[%-12s]", obsType))
	if statePlain != "" {
		meta += stateWarningBadgeStyle.Render(statePlain)
	}
	if pinPlain != "" {
		meta += detailValueStyle.Render(pinPlain)
	}
	meta += " " + timestampStyle.Render(shortTime(localTime(createdAt)))
	if projectText != "" {
		meta += "  ·  " + projectStyle.Render(projectText)
	}
	body := listItemStyle.Render("  ") + meta + "\n" +
		"  " + detailValueStyle.Bold(true).Render(titleLine) + "\n"
	if preview != "" {
		body += contentPreviewStyle.Render("  "+preview) + "\n"
	}
	return body
}

// shortProjectText keeps both ends of a long identity so two project IDs stay
// distinguishable in a narrow column.
func shortProjectText(value string) string {
	value = sanitizeText(value)
	runes := []rune(value)
	if len(runes) <= 18 {
		return value
	}
	return string(runes[:8]) + "…" + string(runes[len(runes)-6:])
}

// shortTime trims a local timestamp to the minute for list rows.
func shortTime(value string) string {
	if len(value) >= 16 {
		return value[:16]
	}
	return value
}

// shortID keeps identity columns readable without hiding the project family.
func shortID(value string) string {
	value = sanitizeText(value)
	runes := []rune(value)
	if len(runes) <= 15 {
		return value
	}
	return string(runes[:12]) + "…"
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// localTime converts a UTC timestamp string from SQLite to local time for display.
func localTime(utc string) string {
	return sanitizeText(timeutil.FormatLocal(utc))
}

func renderObservationState(state string) string {
	if state == "needs_review" {
		return stateWarningBadgeStyle.Render(state)
	}
	return detailValueStyle.Render(sanitizeText(state))
}

func formatReviewDate(value string) string {
	trimmed := strings.TrimSpace(sanitizeText(value))
	formats := []string{"2006-01-02 15:04:05", time.RFC3339, time.RFC3339Nano, "2006-01-02"}
	for _, layout := range formats {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.Format("2006-01-02")
		}
	}
	if len(trimmed) >= len("2006-01-02") {
		return trimmed[:len("2006-01-02")]
	}
	return trimmed
}

// sanitizeText strips terminal control characters from stored knowledge before
// it is rendered. Escape sequences in memory text would otherwise reach the
// terminal raw (colour changes, screen clears, OSC 52 clipboard writes).
func sanitizeText(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// clipCells truncates by terminal cells, not runes: CJK glyphs are two cells
// wide, so a rune-count limit overflows the layout.
func clipCells(value string, max int) string {
	if max <= 1 {
		return ""
	}
	if lipgloss.Width(value) <= max {
		return value
	}
	var b strings.Builder
	width := 0
	for _, r := range value {
		w := runewidth.RuneWidth(r)
		if width+w > max-1 {
			break
		}
		b.WriteRune(r)
		width += w
	}
	return b.String() + "…"
}

// padCells pads a single line with spaces to exactly width cells.
func padCells(value string, width int) string {
	padding := width - lipgloss.Width(value)
	if padding <= 0 {
		return value
	}
	return value + strings.Repeat(" ", padding)
}

// wrapCells wraps text to a cell budget. CJK text has no spaces to break on,
// so it falls back to a hard break; Latin text prefers the last space.
func wrapCells(value string, width int) []string {
	if width < 8 {
		width = 8
	}
	var out []string
	for _, paragraph := range strings.Split(value, "\n") {
		runes := []rune(paragraph)
		if len(runes) == 0 {
			out = append(out, "")
			continue
		}
		line := make([]rune, 0, len(runes))
		lineWidth := 0
		lastSpace := -1
		flush := func() {
			out = append(out, string(line))
			line = line[:0]
			lineWidth = 0
			lastSpace = -1
		}
		for _, r := range runes {
			w := runewidth.RuneWidth(r)
			if lineWidth+w > width && len(line) > 0 {
				if lastSpace > 0 {
					tail := append([]rune{}, line[lastSpace+1:]...)
					out = append(out, string(line[:lastSpace]))
					line = tail
					lineWidth = 0
					for _, t := range tail {
						lineWidth += runewidth.RuneWidth(t)
					}
					lastSpace = -1
				} else {
					flush()
				}
			}
			line = append(line, r)
			lineWidth += w
			if r == ' ' {
				lastSpace = len(line) - 1
			}
		}
		if len(line) > 0 {
			out = append(out, string(line))
		}
	}
	return out
}

func truncateStr(s string, max int) string {
	// Remove control sequences and newlines for single-line display.
	s = strings.ReplaceAll(sanitizeText(s), "\n", " ")
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
