package mcp

import (
	"math"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func argumentStrings(value any) []string {
	if values, ok := value.([]string); ok {
		return values
	}
	var result []string
	if values, ok := value.([]any); ok {
		for _, item := range values {
			result = append(result, item.(string))
		}
	}
	return result
}

func TestRawSearchSyntaxHasClassifiedError(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("raw-query", "raw-query", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "raw-query", Project: "raw-query", Type: "decision", Title: "Raw query marker", Content: "marker"}); err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithConfig(s, MCPConfig{}, map[string]bool{"mem_search": true})
	for _, query := range []string{`foo"bar*baz`, "missingcolumn:marker"} {
		r := mcpToolText(t, srv, "mem_search", map[string]any{"query": query, "raw_fts": true, "all_projects": true})
		text := callResultText(t, r)
		if !r.IsError || !strings.Contains(text, "invalid_query") || strings.Contains(text, "SQL logic error") {
			t.Fatalf("raw syntax not classified: %s", text)
		}
	}
}

func TestSearchEnumsAreDeclaredAndCheckedBeforeScope(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newMCPTestStore(t)
	srv := NewServerWithConfig(s, MCPConfig{}, map[string]bool{"mem_search": true})
	for _, key := range []string{"match_mode", "profile", "strategy", "detail"} {
		property := srv.GetTool("mem_search").Tool.InputSchema.Properties[key].(map[string]any)
		enum := argumentStrings(property["enum"])
		if len(enum) == 0 {
			t.Errorf("%s has no schema enum", key)
		}
		for _, value := range enum {
			r := mcpToolText(t, srv, "mem_search", map[string]any{"query": "marker", key: value, "all_projects": true})
			if r.IsError {
				t.Errorf("valid %s=%q rejected: %s", key, value, callResultText(t, r))
			}
		}
		for _, all := range []bool{false, true} {
			args := map[string]any{"query": "marker", key: "bogus"}
			if all {
				args["all_projects"] = true
			}
			r := mcpToolText(t, srv, "mem_search", args)
			if !r.IsError || !strings.Contains(callResultText(t, r), `"error_code":"invalid_arguments"`) {
				t.Errorf("%s all=%v: %s", key, all, callResultText(t, r))
			}
		}
	}
}

func TestErrorMessagesDoNotRepeatTheirCode(t *testing.T) {
	for _, code := range []string{"invalid_arguments", "invalid_query", "query_has_no_terms", "budget_too_small", "invalid_type"} {
		result := errorWithMeta(code, code+": explanatory message", nil)
		body := callResultJSON(t, result)
		if body["error_code"] != code || body["message"] != "explanatory message" {
			t.Errorf("code duplicated in message: %#v", body)
		}
	}
}

func TestSearchOptionErrorsRemainActionable(t *testing.T) {
	s := newMCPTestStore(t)
	srv := NewServerWithConfig(s, MCPConfig{}, map[string]bool{"mem_search": true})
	for _, options := range []map[string]any{{"strategy": "bogus"}, {"detail": "l2"}, {"root_path": "../bad"}} {
		options["query"], options["all_projects"] = "marker", true
		r := mcpToolText(t, srv, "mem_search", options)
		if !r.IsError || !strings.Contains(callResultText(t, r), "invalid_arguments") {
			t.Fatalf("parameter error mislabeled: %s", callResultText(t, r))
		}
	}
}

func TestEveryRegisteredToolRejectsUnknownArguments(t *testing.T) {
	s := newMCPTestStore(t)
	srv := NewServerWithConfig(s, MCPConfig{}, nil)
	for name := range srv.ListTools() {
		t.Run(name, func(t *testing.T) {
			r := mcpToolText(t, srv, name, map[string]any{"bogus_key": 1.0})
			if !r.IsError || !strings.Contains(callResultText(t, r), "invalid_arguments") || !strings.Contains(callResultText(t, r), "bogus_key") {
				t.Fatalf("unknown key ignored or checked after policy: %s", callResultText(t, r))
			}
			if srv.GetTool(name).Tool.InputSchema.AdditionalProperties != false {
				t.Fatal("schema must advertise strict arguments")
			}
		})
	}
}

func TestMCPArgumentsValidateBeforeScopeAndPolicy(t *testing.T) {
	s := newMCPTestStore(t)
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "missing-project"}, nil)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"mem_context", map[string]any{"limit": 5.0}},
		{"mem_context", map[string]any{"mode": "bogus"}},
		{"mem_context", map[string]any{"all_projects": "true"}},
		{"mem_context", map[string]any{"token_budget": 1.9}},
		{"mem_review", map[string]any{"action": "no_such_action"}},
		{"mem_review", map[string]any{"action": "list", "status": "bogus"}},
		{"mem_get_observation", map[string]any{"id": 1.9}},
		{"mem_get_observation", map[string]any{"id": 1.0000001}},
		{"mem_get_observation", map[string]any{"id": math.Inf(1)}},
		{"mem_get_observation", map[string]any{"id": math.Ldexp(1, 63)}},
		{"mem_timeline", map[string]any{"observation_id": 2.99}},
		{"mem_suggest_topic_key", map[string]any{"title": 5.0}},
	} {
		r := mcpToolText(t, srv, tc.name, tc.args)
		if !r.IsError || !strings.Contains(callResultText(t, r), "invalid_arguments") {
			t.Errorf("%s %#v: %s", tc.name, tc.args, callResultText(t, r))
		}
	}
}
