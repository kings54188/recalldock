package gateway

import (
	"testing"
	"time"
)

// TestGrantExpiryIsEnforced pins the fail-closed grant matcher: an expired or
// unparseable expires_at never authorizes a call, while a live grant with the
// exact session and arguments fingerprint still does.
func TestGrantExpiryIsEnforced(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	args := map[string]any{"path": "/tmp/x"}
	server := Server{
		Enabled:      true,
		StartAllowed: true,
		Stateful:     true,
		Grants: []Grant{
			{ID: "g-expired", Tool: "run", SchemaHash: "sha", Roots: []string{"/tmp"}, ReadOnly: false,
				Session: "s1", ExpiresAt: past, ArgumentsHash: Fingerprint(args)},
		},
	}
	if _, ok := server.Allowed("run", "sha", "/tmp", "s1", args); ok {
		t.Fatal("expired grant authorized a call")
	}
	// Malformed expiry fails closed too.
	server.Grants[0].ExpiresAt = "not-a-time"
	if _, ok := server.Allowed("run", "sha", "/tmp", "s1", args); ok {
		t.Fatal("malformed expiry authorized a call")
	}
	// A live grant with matching session and arguments authorizes.
	server.Grants[0].ExpiresAt = future
	if _, ok := server.Allowed("run", "sha", "/tmp", "s1", args); !ok {
		t.Fatal("live grant rejected")
	}
	// Wrong session, wrong arguments and wrong root keep failing closed.
	if _, ok := server.Allowed("run", "sha", "/tmp", "other", args); ok {
		t.Fatal("foreign session authorized")
	}
	if _, ok := server.Allowed("run", "sha", "/tmp", "s1", map[string]any{"path": "/tmp/y"}); ok {
		t.Fatal("different arguments authorized")
	}
	if _, ok := server.Allowed("run", "sha", "/etc", "s1", args); ok {
		t.Fatal("foreign root authorized")
	}
}
