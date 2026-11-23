package mcp

import (
	"context"
	"testing"
	"time"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func TestPassiveCaptureHonorsProcessProjectAndIgnoresPayloadOverride(t *testing.T) {
	s := newMCPTestStore(t)
	h := handleCapturePassive(s, MCPConfig{DefaultProject: "bound-project"}, NewSessionActivity(time.Minute))
	res, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"project": "untrusted-project",
		"content": "## Key Learnings:\n\n1. Process-bound memory capture must preserve its trusted project context.",
	}}})
	if err != nil || res.IsError {
		t.Fatalf("process-bound capture failed: err=%v", err)
	}
	bound, err := s.RecentObservations("bound-project", "project", 10)
	if err != nil || len(bound) != 1 {
		t.Fatalf("bound project must receive exactly one observation: count=%d err=%v", len(bound), err)
	}
	other, err := s.RecentObservations("untrusted-project", "project", 10)
	if err != nil || len(other) != 0 {
		t.Fatalf("payload project override must not receive memory: count=%d err=%v", len(other), err)
	}
}
