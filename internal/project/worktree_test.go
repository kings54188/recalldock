package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
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

func initRepoWithCommit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-q", "-m", "init")
}

func TestIsLinkedWorktreeDistinguishesMainCheckout(t *testing.T) {
	mainDir := t.TempDir()
	initRepoWithCommit(t, mainDir)
	if linked, root := IsLinkedWorktree(mainDir); linked || root != "" {
		t.Fatalf("main checkout reported linked=%v root=%q", linked, root)
	}

	worktree := filepath.Join(t.TempDir(), "wt")
	runGit(t, mainDir, "worktree", "add", "-q", worktree, "-b", "feature-x")

	linked, root := IsLinkedWorktree(worktree)
	if !linked {
		t.Fatal("linked worktree not detected")
	}
	wantMain, err := filepath.EvalSymlinks(mainDir)
	if err != nil {
		t.Fatal(err)
	}
	if root != wantMain {
		t.Fatalf("main root=%q want %q", root, wantMain)
	}
	nested := filepath.Join(worktree, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if linked, _ := IsLinkedWorktree(nested); !linked {
		t.Fatal("subdirectory of a linked worktree was not detected")
	}
	if linked, _ := IsLinkedWorktree(filepath.Join(worktree, "missing")); linked {
		t.Fatal("missing path reported as worktree")
	}
}

func TestIsLinkedWorktreeIgnoresNonRepositories(t *testing.T) {
	dir := t.TempDir()
	if linked, root := IsLinkedWorktree(dir); linked || root != "" {
		t.Fatalf("plain directory reported linked=%v root=%q", linked, root)
	}
}

// TestBrokenGitDeclarationFailsClosed pins the boundary: a directory that
// declares .git but cannot be resolved must not inherit an ancestor project's
// binding (broken checkout, dangling submodule/worktree pointer).
func TestBrokenGitDeclarationFailsClosed(t *testing.T) {
	parent := t.TempDir()
	initRepoWithCommit(t, parent)
	child := filepath.Join(parent, "broken-sub")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, ".git"), []byte("gitdir: /nonexistent/.git/modules/broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := DetectProjectFull(child)
	if res.Error == nil {
		t.Fatalf("broken .git inherited a binding: %+v", res)
	}
	nested := filepath.Join(child, "src")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if res := DetectProjectFull(nested); res.Error == nil {
		t.Fatalf("nested path under broken .git inherited a binding: %+v", res)
	}
	// The healthy parent checkout keeps working.
	if res := DetectProjectFull(parent); res.Error != nil {
		t.Fatalf("healthy repo regressed: %+v", res)
	}
}
