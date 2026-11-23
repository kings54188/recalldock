package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/store"
)

func TestSessionStartReportsPersistedOrigin(t *testing.T) {
	s := newMCPTestStore(t)
	root := t.TempDir()
	p, err := s.CreateRegisteredProject("origin test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSessionWithOrigin("host-session", p.ID, root, store.SessionOriginHost); err != nil {
		t.Fatal(err)
	}
	req := mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": "host-session", "directory": root}}}
	res, err := handleSessionStart(s, MCPConfig{}, NewSessionActivity(10*time.Minute))(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("session start: %v %s", err, callResultText(t, res))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(callResultText(t, res)), &body); err != nil {
		t.Fatal(err)
	}
	if body["session_origin"] != store.SessionOriginHost {
		t.Fatalf("origin = %v, want host", body["session_origin"])
	}
}

func TestRegisteredRootFlowsThroughMCPAndExplicitHistoryRead(t *testing.T) {
	s := newMCPTestStore(t)
	root := t.TempDir()
	deep := filepath.Join(root, "a/b")
	if err := os.MkdirAll(deep, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("MCP identity")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(deep)
	activity := NewSessionActivity(10 * time.Minute)
	req := mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": "registered-mcp-session"}}}
	res, err := handleSessionStart(s, MCPConfig{}, activity)(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("session start: %v %s", err, callResultText(t, res))
	}
	session, err := s.GetSession("registered-mcp-session")
	if err != nil || session.Project != p.ID {
		t.Fatalf("session identity: %+v %v", session, err)
	}
	current, err := resolveReadProject(s, "")
	if err != nil || current.Project != p.ID {
		t.Fatalf("cwd read: %+v %v", current, err)
	}
	t.Chdir(t.TempDir())
	historical, err := resolveReadProject(s, p.ID)
	if err != nil || historical.Project != p.ID {
		t.Fatalf("history read: %+v %v", historical, err)
	}
}
