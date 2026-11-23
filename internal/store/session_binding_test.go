package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitInit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// TestModelSessionRequiresEstablishedBinding pins the tightened policy: a
// model transport cannot create the first identity for a directory by name,
// while operator/host transports keep the legacy behaviour.
func TestModelSessionRequiresEstablishedBinding(t *testing.T) {
	s := newTestStore(t)
	unbound := t.TempDir()

	err := s.CreateSessionWithOrigin("model-unbound", "fresh-checkout", unbound, SessionOriginModel)
	if !errors.Is(err, ErrProjectBindingRequired) {
		t.Fatalf("unbound model session err=%v", err)
	}
	if _, err := s.GetSession("model-unbound"); err == nil {
		t.Fatal("refused model session was written")
	}
	if err := s.CreateSessionWithOrigin("operator-unbound", "fresh-checkout", unbound, SessionOriginOperator); err != nil {
		t.Fatalf("operator session in unbound dir: %v", err)
	}

	bound := t.TempDir()
	proj, err := s.CreateRegisteredProject("bound workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(proj.ID, bound); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSessionWithOrigin("model-bound", proj.ID, bound, SessionOriginModel); err != nil {
		t.Fatalf("model session in bound dir: %v", err)
	}
}

// TestModelSessionWorktreeInheritsMainBinding pins the worktree policy: a
// linked worktree of a bound checkout reuses that identity instead of
// fragmenting into a new project name.
func TestModelSessionWorktreeInheritsMainBinding(t *testing.T) {
	s := newTestStore(t)
	mainDir := t.TempDir()
	gitInit(t, mainDir, "init", "-q")
	if err := os.WriteFile(filepath.Join(mainDir, "README.md"), []byte("probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitInit(t, mainDir, "add", "README.md")
	gitInit(t, mainDir, "commit", "-q", "-m", "init")

	proj, err := s.CreateRegisteredProject("worktree main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(proj.ID, mainDir); err != nil {
		t.Fatal(err)
	}

	worktree := filepath.Join(t.TempDir(), "wt")
	gitInit(t, mainDir, "worktree", "add", "-q", worktree, "-b", "feature-inherit")

	det := s.DetectProject(worktree)
	if det.ProjectID != proj.ID || det.Source != "worktree_main" {
		t.Fatalf("worktree detection=%+v", det)
	}
	if err := s.CreateSessionWithOrigin("model-worktree", proj.ID, worktree, SessionOriginModel); err != nil {
		t.Fatalf("model session in worktree: %v", err)
	}
	sess, err := s.GetSession("model-worktree")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Project != proj.ID || sess.Origin != SessionOriginModel || sess.Directory != worktree {
		t.Fatalf("session=%+v", sess)
	}

	// An unbound main checkout must not let the worktree invent an identity.
	otherMain := t.TempDir()
	gitInit(t, otherMain, "init", "-q")
	if err := os.WriteFile(filepath.Join(otherMain, "README.md"), []byte("probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitInit(t, otherMain, "add", "README.md")
	gitInit(t, otherMain, "commit", "-q", "-m", "init")
	otherWT := filepath.Join(t.TempDir(), "wt2")
	gitInit(t, otherMain, "worktree", "add", "-q", otherWT, "-b", "feature-unbound")
	det = s.DetectProject(otherWT)
	if det.ProjectID != "" {
		t.Fatalf("unbound worktree resolved to %+v", det)
	}
	if err := s.CreateSessionWithOrigin("model-unbound-wt", det.Project, otherWT, SessionOriginModel); !errors.Is(err, ErrProjectBindingRequired) {
		t.Fatalf("unbound worktree model session err=%v", err)
	}
}
