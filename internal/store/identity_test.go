package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestStoreIdentityStableAcrossReopenAndDistinctAcrossStores(t *testing.T) {
	dataDir := t.TempDir()
	cfg := FallbackConfig(dataDir)

	first, err := New(cfg)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	firstIdentity, err := first.Identity()
	if err != nil {
		first.Close()
		t.Fatalf("read first identity: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	if firstIdentity.StoreID == "" {
		t.Fatal("store_id must not be empty")
	}
	wantDataDir := filepath.Clean(dataDir)
	if resolved, err := filepath.EvalSymlinks(dataDir); err == nil {
		wantDataDir = normalizePath(resolved)
	}
	if firstIdentity.DataDir != wantDataDir {
		t.Fatalf("data_dir = %q; want %q", firstIdentity.DataDir, wantDataDir)
	}
	if firstIdentity.DatabasePath != filepath.Join(wantDataDir, "recalldock.db") {
		t.Fatalf("database_path = %q; want database inside data_dir", firstIdentity.DatabasePath)
	}

	reopened, err := New(cfg)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	reopenedIdentity, err := reopened.Identity()
	if err != nil {
		t.Fatalf("read reopened identity: %v", err)
	}
	if reopenedIdentity.StoreID != firstIdentity.StoreID {
		t.Fatalf("store_id changed across reopen: %q -> %q", firstIdentity.StoreID, reopenedIdentity.StoreID)
	}

	other, err := New(FallbackConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("open other store: %v", err)
	}
	defer other.Close()
	otherIdentity, err := other.Identity()
	if err != nil {
		t.Fatalf("read other identity: %v", err)
	}
	if otherIdentity.StoreID == firstIdentity.StoreID {
		t.Fatalf("independent stores share store_id %q", firstIdentity.StoreID)
	}
}

func TestNewRejectsUnexpectedStoreID(t *testing.T) {
	dataDir := t.TempDir()
	cfg := FallbackConfig(dataDir)
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	identity, err := s.Identity()
	if err != nil {
		s.Close()
		t.Fatalf("read identity: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	cfg.ExpectedStoreID = "store-not-the-active-store"
	_, err = New(cfg)
	if !errors.Is(err, ErrStoreIdentityMismatch) {
		t.Fatalf("New error = %v; want ErrStoreIdentityMismatch", err)
	}
	var mismatch *StoreIdentityMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("New error type = %T; want *StoreIdentityMismatchError", err)
	}
	if mismatch.Expected != cfg.ExpectedStoreID || mismatch.Actual != identity.StoreID {
		t.Fatalf("mismatch = %+v; want expected=%q actual=%q", mismatch, cfg.ExpectedStoreID, identity.StoreID)
	}
}

func TestNewWithExpectedStoreIDDoesNotMigrateUnidentifiedStore(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "recalldock.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE legacy_sentinel (id INTEGER PRIMARY KEY)`); err != nil {
		db.Close()
		t.Fatalf("seed legacy store: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy store: %v", err)
	}

	cfg := FallbackConfig(dataDir)
	cfg.ExpectedStoreID = "store-expected"
	_, err = New(cfg)
	if !errors.Is(err, ErrStoreIdentityMismatch) {
		t.Fatalf("New error = %v; want ErrStoreIdentityMismatch", err)
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen legacy store: %v", err)
	}
	defer db.Close()
	var metadataTables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'store_metadata'`).Scan(&metadataTables); err != nil {
		t.Fatalf("inspect legacy store: %v", err)
	}
	if metadataTables != 0 {
		t.Fatal("expected-store preflight must fail before creating store_metadata")
	}
}

func TestApplyProjectMappingsRequiresStoreHandshakeAndRejectsChains(t *testing.T) {
	s := newTestStore(t)
	identity, err := s.Identity()
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}

	if _, err := s.ApplyProjectMappings("wrong-store", []ProjectMapping{{Source: "old", Target: "new"}}); !errors.Is(err, ErrStoreIdentityMismatch) {
		t.Fatalf("wrong-store error = %v; want ErrStoreIdentityMismatch", err)
	}
	for _, project := range []string{"alpha", "beta"} {
		if err := s.CreateSession("mapping-chain-"+project, project, "/tmp/"+project); err != nil {
			t.Fatalf("create chain fixture %q: %v", project, err)
		}
	}

	_, err = s.ValidateProjectMappings(identity.StoreID, []ProjectMapping{
		{Source: "alpha", Target: "beta"},
		{Source: "beta", Target: "gamma"},
	})
	if !errors.Is(err, ErrInvalidProjectMapping) {
		t.Fatalf("chain error = %v; want ErrInvalidProjectMapping", err)
	}
	if exists, err := s.ProjectExists("alpha"); err != nil || !exists {
		t.Fatalf("invalid preview mutated alpha: exists=%v err=%v", exists, err)
	}
	if _, err := s.ValidateProjectMappings(identity.StoreID, []ProjectMapping{
		{Source: "alpha", Target: "canonical"},
		{Source: "ALPHA", Target: "other"},
	}); !errors.Is(err, ErrInvalidProjectMapping) {
		t.Fatalf("normalized duplicate error = %v; want ErrInvalidProjectMapping", err)
	}
}

func TestApplyProjectMappingsMovesExplicitSources(t *testing.T) {
	s := newTestStore(t)
	for _, item := range []struct {
		session string
		project string
	}{
		{session: "mapping-alpha", project: "alpha-old"},
		{session: "mapping-beta", project: "beta-old"},
	} {
		if err := s.CreateSession(item.session, item.project, "/tmp/"+item.project); err != nil {
			t.Fatalf("create %s: %v", item.session, err)
		}
		if _, err := s.AddObservation(AddObservationParams{
			SessionID: item.session,
			Type:      "manual",
			Title:     item.project,
			Content:   "mapping fixture",
			Project:   item.project,
		}); err != nil {
			t.Fatalf("add %s: %v", item.project, err)
		}
	}
	identity, err := s.Identity()
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}

	result, err := s.ApplyProjectMappings(identity.StoreID, []ProjectMapping{
		{Source: "alpha-old", Target: "canonical"},
		{Source: "beta-old", Target: "canonical"},
	})
	if err != nil {
		t.Fatalf("apply mappings: %v", err)
	}
	if result.ObservationsUpdated != 2 || result.SessionsUpdated != 2 {
		t.Fatalf("result = %+v; want two observations and sessions moved", result)
	}
	for _, old := range []string{"alpha-old", "beta-old"} {
		if exists, err := s.ProjectExists(old); err != nil || exists {
			t.Fatalf("old project %q exists=%v err=%v", old, exists, err)
		}
	}
	if exists, err := s.ProjectExists("canonical"); err != nil || !exists {
		t.Fatalf("canonical exists=%v err=%v", exists, err)
	}
}
