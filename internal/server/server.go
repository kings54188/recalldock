// Package server provides the HTTP API for Engram.
//
// This is how external clients (OpenCode plugin, Claude Code hooks,
// any agent) communicate with the memory engine. Simple JSON REST API.
package server

import (
	"crypto/hmac"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"recalldock.local/recalldock/internal/diagnostic"
	projectpkg "recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
)

var loadServerStats = func(s *store.Store) (*store.Stats, error) {
	return s.Stats()
}

// SyncStatusProvider returns the current sync status. This is implemented
// by autosync.Manager and injected from cmd/engram/main.go.
type SyncStatusProvider interface {
	Status(project string) SyncStatus
}

// SyncStatus mirrors autosync.Status to avoid a direct import cycle.
type SyncStatus struct {
	Enabled              bool       `json:"enabled"`
	Phase                string     `json:"phase"`
	LastError            string     `json:"last_error,omitempty"`
	ConsecutiveFailures  int        `json:"consecutive_failures"`
	BackoffUntil         *time.Time `json:"backoff_until,omitempty"`
	LastSyncAt           *time.Time `json:"last_sync_at,omitempty"`
	ReasonCode           string     `json:"reason_code,omitempty"`
	ReasonMessage        string     `json:"reason_message,omitempty"`
	UpgradeStage         string     `json:"upgrade_stage,omitempty"`
	UpgradeReasonCode    string     `json:"upgrade_reason_code,omitempty"`
	UpgradeReasonMessage string     `json:"upgrade_reason_message,omitempty"`
	// Phase 2: deferred relation retry counts surfaced from sync_apply_deferred.
	DeferredCount int `json:"deferred_count"`
	DeadCount     int `json:"dead_count"`
}

// SemanticRunnerFactory is a function that resolves a store.SemanticRunner by name.
// It is injected from cmd/engram/main.go so that internal/server does not depend on
// internal/llm directly (preserving the import-cycle boundary).
type SemanticRunnerFactory func(name string) (store.SemanticRunner, error)

// SemanticPromptBuilder constructs the LLM prompt for a given pair of observation
// snippets. Injected from cmd/engram/main.go alongside SemanticRunnerFactory.
type SemanticPromptBuilder func(a, b store.ObservationSnippet) string

type Server struct {
	readOnce     sync.Once
	readBudget   store.ReadBudgetState
	readErr      error
	readExplicit bool
	readMu       sync.Mutex
	readBudgets  map[string]store.ReadBudgetState
	store        *store.Store
	mux          *http.ServeMux
	// routeTable is the reviewed list of registered "METHOD /pattern" pairs.
	// Tests assert every entry has an explicit transport-policy decision.
	routeTable []string
	port       int
	// Version is the build version injected by the host (cmd/recalldock serve).
	// It is reported verbatim by /health; empty falls back to "dev".
	Version    string
	listen     func(network, address string) (net.Listener, error)
	serve      func(net.Listener, http.Handler) error
	onWrite    func() // called after successful local writes (for autosync notification)
	syncStatus SyncStatusProvider

	// runnerFactory resolves a SemanticRunner by CLI name (read from RECALLDOCK_AGENT_CLI).
	// When nil, semantic=true requests fail with 500.
	runnerFactory SemanticRunnerFactory
	// promptBuilder constructs LLM prompts for semantic scan pairs.
	// When nil and semantic=true, a no-op builder is used (returns empty string).
	promptBuilder SemanticPromptBuilder
}

func New(s *store.Store, port int) *Server {
	srv := &Server{store: s, port: port, listen: net.Listen, serve: http.Serve}
	srv.mux = http.NewServeMux()
	srv.routes()
	return srv
}

// SetOnWrite configures a callback invoked after every successful local write.
// This is used to notify autosync.Manager via NotifyDirty().
func (s *Server) SetOnWrite(fn func()) {
	s.onWrite = fn
}

// SetSyncStatus configures the sync status provider for the /sync/status endpoint.
func (s *Server) SetSyncStatus(provider SyncStatusProvider) {
	s.syncStatus = provider
}

// SetRunnerFactory configures the semantic runner factory used by POST /conflicts/scan
// when semantic=true. When not set, semantic=true requests fail with 500.
func (s *Server) SetRunnerFactory(fn SemanticRunnerFactory) {
	s.runnerFactory = fn
}

// SetPromptBuilder configures the LLM prompt builder for semantic scan pairs.
// When not set, an empty-string builder is used (valid for tests, not production).
func (s *Server) SetPromptBuilder(fn SemanticPromptBuilder) {
	s.promptBuilder = fn
}

// notifyWrite calls the onWrite callback if configured (best-effort, non-blocking).
func (s *Server) notifyWrite() {
	if s.onWrite != nil {
		s.onWrite()
	}
}

// requireAuth wraps h with optional Bearer-token authentication.
//
// When the RECALLDOCK_HTTP_TOKEN environment variable is set, every request to the
// wrapped handler must supply a matching "Authorization: Bearer <token>" header.
// Comparison is constant-time to prevent timing attacks. When the env var is
// unset the handler is called directly — zero-config is preserved.
//
// The token is read from the environment on every request so that the server
// does not need to restart when the variable changes.
func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := os.Getenv("RECALLDOCK_HTTP_TOKEN")
		if token == "" {
			// No token configured → open access (zero-config default).
			h(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authHeader, prefix) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="engram"`)
			jsonError(w, http.StatusUnauthorized, "authorization required")
			return
		}

		provided := authHeader[len(prefix):]
		// Use constant-time comparison via hmac.Equal to prevent timing attacks.
		if !hmac.Equal([]byte(provided), []byte(token)) {
			// Extra defense: also absorb timing via subtle.ConstantTimeCompare (same algo).
			_ = subtle.ConstantTimeCompare([]byte(provided), []byte(token))
			w.Header().Set("WWW-Authenticate", `Bearer realm="engram"`)
			jsonError(w, http.StatusUnauthorized, "invalid token")
			return
		}

		h(w, r)
	}
}

func (s *Server) Start() error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	listenFn := s.listen
	if listenFn == nil {
		listenFn = net.Listen
	}
	serveFn := s.serve
	if serveFn == nil {
		serveFn = http.Serve
	}

	ln, err := listenFn("tcp", addr)
	if err != nil {
		return fmt.Errorf("recalldock server: listen %s: %w", addr, err)
	}
	log.Printf("[recalldock] HTTP server listening on %s", addr)
	return serveFn(ln, s.Handler())
}

func (s *Server) Handler() http.Handler {
	return s.agentPolicy(s.mux)
}

// routeTable records every registered "METHOD /pattern" pair in registration
// order so tests can assert that each one has an explicitly reviewed transport
// policy. The policy middleware itself stays fail-closed for unknown paths.
func (s *Server) handle(pattern string, h http.HandlerFunc) {
	s.routeTable = append(s.routeTable, pattern)
	s.mux.HandleFunc(pattern, h)
}

func (s *Server) routes() {
	s.handle("GET /health", s.handleHealth)

	// Sessions
	s.handle("POST /sessions", s.handleCreateSession)
	s.handle("POST /sessions/{id}/end", s.handleEndSession)
	s.handle("POST /sessions/{id}/candidates", s.handleCommitSessionCandidates)
	s.handle("GET /sessions/recent", s.handleRecentSessions)
	s.handle("GET /sessions/{id}", s.handleGetSession)
	s.handle("DELETE /sessions/{id}", requireAuth(s.handleDeleteSession))

	// Observations
	s.handle("POST /observations", s.handleAddObservation)
	s.handle("GET /observations", s.handleListObservations)
	s.handle("POST /observations/passive", s.handlePassiveCapture)
	s.handle("GET /observations/recent", s.handleRecentObservations)
	s.handle("PATCH /observations/{id}", s.handleUpdateObservation)
	s.handle("DELETE /observations/{id}", requireAuth(s.handleDeleteObservation))

	// Search
	s.handle("GET /search", s.handleSearch)

	// Timeline
	s.handle("GET /timeline", s.handleTimeline)
	s.handle("GET /observations/{id}", s.handleGetObservation)

	// Lifecycle review
	s.handle("GET /review", s.handleReviewList)
	s.handle("POST /review/mark_reviewed", s.handleReviewMarkReviewed)

	// Prompts
	s.handle("POST /prompts", s.handleAddPrompt)
	s.handle("GET /prompts/recent", s.handleRecentPrompts)
	s.handle("GET /prompts/search", s.handleSearchPrompts)
	s.handle("DELETE /prompts/{id}", requireAuth(s.handleDeletePrompt))

	// Context
	s.handle("GET /context", s.handleContext)

	// Export / Import — sensitive: full data read and bulk mutation.
	s.handle("GET /export", requireAuth(s.handleExport))
	s.handle("POST /import", requireAuth(s.handleImport))

	// Stats / diagnostics
	s.handle("GET /stats", s.handleStats)
	s.handle("GET /doctor", s.handleDoctor)

	// Project detection / migration
	s.handle("GET /project/current", s.handleCurrentProject)
	s.handle("POST /projects/migrate", requireAuth(s.handleMigrateProject))

	// Sync status (degraded-state visibility for autosync)
	s.handle("GET /sync/status", s.handleSyncStatus)

	// Conflicts — CRITICAL ORDER: literals before wildcard (Go 1.22 mux panics on overlap)
	s.handle("GET /conflicts", s.handleListConflicts)
	s.handle("GET /conflicts/stats", s.handleConflictsStats)
	s.handle("GET /conflicts/deferred", s.handleListDeferred)
	s.handle("POST /conflicts/scan", s.handleScanConflicts)
	s.handle("POST /conflicts/judge", s.handleJudgeConflict)
	s.handle("POST /conflicts/compare", s.handleCompareMemories)
	s.handle("POST /conflicts/deferred/replay", s.handleReplayDeferred)
	s.handle("GET /conflicts/{relation_id}", s.handleGetConflict)
}

// ─── Handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	version := s.Version
	if version == "" {
		version = "dev"
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "recalldock",
		"version": version,
	})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID        string `json:"id"`
		Project   string `json:"project"`
		Directory string `json:"directory"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.ID == "" || body.Project == "" {
		jsonError(w, http.StatusBadRequest, "id and project are required")
		return
	}

	if err := s.store.CreateSessionWithOrigin(body.ID, body.Project, body.Directory, store.SessionOriginModel); err != nil {
		if errors.Is(err, store.ErrSessionScopeMismatch) {
			jsonErrorWithFields(w, http.StatusConflict, "Session ID belongs to another project/directory; use a new session ID", map[string]any{"code": "session_scope_mismatch"})
			return
		}
		if errors.Is(err, projectpkg.ErrBinding) {
			jsonErrorWithFields(w, http.StatusConflict, "Directory is not bound to that project; rebind explicitly or use the matching project", map[string]any{"code": "binding_unverified"})
			return
		}
		if errors.Is(err, store.ErrProjectBindingRequired) {
			jsonErrorWithFields(w, http.StatusBadRequest, "A model session needs an established project binding; run `recalldock projects create --name <name>` and `recalldock projects bind <ID> --directory <abs>` once, then retry", map[string]any{"code": "project_binding_required"})
			return
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.notifyWrite()
	jsonResponse(w, http.StatusCreated, map[string]string{"id": body.ID, "status": "created"})
}

func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body struct {
		Summary string `json:"summary"`
	}
	json.NewDecoder(r.Body).Decode(&body)

	if err := s.store.EndSession(id, body.Summary); err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.notifyWrite()
	jsonResponse(w, http.StatusOK, map[string]string{"id": id, "status": "completed"})
}

func (s *Server) handleCommitSessionCandidates(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Project    string                       `json:"project"`
		Candidates []store.MemoryCandidateInput `json:"candidates"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	result, err := s.store.CommitMemoryCandidates(store.CommitMemoryCandidatesParams{
		SessionID: r.PathValue("id"), Project: body.Project, Candidates: body.Candidates,
	})
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if result.Promoted > 0 {
		s.notifyWrite()
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"promoted": result.Promoted, "review": result.Review, "rejected": result.Rejected,
		"duplicate":     result.Duplicate,
		"candidate_ids": result.CandidateIDs,
	})
}

func (s *Server) handleRecentSessions(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	limit := queryInt(r, "limit", 5)

	sessions, err := s.store.RecentSessions(project, limit)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, sessions)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	session, err := s.store.GetSession(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "session not found")
			return
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, session)
}

func (s *Server) handleAddObservation(w http.ResponseWriter, r *http.Request) {
	var body store.AddObservationParams
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.SessionID == "" || body.Title == "" || body.Content == "" {
		jsonError(w, http.StatusBadRequest, "session_id, title, and content are required")
		return
	}
	if !s.validateSessionProject(w, body.SessionID, body.Project) {
		return
	}

	id, err := s.store.AddObservation(body)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.notifyWrite()
	jsonResponse(w, http.StatusCreated, map[string]any{"id": id, "status": "saved"})
}

func (s *Server) handlePassiveCapture(w http.ResponseWriter, r *http.Request) {
	var body store.PassiveCaptureParams
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.SessionID == "" {
		jsonError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if !s.validateSessionProject(w, body.SessionID, body.Project) {
		return
	}

	result, err := s.store.PassiveCapture(body)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.notifyWrite()
	jsonResponse(w, http.StatusOK, result)
}

func (s *Server) handleListObservations(w http.ResponseWriter, r *http.Request) {
	sort := r.URL.Query().Get("sort")
	if sort != "" && sort != "created_at:desc" {
		jsonError(w, http.StatusBadRequest, "unsupported sort: use created_at:desc")
		return
	}

	s.handleRecentObservations(w, r)
}

func (s *Server) handleRecentObservations(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	scope := r.URL.Query().Get("scope")
	limit := queryInt(r, "limit", 20)
	detail := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("detail")))
	if detail != "" && detail != "l0" && detail != "l1" && detail != "l2" {
		jsonError(w, 400, "detail must be l0, l1 or l2")
		return
	}

	obs, err := s.store.RecentObservations(project, scope, limit)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if detail == "l0" || detail == "l1" {
		views := make([]map[string]any, 0, len(obs))
		for _, item := range obs {
			views = append(views, observationIndexView(item, detail))
		}
		jsonResponse(w, 200, views)
		return
	}
	jsonResponse(w, http.StatusOK, obs)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	query := params.Get("q")
	if query == "" {
		jsonError(w, http.StatusBadRequest, "q parameter is required")
		return
	}

	project := params.Get("project")
	if queryBool(r, "all_projects", false) {
		project = ""
	}

	matchMode := params.Get("match_mode")
	if matchMode != "" && matchMode != "all" && matchMode != "any" {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("invalid match_mode %q: must be \"all\" or \"any\"", matchMode))
		return
	}

	opts := searchOptions(r)
	opts.Project = project
	if params.Get("detail") != "" || params.Get("strategy") != "" {
		response, err := s.store.SearchDetailed(query, opts)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, response)
		return
	}
	results, err := s.store.Search(query, opts)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, results)
}

func (s *Server) handleGetObservation(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid observation id")
		return
	}

	obs, err := s.store.GetObservation(id)
	if err != nil {
		jsonError(w, http.StatusNotFound, "observation not found")
		return
	}

	if detail := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("detail"))); detail != "" && detail != "l2" {
		if detail != "l0" && detail != "l1" {
			jsonError(w, http.StatusBadRequest, "detail must be l0, l1 or l2")
			return
		}
		view := map[string]any{"id": obs.ID, "sync_id": obs.SyncID, "revision_count": obs.RevisionCount, "project": obs.Project, "detail": detail, "abstract": obs.EffectiveAbstract(), "state": obs.State()}
		if detail == "l1" {
			view["overview"] = obs.EffectiveOverview(2000)
		}
		jsonResponse(w, http.StatusOK, view)
		return
	}
	jsonResponse(w, http.StatusOK, obs)
}

func (s *Server) handleUpdateObservation(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid observation id")
		return
	}

	var body store.UpdateObservationParams
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	if body.Type == nil && body.Title == nil && body.Content == nil && body.Project == nil && body.Scope == nil && body.TopicKey == nil {
		jsonError(w, http.StatusBadRequest, "at least one field is required")
		return
	}

	obs, err := s.store.UpdateObservation(id, body)
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}

	s.notifyWrite()
	jsonResponse(w, http.StatusOK, obs)
}

func (s *Server) handleDeleteObservation(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid observation id")
		return
	}

	hard := queryBool(r, "hard", false)
	if err := s.store.DeleteObservation(id, hard); err != nil {
		switch {
		case errors.Is(err, store.ErrObservationNotFound):
			jsonError(w, http.StatusNotFound, err.Error())
		default:
			jsonError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	s.notifyWrite()
	jsonResponse(w, http.StatusOK, map[string]any{
		"id":          id,
		"status":      "deleted",
		"hard_delete": hard,
	})
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	idStr := timelineObservationID(r)
	if idStr == "" {
		jsonError(w, http.StatusBadRequest, "observation_id parameter is required")
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid observation_id")
		return
	}

	before := queryInt(r, "before", 5)
	after := queryInt(r, "after", 5)

	result, err := s.store.Timeline(id, before, after)
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, result)
}

func (s *Server) handleReviewList(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	limit := queryInt(r, "limit", 10)

	status := r.URL.Query().Get("status")
	if status != "" && status != "active" && status != "stale" {
		jsonError(w, http.StatusBadRequest, "status must be active or stale")
		return
	}
	observations, err := s.store.ObservationsNeedingReview(project, limit, status)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	structured := make([]map[string]any, 0, len(observations))
	for _, obs := range observations {
		structured = append(structured, reviewObservationPayload(obs))
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"observations": structured,
		"count":        len(structured),
	})
}

func (s *Server) handleReviewMarkReviewed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ObservationID int64 `json:"observation_id"`
		ID            int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	id := body.ObservationID
	if id == 0 {
		id = body.ID
	}
	if id == 0 {
		jsonError(w, http.StatusBadRequest, "observation_id is required")
		return
	}

	if err := s.store.MarkReviewed(id); err != nil {
		if errors.Is(err, store.ErrObservationNotFound) {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	obs, err := s.store.GetObservation(id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "marked reviewed but failed to reload observation: "+err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, reviewObservationPayload(*obs))
}

func reviewObservationPayload(obs store.Observation) map[string]any {
	payload := map[string]any{
		"id":      obs.ID,
		"sync_id": obs.SyncID,
		"title":   obs.Title,
		"type":    obs.Type,
		"state":   obs.State(),
	}
	if obs.Project != nil {
		payload["project"] = *obs.Project
	}
	if obs.ReviewAfter != nil {
		payload["review_after"] = *obs.ReviewAfter
	}
	return payload
}

// ─── Prompts ─────────────────────────────────────────────────────────────────

func (s *Server) handleAddPrompt(w http.ResponseWriter, r *http.Request) {
	var body store.AddPromptParams
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.SessionID == "" || body.Content == "" {
		jsonError(w, http.StatusBadRequest, "session_id and content are required")
		return
	}
	if !s.validateSessionProject(w, body.SessionID, body.Project) {
		return
	}

	// When the client omits the project, derive it from the session so the
	// prompt is attributed correctly without the hook detecting it eagerly on
	// every message.
	if strings.TrimSpace(body.Project) == "" {
		if sess, err := s.store.GetSession(body.SessionID); err == nil {
			body.Project = sess.Project
		}
	}

	id, err := s.store.AddPrompt(body)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.notifyWrite()
	jsonResponse(w, http.StatusCreated, map[string]any{"id": id, "status": "saved"})
}

func (s *Server) handleRecentPrompts(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	limit := queryInt(r, "limit", 20)

	prompts, err := s.store.RecentPrompts(project, limit)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, prompts)
}

func (s *Server) handleSearchPrompts(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		jsonError(w, http.StatusBadRequest, "q parameter is required")
		return
	}

	prompts, err := s.store.SearchPrompts(
		query,
		r.URL.Query().Get("project"),
		queryInt(r, "limit", 10),
	)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, prompts)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		jsonError(w, http.StatusBadRequest, "session id is required")
		return
	}

	if err := s.store.DeleteSession(id); err != nil {
		switch {
		case errors.Is(err, store.ErrSessionDeleteBlocked):
			jsonError(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrSessionHasObservations):
			jsonError(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrSessionNotFound):
			jsonError(w, http.StatusNotFound, err.Error())
		default:
			jsonError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	// local-only delete path: do not notify autosync.
	jsonResponse(w, http.StatusOK, map[string]string{"id": id, "status": "deleted"})
}

func (s *Server) handleDeletePrompt(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid prompt id")
		return
	}

	if err := s.store.DeletePrompt(id); err != nil {
		if errors.Is(err, store.ErrPromptNotFound) {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.notifyWrite()
	jsonResponse(w, http.StatusOK, map[string]any{"id": id, "status": "deleted"})
}

// ─── Export / Import ─────────────────────────────────────────────────────────

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	project := query.Get("project")
	if _, provided := query["project"]; provided && strings.TrimSpace(project) == "" {
		jsonError(w, http.StatusBadRequest, "project parameter must not be blank")
		return
	}

	var (
		data *store.ExportData
		err  error
	)
	if strings.TrimSpace(project) != "" {
		data, err = s.store.ExportProject(project)
	} else {
		data, err = s.store.Export()
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=recalldock-export.json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(data)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	// Limit body to 50MB
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "failed to read body: "+err.Error())
		return
	}

	var data store.ExportData
	if err := json.Unmarshal(body, &data); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	result, err := s.store.Import(&data)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.notifyWrite()
	jsonResponse(w, http.StatusOK, result)
}

// ─── Context ─────────────────────────────────────────────────────────────────

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	scope := r.URL.Query().Get("scope")

	if mode := r.URL.Query().Get("mode"); mode != "" {
		response, err := s.store.FormatContextBounded(store.ContextOptions{Project: project, Scope: scope, Mode: mode, TokenBudget: queryInt(r, "token_budget", 0)})
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonResponse(w, http.StatusOK, map[string]any{"context": response.Text, "mode": response.Mode, "budget": response.Budget})
		return
	}
	context, err := s.store.FormatContext(project, scope)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{"context": context})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	var stats *store.Stats
	var err error
	if selected := strings.TrimSpace(r.URL.Query().Get("project")); selected != "" {
		selected, _ = store.NormalizeProject(selected)
		stats, err = s.store.StatsForProject(selected)
	} else {
		stats, err = loadServerStats(s.store)
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, stats)
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	projectName := strings.TrimSpace(r.URL.Query().Get("project"))
	if projectName != "" {
		projectName, _ = store.NormalizeProject(projectName)
		exists, err := s.store.ProjectExists(projectName)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !exists {
			available, err := s.store.ListProjectNames()
			if err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			jsonErrorWithFields(w, http.StatusNotFound, fmt.Sprintf("project %q not found", projectName), map[string]any{
				"code":               "unknown_project",
				"available_projects": available,
			})
			return
		}
	} else {
		cwd := strings.TrimSpace(r.URL.Query().Get("cwd"))
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				jsonError(w, http.StatusInternalServerError, "failed to detect cwd: "+err.Error())
				return
			}
		}
		res := s.store.DetectProject(cwd)
		if res.Error != nil {
			code := "project_detection_failed"
			if len(res.AvailableProjects) > 0 {
				code = "ambiguous_project"
			}
			jsonErrorWithFields(w, http.StatusBadRequest, "project detection failed: "+res.Error.Error(), map[string]any{
				"code":               code,
				"available_projects": res.AvailableProjects,
			})
			return
		}
		projectName, _ = store.NormalizeProject(res.Project)
	}

	check := strings.TrimSpace(r.URL.Query().Get("check"))
	runner := diagnostic.NewRunner()
	scope := diagnostic.Scope{Store: s.store, Project: projectName, Now: time.Now()}
	var (
		report diagnostic.Report
		err    error
	)
	if check != "" {
		report, err = runner.RunOne(r.Context(), scope, check)
	} else {
		report, err = runner.RunAll(r.Context(), scope)
	}
	if err != nil {
		report = diagnostic.ErrorReport(projectName, err)
	}

	jsonResponse(w, http.StatusOK, report)
}

// ─── Project Detection ───────────────────────────────────────────────────────

func (s *Server) handleCurrentProject(w http.ResponseWriter, r *http.Request) {
	cwd := strings.TrimSpace(r.URL.Query().Get("cwd"))
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to detect cwd: "+err.Error())
			return
		}
	}

	res := s.store.DetectExplicitProject(cwd)
	if r.Context().Value(publicMemoryReadContext{}) != true {
		res = s.store.DetectProject(cwd)
	}
	payload := map[string]any{
		"project":        res.Project,
		"project_source": res.Source,
		"project_path":   res.Path,
		"project_id":     res.ProjectID, "project_name": res.ProjectName, "binding_id": res.BindingID,
		"cwd":                cwd,
		"available_projects": res.AvailableProjects,
	}
	if res.Warning != "" {
		payload["warning"] = res.Warning
	}
	if res.Error != nil {
		payload["error_hint"] = res.Error.Error()
	}

	jsonResponse(w, http.StatusOK, payload)
}

// ─── Sync Status ─────────────────────────────────────────────────────────────

func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	if s.syncStatus == nil {
		jsonResponse(w, http.StatusOK, map[string]any{
			"enabled": false,
			"message": "background sync is not configured",
		})
		return
	}

	status := s.syncStatus.Status(r.URL.Query().Get("project"))
	jsonResponse(w, http.StatusOK, map[string]any{
		"enabled":              status.Enabled,
		"phase":                status.Phase,
		"last_error":           status.LastError,
		"consecutive_failures": status.ConsecutiveFailures,
		"backoff_until":        status.BackoffUntil,
		"last_sync_at":         status.LastSyncAt,
		"reason_code":          status.ReasonCode,
		"reason_message":       status.ReasonMessage,
		"deferred_count":       status.DeferredCount,
		"dead_count":           status.DeadCount,
		"upgrade": map[string]any{
			"stage":          status.UpgradeStage,
			"reason_code":    status.UpgradeReasonCode,
			"reason_message": status.UpgradeReasonMessage,
		},
	})
}

// ─── Project Migration ───────────────────────────────────────────────────────

func (s *Server) handleMigrateProject(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KB max
	var body struct {
		OldProject string `json:"old_project"`
		NewProject string `json:"new_project"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.OldProject == "" || body.NewProject == "" {
		jsonError(w, http.StatusBadRequest, "old_project and new_project are required")
		return
	}
	// Normalize both names using the same rules the store applies so that
	// case-only differences (e.g. "repo_name" vs "Repo_Name") are treated as
	// identical and do not trigger a migration that would create duplicates.
	// See: https://recalldock.local/recalldock/issues/438
	normalizedOld, _ := store.NormalizeProject(body.OldProject)
	normalizedNew, _ := store.NormalizeProject(body.NewProject)
	if normalizedOld == normalizedNew {
		jsonResponse(w, http.StatusOK, map[string]any{"status": "skipped", "reason": "names are identical"})
		return
	}

	result, err := s.store.MigrateProject(body.OldProject, body.NewProject)
	if err != nil {
		log.Printf("[recalldock] project migration failed: %v", err)
		jsonError(w, http.StatusInternalServerError, "migration failed")
		return
	}

	if !result.Migrated {
		jsonResponse(w, http.StatusOK, map[string]any{"status": "skipped", "reason": "no records found"})
		return
	}

	log.Printf("[recalldock] migrated project %q → %q (obs: %d, sessions: %d, prompts: %d)",
		body.OldProject, body.NewProject,
		result.ObservationsUpdated, result.SessionsUpdated, result.PromptsUpdated)

	jsonResponse(w, http.StatusOK, map[string]any{
		"status":       "migrated",
		"old_project":  body.OldProject,
		"new_project":  body.NewProject,
		"observations": result.ObservationsUpdated,
		"sessions":     result.SessionsUpdated,
		"prompts":      result.PromptsUpdated,
	})
}

// ─── Conflicts ───────────────────────────────────────────────────────────────

const (
	conflictsDefaultLimit = 50
	conflictsMaxLimit     = 500
)

// clampConflictsLimit silently clamps limit to [1, conflictsMaxLimit].
// If the provided value is ≤ 0, it returns defaultVal (typically 50).
func clampConflictsLimit(v, defaultVal int) int {
	if v <= 0 {
		return defaultVal
	}
	if v > conflictsMaxLimit {
		return conflictsMaxLimit
	}
	return v
}

// handleListConflicts serves GET /conflicts
// Query params: project, status, since (RFC3339), limit (default 50, max 500), offset.
func (s *Server) handleListConflicts(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	status := r.URL.Query().Get("status")
	limit := clampConflictsLimit(queryInt(r, "limit", conflictsDefaultLimit), conflictsDefaultLimit)
	offset := queryInt(r, "offset", 0)

	opts := store.ListRelationsOptions{
		Project: project,
		Status:  status,
		Limit:   limit,
		Offset:  offset,
	}

	sinceStr := r.URL.Query().Get("since")
	if sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid since parameter: must be RFC3339")
			return
		}
		opts.SinceTime = t
	}

	relations, err := s.store.ListRelations(opts)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Count without limit/offset for the total field.
	countOpts := store.ListRelationsOptions{
		Project:   project,
		Status:    status,
		SinceTime: opts.SinceTime,
	}
	total, err := s.store.CountRelations(countOpts)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"total":     total,
		"limit":     limit,
		"offset":    offset,
		"relations": relations,
	})
}

// handleConflictsStats serves GET /conflicts/stats
// Query params: project (optional — empty means global).
func (s *Server) handleConflictsStats(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")

	stats, err := s.store.GetRelationStats(project)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	payload := map[string]any{
		"project":            stats.Project,
		"by_relation":        stats.ByRelation,
		"by_judgment_status": stats.ByJudgmentStatus,
		"deferred":           stats.DeferredCount,
		"dead":               stats.DeadCount,
	}
	if project != "" && r.Context().Value(publicMemoryReadContext{}) == true {
		delete(payload, "deferred")
		delete(payload, "dead")
		payload["deferred_scope"] = "all_projects_only"
	}
	jsonResponse(w, http.StatusOK, payload)
}

// handleListDeferred serves GET /conflicts/deferred
// Query params: status, limit (default 50, max 500), offset.
func (s *Server) handleListDeferred(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := clampConflictsLimit(queryInt(r, "limit", conflictsDefaultLimit), conflictsDefaultLimit)
	offset := queryInt(r, "offset", 0)

	opts := store.ListDeferredOptions{
		Status: status,
		Limit:  limit,
		Offset: offset,
	}

	rows, err := s.store.ListDeferred(opts)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Phase 3: count total via a second unbounded ListDeferred call.
	// Phase 4 hook: add CountDeferred(status) store method for O(1) count.
	totalOpts := store.ListDeferredOptions{Status: status}
	totalRows, err := s.store.ListDeferred(totalOpts)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"total": len(totalRows),
		"limit": limit,
		"rows":  rows,
	})
}

// handleScanConflicts serves POST /conflicts/scan
// Body: {"project":"X","since":"...","apply":bool,"max_insert":int,
//
//	"semantic":bool,"concurrency":int,"timeout_per_call_seconds":int,"max_semantic":int}
func (s *Server) handleScanConflicts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Project   string `json:"project"`
		Since     string `json:"since"`
		Apply     bool   `json:"apply"`
		MaxInsert int    `json:"max_insert"`

		// Phase 4 semantic fields — all optional, defaults match CLI.
		// Pointer types so absent fields are nil (use default) vs. explicit 0 (invalid).
		Semantic              bool `json:"semantic"`
		Concurrency           *int `json:"concurrency"`
		TimeoutPerCallSeconds *int `json:"timeout_per_call_seconds"`
		MaxSemantic           *int `json:"max_semantic"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.Project == "" {
		jsonError(w, http.StatusBadRequest, "project is required")
		return
	}

	// Resolve semantic params: validate explicit values, apply defaults for absent fields.
	concurrency := 5
	timeoutPerCallSeconds := 60
	maxSemantic := 100

	if body.Semantic {
		if body.Concurrency != nil {
			concurrency = *body.Concurrency
		}
		if body.TimeoutPerCallSeconds != nil {
			timeoutPerCallSeconds = *body.TimeoutPerCallSeconds
		}
		if body.MaxSemantic != nil {
			maxSemantic = *body.MaxSemantic
		}

		// Validate concurrency range [1, 20].
		if concurrency < 1 || concurrency > 20 {
			jsonError(w, http.StatusBadRequest,
				fmt.Sprintf("concurrency must be between 1 and 20; got %d", concurrency))
			return
		}

		// Validate timeout range [1, 600].
		if timeoutPerCallSeconds < 1 || timeoutPerCallSeconds > 600 {
			jsonError(w, http.StatusBadRequest,
				fmt.Sprintf("timeout_per_call_seconds must be between 1 and 600; got %d", timeoutPerCallSeconds))
			return
		}
	}

	opts := store.ScanOptions{
		Project:   body.Project,
		Apply:     body.Apply,
		MaxInsert: body.MaxInsert,
	}
	if body.Since != "" {
		t, err := time.Parse(time.RFC3339, body.Since)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "invalid since: must be RFC3339")
			return
		}
		opts.Since = t
	}

	// Wire semantic options when requested.
	if body.Semantic {
		if s.runnerFactory == nil {
			jsonError(w, http.StatusInternalServerError,
				"RECALLDOCK_AGENT_CLI not set: server runnerFactory is not configured")
			return
		}

		name := os.Getenv("RECALLDOCK_AGENT_CLI")
		runner, err := s.runnerFactory(name)
		if err != nil {
			jsonError(w, http.StatusInternalServerError,
				"failed to resolve agent runner: "+err.Error())
			return
		}

		promptBuilder := s.promptBuilder
		if promptBuilder == nil {
			// Fallback: empty prompt (adequate for tests without LLM).
			promptBuilder = func(a, b store.ObservationSnippet) string { return "" }
		}

		opts.Semantic = true
		opts.Concurrency = concurrency
		opts.TimeoutPerCall = time.Duration(timeoutPerCallSeconds) * time.Second
		opts.MaxSemantic = maxSemantic
		opts.Runner = runner
		opts.BuildPrompt = promptBuilder
	}

	result, err := s.store.ScanProject(opts)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := map[string]any{
		"project":          result.Project,
		"inspected":        result.Inspected,
		"candidates_found": result.CandidatesFound,
		"already_related":  result.AlreadyRelated,
		"inserted":         result.RelationsInserted,
		"capped":           result.Capped,
		"dry_run":          result.DryRun,
		// Semantic counters — always present; zero when semantic=false.
		"semantic_judged":  result.SemanticJudged,
		"semantic_skipped": result.SemanticSkipped,
		"semantic_errors":  result.SemanticErrors,
	}
	if result.Capped {
		resp["warning"] = "cap reached: not all candidates were inserted"
	}

	jsonResponse(w, http.StatusOK, resp)
}

// handleJudgeConflict serves POST /conflicts/judge.
// Body: {"judgment_id":"rel-...","relation":"related|compatible|scoped|conflicts_with|supersedes|not_conflict", ...}
func (s *Server) handleJudgeConflict(w http.ResponseWriter, r *http.Request) {
	var body struct {
		JudgmentID string   `json:"judgment_id"`
		Relation   string   `json:"relation"`
		Reason     string   `json:"reason"`
		Evidence   string   `json:"evidence"`
		Confidence *float64 `json:"confidence"`
		SessionID  string   `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(body.JudgmentID) == "" {
		jsonError(w, http.StatusBadRequest, "judgment_id is required")
		return
	}
	if strings.TrimSpace(body.Relation) == "" {
		jsonError(w, http.StatusBadRequest, "relation is required")
		return
	}

	var reason *string
	if body.Reason != "" {
		reason = &body.Reason
	}
	var evidence *string
	if body.Evidence != "" {
		evidence = &body.Evidence
	}
	if body.Confidence != nil && (*body.Confidence < 0 || *body.Confidence > 1) {
		jsonError(w, http.StatusBadRequest, "confidence must be between 0.0 and 1.0")
		return
	}

	relation, err := s.store.JudgeRelation(store.JudgeRelationParams{
		JudgmentID:    body.JudgmentID,
		Relation:      body.Relation,
		Reason:        reason,
		Evidence:      evidence,
		Confidence:    body.Confidence,
		MarkedByActor: "agent",
		MarkedByKind:  "agent",
		SessionID:     body.SessionID,
	})
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.notifyWrite()
	jsonResponse(w, http.StatusOK, map[string]any{"relation": relation})
}

// handleCompareMemories serves POST /conflicts/compare.
// Body: {"memory_id_a":1,"memory_id_b":2,"relation":"related", "confidence":0.9, "reasoning":"..."}
func (s *Server) handleCompareMemories(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MemoryIDA  int64    `json:"memory_id_a"`
		MemoryIDB  int64    `json:"memory_id_b"`
		Relation   string   `json:"relation"`
		Confidence *float64 `json:"confidence"`
		Reasoning  string   `json:"reasoning"`
		Model      string   `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if body.MemoryIDA == 0 {
		jsonError(w, http.StatusBadRequest, "memory_id_a is required")
		return
	}
	if body.MemoryIDB == 0 {
		jsonError(w, http.StatusBadRequest, "memory_id_b is required")
		return
	}
	if strings.TrimSpace(body.Relation) == "" {
		jsonError(w, http.StatusBadRequest, "relation is required")
		return
	}
	if strings.TrimSpace(body.Reasoning) == "" {
		jsonError(w, http.StatusBadRequest, "reasoning is required")
		return
	}
	if body.Confidence == nil {
		jsonError(w, http.StatusBadRequest, "confidence is required")
		return
	}
	confidence := *body.Confidence
	if confidence < 0 || confidence > 1 {
		jsonError(w, http.StatusBadRequest, "confidence must be between 0.0 and 1.0")
		return
	}

	obsA, err := s.store.GetObservation(body.MemoryIDA)
	if err != nil {
		jsonError(w, http.StatusNotFound, fmt.Sprintf("observation id=%d not found: %s", body.MemoryIDA, err))
		return
	}
	obsB, err := s.store.GetObservation(body.MemoryIDB)
	if err != nil {
		jsonError(w, http.StatusNotFound, fmt.Sprintf("observation id=%d not found: %s", body.MemoryIDB, err))
		return
	}

	syncID, err := s.store.JudgeBySemantic(store.JudgeBySemanticParams{
		SourceID:   obsA.SyncID,
		TargetID:   obsB.SyncID,
		Relation:   body.Relation,
		Confidence: confidence,
		Reasoning:  body.Reasoning,
		Model:      body.Model,
	})
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	if syncID != "" {
		s.notifyWrite()
	}
	jsonResponse(w, http.StatusOK, map[string]any{"sync_id": syncID})
}

// handleReplayDeferred serves POST /conflicts/deferred/replay
func (s *Server) handleReplayDeferred(w http.ResponseWriter, r *http.Request) {
	result, err := s.store.ReplayDeferred()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"retried":   result.Retried,
		"succeeded": result.Succeeded,
		"failed":    result.Failed,
		"dead":      result.Dead,
	})
}

// handleGetConflict serves GET /conflicts/{relation_id}
// Returns full relation detail with source/target observation snippets.
// Returns 404 when relation_id does not exist.
func (s *Server) handleGetConflict(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("relation_id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid relation_id: must be an integer")
		return
	}

	item, err := s.store.GetRelationByIntID(id)
	if err != nil {
		jsonError(w, http.StatusNotFound, err.Error())
		return
	}

	// Build source/target snippets (first 200 chars of title as snippet proxy).
	// Full observation content snippets are not stored in RelationListItem; for Phase 3
	// the title serves as the accessible snippet from the JOIN.
	jsonResponse(w, http.StatusOK, map[string]any{
		"relation_id":     item.ID,
		"sync_id":         item.SyncID,
		"relation":        item.Relation,
		"judgment_status": item.JudgmentStatus,
		"source_id":       item.SourceID,
		"source_snippet":  item.SourceTitle,
		"target_id":       item.TargetID,
		"target_snippet":  item.TargetTitle,
		"created_at":      item.CreatedAt,
		"updated_at":      item.UpdatedAt,
	})
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func (s *Server) validateSessionProject(w http.ResponseWriter, sessionID, projectName string) bool {
	if strings.TrimSpace(projectName) == "" {
		return true
	}
	projectName, _ = store.NormalizeProject(projectName)
	session, err := s.store.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "session not found")
			return false
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	sessionProject, _ := store.NormalizeProject(session.Project)
	if sessionProject != "" && sessionProject != projectName {
		jsonErrorWithFields(w, http.StatusBadRequest, "session project does not match requested project", map[string]any{
			"code":            "session_project_mismatch",
			"session_project": sessionProject,
			"project":         projectName,
		})
		return false
	}
	return true
}

func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	jsonResponse(w, status, map[string]string{"error": msg})
}

func jsonErrorWithFields(w http.ResponseWriter, status int, msg string, fields map[string]any) {
	payload := map[string]any{"error": msg}
	for key, value := range fields {
		payload[key] = value
	}
	jsonResponse(w, status, payload)
}

func searchOptions(r *http.Request) store.SearchOptions {
	q := r.URL.Query()
	return store.SearchOptions{
		Type: q.Get("type"), Project: q.Get("project"), Scope: q.Get("scope"),
		Limit: queryInt(r, "limit", 10), Offset: queryInt(r, "offset", 0),
		MatchMode: q.Get("match_mode"), RawFTS: queryBool(r, "raw_fts", false),
		Profile: q.Get("profile"), Strategy: q.Get("strategy"), Detail: q.Get("detail"),
		TokenBudget: queryInt(r, "token_budget", 1200), RootPath: q.Get("root_path"),
		MaxDepth: queryInt(r, "max_depth", 0), Trace: queryBool(r, "trace", false),
		ExplainEmpty: queryBool(r, "explain_empty", false), IncludeStale: queryBool(r, "include_stale", false),
		Since: q.Get("since"), Until: q.Get("until"),
	}
}

func queryInt(r *http.Request, key string, defaultVal int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

func queryBool(r *http.Request, key string, defaultVal bool) bool {
	v := r.URL.Query().Get(key)
	if v == "" {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return defaultVal
	}
	return b
}
