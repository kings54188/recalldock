package store

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Check the library used by database/sql, not a separately installed sqlite3
// CLI. The multi-process WAL store requires the WAL-reset fix in 3.51.3+.
func TestSQLiteRuntimeHasWALResetFix(t *testing.T) {
	s := newTestStore(t)
	var version string
	if err := s.db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		t.Fatalf("unrecognized SQLite runtime version %q", version)
	}
	var v [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			t.Fatalf("unrecognized SQLite runtime version %q", version)
		}
		v[i] = n
	}
	if v[0] < 3 || (v[0] == 3 && (v[1] < 51 || (v[1] == 51 && v[2] < 3))) {
		t.Fatalf("SQLite runtime %s lacks the required 3.51.3+ WAL-reset fix", version)
	}
	t.Logf("actual SQLite runtime: %s", version)
}

func TestDefaultConfigUsesIndependentDataDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("ENGRAM_DATA_DIR", filepath.Join(home, "legacy-must-not-be-used"))
	cfg, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultDataDirForHome(home)
	if cfg.DataDir != want {
		t.Fatalf("default data directory must be independent, got %q want %q", cfg.DataDir, want)
	}
}
