package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

func gitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// TestLinkedWorktreeSessionStartInheritsMainBinding pins the worktree policy:
// a linked worktree of a bound checkout reuses that identity, so Codex/Claude
// hooks that create worktrees keep working; an unbound checkout still fails
// closed with the binding guidance.
func TestLinkedWorktreeSessionStartInheritsMainBinding(t *testing.T) {
	mainDir := t.TempDir()
	gitForTest(t, mainDir, "init", "-q")
	if err := os.WriteFile(filepath.Join(mainDir, "README.md"), []byte("probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitForTest(t, mainDir, "add", "README.md")
	gitForTest(t, mainDir, "commit", "-q", "-m", "init")

	worktree := filepath.Join(t.TempDir(), "wt")
	gitForTest(t, mainDir, "worktree", "add", "-q", worktree, "-b", "feature-y")

	s := newMCPTestStore(t)
	c, err := client.NewInProcessClient(NewServerWithConfig(s, MCPConfig{}, map[string]bool{"mem_session_start": true}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcppkg.InitializeRequest{Params: mcppkg.InitializeParams{
		ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcppkg.Implementation{Name: "worktree-test", Version: "1"},
	}}); err != nil {
		t.Fatal(err)
	}
	call := func(args map[string]any) *mcppkg.CallToolResult {
		t.Helper()
		r, err := c.CallTool(ctx, mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: "mem_session_start", Arguments: args}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Unbound main checkout: the worktree must not invent an identity.
	r := call(map[string]any{"id": "wt-unbound", "directory": worktree})
	if !r.IsError || !strings.Contains(callResultText(t, r), "project_binding_required") {
		t.Fatalf("unbound worktree start: %s", callResultText(t, r))
	}

	proj, err := s.CreateRegisteredProject("worktree main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(proj.ID, mainDir); err != nil {
		t.Fatal(err)
	}

	r = call(map[string]any{"id": "wt-bound", "directory": worktree})
	if r.IsError {
		t.Fatalf("bound worktree start failed: %s", callResultText(t, r))
	}
	sess, err := s.GetSession("wt-bound")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Project != proj.ID {
		t.Fatalf("worktree session project=%q want %q", sess.Project, proj.ID)
	}
	if sess.Directory != worktree {
		t.Fatalf("worktree session directory=%q want %q", sess.Directory, worktree)
	}
	if sess.Origin != "model" {
		t.Fatalf("model session origin=%q", sess.Origin)
	}

	r = call(map[string]any{"id": "main-session", "directory": mainDir})
	if r.IsError {
		t.Fatalf("main checkout start failed: %s", callResultText(t, r))
	}
}
