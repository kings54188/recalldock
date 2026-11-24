//go:build windows

package filelock

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes a non-blocking exclusive byte-range lock. LockFileEx locks are
// released when the handle closes, including when the process is killed, so a
// crashed holder cannot leave the resource wedged.
//
// Windows has no flock, so this is a one-byte range at offset 0. Every acquirer
// locks the same range on the same file, which gives the same exclusion
// semantics as flock on the whole file.
func tryLock(file *os.File) (bool, error) {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
	if err == nil {
		return true, nil
	}
	if err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_IO_PENDING {
		return false, nil
	}
	return false, err
}

func unlock(file *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
}
