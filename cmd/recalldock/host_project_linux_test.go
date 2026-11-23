//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestAutomaticHostProjectsConcurrentAndIsolated(t *testing.T) {
	cfg := testConfig(t)
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := t.TempDir()
	a := filepath.Join(base, "a", "same")
	b := filepath.Join(base, "b", "same")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const n = 8
	ids := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d, e := ensureHostProject(s, cfg.DataDir, a); ids <- d.Project; errs <- e }()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("different identities for same root")
		}
	}
	other, err := ensureHostProject(s, cfg.DataDir, b)
	if err != nil || first == "" || other.Project == first {
		t.Fatalf("different roots did not isolate: %v", err)
	}
	projects, err := s.ListRegisteredProjects()
	if err != nil || len(projects) != 2 {
		t.Fatalf("duplicate/orphan projects: %d %v", len(projects), err)
	}
	child := filepath.Join(a, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	inherited, err := ensureHostProject(s, cfg.DataDir, child)
	if err != nil || inherited.Project != first || inherited.Path != a {
		t.Fatalf("nested cwd did not reuse its verified scope: %+v %v", inherited, err)
	}
	marker, err := os.ReadFile(filepath.Join(a, ".recalldock", "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(base, "clone")
	if err := os.MkdirAll(filepath.Join(clone, ".recalldock"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".recalldock", "root.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureHostProject(s, cfg.DataDir, clone); err == nil {
		t.Fatal("copied marker accepted")
	}
	after, _ := os.ReadFile(filepath.Join(clone, ".recalldock", "root.json"))
	if string(after) != string(marker) {
		t.Fatal("copied marker overwritten")
	}
}

func TestAutomaticHostHookCreatesAndEndsSession(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	event := func(name string) *bytes.Reader {
		b, err := json.Marshal(map[string]string{"session_id": "synthetic-autoproject", "cwd": root, "hook_event_name": name})
		if err != nil {
			t.Fatal(err)
		}
		return bytes.NewReader(b)
	}
	var out bytes.Buffer
	if err := runHostEvent(cfg, []string{"codex", "--auto-project"}, event("SessionStart"), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Fatal("missing startup context")
	}
	if err := runHostEvent(cfg, []string{"codex", "--auto-project"}, event("SessionEnd"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	session, err := s.GetSession("codex-synthetic-autoproject")
	if err != nil || session.EndedAt == nil || session.Directory != root || session.Summary != nil {
		t.Fatalf("bad session: %+v %v", session, err)
	}
}
