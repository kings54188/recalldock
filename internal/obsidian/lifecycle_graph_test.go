package obsidian

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestArchiveProjectionLabelsLifecycleWithoutLosingBody(t *testing.T) {
	stamp := "2026-11-01T00:00:00Z"
	note := ObservationToMarkdown(store.Observation{ID: 1, Title: "Archive fixture", Content: "Preserved body", ArchivedAt: &stamp})
	if !strings.Contains(note, "lifecycle_state: archived") || !strings.Contains(note, "archived_at:") || !strings.Contains(note, "Preserved body") || !strings.Contains(note, "> [!warning] Archived") {
		t.Fatal("archived projection looks active", note)
	}
}

type graphStore struct {
	*mockStore
	relations []store.SyncMutation
}

func (g *graphStore) ExportRelationMutations(string) ([]store.SyncMutation, error) {
	return g.relations, nil
}

func TestExportTypedKnowledgeEdgesAreScopedAndResolve(t *testing.T) {
	p, q := "p", "other"
	obs := []store.Observation{{ID: 1, SyncID: "obs-one", Project: &p, Type: "decision", Title: "One", Content: "body"}, {ID: 2, SyncID: "obs-two", Project: &p, Type: "decision", Title: "Two", Content: "body"}, {ID: 3, SyncID: "obs-other", Project: &q, Type: "decision", Title: "Other", Content: "foreign"}}
	ms := &graphStore{mockStore: &mockStore{exportData: &store.ExportData{Observations: obs, Dependencies: []store.MemoryDependency{{SourceObservationSyncID: "obs-one", DependentObservationSyncID: "obs-two", Status: "active"}}}, stats: &store.Stats{}}, relations: []store.SyncMutation{{Payload: `{"source_id":"obs-one","target_id":"obs-two","relation":"supersedes","judgment_status":"judged"}`}, {Payload: `{"source_id":"obs-one","target_id":"obs-other","relation":"related","judgment_status":"judged"}`}, {Payload: `{"source_id":"obs-one","target_id":"obs-two","relation":"conflicts_with","judgment_status":"pending"}`}}}
	vault := t.TempDir()
	e := NewExporter(ms, ExportConfig{VaultPath: vault, Project: p})
	result, err := e.Export()
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("export: %+v %v", result, err)
	}
	note, err := os.ReadFile(filepath.Join(vault, "recalldock/p/decision/memory-obs-one.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"supersedes (judged)", "conflicts_with (pending)", "required_by (active)", "[[recalldock/p/decision/memory-obs-two]]"} {
		if !strings.Contains(string(note), want) {
			t.Fatalf("missing %q: %s", want, note)
		}
	}
	if strings.Contains(string(note), "obs-other") {
		t.Fatal("cross-scope relation leaked")
	}
	// The same note is refreshed when only relationship data changes.
	ms.relations = nil
	result, err = e.Export()
	if err != nil || len(result.Errors) != 0 {
		t.Fatal(err)
	}
	note, _ = os.ReadFile(filepath.Join(vault, "recalldock/p/decision/memory-obs-one.md"))
	if strings.Contains(string(note), "supersedes") {
		t.Fatal("removed relation remains current")
	}
}
