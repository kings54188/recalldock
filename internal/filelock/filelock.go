// Package filelock serializes cross-process access to one file path using an
// operating-system advisory lock.
//
// It replaces the earlier "create the lock path with O_EXCL and delete it on
// exit" pattern. That pattern fails closed after a crashed writer, which is the
// right instinct, but the failure is sticky: SIGKILL, a power loss or os.Exit
// skips the deferred remove, and every later operation then reports "lock
// unavailable; inspect before recovery" while pointing the operator at a hidden
// dotfile they have no way to interpret. An OS-level lock is released by the
// kernel when the owning process dies, so a crashed holder cannot wedge the next
// run.
//
// # The lock file is never deleted
//
// Ownership comes from the kernel, never from the file existing. Keeping the
// path stable is required for correctness, because unlinking on release reopens
// a mutual-exclusion hole:
//
//  1. A holds the lock on inode I1.
//  2. B opens the same path, gets I1, and blocks waiting for the lock.
//  3. A releases by unlocking I1 and then unlinking the path.
//  4. C opens the path, creates a brand-new inode I2, and locks it at once.
//  5. B finally acquires its lock on I1.
//
// B and C now both "hold the lock" on different inodes, so two writers proceed
// concurrently. Retaining the file leaves one inode at one path, so every
// acquirer contends for the same byte range.
//
// A lock file left behind by a crash is therefore expected and harmless: it is
// not evidence that the resource is locked.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrUnavailable reports that another live process holds the lock. The text
// stays short because it is shown to operators, and the previous wording sent
// them looking for a file to delete.
var ErrUnavailable = errors.New("resource is locked by another process; retry after it exits")

// Lock is a held advisory lock. Release it exactly once; the methods are safe to
// call again after that but do nothing.
type Lock struct {
	path string
	file *os.File
	done bool
}

// Acquire takes an exclusive lock on path, creating the file when absent and
// leaving it in place.
//
// O_CREATE is atomic, so the first process to arrive creates the inode that
// every later process opens; nothing unlinks it afterwards, so all acquirers
// contend for the same file.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	locked, err := tryLock(file)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("lock %s: %w", filepath.Base(path), err)
	}
	if !locked {
		file.Close()
		return nil, ErrUnavailable
	}
	return &Lock{path: path, file: file}, nil
}

// Release drops the kernel lock and closes the handle. The file stays on disk
// on purpose; see the package documentation.
func (l *Lock) Release() error {
	if l == nil || l.done || l.file == nil {
		return nil
	}
	l.done = true
	unlockErr := unlock(l.file)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// Path reports the locked path, for diagnostics.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}
