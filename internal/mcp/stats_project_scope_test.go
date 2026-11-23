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

func TestPublicStatsDirectoryScopeAndExplicitAllAreTruthful(t *testing.T) {
	for _, selection := range []string{"directory", "all", "unknown"} {
		t.Run(selection, func(t *testing.T) {
			all := selection == "all"
			s := newMCPTestStore(t)
			root := t.TempDir()
			t.Chdir(root)
			for _, project := range []string{"stats-own", "stats-foreign"} {
				if err := s.CreateSession(project, project, root); err != nil {
					t.Fatal(err)
				}
				if _, err := s.AddObservation(store.AddObservationParams{SessionID: project, Project: project, Type: "decision", Title: "Synthetic fixture", Content: project}); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(root, ".recalldock"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".recalldock/config.json"), []byte(`{"project_name":"stats-own"}`), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := client.NewInProcessClient(NewServerWithConfig(s, MCPConfig{}, map[string]bool{"mem_stats": true}))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx := context.Background()
			if _, err := c.Initialize(ctx, mcppkg.InitializeRequest{Params: mcppkg.InitializeParams{ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION, ClientInfo: mcppkg.Implementation{Name: "stats-scope-test", Version: "1"}}}); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{}
			if all {
				args["all_projects"] = true
			}
			if selection == "unknown" {
				args["project"] = "not-an-existing-project"
			}
			r, err := c.CallTool(ctx, mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: "mem_stats", Arguments: args}})
			if err != nil {
				t.Fatal(err)
			}
			text := callResultText(t, r)
			if selection == "unknown" {
				if !r.IsError || strings.Contains(text, "stats-foreign") || strings.Contains(text, "stats-own") {
					t.Fatal("invalid selector enumerated private project history", text)
				}
				return
			}
			if r.IsError {
				t.Fatal(text)
			}
			if all {
				if !strings.Contains(text, "stats-foreign") || !strings.Contains(text, `"project_source":"all_projects"`) {
					t.Fatal("global statistics mislabeled/unavailable", text)
				}
			} else if strings.Contains(text, "stats-foreign") || !strings.Contains(text, "Observations: 1") || !strings.Contains(text, `"project_source":"config"`) {
				t.Fatal("directory scoped statistics exposed other projects", text)
			}
		})
	}
}
