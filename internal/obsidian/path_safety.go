package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// managedPath rejects lexical escapes and existing symlinks below the managed
// namespace. The caller-selected vault may itself be a symlink. This guards
// ordinary exports, not hostile concurrent filesystem replacement (TOCTOU).
func managedPath(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") ||
		(runtime.GOOS != "windows" && strings.Contains(rel, `\`)) ||
		(runtime.GOOS == "windows" && (strings.HasPrefix(rel, `\`) || filepath.VolumeName(rel) != "")) {
		return "", fmt.Errorf("unsafe managed path %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("managed path escapes namespace: %q", rel)
	}
	current := root
	parts := []string{"."}
	if clean != "." {
		parts = append(parts, strings.Split(clean, string(filepath.Separator))...)
	}
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink in managed path: %s", current)
		}
	}
	return filepath.Join(root, clean), nil
}
