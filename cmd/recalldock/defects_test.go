package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestUnboundSearchStillValidatesSelectors(t *testing.T) {
	cfg := testConfig(t)
	mustSeedObservation(t, cfg, "enum-fixture", "enum-project", "decision", "marker", "marker", "project")
	t.Chdir(t.TempDir())
	for _, option := range []struct{ key, value string }{
		{"--scope", "bogus"}, {"--type", "bogus_type"}, {"--strategy", "bogus"},
		{"--detail", "bogus"}, {"--profile", "bogus"}, {"--match-mode", "bogus"}, {"--root-path", "../bad"},
	} {
		for _, all := range []bool{false, true} {
			args := []string{"recalldock", "search", "marker", option.key, option.value, "--json"}
			if all {
				args = append(args, "--all")
			}
			withArgs(t, args...)
			out, stderr, code := captureExitPanic(t, func() { cmdSearch(cfg) })
			if code != 1 || out != "" || stderr == "" {
				t.Errorf("%s all=%v ignored: code=%d out=%q err=%q", option.key, all, code, out, stderr)
			}
		}
	}
	withArgs(t, "recalldock", "search", "marker", "--type", "decision", "--json")
	out, stderr, code := captureExitPanic(t, func() { cmdSearch(cfg) })
	if code != 0 || stderr != "" || !strings.Contains(out, "unbound_cwd") {
		t.Fatalf("valid unbound read changed: %d %q %q", code, out, stderr)
	}
	mustSeedObservation(t, cfg, "custom-type", "enum-project", "fixture_type", "custom marker", "custom marker", "project")
	withArgs(t, "recalldock", "search", "custom", "--type", "fixture_type", "--json")
	_, stderr, code = captureExitPanic(t, func() { cmdSearch(cfg) })
	if code != 1 || !strings.Contains(stderr, "select --project or --all") {
		t.Fatalf("custom type implicitly validated across projects: %d %q", code, stderr)
	}
	for _, selection := range [][]string{{"--all"}, {"--project", "enum-project"}} {
		args := append([]string{"recalldock", "search", "custom", "--type", "fixture_type", "--json"}, selection...)
		withArgs(t, args...)
		out, stderr, code = captureExitPanic(t, func() { cmdSearch(cfg) })
		if code != 0 || stderr != "" || !strings.Contains(out, "custom marker") {
			t.Fatalf("explicit custom type rejected: %d %q %q", code, out, stderr)
		}
	}
}

func TestListedCommandsHonorJSON(t *testing.T) {
	cfg := testConfig(t)
	id := mustSeedObservation(t, cfg, "json-timeline", "matrix-project", "decision", "marker", "marker", "project")
	_, _, relation := seedRelation(t, cfg, "matrix-project")
	db := openTestDB(t, cfg)
	var relationID string
	if err := db.QueryRow(`SELECT id FROM memory_relations WHERE sync_id=?`, relation).Scan(&relationID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		run  func(store.Config)
	}{
		{[]string{"timeline", fmt.Sprint(id), "--before", "0", "--after", "0", "--json"}, cmdTimeline},
		{[]string{"conflicts", "show", relationID, "--json"}, cmdConflicts},
		{[]string{"conflicts", "stats", "--project", "matrix-project", "--json"}, cmdConflicts},
		{[]string{"conflicts", "deferred", "--json"}, cmdConflicts},
		{[]string{"candidates", "list", "--all", "--json"}, cmdCandidates},
		{[]string{"projects", "registry", "--json"}, cmdProjects},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			withArgs(t, append([]string{"recalldock"}, tc.args...)...)
			out, stderr, code := captureExitPanic(t, func() { tc.run(cfg) })
			if code != 0 || !json.Valid([]byte(out)) {
				t.Fatalf("JSON flag ignored/rejected: code=%d out=%q err=%q", code, out, stderr)
			}
		})
	}
}

func TestDryRunCapDoesNotClaimResumableProgress(t *testing.T) {
	cfg := testConfig(t)
	for _, title := range []string{"shared source one", "shared source two", "shared source three"} {
		mustSeedObservation(t, cfg, "cap-fixture", "cap-project", "decision", title, title, "project")
	}
	withArgs(t, "recalldock", "conflicts", "scan", "--project", "cap-project", "--dry-run", "--max-insert", "1")
	out, stderr := captureOutput(t, func() { cmdConflicts(cfg) })
	if stderr != "" || strings.Contains(out, "Re-run to continue") || !strings.Contains(out, "larger --max-insert") || !strings.Contains(out, "does not persist progress") {
		t.Fatalf("dry-run cap gives an impossible continuation: %q %q", out, stderr)
	}
}

func TestReadListsRejectMalformedOptions(t *testing.T) {
	cfg := testConfig(t)
	mustSeedObservation(t, cfg, "flags", "flags-project", "decision", "marker", "marker", "project")
	for _, tc := range []struct {
		args []string
		run  func(store.Config)
	}{
		{[]string{"review", "list", "--porject", "flags-project"}, cmdReview},
		{[]string{"review", "list", "--Project", "flags-project"}, cmdReview},
		{[]string{"review", "list", "--project"}, cmdReview},
		{[]string{"review", "list", "--project", "--json"}, cmdReview},
		{[]string{"review", "list", "--status", "bogus"}, cmdReview},
		{[]string{"review", "list", "--limit", "0"}, cmdReview},
		{[]string{"review", "list", "--limit", "abc"}, cmdReview},
		{[]string{"review", "list", "--limit", "5000"}, cmdReview},
		{[]string{"review", "dependencies", "--porject", "flags-project"}, cmdReview},
		{[]string{"review", "resource-dependencies", "--porject", "flags-project"}, cmdReview},
		{[]string{"conflicts", "list", "--porject", "flags-project"}, cmdConflicts},
		{[]string{"conflicts", "list", "--limit", "abc"}, cmdConflicts},
		{[]string{"conflicts", "scan", "--project", "flags-project", "--apply", "--dry-run"}, cmdConflicts},
		{[]string{"conflicts", "scan", "--project", "flags-project", "--dry-run", "--apply"}, cmdConflicts},
		{[]string{"projects", "list", "--bogus"}, cmdProjects},
		{[]string{"projects", "lsit"}, cmdProjects},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			withArgs(t, append([]string{"recalldock"}, tc.args...)...)
			out, stderr, code := captureExitPanic(t, func() { tc.run(cfg) })
			if code != 1 || out != "" || stderr == "" {
				t.Fatalf("malformed options silently succeeded: code=%d stdout=%q stderr=%q", code, out, stderr)
			}
		})
	}
}

func TestProjectsAndConflictsListEmitJSON(t *testing.T) {
	cfg := testConfig(t)
	mustSeedObservation(t, cfg, "json-list", "json-project", "decision", "marker", "marker", "project")
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := s.AdoptLegacyProject("json-project")
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	withArgs(t, "recalldock", "projects", "list", "--json")
	out, stderr := captureOutput(t, func() { cmdProjects(cfg) })
	if stderr != "" || !json.Valid([]byte(out)) || !strings.Contains(out, "json-project") || !strings.Contains(out, registered.ID) {
		t.Fatalf("projects JSON: %q %q", out, stderr)
	}
	withArgs(t, "recalldock", "projects", "list")
	out, stderr = captureOutput(t, func() { cmdProjects(cfg) })
	if stderr != "" || !strings.Contains(out, "json-project") || !strings.Contains(out, "projects with sessions, observations or prompts") {
		t.Fatalf("registered name/count definition missing: %q %q", out, stderr)
	}
	withArgs(t, "recalldock", "conflicts", "list", "--project="+registered.ID, "--json")
	out, stderr = captureOutput(t, func() { cmdConflicts(cfg) })
	if stderr != "" || !json.Valid([]byte(out)) || !strings.Contains(out, `"relations": []`) {
		t.Fatalf("conflicts JSON: %q %q", out, stderr)
	}
}

func TestGatewayRegistryIsIndependentOfDataDir(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	defaultDir := filepath.Join(home, ".recalldock")
	if runtime.GOOS == "windows" {
		localAppData := filepath.Join(home, "AppData", "Local")
		t.Setenv("USERPROFILE", home)
		t.Setenv("LOCALAPPDATA", localAppData)
		defaultDir = filepath.Join(localAppData, "RecallDock")
	}
	t.Setenv("RECALLDOCK_DATA_DIR", data)
	t.Setenv("RECALLDOCK_GATEWAY_REGISTRY", "")
	if err := os.WriteFile(filepath.Join(data, "gateway.json"), []byte("invalid registry"), 0600); err != nil {
		t.Fatal(err)
	}
	out, stderr := captureOutput(t, func() {
		if err := runGateway([]string{"list"}); err != nil {
			t.Errorf("registry followed data dir: %v", err)
		}
	})
	if strings.TrimSpace(out) != "[]" || stderr != "" {
		t.Fatalf("list: %q %q", out, stderr)
	}
	if _, err := os.Stat(defaultDir); !os.IsNotExist(err) {
		t.Fatal("read-only gateway list created the registry directory")
	}
	definition := filepath.Join(home, "definition.json")
	body, err := json.Marshal(map[string]any{"description": "Synthetic metadata", "source": map[string]any{"path": filepath.Join(home, "original.json"), "format": "json", "keys": []string{"fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definition, body, 0600); err != nil {
		t.Fatal(err)
	}
	captureOutput(t, func() {
		if err := runGateway([]string{"add", "--server", "synthetic", "--input", definition}); err != nil {
			t.Errorf("first gateway add: %v", err)
		}
	})
	info, err := os.Stat(defaultDir)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0700) {
		t.Fatalf("new registry directory must be private: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(data, "recalldock.db")); !os.IsNotExist(err) {
		t.Fatal("gateway add created a knowledge database")
	}
	t.Setenv("RECALLDOCK_GATEWAY_REGISTRY", "relative.json")
	if err := runGateway([]string{"list"}); err == nil {
		t.Fatal("relative registry env accepted")
	}
}
