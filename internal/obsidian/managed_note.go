package obsidian

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func contentDigest(body []byte) string { return fmt.Sprintf("%x", sha256.Sum256(body)) }

// writeManagedNote refuses to replace files modified since our last successful
// write. Untracked files can only be adopted when their bytes already match.
func writeManagedNote(root, rel string, body []byte, state *SyncState) (string, error) {
	path, err := managedPath(root, rel)
	if err != nil {
		return "", err
	}
	status := "created"
	existing, err := os.ReadFile(path)
	if err == nil {
		if string(existing) == string(body) {
			state.Hashes[rel] = contentDigest(body)
			return "skipped", nil
		}
		if expected := state.Hashes[rel]; expected == "" || contentDigest(existing) != expected {
			return "", fmt.Errorf("manual/untracked file conflict: %s (preserved; copy annotations to a separate note before resolving)", path)
		}
		status = "updated"
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := atomicWrite(path, body); err != nil {
		return "", err
	}
	state.Hashes[rel] = contentDigest(body)
	return status, nil
}

func removeManagedNote(root, rel string, state *SyncState) error {
	path, err := managedPath(root, rel)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		delete(state.Hashes, rel)
		return nil
	}
	if err != nil {
		return err
	}
	if expected := state.Hashes[rel]; expected == "" || contentDigest(body) != expected {
		return fmt.Errorf("manual/untracked file conflict: %s (deletion refused)", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	delete(state.Hashes, rel)
	return nil
}

// atomicWrite uses a same-directory temporary file and sync before replacement.
// A directory sync failure after rename is an uncertain commit: reread on retry.
func atomicWrite(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".recalldock-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
