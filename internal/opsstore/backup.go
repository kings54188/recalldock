package opsstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Backup struct {
	Path      string `json:"path"`
	StoreID   string `json:"store_id"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
	Integrity string `json:"integrity"`
}

func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (s *Store) Backup() (Backup, error) {
	var result Backup
	dir := filepath.Join(s.dir, "operations-backups")
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return result, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return result, err
	}
	if !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) || info.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("backup directory must be a private real directory")
	}
	workDir, err := os.MkdirTemp(dir, ".ops-work-*")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(workDir)
	temp := filepath.Join(workDir, "snapshot.db")
	if _, err := s.db.Exec("VACUUM INTO ?", temp); err != nil {
		return result, err
	}
	if err := os.Chmod(temp, 0600); err != nil {
		return result, err
	}
	db, err := sql.Open("sqlite", sqliteFileURI(temp, "mode=ro"))
	if err != nil {
		return result, err
	}
	defer db.Close()
	check := &Store{db: db}
	if err := check.verify(); err != nil {
		return result, err
	}
	if check.ID != s.ID {
		return result, ErrIdentity
	}
	if _, err := check.Doctor(); err != nil {
		return result, err
	}
	result.StoreID = check.ID
	if err := db.Close(); err != nil {
		return result, err
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
	name := "operations-" + time.Now().UTC().Format("20060102T150405Z") + "-" + strings.TrimPrefix(filepath.Base(workDir), ".ops-work-") + ".db"
	result.Path = filepath.Join(dir, name)
	if err := os.Link(temp, result.Path); err != nil {
		return result, err
	}
	if err := os.RemoveAll(workDir); err != nil {
		return result, err
	}
	if err := syncDir(dir); err != nil {
		return result, err
	}
	if err := syncDir(s.dir); err != nil {
		return result, err
	}
	result.Integrity = "ok"
	return result, nil
}
