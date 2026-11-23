// Package tui implements the Bubbletea terminal UI for Engram.
//
// Following the Gentleman Bubbletea patterns:
// - Screen constants as iota
// - Single Model struct holds ALL state
// - Update() with type switch
// - Per-screen key handlers returning (tea.Model, tea.Cmd)
// - Vim keys (j/k) for navigation
// - PrevScreen for back navigation
package tui

import (
	"os"

	"recalldock.local/recalldock/internal/diagnostic"
	projectpkg "recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
	"recalldock.local/recalldock/internal/version"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// ─── Screens ─────────────────────────────────────────────────────────────────

type Screen int

const (
	ScreenDashboard Screen = iota
	ScreenSearch
	ScreenSearchResults
	ScreenRecent
	ScreenObservationDetail
	ScreenTimeline
	ScreenSessions
	ScreenSessionDetail
	ScreenCandidates
	ScreenSimilar
	ScreenDoctor
	ScreenAbout
)

// ─── Custom Messages ─────────────────────────────────────────────────────────

type updateCheckMsg struct {
	result version.CheckResult
}

type statsLoadedMsg struct {
	stats *store.Stats
	err   error
}

type searchResultsMsg struct {
	results []store.SearchResult
	query   string
	err     error
}

type recentObservationsMsg struct {
	observations []store.Observation
	err          error
}

type observationDetailMsg struct {
	observation *store.Observation
	err         error
}

type timelineMsg struct {
	timeline *store.TimelineResult
	err      error
}

type recentSessionsMsg struct {
	sessions []store.SessionSummary
	err      error
}

type sessionObservationsMsg struct {
	observations []store.Observation
	err          error
}

// ─── Model ───────────────────────────────────────────────────────────────────

type Model struct {
	store      *store.Store
	Version    string
	Screen     Screen
	PrevScreen Screen
	Width      int
	Height     int
	Cursor     int
	Scroll     int

	// Update notification
	UpdateStatus version.CheckStatus
	UpdateMsg    string

	// Error display
	ErrorMsg string

	// Dashboard
	Stats *store.Stats

	// Search
	SearchInput   textinput.Model
	SearchQuery   string
	SearchResults []store.SearchResult

	// Recent observations
	RecentObservations []store.Observation

	// Observation detail
	SelectedObservation *store.Observation
	DetailScroll        int

	// Timeline
	Timeline *store.TimelineResult

	// Sessions
	Sessions            []store.SessionSummary
	SelectedSessionIdx  int
	SessionObservations []store.Observation
	SessionDetailScroll int

	// Clipboard feedback
	CopyFeedback string // "✓ Copied!" or "" — shown for 2 s after copy

	// Operator screens
	Project           string
	ProjectHint       string
	ProjectScopes     []projectScope
	ProjectIndex      int
	Candidates        []store.MemoryCandidate
	CandidateCursor   int
	CandidateReason   textinput.Model
	CandidateAction   string
	CandidateFeedback string
	DoctorReport      *diagnostic.Report
	DoctorScroll      int
	DoctorAll         bool // show passing checks too
	SimilarGroups     []store.SimilarMemoryGroup
	SimilarCursor     int
	SimilarScroll     int
	SimilarThreshold  float64
	SimilarProject    string
}

// New creates a new TUI model connected to the given store.
func New(s *store.Store, version string) Model {
	ti := textinput.New()
	ti.Placeholder = "Search memories..."
	ti.CharLimit = 256
	ti.Width = 60

	reason := textinput.New()
	reason.Placeholder = "reason (required)"
	reason.CharLimit = 200
	reason.Width = 60

	m := Model{
		store:            s,
		Version:          version,
		Screen:           ScreenDashboard,
		SearchInput:      ti,
		CandidateReason:  reason,
		ProjectIndex:     -1,
		SimilarThreshold: 0.6,
	}
	if s != nil {
		names := map[string]string{}
		if projects, err := s.ListRegisteredProjects(); err == nil {
			for _, project := range projects {
				names[project.ID] = project.Name
			}
		}
		scopes := []projectScope{{ID: "", Label: "all projects"}}
		detected, source := "", ""
		if cwd, err := os.Getwd(); err == nil {
			if result := s.DetectProject(cwd); result.Error == nil {
				detected, source = result.Project, result.Source
				if detected != "" && (source == projectpkg.SourceRegistered || source == projectpkg.SourceWorktreeMain || source == projectpkg.SourceConfig) {
					if canonical, err := s.CanonicalProjectKey(detected); err != nil {
						m.ErrorMsg = err.Error()
					} else {
						detected = canonical
					}
				}
			}
		}
		if detected != "" {
			label := detected
			if name := names[detected]; name != "" {
				label = name
			}
			switch source {
			case projectpkg.SourceRegistered, projectpkg.SourceWorktreeMain, projectpkg.SourceConfig:
				scopes = append(scopes, projectScope{ID: detected, Label: label})
				m.ProjectIndex = 1
				m.Project = detected
			default:
				m.ProjectHint = detected + " (" + source + ")"
			}
		}
		for _, project := range projectsFromStore(s) {
			if project.ID == detected {
				continue
			}
			scopes = append(scopes, projectScope{ID: project.ID, Label: project.Name})
		}
		m.ProjectScopes = scopes
	}
	if m.ProjectIndex < 0 {
		m.ProjectIndex = 0
	}
	return m
}

// Init loads initial data (stats for the dashboard).
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		loadStats(m.store, m.Project),
		checkForUpdate(m.Version),
		tea.EnterAltScreen,
	)
}

// ─── Commands (data loading) ─────────────────────────────────────────────────

func checkForUpdate(v string) tea.Cmd {
	return func() tea.Msg {
		return updateCheckMsg{result: version.CheckLatest(v)}
	}
}

func loadStats(s *store.Store, project string) tea.Cmd {
	return func() tea.Msg {
		stats, err := s.ProjectStats(project)
		return statsLoadedMsg{stats: stats, err: err}
	}
}

func searchMemories(s *store.Store, query, project string) tea.Cmd {
	return func() tea.Msg {
		results, err := s.Search(query, store.SearchOptions{Project: project, Limit: 50})
		return searchResultsMsg{results: results, query: query, err: err}
	}
}

func loadRecentObservations(s *store.Store, project string) tea.Cmd {
	return func() tea.Msg {
		obs, err := s.RecentObservations(project, "", 50)
		return recentObservationsMsg{observations: obs, err: err}
	}
}

func loadObservationDetail(s *store.Store, id int64) tea.Cmd {
	return func() tea.Msg {
		obs, err := s.GetObservation(id)
		return observationDetailMsg{observation: obs, err: err}
	}
}

func loadTimeline(s *store.Store, obsID int64) tea.Cmd {
	return func() tea.Msg {
		tl, err := s.Timeline(obsID, 10, 10)
		return timelineMsg{timeline: tl, err: err}
	}
}

func loadRecentSessions(s *store.Store) tea.Cmd {
	return func() tea.Msg {
		sessions, err := s.AllSessions("", 50)
		return recentSessionsMsg{sessions: sessions, err: err}
	}
}

func loadSessionObservations(s *store.Store, sessionID string) tea.Cmd {
	return func() tea.Msg {
		obs, err := s.SessionObservations(sessionID, 200)
		return sessionObservationsMsg{observations: obs, err: err}
	}
}

func projectsFromStore(s *store.Store) []store.RegisteredProject {
	projects, err := s.ListRegisteredProjects()
	if err != nil {
		return nil
	}
	return projects
}
