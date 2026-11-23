package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/store"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func TestSearchRetrievalSchemaAndStructuredTrace(t *testing.T) {
	s := newMCPTestStore(t)
	tool := NewServer(s).GetTool("mem_search")
	if tool == nil {
		t.Fatal("mem_search not registered")
	}
	for _, field := range []string{"strategy", "detail", "token_budget", "root_path", "max_depth", "trace", "include_stale"} {
		if _, ok := tool.Tool.InputSchema.Properties[field]; !ok {
			t.Errorf("mem_search schema missing %s", field)
		}
	}

	dir := t.TempDir()
	initTestGitRepo(t, dir)
	t.Chdir(dir)
	if err := s.CreateSession("retrieval-mcp", "retrieval-mcp", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{
		SessionID: "retrieval-mcp", Type: "decision", Title: "Trace contract",
		Content: "FULL-CONTENT-MARKER must not be copied into trace metadata.", Project: "retrieval-mcp",
		MemoryClass: store.MemoryClassProject, MemoryPath: "project/design/search",
		Abstract: "Trace contract", Overview: "Machine-readable retrieval decisions.",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := handleSearch(s, MCPConfig{DefaultProject: "retrieval-mcp"}, NewSessionActivity(time.Minute))(
		context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
			"query": "trace contract", "strategy": "hierarchical", "detail": "l0",
			"token_budget": 1200.0, "max_depth": 3.0, "trace": true,
		}}},
	)
	if err != nil || result.IsError {
		t.Fatalf("search err=%v result=%q", err, callResultText(t, result))
	}
	body := callResultJSON(t, result)
	if body["strategy_used"] != store.SearchStrategyHierarchical || body["estimator"] != store.SearchTokenEstimator {
		t.Fatalf("retrieval metadata=%#v", body)
	}
	trace, ok := body["trace"].([]any)
	if !ok || len(trace) == 0 {
		t.Fatalf("trace=%#v", body["trace"])
	}
	if strings.Contains(callResultText(t, result), "FULL-CONTENT-MARKER") {
		t.Fatal("l0 search response exposed full content")
	}
	for _, raw := range trace {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("trace entry=%#v", raw)
		}
		if _, hasContent := entry["content"]; hasContent {
			t.Fatalf("trace entry exposed content=%#v", entry)
		}
	}
}
