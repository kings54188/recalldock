package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOnlineBackupIncludesCommittedWALAndKeepsIdentity(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateSession("backup-session", "backup-project", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddObservation(AddObservationParams{SessionID: "backup-session", Project: "backup-project", Title: "Snapshot fixture", Content: "Only synthetic content", Type: "decision"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("UPDATE observations SET title='Committed WAL fixture'"); err != nil {
		t.Fatal(err)
	}
	backup, err := st.CreateSQLiteBackup()
	if err != nil {
		t.Fatal(err)
	}
	if backup.Integrity != "ok" || backup.Bytes == 0 || len(backup.SHA256) != 64 || !strings.HasPrefix(filepath.Base(backup.Path), "recalldock-backup-") {
		t.Fatalf("invalid metadata %+v", backup)
	}
	original, err := st.Identity()
	if err != nil || backup.StoreID != original.StoreID {
		t.Fatal("backup identity changed")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(backup.Path)
		if info.Mode().Perm() != 0600 {
			t.Fatalf("backup not private: %o", info.Mode().Perm())
		}
		dir, _ := os.Stat(filepath.Dir(backup.Path))
		if dir.Mode().Perm() != 0700 {
			t.Fatalf("directory not private: %o", dir.Mode().Perm())
		}
	}
	db, err := sql.Open("sqlite", backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title string
	if err := db.QueryRow("SELECT title FROM observations").Scan(&title); err != nil || title != "Committed WAL fixture" {
		t.Fatalf("lost WAL: %q %v", title, err)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(backup.Path), "*.partial"))
	if len(matches) != 0 {
		t.Fatal("left partial file")
	}
}

func TestBackupRefusesSymlinkAndCleansFailedSnapshot(t *testing.T) {
	st := newTestStore(t)
	outside := t.TempDir()
	dir := filepath.Join(st.cfg.DataDir, "backups")
	if err := os.Symlink(outside, dir); err == nil {
		if _, err := st.CreateSQLiteBackup(); err == nil {
			t.Fatal("followed backup directory symlink")
		}
		entries, _ := os.ReadDir(outside)
		if len(entries) != 0 {
			t.Fatal("wrote outside selected directory")
		}
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	old := st.hooks.exec
	st.hooks.exec = func(execer, string, ...any) (sql.Result, error) { return nil, errors.New("synthetic snapshot failure") }
	defer func() { st.hooks.exec = old }()
	if _, err := st.CreateSQLiteBackup(); err == nil {
		t.Fatal("snapshot failure hidden")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("failed backup left partial/success file")
	}
}

func TestBackupRejectsSharedDirectoryAndPreservesExistingFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permission bits are Unix-specific")
	}
	st := newTestStore(t)
	dir := filepath.Join(st.cfg.DataDir, "backups")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "precious.db")
	if err := os.WriteFile(keep, []byte("existing backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSQLiteBackup(); err == nil {
		t.Fatal("accepted shared backup directory")
	}
	body, _ := os.ReadFile(keep)
	if string(body) != "existing backup" {
		t.Fatal("changed prior backup")
	}
}
