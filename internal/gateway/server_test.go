package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/opsstore"
)

func TestGatewayBuiltinRoutingReportsDirectCall(t *testing.T) {
	for _, trust := range []string{"agent", "strict"} {
		t.Run(trust, func(t *testing.T) {
			dir := t.TempDir()
			ops, err := opsstore.Open(dir, true)
			if err != nil {
				t.Fatal(err)
			}
			defer ops.Close()
			pool := NewPool(filepath.Join(dir, "registry.json"), ops)
			pool.Trust = trust
			defer pool.Close()
			c, err := client.NewInProcessClient(NewServer(pool))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx := context.Background()
			if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "test", Version: "1"}}}); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			for _, name := range []string{"work_find", "work_read", "work_checkpoint"} {
				r := schemaCall(t, ctx, c, "tools_call", map[string]any{"server": "recalldock_gateway", "tool": name, "root": root, "arguments": map[string]any{"root": root}})
				text := schemaText(t, r)
				if !r.IsError || !strings.Contains(text, "call "+name+" directly") || strings.Contains(text, "approval_required") {
					t.Fatalf("misleading builtin routing error: %s", text)
				}
			}
			if r := schemaCall(t, ctx, c, "work_find", map[string]any{"root": root}); r.IsError {
				t.Fatal(schemaText(t, r))
			}
		})
	}
}

func TestGatewayRejectsInvalidArgumentsBeforeSideEffects(t *testing.T) {
	dir := t.TempDir()
	ops, err := opsstore.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ops.Close()
	pool := NewPool(filepath.Join(dir, "missing-registry.json"), ops)
	defer pool.Close()
	c, err := client.NewInProcessClient(NewServer(pool))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "test", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cases := []struct {
		name string
		args map[string]any
	}{
		{"work_checkpoint", map[string]any{"root": root, "lane": "test", "expected": 0.9, "checkpoint": map[string]any{"goal": "must not write"}}},
		{"work_checkpoint", map[string]any{"root": root, "lane": "test", "expected": "0", "checkpoint": map[string]any{"goal": "must not write"}}},
		{"work_checkpoint", map[string]any{"root": root, "lane": "test", "expected": 9007199254740992.0, "checkpoint": map[string]any{"goal": "must not write"}}},
		{"work_read", map[string]any{"root": root, "lane": "test", "generation": 1.9}},
		{"work_read", map[string]any{"root": root, "lane": "test", "generation": -1}},
		{"work_read", map[string]any{"root": root, "lane": "test", "mode": "invalid"}},
		{"work_find", map[string]any{"root": root, "query": false}},
		{"tools_find", map[string]any{"query": false}},
		{"tools_describe", map[string]any{"server": "test", "refresh": "true"}},
		{"tools_call", map[string]any{"server": "test", "tool": "test", "root": root, "arguments": []any{}}},
		{"tools_release", map[string]any{"lease": 123}},
		{"work_read", map[string]any{"root": root, "lane": "test", "unexpected": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name+"-"+fmt.Sprint(tc.args), func(t *testing.T) {
			r, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: tc.name, Arguments: tc.args}})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(r)
			if !r.IsError || !strings.Contains(string(body), "invalid_arguments") {
				t.Fatalf("invalid input did not fail before side effects: %s", body)
			}
		})
	}
	tasks, err := ops.Find(root, opsstore.FindOptions{Limit: 10})
	if err != nil || len(tasks.Tasks) != 0 {
		t.Fatalf("invalid CAS created tasks: %+v %v", tasks, err)
	}
}

func TestGatewayWorkFindShrinksPageWithinBudget(t *testing.T) {
	dir := t.TempDir()
	ops, err := opsstore.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ops.Close()
	pool := NewPool(filepath.Join(dir, "registry.json"), ops)
	defer pool.Close()
	root := filepath.Join(t.TempDir(), strings.Repeat("a", 180), strings.Repeat("b", 180))
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, lane := range []string{"a", "b", "c"} {
		cp := opsstore.Checkpoint{}
		cp.Goal = strings.Repeat("goal ", 55)
		if _, err := ops.Save(root, lane, 0, cp); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.NewInProcessClient(NewServer(pool))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "test", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	after := ""
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		r, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "work_find", Arguments: map[string]any{"root": root, "after": after}}})
		if err != nil || r.IsError {
			t.Fatalf("bounded page: %+v %v", r, err)
		}
		text := r.Content[0].(mcp.TextContent).Text
		if len(text) > opsstore.SummaryLimit {
			t.Fatalf("page exceeds limit: %d", len(text))
		}
		var page struct {
			Data struct {
				Tasks []opsstore.Task `json:"tasks"`
				After string          `json:"after"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(text), &page); err != nil {
			t.Fatal(err)
		}
		for _, task := range page.Data.Tasks {
			if seen[task.Lane] {
				t.Fatal("page repeated task")
			}
			seen[task.Lane] = true
		}
		if page.Data.After == "" {
			break
		}
		if page.Data.After == after {
			t.Fatal("page cursor did not advance")
		}
		after = page.Data.After
	}
	if len(seen) != 3 {
		t.Fatalf("pagination lost tasks: %+v", seen)
	}
}

func TestGatewayFixedSurfaceAndProgressIsolation(t *testing.T) {
	dir := t.TempDir()
	ops, err := opsstore.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ops.Close()
	pool := NewPool(filepath.Join(dir, "registry.json"), ops)
	defer pool.Close()
	api := NewServer(pool)
	c, err := client.NewInProcessClient(api)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "test", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	listed, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil || len(listed.Tools) != 7 {
		t.Fatalf("fixed tools: %v %v", listed, err)
	}
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		r, e := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	root := t.TempDir()
	r := call("work_checkpoint", map[string]any{"root": root, "lane": "test", "expected": 0, "checkpoint": map[string]any{"goal": "Synthetic task", "summary": "Verified files", "details": "selected-only detail"}})
	if r.IsError {
		t.Fatalf("checkpoint: %+v", r)
	}
	r = call("work_read", map[string]any{"root": root, "lane": "test"})
	body, _ := json.Marshal(r)
	if string(body) == "" || r.IsError {
		t.Fatalf("read: %s", body)
	}
	r = call("work_checkpoint", map[string]any{"root": root, "lane": "test", "expected": 0, "checkpoint": map[string]any{"goal": "Other"}})
	if !r.IsError {
		t.Fatal("stale generation accepted")
	}
	r = call("tools_call", map[string]any{"server": "unknown", "tool": "write", "root": root, "arguments": map[string]any{}, "expected_schema_hash": "bad", "approval": true})
	if !r.IsError {
		t.Fatal("model approval accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "recalldock.db")); !os.IsNotExist(err) {
		t.Fatal("gateway opened knowledge store")
	}
}

// Real hosts may hide MCP _meta from the model. Lease control must also work
// through text content without replacing backend images or structured results.
func TestModelVisibleLeaseAndErrorCleanup(t *testing.T) {
	p, root, _ := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"image", "echo"} {
		if _, err := p.Grant("test", GrantOptions{Tool: tool, ReadOnly: true, Roots: []string{root}}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.NewInProcessClient(NewServer(p))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "model-only", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	records, _ := p.Ops.LoadCatalog("test")
	hashes := map[string]string{}
	for _, record := range records {
		hashes[record.Name] = record.SchemaHash
	}
	call := func(ctx context.Context, tool string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		r, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: tool, Arguments: args}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	visibleLease := func(r *mcp.CallToolResult) string {
		t.Helper()
		for _, item := range r.Content {
			if text, ok := item.(mcp.TextContent); ok {
				var control struct {
					Gateway struct {
						Lease string `json:"lease"`
						TTL   int    `json:"idle_ttl_seconds"`
					} `json:"recalldock_gateway"`
				}
				if json.Unmarshal([]byte(text.Text), &control) == nil && control.Gateway.Lease != "" && control.Gateway.TTL == 300 {
					return control.Gateway.Lease
				}
			}
		}
		t.Fatal("model-visible result has no lease")
		return ""
	}
	args := map[string]any{"server": "test", "tool": "image", "root": root, "expected_schema_hash": hashes["image"], "arguments": map[string]any{}}
	r := call(ctx, "tools_call", args)
	if !r.IsError || r.StructuredContent == nil {
		t.Fatalf("lost native result: %+v", r)
	}
	if _, ok := r.Content[0].(mcp.ImageContent); !ok {
		t.Fatal("image replaced")
	}
	token := visibleLease(r)
	if r.Meta == nil || r.Meta.AdditionalFields["recalldock_lease"] != token {
		t.Fatal("metadata token differs")
	}
	args["lease"] = token
	if visibleLease(call(ctx, "tools_call", args)) != token {
		t.Fatal("did not reuse lease")
	}
	if r := call(ctx, "tools_release", map[string]any{"lease": token}); r.IsError {
		t.Fatal("release failed")
	}
	if r := call(ctx, "tools_call", args); !r.IsError {
		t.Fatal("released lease accepted")
	}
	delete(args, "lease")
	args["tool"], args["expected_schema_hash"], args["arguments"] = "echo", hashes["echo"], map[string]any{"message": "wait"}
	// Cancel only after dispatch, so the backend outcome is genuinely unknown.
	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	r = call(short, "tools_call", args)
	if !r.IsError || !strings.Contains(r.Content[0].(mcp.TextContent).Text, "unknown") {
		t.Fatalf("unexpected timeout: %+v", r)
	}
	token = visibleLease(r)
	if r := call(ctx, "tools_release", map[string]any{"lease": token}); r.IsError {
		t.Fatal("unknown outcome connection cannot be released")
	}
}

func TestModelCannotChooseGatewaySession(t *testing.T) {
	p, root, _ := poolFixture(t, true)
	p.Trust = "strict"
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	c, err := client.NewInProcessClient(NewServer(p))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "untrusted-input", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		r, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	var found struct {
		Data struct {
			Session string `json:"session"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(call("tools_find", map[string]any{}).Content[0].(mcp.TextContent).Text), &found); err != nil {
		t.Fatal(err)
	}
	arguments := map[string]any{"message": "synthetic"}
	grant := func(session string) Grant {
		t.Helper()
		g, err := p.Grant("test", GrantOptions{Tool: "echo", Roots: []string{root}, Session: session, Arguments: arguments, ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	g := grant("model-chosen-session")
	args := map[string]any{"server": "test", "tool": "echo", "root": root, "arguments": arguments, "expected_schema_hash": g.SchemaHash,
		"session": "model-chosen-session", "approval": true, "host": "operator"}
	r := call("tools_call", args)
	if !r.IsError || !strings.Contains(r.Content[0].(mcp.TextContent).Text, "invalid_arguments") {
		t.Fatalf("model chose authorization session: %+v", r)
	}
	p.mu.Lock()
	count := len(p.leases)
	p.mu.Unlock()
	if count != 0 {
		t.Fatal("denied forged session started backend")
	}
	grant(found.Data.Session)
	delete(args, "session")
	delete(args, "approval")
	delete(args, "host")
	r = call("tools_call", args)
	if r.IsError {
		t.Fatalf("real gateway session denied: %+v", r)
	}
	token := r.Meta.AdditionalFields["recalldock_lease"].(string)
	if r := call("tools_release", map[string]any{"lease": token}); r.IsError {
		t.Fatal("owner release failed")
	}
}
