package obsidian

import (
	"os"
	"path/filepath"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestExportRejectsSymlinkNamespace(t *testing.T) {
	vault, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(vault, "recalldock")); err != nil {
		t.Fatal(err)
	}
	e := NewExporter(&mockStore{exportData: &store.ExportData{}, stats: &store.Stats{}}, ExportConfig{VaultPath: vault})
	if _, err := e.Export(); err == nil {
		t.Fatal("symlink namespace must be rejected")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("export wrote through symlink namespace")
	}
}

func TestExportRejectsTamperedStateDeletion(t *testing.T) {
	vault := t.TempDir()
	outside := filepath.Join(vault, "user-note.md")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(vault, "recalldock")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(filepath.Join(root, ".recalldock-sync-state.json"), SyncState{Files: map[int64]string{1: "../user-note.md"}}); err != nil {
		t.Fatal(err)
	}
	deleted := "2026-01-01"
	e := NewExporter(&mockStore{exportData: &store.ExportData{Observations: []store.Observation{{ID: 1, DeletedAt: &deleted}}}, stats: &store.Stats{}}, ExportConfig{VaultPath: vault})
	r, err := e.Export()
	if err == nil && len(r.Errors) == 0 {
		t.Fatal("tampered state path must be rejected")
	}
	body, err := os.ReadFile(outside)
	if err != nil || string(body) != "keep" {
		t.Fatal("unmanaged file was touched")
	}
}

func TestGraphConfigRejectsSymlinkDirectory(t *testing.T) {
	vault, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(vault, ".obsidian")); err != nil {
		t.Fatal(err)
	}
	if err := WriteGraphConfig(vault, GraphConfigPreserve); err == nil {
		t.Fatal("graph config must not write through a symlink directory")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("outside directory was modified")
	}
}
