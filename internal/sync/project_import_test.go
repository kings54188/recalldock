package sync

import (
	"encoding/json"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func scopedImportStore(t *testing.T) *store.Store {
	t.Helper()
	s := newTestStore(t)
	for _, project := range []string{"proj-a", "proj-b"} {
		if err := s.CreateSession("existing-"+project, project, "/synthetic/"+project); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func scopedImportTransport(t *testing.T) *fakeCloudTransport {
	t.Helper()
	tr := newFakeCloudTransport()
	chunk := ChunkData{}
	for _, project := range []string{"proj-a", "proj-b"} {
		project := project
		chunk.Sessions = append(chunk.Sessions, store.Session{ID: project + "-session", Project: project, Directory: "/synthetic/" + project, StartedAt: "2026-01-01 00:00:00"})
		chunk.Observations = append(chunk.Observations, store.Observation{ID: int64(len(chunk.Observations) + 1), SyncID: project + "-obs", SessionID: project + "-session", Project: &project, Type: "decision", Title: project, Content: project + " content", Scope: "project", RevisionCount: 1, CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00"})
		chunk.Prompts = append(chunk.Prompts, store.Prompt{ID: int64(len(chunk.Prompts) + 1), SyncID: project + "-prompt", SessionID: project + "-session", Project: project, Content: project + " prompt", CreatedAt: "2026-01-01 00:00:00"})
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	tr.manifest.Chunks = []ChunkEntry{{ID: "mixed"}}
	tr.chunks["mixed"] = raw
	return tr
}

func TestImportProjectMixedChunkAndIndependentTracking(t *testing.T) {
	s := scopedImportStore(t)
	sy := NewWithTransport(s, scopedImportTransport(t))
	result, err := sy.ImportProject("PROJ-A")
	if err != nil || result.ChunksImported != 1 || result.SessionsImported != 1 || result.ObservationsImported != 1 || result.PromptsImported != 1 {
		t.Fatalf("project import: %+v, %v", result, err)
	}
	if _, err := s.GetSession("proj-b-session"); err == nil {
		t.Fatal("foreign session imported")
	}
	if _, err := s.GetObservationBySyncID("proj-b-obs"); err == nil {
		t.Fatal("foreign observation imported")
	}
	global, err := s.GetSyncedChunksForTarget(store.LocalChunkTargetKey)
	if err != nil || global["mixed"] {
		t.Fatalf("partial chunk marked globally imported: %+v, %v", global, err)
	}
	again, err := sy.ImportProject("proj-a")
	if err != nil || again.ChunksImported != 0 || again.ChunksSkipped != 1 {
		t.Fatalf("project import not idempotent: %+v, %v", again, err)
	}
	second, err := sy.ImportProject("proj-b")
	if err != nil || second.ObservationsImported != 1 {
		t.Fatalf("second project could not import shared chunk: %+v, %v", second, err)
	}
	all, err := sy.Import()
	if err != nil || all.ChunksImported != 1 {
		t.Fatalf("global import blocked by partial project imports: %+v, %v", all, err)
	}
}

func TestImportProjectUsesScopedApplyAndRejectsForeignIdentity(t *testing.T) {
	s := scopedImportStore(t)
	if err := s.CreateSession("foreign", "proj-b", "/synthetic/b"); err != nil {
		t.Fatal(err)
	}
	tr := newFakeCloudTransport()
	payload, err := json.Marshal(ChunkData{Sessions: []store.Session{{ID: "foreign", Project: "proj-a", Directory: "/synthetic/a", StartedAt: "2026-01-01 00:00:00"}}})
	if err != nil {
		t.Fatal(err)
	}
	tr.manifest.Chunks = []ChunkEntry{{ID: "collision"}}
	tr.chunks["collision"] = payload
	if _, err := NewWithTransport(s, tr).ImportProject("proj-a"); err == nil {
		t.Fatal("project import changed foreign session identity")
	}
	session, err := s.GetSession("foreign")
	if err != nil || session.Project != "proj-b" {
		t.Fatalf("foreign session changed: %+v, %v", session, err)
	}
}

func TestImportProjectFiltersExplicitMutationAndClosesSessionDependency(t *testing.T) {
	s := scopedImportStore(t)
	tr := newFakeCloudTransport()
	for _, entry := range []struct {
		id    string
		chunk ChunkData
	}{
		{"observation-first", ChunkData{Mutations: []store.SyncMutation{
			{Entity: store.SyncEntityObservation, EntityKey: "oa", Op: store.SyncOpUpsert, Project: "proj-a", Payload: `{"sync_id":"oa","session_id":"sa","project":"proj-a","type":"decision","title":"a","content":"a","scope":"project","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}`},
			{Entity: store.SyncEntityObservation, EntityKey: "ob", Op: store.SyncOpUpsert, Project: "proj-b", Payload: `{"sync_id":"ob","session_id":"sb","project":"proj-b","type":"decision","title":"b","content":"b","scope":"project"}`},
		}}},
		{"sessions-last", ChunkData{Sessions: []store.Session{
			{ID: "sa", Project: "proj-a", Directory: "/synthetic/a", StartedAt: "2026-01-01 00:00:00"},
			{ID: "sb", Project: "proj-b", Directory: "/synthetic/b", StartedAt: "2026-01-01 00:00:00"},
		}}},
	} {
		raw, err := json.Marshal(entry.chunk)
		if err != nil {
			t.Fatal(err)
		}
		tr.manifest.Chunks = append(tr.manifest.Chunks, ChunkEntry{ID: entry.id})
		tr.chunks[entry.id] = raw
	}
	result, err := NewWithTransport(s, tr).ImportProject("proj-a")
	if err != nil || result.ObservationsImported != 1 || result.SessionsImported != 1 {
		t.Fatalf("dependency-safe project import failed: %+v, %v", result, err)
	}
	if _, err := s.GetObservationBySyncID("ob"); err == nil {
		t.Fatal("foreign mutation imported")
	}
}

func TestImportProjectInfersLegacyOwnershipAcrossChunks(t *testing.T) {
	s := scopedImportStore(t)
	tr := newFakeCloudTransport()
	chunks := []ChunkData{
		{Observations: []store.Observation{{SyncID: "legacy-a", SessionID: "later-a", Type: "decision", Title: "a", Content: "a", Scope: "project", CreatedAt: "2026-01-01 00:00:00", UpdatedAt: "2026-01-01 00:00:00"}}},
		{Sessions: []store.Session{{ID: "later-a", Project: "proj-a", Directory: "/synthetic/a", StartedAt: "2026-01-01 00:00:00"}}},
	}
	for i, chunk := range chunks {
		id := []string{"child-first", "session-last"}[i]
		raw, err := json.Marshal(chunk)
		if err != nil {
			t.Fatal(err)
		}
		tr.manifest.Chunks = append(tr.manifest.Chunks, ChunkEntry{ID: id})
		tr.chunks[id] = raw
	}
	result, err := NewWithTransport(s, tr).ImportProject("proj-a")
	if err != nil || result.ObservationsImported != 1 || result.SessionsImported != 1 {
		t.Fatalf("legacy ownership inference lost selected data: %+v, %v", result, err)
	}
}

func TestImportProjectRejectsForeignSessionDependencyBeforeRecovery(t *testing.T) {
	s := scopedImportStore(t)
	tr := newFakeCloudTransport()
	project := "proj-a"
	raw, err := json.Marshal(ChunkData{
		Sessions:     []store.Session{{ID: "foreign-new", Project: "proj-b", Directory: "/synthetic/b", StartedAt: "2026-01-01 00:00:00"}},
		Observations: []store.Observation{{SyncID: "cross-owner", SessionID: "foreign-new", Project: &project, Type: "decision", Title: "a", Content: "a", Scope: "project"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tr.manifest.Chunks = []ChunkEntry{{ID: "cross-owner"}}
	tr.chunks["cross-owner"] = raw
	if _, err := NewWithTransport(s, tr).ImportProject("proj-a"); err == nil {
		t.Fatal("scoped import fabricated a local session for a known foreign dependency")
	}
	if _, err := s.GetSession("foreign-new"); err == nil {
		t.Fatal("foreign session was imported or recovered under the wrong project")
	}
}

func TestImportProjectRejectsNullMutationPayload(t *testing.T) {
	tr := newFakeCloudTransport()
	raw, err := json.Marshal(ChunkData{Mutations: []store.SyncMutation{{Entity: store.SyncEntityObservation, EntityKey: "null", Op: store.SyncOpUpsert, Project: "proj-a", Payload: "null"}}})
	if err != nil {
		t.Fatal(err)
	}
	tr.manifest.Chunks = []ChunkEntry{{ID: "null"}}
	tr.chunks["null"] = raw
	if _, err := NewWithTransport(scopedImportStore(t), tr).ImportProject("proj-a"); err == nil {
		t.Fatal("null mutation payload accepted")
	}
}

func TestImportProjectRejectsEmptyProject(t *testing.T) {
	if _, err := NewWithTransport(newTestStore(t), newFakeCloudTransport()).ImportProject(" "); err == nil {
		t.Fatal("empty project must not mean all projects")
	}
}
