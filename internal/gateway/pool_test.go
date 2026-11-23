package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"recalldock.local/recalldock/internal/opsstore"
)

func poolFixture(t *testing.T, stateful bool) (*Pool, string, string) {
	t.Helper()
	dir := t.TempDir()
	runtime := backendRuntime(t, "normal")
	marker := filepath.Join(dir, "started")
	runtime.Env["RECALLDOCK_GATEWAY_TEST_STARTED"] = marker
	source := sourceFixture(t, "json", "{}")
	body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": runtime}})
	if err := os.WriteFile(source.Path, body, 0600); err != nil {
		t.Fatal(err)
	}
	ops, err := opsstore.Open(filepath.Join(dir, "data"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ops.Close() })
	p := NewPool(filepath.Join(dir, "gateway.json"), ops)
	p.Trust = "strict"
	t.Cleanup(p.Close)
	if err := p.Update(func(r *Registry) error {
		r.Servers["test"] = Server{Source: source, Enabled: true, StartAllowed: true, Stateful: stateful}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return p, t.TempDir(), marker
}

func TestPoolLivePermissionsConfigAndNativeContent(t *testing.T) {
	p, root, _ := poolFixture(t, false)
	ctx := context.Background()
	req := CallRequest{Server: "test", Tool: "image", Root: root, Session: "test", Arguments: map[string]any{}}
	if _, _, err := p.Call(ctx, req); err == nil {
		t.Fatal("undiscovered call accepted")
	}
	_, records, err := p.Discover("test")
	if err != nil || len(records) != 2 {
		t.Fatalf("discover: %v", err)
	}
	if _, _, err := p.Call(ctx, req); !errors.Is(err, ErrApproval) {
		t.Fatalf("missing permission: %v", err)
	}
	g, err := p.Grant("test", GrantOptions{Tool: "image", ReadOnly: true, Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedSchemaHash = g.SchemaHash
	result, token, err := p.Call(ctx, req)
	if err != nil || !result.IsError || len(result.Content) != 1 || token != "" {
		t.Fatalf("native result: %+v %s %v", result, token, err)
	}
	req.ExpectedSchemaHash = "changed"
	if _, _, err := p.Call(ctx, req); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale request schema: %v", err)
	}
	req.ExpectedSchemaHash = g.SchemaHash
	if err := p.Revoke("test", "image"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Call(ctx, req); !errors.Is(err, ErrApproval) {
		t.Fatalf("revoked permission: %v", err)
	}
	if _, err := p.Grant("test", GrantOptions{Tool: "image", ReadOnly: true, Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	reg, _ := p.registry()
	source := reg.Servers["test"].Source
	runtime, _, _ := source.Read()
	runtime.Args = append(runtime.Args, "-test.count=1")
	body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": runtime}})
	if err := os.WriteFile(source.Path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Call(ctx, req); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed config: %v", err)
	}
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Call(ctx, req); !errors.Is(err, ErrApproval) {
		t.Fatalf("rediscovery reused permission for different config: %v", err)
	}
	if err := p.Remove("test"); err != nil {
		t.Fatal(err)
	}
	records, err = p.Ops.LoadCatalog("test")
	if err != nil || len(records) != 0 {
		t.Fatal("removed cache survives")
	}
	if _, err := os.Stat(source.Path); err != nil {
		t.Fatal("removed original configuration")
	}
}

func TestLeaseReleaseDuringCallAndIsolation(t *testing.T) {
	p, root, marker := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Grant("test", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	req := CallRequest{Server: "test", Tool: "echo", Root: root, Session: "owner", Arguments: map[string]any{"message": "first"}}
	_, token, err := p.Call(context.Background(), req)
	if err != nil || token == "" {
		t.Fatalf("lease: %v", err)
	}
	req.Lease = token
	other := req
	other.Session = "other"
	if _, _, err := p.Call(context.Background(), other); !errors.Is(err, ErrLease) {
		t.Fatal("another session reused lease")
	}
	p.mu.Lock()
	l := p.leases[token]
	p.mu.Unlock()
	req.Arguments = map[string]any{"message": "wait"}
	done := make(chan error, 1)
	go func() { _, _, err := p.Call(context.Background(), req); done <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backend did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.Release(token); err != nil {
		t.Fatal(err)
	}
	if !l.conn.Alive() {
		t.Fatal("release killed an active call")
	}
	if err := <-done; err != nil {
		t.Fatalf("released active call failed: %v", err)
	}
	if l.conn.Alive() {
		t.Fatal("released connection leaked")
	}
	if _, _, err := p.Call(context.Background(), req); !errors.Is(err, ErrLease) {
		t.Fatal("released lease reused")
	}
}

func TestCallTimeoutNeverRetriesAndLeaseExpires(t *testing.T) {
	p, root, marker := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Grant("test", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	p.IdleTTL = 80 * time.Millisecond
	req := CallRequest{Server: "test", Tool: "echo", Root: root, Session: "owner", Arguments: map[string]any{"message": "first"}}
	_, token, err := p.Call(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	l := p.leases[token]
	p.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for l.conn.Alive() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if l.conn.Alive() {
		t.Fatal("idle connection leaked")
	}
	req.Lease = ""
	// Establish the connection before timing the business call. Slow race
	// instrumentation/startup is not evidence that a request was dispatched.
	p.IdleTTL = 10 * time.Second
	_, token, err = p.Call(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Lease = token
	req.Arguments = map[string]any{"message": "wait"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := p.Call(ctx, req); done <- err }()
	deadline = time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("business request was not dispatched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, ErrUnknown) {
		t.Fatalf("timeout incorrectly classified: %v", err)
	}
	if body, err := os.ReadFile(marker); err != nil || strings.Count(string(body), "started\n") != 1 {
		t.Fatal("business request was retried")
	}
}

func TestCanceledCallDoesNotStartStatefulBackend(t *testing.T) {
	p, root, _ := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Grant("test", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := p.Call(ctx, CallRequest{Server: "test", Tool: "echo", Root: root, Session: "owner", Arguments: map[string]any{"message": "first"}})
	if err == nil || errors.Is(err, ErrUnknown) {
		t.Fatalf("undispatched canceled request: %v", err)
	}
	p.mu.Lock()
	leases := len(p.leases)
	p.mu.Unlock()
	if leases != 0 {
		t.Fatal("canceled caller started/retained a backend")
	}
}

func TestReconfiguredBackendCannotReuseOldLease(t *testing.T) {
	p, root, _ := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Grant("test", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	req := CallRequest{Server: "test", Tool: "echo", Root: root, Session: "owner", Arguments: map[string]any{"message": "before"}}
	_, token, err := p.Call(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := p.registry()
	source := reg.Servers["test"].Source
	runtime, _, _ := source.Read()
	runtime.Args = append(runtime.Args, "-test.count=1")
	body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": runtime}})
	if err := os.WriteFile(source.Path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Grant("test", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	req.Lease = token
	if _, _, err := p.Call(context.Background(), req); !errors.Is(err, ErrChanged) {
		t.Fatalf("new configuration grant reused old backend connection: %v", err)
	}
	req.Lease = ""
	if _, _, err := p.Call(context.Background(), req); err != nil {
		t.Fatalf("new backend not available: %v", err)
	}
}

func TestResolvedConfigChangeRevokesLiveLease(t *testing.T) {
	for _, field := range []string{"environment value", "transport alias", "resolved argument"} {
		t.Run(field, func(t *testing.T) {
			p, root, _ := poolFixture(t, true)
			reg, _ := p.registry()
			source := reg.Servers["test"].Source
			runtime, _, err := source.Read()
			if err != nil {
				t.Fatal(err)
			}
			if field == "transport alias" {
				runtime.Type, runtime.Transport, runtime.URL = "", "stdio", "http://127.0.0.1:1/mcp"
			}
			if field == "resolved argument" {
				t.Setenv("GATEWAY_TEST_EXECUTION_ARGUMENT", "-test.count=1")
				runtime.Args = append(runtime.Args, "${GATEWAY_TEST_EXECUTION_ARGUMENT}")
			}
			save := func() {
				t.Helper()
				body, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": runtime}})
				if err := os.WriteFile(source.Path, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			save()
			if _, _, err := p.Discover("test"); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Grant("test", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{root}}); err != nil {
				t.Fatal(err)
			}
			req := CallRequest{Server: "test", Tool: "echo", Root: root, Session: "owner", Arguments: map[string]any{"message": "verified"}}
			_, token, err := p.Call(context.Background(), req)
			if err != nil || token == "" {
				t.Fatal("initial call did not establish lease")
			}
			p.mu.Lock()
			owned := p.leases[token]
			p.mu.Unlock()
			switch field {
			case "environment value":
				runtime.Env["RECALLDOCK_GATEWAY_TEST_BACKEND"] = "stderr"
				save()
			case "transport alias":
				runtime.Transport = "http"
				save()
			case "resolved argument":
				t.Setenv("GATEWAY_TEST_EXECUTION_ARGUMENT", "-test.count=2")
			}
			req.Lease = token
			if _, _, err := p.Call(context.Background(), req); !errors.Is(err, ErrChanged) {
				t.Fatalf("old live lease executed after config change: %v", err)
			}
			if owned.conn.Alive() {
				t.Fatal("obsolete connection remained alive")
			}
		})
	}
}

func TestLeaseCannotCrossGatewayOrRootOrHost(t *testing.T) {
	p, root, _ := poolFixture(t, true)
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Grant("test", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	req := CallRequest{Server: "test", Tool: "echo", Root: root, Session: "owner", Arguments: map[string]any{"message": "verified"}}
	_, token, err := p.Call(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Lease = token
	other := NewPool(p.RegistryPath, p.Ops)
	defer other.Close()
	if _, _, err := other.Call(context.Background(), req); !errors.Is(err, ErrLease) {
		t.Fatalf("another gateway used lease: %v", err)
	}
	if err := other.Release(token); !errors.Is(err, ErrLease) {
		t.Fatal("another gateway released lease")
	}
	otherRoot := t.TempDir()
	if _, err := p.Grant("test", GrantOptions{Tool: "echo", ReadOnly: true, Roots: []string{root, otherRoot}}); err != nil {
		t.Fatal(err)
	}
	req.Root = otherRoot
	if _, _, err := p.Call(context.Background(), req); !errors.Is(err, ErrLease) {
		t.Fatalf("another root used lease: %v", err)
	}
	req.Root = root
	if err := p.Update(func(r *Registry) error {
		s := r.Servers["test"]
		s.Host = "claude-code"
		r.Servers["test"] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.Host = "codex"
	if _, _, err := p.Call(context.Background(), req); !errors.Is(err, ErrApproval) {
		t.Fatalf("another host used grant: %v", err)
	}
	p.Host = "claude-code"
	if _, _, err := p.Call(context.Background(), req); err != nil {
		t.Fatalf("isolation test damaged owner lease: %v", err)
	}
	if err := p.Release(token); err != nil {
		t.Fatal(err)
	}
}

func TestPoolAgentTrustAllowsCatalogToolWithoutGrant(t *testing.T) {
	p, root, _ := poolFixture(t, false)
	p.Trust = "agent"
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	result, _, err := p.Call(context.Background(), CallRequest{Server: "test", Tool: "image", Root: root, Session: "agent-session", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("agent trust denied an un-granted tool: %v", err)
	}
	if result == nil {
		t.Fatal("agent trust returned no result")
	}
}

func TestPoolAgentTrustIgnoresStaleRegistryConfigHashButUsesLiveSchema(t *testing.T) {
	p, root, _ := poolFixture(t, false)
	p.Trust = "agent"
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	if err := p.Update(func(reg *Registry) error {
		server := reg.Servers["test"]
		server.ConfigHash = strings.Repeat("a", 64)
		reg.Servers["test"] = server
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Call(context.Background(), CallRequest{Server: "test", Tool: "image", Root: root, Session: "agent-session", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("stale registry hash blocked agent call: %v", err)
	}
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	reg, _ := p.registry()
	if reg.Servers["test"].ConfigHash == strings.Repeat("a", 64) {
		t.Fatal("discover did not align registry config hash with catalog")
	}
}
