package store

import (
	"errors"
	"strings"
	"testing"
)

func refineFixture(t *testing.T) (*Store, int64, int64, int64) {
	t.Helper()
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateSession("refine-session", "refine-proj", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	add := func(title, content string) int64 {
		id, err := s.AddObservation(AddObservationParams{
			SessionID: "refine-session", Type: "procedure", Title: title, Content: content, Project: "refine-proj",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	target := add("WAL 切换顺序", "WAL 模式必须在 busy_timeout 之后切换，否则并发写会立刻报 busy。")
	first := add("升级踩坑", "WAL 模式必须在 busy_timeout 之后切换，否则并发写会立刻报 busy。旧库升级前先跑 backup --json 保存快照。")
	second := add("备份提醒", "旧库升级前先跑 backup --json 保存快照。")
	return s, target, first, second
}

func TestBuildRefineDraftAddsOnlyUniqueDetails(t *testing.T) {
	s, target, first, second := refineFixture(t)
	pinned, err := s.AddObservation(AddObservationParams{
		SessionID: "refine-session", Type: "procedure", Title: "pinned note",
		Content: "这条被置顶了，不能直接合并。", Project: "refine-proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PinObservation(pinned); err != nil {
		t.Fatal(err)
	}
	archived, err := s.AddObservation(AddObservationParams{
		SessionID: "refine-session", Type: "procedure", Title: "archived note",
		Content: "已经归档的旧笔记。", Project: "refine-proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectObservationForEdit(archived)
	if err != nil {
		t.Fatal(err)
	}
	flag := true
	if _, err := s.UpdateObservationCAS(archived, selected.SnapshotSHA256, UpdateObservationParams{Archived: &flag}, "test archive", ""); err != nil {
		t.Fatal(err)
	}

	draft, err := s.BuildRefineDraft(target, []int64{first, second, pinned, archived})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Digest == "" {
		t.Fatal("draft digest missing")
	}
	if !strings.Contains(draft.Content, RefineScaffoldMarker) {
		t.Fatalf("scaffold marker missing:\n%s", draft.Content)
	}
	if !strings.Contains(draft.Content, "旧库升级前先跑 backup --json 保存快照") {
		t.Fatalf("unique detail missing:\n%s", draft.Content)
	}
	if count := strings.Count(draft.Content, "旧库升级前先跑"); count != 1 {
		t.Fatalf("duplicate detail appears %d times:\n%s", count, draft.Content)
	}
	if strings.Count(draft.Content, "WAL 模式必须在") != 1 {
		t.Fatalf("already-covered sentence was repeated:\n%s", draft.Content)
	}
	if len(draft.Sources) != 2 {
		t.Fatalf("eligible sources = %d, want 2", len(draft.Sources))
	}
	if draft.Skipped[pinned] == "" || draft.Skipped[archived] == "" {
		t.Fatalf("skipped reasons missing: %+v", draft.Skipped)
	}
	if ids := draft.SortedSkippedIDs(); len(ids) != 2 || ids[0] > ids[1] {
		t.Fatalf("sorted skipped ids = %v", ids)
	}
}

func TestApplyRefinementRequiresCleanDraftAndCAS(t *testing.T) {
	s, target, first, second := refineFixture(t)
	draft, err := s.BuildRefineDraft(target, []int64{first, second})
	if err != nil {
		t.Fatal(err)
	}

	// A stale digest must refuse before touching either the target or sources.
	if _, err := s.ApplyRefinement(target, []int64{first, second}, "新正文", strings.Repeat("0", 64), "merge", ""); !errors.Is(err, ErrObservationSnapshotChanged) {
		t.Fatalf("stale digest error = %v", err)
	}
	// Scaffold text must never be stored.
	if _, err := s.ApplyRefinement(target, []int64{first, second}, draft.Content, draft.Digest, "merge", ""); err == nil || !strings.Contains(err.Error(), RefineScaffoldMarker) {
		t.Fatalf("scaffold content error = %v", err)
	}

	refined := "WAL 切换顺序：先设 busy_timeout，再切 WAL；升级前先跑 backup --json 保存快照。"
	result, err := s.ApplyRefinement(target, []int64{first, second}, refined, draft.Digest, "合并两条重复的升级笔记", "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || len(result.Absorbed) != 2 {
		t.Fatalf("result = %+v", result)
	}
	updated, err := s.GetObservation(target)
	if err != nil || updated.Content != refined {
		t.Fatalf("target content = %q err=%v", updated.Content, err)
	}
	for _, id := range []int64{first, second} {
		source, err := s.GetObservation(id)
		if err != nil || source.ArchivedAt == nil {
			t.Fatalf("source %d not archived: %+v %v", id, source, err)
		}
	}
	sources, err := s.ConsolidatedSources(target)
	if err != nil || len(sources) != 2 {
		t.Fatalf("lineage = %d err=%v", len(sources), err)
	}
}

func TestApplyRefinementRejectsChangedSource(t *testing.T) {
	s, target, source, _ := refineFixture(t)
	draft, err := s.BuildRefineDraft(target, []int64{source})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectObservationForEdit(source)
	if err != nil {
		t.Fatal(err)
	}
	changed := "来源在草稿生成后新增的重要内容"
	if _, err := s.UpdateObservationCAS(source, selected.SnapshotSHA256, UpdateObservationParams{Content: &changed}, "补充", ""); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetObservation(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyRefinement(target, []int64{source}, "合并后的正文", draft.Digest, "合并", ""); !errors.Is(err, ErrObservationSnapshotChanged) {
		t.Fatalf("expected stale snapshot, got %v", err)
	}
	after, err := s.GetObservation(target)
	if err != nil || after.Content != before.Content {
		t.Fatalf("target changed: %+v %v", after, err)
	}
	src, err := s.GetObservation(source)
	if err != nil || src.ArchivedAt != nil {
		t.Fatalf("source archived: %+v %v", src, err)
	}
}

func TestApplyRefinementRollsBackOnLineageFailure(t *testing.T) {
	s, target, source, _ := refineFixture(t)
	draft, err := s.BuildRefineDraft(target, []int64{source})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetObservation(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_refine_lineage BEFORE INSERT ON memory_consolidations BEGIN SELECT RAISE(ABORT, 'lineage failed'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyRefinement(target, []int64{source}, "合并后的正文", draft.Digest, "合并", "")
	if err == nil || result.Updated || len(result.Absorbed) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	after, err := s.GetObservation(target)
	if err != nil || after.Content != before.Content || after.RevisionCount != before.RevisionCount {
		t.Fatalf("target not rolled back: %+v %v", after, err)
	}
	src, err := s.GetObservation(source)
	if err != nil || src.ArchivedAt != nil {
		t.Fatalf("source not rolled back: %+v %v", src, err)
	}
}

func TestBuildRefineDraftRejectsTruncation(t *testing.T) {
	s, target, source, _ := refineFixture(t)
	targetObs, err := s.GetObservation(target)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.MaxObservationLength = len(targetObs.Content) + 10
	if _, err := s.BuildRefineDraft(target, []int64{source}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected oversized draft rejection, got %v", err)
	}
}

func TestConcurrentRefinementAndSourceEditHaveOneWinner(t *testing.T) {
	s, target, source, _ := refineFixture(t)
	other, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	draft, err := s.BuildRefineDraft(target, []int64{source})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := other.SelectObservationForEdit(source)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, err := s.ApplyRefinement(target, []int64{source}, "人工合并后的正文", draft.Digest, "合并", "")
		errs <- err
	}()
	go func() {
		<-start
		text := "并发添加的来源详情"
		_, err := other.UpdateObservationCAS(source, selected.SnapshotSHA256, UpdateObservationParams{Content: &text}, "补充", "")
		errs <- err
	}()
	close(start)
	first, second := <-errs, <-errs
	if (first == nil) == (second == nil) {
		t.Fatalf("expected one success, got %v and %v", first, second)
	}
	for _, err := range []error{first, second} {
		if err != nil && !errors.Is(err, ErrObservationSnapshotChanged) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestApplyRefinementCanArchiveWithoutRewrite(t *testing.T) {
	s, target, first, second := refineFixture(t)
	before, err := s.GetObservation(target)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := s.BuildRefineDraft(target, []int64{first, second})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyRefinement(target, []int64{first, second}, before.Content, draft.Digest, "内容已完整，仅归档来源", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated || len(result.Absorbed) != 2 {
		t.Fatalf("result = %+v", result)
	}
	after, err := s.GetObservation(target)
	if err != nil || after.RevisionCount != before.RevisionCount {
		t.Fatalf("revision changed without a text edit: %+v %v", after, err)
	}
	// Retrying is idempotent: sources are already archived and keep lineage.
	draft, err = s.BuildRefineDraft(target, []int64{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Sources) != 0 || len(draft.Skipped) != 2 {
		t.Fatalf("retry draft = sources %d skipped %d", len(draft.Sources), len(draft.Skipped))
	}
}

func TestApplyRefinementReportsPartialFailure(t *testing.T) {
	s, target, first, _ := refineFixture(t)
	pinned, err := s.AddObservation(AddObservationParams{
		SessionID: "refine-session", Type: "procedure", Title: "pinned note",
		Content: "置顶细节不能合并。", Project: "refine-proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PinObservation(pinned); err != nil {
		t.Fatal(err)
	}
	draft, err := s.BuildRefineDraft(target, []int64{first, pinned})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyRefinement(target, []int64{first, pinned}, "合并后的正文", draft.Digest, "合并来源", "")
	if err == nil {
		t.Fatal("pinned source should be reported")
	}
	if len(result.Failed) != 1 || result.Failed[pinned] == "" {
		t.Fatalf("failed = %+v", result.Failed)
	}
	if len(result.Absorbed) != 1 || result.Absorbed[0] != first {
		t.Fatalf("absorbed = %v", result.Absorbed)
	}
}
