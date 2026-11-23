package gateway

import (
	"context"
	"encoding/json"
	"errors"
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

func registerTestBackend(t *testing.T, p *Pool, sourceDir, serverID, mode string, stateful bool) Source {
	t.Helper()
	rt := backendRuntime(t, mode)
	marker := filepath.Join(sourceDir, serverID+".started")
	rt.Env["RECALLDOCK_GATEWAY_TEST_STARTED"] = marker
	srcPath := filepath.Join(sourceDir, serverID+".json")
	body, err := json.Marshal(map[string]any{"mcpServers": map[string]any{serverID: rt}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	src := Source{Path: srcPath, Format: "json", Keys: []string{"mcpServers", serverID}}
	if err := p.Update(func(r *Registry) error {
		if _, exists := r.Servers[serverID]; exists {
			return errors.New("server already registered")
		}
		r.Servers[serverID] = Server{
			Host:         "codex",
			Description:  "Synthetic test backend " + serverID,
			Source:       src,
			Enabled:      true,
			StartAllowed: true,
			Stateful:     stateful,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestGatewayMatrixBackendsZeroOneSevenEight(t *testing.T) {
	wantBuiltins := map[string]bool{
		"tools_find": true, "tools_describe": true, "tools_call": true, "tools_release": true,
		"work_find": true, "work_read": true, "work_checkpoint": true,
	}
	for _, count := range []int{0, 1, 7, 8} {
		t.Run(fmt.Sprintf("%d_backends", count), func(t *testing.T) {
			dir := t.TempDir()
			ops, err := opsstore.Open(filepath.Join(dir, "ops"), true)
			if err != nil {
				t.Fatal(err)
			}
			defer ops.Close()
			pool := NewPool(filepath.Join(dir, "gateway.json"), ops)
			pool.Trust = "agent"
			defer pool.Close()

			for i := 1; i <= count; i++ {
				id := fmt.Sprintf("srv-%02d", i)
				registerTestBackend(t, pool, dir, id, "normal", false)
			}
			if count > 0 {
				if _, _, err := pool.Discover("srv-01"); err != nil {
					t.Fatal(err)
				}
				if count >= 8 {
					if _, _, err := pool.Discover("srv-08"); err != nil {
						t.Fatal(err)
					}
				}
			}

			c, err := client.NewInProcessClient(NewServer(pool))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx := context.Background()
			if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
				ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
				ClientInfo:      mcp.Implementation{Name: "matrix", Version: "1"},
			}}); err != nil {
				t.Fatal(err)
			}

			listed, err := c.ListTools(ctx, mcp.ListToolsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(listed.Tools) != len(wantBuiltins) {
				t.Fatalf("expected 7 gateway tools with %d backends, got %d", count, len(listed.Tools))
			}
			for _, tool := range listed.Tools {
				if !wantBuiltins[tool.Name] {
					t.Fatalf("unexpected exposed tool: %q", tool.Name)
				}
			}

			// Paginate tools_find using `after` to verify all backends beyond 7 are discoverable.
			foundServers := map[string]bool{}
			after := ""
			for page := 0; page < 10; page++ {
				args := map[string]any{}
				if after != "" {
					args["after"] = after
				}
				res := schemaCall(t, ctx, c, "tools_find", args)
				var parsed struct {
					Data struct {
						Servers []struct {
							ID string `json:"id"`
						} `json:"servers"`
						HasMore bool   `json:"has_more"`
						After   string `json:"after"`
					} `json:"data"`
				}
				if err := json.Unmarshal([]byte(schemaText(t, res)), &parsed); err != nil {
					t.Fatal(err)
				}
				for _, srv := range parsed.Data.Servers {
					foundServers[srv.ID] = true
				}
				if !parsed.Data.HasMore || parsed.Data.After == "" {
					break
				}
				after = parsed.Data.After
			}
			if len(foundServers) != count {
				t.Fatalf("tools_find returned %d servers; want %d (%+v)", len(foundServers), count, foundServers)
			}
			if count >= 8 {
				root := t.TempDir()
				desc := schemaCall(t, ctx, c, "tools_describe", map[string]any{"server": "srv-08", "tool": "echo"})
				if desc.IsError {
					t.Fatalf("describe 8th backend failed: %s", schemaText(t, desc))
				}
				call := schemaCall(t, ctx, c, "tools_call", map[string]any{
					"server": "srv-08", "tool": "echo", "root": root,
					"arguments": map[string]any{"message": "backend-eight-ok"},
				})
				if call.IsError || !strings.Contains(schemaText(t, call), "backend-eight-ok") {
					t.Fatalf("8th backend call failed: %s", schemaText(t, call))
				}
			}
		})
	}
}

func TestGatewayMatrixLiveAddRemoveAndLeaseCleanupWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	ops, err := opsstore.Open(filepath.Join(dir, "ops"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer ops.Close()
	pool := NewPool(filepath.Join(dir, "gateway.json"), ops)
	pool.Trust = "agent"
	defer pool.Close()

	registerTestBackend(t, pool, dir, "alpha", "normal", true)
	if _, _, err := pool.Discover("alpha"); err != nil {
		t.Fatal(err)
	}

	c, err := client.NewInProcessClient(NewServer(pool))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "live-matrix", Version: "1"},
	}}); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	callAlpha := schemaCall(t, ctx, c, "tools_call", map[string]any{
		"server": "alpha", "tool": "echo", "root": root, "arguments": map[string]any{"message": "alpha-1"},
	})
	if callAlpha.IsError {
		t.Fatal(schemaText(t, callAlpha))
	}
	var leaseInfo struct {
		Gateway struct {
			Lease string `json:"lease"`
		} `json:"recalldock_gateway"`
	}
	if len(callAlpha.Content) < 2 {
		t.Fatalf("expected lease block, got %+v", callAlpha.Content)
	}
	if err := json.Unmarshal([]byte(callAlpha.Content[len(callAlpha.Content)-1].(mcp.TextContent).Text), &leaseInfo); err != nil || leaseInfo.Gateway.Lease == "" {
		t.Fatalf("missing lease token: %v", callAlpha.Content)
	}
	alphaLeaseToken := leaseInfo.Gateway.Lease

	// Add and discover "beta" WITHOUT restarting the live gateway client.
	registerTestBackend(t, pool, dir, "beta", "normal", true)
	if _, _, err := pool.Discover("beta"); err != nil {
		t.Fatal(err)
	}
	findBeta := schemaCall(t, ctx, c, "tools_find", map[string]any{"query": "beta"})
	if findBeta.IsError || !strings.Contains(schemaText(t, findBeta), "beta") {
		t.Fatalf("live gateway did not pick up added server beta: %s", schemaText(t, findBeta))
	}
	callBeta := schemaCall(t, ctx, c, "tools_call", map[string]any{
		"server": "beta", "tool": "echo", "root": root, "arguments": map[string]any{"message": "beta-ok"},
	})
	if callBeta.IsError || !strings.Contains(schemaText(t, callBeta), "beta-ok") {
		t.Fatalf("live gateway failed calling newly added beta: %s", schemaText(t, callBeta))
	}
	var betaLeaseInfo struct {
		Gateway struct {
			Lease string `json:"lease"`
		} `json:"recalldock_gateway"`
	}
	_ = json.Unmarshal([]byte(callBeta.Content[len(callBeta.Content)-1].(mcp.TextContent).Text), &betaLeaseInfo)

	// Simulate an external CLI `gateway remove` removing "alpha" from gateway.json on disk + ops catalog.
	if err := pool.Update(func(r *Registry) error {
		delete(r.Servers, "alpha")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Ops.RemoveServerCatalog("alpha"); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	alphaLease := pool.leases[alphaLeaseToken]
	pool.mu.Unlock()
	if alphaLease == nil {
		t.Fatal("expected active lease for alpha before call")
	}

	// Live gateway must immediately reject old lease & close its connection.
	staleCall := schemaCall(t, ctx, c, "tools_call", map[string]any{
		"server": "alpha", "tool": "echo", "root": root, "lease": alphaLeaseToken,
		"arguments": map[string]any{"message": "must-fail"},
	})
	if !staleCall.IsError || !strings.Contains(schemaText(t, staleCall), "approval_required") {
		t.Fatalf("removed server call did not fail closed: %s", schemaText(t, staleCall))
	}
	if alphaLease.conn.Alive() {
		t.Fatal("stale lease connection remained open after external server removal")
	}
	descRemoved := schemaCall(t, ctx, c, "tools_describe", map[string]any{"server": "alpha", "tool": "echo"})
	if !descRemoved.IsError {
		t.Fatalf("tools_describe still returned removed server")
	}

	// Remove "beta" via pool.Remove WHILE an in-flight slow call is running on beta.
	inFlightDone := make(chan *mcp.CallToolResult, 1)
	go func() {
		inFlightDone <- schemaCall(t, ctx, c, "tools_call", map[string]any{
			"server": "beta", "tool": "echo", "root": root, "lease": betaLeaseInfo.Gateway.Lease,
			"arguments": map[string]any{"message": "wait"},
		})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "beta.started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("in-flight beta backend did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := pool.Remove("beta"); err != nil {
		t.Fatal(err)
	}
	res := <-inFlightDone
	if res.IsError {
		t.Fatalf("in-flight call crashed when removed cleanly: %s", schemaText(t, res))
	}
	afterRemove := schemaCall(t, ctx, c, "tools_call", map[string]any{
		"server": "beta", "tool": "echo", "root": root, "lease": betaLeaseInfo.Gateway.Lease,
		"arguments": map[string]any{"message": "after-remove"},
	})
	if !afterRemove.IsError || !strings.Contains(schemaText(t, afterRemove), "approval_required") {
		t.Fatalf("deleted beta allowed post-removal call: %s", schemaText(t, afterRemove))
	}

	// Re-adding alpha must fail closed until freshly discovered (no stale catalog reuse).
	registerTestBackend(t, pool, dir, "alpha", "normal", false)
	readded := schemaCall(t, ctx, c, "tools_call", map[string]any{
		"server": "alpha", "tool": "echo", "root": root, "arguments": map[string]any{"message": "before-discover"},
	})
	if !readded.IsError || !strings.Contains(schemaText(t, readded), "backend_not_discovered") {
		t.Fatalf("re-added server must require discovery: %s", schemaText(t, readded))
	}
}

func TestGatewayMatrixConfigSchemaEnvAndUnicodePaths(t *testing.T) {
	dir := t.TempDir()
	ops, err := opsstore.Open(filepath.Join(dir, "ops"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer ops.Close()
	pool := NewPool(filepath.Join(dir, "gateway.json"), ops)
	pool.Trust = "strict"
	defer pool.Close()

	spaceRoot := filepath.Join(t.TempDir(), "工作 目录", "My Project Space")
	if err := os.MkdirAll(spaceRoot, 0700); err != nil {
		t.Fatal(err)
	}

	src := registerTestBackend(t, pool, dir, "srv-uni", "normal", false)
	// Duplicate registration must fail.
	if err := pool.Update(func(r *Registry) error {
		if _, exists := r.Servers["srv-uni"]; exists {
			return errors.New("server already registered")
		}
		return nil
	}); err == nil {
		t.Fatal("duplicate registration accepted")
	}

	_, records, err := pool.Discover("srv-uni")
	if err != nil || len(records) == 0 {
		t.Fatalf("discover: %v", err)
	}
	grant, err := pool.Grant("srv-uni", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{spaceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	req := CallRequest{
		Server: "srv-uni", Tool: "echo", Root: spaceRoot, Session: "sess-1",
		ExpectedSchemaHash: grant.SchemaHash,
		Arguments:          map[string]any{"message": "中文路径通过"},
	}
	out, _, err := pool.Call(context.Background(), req)
	if err != nil || len(out.Content) == 0 {
		t.Fatalf("unicode/space root call failed: %+v %v", out, err)
	}

	// Change live schema without changing command/args (swap mode -> schema_v2 via source).
	rt2 := backendRuntime(t, "schema_v2")
	body2, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"srv-uni": rt2}})
	if err := os.WriteFile(src.Path, body2, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.Call(context.Background(), req); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed config/env must fail with ErrChanged: %v", err)
	}
	// Rediscover picks up new schema hash: old expected schema rejects with ErrChanged,
	// and calling with the new schema hash still rejects with ErrApproval until re-granted.
	_, newRecords, err := pool.Discover("srv-uni")
	if err != nil || len(newRecords) == 0 {
		t.Fatalf("rediscover: %v", err)
	}
	if _, _, err := pool.Call(context.Background(), req); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale request schema hash should fail with ErrChanged: %v", err)
	}
	req.ExpectedSchemaHash = newRecords[0].SchemaHash
	if _, _, err := pool.Call(context.Background(), req); !errors.Is(err, ErrApproval) {
		t.Fatalf("strict grant survived config+schema change: %v", err)
	}

	// Simulate backend binary removed/missing -> ErrUnavailable / launch failure without panic.
	rtMissing := Runtime{Type: "stdio", Command: filepath.Join(dir, "deleted-backend-binary"), Args: []string{}}
	bodyMissing, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"srv-uni": rtMissing}})
	if err := os.WriteFile(src.Path, bodyMissing, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.Discover("srv-uni"); err == nil {
		t.Fatal("missing backend binary should fail discover")
	}
}
