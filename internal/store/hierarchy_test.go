package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMemoryNodeRebuildPreservesUnchangedRows(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("node-idempotence", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{
		SessionID: "node-idempotence", Type: "decision", Title: "Stable hierarchy",
		Content: "Preserve unchanged hierarchy metadata", Project: "engram",
		MemoryClass: MemoryClassProject, MemoryPath: "project/decisions/backup",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE memory_nodes SET updated_at = '2000-01-01 00:00:00'"); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListMemoryNodes("engram", "project")
	if err != nil || len(before) != 3 {
		t.Fatalf("nodes=%+v err=%v", before, err)
	}
	for _, action := range []struct {
		name string
		run  func() error
	}{
		{"rebuild", s.rebuildAllMemoryNodes},
		{"backup", func() error { _, err := s.CreateSQLiteBackup(); return err }},
		{"migrate", s.migrate},
	} {
		t.Run(action.name, func(t *testing.T) {
			if err := action.run(); err != nil {
				t.Fatal(err)
			}
			after, err := s.ListMemoryNodes("engram", "project")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("unchanged source rewrote nodes: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
	if err := s.DeleteObservation(id, false); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := s.db.QueryRow("SELECT count(*) FROM memory_nodes WHERE deleted_at IS NULL").Scan(&active); err != nil || active != 0 {
		t.Fatalf("removed source left %d active nodes: %v", active, err)
	}
}

func TestNormalizeMemoryPathSafetyAndClassRoot(t *testing.T) {
	valid, err := NormalizeMemoryPath(MemoryClassAgent, "agent/procedures/release")
	if err != nil || valid != "agent/procedures/release" {
		t.Fatalf("valid path=%q err=%v", valid, err)
	}
	for _, value := range []string{
		"/agent/release",
		"agent//release",
		"agent/../release",
		"agent/./release",
		"agent\\release",
		"C:/agent/release",
		"project/release",
		"agent/release\x00secret",
	} {
		if _, err := NormalizeMemoryPath(MemoryClassAgent, value); err == nil {
			t.Errorf("NormalizeMemoryPath(%q) succeeded", value)
		}
	}
	if empty, err := NormalizeMemoryPath(MemoryClassAgent, ""); err != nil || empty != "" {
		t.Fatalf("empty path=%q err=%v", empty, err)
	}
}

func TestObservationHierarchyFallbackAndDeterministicNodes(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("hierarchy", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	id, err := s.AddObservation(AddObservationParams{
		SessionID: "hierarchy", Type: "preference", MemoryClass: MemoryClassUser,
		Title: "Editor preference", Content: "Use a compact editor layout with a persistent sidebar.",
		Project: "engram", Scope: "project", MemoryPath: "user/preferences/editor",
		Abstract: "Compact editor layout", Overview: "User prefers a compact layout with the sidebar visible.",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	obs, err := s.GetObservation(id)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if obs.MemoryPath != "user/preferences/editor" || obs.Abstract != "Compact editor layout" {
		t.Fatalf("observation hierarchy fields=%+v", obs)
	}
	if obs.SummaryState != SummaryStateFresh || obs.SummaryRevision != 1 {
		t.Fatalf("summary state=%q revision=%d", obs.SummaryState, obs.SummaryRevision)
	}

	nodes, err := s.ListMemoryNodes("engram", "project")
	if err != nil {
		t.Fatalf("ListMemoryNodes: %v", err)
	}
	wantPaths := []string{"user", "user/preferences", "user/preferences/editor"}
	if len(nodes) != len(wantPaths) {
		t.Fatalf("nodes=%+v", nodes)
	}
	for index, want := range wantPaths {
		if nodes[index].Path != want || !strings.Contains(nodes[index].Abstract, "Compact editor layout") {
			t.Errorf("node[%d]=%+v want path=%q with aggregate", index, nodes[index], want)
		}
	}

	content := "Use a wide editor layout after the preference changed."
	updated, err := s.UpdateObservation(id, UpdateObservationParams{Content: &content})
	if err != nil {
		t.Fatalf("UpdateObservation: %v", err)
	}
	if updated.SummaryState != SummaryStateStale || updated.SummaryRevision != 1 {
		t.Fatalf("updated summary state=%q revision=%d", updated.SummaryState, updated.SummaryRevision)
	}
	if updated.EffectiveAbstract() != "Compact editor layout" || updated.EffectiveOverview(200) == content {
		t.Fatalf("explicit stale summaries should remain visible with state: %+v", updated)
	}

	if err := s.DeleteObservation(id, false); err != nil {
		t.Fatalf("DeleteObservation: %v", err)
	}
	nodes, err = s.ListMemoryNodes("engram", "project")
	if err != nil {
		t.Fatalf("ListMemoryNodes after delete: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("deleted observation left active nodes: %+v", nodes)
	}
}

func TestObservationHierarchyLegacyFallbackAndRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("hierarchy-roundtrip", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	id, err := s.AddObservation(AddObservationParams{
		SessionID: "hierarchy-roundtrip", Type: "decision", Title: "Legacy compatible title",
		Content: "Legacy compatible content used as the bounded L1 fallback.", Project: "engram",
		MemoryClass: MemoryClassProject, MemoryPath: "project/architecture/store",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	original, err := s.GetObservation(id)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if original.EffectiveAbstract() != original.Title || !strings.HasPrefix(original.EffectiveOverview(24), "Legacy compatible") {
		t.Fatalf("legacy fallback=%q / %q", original.EffectiveAbstract(), original.EffectiveOverview(24))
	}
	if original.SummaryState != SummaryStateMissing || original.SummaryRevision != 0 {
		t.Fatalf("legacy summary state=%q revision=%d", original.SummaryState, original.SummaryRevision)
	}

	exported, err := s.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	dst := newTestStore(t)
	if _, err := dst.Import(exported); err != nil {
		t.Fatalf("Import: %v", err)
	}
	imported, err := dst.GetObservationBySyncID(original.SyncID)
	if err != nil {
		t.Fatalf("GetObservationBySyncID: %v", err)
	}
	if imported.MemoryPath != original.MemoryPath || imported.SummaryState != SummaryStateMissing {
		t.Fatalf("imported hierarchy=%+v", imported)
	}
	nodes, err := dst.ListMemoryNodes("engram", "project")
	if err != nil || len(nodes) != 3 {
		t.Fatalf("imported nodes=%+v err=%v", nodes, err)
	}
}

func TestMemoryPathCannotDriftFromClassOnUpdate(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("hierarchy-class", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddObservation(AddObservationParams{
		SessionID: "hierarchy-class", Type: "decision", Title: "path class", Content: "path class",
		Project: "engram", MemoryClass: MemoryClassProject, MemoryPath: "project/decisions",
	})
	if err != nil {
		t.Fatal(err)
	}
	userClass := MemoryClassUser
	if _, err := s.UpdateObservation(id, UpdateObservationParams{MemoryClass: &userClass}); err == nil {
		t.Fatal("class update succeeded without moving memory_path")
	}
	userPath := "user/preferences"
	if _, err := s.UpdateObservation(id, UpdateObservationParams{MemoryClass: &userClass, MemoryPath: &userPath}); err != nil {
		t.Fatalf("atomic class/path move: %v", err)
	}
}

func TestObservationHierarchyCloudMutationRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("hierarchy-sync", "engram", "/tmp/engram"); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"sync_id": "obs-hierarchy-sync", "session_id": "hierarchy-sync", "type": "procedure",
		"memory_class": MemoryClassAgent, "memory_path": "agent/procedures/sync",
		"abstract": "Replicate hierarchy", "overview": "Hierarchy metadata travels with observation mutations.",
		"summary_state": SummaryStateFresh, "summary_revision": 3,
		"title": "Hierarchy sync", "content": "Full hierarchy sync payload.",
		"project": "engram", "scope": "project", "revision_count": 3, "duplicate_count": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyPulledMutation(DefaultSyncTargetKey, SyncMutation{
		Seq: 1, Entity: SyncEntityObservation, EntityKey: "obs-hierarchy-sync",
		Op: SyncOpUpsert, Payload: string(payload), Project: "engram",
	}); err != nil {
		t.Fatalf("ApplyPulledMutation: %v", err)
	}
	obs, err := s.GetObservationBySyncID("obs-hierarchy-sync")
	if err != nil {
		t.Fatal(err)
	}
	if obs.MemoryPath != "agent/procedures/sync" || obs.Abstract != "Replicate hierarchy" || obs.SummaryRevision != 3 {
		t.Fatalf("synced hierarchy=%+v", obs)
	}
	nodes, err := s.ListMemoryNodes("engram", "project")
	if err != nil || len(nodes) != 3 {
		t.Fatalf("synced nodes=%+v err=%v", nodes, err)
	}
}
