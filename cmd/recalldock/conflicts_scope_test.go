package main

import (
	"testing"

	"recalldock.local/recalldock/internal/store"
)

// TestResolveConflictsProjectUsesStoreBinding pins the fix for a silent
// under-scan: conflicts used a registry-less detector, so a bound checkout
// resolved to "unknown" and a scan reported zero conflicts.
func TestResolveConflictsProjectUsesStoreBinding(t *testing.T) {
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	root := t.TempDir()
	proj, err := s.CreateRegisteredProject("conflicts scope")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(proj.ID, root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	if got := resolveConflictsProject(s, ""); got != proj.ID {
		t.Fatalf("resolved=%q want %q", got, proj.ID)
	}
	explicit, err := s.CreateRegisteredProject("explicit-name")
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveConflictsProject(s, "explicit-name"); got != explicit.ID {
		t.Fatalf("explicit override=%q want %q", got, explicit.ID)
	}

	unbound := t.TempDir()
	t.Chdir(unbound)
	oldExit := exitFunc
	exitFunc = func(int) {}
	t.Cleanup(func() { exitFunc = oldExit })
	if got := resolveConflictsProject(s, ""); got != "" {
		t.Fatalf("unbound dir resolved to %q", got)
	}
}
