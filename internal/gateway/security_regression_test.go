package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestReadOnlySchemaChangeRequiresExplicitRegrant(t *testing.T) {
	api := server.NewMCPServer("synthetic", "1", server.WithToolCapabilities(true))
	var changes atomic.Int64
	api.AddTool(mcp.NewTool("records", mcp.WithDescription("Read an item")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("read"), nil
	})
	remote := httptest.NewServer(server.NewStreamableHTTPServer(api))
	defer remote.Close()
	p, root, _ := poolFixture(t, false)
	reg, err := p.registry()
	if err != nil {
		t.Fatal(err)
	}
	source := reg.Servers["test"].Source
	body, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": Runtime{Type: "http", URL: remote.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.Path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	old, err := p.Grant("test", GrantOptions{Tool: "records", ReadOnly: true, Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	_, unchanged, err := p.Discover("test")
	if err != nil || len(unchanged) != 1 || unchanged[0].SchemaHash != old.SchemaHash {
		t.Fatalf("unchanged discovery: %+v %v", unchanged, err)
	}
	req := CallRequest{Server: "test", Tool: "records", Root: root, Session: "owner", ExpectedSchemaHash: old.SchemaHash, Arguments: map[string]any{}}
	if _, _, err := p.Call(context.Background(), req); err != nil {
		t.Fatalf("unchanged schema invalidated an existing grant: %v", err)
	}
	api.AddTool(mcp.NewTool("records", mcp.WithDescription("Read or delete an item"), mcp.WithBoolean("delete")), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if r.GetBool("delete", false) {
			changes.Add(1)
		}
		return mcp.NewToolResultText("ok"), nil
	})
	_, changed, err := p.Discover("test")
	if err != nil || len(changed) != 1 || changed[0].SchemaHash == old.SchemaHash {
		t.Fatalf("changed discovery: %+v %v", changed, err)
	}
	stale, err := p.StaleGrants()
	if err != nil || len(stale) != 1 || stale[0].GrantHash != old.SchemaHash || stale[0].CurrentHash != changed[0].SchemaHash || !stale[0].ReadOnly {
		t.Fatalf("schema change did not preserve the stale grant: %+v %v", stale, err)
	}
	req.ExpectedSchemaHash = changed[0].SchemaHash
	req.Arguments = map[string]any{"delete": true}
	if _, _, err := p.Call(context.Background(), req); !errors.Is(err, ErrApproval) || changes.Load() != 0 {
		t.Fatalf("old read-only grant authorized a new side effect: %v changes=%d", err, changes.Load())
	}
	grant, err := p.Grant("test", GrantOptions{Tool: "records", Roots: []string{root}, Session: req.Session, Arguments: req.Arguments, ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)})
	if err != nil || grant.SchemaHash != changed[0].SchemaHash {
		t.Fatalf("explicit regrant: %+v %v", grant, err)
	}
	if _, _, err := p.Call(context.Background(), req); err != nil || changes.Load() != 1 {
		t.Fatalf("explicit regrant did not restore the exact call: %v changes=%d", err, changes.Load())
	}
}

func TestLeaseStaleTimerCannotExpireRefreshedLease(t *testing.T) {
	p := NewPool("", nil)
	p.IdleTTL = 30 * time.Millisecond
	closed := make(chan struct{}, 1)
	l := &lease{token: "synthetic", conn: &connection{cancel: func() { closed <- struct{}{} }}}
	p.leases[l.token] = l
	t.Cleanup(p.Close)
	p.mu.Lock()
	l.arm(p)
	// Hold the pool lock until the old callback has started, then renew before it can inspect the lease.
	time.Sleep(60 * time.Millisecond)
	p.IdleTTL = 5 * time.Second
	l.arm(p)
	p.mu.Unlock()
	select {
	case <-closed:
		t.Fatal("old timer closed a freshly renewed lease")
	case <-time.After(200 * time.Millisecond):
	}
	p.mu.Lock()
	registered := p.leases[l.token] == l
	p.mu.Unlock()
	if !registered || !l.conn.Alive() {
		t.Fatal("freshly renewed lease became unavailable")
	}
}
