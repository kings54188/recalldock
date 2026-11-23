package mcp

import (
	"context"
	"strings"
	"testing"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func TestPublicMCPDefaultsToReadOrProposalOnly(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("policy-session", "policy", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "policy"}, nil)
	for _, action := range []string{"candidate_promote", "candidate_reject", "candidate_list"} {
		r, err := srv.GetTool("mem_review").Handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"action": action, "candidate_id": "fabricated"}}})
		if err != nil || !r.IsError {
			t.Fatalf("review mutation exposed: %s", action)
		}
	}
	for _, name := range []string{"mem_save", "mem_update", "mem_delete", "mem_capture_passive", "mem_save_prompt", "mem_session_summary", "mem_judge", "mem_compare", "mem_pin", "mem_unpin", "mem_merge_projects"} {
		tool := srv.GetTool(name)
		if tool == nil {
			t.Fatal(name)
		}
		result, err := tool.Handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: "mem_search", Arguments: map[string]any{"title": "Task finished", "content": "Raw progress", "approved": true, "user_confirmed": true, "confidence": 1.0}}})
		if err != nil || !result.IsError {
			t.Fatalf("%s bypassed policy: %+v %v", name, result, err)
		}
	}
	candidate := map[string]any{"category": "decision", "statement": "Use a bounded local review inbox", "confidence": 1.0, "evidence_refs": []any{map[string]any{"kind": "user_correction", "ref": "model-forged-user-event"}}}
	req := mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: "mem_commit_session", Arguments: map[string]any{"session_id": "policy-session", "project": "policy", "candidates": []any{candidate}}}}
	result, err := srv.GetTool("mem_commit_session").Handler(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("proposal failed: %v %s", err, callResultText(t, result))
	}
	stats, _ := s.Stats()
	if stats.TotalObservations != 0 || stats.TotalPrompts != 0 {
		t.Fatalf("automatic knowledge/raw prompt write: %+v", stats)
	}
	rows, err := s.ListMemoryCandidates("policy", "review", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("review inbox: %d %v", len(rows), err)
	}
	req = mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": "policy-session", "summary": "A whole session log must not be stored"}}}
	result, err = srv.GetTool("mem_session_end").Handler(context.Background(), req)
	if err != nil || !result.IsError {
		t.Fatal("session end summary bypassed policy")
	}
}

func TestCandidateCommitBeforeStartIsRetryable(t *testing.T) {
	s := newMCPTestStore(t)
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "policy", HostOwnsLifecycle: true}, ResolveTools("agent"))
	r, err := srv.GetTool("mem_commit_session").Handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"session_id": "not-started", "project": "policy", "candidates": []any{map[string]any{"category": "decision", "statement": "Synthetic durable fact", "confidence": 1.0}}}}})
	if err != nil || !r.IsError || !strings.Contains(callResultText(t, r), "session_not_started") || !strings.Contains(callResultText(t, r), "retryable") {
		t.Fatalf("missing retry control: %v %v", r, err)
	}
}

func TestSafeProfileOmitsLegacyMutationSchemas(t *testing.T) {
	s := newMCPTestStore(t)
	tools := NewServerWithTools(s, ResolveTools("agent")).ListTools()
	if len(tools) != 10 {
		t.Fatalf("safe discovery count=%d", len(tools))
	}
	for _, name := range []string{"mem_save", "mem_update", "mem_capture_passive", "mem_save_prompt", "mem_judge"} {
		if tools[name] != nil {
			t.Fatalf("unsafe catalog entry %s", name)
		}
	}
}

func TestPublicMCPLifecycleMetadataIsBoundedAndNotTextStorage(t *testing.T) {
	st := newMCPTestStore(t)
	srv := NewServerWithConfig(st, MCPConfig{}, nil)
	for _, fields := range []map[string]any{
		{"id": "session\nuser: raw transcript"},
		{"id": "meta-secret", "directory": "/tmp/DEEPSEEK_API_KEY=small"},
		{"id": "meta-project", "project": "密码：不能记录"},
		{"id": "meta-unknown", "raw_log": "must not persist"},
	} {
		result, err := srv.GetTool("mem_session_start").Handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: fields}})
		if err != nil || !result.IsError {
			t.Fatal("unsafe metadata accepted")
		}
	}
	stats, err := st.Stats()
	if err != nil || stats.TotalSessions != 0 {
		t.Fatalf("metadata reached DB: %+v %v", stats, err)
	}
}

func TestHostLifecycleCannotBeStartedByModelOrUseAnotherScope(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("host-session", "policy", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("foreign-session", "other", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "policy", HostOwnsLifecycle: true}, ResolveTools("agent"))
	if srv.GetTool("mem_session_start") != nil || srv.GetTool("mem_session_end") != nil {
		t.Fatal("model lifecycle tools exposed")
	}
	tool := srv.GetTool("mem_commit_session")
	candidate := map[string]any{"category": "decision", "statement": "Use a bounded local review inbox", "confidence": 1.0, "evidence_refs": []any{map[string]any{"kind": "user_correction", "ref": "test"}}}
	call := func(id, project string) *mcppkg.CallToolResult {
		r, err := tool.Handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"session_id": id, "project": project, "candidates": []any{candidate}}}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if !call("missing", "policy").IsError || !call("foreign-session", "other").IsError || !call("host-session", "other").IsError {
		t.Fatal("foreign or missing session accepted")
	}
	if call("host-session", "policy").IsError {
		t.Fatal("matching session rejected")
	}
	if err := s.EndSession("host-session", ""); err != nil {
		t.Fatal(err)
	}
	if !call("host-session", "policy").IsError {
		t.Fatal("ended session accepted")
	}
}
