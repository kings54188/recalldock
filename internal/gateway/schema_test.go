package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/opsstore"
)

func schemaTestClient(t *testing.T) (context.Context, *client.Client) {
	t.Helper()
	dir := t.TempDir()
	ops, err := opsstore.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ops.Close() })
	pool := NewPool(filepath.Join(dir, "registry.json"), ops)
	t.Cleanup(func() { pool.Close() })
	c, err := client.NewInProcessClient(NewServer(pool))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "schema-test", Version: "1"},
	}}); err != nil {
		t.Fatal(err)
	}
	return ctx, c
}

func schemaCall(t *testing.T, ctx context.Context, c *client.Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	r, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func schemaText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, item := range r.Content {
		if text, ok := item.(mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func TestWorkCheckpointSchemaIsExplicit(t *testing.T) {
	ctx, c := schemaTestClient(t)
	listed, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	for _, tool := range listed.Tools {
		if tool.Name == "work_checkpoint" {
			schema, _ = tool.InputSchema.Properties["checkpoint"].(map[string]any)
		}
	}
	if schema == nil {
		t.Fatal("work_checkpoint has no explicit checkpoint schema")
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("checkpoint must reject unknown fields: %+v", schema["additionalProperties"])
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, field := range []string{"goal", "summary", "next", "status", "blockers", "operations"} {
		if _, ok := properties[field]; !ok {
			t.Fatalf("checkpoint schema missing %q: %+v", field, properties)
		}
	}
	if _, ok := properties["artifacts"]; ok {
		t.Fatal("checkpoint schema advertises a field the decoder rejects")
	}
	status, _ := properties["status"].(map[string]any)
	if got := fmt.Sprint(status["enum"]); got != "[active blocked completed archived]" {
		t.Fatalf("status enum = %s", got)
	}
	next, _ := properties["next"].(map[string]any)
	if next["type"] != "array" {
		t.Fatalf("next must be an array: %+v", next)
	}
}

func TestToolsDescribeServesBuiltinSchema(t *testing.T) {
	ctx, c := schemaTestClient(t)
	r := schemaCall(t, ctx, c, "tools_describe", map[string]any{"server": "recalldock_gateway", "tool": "work_checkpoint"})
	if r.IsError {
		t.Fatalf("describe: %s", schemaText(t, r))
	}
	var described struct {
		Data struct {
			Transport  string         `json:"transport"`
			Name       string         `json:"name"`
			SchemaHash string         `json:"schema_hash"`
			Schema     map[string]any `json:"schema"`
		} `json:"data"`
	}
	body := schemaText(t, r)
	if err := json.Unmarshal([]byte(body), &described); err != nil {
		t.Fatal(err)
	}
	if described.Data.Transport != "builtin" || described.Data.Name != "work_checkpoint" || len(described.Data.SchemaHash) != 64 {
		t.Fatalf("describe = %s", body)
	}
	properties, _ := described.Data.Schema["properties"].(map[string]any)
	if _, ok := properties["checkpoint"]; !ok {
		t.Fatalf("described schema = %s", body)
	}

	r = schemaCall(t, ctx, c, "tools_find", map[string]any{"query": "work_checkpoint"})
	var found struct {
		Data struct {
			Builtin []struct {
				Name string `json:"name"`
			} `json:"builtin_tools"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(schemaText(t, r)), &found); err != nil {
		t.Fatal(err)
	}
	hasCheckpoint := false
	for _, item := range found.Data.Builtin {
		if item.Name == "work_checkpoint" {
			hasCheckpoint = true
		}
	}
	if !hasCheckpoint {
		t.Fatalf("builtin discovery = %s", schemaText(t, r))
	}

	r = schemaCall(t, ctx, c, "tools_describe", map[string]any{"server": "no-such-server", "tool": "x"})
	if !r.IsError || !strings.Contains(schemaText(t, r), "tools_find") {
		t.Fatalf("unknown server should point at tools_find: %s", schemaText(t, r))
	}
}

func TestWorkCheckpointGuidesFieldShape(t *testing.T) {
	ctx, c := schemaTestClient(t)
	root := t.TempDir()

	r := schemaCall(t, ctx, c, "work_checkpoint", map[string]any{
		"root": root, "lane": "test", "expected": 0, "goal": "flat goal", "summary": "flat summary",
	})
	if !r.IsError || !strings.Contains(schemaText(t, r), "inside checkpoint") {
		t.Fatalf("flat fields should explain the checkpoint object: %s", schemaText(t, r))
	}

	r = schemaCall(t, ctx, c, "work_checkpoint", map[string]any{
		"root": root, "lane": "test", "expected": 0,
		"checkpoint": map[string]any{"goal": "task", "summary": "s", "next": []string{"one"}, "artifacts": []string{"x"}},
	})
	if !r.IsError || !strings.Contains(schemaText(t, r), "allowed arguments") {
		t.Fatalf("unknown nested field should list allowed fields: %s", schemaText(t, r))
	}

	r = schemaCall(t, ctx, c, "work_checkpoint", map[string]any{
		"root": root, "lane": "test", "expected": 0,
		"checkpoint": map[string]any{"goal": "task", "summary": "s", "next": []string{"one"}, "status": "delivered"},
	})
	if !r.IsError || !strings.Contains(schemaText(t, r), `"active", "blocked", "completed", "archived"`) {
		t.Fatalf("bad status should list the enum: %s", schemaText(t, r))
	}

	r = schemaCall(t, ctx, c, "work_checkpoint", map[string]any{
		"root": root, "lane": "test", "expected": 0,
		"checkpoint": map[string]any{"goal": "task", "summary": "s", "next": []string{"one"}},
	})
	if r.IsError {
		t.Fatalf("valid checkpoint rejected: %s", schemaText(t, r))
	}
}
