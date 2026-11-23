package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewStoreCreatesPrivateDirectoryWithoutChmodExisting(t *testing.T) {
	cfg := mustDefaultConfig(t)
	cfg.DataDir = filepath.Join(t.TempDir(), "private-data")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	info, err := os.Stat(cfg.DataDir)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0700) {
		t.Fatalf("new data directory not private: %v %v", info, err)
	}
	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(existing, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0755); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = existing
	s, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	info, err = os.Stat(existing)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0755) {
		t.Fatal("silently changed operator-owned existing permissions")
	}
}
