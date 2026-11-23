package store

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultDataDirForHome returns the platform-standard RecallDock data directory
// when an explicit RECALLDOCK_DATA_DIR override is not set.
func DefaultDataDirForHome(home string) string {
	if runtime.GOOS == "windows" {
		if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" && filepath.IsAbs(local) {
			return filepath.Join(filepath.Clean(local), "RecallDock")
		}
		if cacheDir, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cacheDir) {
			return filepath.Join(filepath.Clean(cacheDir), "RecallDock")
		}
		if home != "" {
			return filepath.Join(filepath.Clean(home), "AppData", "Local", "RecallDock")
		}
		return ""
	}
	return filepath.Join(home, ".recalldock")
}

func samePath(a, b string) bool {
	ca, cb := NormalizeWindowsShortPath(a), NormalizeWindowsShortPath(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

func normalizePath(path string) string {
	cleaned := filepath.Clean(path)
	if runtime.GOOS == "windows" && len(cleaned) >= 2 && cleaned[1] == ':' && cleaned[0] >= 'a' && cleaned[0] <= 'z' {
		cleaned = strings.ToUpper(cleaned[:1]) + cleaned[1:]
	}
	return cleaned
}

// resolvePathPrefix canonicalizes path or its deepest existing ancestor so
// renamed/removed child paths still expand Windows 8.3 short names or parent symlinks.
func resolvePathPrefix(path string) string {
	if runtime.GOOS == "windows" {
		return NormalizeWindowsShortPath(path)
	}
	path = normalizePath(path)
	if path == "" || !filepath.IsAbs(path) {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return normalizePath(resolved)
	}
	var tail []string
	cur := path
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return path
		}
		base := filepath.Base(cur)
		if base != "" && base != "." {
			tail = append(tail, base)
		}
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			parts := make([]string, 0, len(tail)+1)
			parts = append(parts, resolved)
			for i := len(tail) - 1; i >= 0; i-- {
				parts = append(parts, tail[i])
			}
			return normalizePath(filepath.Join(parts...))
		}
		cur = parent
	}
}

func rejectUNCPath(path string) error {
	if runtime.GOOS == "windows" && strings.HasPrefix(filepath.Clean(path), `\\`) {
		return errors.New("UNC network paths are not supported")
	}
	return nil
}

func sqliteFileURI(path, rawQuery string) string {
	slashed := filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed, RawQuery: rawQuery}).String()
}

func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func parseFileSourceRefPath(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "file://") {
		return "", false
	}
	rest := strings.TrimPrefix(raw, "file://")
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(rest, "/") && len(rest) >= 3 && rest[2] == ':' {
			rest = rest[1:]
		}
		rest = filepath.FromSlash(rest)
	}
	if !filepath.IsAbs(rest) {
		return "", false
	}
	return filepath.Clean(rest), true
}
