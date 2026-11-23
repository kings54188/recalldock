//go:build !windows

package store

// NormalizeWindowsShortPath returns path normalized without 8.3 expansion on
// non-Windows platforms.
func NormalizeWindowsShortPath(path string) string {
	return normalizePath(path)
}
