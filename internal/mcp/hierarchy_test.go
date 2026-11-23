package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/store"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func TestHierarchyToolSchemas(t *testing.T) {
	srv := NewServer(newMCPTestStore(t))
	for toolName, fields := range map[string][]string{
		"mem_save":            {"memory_path", "abstract", "overview"},
		"mem_update":          {"memory_path", "abstract", "overview"},
		"mem_get_observation": {"detail"},
	} {
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

func TestGetObservationDetailLevels(t *testing.T) {
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	t.Chdir(dir)
	s := newMCPTestStore(t)
	if err := s.CreateSession("hierarchy-mcp", "hierarchy-mcp", dir); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(store.AddObservationParams{
		SessionID: "hierarchy-mcp", Type: "decision", Title: "Layered memory",
		Content: "FULL-CONTENT-MARKER that must only appear in L2.", Project: "hierarchy-mcp",
		MemoryClass: store.MemoryClassProject, MemoryPath: "project/design/memory",
		Abstract: "L0 abstract", Overview: "L1 overview",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := handleGetObservation(s, MCPConfig{DefaultProject: "hierarchy-mcp"})
	for _, tc := range []struct {
		detail    string
		want      string
		doNotWant string
	}{
		{detail: "l0", want: "L0 abstract", doNotWant: "L1 overview"},
		{detail: "l1", want: "L1 overview", doNotWant: "FULL-CONTENT-MARKER"},
		{detail: "l2", want: "FULL-CONTENT-MARKER"},
	} {
		result, err := handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
			"id": float64(id), "detail": tc.detail,
		}}})
		if err != nil || result.IsError {
			t.Fatalf("detail=%s err=%v result=%q", tc.detail, err, callResultText(t, result))
		}
		text := callResultText(t, result)
		if !strings.Contains(text, tc.want) || tc.doNotWant != "" && strings.Contains(text, tc.doNotWant) {
			t.Fatalf("detail=%s text=%q", tc.detail, text)
		}
		body := callResultJSON(t, result)
		if body["detail"] != tc.detail || body["memory_path"] != "project/design/memory" {
			t.Fatalf("detail=%s body=%#v", tc.detail, body)
		}
	}

	invalid, err := handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"id": float64(id), "detail": "raw",
	}}})
	if err != nil || !invalid.IsError {
		t.Fatalf("invalid detail err=%v result=%+v", err, invalid)
	}
	_ = time.Second
}
