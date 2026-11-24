package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"recalldock.local/recalldock/internal/opsstore"
)

func TestGatewayMetadataBackend(t *testing.T) {
	if os.Getenv("RECALLDOCK_GATEWAY_METADATA_BACKEND") != "1" {
		return
	}
	api := server.NewMCPServer("metadata-fixture", "1", server.WithToolCapabilities(true))
	api.AddTool(mcp.NewTool("write", mcp.WithString("value")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		foreign := false
		tokenPresent := false
		if req.Params.Meta != nil {
			foreign = req.Params.Meta.AdditionalFields["scope"] == "foreign"
			if req.Params.Meta.ProgressToken != nil {
				tokenPresent = true
				_ = api.SendNotificationToClient(ctx, "notifications/progress", map[string]any{"progressToken": req.Params.Meta.ProgressToken, "progress": 1})
				if _, err := os.Stat(".wait-progress"); err == nil {
					deadline := time.Now().Add(5 * time.Second)
					for {
						if _, err := os.Stat(".ack-progress"); err == nil {
							break
						}
						if time.Now().After(deadline) {
							return mcp.NewToolResultError("progress acknowledgement missing"), nil
						}
						time.Sleep(5 * time.Millisecond)
					}
				}
			}
		}
		b, _ := json.Marshal(map[string]any{"foreign": foreign, "progress": tokenPresent})
		return mcp.NewToolResultText(string(b)), nil
	})
	_ = server.NewStdioServer(api).Listen(context.Background(), os.Stdin, os.Stdout)
	os.Exit(0)
}

func TestGatewayWriteGrantDropsUnapprovedMetadataAndForwardsProgress(t *testing.T) {
	dir := t.TempDir()
	// The backend child needs a working directory of its own, and it must not be
	// the TempDir holding the ops store and registry: on Windows a terminating
	// child holds a handle on its cwd briefly, which would make RemoveAll fail
	// after the assertions had already passed. The backend reads and writes its
	// progress sentinels through this directory instead, so pass it in the env.
	backendDir := backendWorkDir(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{Type: "stdio", Command: exe, Args: []string{"-test.run=^TestGatewayMetadataBackend$"}, CWD: backendDir, Env: map[string]string{"RECALLDOCK_GATEWAY_METADATA_BACKEND": "1"}}
	source := sourceFixture(t, "json", "{}")
	raw, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": runtime}})
	if err = os.WriteFile(source.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ops, err := opsstore.Open(filepath.Join(dir, "data"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer ops.Close()
	p := NewPool(filepath.Join(dir, "registry.json"), ops)
	defer p.Close()
	if err = p.Update(func(reg *Registry) error {
		reg.Servers["test"] = Server{Source: source, Enabled: true, StartAllowed: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	c, err := client.NewInProcessClient(NewServer(p))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err = c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "metadata-test", Version: "1"}}}); err != nil {
		t.Fatal(err)
	}
	found, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "tools_find", Arguments: map[string]any{}}})
	if err != nil || found.IsError {
		t.Fatalf("find=%+v %v", found, err)
	}
	var body struct {
		Data struct {
			Session string `json:"session"`
		} `json:"data"`
	}
	if err = json.Unmarshal([]byte(found.Content[0].(mcp.TextContent).Text), &body); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"value": "permitted"}
	root := t.TempDir()
	grant, err := p.Grant("test", GrantOptions{Tool: "write", Roots: []string{root}, Session: body.Data.Session, Arguments: args, ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	for _, progress := range []bool{false, true} {
		meta := &mcp.Meta{AdditionalFields: map[string]any{"scope": "foreign"}}
		if progress {
			meta.ProgressToken = "host-token"
		}
		result, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "tools_call", Arguments: map[string]any{"server": "test", "tool": "write", "root": root, "arguments": args, "expected_schema_hash": grant.SchemaHash}, Meta: meta}})
		if err != nil || result.IsError {
			t.Fatalf("call=%+v err=%v", result, err)
		}
		var seen struct {
			Foreign  bool `json:"foreign"`
			Progress bool `json:"progress"`
		}
		if err = json.Unmarshal([]byte(result.Content[0].(mcp.TextContent).Text), &seen); err != nil {
			t.Fatal(err)
		}
		if seen.Foreign || seen.Progress != progress {
			t.Fatalf("backend received unapproved metadata: %+v", seen)
		}
	}
	// The mcp-go in-process transport does not register a notification session.
	// Verify the real stdio backend's progress through Pool's callback instead.
	direct, _, err := p.Call(ctx, CallRequest{Server: "test", Tool: "write", Root: root, Session: body.Data.Session, Arguments: args, ExpectedSchemaHash: grant.SchemaHash, Meta: &mcp.Meta{ProgressToken: map[string]any{"scope": "foreign"}, AdditionalFields: map[string]any{"scope": "foreign"}}})
	if err != nil {
		t.Fatal(err)
	}
	var directSeen struct {
		Foreign  bool `json:"foreign"`
		Progress bool `json:"progress"`
	}
	if err := json.Unmarshal([]byte(direct.Content[0].(mcp.TextContent).Text), &directSeen); err != nil || directSeen.Foreign || directSeen.Progress {
		t.Fatalf("metadata reached backend without notification callback: %+v %v", directSeen, err)
	}
	notices := make(chan mcp.JSONRPCNotification, 1)
	if err := os.WriteFile(filepath.Join(backendDir, ".wait-progress"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	notificationResult, _, err := p.Call(ctx, CallRequest{Server: "test", Tool: "write", Root: root, Session: body.Data.Session, Arguments: args, ExpectedSchemaHash: grant.SchemaHash, Meta: &mcp.Meta{ProgressToken: "host-token", AdditionalFields: map[string]any{"scope": "foreign"}}, Notify: func(n mcp.JSONRPCNotification) {
		notices <- n
		_ = os.WriteFile(filepath.Join(backendDir, ".ack-progress"), nil, 0600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if notificationResult.IsError {
		t.Fatalf("backend error: %+v", notificationResult)
	}
	select {
	case n := <-notices:
		if n.Params.AdditionalFields["progressToken"] != "host-token" {
			t.Fatalf("wrong progress token: %+v", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("progress notification missing")
	}
}
