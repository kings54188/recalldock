package server

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"recalldock.local/recalldock/internal/store"
)

type publicMemoryReadContext struct{}

// selectReadProject gates collection queries before touching history. Exact
// resource URLs remain explicit selections; list/search defaults need a bound
// directory, explicit project, or explicit cross-project selection.
func (s *Server) selectReadProject(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	selected := strings.TrimSpace(q.Get("project"))
	all := queryBool(r, "all_projects", false)
	if selected != "" && all {
		jsonErrorWithFields(w, 400, "Choose project or all_projects, not both", map[string]any{"code": "project_selector_conflict"})
		return false
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	needsSelection := false
	switch r.URL.Path {
	case "/search", "/context", "/stats", "/doctor", "/timeline", "/review",
		"/observations", "/observations/recent", "/sessions/recent", "/prompts/recent", "/prompts/search",
		"/conflicts", "/conflicts/stats", "/conflicts/deferred":
		needsSelection = true
	default:
		needsSelection = len(parts) == 2 && (parts[0] == "observations" || parts[0] == "sessions" || parts[0] == "conflicts")
	}
	if !needsSelection {
		return true
	}
	if selected == "" && !all {
		cwd := strings.TrimSpace(q.Get("cwd"))
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				jsonError(w, 500, "working directory unavailable")
				return false
			}
		}
		detected := s.store.DetectExplicitProject(cwd)
		if detected.Error != nil {
			jsonErrorWithFields(w, 400, detected.Error.Error(), map[string]any{"code": "project_binding_invalid"})
			return false
		}
		if detected.Project == "" {
			jsonErrorWithFields(w, 400, "Bind a directory or select an exact historical project; cwd/git names do not activate history", map[string]any{"code": "project_not_selected"})
			return false
		}
		selected = detected.Project
	}
	if selected != "" {
		var err error
		selected, err = s.store.ResolveExistingProject(selected)
		if err != nil {
			if strings.Contains(err.Error(), "unknown_project") {
				jsonErrorWithFields(w, 404, err.Error(), map[string]any{"code": "unknown_project"})
			} else {
				jsonError(w, 500, err.Error())
			}
			return false
		}
		q.Set("project", selected)
		r.URL.RawQuery = q.Encode()
		if r.URL.Path == "/conflicts/deferred" {
			jsonErrorWithFields(w, 400, "Deferred rows do not support project filtering; select all_projects explicitly", map[string]any{"code": "project_scope_unsupported"})
			return false
		}
		var owner string
		check := false
		if r.URL.Path == "/timeline" {
			id, _ := strconv.ParseInt(timelineObservationID(r), 10, 64)
			owner, err = s.store.ObservationProject(id)
			check = true
		} else if len(parts) == 2 && parts[0] == "observations" && parts[1] != "recent" {
			id, _ := strconv.ParseInt(parts[1], 10, 64)
			owner, err = s.store.ObservationProject(id)
			check = true
		} else if len(parts) == 2 && parts[0] == "sessions" && parts[1] != "recent" {
			owner, err = s.store.SessionProject(parts[1])
			check = true
		} else if len(parts) == 2 && parts[0] == "conflicts" && parts[1] != "stats" {
			id, _ := strconv.ParseInt(parts[1], 10, 64)
			var relation *store.RelationListItem
			relation, err = s.store.GetRelationByIntID(id)
			if err == nil {
				owner, err = s.store.ObservationProjectBySyncID(relation.SourceID)
				if err == nil && owner == selected {
					owner, err = s.store.ObservationProjectBySyncID(relation.TargetID)
				}
			}
			check = true
		}
		if check {
			if err != nil {
				jsonError(w, 404, "Selected resource unavailable")
				return false
			}
			if owner != selected {
				jsonErrorWithFields(w, 403, "Selected resource belongs to another project activation", map[string]any{"code": "project_scope_mismatch"})
				return false
			}
		}
	}
	return true
}

func timelineObservationID(r *http.Request) string {
	if id := r.URL.Query().Get("observation_id"); id != "" {
		return id
	}
	return r.URL.Query().Get("id")
}

func validateReadQuery(r *http.Request) error {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return fmt.Errorf("invalid query: %w", err)
	}
	fields := "project all_projects cwd"
	switch r.URL.Path {
	case "/search":
		fields += " q type scope limit offset match_mode raw_fts profile strategy detail token_budget root_path max_depth trace explain_empty include_stale since until"
	case "/observations", "/observations/recent":
		fields += " scope limit detail"
		if r.URL.Path == "/observations" {
			fields += " sort"
		}
	case "/sessions/recent", "/prompts/recent":
		fields += " limit"
	case "/prompts/search":
		fields += " q limit"
	case "/timeline":
		fields += " observation_id id before after"
	case "/review":
		fields += " limit status"
	case "/context":
		fields += " mode scope token_budget"
	case "/doctor":
		fields += " check"
	case "/conflicts":
		fields += " status limit offset since"
	case "/conflicts/deferred":
		fields += " status limit offset"
	case "/stats", "/conflicts/stats", "/sync/status":
	case "/project/current":
		fields = "cwd"
	default:
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 2 {
			return nil
		}
		switch parts[0] {
		case "observations":
			fields += " detail"
			if id, err := strconv.ParseInt(parts[1], 10, 64); err != nil || id < 1 {
				return fmt.Errorf("invalid observation id")
			}
		case "conflicts":
			if id, err := strconv.ParseInt(parts[1], 10, 64); err != nil || id < 1 {
				return fmt.Errorf("invalid relation_id")
			}
		case "sessions":
		default:
			return nil
		}
	}
	for key, values := range q {
		if !strings.Contains(" "+fields+" ", " "+key+" ") {
			return fmt.Errorf("unknown query parameter %q", key)
		}
		if len(values) != 1 {
			return fmt.Errorf("duplicate query parameter %q", key)
		}
		if strings.TrimSpace(values[0]) == "" {
			return fmt.Errorf("query parameter %q must not be blank", key)
		}
		switch key {
		case "limit", "offset", "before", "after", "token_budget", "max_depth", "observation_id", "id":
			value, err := strconv.ParseInt(values[0], 10, 64)
			min, max := int64(0), int64(10000)
			switch key {
			case "limit":
				min, max = 1, conflictsMaxLimit
			case "before", "after":
				max = 100
			case "token_budget":
				max = 1000000
			case "max_depth":
				max = 100
			case "observation_id", "id":
				min, max = 1, int64(^uint64(0)>>1)
			}
			if err != nil || value < min || value > max {
				return fmt.Errorf("%s must be an integer between %d and %d", key, min, max)
			}
		case "all_projects", "trace", "raw_fts", "explain_empty", "include_stale":
			if _, err := strconv.ParseBool(values[0]); err != nil {
				return fmt.Errorf("%s must be a boolean", key)
			}
		}
	}
	if q.Get("project") != "" && queryBool(r, "all_projects", false) {
		return fmt.Errorf("project_selector_conflict: choose project or all_projects, not both")
	}
	if q.Get("scope") != "" {
		if _, err := store.NormalizeReadScope(q.Get("scope")); err != nil {
			return err
		}
	}
	switch r.URL.Path {
	case "/search":
		if strings.TrimSpace(q.Get("q")) == "" {
			return store.ErrQueryRequired
		}
		opts := searchOptions(r)
		return store.NormalizeSearchOptions(&opts)
	case "/prompts/search":
		if strings.TrimSpace(q.Get("q")) == "" {
			return store.ErrQueryRequired
		}
	case "/timeline":
		if q.Has("id") && q.Has("observation_id") {
			return fmt.Errorf("use either id or observation_id, not both")
		}
		if timelineObservationID(r) == "" {
			return fmt.Errorf("observation_id parameter is required")
		}
	case "/review":
		if value := q.Get("status"); value != "" && value != "active" && value != "stale" {
			return fmt.Errorf("status must be active or stale")
		}
	case "/conflicts":
		if value := q.Get("status"); value != "" && value != store.JudgmentStatusPending && value != store.JudgmentStatusJudged && value != store.JudgmentStatusOrphaned && value != store.JudgmentStatusIgnored {
			return fmt.Errorf("invalid judgment status")
		}
		if value := q.Get("since"); value != "" {
			if _, err := time.Parse(time.RFC3339, value); err != nil {
				return fmt.Errorf("since must be RFC3339")
			}
		}
	case "/conflicts/deferred":
		if value := q.Get("status"); value != "" && value != "deferred" && value != "dead" {
			return fmt.Errorf("status must be deferred or dead")
		}
	case "/doctor":
		if queryBool(r, "all_projects", false) {
			return fmt.Errorf("doctor requires a single project; all_projects is unsupported")
		}
	case "/context":
		mode := strings.ToLower(strings.TrimSpace(q.Get("mode")))
		if mode != "" && mode != "full" && mode != "index" && mode != "l0" && mode != "l1" {
			return fmt.Errorf("mode must be full, index, l0 or l1")
		}
		if mode == "full" && queryInt(r, "token_budget", 0) > 0 {
			return fmt.Errorf("full context does not support token_budget")
		}
	}
	if detail := strings.ToLower(strings.TrimSpace(q.Get("detail"))); detail != "" && detail != "l0" && detail != "l1" && detail != "l2" {
		return fmt.Errorf("detail must be l0, l1 or l2")
	}
	if sort := q.Get("sort"); sort != "" && sort != "created_at:desc" {
		return fmt.Errorf("unsupported sort: use created_at:desc")
	}
	return nil
}
