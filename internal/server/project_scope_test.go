package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestPublicHTTPRequiresSelectionBeforeListingMemory(t *testing.T) {
	s := newServerTestStore(t)
	root := filepath.Join(t.TempDir(), "legacy-name")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	if err := s.CreateSession("seed", "legacy-name", root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "seed", Project: "legacy-name", Type: "decision", Title: "PRIVATE_HTTP_UNBOUND", Content: "unique-http-scope-needle PRIVATE_L2_ONLY", Scope: "project"}); err != nil {
		t.Fatal(err)
	}
	h := New(s, 0).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		return r
	}
	for _, path := range []string{"/search?q=unique-http-scope-needle", "/observations/recent", "/stats"} {
		r := get(path)
		if r.Code != 400 || !strings.Contains(r.Body.String(), "project_not_selected") || strings.Contains(r.Body.String(), "PRIVATE_HTTP_UNBOUND") {
			t.Fatalf("unbound %s: %d %s", path, r.Code, r.Body.String())
		}
	}
	r := get("/search?q=unique-http-scope-needle&project=legacy-name")
	if r.Code != 200 || !strings.Contains(r.Body.String(), "PRIVATE_HTTP_UNBOUND") {
		t.Fatalf("explicit historical selection: %d %s", r.Code, r.Body.String())
	}
	if strings.Contains(r.Body.String(), "PRIVATE_L2_ONLY") {
		t.Fatalf("default L0 search exposed full content: %s", r.Body.String())
	}
	r = get("/observations/recent?project=legacy-name")
	if r.Code != 200 || strings.Contains(r.Body.String(), "PRIVATE_L2_ONLY") || !strings.Contains(r.Body.String(), "PRIVATE_HTTP_UNBOUND") {
		t.Fatalf("default collection did not project L0: %d %s", r.Code, r.Body.String())
	}
	r = get("/project/current")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"project":""`) {
		t.Fatalf("metadata route inferred project identity: %d %s", r.Code, r.Body.String())
	}
}

func TestHTTPUnknownProjectRejectedBeforeBudget(t *testing.T) {
	st := newServerTestStore(t)
	srv := New(st, 0)
	for _, path := range []string{"/search?q=needle", "/observations?detail=l0", "/observations/recent?detail=l0", "/stats?", "/context?mode=index", "/review?", "/conflicts?", "/conflicts/stats?", "/sessions/recent?", "/prompts/recent?"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path+"&project=prj-UNKNOWN", nil))
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), "unknown_project") {
			t.Errorf("unknown project %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if len(srv.readBudgets) != 0 {
		t.Fatal("unknown project allocated a read budget")
	}
}

func TestHTTPKnownEmptyRegisteredProjectAndExplicitAll(t *testing.T) {
	st := newServerTestStore(t)
	project, err := st.CreateRegisteredProject("empty-known")
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, 0).Handler()
	for _, selected := range []string{"empty-known", project.ID} {
		for _, path := range []string{"/search?q=needle", "/observations?detail=l0", "/stats?", "/context?mode=index", "/review?", "/conflicts?"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", path+"&project="+selected, nil))
			if rec.Code != 200 {
				t.Errorf("known empty project %s: %d %s", path, rec.Code, rec.Body.String())
			}
		}
	}
	for _, path := range []string{"/search?q=needle", "/observations?detail=l0", "/stats?", "/context?mode=index", "/review?", "/conflicts?", "/conflicts/deferred?"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path+"&all_projects=true", nil))
		if rec.Code != 200 {
			t.Errorf("explicit all %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestHTTPDeferredReadsRequireExplicitAllProjects(t *testing.T) {
	st, raw := conflictsTestStore(t)
	if err := st.CreateSession("visible", "visible", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	seedDeferredHTTP(t, raw, "hidden-deferred", `{"content":"HIDDEN_DEFERRED_BODY"}`, 0, "deferred")
	h := New(st, 0).Handler()
	for _, test := range []struct {
		path   string
		status int
	}{
		{"/conflicts/deferred?project=visible", 400},
		{"/conflicts/deferred?all_projects=true", 200},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", test.path, nil))
		if rec.Code != test.status {
			t.Errorf("deferred scope %s: %d %s", test.path, rec.Code, rec.Body.String())
		}
		if test.status != 200 && strings.Contains(rec.Body.String(), "HIDDEN_DEFERRED_BODY") {
			t.Fatal("scoped deferred response exposed a global payload")
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/conflicts/stats?project=visible", nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"deferred":`) || !strings.Contains(rec.Body.String(), `"deferred_scope":"all_projects_only"`) {
		t.Fatalf("scoped stats mixed in global deferred counts: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPSearchOffsetRejectsUnsupportedPagination(t *testing.T) {
	st := newServerTestStore(t)
	srv := New(st, 0)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/search?q=needle&offset=1&all_projects=true", nil))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_offset") || len(srv.readBudgets) != 0 {
		t.Fatalf("unsupported pagination: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/search?q=needle&offset=0&all_projects=true", nil))
	if rec.Code != 200 {
		t.Fatalf("zero offset: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPReadQueryValidationBeforeSelectionAndBudget(t *testing.T) {
	st := newServerTestStore(t)
	if err := st.CreateSession("query", "visible", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := st.AddObservation(store.AddObservationParams{SessionID: "query", Project: "visible", Type: "decision", Title: "Strict query fixture", Content: "query validation body"})
	if err != nil {
		t.Fatal(err)
	}
	budget, err := st.CreateReadBudget("visible", 8000)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECALLDOCK_READ_BUDGET_ID", budget.ID)
	srv := New(st, 0)
	for _, path := range []string{
		"/search?q=fixture&limit=abc", "/search?q=fixture&limit=-5", "/search?q=fixture&limit=0", "/search?q=fixture&limit=501",
		"/search?q=fixture&limit=1&limit=2", "/search?q=fixture&q=second", "/search?q=fixture&bogus=true",
		"/search?q=fixture&offset=-1", "/search?q=fixture&offset=10001", "/search?q=fixture&max_depth=abc",
		"/search?q=fixture&token_budget=-1", "/search?q=fixture&token_budget=999999999999999999999",
		"/search?q=fixture&trace=not-bool", "/search?q=fixture&include_stale=not-bool", "/search?q=fixture&all_projects=not-bool",
		"/search?q=fixture&profile=bogus", "/search?q=fixture&match_mode=bogus", "/search?q=fixture&strategy=bogus",
		"/search?q=fixture&detail=l2", "/search?q=fixture&scope=bogus", "/search?q=fixture&type=bogus", "/search?q=fixture&root_path=../escape",
		"/search?q=fixture&since=bad", "/search?q=fixture&until=bad", "/search?q=fixture&since=2026-11-16&until=2026-11-15",
		"/search?q=fixture&project=", "/search?q=fixture&project=visible&project=visible", "/search?q=fixture&project=visible&all_projects=true",
		"/observations?limit=-1", "/observations?sort=updated_at:desc", "/observations?scope=bogus", "/observations?detail=bogus",
		"/observations?offset=1", "/context?mode=bogus", "/context?token_budget=abc", "/context?mode=full&token_budget=10",
		"/review?status=bogus", "/review?limit=-1", "/conflicts?status=bogus", "/conflicts?offset=-1", "/conflicts?limit=501", "/conflicts?since=bad",
		"/conflicts/deferred?status=bogus", "/stats?all_projects=true&all_projects=false", "/stats?project=visible;all_projects=true",
		fmt.Sprintf("/timeline?observation_id=%d&before=-1", id), fmt.Sprintf("/timeline?observation_id=%d&after=abc", id),
		fmt.Sprintf("/timeline?observation_id=%d&id=%d", id, id), fmt.Sprintf("/timeline?observation_id=%d&id=9999", id),
		"/timeline?observation_id=0", "/timeline?id=abc", fmt.Sprintf("/observations/%d?detail=bogus", id),
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
			if rec.Code != 400 {
				t.Errorf("invalid query: %d %s", rec.Code, rec.Body.String())
			}
			after, err := st.ReadBudget(budget.ID)
			if err != nil || after.Used != budget.Used {
				t.Fatalf("invalid query charged budget: %#v %v", after, err)
			}
		})
	}
}

func TestHTTPTimelineAliasesKeepScope(t *testing.T) {
	st := newServerTestStore(t)
	for _, project := range []string{"visible", "hidden"} {
		if err := st.CreateSession(project, project, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddObservation(store.AddObservationParams{SessionID: "visible", Project: "visible", Type: "decision", Title: "Prior neighbor", Content: "neighbor body"}); err != nil {
		t.Fatal(err)
	}
	id, err := st.AddObservation(store.AddObservationParams{SessionID: "visible", Project: "visible", Type: "decision", Title: "Timeline alias fixture", Content: "visible body"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, 0).Handler()
	for _, alias := range []string{"id", "observation_id"} {
		for _, project := range []string{"visible", "hidden"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", fmt.Sprintf("/timeline?%s=%d&project=%s&before=0&after=0", alias, id, project), nil))
			want := 200
			if project == "hidden" {
				want = 403
			}
			if rec.Code != want {
				t.Errorf("alias %s project %s: %d %s", alias, project, rec.Code, rec.Body.String())
			}
			if want == 200 && strings.Contains(rec.Body.String(), "neighbor body") {
				t.Fatal("zero timeline window silently fell back to default neighbors")
			}
		}
	}
}

func TestHTTPCollectionsNeverImplicitlyCrossProjects(t *testing.T) {
	st := newServerTestStore(t)
	t.Chdir(t.TempDir())
	h := New(st, 0).Handler()
	for _, path := range []string{"/conflicts", "/conflicts/stats", "/conflicts/deferred", "/observations/1", "/sessions/missing"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "project_not_selected") {
			t.Errorf("unselected %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestHTTPSearchPassesSupportedFilters(t *testing.T) {
	st, raw := conflictsTestStore(t)
	if err := st.CreateSession("filters", "visible", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	fixture := []struct {
		title, class, path, created string
		stale                       bool
	}{
		{"Wanted result", "project", "project/topic/wanted", "2026-11-15 12:00:00", false},
		{"Operational result", "operational", "operational/topic/wanted", "2026-11-15 12:00:00", false},
		{"Other path result", "project", "project/topic/other", "2026-11-15 12:00:00", false},
		{"Old result", "project", "project/topic/wanted", "2026-11-14 12:00:00", false},
		{"Future result", "project", "project/topic/wanted", "2026-11-16 12:00:00", false},
		{"Stale result", "project", "project/topic/wanted", "2026-11-15 12:00:00", true},
	}
	for _, item := range fixture {
		id, err := st.AddObservation(store.AddObservationParams{SessionID: "filters", Project: "visible", Type: "decision", Title: item.title, Content: "supported filter needle " + item.title, MemoryClass: item.class, MemoryPath: item.path})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec("UPDATE observations SET created_at=?, stale_at=? WHERE id=?", item.created, map[bool]any{true: "2026-11-15 13:00:00", false: nil}[item.stale], id); err != nil {
			t.Fatal(err)
		}
	}
	h := New(st, 0).Handler()
	base := "/search?q=needle&project=visible&strategy=flat&detail=l0&profile=semantic&root_path=project/topic/wanted&since=2026-11-15&until=2026-11-15"
	for _, stale := range []bool{false, true} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", base+"&include_stale="+strconv.FormatBool(stale), nil))
		var response struct {
			Results []map[string]any `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		want := 1
		if stale {
			want = 2
		}
		if rec.Code != 200 || len(response.Results) != want || !strings.Contains(rec.Body.String(), "Wanted result") {
			t.Errorf("filters include_stale=%v: %d %s", stale, rec.Code, rec.Body.String())
		}
	}
}

func TestHTTPConflictsCannotExposeForeignEndpoint(t *testing.T) {
	st, raw := conflictsTestStore(t)
	visibleSource, visibleTarget := seedConflictsSession(t, st, "visible")
	hiddenSource, hiddenTarget := seedConflictsSession(t, st, "hidden")
	if _, err := st.SaveRelation(store.SaveRelationParams{SyncID: "visible-relation", SourceID: visibleSource, TargetID: visibleTarget}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveRelation(store.SaveRelationParams{SyncID: "hidden-relation", SourceID: hiddenSource, TargetID: hiddenTarget}); err != nil {
		t.Fatal(err)
	}
	var hiddenID int64
	if err := raw.QueryRow("SELECT id FROM memory_relations WHERE sync_id='hidden-relation'").Scan(&hiddenID); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	New(st, 0).Handler().ServeHTTP(rec, httptest.NewRequest("GET", fmt.Sprintf("/conflicts/%d?project=visible", hiddenID), nil))
	if rec.Code != 403 || strings.Contains(rec.Body.String(), "source_snippet") {
		t.Fatalf("foreign relation endpoint exposed: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := raw.Exec("UPDATE memory_relations SET target_id=? WHERE sync_id='visible-relation'", hiddenTarget); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	New(st, 0).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/conflicts?project=visible", nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "target_title") || !strings.Contains(rec.Body.String(), `"total":0`) {
		t.Fatalf("cross-project relation list exposed endpoint: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPPublicConflictsSinceIncludesSameDay(t *testing.T) {
	st, raw := conflictsTestStore(t)
	source, target := seedConflictsSession(t, st, "visible")
	if _, err := st.SaveRelation(store.SaveRelationParams{SyncID: "same-day", SourceID: source, TargetID: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE memory_relations SET created_at='2026-11-15 12:00:00' WHERE sync_id='same-day'"); err != nil {
		t.Fatal(err)
	}
	for _, since := range []string{"2026-11-15T00:00:00Z", "2026-11-15T08:00:00+08:00"} {
		rec := httptest.NewRecorder()
		New(st, 0).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/conflicts?project=visible&since="+url.QueryEscape(since), nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"total":1`) {
			t.Errorf("same day %s: %d %s", since, rec.Code, rec.Body.String())
		}
	}
}

func TestPublicHTTPExactResourceCannotContradictProjectSelector(t *testing.T) {
	s := newServerTestStore(t)
	if err := s.CreateSession("visible-session", "visible", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("hidden-session", "hidden", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(store.AddObservationParams{SessionID: "hidden-session", Project: "hidden", Type: "decision", Title: "Hidden title", Content: "HIDDEN_HTTP_RESOURCE_BODY"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, 0).Handler()
	for _, path := range []string{"/timeline?observation_id=" + strconv.FormatInt(id, 10) + "&project=visible", "/observations/" + strconv.FormatInt(id, 10) + "?project=visible", "/sessions/hidden-session?project=visible"} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 403 || !strings.Contains(r.Body.String(), "project_scope_mismatch") || strings.Contains(r.Body.String(), "HIDDEN_HTTP_RESOURCE_BODY") {
			t.Fatalf("foreign resource: %d %s", r.Code, r.Body.String())
		}
	}
}
