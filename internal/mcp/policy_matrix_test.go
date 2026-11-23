package mcp

import (
	"testing"

	"recalldock.local/recalldock/internal/memorypolicy"
)

// mcpPolicyMatrix is the reviewed transport policy for every registered tool.
// Registration must never widen what a model may do: a tool that is not an
// explicit read/lifecycle/proposal path must stay denied.
var mcpPolicyMatrix = map[string]memorypolicy.Decision{
	"mem_search":            memorypolicy.Read,
	"mem_context":           memorypolicy.Read,
	"mem_get_observation":   memorypolicy.Read,
	"mem_current_project":   memorypolicy.Read,
	"mem_suggest_topic_key": memorypolicy.Read,
	"mem_stats":             memorypolicy.Read,
	"mem_timeline":          memorypolicy.Read,
	"mem_doctor":            memorypolicy.Read,

	"mem_commit_session": memorypolicy.Propose,

	"mem_session_start": memorypolicy.Lifecycle,
	"mem_session_end":   memorypolicy.Lifecycle,

	// Operator-only surfaces stay denied on agent transports.
	"mem_review":          memorypolicy.Deny,
	"mem_save":            memorypolicy.Deny,
	"mem_update":          memorypolicy.Deny,
	"mem_delete":          memorypolicy.Deny,
	"mem_save_prompt":     memorypolicy.Deny,
	"mem_pin":             memorypolicy.Deny,
	"mem_unpin":           memorypolicy.Deny,
	"mem_session_summary": memorypolicy.Deny,
	"mem_capture_passive": memorypolicy.Deny,
	"mem_merge_projects":  memorypolicy.Deny,
	"mem_judge":           memorypolicy.Deny,
	"mem_compare":         memorypolicy.Deny,
}

// TestEveryMCPToolHasReviewedPolicy ties the registered tool surface to the
// shared policy so a new tool cannot become callable without review.
func TestEveryMCPToolHasReviewedPolicy(t *testing.T) {
	s := newMCPTestStore(t)
	srv := NewServerWithConfig(s, MCPConfig{}, nil)
	tools := srv.ListTools()
	if len(tools) == 0 {
		t.Fatal("no tools registered")
	}
	for name := range tools {
		want, ok := mcpPolicyMatrix[name]
		if !ok {
			t.Fatalf("tool %q has no reviewed transport policy; add it to mcpPolicyMatrix after review", name)
		}
		if got := memorypolicy.MCP(name, ""); got != want {
			t.Fatalf("tool %q: policy=%s want=%s", name, got, want)
		}
	}
	for name := range mcpPolicyMatrix {
		if _, ok := tools[name]; !ok {
			t.Fatalf("stale policy entry %q: tool is no longer registered", name)
		}
	}
}

// TestMCPReviewIsListOnly pins the operator/agent split on mem_review.
func TestMCPReviewIsListOnly(t *testing.T) {
	if got := memorypolicy.MCP("mem_review", "list"); got != memorypolicy.Read {
		t.Fatalf("review list: %s", got)
	}
	for _, action := range []string{"", "approve", "reject", "promote", "archive"} {
		if got := memorypolicy.MCP("mem_review", action); got != memorypolicy.Deny {
			t.Fatalf("review %q: %s", action, got)
		}
	}
}

// TestMCPAdvertisedVersionComesFromConfig ensures the initialize response
// reports the build version instead of a hardcoded placeholder.
func TestMCPAdvertisedVersionComesFromConfig(t *testing.T) {
	s := newMCPTestStore(t)
	srv := NewServerWithConfig(s, MCPConfig{Version: "9.9.9-test"}, nil)
	if got := srv.ListTools(); len(got) == 0 {
		t.Fatal("no tools registered")
	}
	// The mcp-go server keeps the advertised version privately; exercise the
	// same normalization used at construction.
	if got := mcpServerVersion(MCPConfig{Version: "9.9.9-test"}); got != "9.9.9-test" {
		t.Fatalf("version=%q", got)
	}
	if got := mcpServerVersion(MCPConfig{}); got != "dev" {
		t.Fatalf("empty version=%q", got)
	}
}
