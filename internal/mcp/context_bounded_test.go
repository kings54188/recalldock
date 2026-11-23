package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/store"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func TestMemContextBoundedIndexResponse(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("mcp-bounded", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{
		SessionID: "mcp-bounded", Type: "decision", Title: "MCP bounded",
		Content:  "FULL-CONTEXT-MARKER must not enter the index response.",
		Abstract: "MCP index abstract", Project: "engram",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := handleContext(s, MCPConfig{DefaultProject: "engram"}, NewSessionActivity(10*time.Minute))(
		context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
			"project": "engram", "mode": "index", "token_budget": 300.0,
		}}},
	)
	if err != nil || result.IsError {
		t.Fatalf("context err=%v result=%q", err, callResultText(t, result))
	}
	if strings.Contains(callResultText(t, result), "FULL-CONTEXT-MARKER") {
		t.Fatal("bounded context exposed full body")
	}
	body := callResultJSON(t, result)
	if body["context_mode"] != "index" || body["token_budget"] != float64(300) {
		t.Fatalf("bounded metadata=%#v", body)
	}
}
