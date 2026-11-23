package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func TestPublicMCPSessionWritesHonorProcessProject(t *testing.T) {
	s := newMCPTestStore(t)
	rootA, rootB := t.TempDir(), t.TempDir()
	a, err := s.CreateRegisteredProject("session scope A")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateRegisteredProject("session scope B")
	if err != nil {
		t.Fatal(err)
	}
	for root, project := range map[string]string{rootA: a.ID, rootB: b.ID} {
		if _, err := s.BindProjectRoot(project, root); err != nil {
			t.Fatal(err)
		}
	}
	for id, seed := range map[string]struct{ project, root string }{
		"session-a": {a.ID, rootA}, "session-b": {b.ID, rootB},
	} {
		if err := s.CreateSession(id, seed.project, seed.root); err != nil {
			t.Fatal(err)
		}
	}
	// The process activation, not the caller's cwd, is authoritative.
	t.Chdir(rootB)
	c, err := client.NewInProcessClient(NewServerWithConfig(s, MCPConfig{DefaultProject: a.ID}, map[string]bool{
		"mem_session_start": true, "mem_session_end": true, "mem_commit_session": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcppkg.InitializeRequest{Params: mcppkg.InitializeParams{
		ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcppkg.Implementation{Name: "session-scope-test", Version: "1"},
	}}); err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) *mcppkg.CallToolResult {
		t.Helper()
		r, err := c.CallTool(ctx, mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: name, Arguments: args}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	candidate := []any{map[string]any{"category": "decision", "statement": "Retain explicit session ownership for candidate admission", "confidence": 0.8}}
	for _, project := range []string{"", b.ID} {
		args := map[string]any{"session_id": "session-b", "candidates": candidate}
		if project != "" {
			args["project"] = project
		}
		r := call("mem_commit_session", args)
		if !r.IsError || !strings.Contains(callResultText(t, r), "project_scope_mismatch") {
			t.Errorf("foreign commit project=%q: %s", project, callResultText(t, r))
		}
	}
	for _, directory := range []string{rootB, ""} {
		r := call("mem_session_start", map[string]any{"id": "foreign-start", "directory": directory})
		if !r.IsError || !strings.Contains(callResultText(t, r), "project_scope_mismatch") {
			t.Errorf("foreign start directory=%q: %s", directory, callResultText(t, r))
		}
	}
	r := call("mem_session_start", map[string]any{"id": "session-b", "directory": rootA})
	if !r.IsError || !strings.Contains(callResultText(t, r), "project_scope_mismatch") {
		t.Errorf("foreign session ID with matching directory: %s", callResultText(t, r))
	}
	clone := t.TempDir()
	if err := os.Mkdir(filepath.Join(clone, ".recalldock"), 0700); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(rootA, ".recalldock/root.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".recalldock/root.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	r = call("mem_session_start", map[string]any{"id": "copied-marker", "directory": clone})
	if !r.IsError || !strings.Contains(callResultText(t, r), "project_binding_invalid") {
		t.Errorf("copied directory marker bypassed pin: %s", callResultText(t, r))
	}
	r = call("mem_session_end", map[string]any{"id": "session-b"})
	if !r.IsError || !strings.Contains(callResultText(t, r), "project_scope_mismatch") {
		t.Errorf("foreign end: %s", callResultText(t, r))
	}
	foreign, err := s.GetSession("session-b")
	if err != nil || foreign.EndedAt != nil {
		t.Fatalf("foreign session changed: %+v %v", foreign, err)
	}
	if _, err := s.GetSession("foreign-start"); err == nil {
		t.Error("foreign start wrote a session")
	}
	candidates, err := s.ListMemoryCandidates(b.ID, "", 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("foreign candidates changed: %d %v", len(candidates), err)
	}

	// Matching explicit directory and session remain usable even from B's cwd.
	r = call("mem_session_start", map[string]any{"id": "new-a", "directory": rootA})
	if r.IsError {
		t.Fatalf("matching start: %s", callResultText(t, r))
	}
	r = call("mem_commit_session", map[string]any{"session_id": "session-a", "candidates": candidate})
	if r.IsError {
		t.Fatalf("matching commit: %s", callResultText(t, r))
	}
	for range 2 {
		r = call("mem_session_end", map[string]any{"id": "session-a"})
		if r.IsError {
			t.Fatalf("matching end: %s", callResultText(t, r))
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(callResultText(t, r)), &result); err != nil {
			t.Fatal(err)
		}
		if result["project"] != a.ID {
			t.Fatalf("end relabeled owner: %s", callResultText(t, r))
		}
	}
}

func TestPublicMCPSessionStartUsesLegacyProcessPin(t *testing.T) {
	s := newMCPTestStore(t)
	root := t.TempDir()
	t.Chdir(root)
	pinned := bindTestDir(t, s, root, "trusted project")
	for _, inherited := range []bool{false, true} {
		cfg := MCPConfig{DefaultProject: pinned}
		if inherited {
			budget, err := s.CreateReadBudget(cfg.DefaultProject, 32000)
			if err != nil {
				t.Fatal(err)
			}
			cfg = MCPConfig{ReadBudgetID: budget.ID}
		}
		server := NewServerWithConfig(s, cfg, map[string]bool{"mem_session_start": true})
		handler := server.ListTools()["mem_session_start"].Handler
		id := "direct-pin"
		if inherited {
			id = "inherited-pin"
		}
		r, err := handler(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": id}}})
		if err != nil || r.IsError {
			t.Fatalf("start inherited=%v: %v %s", inherited, err, callResultText(t, r))
		}
		session, err := s.GetSession(id)
		if err != nil || session.Project != pinned || session.Directory != root {
			t.Fatalf("wrong process ownership inherited=%v: %+v %v", inherited, session, err)
		}
	}
}
