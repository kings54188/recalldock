//go:build windows

package store

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func isUNCPath(path string) bool {
	cleaned := filepath.Clean(path)
	return strings.HasPrefix(cleaned, `\\`)
}

func extendedWinPath(path string) string {
	cleaned := filepath.Clean(path)
	if len(cleaned) >= 248 && filepath.IsAbs(cleaned) && !strings.HasPrefix(cleaned, `\\`) {
		return `\\?\` + cleaned
	}
	return cleaned
}

func winDirHandleInfo(path string) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	if isUNCPath(path) {
		return info, fmt.Errorf("UNC network paths are not supported for verified directory bindings: %q", path)
	}
	ptr, err := windows.UTF16PtrFromString(extendedWinPath(path))
	if err != nil {
		return info, err
	}
	handle, err := windows.CreateFile(
		ptr,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return info, err
	}
	defer windows.CloseHandle(handle)
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return info, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return info, fmt.Errorf("reparse points and symlinks are not supported for directory bindings")
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return info, fmt.Errorf("directory required, not a file resource")
	}
	return info, nil
}

func directoryIdentity(root string) (string, string, error) {
	info, err := winDirHandleInfo(root)
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("vol:%08x", info.VolumeSerialNumber), fmt.Sprintf("fid:%08x:%08x", info.FileIndexHigh, info.FileIndexLow), nil
}
