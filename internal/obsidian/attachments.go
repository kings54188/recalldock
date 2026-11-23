package obsidian

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"recalldock.local/recalldock/internal/store"
)

const attachmentFileLimit = 10 << 20
const attachmentTotalLimit = 50 << 20
const attachmentCountLimit = 64

// ValidateAttachmentRoot never reads file contents. An explicit real root is
// mandatory; relative paths and symlink roots cannot silently change scope.
func ValidateAttachmentRoot(root string) error {
	if root == "" {
		return nil
	}
	if !filepath.IsAbs(root) {
		return errors.New("attachment root must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	canonicalInput := store.NormalizeWindowsShortPath(root)
	if resolved != canonicalInput && !(runtime.GOOS == "windows" && strings.EqualFold(resolved, canonicalInput)) {
		return errors.New("attachment root must not traverse symlinks")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("attachment root must be a directory")
	}
	return nil
}

type projectedAttachment struct {
	path string
	body []byte
}

// Registered hashes select exact source bytes. Nothing is read by default and
// no remote URI is fetched. Assets are immutable and never silently garbage
// collected: user notes may still reference an older content-addressed file.
func (e *Exporter) exportAttachments(data *store.ExportData, state *SyncState) (map[int64][]string, error) {
	links := map[int64][]string{}
	if e.config.AttachmentRoot == "" {
		return links, nil
	}
	root := store.NormalizeWindowsShortPath(e.config.AttachmentRoot)
	selected := map[string]int64{}
	for _, obs := range data.Observations {
		if obs.SyncID != "" && obs.DeletedAt == nil && (e.config.Project == "" || (obs.Project != nil && *obs.Project == e.config.Project)) {
			selected[obs.SyncID] = obs.ID
		}
	}
	files := map[string]projectedAttachment{}
	total, count := 0, 0
	for _, dep := range data.ResourceDependencies {
		id, ok := selected[dep.DependentObservationSyncID]
		if !ok || dep.Status != "active" || (e.config.Project != "" && dep.Project != e.config.Project) || !strings.HasPrefix(dep.SourceRef, "file://") {
			continue
		}
		count++
		if count > attachmentCountLimit {
			return nil, errors.New("too many selected attachments (maximum 64)")
		}
		cut := strings.LastIndex(dep.SourceRef, "@sha256:")
		if cut < 0 {
			return nil, errors.New("attachment reference requires SHA256")
		}
		path := strings.TrimPrefix(dep.SourceRef[:cut], "file://")
		if runtime.GOOS == "windows" {
			if strings.HasPrefix(path, "/") && len(path) >= 3 && path[2] == ':' {
				path = path[1:]
			}
			path = filepath.FromSlash(path)
		}
		expected := dep.SourceRef[cut+len("@sha256:"):]
		digest, err := hex.DecodeString(expected)
		if err != nil || len(digest) != 32 {
			return nil, errors.New("invalid attachment SHA256")
		}
		if !filepath.IsAbs(path) {
			return nil, errors.New("attachment source must be an absolute registered file")
		}
		rel, err := filepath.Rel(root, store.NormalizeWindowsShortPath(path))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("attachment source escapes selected root")
		}
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if strings.HasPrefix(part, ".") {
				return nil, errors.New("hidden attachment path is not exported")
			}
		}
		// Reuse the no-symlink containment guard before opening any source.
		source, err := managedPath(root, rel)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(source)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > attachmentFileLimit {
			return nil, errors.New("attachment must be a regular file of at most 10 MiB")
		}
		ext := strings.ToLower(filepath.Ext(source))
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".pdf", ".txt", ".md", ".csv":
		default:
			return nil, errors.New("attachment type is not supported for projection")
		}
		f, err := os.Open(source)
		if err != nil {
			return nil, err
		}
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > attachmentFileLimit {
			f.Close()
			return nil, errors.New("attachment must be a regular file of at most 10 MiB")
		}
		body, err := io.ReadAll(io.LimitReader(f, attachmentFileLimit+1))
		f.Close()
		if err != nil {
			return nil, err
		}
		if len(body) > attachmentFileLimit || contentDigest(body) != strings.ToLower(expected) {
			return nil, errors.New("attachment source hash mismatch; recheck resource before exporting")
		}
		target := filepath.Join("_attachments", strings.ToLower(expected)+ext)
		if _, exists := files[target]; !exists {
			total += len(body)
			if total > attachmentTotalLimit {
				return nil, errors.New("selected attachment bytes exceed 50 MiB")
			}
			files[target] = projectedAttachment{path: target, body: body}
		}
		label := strings.NewReplacer("[", "_", "]", "_", "|", "_", "#", "_", "\n", " ", "\r", " ").Replace(filepath.Base(source))
		// Plain links avoid automatically embedding active content or external refs.
		links[id] = append(links[id], fmt.Sprintf("- [[recalldock/%s|%s]]", filepath.ToSlash(target), label))
	}
	// Validate every target before writing any asset. A manual target edit is
	// preserved even when its original managed digest is still in the state.
	for _, asset := range files {
		target, err := managedPath(e.vaultRoot(), asset.path)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(target)
		if err == nil && (!info.Mode().IsRegular() || info.Size() > attachmentFileLimit) {
			return nil, errors.New("unsafe or oversized existing attachment target")
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		old, err := os.ReadFile(target)
		if err == nil && contentDigest(old) != contentDigest(asset.body) {
			return nil, errors.New("attachment target modified; preserving existing file")
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := managedPath(e.vaultRoot(), filepath.Dir(name)); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(e.vaultRoot(), filepath.Dir(name)), 0755); err != nil {
			return nil, err
		}
		if _, err := writeManagedNote(e.vaultRoot(), name, files[name].body, state); err != nil {
			return nil, err
		}
	}
	for id, lines := range links {
		sort.Strings(lines)
		unique := lines[:0]
		for _, line := range lines {
			if len(unique) == 0 || unique[len(unique)-1] != line {
				unique = append(unique, line)
			}
		}
		links[id] = unique
	}
	return links, nil
}
