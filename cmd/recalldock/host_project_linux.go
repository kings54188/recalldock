//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
)

// ensureHostProject serializes automatic provisioning across host processes.
// A pre-existing marker is always validated, never replaced automatically.
func ensureHostProject(s *store.Store, dataDir, dir string) (project.DetectionResult, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil || !filepath.IsAbs(root) {
		return project.DetectionResult{}, fmt.Errorf("invalid host working directory")
	}
	root = filepath.Clean(root)
	home, _ := os.UserHomeDir()
	home, _ = filepath.EvalSymlinks(home)
	if root == home || filepath.Dir(root) == root {
		return project.DetectionResult{}, fmt.Errorf("cannot automatically bind home or filesystem root")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return project.DetectionResult{}, fmt.Errorf("host working directory unavailable")
	}
	lockPath := filepath.Join(dataDir, ".host-project.lock")
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return project.DetectionResult{}, fmt.Errorf("host project lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	defer lock.Close()
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return project.DetectionResult{}, err
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)

	// Reuse an established scope in deep working directories. The detector
	// already enforces nested roots and Git boundaries; creating a child marker
	// here would silently hide that project's knowledge from the host session.
	d := project.DetectExplicitWithRegistry(root, s)
	if d.Error != nil {
		return d, d.Error
	}
	if d.Project != "" && (d.Source == project.SourceRegistered || d.Source == project.SourceConfig || d.Source == project.SourceWorktreeMain) {
		return d, nil
	}
	if linked, _ := project.IsLinkedWorktree(root); linked {
		return d, fmt.Errorf("linked worktree requires an explicit binding or a bound main checkout")
	}
	// When no identity exists, establish it at this repository's actual root
	// so launching from sibling source directories cannot split one checkout.
	// Nested repositories still receive distinct new opaque IDs.
	if repositoryRoot := project.EnclosingRepositoryRoot(root); repositoryRoot != "" {
		root = repositoryRoot
		if root == home || filepath.Dir(root) == root {
			return project.DetectionResult{}, fmt.Errorf("cannot automatically bind home or filesystem root")
		}
	}
	name := filepath.Base(root)
	p, err := s.CreateRegisteredProject(name)
	if err != nil {
		return project.DetectionResult{}, err
	}
	if _, err = s.BindProjectRoot(p.ID, root); err != nil {
		return project.DetectionResult{}, fmt.Errorf("automatic binding failed; project %s retained for inspection: %w", p.ID, err)
	}
	d = project.DetectExplicitWithRegistry(root, s)
	if d.Error != nil || d.Project != p.ID {
		return d, fmt.Errorf("automatic project binding verification failed")
	}
	return d, nil
}
