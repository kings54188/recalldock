//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
)

func ensureHostProject(s *store.Store, dataDir, dir string) (project.DetectionResult, error) {
	if strings.HasPrefix(filepath.Clean(dir), `\\`) {
		return project.DetectionResult{}, fmt.Errorf("UNC network paths are not supported for host project bindings")
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil || !filepath.IsAbs(root) {
		return project.DetectionResult{}, fmt.Errorf("invalid host working directory")
	}
	root = normalizeHostPath(root)
	home, _ := os.UserHomeDir()
	home, _ = filepath.EvalSymlinks(home)
	home = normalizeHostPath(home)
	if strings.EqualFold(root, home) || filepath.Dir(root) == root {
		return project.DetectionResult{}, fmt.Errorf("cannot automatically bind home or filesystem root")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return project.DetectionResult{}, fmt.Errorf("host working directory unavailable")
	}
	lockPath := filepath.Join(dataDir, ".host-project.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return project.DetectionResult{}, fmt.Errorf("host project lock: %w", err)
	}
	defer lock.Close()
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); err != nil {
		return project.DetectionResult{}, err
	}
	defer func() { _ = windows.UnlockFileEx(windows.Handle(lock.Fd()), 0, 1, 0, ol) }()

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
	if repositoryRoot := project.EnclosingRepositoryRoot(root); repositoryRoot != "" {
		root = normalizeHostPath(repositoryRoot)
		if strings.EqualFold(root, home) || filepath.Dir(root) == root {
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

func normalizeHostPath(path string) string {
	path = filepath.Clean(path)
	if len(path) >= 2 && path[1] == ':' && path[0] >= 'a' && path[0] <= 'z' {
		path = strings.ToUpper(path[:1]) + path[1:]
	}
	return path
}
