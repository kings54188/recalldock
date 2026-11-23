package store

import "testing"

func addSearchFallbackObservation(t *testing.T, s *Store, sessionID, project, scope, typ, title, content string) int64 {
	t.Helper()
	if err := s.CreateSession(sessionID, project, "/work/"+project); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	id, err := s.AddObservation(AddObservationParams{
		SessionID: sessionID,
		Project:   project,
		Scope:     scope,
		Type:      typ,
		Title:     title,
		Content:   content,
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	return id
}

func TestSearchBoundedSubstringFallbackRecallsCJKWithoutEscapingFilters(t *testing.T) {
	s := newTestStore(t)
	wantID := addSearchFallbackObservation(t, s, "cjk-target", "engram", "project", "bugfix", "恢复规则", "项目键需要统一处理大小写规则")
	addSearchFallbackObservation(t, s, "cjk-wrong-project", "other", "project", "bugfix", "恢复规则", "项目键需要统一处理大小写规则")
	addSearchFallbackObservation(t, s, "cjk-wrong-scope", "engram", "personal", "bugfix", "恢复规则", "项目键需要统一处理大小写规则")
	addSearchFallbackObservation(t, s, "cjk-decoy", "engram", "project", "bugfix", "无关项目", "项目预算与大小并不相关")

	results, err := s.Search("项目键 大小写", SearchOptions{Project: "engram", Scope: "project", Type: "bugfix", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].ID != wantID {
		t.Fatalf("bounded CJK fallback escaped filters or missed target: %+v", results)
	}
}

func TestSearchBoundedSubstringFallbackPreservesAllTermsForHyphenatedQuery(t *testing.T) {
	s := newTestStore(t)
	wantID := addSearchFallbackObservation(t, s, "hyphen-target", "engram", "project", "architecture", "handoff format", "codex secure handoff envelope")
	addSearchFallbackObservation(t, s, "hyphen-decoy", "engram", "project", "architecture", "codex only", "codex configuration inventory")

	results, err := s.Search("codex-handoff", SearchOptions{Project: "engram", Scope: "project", Type: "architecture", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].ID != wantID {
		t.Fatalf("hyphen fallback must require every segment: %+v", results)
	}
}

func TestSearchSubstringFallbackDoesNotBroadenOrdinaryASCIIQueries(t *testing.T) {
	s := newTestStore(t)
	addSearchFallbackObservation(t, s, "ascii-one", "engram", "project", "decision", "auth only", "auth configuration")
	addSearchFallbackObservation(t, s, "ascii-two", "engram", "project", "decision", "session only", "session configuration")

	results, err := s.Search("auth session", SearchOptions{Project: "engram", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("ordinary ASCII default search must keep strict AND semantics: %+v", results)
	}
}

func TestSearchCJKMixedPhraseRecallsPartialTermCoverage(t *testing.T) {
	s := newTestStore(t)
	wantID := addSearchFallbackObservation(t, s, "cjk-mixed", "engram", "project", "procedure", "KDE 面板", "底部面板的 kicker 图标与 plasmoid 配置")
	results, err := s.Search("面板 kicker plasmoid 动画图标", SearchOptions{Project: "engram", MatchMode: "any", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 || results[0].ID != wantID {
		t.Fatalf("mixed CJK phrase did not recall partial coverage: %+v", results)
	}
}
