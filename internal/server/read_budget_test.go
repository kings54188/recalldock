package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestHTTPSharedReadBudgetAndProgressiveContext(t *testing.T) {
	s := newServerTestStore(t)
	s.CreateSession("r", "budget", t.TempDir())
	id, _ := s.AddObservation(store.AddObservationParams{SessionID: "r", Project: "budget", Type: "decision", Title: "Readable index", Content: "FULL_HTTP_BODY_MARKER", Scope: "project"})
	budget, _ := s.CreateReadBudget("budget", 8000)
	t.Setenv("RECALLDOCK_READ_BUDGET_ID", budget.ID)
	h := New(s, 0).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	index := get("/context")
	if index.Code != 200 || !strings.Contains(index.Body.String(), `"mode":"index"`) || strings.Contains(index.Body.String(), "FULL_HTTP_BODY_MARKER") {
		t.Fatalf("context did not default to an index: %d %s", index.Code, index.Body.String())
	}
	head := httptest.NewRecorder()
	h.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/observations/"+strconv.FormatInt(id, 10), nil))
	if head.Code != 200 || head.Body.Len() != 0 {
		t.Fatalf("HEAD should not deliver/cache a body: %d %s", head.Code, head.Body.String())
	}
	full := get("/observations/" + strconv.FormatInt(id, 10))
	if full.Code != 200 || !strings.Contains(full.Body.String(), "FULL_HTTP_BODY_MARKER") {
		t.Fatalf("selected: %d %s", full.Code, full.Body.String())
	}
	duplicate := get("/observations/" + strconv.FormatInt(id, 10))
	if duplicate.Code != 200 || strings.Contains(duplicate.Body.String(), "FULL_HTTP_BODY_MARKER") || !strings.Contains(duplicate.Body.String(), "already_loaded") {
		t.Fatalf("duplicate: %d %s", duplicate.Code, duplicate.Body.String())
	}
	if foreign := get("/search?q=Readable&all_projects=TRUE"); foreign.Code != 429 {
		t.Fatal("activation scope widened")
	}
	before, _ := s.ReadBudget(budget.ID)
	if _, err := s.ConsumeReadBudget(budget.ID, "budget", before.Limit-before.Used-10, ""); err != nil {
		t.Fatal(err)
	}
	if invalid := get("/stats?read_budget_id=new&session_id=new"); invalid.Code != 400 {
		t.Fatal("unknown HTTP parameters bypassed strict validation")
	}
	if exhausted := get("/stats"); exhausted.Code != 429 {
		t.Fatal("HTTP parameters reset budget")
	}
}

func TestScopedHTTPBudgetDoesNotTreatUnownedContentAsOwned(t *testing.T) {
	if !foreignProject(map[string]any{"id": 1, "content": "unowned body"}, "project-one") {
		t.Fatal("missing ownership was treated as project scope")
	}
	if !foreignProject(map[string]any{"id": 1, "content": "unowned body", "project": nil}, "project-one") {
		t.Fatal("null ownership was treated as project scope")
	}
	if foreignProject(map[string]any{"id": 1, "content": "owned body", "project": "project-one"}, "project-one") {
		t.Fatal("correct owner rejected")
	}
}

func TestHTTPDoctorAllowsScopedDiagnosticCounts(t *testing.T) {
	st := newServerTestStore(t)
	if err := st.CreateSession("doctor", "diagnostic", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddObservation(store.AddObservationParams{SessionID: "doctor", Project: "diagnostic", Title: "Diagnostic fixture", Content: "Synthetic diagnostic body", Type: "decision"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	New(st, 0).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/doctor?project=diagnostic", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"class_counts"`) {
		t.Fatalf("scoped diagnostics: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPForeignProjectChecksOnlyStringOwnersAndOwnedBodies(t *testing.T) {
	for _, value := range []any{
		map[string]any{"class_counts": map[string]any{"project": float64(3)}},
		map[string]any{"project": nil},
		map[string]any{"content": "body", "project_id": "visible"},
	} {
		if foreignProject(value, "visible") {
			t.Fatalf("nonforeign diagnostic or owned body rejected: %#v", value)
		}
	}
	for _, value := range []any{
		map[string]any{"project_id": "hidden"},
		map[string]any{"project": "hidden"},
		map[string]any{"results": []any{map[string]any{"content": "body", "project_id": "hidden"}}},
		map[string]any{"results": []any{map[string]any{"content": "body", "project": float64(3)}}},
	} {
		if !foreignProject(value, "visible") {
			t.Fatalf("foreign or unowned body accepted: %#v", value)
		}
	}
}

func TestHTTPPinnedBudgetAllowsCanonicalProjectAlias(t *testing.T) {
	st := newServerTestStore(t)
	project, err := st.CreateRegisteredProject("visible-name")
	if err != nil {
		t.Fatal(err)
	}
	budget, err := st.CreateReadBudget(project.ID, 8000)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECALLDOCK_READ_BUDGET_ID", budget.ID)
	srv := New(st, 0)
	for _, selected := range []string{"visible-name", project.ID} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/stats?project="+selected, nil))
		if rec.Code != 200 {
			t.Errorf("pinned canonical alias %s: %d %s", selected, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/stats?project=unknown", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "unknown_project") {
		t.Fatalf("pinned unknown project: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPGlobalBudgetDoesNotAllowForeignProjectResults(t *testing.T) {
	st := newServerTestStore(t)
	if err := st.CreateSession("scope", "visible", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	budget, err := st.CreateReadBudget("", 8000)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECALLDOCK_READ_BUDGET_ID", budget.ID)
	srv := New(st, 0)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]any{"content": "hidden body", "project": "hidden"})
	})
	rec := httptest.NewRecorder()
	srv.boundedRead(next, rec, httptest.NewRequest("GET", "/stats?project=visible", nil))
	if rec.Code != 429 || strings.Contains(rec.Body.String(), "hidden body") {
		t.Fatalf("global budget widened selected project: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPStatsHonorsPinnedReadProject(t *testing.T) {
	st := newServerTestStore(t)
	for _, project := range []string{"visible", "hidden"} {
		if err := st.CreateSession(project, project, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddObservation(store.AddObservationParams{SessionID: project, Project: project, Title: project, Content: "Synthetic scope stat", Type: "decision"}); err != nil {
			t.Fatal(err)
		}
	}
	budget, err := st.CreateReadBudget("visible", 8000)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECALLDOCK_READ_BUDGET_ID", budget.ID)
	rec := httptest.NewRecorder()
	New(st, 0).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/stats", nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "hidden") || !strings.Contains(rec.Body.String(), `"total_observations":1`) {
		t.Fatalf("HTTP scoped stats: %d %s", rec.Code, rec.Body.String())
	}
}
