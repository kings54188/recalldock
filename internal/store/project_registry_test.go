package store

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRegisteredProjectBindingMatrix(t *testing.T) {
	s := newTestStore(t)
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(filepath.Join(root, "a/b/c"), 0700); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("Same display name")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateRegisteredProject("Same display name")
	if err != nil || other.ID == p.ID {
		t.Fatal("display names must not determine identity")
	}
	b, err := s.BindProjectRoot(p.ID, root)
	if err != nil {
		t.Fatal(err)
	}
	got := s.DetectProject(filepath.Join(root, "a/b/c"))
	if got.Error != nil || got.Project != p.ID || got.BindingID != b.ID {
		t.Fatalf("deep inheritance: %+v", got)
	}
	// Copying a marker is not proof that this is the same directory instance.
	copied := filepath.Join(t.TempDir(), "copy")
	if err := os.MkdirAll(filepath.Join(copied, ".recalldock"), 0700); err != nil {
		t.Fatal(err)
	}
	marker, _ := os.ReadFile(filepath.Join(root, ".recalldock/root.json"))
	if err := os.WriteFile(filepath.Join(copied, ".recalldock/root.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	if got := s.DetectProject(copied); got.Error == nil {
		t.Fatal("copied binding must fail closed")
	}
	// A move keeps project history, but requires an explicit new binding.
	moved := filepath.Join(filepath.Dir(root), "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if got := s.DetectProject(moved); got.Error == nil {
		t.Fatal("move must require explicit rebind")
	}
	if _, err := s.BindProjectRoot(p.ID, moved); err != nil {
		t.Fatal(err)
	}
	if got := s.DetectProject(filepath.Join(moved, "a/b/c")); got.Error != nil || got.Project != p.ID {
		t.Fatalf("rebind: %+v", got)
	}
	// Recreated old path never gains the opaque project ID by path/name match.
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if got := s.DetectProject(root); got.Project == p.ID {
		t.Fatal("recreated path inherited old memory")
	}
	// Independent nested registered root beats parent.
	child := filepath.Join(moved, "a/b")
	if _, err := s.BindProjectRoot(other.ID, child); err != nil {
		t.Fatal(err)
	}
	if got := s.DetectProject(filepath.Join(child, "c")); got.Error != nil || got.Project != other.ID {
		t.Fatalf("nested root: %+v", got)
	}
	// Activating history performs no binding or memory loading.
	active, err := s.ActivateProjectReadOnly(p.ID)
	if err != nil || active.ProjectID != p.ID || active.Mode != "read_only" {
		t.Fatalf("activation: %+v %v", active, err)
	}
	if _, err := s.BindProjectRoot("prj-missing", t.TempDir()); !errors.Is(err, ErrUnknownRegisteredProject) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestRegisteredBindingRejectsOtherStoreAndRecreatedDirectory(t *testing.T) {
	s := newTestStore(t)
	other := newTestStore(t)
	root := filepath.Join(t.TempDir(), "root")
	os.Mkdir(root, 0700)
	p, _ := s.CreateRegisteredProject("one")
	s.BindProjectRoot(p.ID, root)
	if got := other.DetectProject(root); got.Error == nil {
		t.Fatal("foreign store marker must fail closed")
	}
	old := root + "-old"
	if err := os.Rename(root, old); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, ".recalldock"), 0700)
	marker, _ := os.ReadFile(filepath.Join(old, ".recalldock/root.json"))
	os.WriteFile(filepath.Join(root, ".recalldock/root.json"), marker, 0600)
	if got := s.DetectProject(root); got.Error == nil {
		t.Fatal("same path with copied marker is a new physical directory")
	}
}

// TestBindProjectRootSupersedesRenamedDirectoryBinding covers the rename
// workflow: a directory keeps its device+inode across `mv`, so rebinding the
// project at the new path must supersede the stale old-path binding instead
// of leaving two active bindings that both validate by inode.
func TestBindProjectRootSupersedesRenamedDirectoryBinding(t *testing.T) {
	s := newTestStore(t)
	parent := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = normalizePath(resolved)
	}
	oldRoot := filepath.Join(parent, "before")
	if err := os.Mkdir(oldRoot, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("rename-me")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, oldRoot); err != nil {
		t.Fatal(err)
	}
	newRoot := filepath.Join(parent, "after")
	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, newRoot); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_bindings WHERE project_id=? AND state='active'`, p.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("renamed-directory rebind must supersede the stale row, active=%d", active)
	}
	var stale string
	if err := s.db.QueryRow(`SELECT state FROM project_bindings WHERE root_path=?`, filepath.Clean(oldRoot)).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != "superseded" {
		t.Fatalf("old-path binding state = %q, want superseded", stale)
	}
	if got := s.DetectProject(newRoot); got.Error != nil || got.ProjectID != p.ID {
		t.Fatalf("new root must resolve to the registered project: %+v", got)
	}
}

func TestRegisteredIdentityExchangeAndWriteBoundary(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	p, err := s.CreateRegisteredProject("history")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("unbound", p.ID, root); err == nil {
		t.Fatal("unbound writes must fail")
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("bound", p.ID, root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(AddObservationParams{SessionID: "bound", Project: p.ID, Type: "decision", Title: "Stable identity", Content: "Synthetic", Scope: "project"}); err != nil {
		t.Fatal(err)
	}
	data, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	dest := newTestStore(t)
	if _, err := dest.Import(data); err != nil {
		t.Fatal(err)
	}
	if _, err := dest.ActivateProjectReadOnly(p.ID); err != nil {
		t.Fatal("export/import lost project identity", err)
	}
	if _, err := dest.Import(data); err != nil {
		t.Fatal("replay", err)
	}
	if err := dest.CreateSession("not-bound-after-import", p.ID, t.TempDir()); err == nil {
		t.Fatal("exchange must not grant a filesystem binding")
	}
	if _, err := dest.ActivateProjectReadOnly("history"); err == nil {
		t.Fatal("display names are not activation identities")
	}
}

func TestRegisteredBindingDoesNotCrossNestedGitBoundary(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	p, _ := s.CreateRegisteredProject("outer")
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "independent")
	if err := os.MkdirAll(filepath.Join(child, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	// A valid independent repo boundary is not merely a folder named .git.
	cmd := exec.Command("git", "init", "--quiet", child)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if got := s.DetectProject(child); got.Error != nil || got.ProjectID == p.ID {
		t.Fatalf("nested git boundary: %+v", got)
	}
}

func TestBindingInterruptedCommitFailsClosedAndCanExplicitlyRecover(t *testing.T) {
	s := newTestStore(t)
	p, _ := s.CreateRegisteredProject("recovery")
	root := t.TempDir()
	original := s.hooks.commit
	s.hooks.commit = func(tx *sql.Tx) error { _ = tx.Rollback(); return errors.New("synthetic interrupted SQL commit") }
	if _, err := s.BindProjectRoot(p.ID, root); err == nil {
		t.Fatal("must surface commit failure")
	}
	s.hooks.commit = original
	if got := s.DetectProject(root); got.Error == nil {
		t.Fatal("uncommitted marker must never authorize a project")
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatalf("explicit recovery: %v", err)
	}
	if got := s.DetectProject(root); got.Error != nil || got.ProjectID != p.ID {
		t.Fatalf("recovered: %+v", got)
	}
}

func TestBindingRejectsUnsafeRootAndOtherWriters(t *testing.T) {
	s := newTestStore(t)
	p, _ := s.CreateRegisteredProject("guard")
	if _, err := s.BindProjectRoot(p.ID, "relative"); err == nil {
		t.Fatal("relative root accepted")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, err := s.BindProjectRoot(p.ID, home); err == nil {
		t.Fatal("HOME must not become an implicit project ancestor")
	}
	root := t.TempDir()
	hidden := filepath.Join(root, ".recalldock")
	if err := os.Mkdir(hidden, 0700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(hidden, ".binding.lock")
	if err := os.WriteFile(lock, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, root); err == nil {
		t.Fatal("other writer's lock ignored")
	}
	body, _ := os.ReadFile(lock)
	if string(body) != "other" {
		t.Fatal("other writer's lock changed")
	}
}

func TestLegacyProjectNameWithPrjPrefixRemainsLegacy(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateSession("legacy-prefix", "prj-old-name", "/nonexistent-legacy-directory"); err != nil {
		t.Fatalf("unrelated legacy names must not be treated as opaque IDs: %v", err)
	}
}
