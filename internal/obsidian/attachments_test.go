package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestAttachmentsRequireExplicitRootAndMatchingRegisteredBytes(t *testing.T) {
	source := t.TempDir()
	file := filepath.Join(source, "diagram.png")
	body := []byte("Synthetic image bytes")
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	p := "p"
	ref := "file://" + file + "@sha256:" + contentDigest(body)
	data := &store.ExportData{Observations: []store.Observation{{ID: 1, SyncID: "obs-one", Type: "decision", Title: "One", Content: "Knowledge body", Project: &p}}, ResourceDependencies: []store.ResourceDependency{{SourceRef: ref, DependentObservationSyncID: "obs-one", Project: p, Status: "active"}}}
	ms := &mockStore{exportData: data, stats: &store.Stats{}}
	vault := t.TempDir()
	e := NewExporter(ms, ExportConfig{VaultPath: vault, Project: p})
	result, err := e.Export()
	if err != nil || len(result.Errors) != 0 {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(vault, "recalldock/_attachments")); !os.IsNotExist(err) {
		t.Fatal("default export read/copied attachments")
	}
	e.config.AttachmentRoot = source
	result, err = e.Export()
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("attachment export: %+v %v", result, err)
	}
	target := filepath.Join(vault, "recalldock/_attachments", contentDigest(body)+".png")
	copied, err := os.ReadFile(target)
	if err != nil || string(copied) != string(body) {
		t.Fatal("attachment bytes not preserved")
	}
	note, _ := os.ReadFile(filepath.Join(vault, "recalldock/p/decision/memory-obs-one.md"))
	if !strings.Contains(string(note), fmt.Sprintf("[[recalldock/_attachments/%s.png|diagram.png]]", contentDigest(body))) {
		t.Fatal("attachment link missing")
	}
	if err := os.WriteFile(file, []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = e.Export()
	if err == nil && len(result.Errors) == 0 {
		t.Fatal("changed source hash silently accepted")
	}
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("user annotation"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = e.Export()
	if err == nil && len(result.Errors) == 0 {
		t.Fatal("modified target silently overwritten")
	}
	copied, _ = os.ReadFile(target)
	if string(copied) != "user annotation" {
		t.Fatal("user attachment overwritten")
	}
}

func TestAttachmentSelectionRejectsEscapesAndSymlinks(t *testing.T) {
	source := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private.png")
	body := []byte("Synthetic outside data")
	if err := os.WriteFile(outside, body, 0600); err != nil {
		t.Fatal(err)
	}
	p := "p"
	obs := store.Observation{ID: 1, SyncID: "obs-one", Project: &p}
	for _, path := range []string{outside, filepath.Join(source, "linked.png")} {
		if path != outside {
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		}
		data := &store.ExportData{Observations: []store.Observation{obs}, ResourceDependencies: []store.ResourceDependency{{SourceRef: "file://" + path + "@sha256:" + contentDigest(body), DependentObservationSyncID: obs.SyncID, Project: p, Status: "active"}}}
		e := NewExporter(&mockStore{exportData: data, stats: &store.Stats{}}, ExportConfig{VaultPath: t.TempDir(), Project: p, AttachmentRoot: source})
		r, err := e.Export()
		if err == nil && len(r.Errors) == 0 {
			t.Fatal("unsafe source accepted")
		}
	}
}
