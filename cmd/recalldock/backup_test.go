package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackupDoesNotInitializeMissingOrFollowSymlinkDatabase(t *testing.T) {
	stubExitWithPanic(t)
	cfg := testConfig(t)
	cfg.DataDir = filepath.Join(cfg.DataDir, "absent")
	withArgs(t, "recalldock", "backup", "--json")
	_, stderr, p := captureOutputAndRecover(t, func() { cmdBackup(cfg) })
	if p == nil || stderr == "" {
		t.Fatal("missing DB accepted")
	}
	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Fatal("backup initialized missing store")
	}
	if err := os.Mkdir(cfg.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "original.db")
	if err := os.WriteFile(target, []byte("do not modify"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cfg.DataDir, "recalldock.db")); err != nil {
		t.Fatal(err)
	}
	_, _, p = captureOutputAndRecover(t, func() { cmdBackup(cfg) })
	if p == nil {
		t.Fatal("followed source symlink")
	}
	body, _ := os.ReadFile(target)
	if string(body) != "do not modify" {
		t.Fatal("changed source target")
	}
}
