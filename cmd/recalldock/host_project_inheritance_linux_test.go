//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"recalldock.local/recalldock/internal/opsstore"
	"recalldock.local/recalldock/internal/project"
	"recalldock.local/recalldock/internal/store"
	"recalldock.local/recalldock/internal/worklog"
)

func hostLinkedWorktree(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	main, linked := t.TempDir(), filepath.Join(t.TempDir(), "linked")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", main}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Synthetic", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Synthetic", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		if body, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git fixture %v: %s %v", args, body, err)
		}
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "Synthetic worktree fixture")
	run("worktree", "add", "-q", "--detach", linked, "HEAD")
	return main, linked
}

func TestHostLinkedWorktreeWithoutBoundMainFailsClosed(t *testing.T) {
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, linked := hostLinkedWorktree(t)
	if _, err := ensureHostProject(s, cfg.DataDir, linked); err == nil {
		t.Fatal("unbound main checkout silently created a worktree identity")
	}
	projects, err := s.ListRegisteredProjects()
	if err != nil || len(projects) != 0 {
		t.Fatalf("rejection created a project: %+v %v", projects, err)
	}
	if _, err := os.Lstat(filepath.Join(linked, ".recalldock")); !os.IsNotExist(err) {
		t.Fatalf("rejection created a marker: %v", err)
	}
}

func TestHostLinkedWorktreeLifecycleReusesMainProject(t *testing.T) {
	for _, host := range []string{"codex", "claude-code", "antigravity"} {
		for _, auto := range []bool{false, true} {
			name := host + "/explicit"
			if auto {
				name = host + "/auto"
			}
			t.Run(name, func(t *testing.T) {
				cfg := testConfig(t)
				main, linked := hostLinkedWorktree(t)
				s, err := store.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				p, err := s.CreateRegisteredProject("Main checkout")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.BindProjectRoot(p.ID, main); err != nil {
					t.Fatal(err)
				}
				s.Close()
				args := []string{host}
				payload := map[string]any{"session_id": "linked", "cwd": linked, "hook_event_name": "SessionStart"}
				if host == "antigravity" {
					args = append(args, "--event", "SessionStart")
					payload = map[string]any{"conversationId": "linked", "workspacePaths": []string{linked}}
				}
				if auto {
					args = append(args, "--auto-project")
				}
				body, _ := json.Marshal(payload)
				var out bytes.Buffer
				if err := runHostEvent(cfg, args, bytes.NewReader(body), &out); err != nil || out.Len() == 0 {
					t.Fatalf("linked lifecycle failed: %v %s", err, out.String())
				}
				s, err = store.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				session, err := s.GetSession(host + "-linked")
				if err != nil || session.Project != p.ID || session.Directory != linked || session.Origin != store.SessionOriginHost {
					t.Fatalf("lifecycle changed inherited scope: %+v %v", session, err)
				}
				projects, err := s.ListRegisteredProjects()
				if err != nil || len(projects) != 1 {
					t.Fatalf("lifecycle created a shadow project: %+v %v", projects, err)
				}
				if _, err := os.Lstat(filepath.Join(linked, ".recalldock", "root.json")); !os.IsNotExist(err) {
					t.Fatalf("lifecycle created a shadow marker: %v", err)
				}
				ops, err := opsstore.Open(cfg.DataDir, true)
				if err != nil {
					t.Fatal(err)
				}
				defer ops.Close()
				if err := ops.ValidateHostSession(host+"-linked", host); err != nil {
					t.Fatal(err)
				}
				bound := ops.WithWriter(opsstore.Writer{Host: host, HostSessionID: host + "-linked", GatewaySessionID: "gateway-synthetic"})
				cp := opsstore.Checkpoint{Checkpoint: worklog.Checkpoint{Goal: "Exact root task"}}
				if _, err := bound.Save(main, "lane", 0, cp); !errors.Is(err, opsstore.ErrHostSession) {
					t.Fatalf("inherited project widened work root: %v", err)
				}
				if _, err := bound.Save(linked, "lane", 0, cp); err != nil {
					t.Fatal(err)
				}
				if _, err := ops.Read(main, "lane", 0); !errors.Is(err, opsstore.ErrNotFound) {
					t.Fatalf("main root inherited worktree task: %v", err)
				}
				if host != "antigravity" {
					payload["hook_event_name"] = "SessionEnd"
					body, _ = json.Marshal(payload)
					out.Reset()
					if err := runHostEvent(cfg, args, bytes.NewReader(body), &out); err != nil || out.Len() != 0 {
						t.Fatalf("linked lifecycle end failed: %v %s", err, out.String())
					}
					if err := ops.ValidateHostSession(host+"-linked", host); err == nil {
						t.Fatal("ended worktree session stayed active")
					}
				}
			})
		}
	}
}

func TestDefaultHostLifecycleLeavesUnregisteredGitScopesUnbound(t *testing.T) {
	for _, host := range []string{"codex", "claude-code", "antigravity"} {
		t.Run(host, func(t *testing.T) {
			cfg := testConfig(t)
			s, err := store.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			main, linked := hostLinkedWorktree(t)
			for _, root := range []string{main, linked} {
				payload := map[string]any{"session_id": "unregistered-git", "cwd": root, "hook_event_name": "SessionStart"}
				if host == "antigravity" {
					payload = map[string]any{"conversationId": "unregistered-git", "workspacePaths": []string{root}}
				}
				body, _ := json.Marshal(payload)
				var out bytes.Buffer
				if err := runHostEvent(cfg, nativeStartHookArgs(t, host), bytes.NewReader(body), &out); err != nil || out.Len() != 0 {
					t.Fatalf("unregistered Git scope forced binding: %v %s", err, out.String())
				}
				if _, err := os.Lstat(filepath.Join(root, ".recalldock", "root.json")); !os.IsNotExist(err) {
					t.Fatalf("default created identity: %v", err)
				}
			}
			projects, err := s.ListRegisteredProjects()
			if err != nil || len(projects) != 0 {
				t.Fatalf("unregistered Git startup created projects: %+v %v", projects, err)
			}
		})
	}
}

func TestHostAutoProjectInheritsValidatedParentBinding(t *testing.T) {
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root := t.TempDir()
	deep := filepath.Join(root, "src/module/deep")
	if err := os.MkdirAll(deep, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("Explicit parent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	got, err := ensureHostProject(s, cfg.DataDir, deep)
	if err != nil || got.Project != p.ID || got.Path != root || got.Source != project.SourceRegistered {
		t.Fatalf("deep hook shadowed known identity: %+v %v", got, err)
	}
	entries, err := s.ListRegisteredProjects()
	if err != nil || len(entries) != 1 {
		t.Fatal("hook minted an unrequested child identity")
	}
	if _, err := os.Stat(filepath.Join(deep, ".recalldock/root.json")); !os.IsNotExist(err) {
		t.Fatal("hook wrote a shadow marker")
	}
}

func TestHostAutoProjectInheritsExplicitConfigWithinGitBoundary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root := t.TempDir()
	deep := filepath.Join(root, "nested/deep")
	if err := os.MkdirAll(deep, 0700); err != nil {
		t.Fatal(err)
	}
	if body, err := exec.Command("git", "init", "--quiet", root).CombinedOutput(); err != nil {
		t.Fatalf("git fixture: %s %v", body, err)
	}
	if err := os.Mkdir(filepath.Join(root, ".recalldock"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".recalldock/config.json"), []byte(`{"project_name":"declared-bucket"}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ensureHostProject(s, cfg.DataDir, deep)
	if err != nil || got.Project != "declared-bucket" || got.Source != project.SourceConfig || got.Path != root {
		t.Fatalf("hook disagrees with explicit read/write scope: %+v %v", got, err)
	}
	entries, err := s.ListRegisteredProjects()
	if err != nil || len(entries) != 0 {
		t.Fatal("explicit legacy declaration was silently replaced by new identity")
	}
}

func TestHostAutoProjectKeepsNestedIdentityAndGitIsolation(t *testing.T) {
	for _, kind := range []string{"registered-child", "nested-git", "copied-marker"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig(t)
			s, err := store.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			root := t.TempDir()
			child := filepath.Join(root, "child")
			deep := filepath.Join(child, "deep")
			if err := os.MkdirAll(deep, 0700); err != nil {
				t.Fatal(err)
			}
			parent, err := s.CreateRegisteredProject("Parent")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.BindProjectRoot(parent.ID, root); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "registered-child":
				p, err := s.CreateRegisteredProject("Child")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.BindProjectRoot(p.ID, child); err != nil {
					t.Fatal(err)
				}
				got, err := ensureHostProject(s, cfg.DataDir, deep)
				if err != nil || got.Project != p.ID || got.Path != child {
					t.Fatalf("nested root: %+v %v", got, err)
				}
			case "nested-git":
				if _, err := exec.LookPath("git"); err != nil {
					t.Skip("git unavailable")
				}
				if body, err := exec.Command("git", "init", "--quiet", child).CombinedOutput(); err != nil {
					t.Fatalf("git fixture: %s %v", body, err)
				}
				got, err := ensureHostProject(s, cfg.DataDir, deep)
				if err != nil || got.Project == parent.ID || got.Source != project.SourceRegistered {
					t.Fatalf("nested Git inherited foreign parent: %+v %v", got, err)
				}
			case "copied-marker":
				marker, err := os.ReadFile(filepath.Join(root, ".recalldock/root.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(child, ".recalldock"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(child, ".recalldock/root.json"), marker, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := ensureHostProject(s, cfg.DataDir, deep); err == nil {
					t.Fatal("copied marker silently adopted")
				}
				entries, err := s.ListRegisteredProjects()
				if err != nil || len(entries) != 1 {
					t.Fatal("invalid marker minted orphan identity")
				}
			}
		})
	}
}

func TestHostAutoProjectUsesUnboundRepositoryRootAcrossWorkingDirectories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root := t.TempDir()
	a, b := filepath.Join(root, "src/a"), filepath.Join(root, "src/b")
	for _, dir := range []string{a, b} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if body, err := exec.Command("git", "init", "--quiet", root).CombinedOutput(); err != nil {
		t.Fatalf("git fixture: %s %v", body, err)
	}
	first, err := ensureHostProject(s, cfg.DataDir, a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureHostProject(s, cfg.DataDir, b)
	if err != nil {
		t.Fatal(err)
	}
	if first.Project != second.Project || first.Path != root || second.Path != root {
		t.Fatalf("one repository split by launch cwd: %+v %+v", first, second)
	}
	entries, err := s.ListRegisteredProjects()
	if err != nil || len(entries) != 1 {
		t.Fatal("unrequested sibling identities")
	}
}
