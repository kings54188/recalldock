package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"recalldock.local/recalldock/internal/store"
	"recalldock.local/recalldock/internal/version"
)

type testFixture struct {
	store        *store.Store
	sessionID    string
	obsID        int64
	secondObs    int64
	otherSession string
}

func newTestFixture(t *testing.T) testFixture {
	t.Helper()
	t.Chdir(t.TempDir())

	cfg, err := store.DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.DataDir = t.TempDir()

	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.CreateSession("session-1", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("create session-1: %v", err)
	}
	if err := s.CreateSession("session-2", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("create session-2: %v", err)
	}

	obsID, err := s.AddObservation(store.AddObservationParams{
		SessionID: "session-1",
		Type:      "bugfix",
		Title:     "Needle observation",
		Content:   "needle content for deterministic search",
		Project:   "engram",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("add first observation: %v", err)
	}

	secondObs, err := s.AddObservation(store.AddObservationParams{
		SessionID: "session-1",
		Type:      "decision",
		Title:     "Second observation",
		Content:   "timeline sibling",
		Project:   "engram",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("add second observation: %v", err)
	}

	return testFixture{store: s, sessionID: "session-1", obsID: obsID, secondObs: secondObs, otherSession: "session-2"}
}

func TestConfiguredProjectAliasUsesCanonicalScope(t *testing.T) {
	fx := newTestFixture(t)
	registered, err := fx.store.CreateRegisteredProject("legacy-alias")
	if err != nil {
		t.Fatal(err)
	}
	project := registered.ID
	if _, err := fx.store.Import(&store.ExportData{
		Sessions:     []store.Session{{ID: "canonical-session", Project: project, Directory: "/synthetic", StartedAt: store.Now()}},
		Observations: []store.Observation{{SyncID: "canonical-observation", SessionID: "canonical-session", Project: &project, Type: "decision", MemoryClass: "project", Scope: "project", Title: "Canonical fixture", Content: "Canonical fixture", CreatedAt: store.Now(), UpdatedAt: store.Now()}},
	}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".recalldock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".recalldock", "config.json"), []byte(`{"project_name":"legacy-alias"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	model := New(fx.store, "test")
	if model.Project != project {
		t.Fatalf("configured alias remained raw: %q, want %q", model.Project, project)
	}
	recent := loadRecentObservations(fx.store, model.Project)().(recentObservationsMsg)
	if recent.err != nil || len(recent.observations) != 1 || recent.observations[0].Title != "Canonical fixture" {
		t.Fatalf("canonical recent missing: %+v %v", recent.observations, recent.err)
	}
	matches := 0
	for _, scope := range model.ProjectScopes {
		if scope.ID == project {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("canonical project duplicated in selector: %d", matches)
	}
}

func TestRecentObservationsExcludeArchiveWithoutChangingHistory(t *testing.T) {
	fx := newTestFixture(t)
	selected, err := fx.store.SelectObservationForEdit(fx.obsID)
	if err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := fx.store.UpdateObservationCAS(fx.obsID, selected.SnapshotSHA256, store.UpdateObservationParams{Archived: &archived}, "archive fixture", ""); err != nil {
		t.Fatal(err)
	}
	loaded := loadRecentObservations(fx.store, "engram")().(recentObservationsMsg)
	if loaded.err != nil {
		t.Fatal(loaded.err)
	}
	for _, observation := range loaded.observations {
		if observation.ArchivedAt != nil {
			t.Fatal("recent list contains archived memory")
		}
	}
	if len(loaded.observations) != 1 || loaded.observations[0].ID != fx.secondObs {
		t.Fatalf("recent = %+v", loaded.observations)
	}
	history, err := fx.store.AllObservations("engram", "", 50)
	if err != nil || len(history) != 2 {
		t.Fatalf("historical contract changed: count=%d err=%v", len(history), err)
	}
}

func TestRecentObservationsLimitAppliesAfterArchiveFilter(t *testing.T) {
	fx := newTestFixture(t)
	ids := []int64{fx.obsID, fx.secondObs}
	for i := len(ids); i < 60; i++ {
		id, err := fx.store.AddObservation(store.AddObservationParams{
			SessionID: fx.sessionID,
			Project:   "engram",
			Type:      "decision",
			Title:     fmt.Sprintf("Recent fact %02d", i),
			Content:   fmt.Sprintf("Distinct recent knowledge number %02d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	archived := true
	for _, id := range ids[10:] {
		selected, err := fx.store.SelectObservationForEdit(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fx.store.UpdateObservationCAS(id, selected.SnapshotSHA256, store.UpdateObservationParams{Archived: &archived}, "archive recent fixture", ""); err != nil {
			t.Fatal(err)
		}
	}
	history, err := fx.store.AllObservations("engram", "", 50)
	if err != nil || len(history) != 50 {
		t.Fatalf("history count=%d err=%v", len(history), err)
	}
	for i, observation := range history {
		if observation.ID != ids[59-i] || observation.ArchivedAt == nil {
			t.Fatalf("latest history[%d] = %+v", i, observation)
		}
	}
	for _, project := range []string{"engram", "", " ENGRAM "} {
		loaded := loadRecentObservations(fx.store, project)().(recentObservationsMsg)
		if loaded.err != nil || len(loaded.observations) != 10 {
			t.Errorf("project %q: recent count=%d err=%v", project, len(loaded.observations), loaded.err)
			continue
		}
		for i, observation := range loaded.observations {
			if observation.ID != ids[9-i] || observation.ArchivedAt != nil {
				t.Errorf("project %q: recent[%d] = %+v", project, i, observation)
			}
		}
	}
	missing := loadRecentObservations(fx.store, "unrelated-project")().(recentObservationsMsg)
	if missing.err != nil || len(missing.observations) != 0 {
		t.Fatalf("unrelated project recent=%+v err=%v", missing.observations, missing.err)
	}
}

func TestNewInitializesModelDefaults(t *testing.T) {
	m := New(nil, "")

	if m.Screen != ScreenDashboard {
		t.Fatalf("screen = %v, want %v", m.Screen, ScreenDashboard)
	}
	if m.SearchInput.Placeholder != "Search memories..." {
		t.Fatalf("placeholder = %q", m.SearchInput.Placeholder)
	}
	if m.SearchInput.CharLimit != 256 {
		t.Fatalf("char limit = %d", m.SearchInput.CharLimit)
	}
	if m.SearchInput.Width != 60 {
		t.Fatalf("width = %d", m.SearchInput.Width)
	}
	if m.ProjectIndex < 0 {
		t.Fatal("project index should default to a valid scope")
	}
}

func TestInitReturnsCommand(t *testing.T) {
	m := New(newTestFixture(t).store, "")
	if cmd := m.Init(); cmd == nil {
		t.Fatal("init should return a startup command")
	}
}

func TestDataLoadingCommands(t *testing.T) {
	fx := newTestFixture(t)

	t.Run("checkForUpdate", func(t *testing.T) {
		t.Setenv("RECALLDOCK_RELEASE_API_URL", "")
		msg := checkForUpdate("dev")()
		loaded, ok := msg.(updateCheckMsg)
		if !ok {
			t.Fatalf("message type = %T", msg)
		}
		if loaded.result.Status != version.StatusUpToDate {
			t.Fatalf("status = %q, want %q", loaded.result.Status, version.StatusUpToDate)
		}
	})

	t.Run("loadStats", func(t *testing.T) {
		msg := loadStats(fx.store, "")()
		loaded, ok := msg.(statsLoadedMsg)
		if !ok {
			t.Fatalf("message type = %T", msg)
		}
		if loaded.err != nil {
			t.Fatalf("unexpected error: %v", loaded.err)
		}
		if loaded.stats == nil || loaded.stats.TotalSessions < 2 {
			t.Fatalf("unexpected stats: %+v", loaded.stats)
		}
	})

	t.Run("searchMemories", func(t *testing.T) {
		msg := searchMemories(fx.store, "needle", "")()
		loaded, ok := msg.(searchResultsMsg)
		if !ok {
			t.Fatalf("message type = %T", msg)
		}
		if loaded.err != nil {
			t.Fatalf("unexpected error: %v", loaded.err)
		}
		if loaded.query != "needle" {
			t.Fatalf("query = %q", loaded.query)
		}
		if len(loaded.results) == 0 {
			t.Fatal("expected at least one search result")
		}
	})

	t.Run("loadRecentObservations", func(t *testing.T) {
		msg := loadRecentObservations(fx.store, "")()
		loaded, ok := msg.(recentObservationsMsg)
		if !ok {
			t.Fatalf("message type = %T", msg)
		}
		if loaded.err != nil {
			t.Fatalf("unexpected error: %v", loaded.err)
		}
		if len(loaded.observations) < 2 {
			t.Fatalf("observations = %d, want >= 2", len(loaded.observations))
		}
	})

	t.Run("loadObservationDetail", func(t *testing.T) {
		msg := loadObservationDetail(fx.store, fx.obsID)()
		loaded, ok := msg.(observationDetailMsg)
		if !ok {
			t.Fatalf("message type = %T", msg)
		}
		if loaded.err != nil {
			t.Fatalf("unexpected error: %v", loaded.err)
		}
		if loaded.observation == nil || loaded.observation.ID != fx.obsID {
			t.Fatalf("unexpected observation: %+v", loaded.observation)
		}
	})

	t.Run("loadTimeline", func(t *testing.T) {
		msg := loadTimeline(fx.store, fx.secondObs)()
		loaded, ok := msg.(timelineMsg)
		if !ok {
			t.Fatalf("message type = %T", msg)
		}
		if loaded.err != nil {
			t.Fatalf("unexpected error: %v", loaded.err)
		}
		if loaded.timeline == nil || loaded.timeline.Focus.ID != fx.secondObs {
			t.Fatalf("unexpected timeline focus: %+v", loaded.timeline)
		}
	})

	t.Run("loadRecentSessions", func(t *testing.T) {
		msg := loadRecentSessions(fx.store)()
		loaded, ok := msg.(recentSessionsMsg)
		if !ok {
			t.Fatalf("message type = %T", msg)
		}
		if loaded.err != nil {
			t.Fatalf("unexpected error: %v", loaded.err)
		}
		if len(loaded.sessions) < 2 {
			t.Fatalf("sessions = %d, want >= 2", len(loaded.sessions))
		}
	})

	t.Run("loadSessionObservations", func(t *testing.T) {
		msg := loadSessionObservations(fx.store, fx.sessionID)()
		loaded, ok := msg.(sessionObservationsMsg)
		if !ok {
			t.Fatalf("message type = %T", msg)
		}
		if loaded.err != nil {
			t.Fatalf("unexpected error: %v", loaded.err)
		}
		if len(loaded.observations) < 2 {
			t.Fatalf("observations = %d, want >= 2", len(loaded.observations))
		}
	})
}
