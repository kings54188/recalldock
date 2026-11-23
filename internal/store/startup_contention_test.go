package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExistingStoreOpenNeverCreatesMissingDirectoryOrDatabase(t *testing.T) {
	for _, missingDir := range []bool{false, true} {
		cfg := mustDefaultConfig(t)
		cfg.DataDir = t.TempDir()
		cfg.RequireExisting = true
		if missingDir {
			cfg.DataDir = filepath.Join(cfg.DataDir, "typo")
		}
		s, err := New(cfg)
		if s != nil {
			s.Close()
		}
		if err == nil || !errors.Is(err, ErrStoreNotInitialized) {
			t.Fatalf("missing store accepted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(cfg.DataDir, "recalldock.db")); !os.IsNotExist(err) {
			t.Fatalf("read created database: %v", err)
		}
		if missingDir {
			if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
				t.Fatalf("read created directory: %v", err)
			}
		}
	}
}

func TestOpenWaitsForExistingWriterBeforeEnablingWAL(t *testing.T) {
	cfg := mustDefaultConfig(t)
	cfg.DataDir = t.TempDir()
	blocker, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "recalldock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	blocker.SetMaxOpenConns(1)
	if _, err := blocker.Exec("CREATE TABLE startup_fixture (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			blocker.Exec("ROLLBACK")
		}
	}()
	done := make(chan error, 1)
	go func() {
		s, err := New(cfg)
		if s != nil {
			s.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("startup did not wait for the active writer: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := blocker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	held = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("startup failed after writer released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startup did not finish after writer released")
	}
}
