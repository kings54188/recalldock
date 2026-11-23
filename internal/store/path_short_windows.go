//go:build windows

package store

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// NormalizeWindowsShortPath expands Windows 8.3 short path segments into their
// long names without dereferencing symlinks or junctions.
func NormalizeWindowsShortPath(path string) string {
	cleaned := normalizePath(path)
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
		return normalizePath(out), true
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
			return normalizePath(filepath.Join(parts...))
		}
		cur = parent
	}
}
