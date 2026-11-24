package obsidian

import (
	"os"
	"path/filepath"
	"testing"

	"recalldock.local/recalldock/internal/filelock"
	"recalldock.local/recalldock/internal/store"
)

func TestExportPreservesManualEditsIncludingForceAndDelete(t *testing.T) {
	for _, mode := range []string{"update", "force", "delete", "hub"} {
		t.Run(mode, func(t *testing.T) {
			vault := t.TempDir()
			project := "p"
			ms := &mockStore{exportData: &store.ExportData{Observations: []store.Observation{{ID: 1, SessionID: "s", Type: "decision", Title: "One", Content: "original", Project: &project}}}, stats: &store.Stats{}}
			e := NewExporter(ms, ExportConfig{VaultPath: vault})
			if r, err := e.Export(); err != nil || len(r.Errors) != 0 {
				t.Fatalf("first export: %+v %v", r, err)
			}
			rel := "p/decision/one-1.md"
			if mode == "hub" {
				rel = "_sessions/s.md"
			}
			path := filepath.Join(vault, "recalldock", rel)
			if err := os.WriteFile(path, []byte("user annotation: preserve me"), 0600); err != nil {
				t.Fatal(err)
			}
			ms.exportData.Observations[0].Content = "new source revision"
			if mode == "force" {
				e.config.Force = true
			}
			if mode == "delete" {
				deleted := "2026-01-01"
				ms.exportData.Observations[0].DeletedAt = &deleted
			}
			r, err := e.Export()
			if err == nil && len(r.Errors) == 0 {
				t.Fatal("manual edit must produce a conflict")
			}
			body, err := os.ReadFile(path)
			if err != nil || string(body) != "user annotation: preserve me" {
				t.Fatal("manual edit was overwritten/deleted")
			}
		})
	}
}

type identifiedMockStore struct {
	*mockStore
	id string
}

func (s *identifiedMockStore) Identity() (store.StoreIdentity, error) {
	return store.StoreIdentity{StoreID: s.id}, nil
}

func TestVaultRejectsDifferentDatabaseEvenWithForce(t *testing.T) {
	vault := t.TempDir()
	ms := &identifiedMockStore{mockStore: &mockStore{exportData: &store.ExportData{}, stats: &store.Stats{}}, id: "store-one"}
	e := NewExporter(ms, ExportConfig{VaultPath: vault})
	if _, err := e.Export(); err != nil {
		t.Fatal(err)
	}
	ms.id = "store-two"
	e.config.Force = true
	if _, err := e.Export(); err == nil {
		t.Fatal("another database must not take over this projection")
	}
}

func TestVaultRefusesScopeChangeAndPartialExport(t *testing.T) {
	vault := t.TempDir()
	ms := &mockStore{exportData: &store.ExportData{}, stats: &store.Stats{}}
	e := NewExporter(ms, ExportConfig{VaultPath: vault, Project: "one"})
	if _, err := e.Export(); err != nil {
		t.Fatal(err)
	}
	e.config.Project = "two"
	if _, err := e.Export(); err == nil {
		t.Fatal("changing scope would silently truncate shared hubs")
	}
	e.config.VaultPath = t.TempDir()
	e.config.Limit = 1
	if _, err := e.Export(); err == nil {
		t.Fatal("partial export must not silently ignore limit or generate misleading hubs")
	}
}

func TestExportLockRefusesAnotherWriter(t *testing.T) {
	vault := t.TempDir()
	root := filepath.Join(vault, "recalldock")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	// The lock is advisory now, so "another writer" must actually hold the OS
	// lock. A file that merely exists no longer means "locked": tolerating a
	// stale lock file left by a crashed process is the point of the change.
	held, err := filelock.Acquire(filepath.Join(root, ".export.lock"))
	if err != nil {
		t.Fatalf("hold lock as another writer: %v", err)
	}
	defer held.Release()

	e := NewExporter(&mockStore{exportData: &store.ExportData{}, stats: &store.Stats{}}, ExportConfig{VaultPath: vault})
	if _, err := e.Export(); err == nil {
		t.Fatal("must not ignore another process's lock")
	}
}

func TestStableNotePathSurvivesTitleAndTypeChanges(t *testing.T) {
	vault := t.TempDir()
	project := "p"
	ms := &mockStore{exportData: &store.ExportData{Observations: []store.Observation{{ID: 1, SyncID: "obs-abc", SessionID: "s", Type: "decision", Title: "Original title", Project: &project}}}, stats: &store.Stats{}}
	e := NewExporter(ms, ExportConfig{VaultPath: vault})
	mustExport := func() {
		t.Helper()
		r, err := e.Export()
		if err != nil || len(r.Errors) != 0 {
			t.Fatalf("export: %+v %v", r, err)
		}
	}
	mustExport()
	path := filepath.Join(vault, "recalldock/p/decision/memory-obs-abc.md")
	ms.exportData.Observations[0].Title = "Renamed title"
	mustExport()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("title edit must retain stable note filename")
	}
	ms.exportData.Observations[0].Type = "procedure"
	mustExport()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("old managed type path should be removed after relocation")
	}
	if _, err := os.Stat(filepath.Join(vault, "recalldock/p/procedure/memory-obs-abc.md")); err != nil {
		t.Fatal(err)
	}
}
