package obsidian

import (
	"os"
	"path/filepath"
	"testing"

	"recalldock.local/recalldock/internal/store"
)

func TestIndependentVaultExportDoesNotTouchLegacyFolder(t *testing.T) {
	vault := t.TempDir()
	legacy := filepath.Join(vault, "engram", "user-note.md")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("Keep this existing user note unchanged."), 0o600); err != nil {
		t.Fatal(err)
	}
	exporter := NewExporter(&mockStore{exportData: &store.ExportData{}, stats: &store.Stats{}}, ExportConfig{VaultPath: vault, GraphConfig: GraphConfigSkip})
	if _, err := exporter.Export(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(vault, "recalldock")); err != nil || !info.IsDir() {
		t.Fatal("export must use the independent recalldock namespace")
	}
	content, err := os.ReadFile(legacy)
	if err != nil || string(content) != "Keep this existing user note unchanged." {
		t.Fatal("export modified the legacy vault folder")
	}
}
