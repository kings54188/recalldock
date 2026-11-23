package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/store"
)

func TestPublicMCPUnboundDirectoryDoesNotActivateLegacyName(t *testing.T) {
	s := newMCPTestStore(t)
	root := filepath.Join(t.TempDir(), "legacy-name")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	if err := s.CreateSession("seed", "legacy-name", root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "seed", Project: "legacy-name", Type: "decision", Title: "PRIVATE_UNBOUND_TITLE", Content: "unique-activation-needle", Scope: "project"}); err != nil {
		t.Fatal(err)
	}
	c, err := client.NewInProcessClient(NewServerWithConfig(s, MCPConfig{}, map[string]bool{"mem_search": true}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcppkg.InitializeRequest{Params: mcppkg.InitializeParams{ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION, ClientInfo: mcppkg.Implementation{Name: "scope-test", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	call := func(args map[string]any) *mcppkg.CallToolResult {
		t.Helper()
		r, err := c.CallTool(ctx, mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: "mem_search", Arguments: args}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := call(map[string]any{"query": "unique-activation-needle"})
	text := callResultText(t, r)
	if !r.IsError || strings.Contains(text, "PRIVATE_UNBOUND_TITLE") {
		t.Fatalf("unbound name activated history: %s", text)
	}
	if !strings.Contains(text, "project_not_selected") {
		t.Fatalf("no actionable scope error: %s", text)
	}
	r = call(map[string]any{"query": "unique-activation-needle", "project": "legacy-name"})
	if r.IsError || !strings.Contains(callResultText(t, r), "PRIVATE_UNBOUND_TITLE") {
		t.Fatalf("explicit historical selection failed: %s", callResultText(t, r))
	}
}

func TestPublicMCPTimelineCannotRelabelForeignProject(t *testing.T) {
	s := newMCPTestStore(t)
	var hiddenID int64
	for _, p := range []string{"visible", "hidden"} {
		if err := s.CreateSession(p, p, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		id, err := s.AddObservation(store.AddObservationParams{SessionID: p, Project: p, Type: "decision", Title: "TITLE_" + p, Content: "BODY_" + p})
		if err != nil {
			t.Fatal(err)
		}
		if p == "hidden" {
			hiddenID = id
		}
	}
	c, err := client.NewInProcessClient(NewServerWithConfig(s, MCPConfig{DefaultProject: "visible"}, map[string]bool{"mem_timeline": true}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcppkg.InitializeRequest{Params: mcppkg.InitializeParams{ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION, ClientInfo: mcppkg.Implementation{Name: "scope-test", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	r, err := c.CallTool(ctx, mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: "mem_timeline", Arguments: map[string]any{"observation_id": hiddenID, "project": "visible"}}})
	if err != nil {
		t.Fatal(err)
	}
	text := callResultText(t, r)
	if !r.IsError || strings.Contains(text, "BODY_hidden") || !strings.Contains(text, "project_scope_mismatch") {
		t.Fatalf("foreign timeline %s: %s", strconv.FormatInt(hiddenID, 10), text)
	}
}

func TestSelectedResourceReadsRequireProjectScope(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newMCPTestStore(t)
	ids := map[string]int64{}
	for _, project := range []string{"visible", "hidden"} {
		if err := s.CreateSession(project, project, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		id, err := s.AddObservation(store.AddObservationParams{SessionID: project, Project: project, Type: "decision", Title: "TITLE_" + project, Content: "BODY_" + project})
		if err != nil {
			t.Fatal(err)
		}
		ids[project] = id
	}
	for _, tool := range []string{"mem_get_observation", "mem_timeline"} {
		for _, test := range []struct {
			name, pin, selected, owner, code string
			all                              bool
		}{
			{name: "unbound", owner: "hidden", code: "project_not_selected"},
			{name: "wrong-selection", selected: "visible", owner: "hidden", code: "project_scope_mismatch"},
			{name: "explicit", selected: "hidden", owner: "hidden"},
			{name: "explicit-all", all: true, owner: "hidden"},
			{name: "pinned-foreign", pin: "visible", owner: "hidden", code: "project_scope_mismatch"},
			{name: "pinned-all", pin: "visible", all: true, owner: "hidden", code: "project_scope_mismatch"},
			{name: "pinned-local", pin: "visible", owner: "visible"},
		} {
			t.Run(tool+"/"+test.name, func(t *testing.T) {
				c, err := client.NewInProcessClient(NewServerWithConfig(s, MCPConfig{DefaultProject: test.pin}, map[string]bool{tool: true}))
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				ctx := context.Background()
				if _, err := c.Initialize(ctx, mcppkg.InitializeRequest{Params: mcppkg.InitializeParams{ProtocolVersion: mcppkg.LATEST_PROTOCOL_VERSION, ClientInfo: mcppkg.Implementation{Name: "scope-test", Version: "1"}}}); err != nil {
					t.Fatal(err)
				}
				key := "id"
				if tool == "mem_timeline" {
					key = "observation_id"
				}
				args := map[string]any{key: ids[test.owner]}
				if test.selected != "" {
					args["project"] = test.selected
				}
				if test.all {
					args["all_projects"] = true
				}
				r, err := c.CallTool(ctx, mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Name: tool, Arguments: args}})
				if err != nil {
					t.Fatal(err)
				}
				text := callResultText(t, r)
				if test.code != "" {
					if !r.IsError || !strings.Contains(text, test.code) || strings.Contains(text, "BODY_") {
						t.Fatalf("scope denial failed: %s", text)
					}
				} else if r.IsError || !strings.Contains(text, "BODY_"+test.owner) {
					t.Fatalf("selected read failed: %s", text)
				}
			})
		}
	}
}
