package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestCLIObsidianExportsRealStoreRelationship(t *testing.T) {
	cfg := testConfig(t)
	a := mustSeedObservation(t, cfg, "graph-a", "p", "decision", "Source", "Source fixture", "project")
	b := mustSeedObservation(t, cfg, "graph-b", "p", "decision", "Target", "Target fixture", "project")
	s, err := store.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	one, _ := s.GetObservation(a)
	two, _ := s.GetObservation(b)
	content := "See [[memory:" + two.SyncID + "|the selected target]]. Keep [[User note]]."
	if _, err := s.UpdateObservation(a, store.UpdateObservationParams{Content: &content}); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	resource := filepath.Join(source, "diagram.png")
	if err := os.WriteFile(resource, []byte("Synthetic attachment bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := hashResourceFile(resource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddResourceDependency(store.AddResourceDependencyParams{SourceRef: identity.SourceRef, DependentObservationID: a, Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRelation(store.SaveRelationParams{SyncID: "rel-cli-fixture", SourceID: one.SyncID, TargetID: two.SyncID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JudgeRelation(store.JudgeRelationParams{JudgmentID: "rel-cli-fixture", Relation: store.RelationSupersedes, MarkedByActor: "operator", MarkedByKind: "human"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	vault := t.TempDir()
	withArgs(t, "recalldock", "obsidian-export", "--vault", vault, "--project", "p", "--attachment-root", source)
	_, stderr := captureOutput(t, func() { cmdObsidianExport(cfg) })
	if stderr != "" {
		t.Fatal(stderr)
	}
	paths, err := filepath.Glob(filepath.Join(vault, "recalldock/p/decision/*.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		found = found || strings.Contains(string(body), "supersedes (judged)")
	}
	if !found {
		t.Fatal("actual CLI adapter hid relationship reader from exporter")
	}
	for _, path := range paths {
		body, _ := os.ReadFile(path)
		if strings.Contains(string(body), "[[memory:") {
			t.Fatal("actual CLI did not rewrite stable body link")
		}
	}
	assets, err := filepath.Glob(filepath.Join(vault, "recalldock/_attachments/*.png"))
	if err != nil || len(assets) != 1 {
		t.Fatal("actual CLI did not export registered attachment")
	}
	asset, err := os.ReadFile(assets[0])
	if err != nil || string(asset) != "Synthetic attachment bytes" {
		t.Fatal("attachment bytes mismatch")
	}
	withArgs(t, "recalldock", "obsidian-export", "--vault", vault, "--project", "P", "--attachment-root", source)
	_, stderr = captureOutput(t, func() { cmdObsidianExport(cfg) })
	if stderr != "" {
		t.Fatal("normalized selector changed projection scope", stderr)
	}
}

func TestObsidianMalformedSelectionFailsBeforeStoreAndVault(t *testing.T) {
	for _, tail := range [][]string{{"--project", ""}, {"--project", "--force"}, {"--project", "p", "--project", "q"}, {"--attachment-root", "relative"}, {"--limit", "invalid"}} {
		t.Run(strings.Join(tail, "_"), func(t *testing.T) {
			stubExitWithPanic(t)
			cfg := testConfig(t)
			cfg.DataDir = filepath.Join(t.TempDir(), "absent")
			vault := filepath.Join(t.TempDir(), "absent")
			withArgs(t, append([]string{"recalldock", "obsidian-export", "--vault", vault}, tail...)...)
			_, stderr, p := captureOutputAndRecover(t, func() { cmdObsidianExport(cfg) })
			if p == nil || stderr == "" {
				t.Fatal("invalid selector was accepted")
			}
			for _, path := range []string{cfg.DataDir, vault} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("invalid selector created files", path)
				}
			}
		})
	}
}
