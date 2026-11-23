package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestCLIResourcesCheckBeyondListLimit(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	path := filepath.Join(root, "seed.md")
	if err := os.WriteFile(path, []byte("synthetic resource"), 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := hashResourceFile(path)
	if err != nil {
		t.Fatal(err)
	}
	id := mustSeedObservation(t, cfg, "resource-limit", "alpha", "decision", "Resource memory", "Synthetic fact", "project")
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	paths := make(map[string]string)
	for i := 0; i < 201; i++ {
		path := filepath.Join(root, fmt.Sprintf("resource-%03d.md", i))
		slashed := filepath.ToSlash(filepath.Clean(path))
		if len(slashed) > 0 && slashed[0] != '/' {
			slashed = "/" + slashed
		}
		ref := "file://" + slashed + "@sha256:" + identity.SHA256
		paths[ref] = path
		if _, err := s.AddResourceDependency(store.AddResourceDependencyParams{Project: "alpha", SourceRef: ref, DependentObservationID: id}); err != nil {
			t.Fatal(err)
		}
	}
	deps, err := s.ListResourceDependencies(store.ResourceDependencyListOptions{Project: "alpha"})
	if err != nil || len(deps) != 200 {
		t.Fatalf("list limit: count=%d err=%v", len(deps), err)
	}
	for _, dep := range deps {
		delete(paths, dep.SourceRef)
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("synthetic resource"), 0600); err != nil {
			t.Fatal(err)
		}
		withArgs(t, "recalldock", "resources", "check", "--project", "alpha", "--file", path)
		out, errout := captureOutput(t, func() { cmdResourcesCheck(cfg) })
		var result ResourceCheckResult
		if err := json.Unmarshal([]byte(out), &result); err != nil || errout != "" || result.Status != "unchanged" {
			t.Fatalf("registered resource beyond list limit: out=%q stderr=%q err=%v", out, errout, err)
		}
	}
}

func TestMainReadCommandsNeverInitializeMissingStore(t *testing.T) {
	commands := [][]string{{"search", "kicker", "--all"}, {"context", "--all"}, {"stats", "--all", "--json"}, {"doctor"}, {"mcp"}}
	for _, command := range commands {
		t.Run(command[0], func(t *testing.T) {
			cfg := testConfig(t)
			cfg.DataDir = filepath.Join(cfg.DataDir, "typo")
			t.Setenv("RECALLDOCK_DATA_DIR", cfg.DataDir)
			stubExitWithPanic(t)
			withArgs(t, append([]string{"recalldock"}, command...)...)
			_, stderr, recovered := captureOutputAndRecover(t, main)
			if recovered == nil || !strings.Contains(stderr, "store_not_initialized") {
				t.Fatalf("read pretended empty: %s", stderr)
			}
			if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
				t.Fatalf("read created missing directory: %v", err)
			}
		})
	}
}

func TestMainUnknownProjectsAreErrorsAcrossReadCommands(t *testing.T) {
	commands := [][]string{{"stats"}, {"doctor"}, {"review", "list"}, {"conflicts", "scan"}}
	for _, command := range commands {
		t.Run(command[0], func(t *testing.T) {
			cfg := testConfig(t)
			s, err := store.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			t.Setenv("RECALLDOCK_DATA_DIR", cfg.DataDir)
			stubExitWithPanic(t)
			args := append([]string{"recalldock"}, command...)
			args = append(args, "--project", "nonexistent_xyz")
			withArgs(t, args...)
			stdout, stderr, recovered := captureOutputAndRecover(t, main)
			if recovered == nil || stdout != "" || !strings.Contains(stderr, "unknown_project") {
				t.Fatalf("unknown project declared healthy: %q %q", stdout, stderr)
			}
		})
	}
}

func TestCLIResourcesCheckExactPathAndProject(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	path := filepath.Join(root, "source.md")
	otherPath := path + "@branch.md"
	for _, p := range []string{path, otherPath} {
		if err := os.WriteFile(p, []byte("synthetic resource"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := hashResourceFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	id := mustSeedObservation(t, cfg, "resource-path", "alpha", "decision", "Resource memory", "Synthetic fact", "project")
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AddResourceDependency(store.AddResourceDependencyParams{Project: "alpha", SourceRef: identity.SourceRef, DependentObservationID: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRegisteredProject("beta"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, project, status string }{
		{path, "alpha", "unregistered"},
		{otherPath, "alpha", "unchanged"},
		{otherPath, "beta", "unregistered"},
	} {
		withArgs(t, "recalldock", "resources", "check", "--project", tc.project, "--file", tc.path)
		out, errout := captureOutput(t, func() { cmdResourcesCheck(cfg) })
		var result ResourceCheckResult
		if err := json.Unmarshal([]byte(out), &result); err != nil || errout != "" || result.Status != tc.status {
			t.Fatalf("path=%q project=%q want=%s out=%q stderr=%q err=%v", tc.path, tc.project, tc.status, out, errout, err)
		}
	}
}

func TestCLIResourcesRejectEmptyProjectBeforeOpeningDatabase(t *testing.T) {
	for _, sub := range []string{"register", "check"} {
		t.Run(sub, func(t *testing.T) {
			stubExitWithPanic(t)
			cfg := testConfig(t)
			cfg.DataDir = filepath.Join(cfg.DataDir, "not-created")
			path := filepath.Join(t.TempDir(), "source.md")
			if err := os.WriteFile(path, []byte("synthetic resource"), 0600); err != nil {
				t.Fatal(err)
			}
			withArgs(t, "recalldock", "resources", sub, "--project", " ", "--observation-id", "1", "--file", path)
			if sub == "check" {
				withArgs(t, "recalldock", "resources", sub, "--project", " ", "--file", path)
			}
			_, errout, panicValue := captureOutputAndRecover(t, func() { cmdResources(cfg) })
			if panicValue == nil || errout == "" {
				t.Fatal("empty project accepted")
			}
			if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
				t.Fatalf("invalid project opened database: %v", err)
			}
		})
	}
}

func TestCLIReadScopeDefaultsUnboundAndAllIsExplicit(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	withCwd(t, dir)
	mustSeedObservation(t, cfg, "scope-one", "unrelated", "decision", "unique-needle", "private-body-needle", "project")
	for _, command := range []string{"search", "context", "stats"} {
		t.Run(command, func(t *testing.T) {
			args := []string{"recalldock", command}
			if command == "search" {
				args = append(args, "needle")
			}
			withArgs(t, args...)
			out, errout := captureOutput(t, func() {
				switch command {
				case "search":
					cmdSearch(cfg)
				case "context":
					cmdContext(cfg)
				case "stats":
					cmdStats(cfg)
				}
			})
			if errout != "" || strings.Contains(out, "private-body-needle") || strings.Contains(out, "unique-needle") || strings.Contains(out, "Observations: 1") {
				t.Fatalf("unbound leak: %q %q", out, errout)
			}
			if !strings.Contains(out, "unbound") {
				t.Fatalf("scope not visible: %q", out)
			}
		})
	}
	withArgs(t, "recalldock", "stats", "--all", "--json")
	out, errout := captureOutput(t, func() { cmdStats(cfg) })
	var result struct {
		Stats store.Stats `json:"stats"`
		Scope string      `json:"scope"`
	}
	if json.Unmarshal([]byte(out), &result) != nil || errout != "" || result.Stats.TotalObservations != 1 || result.Scope != "all" {
		t.Fatalf("all stats: %q %q", out, errout)
	}
	// Merely reusing a basename or having one child repository cannot activate history.
	if err := os.MkdirAll(filepath.Join(dir, "unrelated", ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	withArgs(t, "recalldock", "search", "needle", "--json")
	out, _ = captureOutput(t, func() { cmdSearch(cfg) })
	if strings.Contains(out, "unique-needle") || strings.Contains(out, "private-body-needle") {
		t.Fatal("auto-promoted child memory")
	}
}

func TestCLISearchJSONIncludesBoundedSelectedText(t *testing.T) {
	cfg := testConfig(t)
	withCwd(t, t.TempDir())
	mustSeedObservation(t, cfg, "selected-text", "fixture", "decision", "Visible selected title", "BODY_ONLY_UNSELECTED_SECRET", "project")
	withArgs(t, "recalldock", "search", "Visible", "--project", "fixture", "--detail", "l0", "--json")
	out, stderr := captureOutput(t, func() { cmdSearch(cfg) })
	var result struct {
		Results []struct {
			SelectedContent string `json:"selected_content"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || stderr != "" || len(result.Results) != 1 || !strings.Contains(result.Results[0].SelectedContent, "Visible selected title") {
		t.Fatalf("selected index text missing: %s %s", out, stderr)
	}
	if strings.Contains(out, "BODY_ONLY_UNSELECTED_SECRET") {
		t.Fatal("index included unselected body")
	}
}

func TestCLIReadExplicitScopeAndFlagValidation(t *testing.T) {
	for _, c := range []string{"search", "context", "stats"} {
		for _, tail := range [][]string{{"--all", "--project", "p"}, {"--project"}, {"--typo"}} {
			t.Run(c+strings.Join(tail, "_"), func(t *testing.T) {
				stubExitWithPanic(t)
				cfg := testConfig(t)
				cfg.DataDir = filepath.Join(cfg.DataDir, "not-created")
				args := []string{"recalldock", c}
				if c == "search" {
					args = append(args, "needle")
				}
				args = append(args, tail...)
				withArgs(t, args...)
				_, errout, p := captureOutputAndRecover(t, func() {
					switch c {
					case "search":
						cmdSearch(cfg)
					case "context":
						cmdContext(cfg)
					case "stats":
						cmdStats(cfg)
					}
				})
				if p == nil || errout == "" {
					t.Fatalf("bad flags accepted: %v %q", p, errout)
				}
				if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
					t.Fatalf("bad flags created database: %v", err)
				}
			})
		}
	}
}

func TestCLIReadRegisteredNestedAndExplicitHistorical(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	deep := filepath.Join(root, "a/b/c")
	if err := os.MkdirAll(deep, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.CreateRegisteredProject("Parent")
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.CreateRegisteredProject("Child")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindProjectRoot(p.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("scope-parent", p.ID, root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddObservation(store.AddObservationParams{SessionID: "scope-parent", Project: p.ID, Type: "decision", Title: "needle-parent", Content: "BODY_PARENT_ONLY"}); err != nil {
		t.Fatal(err)
	}
	withCwd(t, deep)
	withArgs(t, "recalldock", "stats", "--json")
	out, _ := captureOutput(t, func() { cmdStats(cfg) })
	if !strings.Contains(out, p.ID) || !strings.Contains(out, `"total_observations": 1`) {
		t.Fatalf("nested parent: %q", out)
	}
	if _, err := s.BindProjectRoot(q.ID, filepath.Join(root, "a/b")); err != nil {
		t.Fatal(err)
	}
	out, _ = captureOutput(t, func() { cmdStats(cfg) })
	if !strings.Contains(out, q.ID) || !strings.Contains(out, `"total_observations": 0`) {
		t.Fatalf("nested child: %q", out)
	}
	withArgs(t, "recalldock", "context", "--project", p.ID)
	out, _ = captureOutput(t, func() { cmdContext(cfg) })
	if !strings.Contains(out, "needle-parent") || strings.Contains(out, "BODY_PARENT_ONLY") {
		t.Fatalf("historical index: %q", out)
	}
	withArgs(t, "recalldock", "stats", "--all", "--json")
	out, _ = captureOutput(t, func() { cmdStats(cfg) })
	if !strings.Contains(out, `"total_observations": 1`) {
		t.Fatalf("all count: %q", out)
	}
}
