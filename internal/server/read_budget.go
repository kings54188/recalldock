package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"recalldock.local/recalldock/internal/store"
)

const maxCapturedReadBytes = 128 << 10

type readCapture struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	oversize bool
}

func (c *readCapture) Header() http.Header { return c.header }
func (c *readCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}
func (c *readCapture) Write(body []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.body.Len()+len(body) > maxCapturedReadBytes {
		c.oversize = true
		return len(body), nil
	}
	c.body.Write(body)
	return len(body), nil
}

const allProjectsBudgetKey = "all-projects"

func (s *Server) initReadBudget() {
	s.readOnce.Do(func() {
		if id := strings.TrimSpace(os.Getenv("RECALLDOCK_READ_BUDGET_ID")); id != "" {
			s.readExplicit = true
			s.readBudget, s.readErr = s.store.ReadBudget(id)
			return
		}
		s.readBudgets = map[string]store.ReadBudgetState{}
	})
}

// readBudgetFor keeps HTTP accounting project-scoped. A single server process
// serves many projects; charging them to one empty-project bucket silently
// mixes unrelated workspaces.
func (s *Server) readBudgetFor(project string) (store.ReadBudgetState, error) {
	if s.readExplicit {
		return s.readBudget, s.readErr
	}
	project, _ = store.NormalizeProject(project)
	if project == "" {
		project = allProjectsBudgetKey
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if state, ok := s.readBudgets[project]; ok {
		return state, nil
	}
	state, err := s.store.CreateReadBudget(project, 0)
	if err != nil {
		return store.ReadBudgetState{}, err
	}
	s.readBudgets[project] = state
	return state, nil
}

func (s *Server) readDenial(w http.ResponseWriter) {
	// Always return the full explanatory envelope. After exhaustion this gate
	// short-circuits before touching any memory data, so repeating this fixed
	// control message carries no unbounded-payload risk; an empty 429 body would
	// leave callers with nothing actionable to diagnose the refusal.
	jsonErrorWithFields(w, http.StatusTooManyRequests, "read activation unavailable/expired/out of scope/exhausted; stop memory reads and let the host inspect it", map[string]any{"code": "read_budget_exhausted"})
}

func foreignProject(value any, project string) bool {
	switch value := value.(type) {
	case map[string]any:
		if _, memoryBody := value["content"]; memoryBody {
			owner, ok := value["project"].(string)
			if !ok {
				owner, ok = value["project_id"].(string)
			}
			if !ok || owner != project {
				return true
			}
		}
		for key, item := range value {
			if key == "project" || key == "project_id" {
				if owner, ok := item.(string); ok && owner != project {
					return true
				}
			}
			if foreignProject(item, project) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if foreignProject(item, project) {
				return true
			}
		}
	}
	return false
}

func (s *Server) boundedRead(next http.Handler, w http.ResponseWriter, r *http.Request) {
	if err := validateReadQuery(r); err != nil {
		jsonErrorWithFields(w, http.StatusBadRequest, err.Error(), map[string]any{"code": "invalid_arguments"})
		return
	}
	if r.URL.Path == "/search" && queryInt(r, "offset", 0) != 0 {
		jsonErrorWithFields(w, http.StatusBadRequest, "Progressive HTTP search does not support offset pagination", map[string]any{"code": "unsupported_offset"})
		return
	}
	s.initReadBudget()
	if s.readErr != nil {
		s.readDenial(w)
		return
	}
	request := r.Clone(context.WithValue(r.Context(), publicMemoryReadContext{}, true))
	if s.readBudget.Project != "" {
		query := request.URL.Query()
		pinned, err := s.store.ResolveExistingProject(s.readBudget.Project)
		if err != nil {
			s.readDenial(w)
			return
		}
		if selected := query.Get("project"); selected != "" {
			selected, err = s.store.ResolveExistingProject(selected)
			if err != nil {
				jsonErrorWithFields(w, http.StatusNotFound, err.Error(), map[string]any{"code": "unknown_project"})
				return
			}
			if selected != pinned {
				s.readDenial(w)
				return
			}
		}
		if queryBool(request, "all_projects", false) {
			s.readDenial(w)
			return
		}
		query.Set("project", pinned)
		request.URL.RawQuery = query.Encode()
	}
	if !s.selectReadProject(w, request) {
		return
	}
	project := request.URL.Query().Get("project")
	if request.URL.Path == "/search" {
		if err := s.store.ValidateObservationType(request.URL.Query().Get("type"), project); err != nil {
			jsonErrorWithFields(w, http.StatusBadRequest, err.Error(), map[string]any{"code": "invalid_type"})
			return
		}
	}
	budget, budgetErr := s.readBudgetFor(project)
	if budgetErr != nil {
		s.readDenial(w)
		return
	}
	if request.URL.Path == "/search" {
		query := request.URL.Query()
		if query.Get("detail") == "" {
			query.Set("detail", "l0")
		}
		if query.Get("strategy") == "" {
			query.Set("strategy", "hierarchical")
		}
		request.URL.RawQuery = query.Encode()
	}
	if request.URL.Path == "/observations" || request.URL.Path == "/observations/recent" {
		query := request.URL.Query()
		if query.Get("detail") == "" {
			query.Set("detail", "l0")
		}
		request.URL.RawQuery = query.Encode()
	}
	// The progressive context view is the network default, not a full history dump.
	if request.URL.Path == "/context" {
		query := request.URL.Query()
		if query.Get("mode") == "" {
			query.Set("mode", "index")
		}
		request.URL.RawQuery = query.Encode()
	}
	capture := &readCapture{header: make(http.Header)}
	next.ServeHTTP(capture, request)
	if capture.oversize {
		s.readDenial(w)
		return
	}
	var decoded any
	_ = json.Unmarshal(capture.body.Bytes(), &decoded)
	budgetProject := budget.Project
	if project != "" && foreignProject(decoded, project) {
		s.readDenial(w)
		return
	}
	receipt := ""
	if object, ok := decoded.(map[string]any); ok && r.Method != http.MethodHead && strings.HasPrefix(request.URL.Path, "/observations/") && object["detail"] != "l0" && object["detail"] != "l1" {
		if syncID, ok := object["sync_id"].(string); ok && syncID != "" {
			receipt = fmt.Sprintf("obs:%s@%.0f", syncID, object["revision_count"])
		}
	}
	if r.Method == http.MethodHead {
		capture.body.Reset()
	}
	capture.header.Set("X-RecallDock-Budget-Limit", fmt.Sprint(budget.Limit))
	capture.header.Set("X-RecallDock-Budget-Used", fmt.Sprint(budget.Limit))
	capture.header.Set("X-RecallDock-Budget-Remaining", fmt.Sprint(budget.Limit))
	capture.header.Set("X-RecallDock-Budget-Estimator", store.ReadBudgetEstimator)
	wire, _ := json.Marshal(map[string]any{"body": capture.body.String(), "headers": capture.header, "status": capture.status})
	state, err := s.store.ConsumeReadBudget(budget.ID, budgetProject, store.EstimateReadTokens(string(wire))+128, receipt)
	if err != nil {
		s.readDenial(w)
		return
	}
	body := capture.body.Bytes()
	if state.Duplicate {
		body, _ = json.Marshal(map[string]any{"already_loaded": true, "content_omitted": true})
		smallWire, _ := json.Marshal(map[string]any{"body": string(body), "headers": capture.header, "status": capture.status})
		state, err = s.store.ConsumeReadBudget(budget.ID, budgetProject, store.EstimateReadTokens(string(smallWire))+128, "")
		if err != nil {
			s.readDenial(w)
			return
		}
	}
	for key, values := range capture.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Del("Content-Length")
	w.Header().Set("X-RecallDock-Budget-Used", fmt.Sprint(state.Used))
	w.Header().Set("X-RecallDock-Budget-Remaining", fmt.Sprint(state.Limit-state.Used))
	status := capture.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
