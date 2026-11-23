package server

import (
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/memorypolicy"
)

// httpPolicyMatrix is the reviewed transport policy for every registered route.
// It is intentionally explicit: adding a route without reviewing its policy
// fails this test instead of silently inheriting a default.
var httpPolicyMatrix = map[string]memorypolicy.Decision{
	"GET /health":                     memorypolicy.Read,
	"POST /sessions":                  memorypolicy.Lifecycle,
	"POST /sessions/{id}/end":         memorypolicy.Lifecycle,
	"POST /sessions/{id}/candidates":  memorypolicy.Propose,
	"GET /sessions/recent":            memorypolicy.Read,
	"GET /sessions/{id}":              memorypolicy.Read,
	"DELETE /sessions/{id}":           memorypolicy.Deny,
	"POST /observations":              memorypolicy.Deny,
	"GET /observations":               memorypolicy.Read,
	"POST /observations/passive":      memorypolicy.Deny,
	"GET /observations/recent":        memorypolicy.Read,
	"PATCH /observations/{id}":        memorypolicy.Deny,
	"DELETE /observations/{id}":       memorypolicy.Deny,
	"GET /search":                     memorypolicy.Read,
	"GET /timeline":                   memorypolicy.Read,
	"GET /observations/{id}":          memorypolicy.Read,
	"GET /review":                     memorypolicy.Read,
	"POST /review/mark_reviewed":      memorypolicy.Deny,
	"POST /prompts":                   memorypolicy.Deny,
	"GET /prompts/recent":             memorypolicy.Read,
	"GET /prompts/search":             memorypolicy.Read,
	"DELETE /prompts/{id}":            memorypolicy.Deny,
	"GET /context":                    memorypolicy.Read,
	"GET /export":                     memorypolicy.Deny,
	"POST /import":                    memorypolicy.Deny,
	"GET /stats":                      memorypolicy.Read,
	"GET /doctor":                     memorypolicy.Read,
	"GET /project/current":            memorypolicy.Read,
	"POST /projects/migrate":          memorypolicy.Deny,
	"GET /sync/status":                memorypolicy.Read,
	"GET /conflicts":                  memorypolicy.Read,
	"GET /conflicts/stats":            memorypolicy.Read,
	"GET /conflicts/deferred":         memorypolicy.Read,
	"POST /conflicts/scan":            memorypolicy.Deny,
	"POST /conflicts/judge":           memorypolicy.Deny,
	"POST /conflicts/compare":         memorypolicy.Deny,
	"POST /conflicts/deferred/replay": memorypolicy.Deny,
	"GET /conflicts/{relation_id}":    memorypolicy.Read,
}

// concretePath turns a registered pattern into a path the policy can classify.
func concretePath(pattern string) string {
	parts := strings.SplitN(pattern, " ", 2)
	if len(parts) != 2 {
		return pattern
	}
	path := parts[1]
	path = strings.ReplaceAll(path, "{relation_id}", "1")
	path = strings.ReplaceAll(path, "{id}", "sample-id")
	return path
}

// TestEveryHTTPRouteHasReviewedPolicy ensures the transport boundary cannot be
// widened by registering a new route: each one must either be an explicit
// read/lifecycle/proposal path or be denied to agent transports.
func TestEveryHTTPRouteHasReviewedPolicy(t *testing.T) {
	s := New(nil, 0)
	if len(s.routeTable) == 0 {
		t.Fatal("no routes recorded")
	}
	seen := make(map[string]bool, len(s.routeTable))
	for _, registered := range s.routeTable {
		seen[registered] = true
		want, ok := httpPolicyMatrix[registered]
		if !ok {
			t.Fatalf("route %q has no reviewed transport policy; add it to httpPolicyMatrix after review", registered)
		}
		method := strings.SplitN(registered, " ", 2)[0]
		got := memorypolicy.HTTP(method, concretePath(registered))
		if got != want {
			t.Fatalf("route %q: policy=%s want=%s", registered, got, want)
		}
	}
	for registered := range httpPolicyMatrix {
		if !seen[registered] {
			t.Fatalf("stale policy entry %q: route is no longer registered", registered)
		}
	}
}

// TestHTTPPolicyStaysFailClosed pins the default for unregistered mutations.
func TestHTTPPolicyStaysFailClosed(t *testing.T) {
	for _, item := range []struct{ method, path string }{
		{"POST", "/observations"},
		{"PUT", "/sessions/x"},
		{"PATCH", "/anything"},
		{"DELETE", "/sessions/x"},
		{"POST", "/future/endpoint"},
		{"POST", "/sessions//end"},
		{"POST", "/sessions/x/unknown"},
	} {
		if got := memorypolicy.HTTP(item.method, item.path); got != memorypolicy.Deny {
			t.Fatalf("%s %s: %s", item.method, item.path, got)
		}
	}
}
