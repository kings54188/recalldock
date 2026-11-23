package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveExistingProjectRejectsUnknownButKeepsKnownEmpty(t *testing.T) {
	s := newTestStore(t)
	p, err := s.CreateRegisteredProject("empty-known")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{p.ID, "empty-known"} {
		got, err := s.ResolveExistingProject(key)
		if err != nil || got != p.ID {
			t.Fatalf("known empty project rejected: %s %v", got, err)
		}
	}
	if _, err := s.ResolveExistingProject("nonexistent_xyz"); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestProjectNameAliasResolvesWithoutFilesystem(t *testing.T) {
	s := newTestStore(t)
	p, err := s.CreateRegisteredProject("xitong")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.CanonicalProjectKey("XiTong")
	if err != nil || got != p.ID {
		t.Fatalf("alias resolution: got %q err %v, want %q", got, err, p.ID)
	}
	// Duplicate display names must not silently select one identity.
	if _, err := s.CreateRegisteredProject("xitong"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CanonicalProjectKey("xitong"); err == nil {
		t.Fatal("ambiguous duplicate name resolved without an explicit ID")
	}
}

func TestCanonicalizeLegacyProjectKeysRepairsHistoricalRows(t *testing.T) {
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = t.TempDir()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("legacy-project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM store_metadata WHERE key=?`, projectKeyCanonicalizationMarker); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sessions(id,project,directory) VALUES('legacy-session','legacy-project','/gone/legacy')`); err != nil {
		t.Fatal(err)
	}
	res, err := s.db.Exec(`INSERT INTO observations(session_id,type,title,content,project) VALUES('legacy-session','decision','legacy title','legacy body','legacy-project')`)
	if err != nil {
		t.Fatal(err)
	}
	obsID, _ := res.LastInsertId()
	if _, err := s.db.Exec(`INSERT INTO memory_candidates(sync_id,session_id,project,category,statement,confidence,status,created_at,expires_at,updated_at)
		VALUES('cand-legacy','legacy-session','legacy-project','decision','legacy statement',0.9,'review',datetime('now'),datetime('now','+7 days'),datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var project string
	if err := s.db.QueryRow(`SELECT project FROM observations WHERE id=?`, obsID).Scan(&project); err != nil || project != p.ID {
		t.Fatalf("observation project = %q err=%v, want %q", project, err, p.ID)
	}
	if err := s.db.QueryRow(`SELECT project FROM sessions WHERE id='legacy-session'`).Scan(&project); err != nil || project != p.ID {
		t.Fatalf("session project = %q err=%v, want %q", project, err, p.ID)
	}
	if err := s.db.QueryRow(`SELECT project FROM memory_candidates WHERE sync_id='cand-legacy'`).Scan(&project); err != nil || project != p.ID {
		t.Fatalf("candidate project = %q err=%v, want %q", project, err, p.ID)
	}
}

func TestCandidatePromotionSurvivesDeletedProjectDirectory(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	p, err := s.CreateRegisteredProject("deleted-project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("deleted-session", p.ID, root); err != nil {
		t.Fatal(err)
	}
	result, err := s.CommitMemoryCandidates(CommitMemoryCandidatesParams{
		SessionID: "deleted-session", Project: p.ID,
		Candidates: []MemoryCandidateInput{{Category: CandidateCategoryDecision, Statement: "A durable decision from a deleted checkout", Confidence: 0.95}},
		Policy:     CandidateAdmissionPolicy{TTL: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.GetMemoryCandidate(result.CandidateIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	digest, err := MemoryCandidateDigest(candidate)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := s.ApproveMemoryCandidate(candidate.SyncID, digest, "Historical decision remains valid after checkout removal")
	if err != nil {
		t.Fatalf("promotion after directory removal: %v", err)
	}
	if derefString(obs.Project) != p.ID {
		t.Fatalf("promoted project = %q", derefString(obs.Project))
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Fatal("fixture root unexpectedly exists")
	}
}

func TestAdoptLegacyProjectRegistersNameKeyedBucket(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.db.Exec(`INSERT INTO sessions(id,project,directory) VALUES('legacy-adopt','ros_ws','/gone/ros_ws')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO observations(session_id,type,title,content,project) VALUES('legacy-adopt','decision','legacy','legacy body','ros_ws')`); err != nil {
		t.Fatal(err)
	}
	p, err := s.AdoptLegacyProject("ros_ws")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "ros_ws" || !isRegisteredProjectID(p.ID) {
		t.Fatalf("adopted identity = %+v", p)
	}
	var project string
	if err := s.db.QueryRow(`SELECT project FROM observations WHERE session_id='legacy-adopt'`).Scan(&project); err != nil || project != p.ID {
		t.Fatalf("observation project = %q err=%v, want %q", project, err, p.ID)
	}
	if got, err := s.CanonicalProjectKey("ros_ws"); err != nil || got != p.ID {
		t.Fatalf("alias after adopt = %q err=%v", got, err)
	}
}
