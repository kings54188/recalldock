// Package operatorui is a local, loopback-only operator surface for RecallDock.
// It is a separate trust surface from the agent HTTP/MCP transports: it opens
// the store with operator rights and requires a per-process bearer token that
// is only printed on the operator's terminal. It is never mounted by `serve`.
package operatorui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"recalldock.local/recalldock/internal/diagnostic"
	projectpkg "recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
)

type Options struct {
	Store   *store.Store
	Version string
	// Project pins the operator scope. Empty means "detect from the working
	// directory once; if unbound, the UI shows all projects read-only".
	Project string
	// Token is injectable for tests; empty generates a fresh 256-bit token.
	Token string
	Now   func() time.Time
	// ReadOnly rejects mutations and skips search recall accounting.
	ReadOnly bool
}

type Server struct {
	store    *store.Store
	version  string
	project  string
	token    string
	hint     string
	now      func() time.Time
	readOnly bool
	mux      *http.ServeMux
	tpl      *template.Template
}

type page struct {
	Page, Title, Version   string
	Project, ProjectLabel  string
	ProjectHint            string
	Notice, Error, CSRF    string
	Pending                int
	Identity               store.StoreIdentity
	Stats                  store.Stats
	Recent                 []store.Observation
	Query                  string
	Since, Until           string
	AllProjects            bool
	ProjectOptions         []projectOption
	Results                []store.SearchResult
	BackupTime, BackupSize string
	BackupCount            int
	ScopeValue             string
	ScopeOptions           []projectOption
	PageNumber             int
	HasPrev, HasNext       bool
	PrevURL, NextURL       string
	Obs                    *store.Observation
	Digest                 string
	History                []store.ObservationVersionSummary
	IsArchived             bool
	Candidates             []candidateView
	Backups                []backupView
	Relations              []store.RelationListItem
	CurrentDir             string
	CurrentProject         string
	CurrentBound           bool
	VerifyChecksums        bool
	VerifyNotice           string
	BatchResults           []batchResultView
	BatchApproved          int
	BatchRejected          int
	BatchFailed            int
	Projects               []store.RegisteredProject
	Bindings               map[string][]store.ProjectBindingState
	ProjectCounts          map[string]int
	Doctor                 diagnostic.Report
	DoctorAll              bool
	DoctorShowAll          bool
	Archived               []store.ArchivedMemorySummary
	ArchivedGroups         []archivedGroup
	ReadOnly               bool
	TruncatedNote          string
	SimilarGroups          []similarGroupView
	SimilarThreshold       float64
	SimilarScope           string
	Refine                 *refineView
	RefineTarget           int64
	RefineUpdated          bool
	RefineAbsorbed         int
	RefineFailures         []refineFailureView
}

// archivedGroup is one project's archived index; the "all projects" view
// renders one section per registered identity instead of refusing to work.
type archivedGroup struct {
	Project string
	Name    string
	Items   []store.ArchivedMemorySummary
}

type projectOption struct {
	Value    string
	Label    string
	Selected bool
}

type batchResultView struct {
	ID, Status, Error string
}

type backupView struct {
	Name, Kind, ModTime, Size string
	SHA256                    string
}

type candidateView struct {
	ID, Category, Origin, Preview, ExpiresAt, Digest, Project string
}

// similarGroupView is one read-only consolidation hint. SourceIDs always
// includes the suggested target; the store skips it when building a draft.
type similarGroupView struct {
	Score        string
	ScoreValue   float64
	MemberCount  int
	Project      string
	ProjectLabel string
	TargetID     int64
	Members      []similarMemberView
}

type similarMemberView struct {
	ID        int64
	Title     string
	Preview   string
	Suggested bool
}

type refineView struct {
	TargetID    int64
	TargetTitle string
	Project     string
	Digest      string
	Content     string
	Sources     []similarMemberView
	Skipped     []skippedSourceView
	Notes       []string
}

type skippedSourceView struct {
	ID     int64
	Reason string
}

type refineFailureView struct {
	ID     int64
	Reason string
}

const (
	cookieName        = "recalldock_ui"
	projectCookieName = "recalldock_ui_project"
)

func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("operator UI requires a store")
	}
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("generate operator token: %w", err)
		}
		token = hex.EncodeToString(buf)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	project := strings.TrimSpace(opts.Project)
	if project != "" && strings.HasPrefix(project, "prj-") {
		registered := false
		if projects, err := opts.Store.ListRegisteredProjects(); err == nil {
			for _, item := range projects {
				if item.ID == project {
					registered = true
					break
				}
			}
		}
		if !registered {
			// A typo used to boot a UI whose every page was silently empty.
			return nil, fmt.Errorf("项目 %q 未注册：先运行 recalldock projects list 确认，或省略 --project 让界面自动检测", project)
		}
	}
	hint := ""
	if project == "" {
		if cwd, err := os.Getwd(); err == nil {
			if detected := opts.Store.DetectProject(cwd); detected.Error == nil && detected.Project != "" {
				// A git remote/root name is only a hint: the CLI deliberately
				// keeps such directories unbound so reads cannot silently hit
				// another project's history. Adopt it only for real scopes.
				switch detected.Source {
				case projectpkg.SourceRegistered, projectpkg.SourceWorktreeMain, projectpkg.SourceConfig:
					project = detected.Project
				default:
					hint = detected.Project + " (" + detected.Source + ")"
				}
			}
		}
	}
	tpl := template.Must(template.New("layout").Funcs(template.FuncMap{
		"fmtTime": func(value string) string {
			if len(value) >= 19 {
				return strings.Replace(value[:19], "T", " ", 1)
			}
			return value
		},
		"ago":       func(value string) string { return humanAgo(value, now()) },
		"highlight": highlightHTML,
		"clip": func(value string, limit int) string {
			runes := []rune(strings.TrimSpace(value))
			if len(runes) <= limit {
				return string(runes)
			}
			return string(runes[:limit]) + "…"
		},
		"project": func(value *string) string {
			if value == nil || strings.TrimSpace(*value) == "" {
				return "—"
			}
			return *value
		},
		"bytes":     func(value int) string { return humanBytes(int64(value)) },
		"shortsha":  shortSHA,
		"binding":   bindingLabel,
		"nextstep":  nextStepText,
		"category":  categoryLabel,
		"origin":    originLabel,
		"shortid":   shortID,
		"hasPrefix": strings.HasPrefix,
	}).Parse(layoutSrc))
	s := &Server{store: opts.Store, version: opts.Version, project: project, token: token, hint: hint, now: now, readOnly: opts.ReadOnly, tpl: tpl}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET /{$}", s.handleDashboard)
	s.mux.HandleFunc("GET /search", s.handleSearch)
	s.mux.HandleFunc("GET /memory/{id}", s.handleMemory)
	s.mux.HandleFunc("GET /memory/{id}/edit", s.handleEditForm)
	s.mux.HandleFunc("POST /memory/{id}/update", s.handleUpdate)
	s.mux.HandleFunc("POST /memory/{id}/archive", s.handleArchive)
	s.mux.HandleFunc("POST /memory/{id}/reactivate", s.handleReactivate)
	s.mux.HandleFunc("GET /similar", s.handleSimilar)
	s.mux.HandleFunc("POST /similar/draft", s.handleSimilarDraft)
	s.mux.HandleFunc("POST /similar/apply", s.handleSimilarApply)
	s.mux.HandleFunc("GET /candidates", s.handleCandidates)
	s.mux.HandleFunc("POST /candidates/{id}/approve", s.handleApprove)
	s.mux.HandleFunc("POST /candidates/{id}/reject", s.handleReject)
	s.mux.HandleFunc("GET /projects", s.handleProjects)
	s.mux.HandleFunc("GET /doctor", s.handleDoctor)
	s.mux.HandleFunc("GET /assets/logo.png", s.handleLogo)
	s.mux.HandleFunc("GET /project", s.handleSwitchProject)
	s.mux.HandleFunc("GET /backups", s.handleBackups)
	s.mux.HandleFunc("GET /conflicts", s.handleConflicts)
	s.mux.HandleFunc("GET /archived", s.handleArchived)
	s.mux.HandleFunc("POST /candidates/approve-batch", s.handleBatchReview)
	s.mux.HandleFunc("POST /candidates/reject-batch", s.handleBatchReview)
	// Anything else gets the same styled shell instead of net/http's plain 404.
	s.mux.HandleFunc("/", s.handleNotFound)
	return s, nil
}

// Token is the bearer secret printed by `recalldock ui`; tests use it directly.
func (s *Server) Token() string { return s.token }

// URL returns the ready-to-open loopback URL.
func (s *Server) URL(addr string) string {
	return "http://" + addr + "/?token=" + s.token
}

func (s *Server) Handler() http.Handler { return s.authenticate(s.mux) }

// Listen binds loopback only; a wildcard or non-loopback address is rejected.
func (s *Server) Listen(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("operator UI only listens on a literal loopback address")
	}
	return net.Listen("tcp", addr)
}

// Serve runs until the listener closes.
func (s *Server) Serve(ln net.Listener) error { return http.Serve(ln, s.Handler()) }

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// img-src 'self' is required for the embedded brand mark: default-src
		// 'none' alone makes the browser block /assets/logo.png.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		// Every page carries a CSRF token and raw knowledge text: never let a
		// shared browser or proxy keep a copy.
		w.Header().Set("Cache-Control", "no-store")
		if !loopbackHost(r.Host) {
			http.Error(w, "operator UI accepts loopback hosts only", http.StatusForbidden)
			return
		}
		if provided := r.URL.Query().Get("token"); provided != "" {
			if subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
				http.Error(w, "invalid operator token", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			clean := *r.URL
			q := clean.Query()
			q.Del("token")
			clean.RawQuery = q.Encode()
			http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.token)) != 1 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintln(w, "RecallDock 操作界面需要启动时打印的一次性 token。")
			fmt.Fprintln(w, "请在运行 `recalldock ui` 的终端里复制完整 URL（含 ?token=...）重新打开。")
			return
		}
		if r.URL.Path != "/project" {
			if err := s.validateScopeSelection(s.scopeSelection(r)); err != nil {
				http.SetCookie(w, &http.Cookie{Name: projectCookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// maxReasonBytes matches the store's bound for operator edit/review reasons.
const maxReasonBytes = 512

// validateReason rejects empty or oversized reasons before touching the store
// so the operator sees a 400 with a usable message instead of an internal 409.
func validateReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", errors.New("必须填写理由：它会被写进审计记录")
	}
	if len(reason) > maxReasonBytes {
		return "", fmt.Errorf("理由过长：最多 %d 字节（约 170 个汉字）", maxReasonBytes)
	}
	return reason, nil
}

// readOnlyDenial renders the shared refusal for mutation routes.
func (s *Server) readOnlyDenial(w http.ResponseWriter, r *http.Request, pageName, title string) bool {
	if !s.readOnly {
		return false
	}
	s.renderError(w, r, pageName, title, http.StatusForbidden, errors.New("界面以只读模式启动（--read-only）：不提供写入"))
	return true
}

func (s *Server) validMutation(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		expected := "http://" + r.Host
		if !strings.EqualFold(origin, expected) {
			return false
		}
	}
	if err := r.ParseForm(); err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(s.token)) == 1
}

// scope returns the active project for this request: "" means all projects.
func (s *Server) scope(r *http.Request) string {
	raw := s.scopeSelection(r)
	if raw == "*" {
		return ""
	}
	return raw
}

func (s *Server) scopeSelection(r *http.Request) string {
	if cookie, err := r.Cookie(projectCookieName); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	return s.project
}

func (s *Server) validateScopeSelection(value string) error {
	if value == "" || value == "*" {
		return nil
	}
	if value == s.project && !strings.HasPrefix(value, "prj-") {
		return nil
	}
	if _, err := s.store.RegisteredProject(value); err != nil {
		return errors.New("项目选择无效或已过期：请运行 recalldock projects list 确认身份，刷新页面后通过顶部重新选择项目或全部项目")
	}
	return nil
}

func scopeLabel(scope string) string {
	if scope == "" {
		return "全部项目"
	}
	return "项目 " + scope
}

// scopeOptions lists "all", the pinned scope and every registered identity.
func (s *Server) scopeOptions(selected string) []projectOption {
	options := []projectOption{{Value: "*", Label: "全部项目", Selected: selected == "*" || selected == ""}}
	seen := map[string]bool{}
	add := func(value, label string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		options = append(options, projectOption{Value: value, Label: label, Selected: value == selected})
	}
	names := map[string]string{}
	if projects, err := s.store.ListRegisteredProjects(); err == nil {
		for _, project := range projects {
			names[project.ID] = project.Name
		}
	}
	addScope := func(value string) {
		label := "当前项目 " + shortID(value)
		if name := names[value]; name != "" {
			label = "当前项目 " + name
		}
		add(value, label)
	}
	addScope(s.project)
	if projects, err := s.store.ListRegisteredProjects(); err == nil {
		for _, project := range projects {
			add(project.ID, project.Name+" · "+shortID(project.ID))
		}
	}
	return options
}

func (s *Server) newPage(r *http.Request, title, pageName string) *page {
	scope := s.scope(r)
	selection := s.scopeSelection(r)
	p := &page{
		Page: pageName, Title: title, Version: s.version,
		Project: scope, ProjectLabel: scopeLabel(scope), CSRF: s.token,
		ScopeValue: selection, ScopeOptions: s.scopeOptions(selection), ProjectHint: s.hint,
		ReadOnly: s.readOnly,
	}
	if identity, err := s.store.Identity(); err == nil {
		p.Identity = identity
	}
	stats, err := s.store.ProjectStats(scope)
	if err == nil {
		p.Stats = *stats
	}
	pending, err := s.store.ListMemoryCandidates(scope, store.CandidateStatusReview, 200)
	if err == nil {
		p.Pending = len(pending)
	}
	switch r.URL.Query().Get("msg") {
	case "archived":
		p.Notice = "已归档：默认召回不再包含它，历史仍保留。"
	case "reactivated":
		p.Notice = "已恢复：重新进入默认召回。"
	case "approved":
		p.Notice = "候选已批准，已写入正式知识。"
	case "rejected":
		p.Notice = "候选已拒绝。"
	case "updated":
		p.Notice = "已保存新修订（旧版本仍在历史里）。"
	case "refined":
		p.Notice = "已保存精炼正文，并把来源归档到底层（归档可在「已归档」里恢复）。"
	}
	return p
}

// highlightHTML escapes text and wraps literal query terms in <mark>. It never
// trusts the input: every slice is escaped before the only markup we add, so a
// malicious title or query cannot inject HTML.
func highlightHTML(text, query string) template.HTML {
	escaped := html.EscapeString(text)
	query = strings.TrimSpace(query)
	if text == "" || query == "" {
		return template.HTML(escaped)
	}
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return template.HTML(escaped)
	}
	if len(terms) > 6 {
		terms = terms[:6]
	}
	patterns := make([]string, 0, len(terms))
	for _, term := range terms {
		runes := []rune(term)
		if len(runes) > 40 {
			term = string(runes[:40])
		}
		patterns = append(patterns, regexp.QuoteMeta(term))
	}
	re, err := regexp.Compile("(?i)(" + strings.Join(patterns, "|") + ")")
	if err != nil {
		return template.HTML(escaped)
	}
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringIndex(text, -1) {
		b.WriteString(html.EscapeString(text[last:loc[0]]))
		b.WriteString(`<mark class="hl">`)
		b.WriteString(html.EscapeString(text[loc[0]:loc[1]]))
		b.WriteString(`</mark>`)
		last = loc[1]
	}
	b.WriteString(html.EscapeString(text[last:]))
	return template.HTML(b.String())
}

func (s *Server) render(w http.ResponseWriter, status int, data *page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("[operatorui] render: %v", err)
	}
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, _ string, title string, status int, err error) {
	// Error pages use a dedicated empty content slot: rendering a page template
	// without its model (for example a memory page with no observation) would
	// panic inside html/template after the headers were written.
	data := s.newPage(r, title, "error")
	data.Error = err.Error()
	s.render(w, status, data)
}

// handleSwitchProject persists the operator's scope choice in a cookie. The
// value must be "*" or an exact registered identity; the redirect target must
// stay on this origin. Changing the visible scope is a navigation preference,
// not a knowledge write, so --read-only keeps it available.
func (s *Server) handleSwitchProject(w http.ResponseWriter, r *http.Request) {
	value := strings.TrimSpace(r.URL.Query().Get("value"))
	if value == "" {
		value = s.project
	}
	if err := s.validateScopeSelection(value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: projectCookieName, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	next := localRedirect(r.URL.Query().Get("next"))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// localRedirect accepts only a path on this origin. A backslash is rejected
// explicitly: browsers normalise "/\evil.example" to "//evil.example", which
// would turn the scope switch into an open redirect.
func localRedirect(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	if strings.ContainsAny(next, "\\\r\n\t") {
		return "/"
	}
	if parsed, err := url.Parse(next); err != nil || parsed.Host != "" || parsed.Scheme != "" {
		return "/"
	}
	return next
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.renderError(w, r, "error", "页面不存在", http.StatusNotFound, errors.New("没有这个页面："+r.URL.Path))
}

func (s *Server) handleLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(logoPNG)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := s.newPage(r, "概览", "dashboard")
	recent, err := s.store.RecentObservations(s.scope(r), "", 8)
	if err != nil {
		s.renderError(w, r, "dashboard", "概览", http.StatusInternalServerError, err)
		return
	}
	data.Recent = recent
	if identity, err := s.store.Identity(); err == nil {
		if _, mod, size, count, ok := latestBackup(filepath.Join(identity.DataDir, "backups")); ok {
			data.BackupTime = mod.Format("2006-01-02 15:04")
			data.BackupSize = humanBytes(size)
			data.BackupCount = count
		}
	}
	s.render(w, http.StatusOK, data)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	data := s.newPage(r, "搜索", "search")
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	data.Query = query
	data.Since = strings.TrimSpace(r.URL.Query().Get("since"))
	data.Until = strings.TrimSpace(r.URL.Query().Get("until"))
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" {
		project = s.scopeSelection(r)
	}
	if project == "*" {
		data.AllProjects = true
		project = ""
	}
	data.ProjectOptions = s.projectOptions(project)
	data.ProjectLabel = s.projectLabel(project)
	if err := s.validateScopeSelection(project); err != nil {
		data.Error = "未知项目筛选：" + project + "（" + err.Error() + "）"
		s.render(w, http.StatusBadRequest, data)
		return
	}
	if query == "" {
		s.render(w, http.StatusOK, data)
		return
	}
	page, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("page")))
	if page < 1 {
		page = 1
	}
	if page > 500 {
		page = 500
	}
	const pageSize = 20
	results, err := s.store.SearchPreview(query, store.SearchOptions{
		Project: project, Limit: pageSize, Offset: (page - 1) * pageSize,
		Detail: store.SearchDetailL0, MatchMode: "all",
		Since: data.Since, Until: data.Until,
	})
	if err != nil {
		data.Error = "搜索条件无效：" + err.Error() + "（时间格式：YYYY-MM-DD 或 YYYY-MM-DD HH:MM:SS）"
		s.render(w, http.StatusBadRequest, data)
		return
	}
	// The store clamps Limit to MaxSearchResults, so a full page alone cannot
	// prove there is another one. Probe a single extra row instead of guessing.
	if len(results) == pageSize {
		probe, probeErr := s.store.SearchPreview(query, store.SearchOptions{
			Project: project, Limit: 1, Offset: page * pageSize,
			Detail: store.SearchDetailL0, MatchMode: "all",
			Since: data.Since, Until: data.Until,
		})
		data.HasNext = probeErr == nil && len(probe) > 0
	}
	data.Results = results
	if !s.readOnly {
		s.store.RecordSearchRecalls(results)
	}
	data.PageNumber = page
	data.HasPrev = page > 1
	pageURL := func(target int) string {
		values := url.Values{}
		values.Set("q", query)
		if data.AllProjects {
			values.Set("project", "*")
		} else if project != "" {
			values.Set("project", project)
		}
		if data.Since != "" {
			values.Set("since", data.Since)
		}
		if data.Until != "" {
			values.Set("until", data.Until)
		}
		values.Set("page", strconv.Itoa(target))
		return "/search?" + values.Encode()
	}
	if data.HasPrev {
		data.PrevURL = pageURL(page - 1)
	}
	if data.HasNext {
		data.NextURL = pageURL(page + 1)
	}
	s.render(w, http.StatusOK, data)
}

// projectLabel names a scope for result summaries: the registered name when
// there is one, otherwise the raw identity.
func (s *Server) projectLabel(project string) string {
	if project == "" {
		return "全部项目"
	}
	if projects, err := s.store.ListRegisteredProjects(); err == nil {
		for _, item := range projects {
			if item.ID == project {
				if item.Name != "" {
					return item.Name + " · " + item.ID
				}
				break
			}
		}
	}
	return project
}

// projectOptions lists registered identities plus the pinned scope; the "*"
// option means "all projects" for this operator surface.
func (s *Server) projectOptions(selected string) []projectOption {
	options := make([]projectOption, 0, 8)
	seen := map[string]bool{}
	add := func(value, label string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		options = append(options, projectOption{Value: value, Label: label, Selected: value == selected})
	}
	names := map[string]string{}
	if projects, err := s.store.ListRegisteredProjects(); err == nil {
		for _, project := range projects {
			names[project.ID] = project.Name
		}
	}
	addScope := func(value string) {
		label := "当前项目 " + shortID(value)
		if name := names[value]; name != "" {
			label = "当前项目 " + name
		}
		add(value, label)
	}
	addScope(s.project)
	if projects, err := s.store.ListRegisteredProjects(); err == nil {
		for _, project := range projects {
			add(project.ID, project.Name+" · "+shortID(project.ID))
		}
	}
	return options
}

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderError(w, r, "memory", "记忆", http.StatusBadRequest, errors.New("无效的记忆 ID"))
		return
	}
	selected, err := s.store.SelectObservationForEdit(id)
	if err != nil {
		s.renderError(w, r, "memory", "记忆", http.StatusNotFound, errors.New("找不到这条记忆（可能已删除）"))
		return
	}
	obs, err := s.store.GetObservation(id)
	if err != nil {
		s.renderError(w, r, "memory", "记忆", http.StatusNotFound, errors.New("找不到这条记忆（可能已删除）"))
		return
	}
	history, err := s.store.ObservationHistory(id, false, 10, 0)
	if err != nil {
		history = nil
	}
	data := s.newPage(r, obs.Title, "memory")
	data.Obs = obs
	data.Digest = selected.SnapshotSHA256
	data.History = history
	data.IsArchived = obs.ArchivedAt != nil
	s.render(w, http.StatusOK, data)
}

// observationDirectory returns the directory a registered project write must
// prove. Name-keyed (legacy) projects need no directory; registered identities
// use an active binding root so the same store validation as the CLI applies.
func (s *Server) observationDirectory(project string) (string, error) {
	if !projectpkg.IsProjectID(project) {
		return "", nil
	}
	bindings, err := s.store.ListProjectBindings(project, 20, "")
	if err != nil {
		return "", err
	}
	for _, binding := range bindings {
		if binding.State == "active" {
			return binding.RootPath, nil
		}
	}
	return "", errors.New("这个注册项目没有活动绑定目录；请先用 `recalldock projects bind` 绑定一个目录")
}

// handleEditForm renders the CAS-guarded edit form. Content edits are an
// operator action: the model transports cannot reach this route, and the
// submitted snapshot digest must still match at write time.
func (s *Server) handleEditForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderError(w, r, "edit", "编辑记忆", http.StatusBadRequest, errors.New("无效的记忆 ID"))
		return
	}
	selected, err := s.store.SelectObservationForEdit(id)
	if err != nil {
		s.renderError(w, r, "edit", "编辑记忆", http.StatusNotFound, errors.New("找不到这条记忆"))
		return
	}
	obs, err := s.store.GetObservation(id)
	if err != nil {
		s.renderError(w, r, "edit", "编辑记忆", http.StatusNotFound, errors.New("找不到这条记忆"))
		return
	}
	data := s.newPage(r, "编辑记忆", "edit")
	data.Obs = obs
	data.Digest = selected.SnapshotSHA256
	s.render(w, http.StatusOK, data)
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyDenial(w, r, "edit", "编辑记忆") {
		return
	}
	if !s.validMutation(r) {
		s.renderError(w, r, "edit", "编辑记忆", http.StatusForbidden, errors.New("请求校验失败：请刷新页面后重试"))
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderError(w, r, "edit", "编辑记忆", http.StatusBadRequest, errors.New("无效的记忆 ID"))
		return
	}
	reason, err := validateReason(r.FormValue("reason"))
	if err != nil {
		s.renderError(w, r, "edit", "编辑记忆", http.StatusBadRequest, err)
		return
	}
	expected := strings.TrimSpace(r.FormValue("expected"))
	obs, err := s.store.GetObservation(id)
	if err != nil {
		s.renderError(w, r, "edit", "编辑记忆", http.StatusNotFound, errors.New("找不到这条记忆"))
		return
	}
	project := ""
	if obs.Project != nil {
		project = *obs.Project
	}
	directory, err := s.observationDirectory(project)
	if err != nil {
		s.renderError(w, r, "edit", "编辑记忆", http.StatusConflict, err)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	content := strings.TrimSpace(r.FormValue("content"))
	abstract := strings.TrimSpace(r.FormValue("abstract"))
	patch := store.UpdateObservationParams{Content: &content, Abstract: &abstract}
	if title != "" {
		patch.Title = &title
	}
	selected, err := s.store.UpdateObservationCAS(id, expected, patch, reason, directory)
	if err != nil {
		s.renderError(w, r, "edit", "编辑记忆", http.StatusConflict, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/memory/%d?msg=updated&rev=%d", id, selected.RevisionCount), http.StatusSeeOther)
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) { s.handleLifecycle(w, r, true) }

func (s *Server) handleReactivate(w http.ResponseWriter, r *http.Request) {
	s.handleLifecycle(w, r, false)
}

func (s *Server) handleLifecycle(w http.ResponseWriter, r *http.Request, archive bool) {
	if s.readOnlyDenial(w, r, "memory", "记忆") {
		return
	}
	if !s.validMutation(r) {
		s.renderError(w, r, "memory", "记忆", http.StatusForbidden, errors.New("请求校验失败：请从操作界面内提交，或刷新页面重试"))
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderError(w, r, "memory", "记忆", http.StatusBadRequest, errors.New("无效的记忆 ID"))
		return
	}
	reason, err := validateReason(r.FormValue("reason"))
	if err != nil {
		s.renderError(w, r, "memory", "记忆", http.StatusBadRequest, err)
		return
	}
	expected := strings.TrimSpace(r.FormValue("expected"))
	obs, err := s.store.GetObservation(id)
	if err != nil {
		s.renderError(w, r, "memory", "记忆", http.StatusNotFound, errors.New("找不到这条记忆"))
		return
	}
	project := ""
	if obs.Project != nil {
		project = *obs.Project
	}
	directory, err := s.observationDirectory(project)
	if err != nil {
		s.renderError(w, r, "memory", "记忆", http.StatusConflict, err)
		return
	}
	flag := archive
	if _, err := s.store.UpdateObservationCAS(id, expected, store.UpdateObservationParams{Archived: &flag}, reason, directory); err != nil {
		s.renderError(w, r, "memory", "记忆", http.StatusConflict, err)
		return
	}
	msg := "reactivated"
	if archive {
		msg = "archived"
	}
	http.Redirect(w, r, fmt.Sprintf("/memory/%d?msg=%s", id, msg), http.StatusSeeOther)
}

// handleSimilar scans active memories for lexical near-duplicates. It is a
// hint surface: no page here writes knowledge, and "all projects" aggregates
// registered identities instead of scanning an unspecified database.
func (s *Server) handleSimilar(w http.ResponseWriter, r *http.Request) {
	data := s.newPage(r, "相似记忆", "similar")
	threshold := parseSimilarThreshold(r.URL.Query().Get("threshold"))
	data.SimilarThreshold = threshold
	data.SimilarScope = s.scope(r)

	projects := []string{}
	if data.SimilarScope != "" {
		projects = []string{data.SimilarScope}
	} else if registered, err := s.store.ListRegisteredProjects(); err == nil {
		for _, project := range registered {
			projects = append(projects, project.ID)
		}
		sort.Strings(projects)
		if len(projects) > 20 {
			projects = projects[:20]
			data.TruncatedNote = "注册项目超过 20 个：只扫描前 20 个；选择具体项目可看完整结果。"
		}
	}
	const maxGroups = 20
	for _, project := range projects {
		groups, err := s.store.SimilarMemoryGroups(project, threshold, 10)
		if err != nil {
			if data.SimilarScope != "" {
				s.renderError(w, r, "similar", "相似记忆", http.StatusInternalServerError, err)
				return
			}
			continue
		}
		for _, group := range groups {
			view := similarGroupView{
				Score:        fmt.Sprintf("%.2f", group.Score),
				ScoreValue:   group.Score,
				MemberCount:  len(group.Observations),
				Project:      project,
				ProjectLabel: s.projectLabel(project),
				TargetID:     group.SuggestedTarget,
			}
			for _, observation := range group.Observations {
				view.Members = append(view.Members, similarMemberView{
					ID:        observation.ID,
					Title:     observation.Title,
					Preview:   similarPreview(observation),
					Suggested: observation.ID == group.SuggestedTarget,
				})
			}
			data.SimilarGroups = append(data.SimilarGroups, view)
		}
	}
	sort.SliceStable(data.SimilarGroups, func(i, j int) bool {
		if data.SimilarGroups[i].ScoreValue != data.SimilarGroups[j].ScoreValue {
			return data.SimilarGroups[i].ScoreValue > data.SimilarGroups[j].ScoreValue
		}
		return data.SimilarGroups[i].Project < data.SimilarGroups[j].Project
	})
	if len(data.SimilarGroups) > maxGroups {
		data.SimilarGroups = data.SimilarGroups[:maxGroups]
		data.TruncatedNote = fmt.Sprintf("相似组超过 %d 组：只显示最相似的前 %d 组；提高阈值或缩小项目范围可减少噪声。", maxGroups, maxGroups)
	}
	s.render(w, http.StatusOK, data)
}

// similarPreview prefers a real abstract; EffectiveAbstract falls back to the
// title, which would repeat the title column verbatim.
func similarPreview(observation store.Observation) string {
	if abstract := strings.TrimSpace(observation.Abstract); abstract != "" {
		return clipRunes(abstract+" — "+observation.Content, 160)
	}
	return clipRunes(observation.Content, 160)
}

func parseSimilarThreshold(raw string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || value < 0.4 || value > 0.95 {
		return 0.6
	}
	return value
}

// parseSourceIDs de-duplicates submitted members and caps the batch. The store
// still validates project, scope, archived and pinned state per source.
func parseSourceIDs(values []string) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(values))
	for _, raw := range values {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == 50 {
			break
		}
	}
	return out
}

func (s *Server) handleSimilarDraft(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyDenial(w, r, "similar", "相似记忆") {
		return
	}
	if !s.validMutation(r) {
		s.renderError(w, r, "similar", "相似记忆", http.StatusForbidden, errors.New("请求校验失败：请刷新页面后重试"))
		return
	}
	targetID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("target")), 10, 64)
	if err != nil || targetID <= 0 {
		s.renderError(w, r, "similar", "相似记忆", http.StatusBadRequest, errors.New("无效的目标记忆 ID"))
		return
	}
	sources := parseSourceIDs(r.Form["sources"])
	if len(sources) == 0 {
		s.renderError(w, r, "similar", "相似记忆", http.StatusBadRequest, errors.New("至少选择一条来源记忆"))
		return
	}
	draft, err := s.store.BuildRefineDraft(targetID, sources)
	if err != nil {
		s.renderError(w, r, "similar", "精炼草稿", http.StatusConflict, errors.New("无法生成草稿："+err.Error()))
		return
	}
	view := &refineView{
		TargetID:    draft.Target.ID,
		TargetTitle: draft.Target.Title,
		Digest:      draft.Digest,
		Content:     draft.Content,
		Notes:       draft.Notes,
	}
	if draft.Target.Project != nil {
		view.Project = *draft.Target.Project
	}
	for _, observation := range draft.Sources {
		view.Sources = append(view.Sources, similarMemberView{
			ID:      observation.ID,
			Title:   observation.Title,
			Preview: similarPreview(observation),
		})
	}
	for _, id := range draft.SortedSkippedIDs() {
		view.Skipped = append(view.Skipped, skippedSourceView{ID: id, Reason: draft.Skipped[id]})
	}
	if len(view.Sources) == 0 {
		s.renderError(w, r, "similar", "精炼草稿", http.StatusConflict, errors.New("没有可合并的来源：它们可能已归档、被置顶或不在同一作用域"))
		return
	}
	data := s.newPage(r, "精炼草稿", "refine")
	data.Refine = view
	s.render(w, http.StatusOK, data)
}

func (s *Server) handleSimilarApply(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyDenial(w, r, "similar", "相似记忆") {
		return
	}
	if !s.validMutation(r) {
		s.renderError(w, r, "similar", "相似记忆", http.StatusForbidden, errors.New("请求校验失败：请刷新页面后重试"))
		return
	}
	reason, err := validateReason(r.FormValue("reason"))
	if err != nil {
		s.renderError(w, r, "similar", "精炼草稿", http.StatusBadRequest, err)
		return
	}
	targetID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("target")), 10, 64)
	if err != nil || targetID <= 0 {
		s.renderError(w, r, "similar", "精炼草稿", http.StatusBadRequest, errors.New("无效的目标记忆 ID"))
		return
	}
	sources := parseSourceIDs(r.Form["sources"])
	if len(sources) == 0 {
		s.renderError(w, r, "similar", "精炼草稿", http.StatusBadRequest, errors.New("至少保留一条来源记忆"))
		return
	}
	obs, err := s.store.GetObservation(targetID)
	if err != nil {
		s.renderError(w, r, "similar", "精炼草稿", http.StatusNotFound, errors.New("找不到目标记忆"))
		return
	}
	project := ""
	if obs.Project != nil {
		project = *obs.Project
	}
	directory, err := s.observationDirectory(project)
	if err != nil {
		s.renderError(w, r, "similar", "精炼草稿", http.StatusConflict, err)
		return
	}
	content := r.FormValue("content")
	expected := strings.TrimSpace(r.FormValue("expected"))
	result, applyErr := s.store.ApplyRefinement(targetID, sources, content, expected, reason, directory)
	if applyErr != nil {
		if errors.Is(applyErr, store.ErrObservationSnapshotChanged) {
			s.renderError(w, r, "similar", "精炼草稿", http.StatusConflict,
				errors.New("目标或来源记忆在生成草稿后发生变化：请回到相似列表重新生成草稿，避免遗漏修改。"))
			return
		}
		consolidation, partial := applyErr.(*store.ConsolidationError)
		if !partial {
			s.renderError(w, r, "similar", "精炼草稿", http.StatusConflict,
				errors.New("保存被拒绝："+applyErr.Error()+"。正文与来源都没有改变。"))
			return
		}
		data := s.newPage(r, "部分来源未归档", "refineresult")
		data.RefineTarget = targetID
		data.RefineUpdated = result.Updated
		data.RefineAbsorbed = len(result.Absorbed)
		for id, reason := range consolidation.Failed {
			data.RefineFailures = append(data.RefineFailures, refineFailureView{ID: id, Reason: reason})
		}
		sort.Slice(data.RefineFailures, func(i, j int) bool { return data.RefineFailures[i].ID < data.RefineFailures[j].ID })
		s.render(w, http.StatusOK, data)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/memory/%d?msg=refined", targetID), http.StatusSeeOther)
}

func (s *Server) handleCandidates(w http.ResponseWriter, r *http.Request) {
	data := s.newPage(r, "候选审核", "candidates")
	items, err := s.store.ListMemoryCandidates(s.scope(r), store.CandidateStatusReview, 50)
	if err != nil {
		s.renderError(w, r, "candidates", "候选审核", http.StatusInternalServerError, err)
		return
	}
	views := make([]candidateView, 0, len(items))
	for _, item := range items {
		digest, err := store.MemoryCandidateDigest(&item)
		if err != nil {
			continue
		}
		views = append(views, candidateView{
			ID: item.SyncID, Category: item.Category, Origin: item.SessionOrigin,
			Preview: clipRunes(item.Statement, 200), ExpiresAt: item.ExpiresAt,
			Digest: digest, Project: item.Project,
		})
	}
	data.Candidates = views
	if len(items) >= 50 {
		data.TruncatedNote = "候选超过 50 条，这里只列出最近的 50 条；处理完再刷新即可继续。"
	}
	s.render(w, http.StatusOK, data)
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) { s.handleReview(w, r, true) }

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) { s.handleReview(w, r, false) }

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request, approve bool) {
	if s.readOnlyDenial(w, r, "candidates", "候选审核") {
		return
	}
	if !s.validMutation(r) {
		s.renderError(w, r, "candidates", "候选审核", http.StatusForbidden, errors.New("请求校验失败：请从操作界面内提交，或刷新页面重试"))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	expected := strings.TrimSpace(r.FormValue("expected"))
	reason, err := validateReason(r.FormValue("reason"))
	if err != nil {
		s.renderError(w, r, "candidates", "候选审核", http.StatusBadRequest, err)
		return
	}
	if approve {
		_, err = s.store.ApproveMemoryCandidate(id, expected, reason)
	} else {
		err = s.store.RejectMemoryCandidateChecked(id, expected, reason)
	}
	if err != nil {
		s.renderError(w, r, "candidates", "候选审核", http.StatusConflict, err)
		return
	}
	msg := "rejected"
	if approve {
		msg = "approved"
	}
	http.Redirect(w, r, "/candidates?msg="+msg, http.StatusSeeOther)
}

// handleBatchReview approves or rejects the selected candidates. Every item is
// still CAS-checked against the digest submitted with the page: a candidate
// that changed after the page was rendered fails individually and is reported
// instead of being overwritten.
func (s *Server) handleBatchReview(w http.ResponseWriter, r *http.Request) {
	if s.readOnlyDenial(w, r, "candidates", "候选审核") {
		return
	}
	if !s.validMutation(r) {
		s.renderError(w, r, "candidates", "候选审核", http.StatusForbidden, errors.New("请求校验失败：请刷新页面后重试"))
		return
	}
	approve := strings.HasSuffix(r.URL.Path, "/approve-batch")
	ids := r.Form["ids"]
	if len(ids) == 0 {
		s.renderError(w, r, "candidates", "候选审核", http.StatusBadRequest, errors.New("没有勾选任何候选"))
		return
	}
	reason, err := validateReason(r.FormValue("reason"))
	if err != nil {
		s.renderError(w, r, "candidates", "候选审核", http.StatusBadRequest, err)
		return
	}
	data := s.newPage(r, "批量处理结果", "batchresult")
	for _, id := range ids {
		id = strings.TrimSpace(id)
		expected := strings.TrimSpace(r.FormValue("expected_" + id))
		view := batchResultView{ID: id}
		var err error
		if approve {
			_, err = s.store.ApproveMemoryCandidate(id, expected, reason)
		} else {
			err = s.store.RejectMemoryCandidateChecked(id, expected, reason)
		}
		switch {
		case err == nil && approve:
			view.Status = "已批准"
			data.BatchApproved++
		case err == nil:
			view.Status = "已拒绝"
			data.BatchRejected++
		default:
			view.Status = "失败"
			view.Error = err.Error()
			data.BatchFailed++
		}
		data.BatchResults = append(data.BatchResults, view)
	}
	s.render(w, http.StatusOK, data)
}

// fileSHA256 streams a bounded file digest; it never loads the whole file into
// memory. Files over 64 MiB are skipped by the caller.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) handleArchived(w http.ResponseWriter, r *http.Request) {
	data := s.newPage(r, "已归档", "archived")
	scope := s.scope(r)
	if scope != "" {
		items, err := s.store.ArchivedMemories(scope, 0, 20)
		if err != nil {
			s.renderError(w, r, "archived", "已归档", http.StatusInternalServerError, err)
			return
		}
		data.Archived = items
		if len(items) > 0 {
			data.ArchivedGroups = []archivedGroup{{Project: scope, Name: s.projectLabel(scope), Items: items}}
		}
		s.render(w, http.StatusOK, data)
		return
	}
	// "All projects" used to be an error page. The archive index is keyed by
	// project, so fetch each registered identity and render one section each.
	projects, err := s.store.ListRegisteredProjects()
	if err != nil {
		s.renderError(w, r, "archived", "已归档", http.StatusInternalServerError, err)
		return
	}
	type scopeEntry struct{ id, name string }
	scopes := make([]scopeEntry, 0, len(projects))
	seen := map[string]bool{}
	for _, project := range projects {
		seen[project.ID] = true
		scopes = append(scopes, scopeEntry{project.ID, project.Name})
	}
	// Legacy name-keyed projects are not registered identities, but their
	// archives are real and must stay reachable from the all-projects view.
	if stats, err := s.store.Stats(); err == nil {
		for _, name := range stats.Projects {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			scopes = append(scopes, scopeEntry{name, name})
		}
	}
	groups := make([]archivedGroup, 0, len(scopes))
	for _, scope := range scopes {
		items, err := s.store.ArchivedMemories(scope.id, 0, 20)
		if err != nil {
			continue
		}
		if len(items) == 0 {
			continue
		}
		groups = append(groups, archivedGroup{Project: scope.id, Name: scope.name, Items: items})
	}
	data.ArchivedGroups = groups
	s.render(w, http.StatusOK, data)
}

func (s *Server) handleConflicts(w http.ResponseWriter, r *http.Request) {
	data := s.newPage(r, "冲突", "conflicts")
	items, err := s.store.ListRelations(store.ListRelationsOptions{Project: s.scope(r), Limit: 50})
	if err != nil {
		s.renderError(w, r, "conflicts", "冲突", http.StatusInternalServerError, err)
		return
	}
	data.Relations = items
	if len(items) >= 50 {
		data.TruncatedNote = "关系超过 50 条，这里只列出最近的 50 条；完整判定用 conflicts CLI。"
	}
	s.render(w, http.StatusOK, data)
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	data := s.newPage(r, "项目", "projects")
	projects, err := s.store.ListRegisteredProjects()
	if err != nil {
		s.renderError(w, r, "projects", "项目", http.StatusInternalServerError, err)
		return
	}
	bindings := make(map[string][]store.ProjectBindingState, len(projects))
	counts := make(map[string]int, len(projects))
	for _, project := range projects {
		list, err := s.store.ListProjectBindings(project.ID, 20, "")
		if err == nil {
			bindings[project.ID] = list
		}
		if stats, err := s.store.ProjectStats(project.ID); err == nil && stats != nil {
			counts[project.ID] = stats.TotalObservations
		}
	}
	data.Projects = projects
	data.Bindings = bindings
	data.ProjectCounts = counts
	if cwd, err := os.Getwd(); err == nil {
		data.CurrentDir = cwd
		detected := s.store.DetectProject(cwd)
		if detected.Error == nil && detected.Project != "" {
			data.CurrentProject = detected.Project
			// worktree_main is a real bound scope: the worktree carries the
			// main checkout's root marker, so treating it as "unbound" was a
			// false warning.
			data.CurrentBound = detected.Source == projectpkg.SourceRegistered ||
				detected.Source == projectpkg.SourceConfig ||
				detected.Source == projectpkg.SourceWorktreeMain
		}
	}
	s.render(w, http.StatusOK, data)
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	data := s.newPage(r, "备份", "backups")
	identity, err := s.store.Identity()
	if err != nil {
		s.renderError(w, r, "backups", "备份", http.StatusInternalServerError, err)
		return
	}
	verify := r.URL.Query().Get("verify") == "1"
	data.VerifyChecksums = verify
	if verify {
		data.VerifyNotice = "已计算选中文件的 SHA256（≤64 MiB）；与安装收据或备份记录核对，不要拿它当发布签名。"
	}
	views := make([]backupView, 0, 16)
	for _, dir := range []struct{ path, kind string }{
		{filepath.Join(identity.DataDir, "backups"), "知识库"},
		{filepath.Join(identity.DataDir, "operations-backups"), "进度库"},
	} {
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			view := backupView{
				Name: entry.Name(), Kind: dir.kind,
				ModTime: info.ModTime().Format("2006-01-02 15:04"),
				Size:    humanBytes(info.Size()),
			}
			// Hash at most ten files per request: a directory full of large
			// snapshots must not turn one page load into a multi-gigabyte read.
			if verify && info.Size() <= 64<<20 && len(views) < 10 {
				if sum, err := fileSHA256(filepath.Join(dir.path, entry.Name())); err == nil {
					view.SHA256 = sum
				}
			}
			views = append(views, view)
		}
	}
	if len(views) == 0 {
		s.render(w, http.StatusOK, data)
		return
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ModTime > views[j].ModTime })
	if len(views) > 50 {
		views = views[:50]
		data.TruncatedNote = "备份文件超过 50 份，这里只列出最新的 50 份；完整列表看数据目录。"
	}
	data.Backups = views
	s.render(w, http.StatusOK, data)
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	data := s.newPage(r, "诊断", "doctor")
	report, err := diagnostic.NewRunner().RunAll(context.Background(), diagnostic.Scope{Store: s.store, Project: s.scope(r), Now: s.now()})
	if err != nil {
		s.renderError(w, r, "doctor", "诊断", http.StatusInternalServerError, err)
		return
	}
	data.Doctor = report
	data.DoctorAll = r.URL.Query().Get("all") == "1"
	data.DoctorShowAll = data.DoctorAll || report.Summary.Warnings+report.Summary.Errors+report.Summary.Blocked == 0
	s.render(w, http.StatusOK, data)
}

// latestBackup reports the newest file in dir plus the total count and size of
// the newest file. Backups are never opened here: this is metadata only.
func latestBackup(dir string) (string, time.Time, int64, int, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, 0, 0, false
	}
	var name string
	var mod time.Time
	var size int64
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		count++
		if mod.IsZero() || info.ModTime().After(mod) {
			name, mod, size = entry.Name(), info.ModTime(), info.Size()
		}
	}
	return name, mod, size, count, count > 0
}

func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

// humanAgo turns a stored UTC timestamp into a short relative label. It falls
// back to the raw value when the timestamp cannot be parsed, never to a wrong
// duration.
func humanAgo(value string, now time.Time) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "—"
	}
	layouts := []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"}
	var parsed time.Time
	for _, layout := range layouts {
		if candidate, err := time.Parse(layout, trimmed); err == nil {
			parsed = candidate
			break
		}
	}
	if parsed.IsZero() {
		return trimmed
	}
	delta := now.Sub(parsed)
	future := delta < 0
	if future {
		delta = -delta
	}
	var text string
	switch {
	case delta < time.Minute:
		text = "刚刚"
	case delta < time.Hour:
		text = fmt.Sprintf("%d 分钟", int(delta.Minutes()))
		if future {
			return "还有 " + text
		}
		text += "前"
		return text
	case delta < 24*time.Hour:
		text = fmt.Sprintf("%d 小时", int(delta.Hours()))
		if future {
			return "还有 " + text
		}
		text += "前"
		return text
	case delta < 30*24*time.Hour:
		text = fmt.Sprintf("%d 天", int(delta.Hours()/24))
		if future {
			return "还有 " + text
		}
		text += "前"
		return text
	default:
		return parsed.Format("2006-01-02")
	}
	return text
}

// shortID keeps identity columns readable without hiding which project it is.
func shortID(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= 15 {
		return value
	}
	return string(runes[:12]) + "…"
}

func categoryLabel(value string) string {
	switch value {
	case "decision":
		return "决策"
	case "procedure":
		return "流程"
	case "project_constraint":
		return "项目约束"
	case "user_preference":
		return "用户偏好"
	case "user_fact":
		return "用户事实"
	case "failure_avoidance":
		return "避免重犯"
	case "tool_behavior":
		return "工具行为"
	case "verified_outcome":
		return "已验证结果"
	case "agent_identity":
		return "代理身份"
	case "session_context":
		return "会话上下文"
	case "task_state":
		return "任务状态"
	case "":
		return "未分类"
	default:
		return value
	}
}

func originLabel(value string) string {
	switch value {
	case "model":
		return "模型"
	case "operator":
		return "操作人"
	case "host":
		return "宿主"
	case "", "unknown":
		return "未标注"
	default:
		return value
	}
}

func shortSHA(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

// bindingLabel renders binding state in the operator's language while keeping
// the raw state visible through the title attribute in the template.
func bindingLabel(state string) string {
	switch state {
	case "active":
		return "生效中"
	case "retired":
		return "已退役"
	case "superseded":
		return "已被替换"
	case "":
		return "未绑定"
	default:
		return state
	}
}

// nextStepText trims the diagnostic runner's boilerplate so a page of passing
// checks is not a wall of "No action required.".
func nextStepText(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.EqualFold(trimmed, "No action required.") {
		return "无需操作"
	}
	return trimmed
}

func clipRunes(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}
