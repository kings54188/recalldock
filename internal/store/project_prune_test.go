package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneMissingRegisteredIdentityProtectsExistingRoots(t *testing.T) {
	s := newCandidateTestStore(t)
	p, e := s.CreateRegisteredProject("smoke")
	if e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(t.TempDir(), "gone")
	if e = os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	if _, e = s.BindProjectRoot(p.ID, root); e != nil {
		t.Fatal(e)
	}
	if s.PruneMissingRegisteredProject(p.ID, true) == nil {
		t.Fatal("existing root removed")
	}
	if e = os.Remove(filepath.Join(root, ".recalldock", "root.json")); e != nil {
		t.Fatal(e)
	}
	// .binding.lock is intentionally left on disk by filelock: the lock path must
	// stay stable so every acquirer contends for the same inode. Removing the
	// metadata directory therefore needs RemoveAll, not Remove.
	if e = os.RemoveAll(filepath.Join(root, ".recalldock")); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(root); e != nil {
		t.Fatal(e)
	}
	if e = s.PruneMissingRegisteredProject(p.ID, false); e != nil {
		t.Fatal(e)
	}
	if _, e = s.RegisteredProject(p.ID); e != nil {
		t.Fatal("dry run removed identity")
	}
	if e = s.PruneMissingRegisteredProject(p.ID, true); e != nil {
		t.Fatal(e)
	}
	if _, e = s.RegisteredProject(p.ID); e == nil {
		t.Fatal("identity retained")
	}
}

func TestPruneProjectRefusesRegisteredIdentityKeys(t *testing.T) {
	s := newCandidateTestStore(t)
	p, e := s.CreateRegisteredProject("smoke-registered")
	if e != nil {
		t.Fatal(e)
	}
	// A legacy name-keyed row can predate canonicalization; model/operator
	// writes no longer create one, so seed it directly for the prune guard.
	if _, e = s.db.Exec(`INSERT INTO sessions(id,project,directory) VALUES(?,?,?)`, "name-keyed", "smoke-registered", "/tmp/gone-name"); e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(t.TempDir(), "bound-root")
	if e = os.MkdirAll(filepath.Join(root, ".recalldock"), 0700); e != nil {
		t.Fatal(e)
	}
	if _, e = s.BindProjectRoot(p.ID, root); e != nil {
		t.Fatal(e)
	}
	if e = s.CreateSession("id-keyed", p.ID, root); e != nil {
		t.Fatal(e)
	}
	// The name-keyed interactive path must refuse registered identity keys:
	// their real data may live under other keys (legacy imports) and only the
	// missing-root verified `--project ID` path is safe.
	for _, key := range []string{"smoke-registered", p.ID} {
		if _, err := s.PruneProject(key); err == nil {
			t.Fatalf("name-keyed prune accepted registered key %q", key)
		}
	}
	// Unregistered legacy name buckets remain prunable.
	if _, err := s.PruneProject("legacy-bucket"); err != nil {
		t.Fatalf("unregistered bucket refused: %v", err)
	}
}

func TestDeleteRegisteredProjectOnlyUnboundEmpty(t *testing.T) {
	s := newCandidateTestStore(t)
	p, err := s.CreateRegisteredProject("delete-probe")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRegisteredProject(p.ID, false); err == nil {
		t.Fatal("delete without confirm accepted")
	}
	if err := s.DeleteRegisteredProject(p.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisteredProject(p.ID); err == nil {
		t.Fatal("identity survived delete")
	}
	bound, err := s.CreateRegisteredProject("bound-delete")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := s.BindProjectRoot(bound.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRegisteredProject(bound.ID, true); err == nil {
		t.Fatal("bound project deleted")
	}
}
