package filelock

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReleaseKeepsStableLockPath pins the inode race that a deleting release
// creates, and is the reason Release never unlinks.
//
// Sequence with unlink-on-release:
//  1. A holds the lock on inode I1.
//  2. B opens the same path -> gets I1, blocks waiting for the lock.
//  3. A releases: unlock(I1), then unlink(path).
//  4. C opens the path -> creates a brand-new inode I2, locks it immediately.
//  5. B acquires its lock on I1.
//
// B and C now both "hold the lock" while holding different inodes, so mutual
// exclusion is gone. The race needs only one other process opening the file
// around release time, so it is reachable in normal use, not just under stress.
//
// A correct implementation keeps the file at a stable path and only releases the
// kernel lock, which is what this test asserts.
func TestReleaseKeepsStableLockPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file removed on release: %v; that reopens the inode race", err)
	}
}

// A crashed holder leaves the file behind; that must be harmless and must not
// require deleting the file to recover.
func TestStaleFileSurvivesRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(path, []byte("stale\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire over stale file: %v", err)
	}
	lock.Release()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale lock file removed on release: %v", err)
	}
}
