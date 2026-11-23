package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/store"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func TestMemoryClassToolSchemas(t *testing.T) {
	s := newMCPTestStore(t)
	srv := NewServer(s)
	tests := map[string][]string{
		"mem_save":   {"memory_class"},
		"mem_update": {"memory_class"},
		"mem_search": {"profile"},
	}
	for toolName, fields := range tests {
		tool := srv.GetTool(toolName)
		if tool == nil {
			t.Fatalf("%s not registered", toolName)
		}
		for _, field := range fields {
			if _, ok := tool.Tool.InputSchema.Properties[field]; !ok {
				t.Errorf("%s schema missing %s", toolName, field)
			}
		}
	}
}

func TestMemoryClassHandlersSaveUpdateAndSearchProfile(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	t.Chdir(dir)

	s := newMCPTestStore(t)
	cfg := MCPConfig{DefaultProject: "memory-class-mcp"}
	activity := NewSessionActivity(10 * time.Minute)
	save := handleSave(s, cfg, activity)
	saveResult, err := save(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"title": "classified handler memory", "content": "classified handler phase2", "type": "decision",
		"memory_class": store.MemoryClassAgent, "capture_prompt": false,
	}}})
	if err != nil || saveResult.IsError {
		t.Fatalf("save: err=%v isError=%v text=%q", err, saveResult.IsError, callResultText(t, saveResult))
	}
	results, err := s.Search("classified handler phase2", store.SearchOptions{Project: "memory-class-mcp", Limit: 5})
	if err != nil || len(results) != 1 {
		t.Fatalf("Search saved memory: err=%v results=%+v", err, results)
	}
	if results[0].MemoryClass != store.MemoryClassAgent {
		t.Fatalf("saved memory_class=%q", results[0].MemoryClass)
	}

	update := handleUpdate(s)
	updateResult, err := update(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"id": float64(results[0].ID), "memory_class": store.MemoryClassUser,
	}}})
	if err != nil || updateResult.IsError {
		t.Fatalf("update: err=%v isError=%v text=%q", err, updateResult.IsError, callResultText(t, updateResult))
	}
	updated, err := s.GetObservation(results[0].ID)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if updated.MemoryClass != store.MemoryClassUser {
		t.Fatalf("updated memory_class=%q", updated.MemoryClass)
	}

	if _, err := s.AddObservation(store.AddObservationParams{
		SessionID: updated.SessionID, Type: "checkpoint", Title: "operational handler", Content: "classified handler phase2 operational",
		Project: "memory-class-mcp", MemoryClass: store.MemoryClassOperational,
	}); err != nil {
		t.Fatalf("seed operational memory: %v", err)
	}
	searchResult, err := handleSearch(s, cfg, activity)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"query": "classified handler phase2", "profile": store.SearchProfileOperational,
	}}})
	if err != nil || searchResult.IsError {
		t.Fatalf("search: err=%v isError=%v text=%q", err, searchResult.IsError, callResultText(t, searchResult))
	}
	body := callResultJSON(t, searchResult)
	structured, ok := body["results"].([]any)
	if !ok || len(structured) != 1 {
		t.Fatalf("structured results=%#v", body["results"])
	}
	entry, ok := structured[0].(map[string]any)
	if !ok || entry["memory_class"] != store.MemoryClassOperational {
		t.Fatalf("structured result=%#v", structured[0])
	}

	invalid, err := save(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"title": "invalid class", "content": "invalid class", "memory_class": "resource", "capture_prompt": false,
	}}})
	if err != nil {
		t.Fatalf("invalid save handler error: %v", err)
	}
	if !invalid.IsError || !strings.Contains(callResultText(t, invalid), "invalid memory_class") {
		t.Fatalf("invalid save result: isError=%v text=%q", invalid.IsError, callResultText(t, invalid))
	}
}
