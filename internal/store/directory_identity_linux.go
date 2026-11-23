//go:build linux

package store

import (
	"fmt"
	"os"
	"syscall"
)

func directoryIdentity(root string) (string, string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("directory required, not a file resource")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", "", fmt.Errorf("directory identity unavailable")
	}
	return fmt.Sprint(stat.Dev), fmt.Sprint(stat.Ino), nil
}
