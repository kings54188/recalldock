package mcp

import (
	"context"
	"strings"
	"testing"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
	"recalldock.local/recalldock/internal/store"
)

func TestPublicReviewUsesTrustedProjectFilter(t *testing.T) {
	s := newMCPTestStore(t)
	for _, project := range []string{"review-a", "review-b"} {
		if err := s.CreateSession(project, project, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		source, err := s.AddObservation(store.AddObservationParams{SessionID: project, Project: project, Type: "decision", Title: project + " source", Content: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		dependent, err := s.AddObservation(store.AddObservationParams{SessionID: project, Project: project, Type: "decision", Title: project + " dependent", Content: "derived"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddMemoryDependency(store.AddMemoryDependencyParams{SourceObservationID: source, DependentObservationID: dependent}); err != nil {
			t.Fatal(err)
		}
		content := "v2"
		if _, err := s.UpdateObservation(source, store.UpdateObservationParams{Content: &content}); err != nil {
			t.Fatal(err)
		}
	}
	srv := NewServerWithConfig(s, MCPConfig{DefaultProject: "review-a"}, map[string]bool{"mem_review": true})
	r := mcpToolText(t, srv, "mem_review", map[string]any{"action": "list", "status": "stale"})
	text := callResultText(t, r)
	if r.IsError || strings.Contains(text, "review-b") || !strings.Contains(text, "review-a dependent") {
		t.Fatalf("review escaped trusted project: %s", text)
	}
	r = mcpToolText(t, srv, "mem_review", map[string]any{"action": "list", "status": "active"})
	if r.IsError || strings.Contains(callResultText(t, r), "review-a dependent") {
		t.Fatalf("active status ignored: %s", callResultText(t, r))
	}
}

func TestHandleReviewDependencyWorkflowRequiresExplicitRefresh(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("dependency-review", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	sourceID, err := s.AddObservation(store.AddObservationParams{SessionID: "dependency-review", Type: "decision", Title: "Source", Content: "v1", Project: "engram"})
	if err != nil {
		t.Fatalf("add source: %v", err)
	}
	dependentID, err := s.AddObservation(store.AddObservationParams{SessionID: "dependency-review", Type: "architecture", Title: "Derived", Content: "derived from v1", Project: "engram"})
	if err != nil {
		t.Fatalf("add dependent: %v", err)
	}

	h := handleReview(s, MCPConfig{})
	link, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "link_dependency", "source_observation_id": float64(sourceID),
		"dependent_observation_id": float64(dependentID), "reason": "architecture derives from decision",
	}}})
	if err != nil || link.IsError {
		t.Fatalf("link dependency: err=%v text=%q", err, callResultText(t, link))
	}
	linkBody := callResultJSON(t, link)
	if linkBody["status"] != store.DependencyStatusActive || linkBody["source_revision"] != float64(1) {
		t.Fatalf("unexpected dependency link response: %+v", linkBody)
	}

	sourceContent := "v2"
	if _, err := s.UpdateObservation(sourceID, store.UpdateObservationParams{Content: &sourceContent}); err != nil {
		t.Fatalf("update source: %v", err)
	}
	list, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "list", "project": "engram", "limit": 10.0,
	}}})
	if err != nil || list.IsError {
		t.Fatalf("list review: err=%v text=%q", err, callResultText(t, list))
	}
	listBody := callResultJSON(t, list)
	observations, ok := listBody["observations"].([]any)
	if !ok || len(observations) != 1 {
		t.Fatalf("review observations=%v", listBody["observations"])
	}
	entry := observations[0].(map[string]any)
	if !strings.Contains(entry["stale_reason"].(string), "dependency:") {
		t.Fatalf("missing dependency stale reason: %+v", entry)
	}
	if dependencies, ok := entry["dependencies"].([]any); !ok || len(dependencies) != 1 {
		t.Fatalf("missing incoming dependency trace: %+v", entry)
	}
	search, err := handleSearch(s, MCPConfig{DefaultProject: "engram"}, NewSessionActivity(0))(
		context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
			"query": "derived", "include_stale": true,
		}}},
	)
	if err != nil || search.IsError {
		t.Fatalf("search stale: err=%v text=%q", err, callResultText(t, search))
	}
	searchResults := callResultJSON(t, search)["results"].([]any)
	if len(searchResults) != 1 || !strings.Contains(searchResults[0].(map[string]any)["stale_reason"].(string), "dependency:") {
		t.Fatalf("search did not expose stale reason: %+v", searchResults)
	}
	get, err := handleGetObservation(s, MCPConfig{DefaultProject: "engram"})(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"id": float64(dependentID)}}})
	if err != nil || get.IsError {
		t.Fatalf("get stale: err=%v text=%q", err, callResultText(t, get))
	}
	getBody := callResultJSON(t, get)
	if getBody["state"] != store.ObservationStateNeedsReview || !strings.Contains(getBody["stale_reason"].(string), "dependency:") {
		t.Fatalf("get did not expose stale metadata: %+v", getBody)
	}

	marked, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "mark_reviewed", "observation_id": float64(dependentID),
	}}})
	if err != nil || marked.IsError {
		t.Fatalf("mark reviewed: err=%v text=%q", err, callResultText(t, marked))
	}
	if got := callResultJSON(t, marked)["state"]; got != store.ObservationStateNeedsReview {
		t.Fatalf("mark_reviewed must not clear dependency stale state, got %v", got)
	}

	dependentContent := "refreshed from v2"
	if _, err := s.UpdateObservation(dependentID, store.UpdateObservationParams{Content: &dependentContent}); err != nil {
		t.Fatalf("refresh dependent: %v", err)
	}
	deps, err := s.ListMemoryDependencies(store.MemoryDependencyListOptions{DependentObservationSyncID: entry["sync_id"].(string)})
	if err != nil || len(deps) != 1 || deps[0].Status != store.DependencyStatusActive || deps[0].SourceRevision != 2 {
		t.Fatalf("dependency was not rebound after explicit refresh: deps=%+v err=%v", deps, err)
	}
}

func TestHandleReviewListsDependenciesAndProcessesQueue(t *testing.T) {
	s := newMCPTestStore(t)
	h := handleReview(s, MCPConfig{})
	list, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "list_dependencies", "status": store.DependencyStatusStale,
	}}})
	if err != nil || list.IsError {
		t.Fatalf("list dependencies: err=%v text=%q", err, callResultText(t, list))
	}
	if deps, ok := callResultJSON(t, list)["dependencies"].([]any); !ok || len(deps) != 0 {
		t.Fatalf("expected empty dependencies, got %v", deps)
	}
	processed, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "process_invalidations", "limit": 25.0,
	}}})
	if err != nil || processed.IsError {
		t.Fatalf("process invalidations: err=%v text=%q", err, callResultText(t, processed))
	}
	body := callResultJSON(t, processed)
	if body["processed"] != float64(0) || body["remaining"] != float64(0) {
		t.Fatalf("unexpected queue result: %+v", body)
	}
}

func TestHandleReviewResourceDependencyWorkflowRequiresExplicitRefresh(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("resource-review", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	dependentID, err := s.AddObservation(store.AddObservationParams{
		SessionID: "resource-review", Type: "architecture", Title: "Indexed contract",
		Content: "derived from pinned source", Project: "engram",
	})
	if err != nil {
		t.Fatalf("add dependent: %v", err)
	}

	oldRef := "git:0123456789abcdef0123456789abcdef01234567:docs/contract.md#L1-L20@sha256:" + strings.Repeat("a", 64)
	newRef := "git:89abcdef0123456789abcdef0123456789abcdef:docs/contract.md#L1-L22@sha256:" + strings.Repeat("b", 64)
	h := handleReview(s, MCPConfig{})

	missingProject, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "link_resource", "source_ref": oldRef, "dependent_observation_id": float64(dependentID),
	}}})
	if err != nil || !missingProject.IsError || !strings.Contains(callResultText(t, missingProject), "project is required") {
		t.Fatalf("missing project result: err=%v text=%q", err, callResultText(t, missingProject))
	}

	linked, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "link_resource", "project": "engram", "source_ref": oldRef,
		"dependent_observation_id": float64(dependentID), "reason": "contract source",
	}}})
	if err != nil || linked.IsError {
		t.Fatalf("link resource: err=%v text=%q", err, callResultText(t, linked))
	}
	linkedBody := callResultJSON(t, linked)
	dependency := linkedBody["resource_dependency"].(map[string]any)
	dependencySyncID := dependency["sync_id"].(string)
	if dependency["source_ref"] != oldRef || dependency["status"] != store.DependencyStatusActive {
		t.Fatalf("unexpected linked dependency: %+v", dependency)
	}

	changed, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "notify_resource_changed", "project": "engram", "source_ref": oldRef,
		"replacement_source_ref": newRef, "reason": "pinned source changed",
	}}})
	if err != nil || changed.IsError {
		t.Fatalf("notify resource changed: err=%v text=%q", err, callResultText(t, changed))
	}
	if body := callResultJSON(t, changed); body["matched"] != float64(1) || body["staled"] != float64(1) {
		t.Fatalf("unexpected change result: %+v", body)
	}

	reviewed, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "list", "project": "engram",
	}}})
	if err != nil || reviewed.IsError {
		t.Fatalf("list resource review: err=%v text=%q", err, callResultText(t, reviewed))
	}
	entries := callResultJSON(t, reviewed)["observations"].([]any)
	if len(entries) != 1 {
		t.Fatalf("review entries=%+v", entries)
	}
	entry := entries[0].(map[string]any)
	if !strings.Contains(entry["stale_reason"].(string), "resource:") {
		t.Fatalf("missing resource stale reason: %+v", entry)
	}
	if resourceDependencies, ok := entry["resource_dependencies"].([]any); !ok || len(resourceDependencies) != 1 {
		t.Fatalf("missing resource dependency trace: %+v", entry)
	}

	listed, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "list_resource_dependencies", "project": "engram", "status": store.DependencyStatusStale,
	}}})
	if err != nil || listed.IsError {
		t.Fatalf("list resource dependencies: err=%v text=%q", err, callResultText(t, listed))
	}
	if dependencies := callResultJSON(t, listed)["resource_dependencies"].([]any); len(dependencies) != 1 {
		t.Fatalf("resource dependencies=%+v", dependencies)
	}

	refreshed, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "refresh_resource_dependency", "project": "engram",
		"resource_dependency_sync_id": dependencySyncID, "reason": "memory checked against replacement",
	}}})
	if err != nil || refreshed.IsError {
		t.Fatalf("refresh resource dependency: err=%v text=%q", err, callResultText(t, refreshed))
	}
	refreshedDependency := callResultJSON(t, refreshed)["resource_dependency"].(map[string]any)
	if refreshedDependency["source_ref"] != newRef || refreshedDependency["status"] != store.DependencyStatusActive {
		t.Fatalf("unexpected refreshed dependency: %+v", refreshedDependency)
	}
	observation, err := s.GetObservation(dependentID)
	if err != nil || observation.StaleAt != nil || observation.StaleReason != nil {
		t.Fatalf("observation stale state not cleared after explicit refresh: obs=%+v err=%v", observation, err)
	}
}
