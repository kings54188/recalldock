package store

import (
	"strings"
	"testing"
)

func TestProjectMergeMovesCurrentAssociationsAndExpiresOldActivation(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	const source = "legacy-source"
	if err := s.CreateSession("legacy-session", source, root); err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, name := range []string{"source", "derived"} {
		id, err := s.AddObservation(AddObservationParams{SessionID: "legacy-session", Project: source, Type: "decision", Title: name, Content: "durable migration " + name, MemoryPath: "project/design/" + name})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := s.AddMemoryDependency(AddMemoryDependencyParams{SourceObservationID: ids[0], DependentObservationID: ids[1], Reason: "Derived fixture"}); err != nil {
		t.Fatal(err)
	}
	dependent, _ := s.GetObservation(ids[1])
	if _, err := s.db.Exec("INSERT INTO resource_dependencies(sync_id,source_ref,dependent_observation_sync_id,project) VALUES(?,?,?,?)", "resource-merge", "file:///synthetic/source@sha256:"+strings.Repeat("a", 64), dependent.SyncID, source); err != nil {
		t.Fatal(err)
	}
	result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{SessionID: "legacy-session", Project: source, Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "Use explicit stable project identities for this synthetic workspace", Confidence: 0.8}}})
	if err != nil || len(result.CandidateIDs) != 1 {
		t.Fatalf("seed candidate: %+v %v", result, err)
	}
	budget, err := s.CreateReadBudget(source, 8000)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.CreateRegisteredProject("Unified identity")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(target.ID, root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeProjects([]string{source}, target.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"observations", "sessions", "memory_candidates", "memory_dependencies", "resource_dependencies"} {
		var old int
		if err := s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE project=?", source).Scan(&old); err != nil || old != 0 {
			t.Fatalf("%s stranded old key: %d %v", table, old, err)
		}
	}
	nodes, err := s.ListMemoryNodes(target.ID, "project")
	if err != nil || len(nodes) == 0 {
		t.Fatal("target hierarchy not rebuilt")
	}
	oldNodes, err := s.ListMemoryNodes(source, "project")
	if err != nil || len(oldNodes) != 0 {
		t.Fatal("source hierarchy still active")
	}
	if _, err := s.ConsumeReadBudget(budget.ID, source, 1, ""); err == nil {
		t.Fatal("old activation survived project migration")
	}
	var historicalAudit, newAudit int
	if err := s.db.QueryRow("SELECT count(*) FROM memory_candidate_audit WHERE project=?", source).Scan(&historicalAudit); err != nil || historicalAudit == 0 {
		t.Fatal("historical audit silently rewritten")
	}
	if err := s.db.QueryRow("SELECT count(*) FROM memory_candidate_audit WHERE project=? AND action='project_migrated'", target.ID).Scan(&newAudit); err != nil || newAudit != 1 {
		t.Fatal("migration audit missing")
	}
	first, err := s.ObservationVersion(ids[0], 1, false)
	if err != nil || derefString(first.State.Project) != source {
		t.Fatal("historical knowledge ownership rewritten")
	}
}

func TestProjectMergeDoesNotGuessSeparatorAliases(t *testing.T) {
	s := newTestStore(t)
	for _, p := range []string{"same-name", "same_name"} {
		if err := s.CreateSession(p, p, t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.MergeProjects([]string{"same-name"}, "target"); err != nil {
		t.Fatal(err)
	}
	untouched, err := s.GetSession("same_name")
	if err != nil || untouched.Project != "same_name" {
		t.Fatal("unselected similar project was merged")
	}
}

func TestProjectMergeProtectsBoundRegisteredIdentity(t *testing.T) {
	s := newTestStore(t)
	source, err := s.CreateRegisteredProject("Source")
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.CreateRegisteredProject("Target")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(source.ID, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeProjects([]string{source.ID}, target.ID); err == nil {
		t.Fatal("source binding silently detached from identity")
	}
	if _, err := s.MergeProjects([]string{"legacy"}, "prj-"+strings.Repeat("0", 32)); err == nil {
		t.Fatal("unknown target ID became a data bucket")
	}
}

func TestMappingPreviewRejectsInvalidIdentityAndPreservesRawSource(t *testing.T) {
	s := newTestStore(t)
	identity, err := s.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sessions(id,project,directory) VALUES('raw-source','Legacy Mixed','/synthetic')`); err != nil {
		t.Fatal(err)
	}
	mappings := []ProjectMapping{{Source: "Legacy Mixed", Target: "target"}}
	normalized, err := s.ValidateProjectMappings(identity.StoreID, mappings)
	if err != nil || normalized[0].Source != "Legacy Mixed" {
		t.Fatalf("preview discarded exact raw key: %+v %v", normalized, err)
	}
	if _, err := s.ApplyProjectMappings(identity.StoreID, mappings); err != nil {
		t.Fatal(err)
	}
	session, err := s.GetSession("raw-source")
	if err != nil || session.Project != "target" {
		t.Fatal("raw source stranded")
	}
	if _, err := s.ValidateProjectMappings(identity.StoreID, []ProjectMapping{{Source: "target", Target: "prj-" + strings.Repeat("0", 32)}}); err == nil {
		t.Fatal("preview marked unknown registered target valid")
	}
	p, _ := s.CreateRegisteredProject("Bound source")
	if _, err := s.BindProjectRoot(p.ID, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidateProjectMappings(identity.StoreID, []ProjectMapping{{Source: p.ID, Target: "target"}}); err == nil {
		t.Fatal("preview accepted active source binding")
	}
}
