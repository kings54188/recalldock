package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/store"
)

func TestSessionStartDirectoryBecomesDefaultReadProject(t *testing.T) {
	s := newMCPTestStore(t)
	root := t.TempDir()
	project, err := s.CreateRegisteredProject("session-default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(project.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("seed", project.ID, root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "seed", Project: project.ID, Type: "decision", Title: "DEFAULT_PROJECT_NOTE", Content: "session default probe"}); err != nil {
		t.Fatal(err)
	}

	if err := s.CreateSession("foreign-seed", "foreign-stats-project", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "foreign-seed", Project: "foreign-stats-project", Type: "decision", Title: "FOREIGN_PROJECT_NOTE", Content: "foreign probe"}); err != nil {
		t.Fatal(err)
	}
	c, err := client.NewInProcessClient(NewServerWithConfig(s, MCPConfig{}, map[string]bool{"mem_session_start": true, "mem_context": true, "mem_stats": true}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcppkg.InitializeRequest{Params: mcppkg.InitializeParams{ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION, ClientInfo: mcppkg.Implementation{Name: "session-default-test", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) string {
		t.Helper()
		r, err := c.CallTool(ctx, mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: name, Arguments: args}})
		if err != nil {
			t.Fatal(err)
		}
		if r.IsError {
			t.Fatalf("%s: %s", name, callResultText(t, r))
		}
		return callResultText(t, r)
	}
	started := call("mem_session_start", map[string]any{"id": "model-session", "directory": root})
	t.Logf("started: %s", started)
	statsText := call("mem_stats", map[string]any{})
	if strings.Contains(statsText, "foreign-stats-project") || !strings.Contains(statsText, "Observations: 1") {
		t.Fatalf("stats ignored session default: %s", statsText)
	}
	text := call("mem_context", map[string]any{"mode": "index"})
	if !strings.Contains(text, "DEFAULT_PROJECT_NOTE") || !strings.Contains(text, `"project_source":"session"`) {
		t.Fatalf("session directory was not used as default project: %s", text)
	}
}

func TestMCPRepeatedSessionEndDoesNotRefreshEndedAt(t *testing.T) {
	s := newMCPTestStore(t)
	root := t.TempDir()
	project, err := s.CreateRegisteredProject("end-idempotent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(project.ID, root); err != nil {
		t.Fatal(err)
	}
	c, err := client.NewInProcessClient(NewServerWithConfig(s, MCPConfig{}, map[string]bool{"mem_session_start": true, "mem_session_end": true}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcppkg.InitializeRequest{Params: mcppkg.InitializeParams{ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION, ClientInfo: mcppkg.Implementation{Name: "end-test", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) {
		t.Helper()
		r, err := c.CallTool(ctx, mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: name, Arguments: args}})
		if err != nil || r.IsError {
			t.Fatalf("%s: %v %s", name, err, callResultText(t, r))
		}
	}
	call("mem_session_start", map[string]any{"id": "end-idempotent-session", "directory": root})
	call("mem_session_end", map[string]any{"id": "end-idempotent-session"})
	first, err := s.GetSession("end-idempotent-session")
	if err != nil || first.EndedAt == nil {
		t.Fatalf("first end: %+v %v", first, err)
	}
	time.Sleep(1100 * time.Millisecond)
	call("mem_session_end", map[string]any{"id": "end-idempotent-session"})
	second, err := s.GetSession("end-idempotent-session")
	if err != nil || second.EndedAt == nil {
		t.Fatalf("second end: %+v %v", second, err)
	}
	if *second.EndedAt != *first.EndedAt {
		t.Fatalf("repeated end refreshed ended_at: %q -> %q", *first.EndedAt, *second.EndedAt)
	}
}
