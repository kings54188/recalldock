package mcp

import (
	"context"
	"strings"
	"testing"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"recalldock.local/recalldock/internal/store"
)

func mcpToolText(t *testing.T, srv *server.MCPServer, name string, args map[string]any) *mcppkg.CallToolResult {
	t.Helper()
	tool := srv.GetTool(name)
	if tool == nil {
		t.Fatalf("tool %q not registered", name)
	}
	r, err := tool.Handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: name, Arguments: args}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMCPEmptySearchAndTinyBudgetUseErrorChannel(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("errors", "errors-project", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "errors", Project: "errors-project", Type: "decision", Title: "Budget marker", Content: "budget marker body", MemoryClass: store.MemoryClassProject}); err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "errors-project"}, map[string]bool{"mem_search": true})
	empty := mcpToolText(t, srv, "mem_search", map[string]any{"query": "  "})
	if !empty.IsError || !strings.Contains(callResultText(t, empty), "query_required") {
		t.Fatalf("empty query not rejected on error channel: %s", callResultText(t, empty))
	}
	tooSmall := mcpToolText(t, srv, "mem_search", map[string]any{"query": "budget marker", "token_budget": 1.0})
	if !tooSmall.IsError || !strings.Contains(callResultText(t, tooSmall), "budget_too_small") {
		t.Fatalf("tiny budget became an empty success: %s", callResultText(t, tooSmall))
	}
}

func TestMCPSearchParameterErrorsDoNotSuggestKeywords(t *testing.T) {
	s := newMCPTestStore(t)
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "errors-project"}, map[string]bool{"mem_search": true})
	for _, args := range []map[string]any{
		{"query": "marker", "detail": "l2"},
		{"query": "marker", "strategy": "vector"},
		{"query": "marker", "token_budget": -1.0},
		{"query": "marker", "max_depth": -1.0},
	} {
		r := mcpToolText(t, srv, "mem_search", args)
		if !r.IsError || strings.Contains(callResultText(t, r), "Try simpler keywords") {
			t.Fatalf("misleading parameter error: %s", callResultText(t, r))
		}
	}
}

func TestMCPSearchBudgetDenialsDoNotRecordRecalls(t *testing.T) {
	for _, remaining := range []int{0, 1} {
		t.Run(string(rune('0'+remaining)), func(t *testing.T) {
			s := newMCPTestStore(t)
			if err := s.CreateSession("denied-recall", "denied-project", t.TempDir()); err != nil {
				t.Fatal(err)
			}
			id, err := s.AddObservation(store.AddObservationParams{SessionID: "denied-recall", Project: "denied-project", Type: "decision", Title: "图标", Content: "图标尺寸验证"})
			if err != nil {
				t.Fatal(err)
			}
			state, err := s.CreateReadBudget("denied-project", store.DefaultReadBudget)
			if err != nil {
				t.Fatal(err)
			}
			srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "denied-project", ReadBudgetID: state.ID}, map[string]bool{"mem_search": true})
			state, err = s.ReadBudget(state.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ConsumeReadBudget(state.ID, state.Project, state.Limit-state.Used-remaining, ""); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				r := mcpToolText(t, srv, "mem_search", map[string]any{"query": "图标"})
				if !r.IsError || !strings.Contains(callResultText(t, r), "read_budget_exhausted") {
					t.Fatalf("budget denial: %s", callResultText(t, r))
				}
			}
			o, err := s.GetObservation(id)
			if err != nil || o.RecallCount != 0 {
				t.Fatalf("rejected recall counted: %+v, %v", o, err)
			}
			if remaining == 0 {
				var count int
				if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='observations_cjk_fts'").Scan(&count); err != nil || count != 0 {
					t.Fatalf("exhausted budget still searched: %d, %v", count, err)
				}
			}
		})
	}
}

func TestMCPSearchRejectsUnknownAndMistypedArguments(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("strict-args", "args-project", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "strict-args", Project: "args-project", Title: "marker", Content: "marker", Type: "decision"}); err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "args-project"}, map[string]bool{"mem_search": true})
	tool := srv.GetTool("mem_search")
	if tool.Tool.InputSchema.AdditionalProperties != false {
		t.Fatalf("inputSchema not strict: %#v", tool.Tool.InputSchema)
	}
	for key, value := range map[string]any{"score_threshold": 0.9, "unknown_param_xyz": "ignored", "matchmode": "any", "match_mode": true, "all_projects": "true", "token_budget": 0.5, "limit": 1.5} {
		r := mcpToolText(t, srv, "mem_search", map[string]any{"query": "marker", key: value})
		if !r.IsError || !strings.Contains(callResultText(t, r), key) {
			t.Fatalf("silently ignored %s: %s", key, callResultText(t, r))
		}
	}
}

func TestMCPReadSelectorsRejectInvalidEnums(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("enum-test", "enum-project", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "enum-test", Project: "enum-project", Type: "decision", Title: "kicker", Content: "kicker canvas"}); err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "enum-project"}, map[string]bool{"mem_search": true, "mem_context": true})
	for _, name := range []string{"mem_search", "mem_context"} {
		for _, mode := range []string{"index", "l0", "l1", "full"} {
			args := map[string]any{"scope": "bogus_scope"}
			if name == "mem_search" {
				args["query"] = "kicker"
			} else {
				args["mode"] = mode
			}
			r := mcpToolText(t, srv, name, args)
			if !r.IsError || !strings.Contains(callResultText(t, r), "invalid_scope") {
				t.Fatalf("invalid scope accepted by %s: %s", name, callResultText(t, r))
			}
		}
	}
	r := mcpToolText(t, srv, "mem_search", map[string]any{"query": "kicker", "type": "bogus_type"})
	if !r.IsError || !strings.Contains(callResultText(t, r), "invalid_type") {
		t.Fatalf("invalid type returned success: %s", callResultText(t, r))
	}
}

func TestMCPReadBudgetRowsAreProjectScoped(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("scoped-budget", "scoped-project", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "scoped-budget", Project: "scoped-project", Type: "decision", Title: "Scoped", Content: "scoped body", MemoryClass: store.MemoryClassProject}); err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "scoped-project"}, map[string]bool{"mem_context": true})
	result := mcpToolText(t, srv, "mem_context", map[string]any{"mode": "index"})
	if result.IsError {
		t.Fatalf("scoped context failed: %s", callResultText(t, result))
	}
	rows, err := s.DB().Query(`SELECT project FROM read_budgets`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			t.Fatal(err)
		}
		if project == "" {
			t.Fatal("read budget row has an empty project")
		}
	}
}

func TestMCPContextErrorIsNotWrappedAsSuccess(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("ctx-error", "ctx-project", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "ctx-project"}, map[string]bool{"mem_context": true})
	r := mcpToolText(t, srv, "mem_context", map[string]any{"mode": "not-a-mode"})
	if !r.IsError {
		t.Fatalf("invalid context mode returned success: %s", callResultText(t, r))
	}
	if strings.Contains(callResultText(t, r), "read_budget") {
		t.Fatalf("error was wrapped in a successful budget envelope: %s", callResultText(t, r))
	}
}
