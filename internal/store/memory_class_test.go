package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMemoryClassMigrationBackfillsDeterministicallyAndOnlyOnce(t *testing.T) {
	fixtures := []legacyObsRow{
		{syncID: "obs-class-operational", obsType: "checkpoint", title: "checkpoint", content: "phase2 class migration", project: "engram", scope: "project"},
		{syncID: "obs-class-user", obsType: "preference", title: "preference", content: "phase2 class migration", project: "engram", scope: "project"},
		{syncID: "obs-class-agent", obsType: "learning", title: "learning", content: "phase2 class migration", project: "engram", scope: "project"},
		{syncID: "obs-class-project", obsType: "decision", title: "decision", content: "phase2 class migration", project: "engram", scope: "project"},
		{syncID: "obs-class-unknown", obsType: "custom_unknown_type", title: "unknown", content: "phase2 class migration", project: "engram", scope: "project"},
	}
	s := newTestStoreWithLegacySchema(t, fixtures)

	want := map[string]string{
		"obs-class-operational": MemoryClassOperational,
		"obs-class-user":        MemoryClassUser,
		"obs-class-agent":       MemoryClassAgent,
		"obs-class-project":     MemoryClassProject,
		"obs-class-unknown":     MemoryClassProject,
	}
	var agentID int64
	for syncID, wantClass := range want {
		obs, err := s.GetObservationBySyncID(syncID)
		if err != nil {
			t.Fatalf("GetObservationBySyncID(%q): %v", syncID, err)
		}
		if obs.MemoryClass != wantClass {
			t.Errorf("%s memory_class=%q, want %q", syncID, obs.MemoryClass, wantClass)
		}
		if syncID == "obs-class-agent" {
			agentID = obs.ID
		}
	}

	projectClass := MemoryClassProject
	if _, err := s.UpdateObservation(agentID, UpdateObservationParams{MemoryClass: &projectClass}); err != nil {
		t.Fatalf("set explicit class: %v", err)
	}
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	obs, err := s.GetObservation(agentID)
	if err != nil {
		t.Fatalf("GetObservation after second migrate: %v", err)
	}
	if obs.MemoryClass != MemoryClassProject {
		t.Fatalf("second migration rewrote explicit class to %q", obs.MemoryClass)
	}
}

func TestMemoryClassValidationAndTypeDefaults(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("memory-class-validation", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	tests := []struct {
		name          string
		typeName      string
		explicitClass string
		wantClass     string
	}{
		{name: "operational inferred", typeName: "session_summary", wantClass: MemoryClassOperational},
		{name: "user inferred", typeName: "user_fact", wantClass: MemoryClassUser},
		{name: "agent inferred", typeName: "procedure", wantClass: MemoryClassAgent},
		{name: "project inferred", typeName: "architecture", wantClass: MemoryClassProject},
		{name: "explicit class independent of type", typeName: "decision", explicitClass: MemoryClassUser, wantClass: MemoryClassUser},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := s.AddObservation(AddObservationParams{
				SessionID:   "memory-class-validation",
				Type:        tc.typeName,
				Title:       tc.name,
				Content:     "memory class validation " + tc.name,
				Project:     "engram",
				MemoryClass: tc.explicitClass,
			})
			if err != nil {
				t.Fatalf("AddObservation: %v", err)
			}
			obs, err := s.GetObservation(id)
			if err != nil {
				t.Fatalf("GetObservation: %v", err)
			}
			if obs.MemoryClass != tc.wantClass {
				t.Fatalf("memory_class=%q, want %q", obs.MemoryClass, tc.wantClass)
			}
		})
	}

	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "memory-class-validation", Type: "manual", Title: "invalid", Content: "invalid class", Project: "engram", MemoryClass: "resource",
	}); err == nil || !strings.Contains(err.Error(), "invalid memory_class") {
		t.Fatalf("invalid AddObservation class error=%v", err)
	}

	id, err := s.AddObservation(AddObservationParams{
		SessionID: "memory-class-validation", Type: "manual", Title: "update class", Content: "update class", Project: "engram",
	})
	if err != nil {
		t.Fatalf("seed update observation: %v", err)
	}
	agentClass := MemoryClassAgent
	updated, err := s.UpdateObservation(id, UpdateObservationParams{MemoryClass: &agentClass})
	if err != nil {
		t.Fatalf("UpdateObservation class: %v", err)
	}
	if updated.MemoryClass != MemoryClassAgent {
		t.Fatalf("updated memory_class=%q", updated.MemoryClass)
	}
	invalid := "resource"
	if _, err := s.UpdateObservation(id, UpdateObservationParams{MemoryClass: &invalid}); err == nil || !strings.Contains(err.Error(), "invalid memory_class") {
		t.Fatalf("invalid UpdateObservation class error=%v", err)
	}
}

func TestSearchMemoryProfilesPreserveLegacyDefault(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("memory-profile", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	classes := []string{MemoryClassOperational, MemoryClassUser, MemoryClassAgent, MemoryClassProject}
	for _, class := range classes {
		if _, err := s.AddObservation(AddObservationParams{
			SessionID: "memory-profile", Type: "manual", Title: "phase2-shared " + class,
			Content: "phase2-shared profile result " + class, Project: "engram", MemoryClass: class,
		}); err != nil {
			t.Fatalf("AddObservation(%s): %v", class, err)
		}
	}

	assertClasses := func(t *testing.T, profile string, want ...string) {
		t.Helper()
		results, err := s.Search("phase2-shared", SearchOptions{Project: "engram", Limit: 10, Profile: profile})
		if err != nil {
			t.Fatalf("Search(profile=%q): %v", profile, err)
		}
		got := map[string]bool{}
		for _, result := range results {
			got[result.MemoryClass] = true
		}
		if len(results) != len(want) {
			t.Fatalf("Search(profile=%q) returned %d results/classes=%v, want %d", profile, len(results), got, len(want))
		}
		for _, class := range want {
			if !got[class] {
				t.Errorf("Search(profile=%q) missing class %q; got %v", profile, class, got)
			}
		}
	}

	assertClasses(t, "", classes...)
	assertClasses(t, SearchProfileAll, classes...)
	assertClasses(t, SearchProfileSemantic, MemoryClassUser, MemoryClassAgent, MemoryClassProject)
	assertClasses(t, SearchProfileOperational, MemoryClassOperational)
	if _, err := s.Search("phase2-shared", SearchOptions{Profile: "invalid"}); err == nil || !strings.Contains(err.Error(), "invalid profile") {
		t.Fatalf("invalid profile error=%v", err)
	}

	if _, err := s.AddObservation(AddObservationParams{
		SessionID: "memory-profile", Type: "checkpoint", Title: "exact lane", Content: "exact lane topic",
		Project: "engram", TopicKey: "checkpoint/lane", MemoryClass: MemoryClassOperational,
	}); err != nil {
		t.Fatalf("AddObservation(topic): %v", err)
	}
	results, err := s.Search("checkpoint/lane", SearchOptions{Project: "engram", Profile: SearchProfileSemantic})
	if err != nil {
		t.Fatalf("semantic topic search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("semantic topic search leaked operational result: %+v", results)
	}
}

func TestMemoryClassExportImportAndSyncRoundTrip(t *testing.T) {
	src := newTestStore(t)
	if err := src.CreateSession("memory-class-roundtrip", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	id, err := src.AddObservation(AddObservationParams{
		SessionID: "memory-class-roundtrip", Type: "decision", Title: "roundtrip", Content: "memory class roundtrip",
		Project: "engram", MemoryClass: MemoryClassUser,
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	original, err := src.GetObservation(id)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}

	exported, err := src.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	dstImport := newTestStore(t)
	if _, err := dstImport.Import(exported); err != nil {
		t.Fatalf("Import: %v", err)
	}
	imported, err := dstImport.GetObservationBySyncID(original.SyncID)
	if err != nil {
		t.Fatalf("GetObservationBySyncID after import: %v", err)
	}
	if imported.MemoryClass != MemoryClassUser {
		t.Fatalf("imported memory_class=%q", imported.MemoryClass)
	}

	if err := src.EnrollProject("engram"); err != nil {
		t.Fatalf("EnrollProject: %v", err)
	}
	mutations, err := src.ListPendingSyncMutations(DefaultSyncTargetKey, 100)
	if err != nil {
		t.Fatalf("ListPendingSyncMutations: %v", err)
	}
	dstSync := newTestStore(t)
	if err := dstSync.ApplyPulledChunk("phase2-test", "memory-class", mutations); err != nil {
		t.Fatalf("ApplyPulledChunk: %v", err)
	}
	synced, err := dstSync.GetObservationBySyncID(original.SyncID)
	if err != nil {
		t.Fatalf("GetObservationBySyncID after sync: %v", err)
	}
	if synced.MemoryClass != MemoryClassUser {
		t.Fatalf("synced memory_class=%q", synced.MemoryClass)
	}

	legacyPayload := map[string]any{
		"sync_id": original.SyncID, "session_id": original.SessionID, "type": "learning", "title": "legacy peer update",
		"content": original.Content, "project": original.Project, "scope": original.Scope, "revision_count": original.RevisionCount + 1,
	}
	payloadJSON, err := json.Marshal(legacyPayload)
	if err != nil {
		t.Fatalf("marshal legacy payload: %v", err)
	}
	if err := dstSync.ApplyPulledChunk("phase2-test", "legacy-memory-class", []SyncMutation{{
		Entity: SyncEntityObservation, EntityKey: original.SyncID, Op: SyncOpUpsert, Payload: string(payloadJSON),
	}}); err != nil {
		t.Fatalf("ApplyPulledChunk legacy payload: %v", err)
	}
	legacyUpdated, err := dstSync.GetObservationBySyncID(original.SyncID)
	if err != nil {
		t.Fatalf("GetObservationBySyncID after legacy sync: %v", err)
	}
	if legacyUpdated.MemoryClass != MemoryClassUser {
		t.Fatalf("legacy payload overwrote explicit memory_class with %q", legacyUpdated.MemoryClass)
	}
}
