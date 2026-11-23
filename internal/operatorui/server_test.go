package operatorui

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	t.Chdir(t.TempDir())
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
	ui, err := New(Options{Store: s, Version: "test", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	return ui, s, "test-token"
}

func do(ui *Server, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Host = "127.0.0.1:7438"
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	ui.Handler().ServeHTTP(rec, req)
	return rec
}

func TestUISearchOnlyRecordsDeliveredPage(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("readOnly=%t", readOnly), func(t *testing.T) {
			ui, s, token := newTestServer(t)
			ui.readOnly = readOnly
			if err := s.CreateSession("page-session", "page-project", t.TempDir()); err != nil {
				t.Fatal(err)
			}
			ids := make([]int64, 25)
			for i := range ids {
				id, err := s.AddObservationNew(store.AddObservationParams{SessionID: "page-session", Project: "page-project", Type: "note", Title: fmt.Sprintf("分页探测 %02d", i), Content: fmt.Sprintf("分页探测内容 %02d", i)})
				if err != nil {
					t.Fatal(err)
				}
				ids[i] = id
			}
			cookie := (&http.Cookie{Name: cookieName, Value: token}).String()
			recalls := map[int64]int{}
			for page := 1; page <= 3; page++ {
				rec := do(ui, "GET", fmt.Sprintf("/search?q=%s&page=%d", url.QueryEscape("分页探测"), page), "", map[string]string{"Cookie": cookie})
				if rec.Code != http.StatusOK {
					t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
				}
				if page == 1 && !strings.Contains(rec.Body.String(), "page=2") {
					t.Fatal("probe did not create next-page link")
				}
				delivered := 0
				for _, id := range ids {
					if strings.Contains(rec.Body.String(), fmt.Sprintf(`href="/memory/%d"`, id)) {
						delivered++
						if !readOnly {
							recalls[id]++
						}
					}
					obs, err := s.GetObservation(id)
					if err != nil {
						t.Fatal(err)
					}
					want := recalls[id]
					if obs.RecallCount != want || (want == 0 && obs.LastRecallAt != nil) {
						t.Fatalf("page=%d id=%d recall=%d last=%v want=%d", page, id, obs.RecallCount, obs.LastRecallAt, want)
					}
				}
				if want := []int{20, 5, 0}[page-1]; delivered != want {
					t.Fatalf("page=%d delivered=%d want=%d", page, delivered, want)
				}
			}
		})
	}
}

func TestUIReadOnlySearchDoesNotPersistCJKProjection(t *testing.T) {
	ui, s, token := newTestServer(t)
	ui.readOnly = true
	if err := s.CreateSession("readonly-cjk", "readonly-project", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservationNew(store.AddObservationParams{SessionID: "readonly-cjk", Project: "readonly-project", Type: "note", Title: "只读中文检索", Content: "只读中文检索内容"})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", identity.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fingerprint := func() [3]int {
		t.Helper()
		var values [3]int
		for i, table := range []string{"observations_cjk_dirty", "observations_cjk_fts"} {
			var exists int
			if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name=?", table).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			values[i] = -1
			if exists != 0 {
				if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&values[i]); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := db.QueryRow("SELECT count(*) FROM store_metadata WHERE key='cjk_bm25_v1'").Scan(&values[2]); err != nil {
			t.Fatal(err)
		}
		return values
	}
	before := fingerprint()
	if before[0] == 0 {
		t.Fatal("fixture must leave a pending CJK projection")
	}
	rec := do(ui, "GET", "/search?q="+url.QueryEscape("只读中文检索"), "", map[string]string{"Cookie": (&http.Cookie{Name: cookieName, Value: token}).String()})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), fmt.Sprintf(`href="/memory/%d"`, id)) {
		t.Fatalf("readonly CJK search failed to deliver observation: status=%d", rec.Code)
	}
	if after := fingerprint(); before != after {
		t.Fatalf("projection changed: before=%v after=%v", before, after)
	}
	obs, err := s.GetObservation(id)
	if err != nil {
		t.Fatal(err)
	}
	if obs.RecallCount != 0 || obs.LastRecallAt != nil {
		t.Fatal("readonly search persisted recall metadata")
	}
}

func TestUIProjectCookieValidatedAcrossPages(t *testing.T) {
	ui, s, token := newTestServer(t)
	project, err := s.CreateRegisteredProject("cookie fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("cookie-session", "legacy-cookie", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	cookie := (&http.Cookie{Name: cookieName, Value: token}).String()
	for _, value := range []string{"prj-UNKNOWN", "unknown-legacy", "legacy-cookie"} {
		for _, path := range []string{"/", "/search?q=fixture", "/search?q=fixture&project=*", "/similar", "/candidates", "/doctor", "/archived"} {
			rec := do(ui, "GET", path, "", map[string]string{"Cookie": cookie + "; " + projectCookieName + "=" + value})
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "项目") || !strings.Contains(rec.Body.String(), "recalldock projects list") {
				t.Errorf("cookie=%q path=%s status=%d missing actionable project error", value, path, rec.Code)
			}
		}
	}
	for _, value := range []string{project.ID, "*"} {
		rec := do(ui, "GET", "/", "", map[string]string{"Cookie": cookie + "; " + projectCookieName + "=" + value})
		if rec.Code != http.StatusOK {
			t.Fatalf("valid cookie %q: %d", value, rec.Code)
		}
	}
	rec := do(ui, "GET", "/project?value=*&next=//invalid", "", map[string]string{"Cookie": cookie + "; " + projectCookieName + "=prj-UNKNOWN"})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("invalid cookie recovery: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestUIPinnedProjectCookieStillValidatesRegisteredIdentity(t *testing.T) {
	ui, s, token := newTestServer(t)
	ui.project = "prj-UNKNOWN"
	cookie := (&http.Cookie{Name: cookieName, Value: token}).String()
	for _, path := range []string{"/", "/project?value=prj-UNKNOWN"} {
		rec := do(ui, "GET", path, "", map[string]string{"Cookie": cookie + "; " + projectCookieName + "=prj-UNKNOWN"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("stale pin path=%s status=%d", path, rec.Code)
		}
	}
	ui.project = "trusted-legacy"
	if err := s.CreateSession("trusted-session", ui.project, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	rec := do(ui, "GET", "/", "", map[string]string{"Cookie": cookie + "; " + projectCookieName + "=" + ui.project})
	if rec.Code != http.StatusOK {
		t.Fatalf("trusted legacy pin status=%d", rec.Code)
	}
}

func TestUITokenAndCookieAuth(t *testing.T) {
	ui, _, token := newTestServer(t)
	rec := do(ui, "GET", "/", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	rec = do(ui, "GET", "/?token=wrong", "", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	rec = do(ui, "GET", "/?token="+token, "", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("token entry: %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "token") {
		t.Fatalf("redirect leaks the token: %q", loc)
	}
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 || cookie[0].Name != cookieName {
		t.Fatalf("cookie missing: %v", cookie)
	}
	rec = do(ui, "GET", "/", "", map[string]string{"Cookie": cookie[0].String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie auth: %d", rec.Code)
	}
}

func TestUIRejectsForeignHostAndCSRF(t *testing.T) {
	ui, _, token := newTestServer(t)
	rec := do(ui, "GET", "/?token="+token, "", map[string]string{"Host": "evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign host: %d", rec.Code)
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}
	rec = do(ui, "POST", "/memory/1/archive", "expected=x&reason=y", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing csrf: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "请求校验失败") {
		t.Fatalf("csrf error page missing message: %s", rec.Body.String()[:200])
	}
}

func TestUIArchiveAndCandidates(t *testing.T) {
	ui, s, token := newTestServer(t)
	dir := t.TempDir()
	if err := s.CreateSession("ui-sess", "ui-proj", dir); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(store.AddObservationParams{
		SessionID: "ui-sess", Type: "note", Title: "UI <script>alert(1)</script>", Content: "body", Project: "ui-proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}

	rec := do(ui, "GET", "/memory/1", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("memory page: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("title was not HTML-escaped")
	}
	if !strings.Contains(rec.Body.String(), "&lt;script&gt;") {
		t.Fatal("expected escaped title in page")
	}

	form := url.Values{"csrf": {token}, "expected": {selected.SnapshotSHA256}, "reason": {"ui archive"}}
	rec = do(ui, "POST", "/memory/1/archive", form.Encode(), map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	obs, err := s.GetObservation(id)
	if err != nil {
		t.Fatal(err)
	}
	if obs.ArchivedAt == nil {
		t.Fatal("observation was not archived")
	}

	// Candidates: seed two observations through the operator approval API.
	if _, err := s.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{
		SessionID: "ui-sess", Project: "ui-proj",
		Candidates: []store.MemoryCandidateInput{{Category: "decision", Statement: "UI approval probe", Confidence: 0.9}},
	}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListMemoryCandidates("ui-proj", store.CandidateStatusReview, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("candidates=%v err=%v", list, err)
	}
	digest, err := store.MemoryCandidateDigest(&list[0])
	if err != nil {
		t.Fatal(err)
	}
	rec = do(ui, "GET", "/candidates", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), list[0].SyncID) {
		t.Fatalf("candidates page: %d", rec.Code)
	}
	form = url.Values{"csrf": {token}, "expected": {digest}, "reason": {"ui approve"}}
	rec = do(ui, "POST", "/candidates/"+list[0].SyncID+"/approve", form.Encode(), map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	promoted, err := s.GetMemoryCandidate(list[0].SyncID)
	if err != nil || promoted.Status != store.CandidateStatusPromoted {
		t.Fatalf("status=%v err=%v", promoted, err)
	}
}

func TestUIListenLoopbackOnly(t *testing.T) {
	ui, _, _ := newTestServer(t)
	if _, err := ui.Listen("0.0.0.0:0"); err == nil {
		t.Fatal("non-loopback bind must be rejected")
	}
}

func TestUISearchFiltersAndBackupCard(t *testing.T) {
	ui, s, token := newTestServer(t)
	dir := t.TempDir()
	if err := s.CreateSession("search-sess", "search-proj", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{
		SessionID: "search-sess", Type: "note", Title: "temporal ui note",
		Content: "the quick brown fox", Project: "search-proj",
	}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}
	rec := do(ui, "GET", "/search?q=brown+fox&project=*", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "temporal ui note") {
		t.Fatal("search did not return the observation")
	}
	if !strings.Contains(rec.Body.String(), "全部项目") {
		t.Fatal("project selector missing")
	}
	// A future since bound filters everything out instead of erroring.
	rec = do(ui, "GET", "/search?q=brown+fox&since=2099-01-01", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "temporal ui note") {
		t.Fatalf("since filter page: %d", rec.Code)
	}
	// Invalid bounds surface as a readable error page.
	rec = do(ui, "GET", "/search?q=brown&since=not-a-date", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid time bound") {
		t.Fatalf("invalid bound: %d", rec.Code)
	}

	// Backup card: a synthetic backup file in the store's data dir is listed.
	identity, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	backupDir := identity.DataDir + "/backups"
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupDir+"/recalldock-backup-test.db", []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec = do(ui, "GET", "/", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "上次备份") {
		t.Fatalf("dashboard backup card: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "份") {
		t.Fatal("backup count missing")
	}
}

func TestUIScopeSwitcher(t *testing.T) {
	ui, s, token := newTestServer(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	a, err := s.CreateRegisteredProject("scope A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateRegisteredProject("scope B")
	if err != nil {
		t.Fatal(err)
	}
	for root, project := range map[string]string{dirA: a.ID, dirB: b.ID} {
		if _, err := s.BindProjectRoot(project, root); err != nil {
			t.Fatal(err)
		}
	}
	for i, project := range []string{a.ID, b.ID} {
		session := "scope-sess-" + strconv.Itoa(i)
		if err := s.CreateSession(session, project, []string{dirA, dirB}[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{
			SessionID: session, Project: project,
			Candidates: []store.MemoryCandidateInput{{Category: "decision", Statement: "scope probe " + strconv.Itoa(i), Confidence: 0.5}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}

	rec := do(ui, "GET", "/project?value="+b.ID, "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("switch: %d", rec.Code)
	}
	var scopeCookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == projectCookieName {
			scopeCookie = c.Value
		}
	}
	if scopeCookie != b.ID {
		t.Fatalf("scope cookie=%q", scopeCookie)
	}
	rec = do(ui, "GET", "/candidates", "", map[string]string{"Cookie": cookie.String() + "; " + projectCookieName + "=" + scopeCookie})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "scope probe 1") || strings.Contains(rec.Body.String(), "scope probe 0") {
		t.Fatalf("scoped candidates page: %d", rec.Code)
	}

	if rec := do(ui, "GET", "/project?value=prj-does-not-exist", "", map[string]string{"Cookie": cookie.String()}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown project: %d", rec.Code)
	}
	rec = do(ui, "GET", "/project?value=*&next=//evil.example", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("open redirect not blocked: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestUIBatchReviewAndBackupsPage(t *testing.T) {
	ui, s, token := newTestServer(t)
	dir := t.TempDir()
	if err := s.CreateSession("batch-sess", "batch-proj", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{
		SessionID: "batch-sess", Project: "batch-proj",
		Candidates: []store.MemoryCandidateInput{
			{Category: "decision", Statement: "batch probe one", Confidence: 0.5},
			{Category: "decision", Statement: "batch probe two", Confidence: 0.5},
		},
	}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListMemoryCandidates("batch-proj", store.CandidateStatusReview, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	digests := map[string]string{}
	for i := range list {
		digests[list[i].SyncID], err = store.MemoryCandidateDigest(&list[i])
		if err != nil {
			t.Fatal(err)
		}
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}

	form := url.Values{"csrf": {token}, "reason": {"批处理"}, "ids": {list[0].SyncID, list[1].SyncID}}
	form.Set("expected_"+list[0].SyncID, digests[list[0].SyncID])
	form.Set("expected_"+list[1].SyncID, "0000000000000000000000000000000000000000000000000000000000000000")
	rec := do(ui, "POST", "/candidates/approve-batch", form.Encode(), map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String()[:200])
	}
	page := rec.Body.String()
	if !strings.Contains(page, "批准 1 条") || !strings.Contains(page, "失败 1 条") {
		t.Fatalf("batch summary missing: %s", page[:300])
	}
	first, err := s.GetMemoryCandidate(list[0].SyncID)
	if err != nil || first.Status != store.CandidateStatusPromoted {
		t.Fatalf("first candidate=%v err=%v", first, err)
	}
	second, err := s.GetMemoryCandidate(list[1].SyncID)
	if err != nil || second.Status != store.CandidateStatusReview {
		t.Fatalf("stale candidate must stay in review: %v err=%v", second, err)
	}

	// Missing selection / reason are rejected before any write.
	rec = do(ui, "POST", "/candidates/reject-batch", url.Values{"csrf": {token}, "reason": {"x"}}.Encode(), map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty selection: %d", rec.Code)
	}

	// Backups page lists both stores' files.
	identity, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"backups", "operations-backups"} {
		if err := os.MkdirAll(identity.DataDir+"/"+sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(identity.DataDir+"/"+sub+"/"+sub+"-probe.db", []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec = do(ui, "GET", "/backups", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "backups-probe.db") || !strings.Contains(rec.Body.String(), "operations-backups-probe.db") {
		t.Fatalf("backups page: %d", rec.Code)
	}
}

func TestUIPaginationAndScopedDashboard(t *testing.T) {
	ui, s, token := newTestServer(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	a, err := s.CreateRegisteredProject("paged A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateRegisteredProject("paged B")
	if err != nil {
		t.Fatal(err)
	}
	for root, project := range map[string]string{dirA: a.ID, dirB: b.ID} {
		if _, err := s.BindProjectRoot(project, root); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession("paged-sess-a", a.ID, dirA); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("paged-sess-b", b.ID, dirB); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		if _, err := s.AddObservation(store.AddObservationParams{
			SessionID: "paged-sess-a", Type: "note", Title: "paged marker", Content: "body " + strconv.Itoa(i), Project: a.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddObservation(store.AddObservationParams{
		SessionID: "paged-sess-b", Type: "note", Title: "other project", Content: "body", Project: b.ID,
	}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}
	scope := &http.Cookie{Name: projectCookieName, Value: a.ID}
	header := map[string]string{"Cookie": cookie.String() + "; " + scope.String()}

	rec := do(ui, "GET", "/search?q=paged+marker&project="+a.ID, "", header)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "下一页") {
		t.Fatalf("expected next page link: %d", rec.Code)
	}
	rec = do(ui, "GET", "/search?q=paged+marker&project="+a.ID+"&page=2", "", header)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "上一页") {
		t.Fatalf("expected prev page link: %d", rec.Code)
	}
	rec = do(ui, "GET", "/", "", header)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<div class="k">长期记忆</div><div class="v">25</div>`) {
		t.Fatalf("dashboard is not scoped: %s", rec.Body.String()[:400])
	}
}

func TestUIProjectsDirectoryStatusAndBackupChecksums(t *testing.T) {
	ui, s, token := newTestServer(t)
	bindingRoot := t.TempDir()
	project, err := s.CreateRegisteredProject("dir status")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(project.ID, bindingRoot); err != nil {
		t.Fatal(err)
	}
	t.Chdir(bindingRoot)
	cookie := &http.Cookie{Name: cookieName, Value: token}

	rec := do(ui, "GET", "/projects", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("projects: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, bindingRoot) || !strings.Contains(body, project.ID) || !strings.Contains(body, "已绑定") {
		t.Fatalf("directory status missing: %s", body[:400])
	}

	// Backup checksum verification streams a bounded digest.
	identity, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := identity.DataDir + "/backups"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("backup probe")
	if err := os.WriteFile(dir+"/backup-probe.db", payload, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	rec = do(ui, "GET", "/backups?verify=1", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("checksum page: %d", rec.Code)
	}
}

func TestUIConflictsPage(t *testing.T) {
	ui, s, token := newTestServer(t)
	dir := t.TempDir()
	if err := s.CreateSession("conf-sess", "conf-proj", dir); err != nil {
		t.Fatal(err)
	}
	src, err := s.AddObservation(store.AddObservationParams{SessionID: "conf-sess", Type: "decision", Title: "conflict source", Content: "a", Project: "conf-proj"})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := s.AddObservation(store.AddObservationParams{SessionID: "conf-sess", Type: "decision", Title: "conflict target", Content: "b", Project: "conf-proj"})
	if err != nil {
		t.Fatal(err)
	}
	srcObs, _ := s.GetObservation(src)
	dstObs, _ := s.GetObservation(dst)
	if _, err := s.SaveRelation(store.SaveRelationParams{SyncID: "rel-ui-probe", SourceID: srcObs.SyncID, TargetID: dstObs.SyncID}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}
	rec := do(ui, "GET", "/conflicts", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "conflict source") || !strings.Contains(rec.Body.String(), "conflict target") {
		t.Fatalf("conflicts page: %d", rec.Code)
	}
}

func TestUIReadOnlyModeAndArchivedPage(t *testing.T) {
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
	ui, err := New(Options{Store: s, Version: "test", Token: "ro-token", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := s.CreateSession("ro-sess", "ro-proj", dir); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(store.AddObservationParams{SessionID: "ro-sess", Type: "note", Title: "read only probe", Content: "body", Project: "ro-proj"})
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.CreateRegisteredProject("ro proj")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(project.ID, dir); err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: "ro-token"}
	form := url.Values{"csrf": {"ro-token"}, "expected": {selected.SnapshotSHA256}, "reason": {"ro"}}
	rec := do(ui, "POST", "/memory/1/archive", form.Encode(), map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "只读模式") {
		t.Fatalf("read-only mutation: %d", rec.Code)
	}
	rec = do(ui, "GET", "/candidates", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "批准所选") {
		t.Fatalf("read-only candidates page leaked the form: %d", rec.Code)
	}
	// Read-only still allows browsing the archive.
	if _, err := s.UpdateObservationCAS(id, selected.SnapshotSHA256, store.UpdateObservationParams{Archived: boolPtr(true)}, "archive probe", dir); err != nil {
		t.Fatal(err)
	}
	ui.project = "ro-proj"
	scope := &http.Cookie{Name: projectCookieName, Value: ui.project}
	rec = do(ui, "GET", "/archived", "", map[string]string{"Cookie": cookie.String() + "; " + scope.String()})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "read only probe") {
		t.Fatalf("archived page: %d", rec.Code)
	}
}

func boolPtr(value bool) *bool { return &value }

func TestUIEditMemoryWithCAS(t *testing.T) {
	ui, s, token := newTestServer(t)
	dir := t.TempDir()
	if err := s.CreateSession("edit-sess", "edit-proj", dir); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(store.AddObservationParams{SessionID: "edit-sess", Type: "note", Title: "editable", Content: "old body", Project: "edit-proj"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}
	rec := do(ui, "GET", "/memory/1/edit", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), selected.SnapshotSHA256) || !strings.Contains(rec.Body.String(), "old body") {
		t.Fatalf("edit form: %d", rec.Code)
	}
	form := url.Values{"csrf": {token}, "expected": {selected.SnapshotSHA256}, "reason": {"fix typo"}, "title": {"editable"}, "abstract": {"new abstract"}, "content": {"new body"}}
	rec = do(ui, "POST", "/memory/1/update", form.Encode(), map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String()[:300])
	}
	obs, err := s.GetObservation(id)
	if err != nil || obs.Content != "new body" || obs.RevisionCount < 2 {
		t.Fatalf("obs=%+v err=%v", obs, err)
	}
	// A stale page (same digest again) must be rejected without overwriting.
	form.Set("content", "second overwrite")
	rec = do(ui, "POST", "/memory/1/update", form.Encode(), map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale update must conflict: %d", rec.Code)
	}
	obs, err = s.GetObservation(id)
	if err != nil || obs.Content != "new body" {
		t.Fatalf("stale update changed content: %+v", obs)
	}
	// Missing reason is refused before touching the store.
	refreshed, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	noReason := url.Values{"csrf": {token}, "expected": {refreshed.SnapshotSHA256}, "content": {"x"}}
	rec = do(ui, "POST", "/memory/1/update", noReason.Encode(), map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing reason: %d", rec.Code)
	}
}

func TestUISearchHighlightsQuery(t *testing.T) {
	ui, s, token := newTestServer(t)
	dir := t.TempDir()
	if err := s.CreateSession("hl-sess", "hl-proj", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{
		SessionID: "hl-sess", Type: "note", Title: "Brown fox <script>alert(1)</script>",
		Content: "the quick brown fox", Project: "hl-proj",
	}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}
	rec := do(ui, "GET", "/search?q=brown", "", map[string]string{"Cookie": cookie.String()})
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d", rec.Code)
	}
	if !strings.Contains(body, `<mark class="hl">Brown</mark>`) && !strings.Contains(body, `<mark class="hl">brown</mark>`) {
		t.Fatalf("query term not highlighted: %s", body[:400])
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("highlight must not bypass HTML escaping")
	}
}

func TestUISecurityHeadersAndOpenRedirect(t *testing.T) {
	ui, _, token := newTestServer(t)
	rec := do(ui, "GET", "/?token="+token, "", nil)
	headers := rec.Header()
	csp := headers.Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self'") {
		t.Fatalf("CSP must allow the embedded logo: %q", csp)
	}
	if headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff header")
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatal("token-bearing pages must not be cached")
	}

	cookie := &http.Cookie{Name: cookieName, Value: token}
	// A backslash is normalised by browsers to a slash, so "/\evil.example"
	// would become a protocol-relative redirect.
	rec = do(ui, "GET", `/project?value=*&next=/%5Cevil.example`, "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("backslash redirect not blocked: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = do(ui, "GET", "/project?value=*&next=//evil.example", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("protocol-relative redirect not blocked: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = do(ui, "GET", "/project?value=*&next=/projects", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/projects" {
		t.Fatalf("local redirect lost: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestUIUnknownPathAndBadSearchFilter(t *testing.T) {
	ui, _, token := newTestServer(t)
	cookie := &http.Cookie{Name: cookieName, Value: token}
	rec := do(ui, "GET", "/nope", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "页面不存在") {
		t.Fatalf("styled 404 missing: %d", rec.Code)
	}

	// An unregistered project filter is refused but keeps the form usable.
	rec = do(ui, "GET", "/search?q=probe&project=prj-deadbeef", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown project filter: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "未知项目筛选") || !strings.Contains(body, `name="q"`) {
		t.Fatalf("filter error should keep the search form: %s", body[:400])
	}

	// Invalid time bounds are reported inline instead of replacing the page.
	rec = do(ui, "GET", "/search?q=probe&since=not-a-date", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid time bound") {
		t.Fatalf("invalid bound: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `value="probe"`) {
		t.Fatal("invalid bound should preserve the query in the form")
	}
}

func TestUIReadOnlyScopeSwitchAndCandidateList(t *testing.T) {
	t.Chdir(t.TempDir())
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
	ui, err := New(Options{Store: s, Version: "test", Token: "ro-token", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := s.CreateSession("ro-sess", "ro-proj", dir); err != nil {
		t.Fatal(err)
	}
	result, err := s.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{
		SessionID: "ro-sess", Project: "ro-proj",
		Candidates: []store.MemoryCandidateInput{{Category: "decision", Statement: "read only candidate probe", Confidence: 0.5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: "ro-token"}

	rec := do(ui, "GET", "/candidates", "", map[string]string{"Cookie": cookie.String()})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, result.CandidateIDs[0]) {
		t.Fatalf("read-only candidates must stay visible: %d", rec.Code)
	}
	if !strings.Contains(body, "read only candidate probe") {
		t.Fatal("read-only candidate statement missing")
	}
	if strings.Contains(body, "批准所选") {
		t.Fatal("read-only page must not render the decision form")
	}

	// Changing the browsing scope writes no knowledge, so it stays available.
	rec = do(ui, "GET", "/project?value=*", "", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("read-only scope switch: %d", rec.Code)
	}
	scopeSet := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == projectCookieName && c.Value == "*" {
			scopeSet = true
		}
	}
	if !scopeSet {
		t.Fatal("read-only scope switch did not set the scope cookie")
	}
}

func TestUIArchivedAllProjectsAndNilProject(t *testing.T) {
	ui, s, token := newTestServer(t)
	dir := t.TempDir()
	project, err := s.CreateRegisteredProject("archive group")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(project.ID, dir); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("arch-sess", project.ID, dir); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(store.AddObservationParams{
		SessionID: "arch-sess", Type: "note", Title: "archived probe", Content: "body", Project: project.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateObservationCAS(id, selected.SnapshotSHA256, store.UpdateObservationParams{Archived: boolPtr(true)}, "archive probe", dir); err != nil {
		t.Fatal(err)
	}
	// An observation with no project must render an em dash, not "<nil>".
	if _, err := s.AddObservation(store.AddObservationParams{
		SessionID: "arch-sess", Type: "note", Title: "no project probe", Content: "body", Project: "",
	}); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}
	scope := &http.Cookie{Name: projectCookieName, Value: "*"}

	rec := do(ui, "GET", "/archived", "", map[string]string{"Cookie": cookie.String() + "; " + scope.String()})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "archived probe") {
		t.Fatalf("all-projects archive grouping: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "archive group") {
		t.Fatal("archive group header should use the registered project name")
	}

	rec = do(ui, "GET", "/", "", map[string]string{"Cookie": cookie.String()})
	body := rec.Body.String()
	if strings.Contains(body, "&lt;nil&gt;") {
		t.Fatal("nil project rendered as <nil>")
	}
	if !strings.Contains(body, "no project probe") {
		t.Fatal("dashboard should list the unscoped observation")
	}
}

func TestUIReasonBoundsAndUnknownProjectPin(t *testing.T) {
	ui, s, token := newTestServer(t)
	dir := t.TempDir()
	project, err := s.CreateRegisteredProject("reason scope")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(project.ID, dir); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("reason-sess", project.ID, dir); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(store.AddObservationParams{
		SessionID: "reason-sess", Type: "note", Title: "reason probe", Content: "body", Project: project.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.SelectObservationForEdit(id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{
		SessionID: "reason-sess", Project: project.ID,
		Candidates: []store.MemoryCandidateInput{{Category: "decision", Statement: "reason candidate", Confidence: 0.5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: cookieName, Value: token}
	longReason := strings.Repeat("理", 400) // 1200 bytes, over the 512-byte bound

	check := func(label, target, body string) {
		t.Helper()
		rec := do(ui, "POST", target, body, map[string]string{"Cookie": cookie.String()})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d", label, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "理由") {
			t.Fatalf("%s: missing reason message", label)
		}
	}
	check("empty archive reason", "/memory/"+strconv.FormatInt(id, 10)+"/archive",
		url.Values{"csrf": {token}, "expected": {selected.SnapshotSHA256}}.Encode())
	check("long archive reason", "/memory/"+strconv.FormatInt(id, 10)+"/archive",
		url.Values{"csrf": {token}, "expected": {selected.SnapshotSHA256}, "reason": {longReason}}.Encode())
	check("long edit reason", "/memory/"+strconv.FormatInt(id, 10)+"/update",
		url.Values{"csrf": {token}, "expected": {selected.SnapshotSHA256}, "content": {"x"}, "reason": {longReason}}.Encode())
	check("long batch reason", "/candidates/reject-batch",
		url.Values{"csrf": {token}, "ids": {result.CandidateIDs[0]}, "reason": {longReason}}.Encode())

	// Empty or oversized pins are rejected at startup instead of showing a
	// permanently empty store.
	for _, pin := range []string{"prj-deadbeef", "prj-" + strings.Repeat("a", 32)} {
		if _, err := New(Options{Store: s, Token: "t", Project: pin}); err == nil {
			t.Fatalf("unknown project pin %q must be rejected", pin)
		}
	}
	// Legacy name-keyed scopes remain usable.
	if _, err := New(Options{Store: s, Token: "t", Project: "legacy-name"}); err != nil {
		t.Fatalf("legacy project name should stay accepted: %v", err)
	}
}
