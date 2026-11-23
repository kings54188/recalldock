package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type SQLiteBackup struct {
	Path      string `json:"path"`
	StoreID   string `json:"store_id"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	Integrity string `json:"integrity"`
}

// BackupSQLite remains the trusted maintenance API; it now uses exactly the
// same verified, private online snapshot path as the operator command.
func (s *Store) BackupSQLite() (string, error) {
	backup, err := s.CreateSQLiteBackup()
	return backup.Path, err
}

// CreateSQLiteBackup never copies a live .db without its WAL. VACUUM INTO is a
// consistent SQLite read snapshot; publish only after verification and fsync.
// https://sqlite.org/lang_vacuum.html#vacuuminto
func (s *Store) CreateSQLiteBackup() (SQLiteBackup, error) {
	var result SQLiteBackup
	dir, err := filepath.Abs(filepath.Join(s.cfg.DataDir, "backups"))
	if err != nil {
		return result, err
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return result, fmt.Errorf("create private backup directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return result, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return result, fmt.Errorf("backup directory must be a real private directory (0700), not a symlink or shared path")
	}
	workDir, err := os.MkdirTemp(dir, ".recalldock-backup-work-*")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(workDir)
	temp := filepath.Join(workDir, "snapshot.db")
	if _, err := s.execHook(s.db, "VACUUM INTO ?", temp); err != nil {
		return result, fmt.Errorf("online SQLite snapshot: %w", err)
	}
	if err := os.Chmod(temp, 0600); err != nil {
		return result, err
	}
	absolute, err := filepath.Abs(temp)
	if err != nil {
		return result, err
	}
	db, err := sql.Open("sqlite", sqliteFileURI(absolute, "mode=ro&_pragma=query_only(1)"))
	if err != nil {
		return result, err
	}
	db.SetMaxOpenConns(1)
	result.StoreID, err = verifyBackup(db)
	closeErr := db.Close()
	if err != nil {
		return result, err
	}
	if closeErr != nil {
		return result, closeErr
	}
	f, err := os.Open(temp)
	if err != nil {
		return result, err
	}
	hash := sha256.New()
	result.Bytes, err = io.Copy(hash, f)
	closeFileErr := f.Close()
	if err != nil {
		return result, err
	}
	if closeFileErr != nil {
		return result, closeFileErr
	}
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	suffix := strings.TrimPrefix(filepath.Base(workDir), ".recalldock-backup-work-")
	path := filepath.Join(dir, "recalldock-backup-"+time.Now().UTC().Format("20060102T150405Z")+"-"+suffix+".db")
	// Link publishes atomically without clobbering an existing name. Unsupported
	// filesystems fail closed rather than falling back to an overwriting rename.
	if err := os.Link(temp, path); err != nil {
		return result, fmt.Errorf("publish snapshot without overwrite: %w", err)
	}
	result.Path = path
	if err := os.RemoveAll(workDir); err != nil {
		return result, fmt.Errorf("snapshot at %s; remove staging directory failed: %w", path, err)
	}
	if err := syncDir(dir); err != nil {
		return result, fmt.Errorf("snapshot at %s; directory sync failed: %w", path, err)
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return result, fmt.Errorf("snapshot at %s; parent directory sync failed: %w", path, err)
	}
	result.Integrity = "ok"
	return result, nil
}

func verifyBackup(db *sql.DB) (string, error) {
	var integrity, id string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return "", fmt.Errorf("verify snapshot integrity: %w", err)
	}
	if integrity != "ok" {
		return "", fmt.Errorf("snapshot failed integrity verification")
	}
	if err := db.QueryRow("SELECT value FROM store_metadata WHERE key='store_id'").Scan(&id); err != nil || id == "" {
		return "", fmt.Errorf("snapshot has no valid store identity")
	}
	return id, nil
}
