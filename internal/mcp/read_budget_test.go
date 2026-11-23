package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/store"
)

func TestMCPSharedBudgetCatalogAndRevisionDedup(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("read-session", "budget", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(store.AddObservationParams{SessionID: "read-session", Project: "budget", Type: "decision", Title: "Budget fixture", Content: "SELECTED_BODY_MARKER " + strings.Repeat("bounded data ", 50), Scope: "project"})
	if err != nil {
		t.Fatal(err)
	}
	budget, _ := s.CreateReadBudget("budget", 8000)
	cfg := MCPConfig{ReadBudgetID: budget.ID}
	allowed := map[string]bool{"mem_get_observation": true, "mem_context": true}
	srv := NewServerWithConfig(s, cfg, allowed)
	catalog, _ := s.ReadBudget(budget.ID)
	second := NewServerWithConfig(s, cfg, allowed)
	afterCatalog, _ := s.ReadBudget(budget.ID)
	if catalog.Used == 0 || afterCatalog.Used <= catalog.Used {
		t.Fatal("each server catalog must be accounted without resetting the shared budget")
	}
	req := mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": float64(id)}}}
	first, err := srv.GetTool("mem_get_observation").Handler(context.Background(), req)
	if err != nil || first.IsError || !strings.Contains(callResultText(t, first), "SELECTED_BODY_MARKER") {
		t.Fatalf("selected read: %v %s", err, callResultText(t, first))
	}
	charged, _ := s.ReadBudget(budget.ID)
	wire, _ := json.Marshal(first)
	if charged.Used-afterCatalog.Used < store.EstimateReadTokens(string(wire)) {
		t.Fatal("only text instead of full serialized envelope was charged")
	}
	duplicate, err := second.GetTool("mem_get_observation").Handler(context.Background(), req)
	if err != nil || duplicate.IsError || strings.Contains(callResultText(t, duplicate), "SELECTED_BODY_MARKER") || !strings.Contains(callResultText(t, duplicate), "already_loaded") {
		t.Fatalf("duplicate body: %v %s", err, callResultText(t, duplicate))
	}
	revised := "NEW_REVISION_MARKER " + strings.Repeat("new data ", 30)
	if _, err := s.UpdateObservation(id, store.UpdateObservationParams{Content: &revised}); err != nil {
		t.Fatal(err)
	}
	next, err := srv.GetTool("mem_get_observation").Handler(context.Background(), req)
	if err != nil || next.IsError || !strings.Contains(callResultText(t, next), "NEW_REVISION_MARKER") {
		t.Fatalf("new revision not delivered: %v %s", err, callResultText(t, next))
	}
}

func TestMCPBudgetCannotBeResetByToolParameters(t *testing.T) {
	s := newMCPTestStore(t)
	s.CreateSession("r", "budget", t.TempDir())
	id, _ := s.AddObservation(store.AddObservationParams{SessionID: "r", Project: "budget", Type: "decision", Title: "Selected", Content: "WHOLE_PAYLOAD_MUST_NOT_LEAK", Scope: "project"})
	budget, _ := s.CreateReadBudget("budget", 8000)
	srv := NewServerWithConfig(s, MCPConfig{ReadBudgetID: budget.ID}, map[string]bool{"mem_get_observation": true})
	state, _ := s.ReadBudget(budget.ID)
	if _, err := s.ConsumeReadBudget(budget.ID, "budget", state.Limit-state.Used-40, ""); err != nil {
		t.Fatal(err)
	}
	req := mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": float64(id), "session_id": "new-session", "read_budget_id": "fake-reset", "token_budget": 1000000.0}}}
	res, err := srv.GetTool("mem_get_observation").Handler(context.Background(), req)
	if err != nil || !res.IsError || strings.Contains(callResultText(t, res), "WHOLE_PAYLOAD_MUST_NOT_LEAK") {
		t.Fatalf("oversized envelope escaped budget: %v %s", err, callResultText(t, res))
	}
	final, _ := s.ReadBudget(budget.ID)
	if final.Used != state.Limit-40 {
		t.Fatal("failed reservation reset or consumed the payload allowance")
	}
}

func TestUnknownReadBudgetFailsClosedWithoutMintingReplacement(t *testing.T) {
	s := newMCPTestStore(t)
	srv := NewServerWithConfig(s, MCPConfig{ReadBudgetID: "unknown-host-reference"}, map[string]bool{"mem_search": true})
	result, err := srv.GetTool("mem_search").Handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"query": "anything", "session_id": "reset"}}})
	if err != nil || !result.IsError {
		t.Fatal("unknown accounting reference failed open")
	}
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM read_budgets`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("replacement budget minted: %d %v", count, err)
	}
}

func TestDedupReceiptDoesNotRetainStructuredBody(t *testing.T) {
	s := newMCPTestStore(t)
	budget, _ := s.CreateReadBudget("budget", 8000)
	gate := newMCPReadGate(s, MCPConfig{ReadBudgetID: budget.ID})
	next := func(context.Context, mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
		result := mcppkg.NewToolResultText(`{"project":"budget","observation_sync_id":"obs-structured","revision_count":1,"detail":"l2","result":"BODY_NOT_IN_RECEIPT"}`)
		result.StructuredContent = map[string]any{"body": "BODY_NOT_IN_RECEIPT"}
		return result, nil
	}
	handler := gate.wrap("mem_get_observation", next)
	if first, err := handler(context.Background(), mcppkg.CallToolRequest{}); err != nil || first.IsError {
		t.Fatal("first structured response rejected", err)
	}
	duplicate, err := handler(context.Background(), mcppkg.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(duplicate)
	if duplicate.IsError || strings.Contains(string(wire), "BODY_NOT_IN_RECEIPT") || duplicate.StructuredContent != nil {
		t.Fatalf("receipt retained a structured body: %s", wire)
	}
}

func TestPublicMCPStatsCannotSmuggleGlobalCountsIntoProjectEnvelope(t *testing.T) {
	st := newMCPTestStore(t)
	for _, project := range []string{"visible", "hidden"} {
		if err := st.CreateSession(project, project, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddObservation(store.AddObservationParams{SessionID: project, Project: project, Title: project, Content: "Synthetic scope stat", Type: "decision"}); err != nil {
			t.Fatal(err)
		}
	}
	budget, err := st.CreateReadBudget("visible", 8000)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithConfig(st, MCPConfig{ReadBudgetID: budget.ID}, map[string]bool{"mem_stats": true})
	result, err := srv.GetTool("mem_stats").Handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{}}})
	if err != nil || result.IsError {
		t.Fatal("scoped stats failed")
	}
	text := callResultText(t, result)
	if strings.Contains(text, "hidden") || !strings.Contains(text, "Observations: 1") {
		t.Fatalf("scoped stats included global metadata: %s", text)
	}
}

func TestMCPBudgetDenialAlwaysExplains(t *testing.T) {
	s := newMCPTestStore(t)
	budget, _ := s.CreateReadBudget("budget", 8000)
	gate := newMCPReadGate(s, MCPConfig{ReadBudgetID: budget.ID})
	state, _ := s.ReadBudget(budget.ID)
	if _, err := s.ConsumeReadBudget(budget.ID, "budget", state.Limit-state.Used, ""); err != nil {
		t.Fatalf("exhaust budget: %v", err)
	}
	next := func(context.Context, mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
		return mcppkg.NewToolResultText(`{"project":"budget","result":"x"}`), nil
	}
	handler := gate.wrap("mem_search", next)
	// 0.1.7 bug: only the first refusal carried the explanation; every later
	// refusal was an empty error the agent could not diagnose.
	for i := 0; i < 3; i++ {
		res, err := handler(context.Background(), mcppkg.CallToolRequest{})
		if err != nil || res == nil || !res.IsError {
			t.Fatalf("call %d: expected error result, got err=%v res=%v", i, err, res)
		}
		text := callResultText(t, res)
		if !strings.Contains(text, "read_budget_exhausted") {
			t.Fatalf("call %d: denial must carry the explanatory code every time, got %q", i, text)
		}
	}
}
