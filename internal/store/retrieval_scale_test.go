package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSearchBM25RanksRareTermsBeforeRecentCommonTerms(t *testing.T) {
	s := newTestStore(t)
	want := addSearchFallbackObservation(t, s, "ranked", "engram", "project", "decision", "Kicker 正方形 SVG", "kicker 图标尺寸与画布内边距")
	for i := 0; i < 65; i++ {
		addSearchFallbackObservation(t, s, "ranked", "engram", "project", "decision", fmt.Sprintf("面板记录 %d", i), "普通面板操作说明")
	}
	for _, strategy := range []string{SearchStrategyFlat, SearchStrategyHierarchical} {
		r, err := s.SearchDetailed("kicker 面板 图标", SearchOptions{Project: "engram", Limit: 5, MatchMode: "any", Strategy: strategy, Detail: SearchDetailL0})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Results) == 0 || r.Results[0].ID != want {
			t.Fatalf("%s lost older rare-term match: %+v", strategy, r.Results)
		}
	}
}

func TestSearchBM25UsesLengthNormalization(t *testing.T) {
	s := newTestStore(t)
	addSearchFallbackObservation(t, s, "length", "engram", "project", "decision", "图标", "图标 "+strings.Repeat("其他内容 ", 150))
	want := addSearchFallbackObservation(t, s, "length", "engram", "project", "decision", "图标", "图标尺寸验证")
	r, err := s.SearchDetailed("图标", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat, Limit: 5})
	if err != nil || len(r.Results) != 2 || r.Results[0].ID != want || r.Results[0].Score == r.Results[1].Score {
		t.Fatalf("length-normalized ordering: %+v, %v", r, err)
	}
}

func TestSearchDetailedRecallsOnlyDeliveredCandidates(t *testing.T) {
	s := newTestStore(t)
	ids := []int64{}
	for i := 0; i < 3; i++ {
		ids = append(ids, addSearchFallbackObservation(t, s, "delivered", "engram", "project", "decision", fmt.Sprintf("recall marker %d", i), "recall marker body"))
	}
	r, err := s.SearchDetailed("recall marker", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat, Detail: SearchDetailL0, Limit: 1})
	if err != nil || len(r.Results) != 1 {
		t.Fatalf("search: %+v, %v", r, err)
	}
	for _, id := range ids {
		o, err := s.GetObservation(id)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if id == r.Results[0].ID {
			want = 1
		}
		if o.RecallCount != want {
			t.Fatalf("observation %d recalled %d times, want %d", id, o.RecallCount, want)
		}
	}
}

func TestSearchRawFTSPrefixIsNotDiscardedByCoverage(t *testing.T) {
	s := newTestStore(t)
	want := addSearchFallbackObservation(t, s, "raw-prefix", "engram", "project", "decision", "authentication", "authentication middleware")
	r, err := s.SearchDetailed("auth*", SearchOptions{Project: "engram", RawFTS: true, Strategy: SearchStrategyFlat})
	if err != nil || !searchResponseContains(r, want) {
		t.Fatalf("native FTS prefix dropped: %+v, %v", r, err)
	}
}

func TestSearchDetailDowngradeIsVisibleWithoutTrace(t *testing.T) {
	s := newTestStore(t)
	id := addSearchFallbackObservation(t, s, "downgrade", "engram", "project", "decision", "detail marker", strings.Repeat("detail marker body ", 100))
	l0, err := s.SearchDetailed("detail marker", SearchOptions{Project: "engram", Detail: SearchDetailL0})
	if err != nil || !searchResponseContains(l0, id) {
		t.Fatalf("selected L0 result missing: %v", err)
	}
	encoded, err := json.MarshalIndent(l0, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	budget := EstimateReadTokens(string(encoded)) + 100
	r, err := s.SearchDetailed("detail marker", SearchOptions{Project: "engram", Detail: SearchDetailL1, TokenBudget: budget})
	if err != nil || len(r.Results) != 1 {
		t.Fatalf("search: %+v, %v", r, err)
	}
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"detail_downgraded":{"requested":"l1","applied":"l0","reason":"budget"}`) || !r.Budget.Truncated {
		t.Fatalf("silent downgrade: %s", body)
	}
}

func TestSearchSubstringFallbackHonorsTimeBeforeLimit(t *testing.T) {
	s := newTestStore(t)
	want := addSearchFallbackObservation(t, s, "time-fallback", "engram", "project", "decision", "图标验证", "图标尺寸")
	if _, err := s.DB().Exec("UPDATE observations SET created_at='2026-01-01 00:00:00' WHERE id=?", want); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		addSearchFallbackObservation(t, s, "time-fallback", "engram", "project", "decision", fmt.Sprintf("图标记录 %d", i), "图标尺寸")
	}
	r, err := s.SearchDetailed("图标", SearchOptions{Project: "engram", Until: "2026-02-01", Strategy: SearchStrategyFlat, Limit: 5})
	if err != nil || !searchResponseContains(r, want) {
		t.Fatalf("time filter after candidate cutoff: %+v, %v", r, err)
	}
}

func TestSearchCJKPaginationAndMixedToken(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 2; i++ {
		addSearchFallbackObservation(t, s, "paging", "engram", "project", "decision", fmt.Sprintf("中文 API %d", i), "项目键需要统一处理大小写规则 中文 API")
	}
	first, err := s.Search("项目键 大小写", SearchOptions{Project: "engram", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Search("项目键 大小写", SearchOptions{Project: "engram", Limit: 1, Offset: 1})
	if err != nil || len(first) != 1 || len(second) != 1 || first[0].ID == second[0].ID {
		t.Fatalf("repeated fallback page: %+v, %+v, %v", first, second, err)
	}
	mixed, err := s.Search("中文API", SearchOptions{Project: "engram", Limit: 5})
	if err != nil || len(mixed) != 2 {
		t.Fatalf("mixed Chinese/ASCII token lost: %+v, %v", mixed, err)
	}
}

func TestSearchRootAndStaleAreFilteredBeforeCandidateLimit(t *testing.T) {
	s := newTestStore(t)
	want := addSearchFallbackObservation(t, s, "root-cutoff", "engram", "project", "decision", "marker", "marker "+strings.Repeat("long document ", 80))
	if _, err := s.DB().Exec("UPDATE observations SET memory_path='project/wanted' WHERE id=?", want); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		id := addSearchFallbackObservation(t, s, "root-cutoff", "engram", "project", "decision", fmt.Sprintf("marker %d", i), "marker")
		if _, err := s.DB().Exec("UPDATE observations SET memory_path='project/elsewhere', stale_at='2026-01-01 00:00:00' WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
	}
	for _, opts := range []SearchOptions{
		{Project: "engram", Strategy: SearchStrategyFlat, Detail: SearchDetailL0, RootPath: "project/wanted", IncludeStale: true},
		{Project: "engram", Strategy: SearchStrategyFlat, Detail: SearchDetailL0},
	} {
		r, err := s.SearchDetailed("marker", opts)
		if err != nil || !searchResponseContains(r, want) {
			t.Fatalf("filters after candidate limit: %+v, %v", r, err)
		}
	}
}

func TestRawFTSNotAndSingleCharacterSurviveNativeRanking(t *testing.T) {
	s := newTestStore(t)
	addSearchFallbackObservation(t, s, "raw-not", "engram", "project", "decision", "项目键", "大小写")
	r, err := s.SearchDetailed("项目键 NOT 大小写", SearchOptions{Project: "engram", RawFTS: true, Strategy: SearchStrategyFlat})
	if err != nil || len(r.Results) != 0 {
		t.Fatalf("NOT bypassed: %+v, %v", r, err)
	}
	want := addSearchFallbackObservation(t, s, "raw-not", "engram", "project", "decision", "C compiler", "C language compiler")
	r, err = s.SearchDetailed("C", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat})
	if err != nil || !searchResponseContains(r, want) {
		t.Fatalf("single-character native match lost: %+v, %v", r, err)
	}
}

func TestSearchTiesUsePinThenTimeAndReportCutoff(t *testing.T) {
	flat := []SearchResult{
		{Observation: Observation{ID: 1, Title: "marker", CreatedAt: "2025-01-01"}, Rank: -1},
		{Observation: Observation{ID: 2, Title: "marker", CreatedAt: "2026-01-01"}, Rank: -1},
		{Observation: Observation{ID: 3, Title: "marker", CreatedAt: "2024-01-01", Pinned: true}, Rank: -1},
	}
	fused := fuseSearchCandidates("marker", flat, nil, nil)
	if fused[0].result.ID != 3 || fused[1].result.ID != 2 || fused[2].result.ID != 1 {
		t.Fatalf("bad tie order: %+v", fused)
	}
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		addSearchFallbackObservation(t, s, "ties", "engram", "project", "decision", fmt.Sprintf("marker %d", i), "marker")
	}
	r, err := s.SearchDetailed("marker", SearchOptions{Project: "engram", Limit: 1, Strategy: SearchStrategyFlat})
	if err != nil || r.RecallSummary.ScoreTies != 3 || r.RecallSummary.TiedAtCutoff != 3 || r.RecallSummary.BudgetDropped != 0 || r.RecallSummary.LimitDropped != 2 {
		t.Fatalf("missing cutoff diagnostic: %+v, %v", r, err)
	}
}

func TestCJKIndexTracksChangesAndRollback(t *testing.T) {
	s := newTestStore(t)
	id := addSearchFallbackObservation(t, s, "index-update", "engram", "project", "decision", "面板", "面板说明")
	if r, err := s.Search("面板", SearchOptions{Project: "engram"}); err != nil || len(r) != 1 {
		t.Fatalf("initial index: %+v, %v", r, err)
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE observations SET title='图标',content='图标说明' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Search("图标", SearchOptions{Project: "engram"}); err != nil || len(r) != 0 {
		t.Fatalf("rolled-back index change leaked: %+v, %v", r, err)
	}
	if _, err := s.DB().Exec("UPDATE observations SET title='图标',content='图标说明' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Search("图标", SearchOptions{Project: "engram"}); err != nil || len(r) != 1 || r[0].ID != id {
		t.Fatalf("index update missed: %+v, %v", r, err)
	}
	if r, err := s.Search("面板", SearchOptions{Project: "engram"}); err != nil || len(r) != 0 {
		t.Fatalf("old index terms retained: %+v, %v", r, err)
	}
	if _, err := s.DB().Exec("UPDATE observations SET archived_at='2026-01-01 00:00:00' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Search("图标", SearchOptions{Project: "engram"}); err != nil || len(r) != 0 {
		t.Fatalf("archived index entry retained: %+v, %v", r, err)
	}
}

func TestCJKRankingPreservesNativeSinglesTopicAndTokenBoundaries(t *testing.T) {
	s := newTestStore(t)
	single := addSearchFallbackObservation(t, s, "cjk-boundaries", "engram", "project", "decision", "中", "中")
	r, err := s.SearchDetailed("中", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat})
	if err != nil || !searchResponseContains(r, single) {
		t.Fatalf("single Han lost: %+v, %v", r, err)
	}
	exact := addSearchFallbackObservation(t, s, "cjk-boundaries", "engram", "project", "decision", "exact record", "generic")
	if _, err := s.DB().Exec("UPDATE observations SET topic_key='项目/规则' WHERE id=?", exact); err != nil {
		t.Fatal(err)
	}
	addSearchFallbackObservation(t, s, "cjk-boundaries", "engram", "project", "decision", "项目 规则", "unrelated")
	r, err = s.SearchDetailed("项目/规则", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat, Limit: 1})
	if err != nil || len(r.Results) != 1 || r.Results[0].ID != exact {
		t.Fatalf("exact topic priority lost: %+v, %v", r, err)
	}
	addSearchFallbackObservation(t, s, "cjk-boundaries", "engram", "project", "decision", "中文", "capital")
	r, err = s.SearchDetailed("中文API", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat})
	if err != nil || len(r.Results) != 0 {
		t.Fatalf("ASCII substring counted as token: %+v, %v", r, err)
	}
}

func TestSearchRootUnderscoreIsLiteralBeforeLimit(t *testing.T) {
	s := newTestStore(t)
	want := addSearchFallbackObservation(t, s, "wild-root", "engram", "project", "decision", "图标 marker", strings.Repeat("long body ", 100)+"图标 marker")
	if _, err := s.DB().Exec("UPDATE observations SET memory_path='project/wild_card/child' WHERE id=?", want); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		id := addSearchFallbackObservation(t, s, "wild-root", "engram", "project", "decision", fmt.Sprintf("图标 marker %d", i), "图标 marker")
		if _, err := s.DB().Exec("UPDATE observations SET memory_path='project/wildXcard/child' WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{"图标", "marker"} {
		r, err := s.SearchDetailed(query, SearchOptions{Project: "engram", RootPath: "project/wild_card", Strategy: SearchStrategyFlat, Detail: SearchDetailL0})
		if err != nil || len(r.Results) != 1 || r.Results[0].ID != want {
			t.Fatalf("LIKE wildcard escaped scope for %s: %+v, %v", query, r, err)
		}
	}
}

func TestSearchCJKAllRejectsMissingMeaningfulTerms(t *testing.T) {
	s := newTestStore(t)
	addSearchFallbackObservation(t, s, "cjk-and", "engram", "project", "decision", "不存在的配置", "不存在的文件可以忽略")
	addSearchFallbackObservation(t, s, "cjk-and", "engram", "project", "decision", "面板", "面板说明")
	for _, query := range []string{"不存在的词xyz", "面板 图标", "不存在 xyz"} {
		for _, strategy := range []string{SearchStrategyFlat, SearchStrategyHierarchical, SearchStrategyAuto} {
			r, err := s.SearchDetailed(query, SearchOptions{Project: "engram", Strategy: strategy, Detail: SearchDetailL0})
			if err != nil || len(r.Results) != 0 {
				t.Fatalf("all broadened %q under %s: %+v, %v", query, strategy, r, err)
			}
		}
	}
}

func TestSearchCJKAnyKeepsChinesePhraseAsOneAlternative(t *testing.T) {
	s := newTestStore(t)
	want := addSearchFallbackObservation(t, s, "cjk-any", "engram", "project", "decision", "Kicker icon", "kicker SVG rendering")
	addSearchFallbackObservation(t, s, "cjk-any", "engram", "project", "decision", "URDF xyz origin", "xyz coordinates belong to robot joints")
	for i := 0; i < 30; i++ {
		addSearchFallbackObservation(t, s, "cjk-any", "engram", "project", "decision", fmt.Sprintf("不存在的配置 %d", i), "不存在的文件路径")
	}
	r, err := s.SearchDetailed("kicker 不存在的词xyz", SearchOptions{Project: "engram", MatchMode: "any", Strategy: SearchStrategyFlat, Limit: 5})
	if err != nil || len(r.Results) != 1 || r.Results[0].ID != want {
		t.Fatalf("Chinese fragments polluted OR pool: %+v, %v", r, err)
	}
}

func TestSearchStopwordsAreExplicitAndDoNotRemoveEmbeddedNegation(t *testing.T) {
	s := newTestStore(t)
	want := addSearchFallbackObservation(t, s, "stopwords", "engram", "project", "decision", "Kicker 不允许", "kicker 不允许自动覆盖")
	addSearchFallbackObservation(t, s, "stopwords", "engram", "project", "decision", "允许", "允许自动覆盖")
	r, err := s.SearchDetailed("kicker 的 了 是 在 不 和", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat})
	if err != nil || len(r.Results) != 1 || r.Results[0].ID != want {
		t.Fatalf("stopword-only alternatives entered search: %+v, %v", r, err)
	}
	body, _ := json.Marshal(r)
	if !strings.Contains(string(body), `"ignored_terms"`) {
		t.Fatalf("stopword normalization is hidden: %s", body)
	}
	if _, err := s.SearchDetailed("的 了", SearchOptions{Project: "engram"}); !errors.Is(err, ErrQueryHasNoTerms) {
		t.Fatalf("stopword-only query was silently broadened: %v", err)
	}
	r, err = s.SearchDetailed("不允许", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat})
	if err != nil || len(r.Results) != 1 || r.Results[0].ID != want {
		t.Fatalf("embedded negation was stripped: %+v, %v", r, err)
	}
}

func TestSearchAllHierarchyCannotBypassMissingTerm(t *testing.T) {
	s := newTestStore(t)
	id := addSearchFallbackObservation(t, s, "hierarchy-and", "engram", "project", "decision", "Kicker node", "kicker settings")
	if _, err := s.DB().Exec("UPDATE observations SET memory_path='project/kicker',abstract='kicker settings' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if err := s.rebuildAllMemoryNodes(); err != nil {
		t.Fatal(err)
	}
	r, err := s.SearchDetailed("kicker missingterm", SearchOptions{Project: "engram", Strategy: SearchStrategyHierarchical})
	if err != nil || len(r.Results) != 0 {
		t.Fatalf("hierarchy bypassed AND: %+v, %v", r, err)
	}
}

func TestSearchRawBM25AndTypoSuggestion(t *testing.T) {
	s := newTestStore(t)
	want := addSearchFallbackObservation(t, s, "spelling", "engram", "project", "decision", "Kicker icon", "kicker SVG rendering")
	r, err := s.SearchDetailed("kicker", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(r)
	if !strings.Contains(string(body), `"raw_bm25":-`) {
		t.Fatalf("raw BM25 absent: %s", body)
	}
	r, err = s.SearchDetailed("kiker", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(r)
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	suggestion, ok := envelope["did_you_mean"].(map[string]any)
	if !ok || suggestion["query"] != "kicker" || suggestion["id"] != float64(want) || len(r.Results) != 0 {
		t.Fatalf("typo suggestion missing or silently applied: %s", body)
	}
	addSearchFallbackObservation(t, s, "foreign-spelling", "other", "project", "decision", "secretunique", "secretunique")
	r, err = s.SearchDetailed("secretuniqe", SearchOptions{Project: "engram", Strategy: SearchStrategyFlat})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(r)
	if strings.Contains(string(body), `"did_you_mean"`) {
		t.Fatalf("typo probe crossed project scope: %s", body)
	}
}

func TestSpellingSuggestionPreservesMixedTermGroups(t *testing.T) {
	s := newTestStore(t)
	addSearchFallbackObservation(t, s, "spelling-group", "engram", "project", "decision", "中文资料", "Chinese-only reference")
	addSearchFallbackObservation(t, s, "spelling-group", "engram", "project", "decision", "Kicker icon", "kicker SVG rendering")
	r, err := s.SearchDetailed("中文kiker", SearchOptions{Project: "engram", MatchMode: "any", Strategy: SearchStrategyFlat})
	if err != nil || len(r.Results) != 0 || r.DidYouMean != nil {
		t.Fatalf("spelling suggestion split a compound into OR: %+v, %v", r, err)
	}
}

func TestExplainEmptyAddsStageCounts(t *testing.T) {
	s := newTestStore(t)
	addSearchFallbackObservation(t, s, "explain-stages", "engram", "project", "experience", "marker", "marker body")
	for _, explain := range []bool{false, true} {
		r, err := s.SearchDetailed("marker", SearchOptions{Project: "engram", Type: "decision", ExplainEmpty: explain, Strategy: SearchStrategyFlat})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(r)
		if strings.Contains(string(body), `"empty_explanation"`) != explain {
			t.Fatalf("explain_empty has no distinct effect: %s", body)
		}
	}
}

func TestSearchRejectsUnknownSelectorsAndPreservesStoredCustomType(t *testing.T) {
	s := newTestStore(t)
	want := addSearchFallbackObservation(t, s, "selectors", "engram", "project", "custom_legal_type", "Kicker custom", "kicker canvas")
	for _, opts := range []SearchOptions{{Scope: "bogus_scope"}, {Type: "bogus_type"}} {
		opts.Project = "engram"
		if _, err := s.SearchDetailed("kicker", opts); err == nil {
			t.Fatalf("selector accepted: %+v", opts)
		}
	}
	r, err := s.SearchDetailed("ｋｉｃｋｅｒ", SearchOptions{Project: "engram", Type: "custom_legal_type"})
	if err != nil || len(r.Results) != 1 || r.Results[0].ID != want {
		t.Fatalf("NFKC/custom type regression: %+v, %v", r, err)
	}
}

func TestSearchDetailedScoresAreBoundedAndLabeled(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("scale", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"alpha release note", "beta release note"} {
		if _, err := s.AddObservation(AddObservationParams{SessionID: "scale", Project: "engram", Type: "decision", Title: title, Content: title, MemoryClass: MemoryClassProject}); err != nil {
			t.Fatal(err)
		}
	}
	response, err := s.SearchDetailed("release note", SearchOptions{Project: "engram", Limit: 10, Strategy: SearchStrategyFlat})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 {
		t.Fatal("no results")
	}
	for _, result := range response.Results {
		if result.Score < 0 || result.Score > 1 {
			t.Fatalf("score out of 0..1: %+v", result)
		}
		if result.ScoreSource != "fts5_bm25" {
			t.Fatalf("score source = %q", result.ScoreSource)
		}
	}
}

func TestSearchDetailedHierarchyIgnoresSubstringInUnrelatedNode(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("substring", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "substring", Project: "engram", Type: "decision", Title: "Attested host evidence",
		Content:     "This note contains host-attested provenance but not the queried word.",
		MemoryClass: MemoryClassProject, MemoryPath: "project/tooling/provenance",
		Abstract: "host-attested provenance",
	}); err != nil {
		t.Fatal(err)
	}
	response, err := s.SearchDetailed("test", SearchOptions{Project: "engram", Limit: 10, Strategy: SearchStrategyHierarchical})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 {
		t.Fatalf("substring inside attested promoted an unrelated memory: %+v", response.Results)
	}
}

func TestSearchDetailedBudgetTooSmallIsAnErrorNotAnEmptyResult(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("budget-small", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{SessionID: "budget-small", Project: "engram", Type: "decision", Title: "Budget small", Content: "budget small marker", MemoryClass: MemoryClassProject}); err != nil {
		t.Fatal(err)
	}
	_, err := s.SearchDetailed("budget small", SearchOptions{Project: "engram", Limit: 10, TokenBudget: 1})
	if !errors.Is(err, ErrBudgetTooSmall) {
		t.Fatalf("err = %v, want ErrBudgetTooSmall", err)
	}
}

func TestSearchDetailedEmptyQueryIsRejected(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SearchDetailed("   ", SearchOptions{}); !errors.Is(err, ErrQueryRequired) {
		t.Fatalf("err = %v, want ErrQueryRequired", err)
	}
}

func TestSearchDetailedHierarchyHonorsTypeFilter(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("type-filter", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "type-filter", Project: "engram", Type: "decision", Title: "Parent node",
		Content: "Parent node document", MemoryClass: MemoryClassProject,
		MemoryPath: "project/tooling/type-filter-node", Abstract: "type filter test node",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "type-filter", Project: "engram", Type: "experience", Title: "Child experience",
		Content: "Child body without the query word", MemoryClass: MemoryClassProject,
		MemoryPath: "project/tooling/type-filter-node/child",
	}); err != nil {
		t.Fatal(err)
	}
	response, err := s.SearchDetailed("type filter test node", SearchOptions{Project: "engram", Type: "decision", Limit: 10, Strategy: SearchStrategyHierarchical})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range response.Results {
		if result.Type != "decision" {
			t.Fatalf("hierarchy bypassed type filter: %+v", result)
		}
	}
}

func TestSearchDetailedEmptyExplanationAndDidYouMean(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("explain", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{SessionID: "explain", Project: "engram", Type: "decision", Title: "Release checklist", Content: "Release checklist for the signing gate", MemoryClass: MemoryClassProject}); err != nil {
		t.Fatal(err)
	}
	none, err := s.SearchDetailed("qqqqzzzzxxxx", SearchOptions{Project: "engram", ExplainEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	if none.EmptyReason != "no_match" || len(none.Results) != 0 || none.Trace == nil {
		t.Fatalf("empty explanation = %+v", none)
	}
	partial, err := s.SearchDetailed("release missingterm", SearchOptions{Project: "engram", ExplainEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	if partial.EmptyReason != "no_match" || partial.DidYouMean != nil || partial.EmptyExplanation == nil || len(partial.EmptyExplanation.Relaxations) == 0 {
		t.Fatalf("did-you-mean = %+v", partial)
	}
	if partial.RecallSummary.Flat != 0 {
		t.Fatalf("recall summary = %+v", partial.RecallSummary)
	}
}

func TestSearchRawFTSIsOptIn(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("raw-fts", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{SessionID: "raw-fts", Project: "engram", Type: "decision", Title: "测试 面板", Content: "测试 面板", MemoryClass: MemoryClassProject}); err != nil {
		t.Fatal(err)
	}
	literal, err := s.SearchDetailed("NEAR(测试 面板)", SearchOptions{Project: "engram", ExplainEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(literal.Results) != 0 {
		t.Fatalf("raw FTS syntax was executed by default: %+v", literal.Results)
	}
	raw, err := s.SearchDetailed("NEAR(测试 面板)", SearchOptions{Project: "engram", RawFTS: true, Strategy: SearchStrategyFlat})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Results) == 0 {
		t.Fatal("explicit raw_fts did not execute FTS syntax")
	}
}

func TestSearchEmptyReasonDistinguishesFiltersAndPath(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("empty-reason", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{SessionID: "empty-reason", Project: "engram", Type: "experience", Title: "only experience", Content: "unique-empty-reason", MemoryClass: MemoryClassProject, MemoryPath: "project/runbooks/release"}); err != nil {
		t.Fatal(err)
	}
	filtered, err := s.SearchDetailed("unique-empty-reason", SearchOptions{Project: "engram", Type: "decision", ExplainEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.EmptyReason != "filtered_out" {
		t.Fatalf("type-filter reason = %q", filtered.EmptyReason)
	}
	path, err := s.SearchDetailed("unique-empty-reason", SearchOptions{Project: "engram", RootPath: "project/nonexistent", ExplainEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	if path.EmptyReason != "path_out_of_scope" {
		t.Fatalf("root-path reason = %q", path.EmptyReason)
	}
}
