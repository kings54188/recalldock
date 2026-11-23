package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"recalldock.local/recalldock/internal/store"
)

// ─── Similar memories ────────────────────────────────────────────────────────

type similarLoadedMsg struct {
	groups    []store.SimilarMemoryGroup
	project   string
	threshold float64
	err       error
}

// loadSimilar scans one explicit project. "All projects" is refused instead of
// silently scanning nothing: a similarity hint must name its scope.
func loadSimilar(s *store.Store, project string, threshold float64) tea.Cmd {
	return func() tea.Msg {
		if strings.TrimSpace(project) == "" {
			return similarLoadedMsg{threshold: threshold}
		}
		groups, err := s.SimilarMemoryGroups(project, threshold, 20)
		return similarLoadedMsg{groups: groups, project: project, threshold: threshold, err: err}
	}
}

func (m Model) similarGroupWindow() int {
	limit := m.Height - 14
	if limit < 3 {
		limit = 3
	}
	return limit
}

func (m Model) handleSimilarKeys(key string) (tea.Model, tea.Cmd) {
	visible := m.similarGroupWindow()
	switch key {
	case "up", "k":
		moveCursor(&m.SimilarCursor, &m.SimilarScroll, -1, len(m.SimilarGroups), visible)
	case "down", "j":
		moveCursor(&m.SimilarCursor, &m.SimilarScroll, 1, len(m.SimilarGroups), visible)
	case "pgup":
		moveCursor(&m.SimilarCursor, &m.SimilarScroll, -visible, len(m.SimilarGroups), visible)
	case "pgdown":
		moveCursor(&m.SimilarCursor, &m.SimilarScroll, visible, len(m.SimilarGroups), visible)
	case "home", "g":
		m.SimilarCursor, m.SimilarScroll = 0, 0
	case "end", "G":
		if len(m.SimilarGroups) > 0 {
			m.SimilarCursor = len(m.SimilarGroups) - 1
			m.SimilarScroll = maxInt(0, m.SimilarCursor-visible+1)
		}
	case "enter":
		if m.Project != "" && len(m.SimilarGroups) > 0 && m.SimilarCursor < len(m.SimilarGroups) {
			target := m.SimilarGroups[m.SimilarCursor].SuggestedTarget
			m.PrevScreen = ScreenSimilar
			return m, loadObservationDetail(m.store, target)
		}
	case "[":
		if m.SimilarThreshold > 0.4 {
			m.SimilarThreshold -= 0.05
			m.SimilarCursor, m.SimilarScroll = 0, 0
			return m, loadSimilar(m.store, m.Project, m.SimilarThreshold)
		}
	case "]":
		if m.SimilarThreshold < 0.9 {
			m.SimilarThreshold += 0.05
			m.SimilarCursor, m.SimilarScroll = 0, 0
			return m, loadSimilar(m.store, m.Project, m.SimilarThreshold)
		}
	case "r":
		return m, loadSimilar(m.store, m.Project, m.SimilarThreshold)
	case "esc", "q":
		m.Screen = ScreenDashboard
		m.Cursor = 0
		m.SimilarCursor = 0
		m.SimilarScroll = 0
		return m, loadStats(m.store, m.Project)
	}
	return m, nil
}

func (m Model) viewSimilar() string {
	var b strings.Builder
	b.WriteString(m.pageTitle("⧉  Similar memories",
		fmt.Sprintf("阈值 %.2f · %s", m.SimilarThreshold, shortProject(currentScope(m)))))
	b.WriteString("\n")

	if m.Project == "" {
		b.WriteString(noResultsStyle.Render("先选择一个具体项目。"))
		b.WriteString("\n")
		b.WriteString(pageSubStyle.Render("   相似扫描需要明确的记忆范围；按 p 切换到某个项目后会自动重扫。"))
		b.WriteString(m.footer("p project   esc back"))
		return b.String()
	}
	if len(m.SimilarGroups) == 0 {
		b.WriteString(noResultsStyle.Render(fmt.Sprintf("阈值 %.2f 下没有发现相似组。", m.SimilarThreshold)))
		b.WriteString("\n")
		b.WriteString(pageSubStyle.Render("   按 [ 放宽阈值，或 ] 收紧；扫描只读，不会合并任何内容。"))
		b.WriteString(m.footer("[ looser   ] stricter   r rescan   p project   esc back"))
		return b.String()
	}

	width := m.innerWidth()
	limit := m.similarGroupWindow()
	start := 0
	if m.SimilarScroll >= limit {
		start = m.SimilarScroll - limit + 1
	}
	end := start + limit
	if end > len(m.SimilarGroups) {
		end = len(m.SimilarGroups)
	}
	for i := start; i < end; i++ {
		group := m.SimilarGroups[i]
		targetTitle := ""
		for _, observation := range group.Observations {
			if observation.ID == group.SuggestedTarget {
				targetTitle = observation.Title
				break
			}
		}
		line := fmt.Sprintf("%.2f  %d 条  #%d  %s", group.Score, len(group.Observations), group.SuggestedTarget,
			clipCells(sanitizeText(targetTitle), width-30))
		if i == m.SimilarCursor {
			b.WriteString(listSelectedStyle.Width(width - 1).Render(clipCells("▸ "+line, width-1)))
		} else {
			b.WriteString(listItemStyle.Render("  " + line))
		}
		b.WriteString("\n")
	}
	if len(m.SimilarGroups) > limit {
		b.WriteString("  " + timestampStyle.Render(fmt.Sprintf("showing %d-%d of %d", start+1, end, len(m.SimilarGroups))))
		b.WriteString("\n")
	}

	if m.SimilarCursor < len(m.SimilarGroups) {
		group := m.SimilarGroups[m.SimilarCursor]
		b.WriteString("\n")
		b.WriteString(sectionHeadingStyle.Render(fmt.Sprintf("◆  相似度 %.2f · %d 条", group.Score, len(group.Observations))))
		b.WriteString("\n")
		members := group.Observations
		if len(members) > 8 {
			members = members[:8]
		}
		for _, observation := range members {
			marker := "  "
			if observation.ID == group.SuggestedTarget {
				marker = "→ "
			}
			line := fmt.Sprintf("%s#%-5d %s", marker, observation.ID,
				clipCells(sanitizeText(observation.Title), width-14))
			if observation.ID == group.SuggestedTarget {
				b.WriteString(listSelectedStyle.Width(width - 1).Render(clipCells(line+"  建议目标", width-1)))
			} else {
				b.WriteString(pageSubStyle.Render("  " + line))
			}
			b.WriteString("\n")
		}
		if len(group.Observations) > len(members) {
			b.WriteString("  " + timestampStyle.Render(fmt.Sprintf("还有 %d 条未显示", len(group.Observations)-len(members))))
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("  网页里可生成精炼草稿：recalldock ui → 相似"))
		b.WriteString("\n")
	}
	b.WriteString(m.footer("j/k move   enter open target   [ ] threshold   r rescan   p project   esc back"))
	return b.String()
}
