//go:build windows

package opsstore

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func isUNCPath(path string) bool {
	return strings.HasPrefix(filepath.Clean(path), `\\`)
}

func extendedWinPath(path string) string {
	cleaned := filepath.Clean(path)
	if len(cleaned) >= 248 && filepath.IsAbs(cleaned) && !strings.HasPrefix(cleaned, `\\`) {
		return `\\?\` + cleaned
	}
	return cleaned
}

func expandOpsShortPath(path string) string {
	cleaned := normalizeOpsPath(path)
	if cleaned == "" || !filepath.IsAbs(cleaned) || isUNCPath(cleaned) {
		return cleaned
	}
	tryLong := func(p string) (string, bool) {
		in, err := windows.UTF16FromString(extendedWinPath(p))
		if err != nil || len(in) == 0 {
			return "", false
		}
		buf := make([]uint16, len(in)+260)
		n, err := windows.GetLongPathName(&in[0], &buf[0], uint32(len(buf)))
		if err != nil || n == 0 {
			return "", false
		}
		if n > uint32(len(buf)) {
			buf = make([]uint16, n)
			n, err = windows.GetLongPathName(&in[0], &buf[0], n)
			if err != nil || n == 0 {
				return "", false
			}
		}
		out := strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`)
		return normalizeOpsPath(out), true
	}
	if long, ok := tryLong(cleaned); ok {
		return long
	}
	var tail []string
	cur := cleaned
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return cleaned
		}
		if base := filepath.Base(cur); base != "" && base != "." {
			tail = append(tail, base)
		}
		if long, ok := tryLong(parent); ok {
			parts := make([]string, 0, len(tail)+1)
			parts = append(parts, long)
			for i := len(tail) - 1; i >= 0; i-- {
				parts = append(parts, tail[i])
			}
			return normalizeOpsPath(filepath.Join(parts...))
		}
		cur = parent
	}
}

func directoryIdentity(root string) (string, error) {
	if isUNCPath(root) {
		return "", fmt.Errorf("UNC network paths are not supported for operations workspace identity: %q", root)
	}
	ptr, err := windows.UTF16PtrFromString(extendedWinPath(root))
	if err != nil {
		return "", err
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
		return "", err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", fmt.Errorf("reparse points and symlinks are not supported for operations workspace identity")
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return "", fmt.Errorf("verified operations workspace identity requires a directory")
	}
	return fmt.Sprintf("win:%08x:%08x:%08x:%08x:%08x",
		info.VolumeSerialNumber,
		info.FileIndexHigh,
		info.FileIndexLow,
		info.CreationTime.HighDateTime,
		info.CreationTime.LowDateTime,
	), nil
}
