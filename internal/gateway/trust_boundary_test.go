package gateway

import (
	"context"
	"errors"
	"testing"
)

// TestAgentTrustRootScopeDecision documents and pins the agent-trust boundary.
//
// "agent" is a deliberate mode: it delegates the call decision to the local
// host agent instead of enforcing per-tool grants. Two properties follow, and
// both are intentional rather than bugs:
//
//   - An enabled backend serves every catalogued tool without a per-tool Grant.
//   - The Root argument is accepted as given, so a backend can be driven from
//     any absolute directory the agent supplies.
//
// What must NOT follow is that a host-scoped backend becomes reachable from a
// different host, that a disabled backend starts serving, or that a non-absolute
// root is accepted. Those are real boundaries and this test asserts they hold
// under agent trust too.
func TestAgentTrustRootScopeDecision(t *testing.T) {
	t.Run("undiscovered tool requires explicit refresh", func(t *testing.T) {
		p, root, _ := poolFixture(t, false)
		p.Trust = "agent"
		// Without discovery the catalog is empty, so the call cannot resolve a
		// tool even though trust is delegated.
		_, _, err := p.Call(context.Background(), CallRequest{
			Server: "test", Tool: "image", Root: root, Session: "s", Arguments: map[string]any{},
		})
		if err == nil {
			t.Fatal("agent trust served a tool that was never discovered")
		}
	})

	t.Run("arbitrary absolute root is accepted under agent trust", func(t *testing.T) {
		p, root, _ := poolFixture(t, false)
		p.Trust = "agent"
		if _, _, err := p.Discover("test"); err != nil {
			t.Fatal(err)
		}
		// A root unrelated to any grant still works: this is the documented
		// delegation, recorded here so a future change to it is deliberate.
		other := t.TempDir()
		if _, _, err := p.Call(context.Background(), CallRequest{
			Server: "test", Tool: "image", Root: other, Session: "s", Arguments: map[string]any{},
		}); err != nil {
			t.Fatalf("agent trust rejected an absolute root: %v", err)
		}
		_ = root
	})

	t.Run("non-absolute root stays refused", func(t *testing.T) {
		p, _, _ := poolFixture(t, false)
		p.Trust = "agent"
		if _, _, err := p.Discover("test"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.Call(context.Background(), CallRequest{
			Server: "test", Tool: "image", Root: "relative/path", Session: "s", Arguments: map[string]any{},
		}); !errors.Is(err, ErrApproval) {
			t.Fatalf("agent trust accepted a relative root: %v", err)
		}
	})

	t.Run("host mismatch stays refused", func(t *testing.T) {
		p, root, _ := poolFixture(t, false)
		p.Trust = "agent"
		p.Host = "codex"
		// Bind the backend to a different host before discovery: the host check
		// runs on both Discover and Call, so a mismatch must block at discovery.
		if err := p.Update(func(reg *Registry) error {
			srv := reg.Servers["test"]
			srv.Host = "claude-code"
			reg.Servers["test"] = srv
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.Discover("test"); !errors.Is(err, ErrApproval) {
			t.Fatalf("agent trust crossed host boundary at discovery: %v", err)
		}
		if _, _, err := p.Call(context.Background(), CallRequest{
			Server: "test", Tool: "image", Root: root, Session: "s", Arguments: map[string]any{},
		}); !errors.Is(err, ErrApproval) {
			t.Fatalf("agent trust crossed host boundary at call: %v", err)
		}
	})

	t.Run("disabled backend stays refused", func(t *testing.T) {
		p, root, _ := poolFixture(t, false)
		p.Trust = "agent"
		if _, _, err := p.Discover("test"); err != nil {
			t.Fatal(err)
		}
		if err := p.Update(func(reg *Registry) error {
			srv := reg.Servers["test"]
			srv.Enabled = false
			reg.Servers["test"] = srv
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := p.Call(context.Background(), CallRequest{
			Server: "test", Tool: "image", Root: root, Session: "s", Arguments: map[string]any{},
		}); !errors.Is(err, ErrApproval) {
			t.Fatalf("agent trust served a disabled backend: %v", err)
		}
	})
}

// TestStrictTrustRequiresGrant pins the contrast: the same call that agent trust
// permits is refused under strict trust until an explicit grant exists.
func TestStrictTrustRequiresGrant(t *testing.T) {
	p, root, _ := poolFixture(t, false)
	p.Trust = "strict"
	if _, _, err := p.Discover("test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Call(context.Background(), CallRequest{
		Server: "test", Tool: "image", Root: root, Session: "s", Arguments: map[string]any{},
	}); !errors.Is(err, ErrApproval) {
		t.Fatalf("strict trust served an un-granted tool: %v", err)
	}
	if _, err := p.Grant("test", GrantOptions{Tool: "image", ReadOnly: true, Roots: []string{root}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Call(context.Background(), CallRequest{
		Server: "test", Tool: "image", Root: root, Session: "s", Arguments: map[string]any{},
	}); err != nil {
		t.Fatalf("strict trust rejected a granted tool: %v", err)
	}
}
