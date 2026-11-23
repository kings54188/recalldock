//go:build linux

package opsstore

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func directoryIdentity(root string) (string, error) {
	var stat unix.Statx_t
	mask := unix.STATX_INO | unix.STATX_TYPE | unix.STATX_BTIME
	if err := unix.Statx(unix.AT_FDCWD, root, unix.AT_SYMLINK_NOFOLLOW, mask, &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mask&uint32(mask) != uint32(mask) {
		return "", fmt.Errorf("verified directory instance identity unavailable")
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", stat.Dev_major, stat.Dev_minor, stat.Ino, stat.Btime.Sec, stat.Btime.Nsec), nil
}

func expandOpsShortPath(path string) string {
	return normalizeOpsPath(path)
}
