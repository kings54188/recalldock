package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ─── Update ──────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		return m, nil

	case tea.KeyMsg:
		// Global quit — always works
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		// If search input is focused, let it handle most keys
		if m.Screen == ScreenSearch && m.SearchInput.Focused() {
			return m.handleSearchInputKeys(msg)
		}
		if m.Screen == ScreenCandidates && m.CandidateReason.Focused() {
			return m.handleCandidateReasonKeys(msg)
		}
		// Project scope cycling is a navigation action, not a knowledge write:
		// it stays available on every screen (typing surfaces are handled above).
		if msg.String() == "p" && m.Screen != ScreenAbout {
			return m.cycleProject()
		}
		return m.handleKeyPress(msg.String())

	// ─── Data loaded messages ────────────────────────────────────────────
	case updateCheckMsg:
		m.UpdateStatus = msg.result.Status
		m.UpdateMsg = msg.result.Message
		return m, nil

	case statsLoadedMsg:
		if msg.err != nil {
			m.ErrorMsg = msg.err.Error()
			return m, nil
		}
		m.Stats = msg.stats
		return m, nil

	case searchResultsMsg:
		if msg.err != nil {
			m.ErrorMsg = msg.err.Error()
			return m, nil
		}
		m.SearchResults = msg.results
		m.SearchQuery = msg.query
		m.Screen = ScreenSearchResults
		m.Cursor = 0
		m.Scroll = 0
		return m, nil

	case candidatesLoadedMsg:
		if msg.err != nil {
			m.ErrorMsg = msg.err.Error()
			return m, nil
		}
		m.Candidates = msg.candidates
		m.CandidateCursor = clampIndex(m.CandidateCursor, len(m.Candidates))
		return m, nil

	case candidateReviewedMsg:
		if msg.err != nil {
			m.CandidateFeedback = "操作失败：" + msg.err.Error()
			return m, loadCandidates(m.store, m.Project)
		}
		if msg.approve {
			m.CandidateFeedback = "已批准 " + msg.id
		} else {
			m.CandidateFeedback = "已拒绝 " + msg.id
		}
		return m, tea.Batch(loadCandidates(m.store, m.Project), loadStats(m.store, m.Project))

	case similarLoadedMsg:
		if msg.err != nil {
			m.ErrorMsg = msg.err.Error()
			return m, nil
		}
		m.SimilarGroups = msg.groups
		m.SimilarProject = msg.project
		m.SimilarThreshold = msg.threshold
		moveCursor(&m.SimilarCursor, &m.SimilarScroll, 0, len(m.SimilarGroups), m.similarGroupWindow())
		return m, nil

	case doctorLoadedMsg:
		if msg.err != nil {
			m.ErrorMsg = msg.err.Error()
			return m, nil
		}
		report := msg.report
		m.DoctorReport = &report
		m.DoctorScroll = clampIndex(m.DoctorScroll, len(m.doctorVisibleChecks()))
		return m, nil

	case recentObservationsMsg:
		if msg.err != nil {
			m.ErrorMsg = msg.err.Error()
			return m, nil
		}
		m.RecentObservations = msg.observations
		if m.Screen == ScreenRecent {
			moveCursor(&m.Cursor, &m.Scroll, 0, len(m.RecentObservations), m.observationWindow())
		}
		return m, nil

	case observationDetailMsg:
		if msg.err != nil {
			m.ErrorMsg = msg.err.Error()
			return m, nil
		}
		m.SelectedObservation = msg.observation
		m.Screen = ScreenObservationDetail
		m.DetailScroll = 0
		return m, nil

	case timelineMsg:
		if msg.err != nil {
			m.ErrorMsg = msg.err.Error()
			return m, nil
		}
		m.Timeline = msg.timeline
		m.Screen = ScreenTimeline
		m.Scroll = 0
		return m, nil

	case recentSessionsMsg:
		if msg.err != nil {
			m.ErrorMsg = msg.err.Error()
			return m, nil
		}
		m.Sessions = msg.sessions
		m.SelectedSessionIdx = clampIndex(m.SelectedSessionIdx, len(m.Sessions))
		if m.Screen == ScreenSessions {
			moveCursor(&m.Cursor, &m.Scroll, 0, len(m.Sessions), maxInt(5, m.Height-10))
		}
		return m, nil

	case sessionObservationsMsg:
		if msg.err != nil {
			m.ErrorMsg = msg.err.Error()
			return m, nil
		}
		m.SessionObservations = msg.observations
		m.Screen = ScreenSessionDetail
		m.Cursor = 0
		m.SessionDetailScroll = 0
		return m, nil

	case clipboardCopiedMsg:
		// Emit the OSC 52 sequence to stdout so the terminal copies the content,
		// set the feedback label, and schedule its removal after 2 seconds.
		m.CopyFeedback = "✓ Copied!"
		return m, tea.Batch(
			tea.Println(msg.sequence),
			clearFeedbackAfter(2*time.Second),
		)

	case clipboardClearMsg:
		m.CopyFeedback = ""
		return m, nil
	}

	return m, nil
}

// ─── Key Press Router ────────────────────────────────────────────────────────

func (m Model) handleKeyPress(key string) (tea.Model, tea.Cmd) {
	// Clear error on any keypress
	m.ErrorMsg = ""

	switch m.Screen {
	case ScreenDashboard:
		return m.handleDashboardKeys(key)
	case ScreenSearch:
		return m.handleSearchKeys(key)
	case ScreenSearchResults:
		return m.handleSearchResultsKeys(key)
	case ScreenRecent:
		return m.handleRecentKeys(key)
	case ScreenObservationDetail:
		return m.handleObservationDetailKeys(key)
	case ScreenTimeline:
		return m.handleTimelineKeys(key)
	case ScreenSessions:
		return m.handleSessionsKeys(key)
	case ScreenSessionDetail:
		return m.handleSessionDetailKeys(key)
	case ScreenCandidates:
		return m.handleCandidatesKeys(key)
	case ScreenSimilar:
		return m.handleSimilarKeys(key)
	case ScreenDoctor:
		return m.handleDoctorKeys(key)
	case ScreenAbout:
		return m.handleAboutKeys(key)
	}
	return m, nil
}

// ─── Dashboard ───────────────────────────────────────────────────────────────

// Dashboard entries are addressed by constant so tests and key handling never
// depend on a menu position that shifts when items are added.
const (
	menuSearch = iota
	menuRecent
	menuSessions
	menuSimilar
	menuCandidates
	menuDoctor
	menuAbout
	menuQuit
)

type menuEntry struct {
	icon  string
	label string
	desc  string
}

// Menu markers use geometric glyphs that render in minimal terminal fonts;
// colorful emoji are not guaranteed (they show as tofu in many Linux ttys).
var dashboardMenu = []menuEntry{
	{"●", "Search memories", "全文检索"},
	{"◆", "Recent observations", "最近写入"},
	{"■", "Browse sessions", "按会话查看"},
	{"⧉", "Similar memories", "找重复与可合并"},
	{"✓", "Review candidates", "审核 AI 候选"},
	{"✚", "Run doctor", "只读诊断"},
	{"◈", "About & shortcuts", "版本与常用命令"},
	{"×", "Quit", ""},
}

func (m Model) handleDashboardKeys(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k":
		if m.Cursor > 0 {
			m.Cursor--
		}
	case "down", "j":
		if m.Cursor < len(dashboardMenu)-1 {
			m.Cursor++
		}
	case "home", "g":
		m.Cursor = 0
	case "end", "G":
		m.Cursor = len(dashboardMenu) - 1
	case "enter", " ":
		return m.handleDashboardSelection()
	case "s", "/":
		m.PrevScreen = ScreenDashboard
		m.Screen = ScreenSearch
		m.Cursor = 0
		m.SearchInput.SetValue("")
		m.SearchInput.Focus()
		return m, nil
	case "p":
		return m.cycleProject()
	case "r":
		return m, loadStats(m.store, m.Project)
	case "q":
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) handleDashboardSelection() (tea.Model, tea.Cmd) {
	switch m.Cursor {
	case menuSearch:
		m.PrevScreen = ScreenDashboard
		m.Screen = ScreenSearch
		m.Cursor = 0
		m.SearchInput.SetValue("")
		m.SearchInput.Focus()
		return m, nil
	case menuRecent:
		m.PrevScreen = ScreenDashboard
		m.Screen = ScreenRecent
		m.Cursor = 0
		m.Scroll = 0
		return m, loadRecentObservations(m.store, m.Project)
	case menuSessions:
		m.PrevScreen = ScreenDashboard
		m.Screen = ScreenSessions
		m.Cursor = 0
		m.Scroll = 0
		return m, loadRecentSessions(m.store)
	case menuSimilar:
		m.PrevScreen = ScreenDashboard
		m.Screen = ScreenSimilar
		m.SimilarCursor = 0
		m.SimilarScroll = 0
		return m, loadSimilar(m.store, m.Project, m.SimilarThreshold)
	case menuCandidates:
		m.PrevScreen = ScreenDashboard
		m.Screen = ScreenCandidates
		m.CandidateCursor = 0
		m.CandidateFeedback = ""
		return m, loadCandidates(m.store, m.Project)
	case menuDoctor:
		m.PrevScreen = ScreenDashboard
		m.Screen = ScreenDoctor
		m.DoctorScroll = 0
		return m, runDoctor(m.store, m.Project)
	case menuAbout:
		m.PrevScreen = ScreenDashboard
		m.Screen = ScreenAbout
		return m, nil
	case menuQuit:
		return m, tea.Quit
	}
	return m, nil
}

// ─── Search Input ────────────────────────────────────────────────────────────

func (m Model) handleSearchInputKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		query := m.SearchInput.Value()
		if query != "" {
			m.SearchInput.Blur()
			return m, searchMemories(m.store, query, m.Project)
		}
		return m, nil
	case "esc":
		m.SearchInput.Blur()
		m.Screen = ScreenDashboard
		m.Cursor = 0
		return m, nil
	}

	// Let the text input component handle everything else
	var cmd tea.Cmd
	m.SearchInput, cmd = m.SearchInput.Update(msg)
	return m, cmd
}

func (m Model) handleSearchKeys(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "q":
		m.Screen = ScreenDashboard
		m.Cursor = 0
		return m, nil
	case "i", "/":
		m.SearchInput.Focus()
		return m, nil
	}
	return m, nil
}

// ─── Search Results ──────────────────────────────────────────────────────────

func (m Model) handleSearchResultsKeys(key string) (tea.Model, tea.Cmd) {
	visibleItems := m.observationWindow()
	switch key {
	case "up", "k":
		moveCursor(&m.Cursor, &m.Scroll, -1, len(m.SearchResults), visibleItems)
	case "down", "j":
		moveCursor(&m.Cursor, &m.Scroll, 1, len(m.SearchResults), visibleItems)
	case "pgup":
		moveCursor(&m.Cursor, &m.Scroll, -visibleItems, len(m.SearchResults), visibleItems)
	case "pgdown":
		moveCursor(&m.Cursor, &m.Scroll, visibleItems, len(m.SearchResults), visibleItems)
	case "home", "g":
		m.Cursor, m.Scroll = 0, 0
	case "end", "G":
		if len(m.SearchResults) > 0 {
			m.Cursor = len(m.SearchResults) - 1
			m.Scroll = maxInt(0, m.Cursor-visibleItems+1)
		}
	case "enter":
		if len(m.SearchResults) > 0 && m.Cursor < len(m.SearchResults) {
			obsID := m.SearchResults[m.Cursor].ID
			m.PrevScreen = ScreenSearchResults
			return m, loadObservationDetail(m.store, obsID)
		}
	case "c":
		if len(m.SearchResults) > 0 && m.Cursor < len(m.SearchResults) {
			return m, copyToClipboard(m.SearchResults[m.Cursor].Content)
		}
	case "t":
		// Timeline for selected result
		if len(m.SearchResults) > 0 && m.Cursor < len(m.SearchResults) {
			obsID := m.SearchResults[m.Cursor].ID
			m.PrevScreen = ScreenSearchResults
			return m, loadTimeline(m.store, obsID)
		}
	case "r":
		if m.SearchQuery != "" {
			return m, searchMemories(m.store, m.SearchQuery, m.Project)
		}
	case "/", "s":
		m.PrevScreen = ScreenSearchResults
		m.Screen = ScreenSearch
		m.SearchInput.Focus()
		return m, nil
	case "esc", "q":
		m.PrevScreen = ScreenDashboard
		m.Screen = ScreenSearch
		m.Cursor = 0
		m.Scroll = 0
		m.SearchInput.Focus()
		return m, nil
	}
	return m, nil
}

// ─── Recent Observations ─────────────────────────────────────────────────────

func (m Model) handleRecentKeys(key string) (tea.Model, tea.Cmd) {
	visibleItems := m.observationWindow()
	switch key {
	case "up", "k":
		moveCursor(&m.Cursor, &m.Scroll, -1, len(m.RecentObservations), visibleItems)
	case "down", "j":
		moveCursor(&m.Cursor, &m.Scroll, 1, len(m.RecentObservations), visibleItems)
	case "pgup":
		moveCursor(&m.Cursor, &m.Scroll, -visibleItems, len(m.RecentObservations), visibleItems)
	case "pgdown":
		moveCursor(&m.Cursor, &m.Scroll, visibleItems, len(m.RecentObservations), visibleItems)
	case "home", "g":
		m.Cursor, m.Scroll = 0, 0
	case "end", "G":
		if len(m.RecentObservations) > 0 {
			m.Cursor = len(m.RecentObservations) - 1
			m.Scroll = maxInt(0, m.Cursor-visibleItems+1)
		}
	case "enter":
		if len(m.RecentObservations) > 0 && m.Cursor < len(m.RecentObservations) {
			obsID := m.RecentObservations[m.Cursor].ID
			m.PrevScreen = ScreenRecent
			return m, loadObservationDetail(m.store, obsID)
		}
	case "c":
		if len(m.RecentObservations) > 0 && m.Cursor < len(m.RecentObservations) {
			return m, copyToClipboard(m.RecentObservations[m.Cursor].Content)
		}
	case "t":
		if len(m.RecentObservations) > 0 && m.Cursor < len(m.RecentObservations) {
			obsID := m.RecentObservations[m.Cursor].ID
			m.PrevScreen = ScreenRecent
			return m, loadTimeline(m.store, obsID)
		}
	case "r":
		return m, loadRecentObservations(m.store, m.Project)
	case "esc", "q":
		m.Screen = ScreenDashboard
		m.Cursor = 0
		m.Scroll = 0
		return m, loadStats(m.store, m.Project)
	}
	return m, nil
}

// ─── Observation Detail ──────────────────────────────────────────────────────

func (m Model) handleObservationDetailKeys(key string) (tea.Model, tea.Cmd) {
	maxScroll := m.detailMaxScroll()
	page := m.detailVisibleLines()
	switch key {
	case "up", "k":
		if m.DetailScroll > 0 {
			m.DetailScroll--
		}
	case "down", "j":
		// Clamp here: the view also clamps, but an unbounded counter would make
		// scrolling back up take dozens of invisible keypresses.
		if m.DetailScroll < maxScroll {
			m.DetailScroll++
		}
	case "pgup":
		m.DetailScroll -= page
		if m.DetailScroll < 0 {
			m.DetailScroll = 0
		}
	case "pgdown":
		m.DetailScroll += page
		if m.DetailScroll > maxScroll {
			m.DetailScroll = maxScroll
		}
	case "home", "g":
		m.DetailScroll = 0
	case "end", "G":
		m.DetailScroll = maxScroll
	case "c":
		if m.SelectedObservation != nil {
			return m, copyToClipboard(m.SelectedObservation.Content)
		}
	case "t":
		// View timeline for this observation; esc must come back here, not to
		// whichever screen opened the detail.
		if m.SelectedObservation != nil {
			m.PrevScreen = ScreenObservationDetail
			return m, loadTimeline(m.store, m.SelectedObservation.ID)
		}
	case "esc", "q":
		m.Screen = m.PrevScreen
		m.Cursor = 0
		m.DetailScroll = 0
		return m, m.refreshScreen(m.PrevScreen)
	}
	return m, nil
}

// ─── Timeline ────────────────────────────────────────────────────────────────

func (m Model) handleTimelineKeys(key string) (tea.Model, tea.Cmd) {
	maxScroll := maxInt(0, len(m.timelineLines())-m.listWindow(8))
	page := m.listWindow(8)
	switch key {
	case "up", "k":
		if m.Scroll > 0 {
			m.Scroll--
		}
	case "down", "j":
		if m.Scroll < maxScroll {
			m.Scroll++
		}
	case "pgup":
		m.Scroll -= page
		if m.Scroll < 0 {
			m.Scroll = 0
		}
	case "pgdown":
		m.Scroll += page
		if m.Scroll > maxScroll {
			m.Scroll = maxScroll
		}
	case "home", "g":
		m.Scroll = 0
	case "end", "G":
		m.Scroll = maxScroll
	case "esc", "q":
		m.Screen = m.PrevScreen
		m.Cursor = 0
		m.Scroll = 0
		return m, m.refreshScreen(m.PrevScreen)
	}
	return m, nil
}

// ─── Sessions ────────────────────────────────────────────────────────────────

func (m Model) handleSessionsKeys(key string) (tea.Model, tea.Cmd) {
	visibleItems := m.Height - 10
	if visibleItems < 5 {
		visibleItems = 5
	}
	switch key {
	case "up", "k":
		moveCursor(&m.Cursor, &m.Scroll, -1, len(m.Sessions), visibleItems)
	case "down", "j":
		moveCursor(&m.Cursor, &m.Scroll, 1, len(m.Sessions), visibleItems)
	case "pgup":
		moveCursor(&m.Cursor, &m.Scroll, -visibleItems, len(m.Sessions), visibleItems)
	case "pgdown":
		moveCursor(&m.Cursor, &m.Scroll, visibleItems, len(m.Sessions), visibleItems)
	case "home", "g":
		m.Cursor, m.Scroll = 0, 0
	case "end", "G":
		if len(m.Sessions) > 0 {
			m.Cursor = len(m.Sessions) - 1
			m.Scroll = maxInt(0, m.Cursor-visibleItems+1)
		}
	case "enter":
		if len(m.Sessions) > 0 && m.Cursor < len(m.Sessions) {
			m.SelectedSessionIdx = m.Cursor
			m.PrevScreen = ScreenSessions
			sessionID := m.Sessions[m.Cursor].ID
			return m, loadSessionObservations(m.store, sessionID)
		}
	case "r":
		return m, loadRecentSessions(m.store)
	case "esc", "q":
		m.Screen = ScreenDashboard
		m.Cursor = 0
		m.Scroll = 0
		return m, loadStats(m.store, m.Project)
	}
	return m, nil
}

// ─── Session Detail ──────────────────────────────────────────────────────────

func (m Model) handleSessionDetailKeys(key string) (tea.Model, tea.Cmd) {
	visibleItems := m.listWindow(12)
	switch key {
	case "up", "k":
		moveCursor(&m.Cursor, &m.SessionDetailScroll, -1, len(m.SessionObservations), visibleItems)
	case "down", "j":
		moveCursor(&m.Cursor, &m.SessionDetailScroll, 1, len(m.SessionObservations), visibleItems)
	case "pgup":
		moveCursor(&m.Cursor, &m.SessionDetailScroll, -visibleItems, len(m.SessionObservations), visibleItems)
	case "pgdown":
		moveCursor(&m.Cursor, &m.SessionDetailScroll, visibleItems, len(m.SessionObservations), visibleItems)
	case "home", "g":
		m.Cursor, m.SessionDetailScroll = 0, 0
	case "end", "G":
		if len(m.SessionObservations) > 0 {
			m.Cursor = len(m.SessionObservations) - 1
			m.SessionDetailScroll = maxInt(0, m.Cursor-visibleItems+1)
		}
	case "enter":
		if len(m.SessionObservations) > 0 && m.Cursor < len(m.SessionObservations) {
			obsID := m.SessionObservations[m.Cursor].ID
			m.PrevScreen = ScreenSessionDetail
			return m, loadObservationDetail(m.store, obsID)
		}
	case "c":
		if len(m.SessionObservations) > 0 && m.Cursor < len(m.SessionObservations) {
			return m, copyToClipboard(m.SessionObservations[m.Cursor].Content)
		}
	case "t":
		if len(m.SessionObservations) > 0 && m.Cursor < len(m.SessionObservations) {
			obsID := m.SessionObservations[m.Cursor].ID
			m.PrevScreen = ScreenSessionDetail
			return m, loadTimeline(m.store, obsID)
		}
	case "esc", "q":
		m.Screen = ScreenSessions
		m.Cursor = m.SelectedSessionIdx
		m.SessionDetailScroll = 0
		return m, loadRecentSessions(m.store)
	}
	return m, nil
}

// ─── About ───────────────────────────────────────────────────────────────────

func (m Model) handleAboutKeys(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "q", "enter":
		m.Screen = ScreenDashboard
		m.Cursor = 0
		return m, loadStats(m.store, m.Project)
	}
	return m, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func (m Model) observationWindow() int {
	chrome := 10
	if m.ProjectHint != "" {
		chrome++
	}
	return maxInt(1, (m.Height-chrome)/3)
}

// listWindow is how many two-line list items fit below the frame.
func (m Model) listWindow(chrome int) int {
	visible := (m.Height - chrome) / 2
	if visible < 3 {
		visible = 3
	}
	return visible
}

// moveCursor applies one navigation step and keeps the cursor inside the
// visible window; it never lets the cursor leave the list bounds.
func moveCursor(cursor, scroll *int, delta, count, visible int) {
	if count <= 0 {
		*cursor = 0
		*scroll = 0
		return
	}
	next := *cursor + delta
	if next < 0 {
		next = 0
	}
	if next > count-1 {
		next = count - 1
	}
	*cursor = next
	if *cursor < *scroll {
		*scroll = *cursor
	}
	if *cursor >= *scroll+visible {
		*scroll = *cursor - visible + 1
	}
	if *scroll < 0 {
		*scroll = 0
	}
	if *scroll > maxInt(0, count-visible) {
		*scroll = maxInt(0, count-visible)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// refreshScreen returns the appropriate data-loading Cmd for a given screen.
// Used when navigating back so lists show fresh data from the DB.
func (m Model) refreshScreen(screen Screen) tea.Cmd {
	switch screen {
	case ScreenDashboard:
		return loadStats(m.store, m.Project)
	case ScreenRecent:
		return loadRecentObservations(m.store, m.Project)
	case ScreenSessions:
		return loadRecentSessions(m.store)
	case ScreenSearchResults:
		if m.SearchQuery != "" {
			return searchMemories(m.store, m.SearchQuery, m.Project)
		}
	case ScreenCandidates:
		return loadCandidates(m.store, m.Project)
	case ScreenSimilar:
		return loadSimilar(m.store, m.Project, m.SimilarThreshold)
	case ScreenDoctor:
		return runDoctor(m.store, m.Project)
	}
	return nil
}
