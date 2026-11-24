package filelock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// The path must survive release: it is the stable rendezvous point. Deleting
	// it lets two processes lock different inodes; see filelock_race_test.go.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file removed on release: %v", err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	second.Release()
}

func TestAcquireRejectsSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer first.Release()

	if _, err := Acquire(path); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second acquire error = %v, want ErrUnavailable", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

// The whole point of swapping O_EXCL for an advisory lock: a killed holder must
// not wedge the resource. This spawns a real child that takes the lock and exits
// without releasing it, then asserts the parent can acquire.
//
// On Windows the child cannot be killed while it holds a byte-range lock on a
// file it still has open, but exiting is enough: the kernel drops the lock when
// the last handle to the process closes.
func TestLockReleasedWhenHolderDies(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	path := filepath.Join(t.TempDir(), "x.lock")
	cmd := exec.Command(os.Args[0], "-test.run=TestLockHelperProcess")
	cmd.Env = append(os.Environ(), "FILELOCK_HELPER_PATH="+path)
	if err := cmd.Run(); err != nil {
		if exitErr := (*exec.ExitError)(nil); !errors.As(err, &exitErr) {
			t.Fatalf("run helper: %v", err)
		}
	}

	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("lock still held after holder died: %v", err)
	}
	lock.Release()
}

// TestLockHelperProcess is not a real test: it acquires the lock named by the
// environment, then exits without releasing, simulating a crash.
func TestLockHelperProcess(t *testing.T) {
	path := os.Getenv("FILELOCK_HELPER_PATH")
	if path == "" {
		t.Skip("helper process only")
	}
	if _, err := Acquire(path); err != nil {
		t.Fatalf("helper acquire: %v", err)
	}
	// Deliberately no Release: process exit must release the OS lock.
}

// A stale file from a dead process must not block a later acquirer.
func TestAcquireOverStaleFileSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(path, []byte("stale from a dead process\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire over stale file: %v", err)
	}
	lock.Release()
}

func TestAcquireCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "x.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire in missing directory: %v", err)
	}
	lock.Release()
}

func TestPathReportsLockedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()
	if lock.Path() != path {
		t.Fatalf("Path() = %q, want %q", lock.Path(), path)
	}
}

// Two holders must never overlap, which a stable path plus a kernel lock
// guarantees.
func TestConcurrentAcquireSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	var wg sync.WaitGroup
	var mu sync.Mutex
	inside, maxInside := 0, 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 20; attempt++ {
				lock, err := Acquire(path)
				if err != nil {
					continue
				}
				mu.Lock()
				inside++
				if inside > maxInside {
					maxInside = inside
				}
				mu.Unlock()

				mu.Lock()
				inside--
				mu.Unlock()
				lock.Release()
			}
		}()
	}
	wg.Wait()
	if maxInside > 1 {
		t.Fatalf("%d holders observed concurrently; mutual exclusion failed", maxInside)
	}
}

func TestPlatformIsSupported(t *testing.T) {
	switch runtime.GOOS {
	case "windows", "linux", "darwin", "freebsd", "openbsd", "netbsd":
	default:
		t.Skipf("advisory locking semantics unverified on %s", runtime.GOOS)
	}
	path := filepath.Join(t.TempDir(), "x.lock")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire on %s: %v", runtime.GOOS, err)
	}
	lock.Release()
}
