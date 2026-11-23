package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/store"
)

func TestPublicPersonalScopeDoesNotBroadenSelectedProject(t *testing.T) {
	for _, tool := range []string{"mem_search", "mem_context"} {
		for _, activation := range []string{"pinned", "directory"} {
			t.Run(tool+"/"+activation, func(t *testing.T) {
				s := newMCPTestStore(t)
				root := t.TempDir()
				t.Chdir(root)
				for _, seed := range []struct{ project, title string }{{"alpha", "OWN_PERSONAL_NOTE"}, {"beta", "FOREIGN_PERSONAL_NOTE"}} {
					if err := s.CreateSession(seed.project, seed.project, root); err != nil {
						t.Fatal(err)
					}
					if _, err := s.AddObservation(store.AddObservationParams{SessionID: seed.project, Project: seed.project, Type: "decision", Title: seed.title, Content: "scope-personal-probe stable preference", Scope: "personal"}); err != nil {
						t.Fatal(err)
					}
				}
				cfg := MCPConfig{}
				if activation == "pinned" {
					cfg.DefaultProject = "alpha"
				} else {
					if err := os.Mkdir(filepath.Join(root, ".recalldock"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(root, ".recalldock/config.json"), []byte(`{"project_name":"alpha"}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				c, err := client.NewInProcessClient(NewServerWithConfig(s, cfg, map[string]bool{tool: true}))
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				ctx := context.Background()
				if _, err := c.Initialize(ctx, mcppkg.InitializeRequest{Params: mcppkg.InitializeParams{ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION, ClientInfo: mcppkg.Implementation{Name: "personal-scope-test", Version: "1"}}}); err != nil {
					t.Fatal(err)
				}
				args := map[string]any{"scope": "personal"}
				if tool == "mem_search" {
					args["query"] = "scope-personal-probe"
					args["detail"] = "l0"
				} else {
					args["mode"] = "index"
				}
				r, err := c.CallTool(ctx, mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: tool, Arguments: args}})
				if err != nil {
					t.Fatal(err)
				}
				text := callResultText(t, r)
				if r.IsError || !strings.Contains(text, "OWN_PERSONAL_NOTE") || strings.Contains(text, "FOREIGN_PERSONAL_NOTE") {
					t.Fatalf("scope personal bypassed project activation: %s", text)
				}
			})
		}
	}
}

func TestPublicPersonalExplicitAllRequiresUnpinnedActivation(t *testing.T) {
	for _, tool := range []string{"mem_search", "mem_context"} {
		for _, pinned := range []bool{false, true} {
			t.Run(tool+map[bool]string{false: "/all", true: "/pinned"}[pinned], func(t *testing.T) {
				s := newMCPTestStore(t)
				root := t.TempDir()
				t.Chdir(root)
				for _, p := range []string{"alpha", "beta"} {
					if err := s.CreateSession(p, p, root); err != nil {
						t.Fatal(err)
					}
					if _, err := s.AddObservation(store.AddObservationParams{SessionID: p, Project: p, Type: "decision", Title: "PERSONAL_" + p, Content: "explicit-all-personal-probe", Scope: "personal"}); err != nil {
						t.Fatal(err)
					}
				}
				cfg := MCPConfig{}
				if pinned {
					cfg.DefaultProject = "alpha"
				}
				c, err := client.NewInProcessClient(NewServerWithConfig(s, cfg, map[string]bool{tool: true}))
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				ctx := context.Background()
				if _, err := c.Initialize(ctx, mcppkg.InitializeRequest{Params: mcppkg.InitializeParams{ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION, ClientInfo: mcppkg.Implementation{Name: "explicit-all-test", Version: "1"}}}); err != nil {
					t.Fatal(err)
				}
				args := map[string]any{"scope": "personal", "all_projects": true}
				if tool == "mem_search" {
					args["query"] = "explicit-all-personal-probe"
				} else {
					args["mode"] = "index"
				}
				r, err := c.CallTool(ctx, mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: tool, Arguments: args}})
				if err != nil {
					t.Fatal(err)
				}
				text := callResultText(t, r)
				if pinned {
					if !r.IsError || strings.Contains(text, "PERSONAL_beta") {
						t.Fatal("all_projects bypassed process pin", text)
					}
				} else if r.IsError || !strings.Contains(text, "PERSONAL_alpha") || !strings.Contains(text, "PERSONAL_beta") || !strings.Contains(text, `"project_source":"all_projects"`) {
					t.Fatal("explicit global personal selection unavailable/mislabeled", text)
				}
			})
		}
	}
}
