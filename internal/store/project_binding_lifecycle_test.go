package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBindingDeactivationIsExactAndLeavesFailClosedFence(t *testing.T) {
	s := newTestStore(t)
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateRegisteredProject("Parent")
	c, _ := s.CreateRegisteredProject("Child")
	target, _ := s.CreateRegisteredProject("Target")
	if _, err := s.BindProjectRoot(p.ID, parent); err != nil {
		t.Fatal(err)
	}
	b, err := s.BindProjectRoot(c.ID, child)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeactivateProjectBinding(c.ID, child, "bind-wrong"); err == nil {
		t.Fatal("wrong CAS accepted")
	}
	if _, err := s.BindProjectRoot(target.ID, child); err == nil {
		t.Fatal("active foreign marker replaced")
	}
	stopped, err := s.DeactivateProjectBinding(c.ID, child, b.ID)
	if err != nil || stopped.State != "superseded" {
		t.Fatalf("deactivate: %+v %v", stopped, err)
	}
	if got := s.DetectExplicitProject(child); got.Error == nil || got.Project == p.ID {
		t.Fatal("inactive child inherited parent")
	}
	rebound, err := s.BindProjectRoot(target.ID, child)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeactivateProjectBinding(c.ID, child, b.ID); err != nil {
		t.Fatal("replay should describe selected old binding", err)
	}
	if got := s.DetectExplicitProject(child); got.Error != nil || got.BindingID != rebound.ID {
		t.Fatal("old replay deactivated new binding")
	}
	entries, err := s.ListProjectBindings(c.ID, 20, "")
	if err != nil || len(entries) != 1 || entries[0].State != "superseded" {
		t.Fatalf("index: %+v %v", entries, err)
	}
	if _, err := s.MergeProjects([]string{c.ID}, target.ID); err != nil {
		t.Fatal("unbound identity cannot migrate", err)
	}
}

func TestBindingDeactivationCanSelectMissingRootWithoutGuessing(t *testing.T) {
	s := newTestStore(t)
	root := filepath.Join(t.TempDir(), "old")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	p, _ := s.CreateRegisteredProject("Moved")
	b, err := s.BindProjectRoot(p.ID, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeactivateProjectBinding(p.ID, root, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListProjectBindings(p.ID, 21, ""); err == nil {
		t.Fatal("unbounded index")
	}
}
