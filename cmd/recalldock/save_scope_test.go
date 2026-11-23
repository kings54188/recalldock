package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestSaveUnboundRequiresExplicitProjectBeforeDatabaseCreation(t *testing.T) {
	stubExitWithPanic(t)
	root := t.TempDir()
	withCwd(t, root)
	cfg := testConfig(t)
	cfg.DataDir = filepath.Join(cfg.DataDir, "absent")
	// A nearby single repository is not authority to choose a write scope.
	if err := os.MkdirAll(filepath.Join(root, "child-repo/.git"), 0700); err != nil {
		t.Fatal(err)
	}
	withArgs(t, "recalldock", "save", "Durable title", "Durable statement")
	_, stderr, p := captureOutputAndRecover(t, func() { cmdSave(cfg) })
	if p == nil || !strings.Contains(stderr, "explicit project") {
		t.Fatalf("unbound write accepted: %v %q", p, stderr)
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatal("unbound default write initialized DB")
	}
}

func TestSaveDefaultProjectMatchesExplicitLocalConfigurationAndReadScope(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	withCwd(t, root)
	if err := os.Mkdir(filepath.Join(root, ".recalldock"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".recalldock/config.json"), []byte(`{"project_name":"declared-project"}`), 0600); err != nil {
		t.Fatal(err)
	}
	withArgs(t, "recalldock", "save", "Configured durable title", "Scoped durable body")
	out, stderr := captureOutput(t, func() { cmdSave(cfg) })
	if stderr != "" || !strings.Contains(out, "declared-project") {
		t.Fatalf("save scope not shown: %q %q", out, stderr)
	}
	withArgs(t, "recalldock", "stats", "--json")
	out, stderr = captureOutput(t, func() { cmdStats(cfg) })
	var response struct {
		Project string      `json:"project"`
		Stats   store.Stats `json:"stats"`
	}
	if json.Unmarshal([]byte(out), &response) != nil || response.Project != "declared-project" || response.Stats.TotalObservations != 1 || stderr != "" {
		t.Fatalf("saved into hidden/empty project: %q %q", out, stderr)
	}
	st, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	data, err := st.Export()
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Observations) != 1 || data.Observations[0].Project == nil || *data.Observations[0].Project != "declared-project" {
		t.Fatal("configured write did not select project")
	}
}

func TestSaveExplicitLegacyProjectDoesNotScanOrPromoteChildren(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	withCwd(t, root)
	for _, child := range []string{"repo-one", "repo-two"} {
		if err := os.MkdirAll(filepath.Join(root, child, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	withArgs(t, "recalldock", "save", "Selected old project", "Durable explicit operator statement", "--project", "legacy-project")
	out, stderr := captureOutput(t, func() { cmdSave(cfg) })
	if stderr != "" || !strings.Contains(out, "Memory saved:") {
		t.Fatalf("explicit choice lost to discovery: %q %q", out, stderr)
	}
}

func TestSaveConfigCannotImpersonateRegisteredBinding(t *testing.T) {
	stubExitWithPanic(t)
	cfg := testConfig(t)
	cfg.DataDir = filepath.Join(cfg.DataDir, "absent")
	root := t.TempDir()
	withCwd(t, root)
	if err := os.Mkdir(filepath.Join(root, ".recalldock"), 0700); err != nil {
		t.Fatal(err)
	}
	fake := "prj-" + strings.Repeat("a", 32)
	body, _ := json.Marshal(map[string]string{"project_name": fake})
	if err := os.WriteFile(filepath.Join(root, ".recalldock/config.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	withArgs(t, "recalldock", "save", "No binding", "Must not be saved")
	_, stderr, p := captureOutputAndRecover(t, func() { cmdSave(cfg) })
	if p == nil || !strings.Contains(stderr, "matching bound directory") {
		t.Fatalf("config identity accepted: %v %q", p, stderr)
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatal("config impersonation initialized a store")
	}
}
