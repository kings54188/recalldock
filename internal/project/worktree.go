package project

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"os/exec"
)

// IsLinkedWorktree reports whether dir lives in a git linked worktree (a
// `git worktree add` checkout) rather than the main working tree. It returns
// the main working tree root when it can be resolved.
//
// The check is filesystem/scope information only: it never selects historical
// memory and never writes a binding. Callers use it to fail closed instead of
// silently creating a second identity for the same repository.
func IsLinkedWorktree(dir string) (bool, string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, ""
	}
	gitDir := gitRevParse(abs, "--absolute-git-dir")
	commonDir := gitRevParse(abs, "--git-common-dir")
	if gitDir == "" || commonDir == "" {
		return false, ""
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(abs, commonDir)
	}
	gitDir = canonicalizePath(gitDir)
	commonDir = canonicalizePath(commonDir)
	if samePath(gitDir, commonDir) {
		return false, ""
	}
	// <main>/.git is the common dir for the main worktree; its parent holds the
	// main checkout. A bare repository has no checkout to point at.
	mainRoot := filepath.Dir(commonDir)
	if filepath.Base(commonDir) != ".git" {
		return true, ""
	}
	return true, mainRoot
}

func gitRevParse(dir, flag string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", flag).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
