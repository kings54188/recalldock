package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"recalldock.local/recalldock/internal/mcp"
	"recalldock.local/recalldock/internal/store"
)

func TestHostMCPRequiresExplicitScope(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	child := filepath.Join(root, "nested")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	withCwd(t, child)
	stubExitWithPanic(t)
	withArgs(t, "recalldock", "mcp", "--require-project")
	_, stderr, stopped := captureOutputAndRecover(t, func() { cmdMCP(cfg) })
	if stopped == nil || !strings.Contains(stderr, "not bound") {
		t.Fatalf("unbound host started: %v %q", stopped, stderr)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("host-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	s.Close()
	oldNew, oldServe := newMCPServerWithConfig, serveMCP
	t.Cleanup(func() { newMCPServerWithConfig, serveMCP = oldNew, oldServe })
	var got string
	newMCPServerWithConfig = func(s *store.Store, c mcp.MCPConfig, a map[string]bool) *server.MCPServer {
		got = c.DefaultProject
		return oldNew(s, c, a)
	}
	serveMCP = func(*server.MCPServer, ...server.StdioOption) error { return nil }
	_, stderr = captureOutput(t, func() { cmdMCP(cfg) })
	if stderr != "" || got != p.ID {
		t.Fatalf("nested binding: %q %q", got, stderr)
	}
}

func TestExtractRequiredMCPScope(t *testing.T) {
	args, required, err := extractRequiredMCPScope([]string{"--tools=agent", "--require-project"})
	if err != nil || !required || len(args) != 1 || args[0] != "--tools=agent" {
		t.Fatalf("unexpected parse: %v %v %v", args, required, err)
	}
	if _, _, err := extractRequiredMCPScope([]string{"--require-project", "--require-project"}); err == nil {
		t.Fatal("duplicate accepted")
	}
}
