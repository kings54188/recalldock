package operatorui

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func similarFixture(t *testing.T) (*Server, *store.Store, string, int64, int64, int64) {
	t.Helper()
	ui, s, token := newTestServer(t)
	ui.project = "similar-proj"
	if err := s.CreateSession("similar-sess", "similar-proj", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	add := func(title, content string) int64 {
		id, err := s.AddObservation(store.AddObservationParams{
			SessionID: "similar-sess", Type: "procedure", Title: title, Content: content, Project: "similar-proj",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	target := add("WAL 切换顺序", "WAL 模式必须在 busy_timeout 之后切换，否则并发写会立刻报 busy。")
	first := add("升级踩坑", "WAL 模式必须在 busy_timeout 之后切换，否则并发写会立刻报 busy。旧库升级前先跑 backup --json 保存快照。")
	second := add("备份提醒", "旧库升级前先跑 backup --json 保存快照，并核对 integrity。")
	return ui, s, token, target, first, second
}

func scopedCookie(token string) string {
	return cookieName + "=" + token + "; " + projectCookieName + "=similar-proj"
}

func TestSimilarThresholdRejectsNaN(t *testing.T) {
	if got := parseSimilarThreshold("NaN"); got != 0.6 {
		t.Fatalf("threshold = %v", got)
	}
}

func TestUISimilarGroupsScoped(t *testing.T) {
	ui, _, token, target, _, _ := similarFixture(t)
	rec := do(ui, "GET", "/similar", "", map[string]string{"Cookie": scopedCookie(token)})
	if rec.Code != http.StatusOK {
		t.Fatalf("similar page: %d %s", rec.Code, rec.Body.String()[:300])
	}
	body := rec.Body.String()
	if !strings.Contains(body, "建议目标") || !strings.Contains(body, "生成精炼草稿") {
		t.Fatalf("group controls missing: %s", body[:500])
	}
	if !strings.Contains(body, "name=\"target\" value=\"") {
		t.Fatalf("suggested target control missing")
	}
	if !strings.Contains(body, "href=\"/memory/"+intString(target)+"\"") {
		t.Fatalf("target %d link missing", target)
	}
	if strings.Contains(body, "只读模式") {
		t.Fatal("mutation form should be present without --read-only")
	}
}

func TestUISimilarDraftAndApplyCAS(t *testing.T) {
	ui, s, token, target, first, second := similarFixture(t)
	cookie := scopedCookie(token)
	rec := do(ui, "GET", "/similar", "", map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("similar page: %d", rec.Code)
	}

	form := url.Values{}
	form.Set("csrf", token)
	form.Set("target", intString(target))
	form.Add("sources", intString(target))
	form.Add("sources", intString(first))
	form.Add("sources", intString(second))
	rec = do(ui, "POST", "/similar/draft", form.Encode(), map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("draft: %d %s", rec.Code, rec.Body.String()[:400])
	}
	draftBody := rec.Body.String()
	if !strings.Contains(draftBody, store.RefineScaffoldMarker) {
		t.Fatalf("draft scaffold missing: %s", draftBody[:500])
	}
	if !strings.Contains(draftBody, "integrity") {
		t.Fatalf("source-only detail missing from draft")
	}
	digest := regexp.MustCompile(`name="expected" value="([0-9a-f]{64})"`).FindStringSubmatch(draftBody)
	if len(digest) != 2 {
		t.Fatalf("digest hidden field missing")
	}

	// Saving the scaffold unchanged must be refused, with the target untouched.
	scaffold := url.Values{"csrf": {token}, "target": {intString(target)}, "expected": {digest[1]},
		"content": {"WAL 先设 busy_timeout。" + store.RefineScaffoldMarker + " 未合并"}, "reason": {"合并"}}
	for _, id := range []int64{target, first, second} {
		scaffold.Add("sources", intString(id))
	}
	rec = do(ui, "POST", "/similar/apply", scaffold.Encode(), map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "待人工精炼") {
		t.Fatalf("scaffold apply: %d %s", rec.Code, rec.Body.String()[:300])
	}

	// A stale digest must also be refused before archiving anything.
	stale := url.Values{"csrf": {token}, "target": {intString(target)}, "expected": {strings.Repeat("a", 64)},
		"content": {"合并后的正文"}, "reason": {"合并"}}
	for _, id := range []int64{first, second} {
		stale.Add("sources", intString(id))
	}
	rec = do(ui, "POST", "/similar/apply", stale.Encode(), map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "重新生成草稿") {
		t.Fatalf("stale apply: %d %s", rec.Code, rec.Body.String()[:300])
	}

	refined := "WAL 切换顺序：先设 busy_timeout；旧库升级前先跑 backup --json 并核对 integrity。"
	apply := url.Values{"csrf": {token}, "target": {intString(target)}, "expected": {digest[1]},
		"content": {refined}, "reason": {"合并两条重复的升级笔记"}}
	for _, id := range []int64{first, second} {
		apply.Add("sources", intString(id))
	}
	rec = do(ui, "POST", "/similar/apply", apply.Encode(), map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String()[:400])
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/memory/"+intString(target)) {
		t.Fatalf("apply redirect = %q", loc)
	}
	updated, err := s.GetObservation(target)
	if err != nil || updated.Content != refined {
		t.Fatalf("target content = %q err=%v", updated.Content, err)
	}
	for _, id := range []int64{first, second} {
		source, err := s.GetObservation(id)
		if err != nil || source.ArchivedAt == nil {
			t.Fatalf("source %d not archived", id)
		}
	}
}

func TestUISimilarRejectsChangedSource(t *testing.T) {
	ui, s, token, target, source, _ := similarFixture(t)
	cookie := scopedCookie(token)
	form := url.Values{"csrf": {token}, "target": {intString(target)}, "sources": {intString(source)}}
	rec := do(ui, "POST", "/similar/draft", form.Encode(), map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("draft: %d", rec.Code)
	}
	digest := regexp.MustCompile(`name="expected" value="([0-9a-f]{64})"`).FindStringSubmatch(rec.Body.String())
	if len(digest) != 2 {
		t.Fatal("missing digest")
	}
	selected, err := s.SelectObservationForEdit(source)
	if err != nil {
		t.Fatal(err)
	}
	changed := "来源新添了一段需要保留的重要说明。"
	if _, err := s.UpdateObservationCAS(source, selected.SnapshotSHA256, store.UpdateObservationParams{Content: &changed}, "补充", ""); err != nil {
		t.Fatal(err)
	}
	form.Set("expected", digest[1])
	form.Set("content", "合并后的正文")
	form.Set("reason", "合并")
	rec = do(ui, "POST", "/similar/apply", form.Encode(), map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "重新生成草稿") {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	obs, err := s.GetObservation(source)
	if err != nil || obs.ArchivedAt != nil {
		t.Fatalf("source archived: %+v %v", obs, err)
	}
}

func TestUISimilarReadOnlyAndInvalidInput(t *testing.T) {
	ui, s, token := newTestServer(t)
	ui.project = "ro-proj"
	if err := s.CreateSession("ro-sess", "ro-proj", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	target, err := s.AddObservation(store.AddObservationParams{
		SessionID: "ro-sess", Type: "note", Title: "ro target", Content: "target body", Project: "ro-proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.AddObservation(store.AddObservationParams{
		SessionID: "ro-sess", Type: "note", Title: "ro source", Content: "target body with extra", Project: "ro-proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := New(Options{Store: s, Version: "test", Project: ui.project, Token: "test-token", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	cookie := cookieName + "=" + token + "; " + projectCookieName + "=" + ui.project
	form := url.Values{"csrf": {token}, "target": {intString(target)}}
	form.Add("sources", intString(source))
	rec := do(readOnly, "POST", "/similar/draft", form.Encode(), map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "只读模式") {
		t.Fatalf("read-only draft: %d %s", rec.Code, rec.Body.String()[:200])
	}
	rec = do(ui, "POST", "/similar/draft", url.Values{"csrf": {token}, "target": {intString(target)}}.Encode(), map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no sources draft: %d", rec.Code)
	}
	rec = do(ui, "POST", "/similar/apply", url.Values{"csrf": {token}, "target": {intString(target)}, "content": {"x"}, "expected": {strings.Repeat("0", 64)}, "reason": {""}}.Encode(), map[string]string{"Cookie": cookie})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty reason apply: %d", rec.Code)
	}
}

// intString keeps test form values aligned with the decimal IDs the UI emits.
func intString(value int64) string {
	return strconv.FormatInt(value, 10)
}
