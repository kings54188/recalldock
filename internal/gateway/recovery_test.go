package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/opsstore"
)

func TestGatewayRecoveryViewsAndPagination(t *testing.T) {
	ctx, c := schemaTestClient(t)
	root := t.TempDir()
	call := func(name string, args map[string]any) string {
		t.Helper()
		r := schemaCall(t, ctx, c, name, args)
		if r.IsError {
			t.Fatal(schemaText(t, r))
		}
		return schemaText(t, r)
	}
	for _, lane := range []string{"a", "b", "c"} {
		call("work_checkpoint", map[string]any{"root": root, "lane": lane, "expected": 0, "checkpoint": map[string]any{"goal": "Synthetic task", "summary": "Passed 26/26", "details": "selected detail"}})
	}
	var page struct {
		Data opsstore.TaskPage `json:"data"`
	}
	body := call("work_find", map[string]any{"root": root, "query": "26/26", "limit": 1})
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data.Tasks) != 1 || page.Data.Tasks[0].Lane != "c" || !page.Data.HasMore || page.Data.Total != 3 || page.Data.After == "" || strings.Contains(body, "26/26") {
		t.Fatalf("recovery search: %s", body)
	}
	for _, args := range []map[string]any{{"root": root, "limit": 0}, {"root": root, "limit": 21}, {"root": root, "limit": 1.2}, {"root": root, "order": "bad"}, {"root": root, "status": "bad"}} {
		if r := schemaCall(t, ctx, c, "work_find", args); !r.IsError {
			t.Fatalf("invalid find input accepted: %+v", args)
		}
	}
	event := map[string]any{"id": "attempt", "input_fingerprint": "input", "kind": "attempt", "status": "reported", "summary": "Event summary", "details": "event selected details"}
	args := map[string]any{"root": root, "lane": "c", "expected": 1, "event": event}
	call("work_checkpoint", args)
	body = call("work_checkpoint", args)
	if !strings.Contains(body, `"replayed":true`) {
		t.Fatalf("replay marker missing: %s", body)
	}
	body = call("work_read", map[string]any{"root": root, "lane": "c", "view": "event", "mode": "l2", "generation": 2})
	if !strings.Contains(body, `"mode":"l2"`) || !strings.Contains(body, "event selected details") || !strings.Contains(body, `"kind":"attempt"`) {
		t.Fatalf("event missing: %s", body)
	}
	body = call("work_read", map[string]any{"root": root, "lane": "c", "view": "history", "limit": 1})
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data.Tasks) != 1 || page.Data.Tasks[0].Generation != 2 || !page.Data.HasMore || page.Data.Before != 2 || page.Data.Total != 2 {
		t.Fatalf("history view: %s", body)
	}
	for _, args := range []map[string]any{
		{"root": root, "lane": "c", "view": "event", "generation": 2},
		{"root": root, "lane": "c", "view": "event", "mode": "l2"},
		{"root": root, "lane": "c", "view": "history", "generation": 2},
		{"root": root, "lane": "c", "limit": 1},
	} {
		if r := schemaCall(t, ctx, c, "work_read", args); !r.IsError {
			t.Fatalf("ambiguous read input accepted: %+v", args)
		}
	}
}

func TestGatewayWorkCanStartWithoutKnowledgeBinding(t *testing.T) {
	ctx, c := schemaTestClient(t)
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	found := schemaCall(t, ctx, c, "work_find", map[string]any{"root": root})
	if found.IsError {
		t.Fatal(schemaText(t, found))
	}
	var page struct {
		Data opsstore.TaskPage `json:"data"`
	}
	if err := json.Unmarshal([]byte(schemaText(t, found)), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data.Tasks) != 0 || page.Data.Total != 0 {
		t.Fatalf("unregistered root has task state: %+v", page)
	}
	if _, err := os.Lstat(filepath.Join(root, ".recalldock")); !os.IsNotExist(err) {
		t.Fatalf("lookup initialized root: %v", err)
	}
	created := schemaCall(t, ctx, c, "work_checkpoint", map[string]any{"root": root, "lane": "meaningful-task", "expected": 0, "checkpoint": map[string]any{"goal": "Verify the next task milestone", "summary": "First recovery point", "next": []string{"Run the remaining checks"}}})
	if created.IsError {
		t.Fatal(schemaText(t, created))
	}
	read := schemaCall(t, ctx, c, "work_read", map[string]any{"root": root, "lane": "meaningful-task"})
	if read.IsError {
		t.Fatal(schemaText(t, read))
	}
	var snapshot struct {
		Data opsstore.Snapshot `json:"data"`
	}
	if err := json.Unmarshal([]byte(schemaText(t, read)), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Data.Root != root || snapshot.Data.Lane != "meaningful-task" || snapshot.Data.Generation != 1 {
		t.Fatalf("incorrect task scope: %+v", snapshot)
	}
	if _, err := os.Lstat(filepath.Join(root, ".recalldock", "root.json")); !os.IsNotExist(err) {
		t.Fatalf("Work created a knowledge identity: %v", err)
	}
}

func TestGatewayWorkDescriptionsExplainOptionalTaskRecovery(t *testing.T) {
	ctx, c := schemaTestClient(t)
	listed, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 7 {
		t.Fatalf("gateway surface changed: %d", len(listed.Tools))
	}
	want := map[string][]string{"work_find": {"existing task", "no saved task", "chat"}, "work_read": {"selected task"}, "work_checkpoint": {"meaningful", "expected=0", "chat"}}
	for _, tool := range listed.Tools {
		for _, word := range want[tool.Name] {
			if !strings.Contains(tool.Description, word) {
				t.Errorf("%s lacks use/empty-result guidance %q", tool.Name, word)
			}
		}
	}
}

func TestGatewayWriterIsServerOwnedAndBoundSessionMustStayActive(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	ops, err := opsstore.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ops.Close()
	if err := ops.RegisterHostSession("claude-code-real", "claude-code", root); err != nil {
		t.Fatal(err)
	}
	pool := NewPool(filepath.Join(dir, "registry.json"), ops)
	pool.Host, pool.HostSessionID = "claude-code", "claude-code-real"
	defer pool.Close()
	ctx := context.Background()
	clients := []*client.Client{}
	for range 2 {
		c, err := client.NewInProcessClient(NewServer(pool))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "test", Version: "1"}}}); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, c)
	}
	decode := func(r *mcp.CallToolResult) opsstore.Task {
		t.Helper()
		if r.IsError {
			t.Fatal(schemaText(t, r))
		}
		var value struct {
			Data opsstore.Task `json:"data"`
		}
		if err := json.Unmarshal([]byte(schemaText(t, r)), &value); err != nil {
			t.Fatal(err)
		}
		return value.Data
	}
	cp := map[string]any{"root": root, "lane": "lane", "expected": 0, "checkpoint": map[string]any{"goal": "Synthetic task"}}
	first := decode(schemaCall(t, ctx, clients[0], "work_checkpoint", cp))
	if first.Writer == nil || first.Writer.HostSessionID != "claude-code-real" || first.Writer.Host != "claude-code" {
		t.Fatalf("missing trusted provenance: %+v", first)
	}
	event := map[string]any{"root": root, "lane": "lane", "expected": 1, "event": map[string]any{"id": "attempt", "input_fingerprint": "input", "status": "reported", "summary": "attempt"}}
	decode(schemaCall(t, ctx, clients[0], "work_checkpoint", event))
	replay := decode(schemaCall(t, ctx, clients[1], "work_checkpoint", event))
	if !replay.Replayed || replay.Writer == nil || *replay.Writer != *first.Writer {
		t.Fatalf("retry reassigned historical writer: %+v", replay)
	}
	cp["expected"] = 2
	second := decode(schemaCall(t, ctx, clients[1], "work_checkpoint", cp))
	if second.Writer == nil || second.Writer.GatewaySessionID == first.Writer.GatewaySessionID {
		t.Fatalf("gateway writers share mutable provenance: %+v %+v", first, second)
	}
	cp["session"] = "claude-code-forged"
	if r := schemaCall(t, ctx, clients[0], "work_checkpoint", cp); !r.IsError {
		t.Fatal("model could choose writer identity")
	}
	delete(cp, "session")
	if err := ops.EndHostSession("claude-code-real", "claude-code", root); err != nil {
		t.Fatal(err)
	}
	cp["expected"] = 3
	if r := schemaCall(t, ctx, clients[0], "work_checkpoint", cp); !r.IsError {
		t.Fatal("ended host session still wrote progress")
	}
	current, err := ops.Read(root, "lane", 0)
	if err != nil || current.Generation != 3 {
		t.Fatalf("rejected write changed progress: %+v %v", current, err)
	}
}

func TestBuiltinDiscoveryIsCompleteAndBudgeted(t *testing.T) {
	ctx, c := schemaTestClient(t)
	r := schemaCall(t, ctx, c, "tools_describe", map[string]any{"server": "recalldock_gateway"})
	body := schemaText(t, r)
	var envelope struct {
		Data struct {
			Tools []builtinToolRecord `json:"tools"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatal(err)
	}
	if r.IsError || len(envelope.Data.Tools) != 7 || len(body) > opsstore.SummaryLimit {
		t.Fatalf("incomplete builtin describe: %s", body)
	}
	seen := map[string]bool{}
	after := ""
	for range 8 {
		r := schemaCall(t, ctx, c, "tools_find", map[string]any{"query": "work", "builtin_after": after})
		body := schemaText(t, r)
		if r.IsError || len(body) > opsstore.SummaryLimit {
			t.Fatalf("builtin search: %s", body)
		}
		var page struct {
			Data struct {
				Items   []builtinToolRecord `json:"builtin_tools"`
				After   string              `json:"builtin_after"`
				HasMore bool                `json:"builtin_has_more"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Data.Items {
			if seen[item.Name] {
				t.Fatal("builtin repeated")
			}
			seen[item.Name] = true
		}
		if !page.Data.HasMore {
			break
		}
		if page.Data.After == "" || page.Data.After == after {
			t.Fatal("builtin cursor did not advance")
		}
		after = page.Data.After
	}
	for _, name := range []string{"work_find", "work_read", "work_checkpoint"} {
		if !seen[name] {
			t.Fatalf("builtin %s undiscoverable: %+v", name, seen)
		}
	}
}

func TestDiscoveryEscapedMetadataRemainsReachable(t *testing.T) {
	p, _, _ := poolFixture(t, false)
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	source := reg.Servers["test"].Source
	ids := []string{strings.Repeat("a", 80), strings.Repeat("b", 80)}
	if err := p.Update(func(reg *Registry) error {
		reg.Servers = map[string]Server{}
		for _, id := range ids {
			reg.Servers[id] = Server{Description: "work" + strings.Repeat("x", 116), Source: source, Enabled: true, StartAllowed: true}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		hash := strings.Repeat("a", 64)
		tools := []opsstore.ToolRecord{}
		for _, name := range []string{strings.Repeat(`"`, 200), strings.Repeat(`"`, 199) + "a"} {
			tools = append(tools, opsstore.ToolRecord{Server: id, Name: name, ConfigHash: hash, SchemaHash: hash, Summary: "work" + strings.Repeat(`"`, 116), SchemaJSON: "{}"})
		}
		if err := p.Ops.SaveCatalog(id, hash, tools); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.NewInProcessClient(NewServer(p))
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
	for range 8 {
		r := schemaCall(t, ctx, c, "tools_find", map[string]any{"query": "work", "tool_after": after})
		body := schemaText(t, r)
		if r.IsError || len(body) > opsstore.SummaryLimit {
			t.Fatalf("accepted escaped metadata is undiscoverable: %s", body)
		}
		var page struct {
			Data discoveryPage `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		for _, tool := range page.Data.Tools {
			key := tool.Server + "\x00" + tool.Name
			if seen[key] {
				t.Fatal("escaped tool repeated")
			}
			seen[key] = true
		}
		if !page.Data.ToolHasMore {
			break
		}
		if page.Data.ToolAfter == "" || page.Data.ToolAfter == after {
			t.Fatal("escaped tool cursor did not advance")
		}
		after = page.Data.ToolAfter
	}
	if len(seen) != 4 {
		t.Fatalf("escaped tools lost: %d", len(seen))
	}
}

func TestToolsFindFiltersBeforePagingAndRetainsBudgetCursors(t *testing.T) {
	p, _, _ := poolFixture(t, false)
	p.Host = "claude-code"
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	source := reg.Servers["test"].Source
	if err := p.Update(func(reg *Registry) error {
		reg.Servers = map[string]Server{}
		for _, id := range []string{"a-secret", "b-secret", "c-secret", "visible-a", "visible-b", "visible-c", "visible-d"} {
			host := "claude-code"
			if strings.Contains(id, "secret") {
				host = "codex"
			}
			reg.Servers[id] = Server{Host: host, Description: strings.Repeat("work needle ", 20), Source: source, Enabled: true, StartAllowed: true}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a-secret", "b-secret", "c-secret", "visible-a", "visible-b", "visible-c", "visible-d"} {
		hash := strings.Repeat("a", 64)
		if err := p.Ops.SaveCatalog(id, hash, []opsstore.ToolRecord{{Server: id, Name: "work-needle", ConfigHash: hash, SchemaHash: hash, Summary: strings.Repeat("work needle ", 20), SchemaJSON: "{}"}}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.NewInProcessClient(NewServer(p))
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
	for range 8 {
		r := schemaCall(t, ctx, c, "tools_find", map[string]any{"query": "work", "tool_after": after})
		body := schemaText(t, r)
		if r.IsError || len(body) > opsstore.SummaryLimit || strings.Contains(body, "secret") {
			t.Fatalf("discovery leaked or exceeded budget: %s", body)
		}
		var page struct {
			Data struct {
				Tools   []opsstore.ToolSummary `json:"tools"`
				After   string                 `json:"tool_after"`
				HasMore bool                   `json:"tool_has_more"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		for _, tool := range page.Data.Tools {
			if seen[tool.Server] {
				t.Fatal("tool repeated")
			}
			seen[tool.Server] = true
		}
		if !page.Data.HasMore {
			break
		}
		if page.Data.After == "" || page.Data.After == after {
			t.Fatal("tool cursor did not advance")
		}
		after = page.Data.After
	}
	if len(seen) != 4 {
		t.Fatalf("visible tools lost to filtering or budget: %+v", seen)
	}
}
