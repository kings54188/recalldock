package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/store"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func TestHandleCommitSessionReturnsCountsIDsAndResults(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("commit-session", "engram", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	statement := "Keep candidate responses bounded and auditable"
	req := mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"session_id": "commit-session",
		"project":    "engram",
		"candidates": []any{map[string]any{
			"category":   store.CandidateCategoryDecision,
			"statement":  statement,
			"confidence": 0.95,
			"evidence_refs": []any{map[string]any{
				"kind": "test", "ref": "go-test/internal-mcp/commit-session", "digest": strings.Repeat("b", 64),
			}},
		}},
	}}}

	res, err := handleCommitSession(s, MCPConfig{}, NewSessionActivity(time.Minute))(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", callResultText(t, res))
	}
	text := callResultText(t, res)
	if strings.Contains(text, statement) || strings.Contains(text, "evidence_refs") {
		t.Fatalf("bounded response leaked candidate body: %s", text)
	}
	var envelope struct {
		Promoted     int      `json:"promoted"`
		Review       int      `json:"review"`
		Rejected     int      `json:"rejected"`
		CandidateIDs []string `json:"candidate_ids"`
		Results      []struct {
			CandidateID string `json:"candidate_id"`
			Status      string `json:"status"`
			Reason      string `json:"reason"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode response: %v (%s)", err, text)
	}
	if envelope.Promoted != 0 || len(envelope.CandidateIDs) != 1 {
		t.Fatalf("unexpected response: %+v", envelope)
	}
	// B6 fix: per-candidate admission results must be visible to the agent so a
	// rejected/review count is never opaque (reasons were sqlite/CLI-only before).
	if len(envelope.Results) != 1 || envelope.Results[0].CandidateID != envelope.CandidateIDs[0] {
		t.Fatalf("results must mirror candidate_ids: %+v", envelope.Results)
	}
	if envelope.Results[0].Status != store.CandidateStatusReview {
		t.Fatalf("evidence-backed candidate without operator auto-admit should stage for review, got %+v", envelope.Results[0])
	}
}

func TestHandleCommitSessionReportsRejectionReasons(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("commit-reasons", "engram", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	long := strings.Repeat("durable fact ", 200) // > maxCandidateStatementBytes
	res, err := handleCommitSession(s, MCPConfig{}, NewSessionActivity(time.Minute))(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"session_id": "commit-reasons", "project": "engram",
		"candidates": []any{map[string]any{
			"category": store.CandidateCategoryProcedure, "statement": long, "confidence": 0.9,
			"evidence_refs": []any{map[string]any{"kind": "test", "ref": "go-test/oversize", "digest": strings.Repeat("c", 64)}},
		}},
	}}})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := callResultText(t, res)
	if strings.Contains(text, "durable fact") {
		t.Fatalf("response leaked rejected statement body: %s", text)
	}
	var envelope struct {
		Rejected int `json:"rejected"`
		Results  []struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("decode response: %v (%s)", err, text)
	}
	if envelope.Rejected != 1 || len(envelope.Results) != 1 || envelope.Results[0].Reason != "statement_too_long" {
		t.Fatalf("oversize rejection must carry statement_too_long reason: %+v", envelope)
	}
}

func TestHandleCommitSessionRejectsOversizedBatch(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("commit-session", "engram", t.TempDir()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	candidates := make([]any, store.MaxCandidatesPerCommit+1)
	for i := range candidates {
		candidates[i] = map[string]any{"category": store.CandidateCategoryDecision, "statement": "item", "confidence": 0.5}
	}
	res, err := handleCommitSession(s, MCPConfig{}, NewSessionActivity(time.Minute))(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"session_id": "commit-session", "project": "engram", "candidates": candidates,
	}}})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected oversized batch tool error: %s", callResultText(t, res))
	}
}
