package store

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateLegacyDatabaseOpensOnlyACopy(t *testing.T) {
	path := os.Getenv("RECALLDOCK_LEGACY_DB")
	if path == "" {
		t.Skip("set RECALLDOCK_LEGACY_DB to a private consistent SQLite snapshot")
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal("cannot open private SQLite fixture")
	}
	defer source.Close()
	cfg := mustDefaultConfig(t)
	cfg.DataDir = t.TempDir()
	target, err := os.OpenFile(filepath.Join(cfg.DataDir, "recalldock.db"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatal("cannot copy private SQLite fixture")
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal("cannot open/migrate the temporary legacy copy")
	}
	defer s.Close()
	var integrity string
	if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("legacy copy failed integrity check")
	}
	stats, err := s.Stats()
	if err != nil || stats.TotalObservations == 0 {
		t.Fatal("legacy copy lost its observations")
	}
	t.Logf("legacy SQLite copy: observations=%d sessions=%d prompts=%d; integrity=ok", stats.TotalObservations, stats.TotalSessions, stats.TotalPrompts)
}
