//go:build !windows

package filelock

import (
	"os"
	"syscall"
)

// tryLock takes a non-blocking exclusive flock. The kernel releases it when the
// descriptor closes, including on process death, which is what makes a crashed
// holder recoverable without operator intervention.
func tryLock(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return false, nil
	}
	return false, err
}

func unlock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
