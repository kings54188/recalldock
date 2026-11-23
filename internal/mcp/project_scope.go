package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"recalldock.local/recalldock/internal/memorypolicy"
	projectpkg "recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
)

// guardProjectScope runs before memory queries. A directory basename or git
// remote is a discovery hint, never activation of a similarly named history.
// Explicit historical read selectors remain available without a write binding.
func guardProjectScope(s *store.Store, cfg MCPConfig, name string, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		decision := memorypolicy.MCP(name, r.GetString("action", ""))
		if decision == memorypolicy.Lifecycle || decision == memorypolicy.Propose {
			return guardSessionProject(s, cfg, name, next)(ctx, r)
		}
		if decision != memorypolicy.Read || name == "mem_suggest_topic_key" {
			return next(ctx, r)
		}
		selected, _ := store.NormalizeProject(strings.TrimSpace(r.GetString("project", "")))
		bound, _ := store.NormalizeProject(cfg.DefaultProject)
		deny := func(code, message string) (*mcp.CallToolResult, error) {
			body, _ := json.Marshal(map[string]any{"error_code": code, "message": message, "project": bound})
			return mcp.NewToolResultError(string(body)), nil
		}
		validatedNext := func(project string) (*mcp.CallToolResult, error) {
			exists, err := s.ProjectExists(project)
			if err != nil {
				return deny("project_lookup_failed", "Unable to validate the selected project.")
			}
			if !exists {
				return deny("unknown_project", "Selected project is not in this store; choose an exact known identity. No other project history is enumerated.")
			}
			project, err = s.ResolveExistingProject(project)
			if err != nil {
				return deny("project_lookup_failed", "Unable to validate the selected project.")
			}
			if name == "mem_get_observation" || name == "mem_timeline" {
				key := "id"
				if name == "mem_timeline" {
					key = "observation_id"
				}
				owner, err := s.ObservationProject(int64(intArg(r, key, 0)))
				if err != nil {
					return deny("observation_not_found", "Selected observation is unavailable.")
				}
				if owner != project {
					return deny("project_scope_mismatch", "Selected resource belongs to another project activation.")
				}
			}
			if name == "mem_timeline" {
				args := map[string]any{}
				for k, v := range r.GetArguments() {
					args[k] = v
				}
				args["project"] = project
				r.Params.Arguments = args
			}
			return next(ctx, r)
		}
		all := r.GetBool("all_projects", false)
		if all && selected != "" {
			return deny("project_selector_conflict", "Choose one project or all_projects, not both.")
		}
		if bound != "" {
			if all || selected != "" && selected != bound {
				return deny("project_scope_mismatch", "The trusted process activation is scoped to one project; select another activation outside tool arguments.")
			}
			return validatedNext(bound)
		}
		if selected != "" {
			return validatedNext(selected)
		}
		if all {
			if name == "mem_timeline" {
				owner, err := s.ObservationProject(int64(intArg(r, "observation_id", 0)))
				if err != nil {
					return deny("observation_not_found", "Selected observation is unavailable.")
				}
				args := map[string]any{}
				for k, v := range r.GetArguments() {
					args[k] = v
				}
				args["project"] = owner
				r.Params.Arguments = args
			}
			return next(ctx, r)
		}
		if def, ok := sessionProjectDefaultFor(s); ok {
			if name == "mem_current_project" {
				return respondWithProject(s, projectpkg.DetectionResult{Project: def.project, Source: projectpkg.SourceSessionProject, Path: def.path}, "Session default project; no memory content loaded.", nil), nil
			}
			return validatedNext(def.project)
		}
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		detected := s.DetectExplicitProject(cwd)
		if detected.Error != nil {
			return deny("project_binding_invalid", detected.Error.Error())
		}
		if detected.Project == "" {
			return deny("project_not_selected", "No explicit project is selected. Bind this directory, select an exact historical project, or pass all_projects=true for a read that supports cross-project selection; cwd/git names do not activate history.")
		}
		if name == "mem_current_project" {
			return respondWithProject(s, detected, "Explicit project activation; no memory content loaded.", nil), nil
		}
		return validatedNext(detected.Project)
	}
}

// Session IDs and directory arguments cannot broaden a trusted process pin.
// This wraps the registered handler (including SDK task calls), just like reads.
// It checks project ownership, not host/session authenticity.
func guardSessionProject(s *store.Store, cfg MCPConfig, name string, next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		bound, _ := store.NormalizeProject(strings.TrimSpace(cfg.DefaultProject))
		if bound == "" {
			return next(ctx, r)
		}
		deny := func() (*mcp.CallToolResult, error) {
			return errorWithMeta("project_scope_mismatch", "Session metadata and proposals must remain in the trusted process project activation.", nil), nil
		}
		selected, _ := store.NormalizeProject(strings.TrimSpace(r.GetString("project", "")))
		if selected != "" && selected != bound {
			return deny()
		}
		key := "id"
		if name == "mem_commit_session" {
			key = "session_id"
		}
		if name == "mem_session_start" {
			directory := strings.TrimSpace(r.GetString("directory", ""))
			if directory == "" {
				var err error
				directory, err = os.Getwd()
				if err != nil {
					return nil, err
				}
			}
			detected := s.DetectExplicitProject(directory)
			if detected.Error != nil {
				return errorWithMeta("project_binding_invalid", "Session directory binding is invalid; use a valid directory or ask the operator to repair its binding.", nil), nil
			}
			if detected.Project != "" && detected.Project != bound {
				return deny()
			}
		}
		owner, err := s.SessionProject(strings.TrimSpace(r.GetString(key, "")))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return errorWithMeta("session_lookup_failed", "Unable to validate session ownership.", nil), nil
		}
		if err == nil && owner != bound {
			return deny()
		}
		return next(ctx, r)
	}
}
