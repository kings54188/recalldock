package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"recalldock.local/recalldock/internal/store"
)

func similarTUIStore(t *testing.T) (*store.Store, int64, int64) {
	t.Helper()
	cfg, err := store.DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateSession("sim-sess", "tui-sim", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	add := func(title, content string) int64 {
		id, err := s.AddObservation(store.AddObservationParams{
			SessionID: "sim-sess", Type: "procedure", Title: title, Content: content, Project: "tui-sim",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := add("WAL 切换顺序", "WAL 模式必须在 busy_timeout 之后切换，否则并发写会立刻报 busy。")
	second := add("升级踩坑", "WAL 模式必须在 busy_timeout 之后切换，否则并发写会立刻报 busy。升级前先跑 backup --json。")
	return s, first, second
}

func TestSimilarScreenLoadsRendersAndOpensTarget(t *testing.T) {
	s, _, second := similarTUIStore(t)
	groups, err := s.SimilarMemoryGroups("tui-sim", 0.6, 20)
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups = %d err=%v", len(groups), err)
	}
	m := New(s, "test")
	m.Project = "tui-sim"
	m.Screen = ScreenSimilar
	m.Width = 100
	m.Height = 30

	updatedModel, _ := m.Update(similarLoadedMsg{groups: groups, project: "tui-sim", threshold: 0.6})
	updated := updatedModel.(Model)
	if len(updated.SimilarGroups) != 1 {
		t.Fatalf("loaded groups = %d", len(updated.SimilarGroups))
	}
	view := updated.View()
	for _, want := range []string{"Similar memories", "建议目标", "WAL 切换顺序", "升级踩坑"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}

	updatedModel, cmd := updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = updatedModel.(Model)
	if cmd == nil {
		t.Fatal("enter should load the suggested target")
	}
	msg := cmd()
	detail, ok := msg.(observationDetailMsg)
	if !ok || detail.err != nil || detail.observation == nil {
		t.Fatalf("detail msg = %#v", msg)
	}
	if detail.observation.ID != second {
		t.Fatalf("opened #%d, want suggested target #%d", detail.observation.ID, second)
	}
	if updated.PrevScreen != ScreenSimilar {
		t.Fatalf("prev screen = %v", updated.PrevScreen)
	}
}

func TestSimilarScreenThresholdKeysAndEmptyScope(t *testing.T) {
	s, _, _ := similarTUIStore(t)
	m := New(s, "test")
	m.Project = "tui-sim"
	m.Screen = ScreenSimilar
	m.Width = 100
	m.Height = 30

	updatedModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("]")})
	updated := updatedModel.(Model)
	if updated.SimilarThreshold < 0.64 || updated.SimilarThreshold > 0.66 {
		t.Fatalf("threshold = %v", updated.SimilarThreshold)
	}
	if cmd == nil {
		t.Fatal("threshold change should rescan")
	}
	if msg, ok := cmd().(similarLoadedMsg); !ok || msg.err != nil {
		t.Fatalf("rescan msg = %#v", msg)
	}

	m = New(s, "test")
	m.Project = ""
	m.Screen = ScreenSimilar
	m.Width = 100
	m.Height = 30
	view := m.View()
	if !strings.Contains(view, "先选择一个具体项目") {
		t.Fatalf("empty scope hint missing:\n%s", view)
	}
	updatedModel, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if cmd == nil {
		t.Fatal("r should rescan")
	}
	if msg, ok := cmd().(similarLoadedMsg); !ok || msg.project != "" || len(msg.groups) != 0 {
		t.Fatalf("empty scope scan = %#v", msg)
	}
}

func TestSimilarScreenClipsAndSanitizesTitles(t *testing.T) {
	longTitle := "超长标题" + strings.Repeat("很长的细节", 30) + "\x1b[2J"
	groups := []store.SimilarMemoryGroup{{
		Score:           0.91,
		SuggestedTarget: 7,
		Observations: []store.Observation{
			{ID: 7, Title: longTitle},
			{ID: 8, Title: "短标题"},
		},
	}}
	m := New(nil, "")
	m.Project = "p"
	m.Screen = ScreenSimilar
	m.Width = 58
	m.Height = 16
	m.SimilarGroups = groups
	view := m.View()
	if strings.Contains(view, "\x1b") {
		t.Fatalf("escape sequence leaked into the frame: %q", view)
	}
	if strings.Contains(view, longTitle) {
		t.Fatal("unclipped title leaked into the frame")
	}
	for _, want := range []string{"0.91", "短标题"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q", want)
		}
	}
}

func TestSimilarScreenEscReturnsDashboard(t *testing.T) {
	m := New(nil, "")
	m.Screen = ScreenSimilar
	m.SimilarCursor = 2

	updatedModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	updated := updatedModel.(Model)
	if updated.Screen != ScreenDashboard {
		t.Fatalf("screen = %v", updated.Screen)
	}
	if updated.SimilarCursor != 0 {
		t.Fatalf("cursor = %d", updated.SimilarCursor)
	}
}
