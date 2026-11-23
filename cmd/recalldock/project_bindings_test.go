package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestCLIProjectBindingSelectionAndPreflight(t *testing.T) {
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateRegisteredProject("Selected binding")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	b, err := s.BindProjectRoot(p.ID, root)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	var runErr error
	out, stderr := captureOutput(t, func() { runErr = runProjectBindings(cfg, []string{"bindings", p.ID}) })
	var entries []store.ProjectBindingState
	if runErr != nil || stderr != "" || json.Unmarshal([]byte(out), &entries) != nil || len(entries) != 1 || entries[0].ID != b.ID {
		t.Fatalf("index: %s %v", out, runErr)
	}
	out, _ = captureOutput(t, func() {
		runErr = runProjectBindings(cfg, []string{"unbind", p.ID, "--directory", root, "--binding", b.ID})
	})
	var result store.ProjectBindingState
	if runErr != nil || json.Unmarshal([]byte(out), &result) != nil || result.State != "superseded" {
		t.Fatalf("unbind: %s %v", out, runErr)
	}
	fresh := cfg
	fresh.DataDir = filepath.Join(t.TempDir(), "not-created")
	if err := runProjectBindings(fresh, []string{"unbind", p.ID, "--directory", "relative", "--binding", b.ID}); err == nil {
		t.Fatal("invalid path accepted")
	}
	if _, err := os.Stat(fresh.DataDir); !os.IsNotExist(err) {
		t.Fatal("invalid command initialized database")
	}
}
